package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/compliance"
	"github.com/felipemarques-rec/sandcode/internal/store"
)

// handleRunCompliance returns the per-run compliance & explainability report.
// Default media type is JSON; ?format=md returns Markdown. Gated by
// rbac.CapAuditRead at registration.
func (s *Server) handleRunCompliance(w http.ResponseWriter, r *http.Request) {
	if s.opts.Audit == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "audit log not configured"})
		return
	}
	runID := r.PathValue("id")
	if runID == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "id: required"})
		return
	}
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "json"
	}
	if format != "json" && format != "md" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "format: must be one of json|md"})
		return
	}

	ident, found, err := s.runIdentity(r.Context(), runID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal error"})
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "unknown run"})
		return
	}

	rows, err := s.opts.Audit.ListByRun(r.Context(), runID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: err.Error()})
		return
	}

	rep := compliance.Build(compliance.ReportInput{
		Run:       ident,
		AuditRows: rows,
		Now:       time.Now(),
	})

	if format == "md" {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, rep.RenderMarkdown())
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// runIdentity resolves run identity preferring the full run store, falling
// back to the execution-state cache (minimal identity). A genuine store error
// (anything other than store.ErrRunNotFound) is surfaced so the handler can
// return 500 rather than masking an outage as 404.
func (s *Server) runIdentity(ctx context.Context, runID string) (compliance.RunIdentity, bool, error) {
	if s.opts.RunStore != nil {
		run, err := s.opts.RunStore.GetRun(ctx, runID)
		if err == nil {
			return fromRun(run), true, nil
		}
		if !errors.Is(err, store.ErrRunNotFound) {
			return compliance.RunIdentity{}, false, err
		}
		// not found in store → try the execution-state cache below
	}
	if st, err := s.opts.StateCache.Get(runID); err == nil {
		return compliance.RunIdentity{
			ID:        st.RunID,
			Status:    string(st.Phase),
			StartedAt: st.CreatedAt,
		}, true, nil
	}
	return compliance.RunIdentity{}, false, nil
}

func fromRun(run store.Run) compliance.RunIdentity {
	return compliance.RunIdentity{
		ID:         run.ID,
		Agent:      run.Agent,
		Prompt:     run.Prompt,
		Status:     string(run.Status),
		StartedAt:  run.StartedAt,
		FinishedAt: run.FinishedAt,
		ExitCode:   run.ExitCode,
	}
}
