package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/event"
	"github.com/felipemarques-rec/sandcode/internal/metrics"
)

// newReplayTestServer wires a bus + persistor + on-disk event store so
// SSE replay tests can publish to the bus and have the store reflect
// it before the SSE request hits the handler.
func newReplayTestServer(t *testing.T) (*Server, event.Bus, *event.SQLiteStore) {
	t.Helper()
	bus := event.NewLocalBus()
	t.Cleanup(func() { _ = bus.Close() })

	es, err := event.OpenStore(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = es.Close() })

	sub := event.PersistTo(bus, es)
	t.Cleanup(sub.Cancel)

	reg := metrics.NewRegistry()
	cache := NewStateCache(0)
	t.Cleanup(cache.Attach(bus).Cancel)

	srv := New(Options{
		Registry:   reg,
		StateCache: cache,
		Bus:        bus,
		Store:      es,
	})
	srv.opts.SSEKeepalive = -1
	return srv, bus, es
}

// seedHistory publishes a slice of events through the bus, waiting
// briefly to let the synchronous PersistTo subscriber drain. Returns
// the events with their generated IDs populated so callers can pick
// one to use as the `from` query value.
func seedHistory(t *testing.T, bus event.Bus, runID string, types []event.Type) []event.Event {
	t.Helper()
	out := make([]event.Event, 0, len(types))
	for _, typ := range types {
		ev := event.New(typ, runID, nil)
		if err := bus.Publish(context.Background(), ev); err != nil {
			t.Fatalf("publish %s: %v", typ, err)
		}
		out = append(out, ev)
	}
	return out
}

func TestRunEventsSSE_Replay_StrictlyAfterFrom(t *testing.T) {
	srv, _, _ := newReplayTestServer(t)
	const runID = "rep00001"

	history := seedHistory(t, srv.opts.Bus, runID, []event.Type{
		event.RunSubmitted,
		event.RunClassified,
		event.RunPlanned,
		event.RunCompleted,
	})

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/runs/" + runID + "/events?from=" + history[1].ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(body)

	// from=history[1] (run.classified) means we must see events 2 and 3
	// (run.planned, run.completed) but NOT 0 or 1.
	mustNot := []string{
		"id: " + history[0].ID,
		"id: " + history[1].ID,
	}
	must := []string{
		"id: " + history[2].ID,
		"id: " + history[3].ID,
		"event: run.completed",
	}
	for _, s := range mustNot {
		if strings.Contains(got, s) {
			t.Errorf("body unexpectedly contains %q:\n%s", s, got)
		}
	}
	for _, s := range must {
		if !strings.Contains(got, s) {
			t.Errorf("body missing %q:\n%s", s, got)
		}
	}
}

func TestRunEventsSSE_Replay_UnknownFromReturns400(t *testing.T) {
	srv, _, _ := newReplayTestServer(t)
	const runID = "rep00002"
	seedHistory(t, srv.opts.Bus, runID, []event.Type{event.RunSubmitted})

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/runs/" + runID + "/events?from=does-not-exist")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 400; body=%s", resp.StatusCode, body)
	}
}

