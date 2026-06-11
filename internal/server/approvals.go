package server

import (
	"encoding/json"
	"net/http"

	"github.com/felipemarques-rec/sandcode/internal/approval"
)

// handleApproveRun resolves a run blocked on a governance Review verdict.
// Body: {"decision":"approve"|"reject","approver":"<id>","reason":"<optional>"}.
func (s *Server) handleApproveRun(w http.ResponseWriter, r *http.Request) {
	if s.opts.Approvals == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "approvals not enabled"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "run id required"})
		return
	}
	var body struct {
		Decision string `json:"decision"`
		Approver string `json:"approver"`
		Reason   string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid request body"})
		return
	}
	var approved bool
	switch body.Decision {
	case "approve":
		approved = true
	case "reject":
		approved = false
	default:
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: `decision must be "approve" or "reject"`})
		return
	}
	// When a keyring is configured, the approver identity must come from the
	// authenticated principal (set by withAuth), never the client-supplied
	// body.approver, which a caller could spoof. Legacy (nil keyring) preserves
	// the body-supplied approver.
	approver := body.Approver
	if s.opts.Keyring != nil {
		if p, ok := principalFrom(r.Context()); ok {
			approver = p.ID
		}
	}
	if s.opts.Approvals.Resolve(id, approval.Decision{Approved: approved, Approver: approver, Reason: body.Reason}) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "resolved", "decision": body.Decision})
		return
	}
	writeJSON(w, http.StatusNotFound, errorResponse{Error: "no run awaiting approval for this id"})
}
