// Refine loop — Stage 2 verify+retry pipeline.
//
// Lifecycle:
//
//	agent.executing → agent.completed → verify.started
//	  → verify.passed                                          → run completes
//	  → verify.failed (attempt < max) → refine.triggered       → agent.executing (attempt++)
//	  → verify.failed (attempt = max)                          → run fails
//
// The refine loop is opt-in via RunOptions.Refine.Enabled. When disabled,
// Run() behaves exactly as before (single attempt, no verify).
//
// Design choices:
//
//   - Verify runs in the SAME sandbox as the agent — keeps the worktree state
//     intact across attempts so the agent can incrementally iterate.
//   - Refine prompts are built deterministically from the previous attempt's
//     verify output. No LLM call in the loop control itself.
//   - The MaxAttempts cap is enforced HERE in addition to the runtime state
//     machine (defense in depth). The state machine also short-circuits if
//     it ever sees refine.triggered past the cap.

package orchestrator

import (
	"fmt"
	"strings"
	"time"
)

// RefineOptions configures the verify+refine post-agent loop.
//
// When Enabled is false, Run() executes a single agent attempt and skips
// verification entirely — the existing behavior. When Enabled is true and
// VerifyCmd is non-empty, the orchestrator runs VerifyCmd after each agent
// attempt; on failure it loops up to MaxAttempts.
type RefineOptions struct {
	// Enabled turns the refine pipeline on. When false, all other fields
	// are ignored (kept for forward-compat with explicit zero-value).
	Enabled bool

	// VerifyCmd is the command run inside the sandbox to verify the agent's
	// changes — e.g. ["go", "test", "./..."] or ["pytest", "-x"]. Required
	// when Enabled is true; an empty VerifyCmd silently degrades to single-
	// attempt behavior (no error — useful for callers building configs
	// incrementally).
	VerifyCmd []string

	// MaxAttempts caps the total agent invocations (1 = no refine, just
	// run verify once at the end). Defaults to 3 via effective() when zero.
	MaxAttempts int

	// VerifyTimeout caps wall-clock for each verify invocation. Zero =
	// inherits the run-level Timeout from RunOptions.
	VerifyTimeout time.Duration

	// VerifyTailBytes caps the size of the verify output captured into
	// the verify.failed event payload and the next attempt's refine prompt.
	// Defaults to 2000 via effective() when zero.
	VerifyTailBytes int
}

// effective returns a copy of the options with zero-valued knobs filled
// in with conventional defaults. Centralizing this keeps the loop logic
// and the test fixtures using identical defaults.
func (r RefineOptions) effective() RefineOptions {
	if r.MaxAttempts <= 0 {
		r.MaxAttempts = 3
	}
	if r.VerifyTailBytes <= 0 {
		r.VerifyTailBytes = 2000
	}
	return r
}

// active reports whether refine should actually engage on this run.
// Requires Enabled AND a non-empty VerifyCmd — callers building configs
// incrementally don't trip the loop just by flipping Enabled.
func (r RefineOptions) active() bool {
	return r.Enabled && len(r.VerifyCmd) > 0
}

// buildRefinePrompt augments the previous prompt with the verify failure
// feedback for the next attempt. The format intentionally mirrors what
// a senior engineer would say after a failing test: "this is what you
// tried, this is what failed, fix it." Keeping it deterministic (no LLM)
// makes refine cycles replay-stable.
//
// The original enriched prompt is preserved at the bottom so all the
// system-prompt + lessons context the kernel injected still applies.
func buildRefinePrompt(originalEnriched, verifyTail string, nextAttempt, maxAttempts int, verifyCmd []string) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("[SANDCODE REFINE — Attempt %d of %d]\n\n", nextAttempt, maxAttempts))
	b.WriteString("## Previous Attempt Failed Verification\n\n")
	b.WriteString(fmt.Sprintf("The verifier (`%s`) reported a failure. Output tail:\n\n",
		strings.Join(verifyCmd, " ")))
	b.WriteString("```\n")
	b.WriteString(strings.TrimSpace(verifyTail))
	b.WriteString("\n```\n\n")
	b.WriteString("## What to Do\n")
	b.WriteString("- Inspect the diff you produced and the failure output above.\n")
	b.WriteString("- Fix the specific root cause; do not revert unrelated changes.\n")
	b.WriteString("- If you cannot fix it confidently, declare the gap explicitly.\n")
	b.WriteString("- Do NOT re-run the verifier yourself — the orchestrator will.\n\n")
	b.WriteString("---\n\n")
	b.WriteString("## Original Task Context (re-stated)\n\n")
	b.WriteString(originalEnriched)
	return b.String()
}

// tail returns the last n bytes of s, prefixed with "...\n" when truncated.
// Used to cap verify output before it lands in event payloads or refine
// prompts so we don't blow the context window.
func tail(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return "...(truncated)\n" + s[len(s)-n:]
}
