package server

import (
	"fmt"
	"net/http"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/governance"
)

// validAuditResultFilters enumerates the values accepted by the
// ?result= query parameter on GET /v1/runs/{id}/audit. Empty (omitted)
// means "no filter". Wire strings mirror governance.Result.
var validAuditResultFilters = map[string]governance.Result{
	"allow":  governance.Allow,
	"deny":   governance.Deny,
	"review": governance.Review,
}

// AuditRowDTO is the wire shape of governance.AuditRow on
// GET /v1/runs/{id}/audit. We define a separate struct (rather than
// adding JSON tags to the internal type) so the server package owns
// API stability and the governance package stays decoupled from HTTP.
type AuditRowDTO struct {
	ID         string    `json:"id"`
	RunID      string    `json:"run_id"`
	ActionType string    `json:"action_type"`
	Result     string    `json:"result"`
	Reasons    []string  `json:"reasons,omitempty"`
	PolicyName string    `json:"policy_name,omitempty"`
	Approver   string    `json:"approver,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// ListAuditResponse wraps the audit rows in an object so future fields
// (cursor, total) can land without breaking clients.
type ListAuditResponse struct {
	Rows []AuditRowDTO `json:"rows"`
}

// handleListRunAudit returns every governance decision recorded for the
// given run, chronologically. Supports ?result=allow|deny|review for
// in-memory filtering after load — audit volumes are small (per-run
// per-policy per-action), so a dedicated SQL filter isn't worth the
// AuditLog interface churn yet.
func (s *Server) handleListRunAudit(w http.ResponseWriter, r *http.Request) {
	if s.opts.Audit == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{
			Error: "audit log not configured",
		})
		return
	}
	runID := r.PathValue("id")
	if runID == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "id: required"})
		return
	}

	// Parse + validate the optional result filter BEFORE the DB read.
	var resultFilter governance.Result
	if raw := r.URL.Query().Get("result"); raw != "" {
		f, ok := validAuditResultFilters[raw]
		if !ok {
			writeJSON(w, http.StatusBadRequest, errorResponse{
				Error: fmt.Sprintf("result: must be one of allow|deny|review, got %q", raw),
			})
			return
		}
		resultFilter = f
	}

	rows, err := s.opts.Audit.ListByRun(r.Context(), runID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: err.Error()})
		return
	}
	out := make([]AuditRowDTO, 0, len(rows))
	for _, r := range rows {
		if resultFilter != "" && r.Result != resultFilter {
			continue
		}
		out = append(out, toAuditRowDTO(r))
	}
	writeJSON(w, http.StatusOK, ListAuditResponse{Rows: out})
}

func toAuditRowDTO(r governance.AuditRow) AuditRowDTO {
	return AuditRowDTO{
		ID:         r.ID,
		RunID:      r.RunID,
		ActionType: string(r.ActionType),
		Result:     string(r.Result),
		Reasons:    r.Reasons,
		PolicyName: r.PolicyName,
		Approver:   r.Approver,
		CreatedAt:  r.CreatedAt,
	}
}
