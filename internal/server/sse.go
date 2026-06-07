package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/event"
)

// sseChannelBuffer is the per-stream goroutine-hand-off buffer. Bus
// publication is synchronous in the publisher's goroutine, so a slow
// HTTP consumer must not block run progress. We absorb up to this many
// events; past that, the publisher path drops events and logs once.
const sseChannelBuffer = 64

// handleRunEventsSSE streams the lifecycle of a single run as
// Server-Sent Events. It exits when the client disconnects, the run
// reaches a terminal phase, or the server shuts down.
//
// Default behaviour is live-tail: events that fired before the
// subscription was established are not replayed.
//
// Replay can be requested two ways. Both require Options.Store to be
// configured:
//
//   - `?from=<event_id>` query parameter — explicit, takes precedence.
//   - `Last-Event-ID` request header — the SSE-spec reconnect mechanism
//     the browser EventSource API sends automatically. Falls back here
//     when `?from=` is empty.
//
// On replay the handler emits events strictly AFTER the named ID, then
// transitions to live tail. Replay/live overlap is deduplicated by
// event ID so clients never see the same event twice across the seam.
func (s *Server) handleRunEventsSSE(w http.ResponseWriter, r *http.Request) {
	if s.opts.Bus == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{
			Error: "event bus not configured",
		})
		return
	}
	runID := r.PathValue("id")
	if runID == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "id: required"})
		return
	}

	// Replay anchor: explicit ?from= wins; Last-Event-ID is the
	// EventSource-driven fallback for automatic browser reconnects.
	// Validate up front (before WriteHeader(200)) so invalid requests
	// get a proper HTTP error, not a half-streamed 200.
	from := r.URL.Query().Get("from")
	if from == "" {
		from = r.Header.Get("Last-Event-ID")
	}
	if from != "" {
		if s.opts.Store == nil {
			writeJSON(w, http.StatusServiceUnavailable, errorResponse{
				Error: "event store not configured; cannot replay history",
			})
			return
		}
		history, err := s.opts.Store.LoadRun(r.Context(), runID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errorResponse{
				Error: err.Error(),
			})
			return
		}
		if !containsEventID(history, from) {
			writeJSON(w, http.StatusBadRequest, errorResponse{
				Error: "from: event id not found in run",
			})
			return
		}
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errorResponse{
			Error: "streaming unsupported on this transport",
		})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Subscribe BEFORE the replay snapshot so events emitted during
	// snapshot read are not missed on the live side. The dedup set
	// below handles the overlap: any event that ends up in both the
	// replay snapshot and the live channel is emitted exactly once.
	events := make(chan event.Event, sseChannelBuffer)
	var logDrop sync.Once
	sub := s.opts.Bus.Subscribe("*", func(_ context.Context, ev event.Event) error {
		if ev.RunID != runID {
			return nil
		}
		select {
		case events <- ev:
		default:
			logDrop.Do(func() {
				s.opts.Logger.Warn("server: sse buffer overflow, dropping events",
					"run_id", runID,
				)
			})
		}
		return nil
	})
	defer sub.Cancel()

	seen := make(map[string]struct{})

	// Replay phase. The second LoadRun runs AFTER we're subscribed,
	// so any race-window event (emitted between Subscribe and this
	// snapshot) is captured in both places and the dedup map below
	// drops the live duplicate.
	if from != "" {
		history, err := s.opts.Store.LoadRun(r.Context(), runID)
		if err != nil {
			// Connection is already committed (200 sent); surface as a
			// stream-level error event and exit. Validation above means
			// this only triggers on transient store errors.
			_, _ = fmt.Fprintf(w, "event: error\ndata: %q\n\n", err.Error())
			flusher.Flush()
			return
		}
		idx := indexOfEventID(history, from)
		// idx >= 0 is guaranteed by the validation pass; if a
		// concurrent compaction removed `from` between validate and
		// replay we treat the slice as "no replay" rather than 400.
		if idx >= 0 {
			for _, ev := range history[idx+1:] {
				if err := writeSSEEvent(w, ev); err != nil {
					return
				}
				flusher.Flush()
				seen[ev.ID] = struct{}{}
				if isTerminalEvent(ev.Type) {
					return
				}
			}
		}
	}

	var keepaliveCh <-chan time.Time
	if s.opts.SSEKeepalive > 0 {
		ticker := time.NewTicker(s.opts.SSEKeepalive)
		defer ticker.Stop()
		keepaliveCh = ticker.C
	}

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-keepaliveCh:
			// SSE comment line — clients ignore it, but proxies see traffic.
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case ev := <-events:
			if _, dup := seen[ev.ID]; dup {
				continue
			}
			if err := writeSSEEvent(w, ev); err != nil {
				return
			}
			flusher.Flush()
			if isTerminalEvent(ev.Type) {
				return
			}
		}
	}
}

// containsEventID reports whether any event in history has the given ID.
func containsEventID(history []event.Event, id string) bool {
	return indexOfEventID(history, id) >= 0
}

// indexOfEventID returns the position of id in history, or -1.
func indexOfEventID(history []event.Event, id string) int {
	for i := range history {
		if history[i].ID == id {
			return i
		}
	}
	return -1
}

// writeSSEEvent serialises ev in SSE wire format:
//
//	event: <type>
//	id:    <event_id>
//	data:  <json>
//	\n
func writeSSEEvent(w http.ResponseWriter, ev event.Event) error {
	body, err := json.Marshal(ev)
	if err != nil {
		// Encoder shouldn't fail on event.Event; if it does, write a
		// minimal envelope so the client at least sees the type.
		body = []byte(fmt.Sprintf(`{"error":%q}`, err.Error()))
	}
	if _, err := fmt.Fprintf(w, "event: %s\nid: %s\ndata: %s\n\n",
		ev.Type, ev.ID, body); err != nil {
		return err
	}
	return nil
}

// isTerminalEvent reports whether the event marks the end of a run's
// lifecycle. SSE streams close on terminal events so clients can move
// on without waiting for the server-side keepalive to expire.
func isTerminalEvent(t event.Type) bool {
	switch t {
	case event.RunCompleted, event.RunFailed, event.RunCancelled:
		return true
	default:
		return false
	}
}
