// Package secreview implements the Security Reviewer role: it scans a run's
// diff for leaked secrets (deterministic, reusing internal/redact) and,
// optionally, vulnerabilities (LLM). It is observational — the caller folds
// the SecReport into REPORT.md and an event; it never gates the run.
package secreview

import (
	"context"
	"fmt"
	"strings"

	"github.com/felipemarques-rec/sandcode/internal/redact"
)

// SecFinding is a single security issue. Detail MUST NOT echo the secret value.
type SecFinding struct {
	Rule     string // e.g. "anthropic", "sql_injection"
	Severity string // "high" | "medium" | "low"
	Detail   string // human description / location — never the secret itself
}

// SecReport is a Security Reviewer's output.
type SecReport struct {
	Findings []SecFinding
	Reviewer string // "deterministic:secrets" or "llm:<model>"
}

// SecRequest is the input a Security Reviewer consumes.
type SecRequest struct {
	RunID  string
	Prompt string
	Diff   string // the implementation diff to scan
}

// SecurityReviewer scans a diff and returns a SecReport.
type SecurityReviewer interface {
	Review(ctx context.Context, req SecRequest) (SecReport, error)
}

// Scanner is the deterministic default: it scans the ADDED (+) lines of the
// diff for known secret patterns (reusing internal/redact). No LLM, no key.
type Scanner struct{}

// NewScanner returns the deterministic secret Scanner.
func NewScanner() Scanner { return Scanner{} }

// Review scans added diff lines for secret patterns. One finding per matched
// rule (de-duplicated); never echoes the matched value. Never errors.
func (Scanner) Review(_ context.Context, req SecRequest) (SecReport, error) {
	seen := map[string]bool{}
	var findings []SecFinding
	for _, line := range strings.Split(req.Diff, "\n") {
		// Only added lines matter (secrets being introduced); skip the
		// "+++ b/file" file header, which also starts with '+'.
		if !strings.HasPrefix(line, "+") || strings.HasPrefix(line, "+++") {
			continue
		}
		for _, rule := range redact.Scan(line) {
			if seen[rule] {
				continue
			}
			seen[rule] = true
			findings = append(findings, SecFinding{
				Rule:     rule,
				Severity: "high",
				Detail:   fmt.Sprintf("secret pattern %q introduced in diff", rule),
			})
		}
	}
	return SecReport{Findings: findings, Reviewer: "deterministic:secrets"}, nil
}