func TestRunEventsSSE_Replay_WithoutStoreReturns503(t *testing.T) {
	bus := event.NewLocalBus()
	defer bus.Close()

	reg := metrics.NewRegistry()
	cache := NewStateCache(0)
	t.Cleanup(cache.Attach(bus).Cancel)

	srv := New(Options{
		Registry:   reg,
		StateCache: cache,
		Bus:        bus,
		// Store deliberately omitted.
	})
	srv.opts.SSEKeepalive = -1

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/runs/rep00003/events?from=some-id")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

func TestRunEventsSSE_Replay_TerminalInReplayClosesStream(t *testing.T) {
	srv, _, _ := newReplayTestServer(t)
	const runID = "rep00004"

	history := seedHistory(t, srv.opts.Bus, runID, []event.Type{
		event.RunSubmitted,
		event.RunCompleted,
	})

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// `from=history[0]` means we replay only history[1] (terminal).
	// The handler must close the response after emitting it instead
	// of hanging on the live tail.
	deadline := time.After(2 * time.Second)
	done := make(chan struct{})
	var body []byte
	go func() {
		resp, err := http.Get(ts.URL + "/v1/runs/" + runID + "/events?from=" + history[0].ID)
		if err != nil {
			t.Errorf("get: %v", err)
			close(done)
			return
		}
		defer resp.Body.Close()
		body, _ = io.ReadAll(resp.Body)
		close(done)
	}()
	select {
	case <-done:
	case <-deadline:
		t.Fatal("SSE stream did not close after terminal event in replay")
	}
	if !strings.Contains(string(body), "event: run.completed") {
		t.Errorf("body missing terminal event:\n%s", string(body))
	}
	if strings.Contains(string(body), "id: "+history[0].ID) {
		t.Errorf("body should not contain pre-`from` event:\n%s", string(body))
	}
}

func TestRunEventsSSE_Replay_DedupsAgainstLiveTail(t *testing.T) {
	// Verifies the seen-set covers the replay/live overlap window.
	// We populate the store, then connect with from=<first>. The
	// handler will replay the rest. To simulate a race-window
	// duplicate, we then re-publish (with the same ID) one of the
	// already-replayed events; the dedup map must drop it so the
	// client sees the event exactly once.
	srv, bus, _ := newReplayTestServer(t)
	const runID = "rep00005"

	history := seedHistory(t, bus, runID, []event.Type{
		event.RunSubmitted,
		event.RunClassified,
		event.AgentExecuting,
	})

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet,
		ts.URL+"/v1/runs/"+runID+"/events?from="+history[0].ID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	// Let the handler complete its replay phase and enter live tail.
	time.Sleep(80 * time.Millisecond)

	// Re-publish history[1] verbatim. PersistTo's Append will fail
	// (PRIMARY KEY conflict) and log, but the bus subscriber on the
	// SSE handler will still receive the event and must dedup it.
	_ = bus.Publish(context.Background(), history[1])

	// Fire a terminal event to close the stream cleanly.
	terminal := event.New(event.RunCompleted, runID, nil)
	_ = bus.Publish(context.Background(), terminal)

	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	// history[1] should appear EXACTLY once across the whole stream
	// (the replay copy). The re-published live copy must have been
	// dropped by the seen-set dedup.
	if n := strings.Count(got, "id: "+history[1].ID); n != 1 {
		t.Errorf("history[1] id appears %d times, want 1:\n%s", n, got)
	}
	if !strings.Contains(got, "id: "+terminal.ID) {
		t.Errorf("terminal event missing:\n%s", got)
	}
}

func TestRunEventsSSE_Replay_LastEventIDHeaderDrivesReplay(t *testing.T) {
	// Browsers' EventSource sends Last-Event-ID automatically on
	// reconnect. The handler must treat it like ?from=.
	srv, _, _ := newReplayTestServer(t)
	const runID = "rep00007"

	history := seedHistory(t, srv.opts.Bus, runID, []event.Type{
		event.RunSubmitted,
		event.RunClassified,
		event.RunCompleted,
	})

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/runs/"+runID+"/events", nil)
	req.Header.Set("Last-Event-ID", history[0].ID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	if strings.Contains(got, "id: "+history[0].ID) {
		t.Errorf("body should not include the Last-Event-ID itself:\n%s", got)
	}
	for _, want := range []string{
		"id: " + history[1].ID,
		"id: " + history[2].ID,
		"event: run.completed",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("body missing %q:\n%s", want, got)
		}
	}
}

func TestRunEventsSSE_Replay_QueryParamWinsOverHeader(t *testing.T) {
	// When both ?from= and Last-Event-ID are sent, the explicit query
	// parameter takes precedence — header is only a fallback.
	srv, _, _ := newReplayTestServer(t)
	const runID = "rep00008"

	history := seedHistory(t, srv.opts.Bus, runID, []event.Type{
		event.RunSubmitted,
		event.RunClassified,
		event.RunPlanned,
		event.RunCompleted,
	})

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Header says start after [0] (replay events 1, 2, 3).
	// Query says start after [2] (replay only event 3).
	// Query must win.
	req, _ := http.NewRequest(http.MethodGet,
		ts.URL+"/v1/runs/"+runID+"/events?from="+history[2].ID, nil)
	req.Header.Set("Last-Event-ID", history[0].ID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	// Events 1 and 2 must NOT appear — query param wins, so only
	// events strictly after history[2] are replayed.
	for _, mustNot := range []string{
		"id: " + history[1].ID,
		"id: " + history[2].ID,
	} {
		if strings.Contains(got, mustNot) {
			t.Errorf("body should not include %q (query wins):\n%s", mustNot, got)
		}
	}
	if !strings.Contains(got, "id: "+history[3].ID) {
		t.Errorf("body missing post-query event:\n%s", got)
	}
}

func TestRunEventsSSE_Replay_LastEventIDUnknownReturns400(t *testing.T) {
	srv, _, _ := newReplayTestServer(t)
	const runID = "rep00009"
	seedHistory(t, srv.opts.Bus, runID, []event.Type{event.RunSubmitted})

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/runs/"+runID+"/events", nil)
	req.Header.Set("Last-Event-ID", "no-such-event")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 400; body=%s", resp.StatusCode, body)
	}
}

func TestRunEventsSSE_NoFrom_StaysLiveTail(t *testing.T) {
	// Regression: when `from` is empty the handler should behave
	// exactly as before — no replay, just live tail.
	srv, bus, _ := newReplayTestServer(t)
	const runID = "rep00006"

	// Pre-seed some events the handler must NOT replay.
	ignored := seedHistory(t, bus, runID, []event.Type{event.RunSubmitted})

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/runs/" + runID + "/events")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	// Wait for handler to subscribe, then publish a terminal event.
	time.Sleep(50 * time.Millisecond)
	terminal := event.New(event.RunCompleted, runID, nil)
	_ = bus.Publish(context.Background(), terminal)

	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	if strings.Contains(got, "id: "+ignored[0].ID) {
		t.Errorf("body should not include pre-subscription events:\n%s", got)
	}
	if !strings.Contains(got, "id: "+terminal.ID) {
		t.Errorf("body missing live terminal event:\n%s", got)
	}
}
