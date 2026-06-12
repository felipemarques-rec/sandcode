// Package compliance assembles a per-run compliance & explainability report
// from already-fetched primitives (run identity + governance audit trail +
// trace id). It is a pure leaf: no HTTP, no store access. Callers fetch the
// inputs from whatever store they hold and call Build.
package compliance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/governance"
	"github.com/felipemarques-rec/sandcode/internal/redact"
)

// SchemaVersion is the stable wire contract version for Report.
const SchemaVersion = "1.0"

// Report is the canonical compliance & explainability document for one run.
type Report struct {
	SchemaVersion string      `json:"schema_version"`
	GeneratedAt   time.Time   `json:"generated_at"`
	Run           RunIdentity `json:"run"`
	TraceID       string      `json:"trace_id,omitempty"`
	Decisions     []Decision  `json:"decisions"`
	Summary       Summary     `json:"summary"`
	Integrity     Integrity   `json:"integrity"`
}

// RunIdentity captures who/what the run was and how it ended.
type RunIdentity struct {
	ID         string    `json:"id"`
	Agent      string    `json:"agent,omitempty"`
	Prompt     string    `json:"prompt,omitempty"` // redacted on assembly
	Status     string    `json:"status,omitempty"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	ExitCode   int       `json:"exit_code"`
}

// Decision is one governance audit row, projected for export.
type Decision struct {
	Result     string    `json:"result"`
	ActionType string    `json:"action_type"`
	PolicyName string    `json:"policy_name,omitempty"`
	Reasons    []string  `json:"reasons,omitempty"`
	Approver   string    `json:"approver,omitempty"`
	At         time.Time `json:"at"`
}

// Summary is the aggregate view over Decisions.
type Summary struct {
	Total         int            `json:"total"`
	ByResult      map[string]int `json:"by_result"`
	PoliciesFired int            `json:"policies_fired"`
}

// Integrity is a tamper-evident digest over the Decisions slice.
type Integrity struct {
	Algorithm string `json:"algorithm"`
	Digest    string `json:"digest"`
}

// ReportInput carries already-fetched primitives. The package never reaches
// into a store; callers fill this from their own sources.
type ReportInput struct {
	Run       RunIdentity
	TraceID   string
	AuditRows []governance.AuditRow
	Now       time.Time // injected for deterministic GeneratedAt
}

// Build assembles a Report. It is pure and deterministic for a given input.
// The run prompt is redacted defense-in-depth (data is already redacted at
// rest). The integrity digest is SHA-256 over the canonical JSON of Decisions.
func Build(in ReportInput) Report {
	run := in.Run
	run.Prompt = redact.Redact(run.Prompt)

	decisions := make([]Decision, 0, len(in.AuditRows))
	byResult := map[string]int{}
	fired := map[string]struct{}{}
	for _, r := range in.AuditRows {
		d := Decision{
			Result:     string(r.Result),
			ActionType: string(r.ActionType),
			PolicyName: r.PolicyName,
			Reasons:    r.Reasons,
			Approver:   r.Approver,
			At:         r.CreatedAt,
		}
		decisions = append(decisions, d)
		byResult[d.Result]++
		if d.PolicyName != "" && d.Result != string(governance.Allow) {
			fired[d.PolicyName] = struct{}{}
		}
	}

	return Report{
		SchemaVersion: SchemaVersion,
		GeneratedAt:   in.Now,
		Run:           run,
		TraceID:       in.TraceID,
		Decisions:     decisions,
		Summary: Summary{
			Total:         len(decisions),
			ByResult:      byResult,
			PoliciesFired: len(fired),
		},
		Integrity: digestOf(decisions),
	}
}

// digestOf hashes the canonical JSON of decisions. Decision is a struct (no
// maps), so json.Marshal is deterministic — an auditor re-verifies by
// re-marshaling report.decisions and re-hashing with SHA-256.
func digestOf(decisions []Decision) Integrity {
	b, _ := json.Marshal(decisions)
	sum := sha256.Sum256(b)
	return Integrity{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}
}
