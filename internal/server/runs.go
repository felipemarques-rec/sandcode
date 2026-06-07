package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/felipemarques-rec/sandcode/internal/orchestrator"
	"github.com/felipemarques-rec/sandcode/internal/runtime"
	"github.com/felipemarques-rec/sandcode/internal/scheduler"
)

// Default and maximum page size for GET /v1/runs. The maximum is
// pinned to the state cache's default capacity so a single page can
// always cover the full hot set.
const (
	defaultListLimit = 100
	maxListLimit     = DefaultStateCacheCapacity
)

// ListRunsResponse wraps the run summaries in a JSON object so future
// pagination metadata can land alongside without breaking clients.
type ListRunsResponse struct {
	Runs []runtime.ExecutionState `json:"runs"`
}

// CreateRunResponse is the 202 body for POST /v1/runs.
type CreateRunResponse struct {
	RunID string `json:"run_id"`
}

// errorResponse is the canonical error body shape.
type errorResponse struct {
	Error string `json:"error"`
}

func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	if s.opts.Launcher == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{
			Error: "launcher not configured",
		})
		return
	}

	const maxBody = 256 * 1024 // 256 KiB is plenty for a JSON RunRequest.
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)

	var req RunRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{
			Error: "invalid json: " + err.Error(),
		})
		return
	}
	if err := req.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	if err := s.checkRunPolicy(req); err != nil {
		writeJSON(w, http.StatusForbidden, errorResponse{Error: err.Error()})
		return
	}

	runID := orchestrator.NewRunID()

	if s.sched != nil {
		prio, perr := scheduler.ParsePriority(req.Priority)
		if perr != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: perr.Error()})
			return
		}
		s.putPending(runID, req)
		switch err := s.sched.Submit(runID, prio); err {
		case nil:
			// fall through to 202 below
		case scheduler.ErrQueueFull:
			s.takePending(runID) // un-stage; nothing will launch it
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusTooManyRequests, errorResponse{Error: "scheduler queue full"})
			return
		case scheduler.ErrStopped:
			s.takePending(runID)
			writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "server draining"})
			return
		default:
			s.takePending(runID)
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: err.Error()})
			return
		}
		w.Header().Set("Location", "/v1/runs/"+runID)
		writeJSON(w, http.StatusAccepted, CreateRunResponse{RunID: runID})
		return
	}

	s.launchAsync(runID, req) // legacy unbounded path (scheduler disabled)

	w.Header().Set("Location", "/v1/runs/"+runID)
	writeJSON(w, http.StatusAccepted, CreateRunResponse{RunID: runID})
}

func (s *Server) handleCancelRun(w http.ResponseWriter, r *http.Request) {
	if s.sched == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "scheduler not enabled"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "run id required"})
		return
	}
	if s.sched.Cancel(id) {
		// Scheduler entry gone; drop the now-unreachable staged request
		// (Cancel only succeeds for queued runs, so launchFunc will
		// never run for it — symmetric with the reject paths' un-stage).
		s.takePending(id)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusConflict, errorResponse{Error: "cannot cancel; run not in queue"})
}

// handleListRuns returns the cached run state snapshots newest-first.
// Query params:
//
//   - limit  : 1..maxListLimit (default 100)
//   - phase  : filter to runs whose Phase matches exactly
//
// Source is the in-memory state cache, not the persistent store —
// `?from=` replays on /v1/runs/{id}/events for full history.
func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	limit := defaultListLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeJSON(w, http.StatusBadRequest, errorResponse{
				Error: "limit: positive integer required",
			})
			return
		}
		if n > maxListLimit {
			n = maxListLimit
		}
		limit = n
	}

	phaseFilter := runtime.Phase(q.Get("phase"))

	// StateCache.List() returns insertion order (oldest first). The
	// list endpoint surfaces newest-first, so we walk the slice from
	// the tail and stop when limit is reached.
	all := s.opts.StateCache.List()
	out := make([]runtime.ExecutionState, 0, limit)
	for i := len(all) - 1; i >= 0 && len(out) < limit; i-- {
		st := all[i]
		if phaseFilter != "" && st.Phase != phaseFilter {
			continue
		}
		out = append(out, st)
	}
	writeJSON(w, http.StatusOK, ListRunsResponse{Runs: out})
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	if runID == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "id: required"})
		return
	}
	st, err := s.opts.StateCache.Get(runID)
	if err != nil {
		if errors.Is(err, ErrUnknownRun) {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: "unknown run"})
			return
		}
		s.opts.Logger.Error("server: get run failed", "run_id", runID, "error", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// writeJSON serialises v as JSON and writes the response. HTML
// escaping is disabled because we never embed responses in HTML and
// the default escaping mangles error strings (e.g. ">" → ">").
// JSON encoding errors here are programmer errors (the input is our
// own struct), so we surface them as 500.
func writeJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		http.Error(w, "internal encode error", http.StatusInternalServerError)
		return
	}
	// json.Encoder.Encode appends a trailing newline; preserve it so
	// curl output is human-readable.
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}
