// Coordination spine (SP2a + SP2b). Coordinate is a sibling to Run/ParallelRun/
// DAGRun: it runs a single Run, then — after the caller awaits the result —
// optionally invokes a Reviewer (SP2b) to score the diff, then a Reporter to
// produce a report artifact, emitting observation-only review.generated and
// report.generated events. Both run strictly post-await, so Run's W13-sensitive
// streaming goroutine is never touched. Both are observational and best-effort;
// with Reviewer and Reporter nil, Coordinate is a byte-identical pass-through.
package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/felipemarques-rec/sandcode/internal/agent"
	"github.com/felipemarques-rec/sandcode/internal/auth"
	"github.com/felipemarques-rec/sandcode/internal/event"
	"github.com/felipemarques-rec/sandcode/internal/judge"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
	"github.com/felipemarques-rec/sandcode/internal/secreview"
)

// Reporter produces a report artifact from a completed run's result.
type Reporter interface {
	Report(ctx context.Context, in ReportInput) (ReportOutput, error)
}

// ReportInput is the data a Reporter consumes.
type ReportInput struct {
	RunID       string
	Prompt      string               // original prompt (not the enriched one)
	Result      Result               // diff, status, exitCode, attempts, started/finished
	Verify      *VerifyOutput        // last verify, if any (nil when refine inactive)
	Review      *judge.Review        // reviewer verdict, if any (nil when no reviewer)
	Security    *secreview.SecReport // security findings, if any (nil when none ran)
	Performance *judge.Review        // performance advisor verdict, if any (nil when none ran)
	Refactoring *judge.Review        // refactoring advisor verdict, if any (nil when none ran)
	Worktree    string               // host path of the worktree (when KeepWorktree)
}

// ReportOutput is a Reporter's artifact.
type ReportOutput struct {
	Path     string // path of the written REPORT.md (empty if content-only)
	Markdown string // generated content
}

// CoordinateOptions configures Coordinate. It embeds RunOptions (so Verifier,
// Registry, Refine, etc. all apply to the underlying Run) and adds an optional
// Reviewer and Reporter.
type CoordinateOptions struct {
	RunOptions
	Reporter              Reporter                   // nil ⇒ no report (byte-identical pass-through)
	Reviewer              judge.Reviewer             // nil ⇒ no review (byte-identical)
	SecurityReviewer      secreview.SecurityReviewer // nil ⇒ no security review (byte-identical)
	PerformanceReviewer   judge.Reviewer             // nil ⇒ no performance review (byte-identical)
	RefactoringSpecialist judge.Reviewer             // nil ⇒ no refactoring review (byte-identical)
}

// CoordinateResult is the outcome of Coordinate.
type CoordinateResult struct {
	Run         Result               // the underlying Run's Result, intact
	Report      *ReportOutput        // nil when no Reporter ran
	Review      *judge.Review        // nil when no Reviewer ran
	Security    *secreview.SecReport // nil when no SecurityReviewer ran
	Performance *judge.Review        // nil when no PerformanceReviewer ran
	Refactoring *judge.Review        // nil when no RefactoringSpecialist ran
}

// reportGeneratedPayload is the JSON wire shape of an event.ReportGenerated
// event. Unexported, matching the package's other event payload structs.
type reportGeneratedPayload struct {
	RunID  string `json:"run_id"`
	Path   string `json:"path,omitempty"`
	Bytes  int    `json:"bytes"`
	Status string `json:"status"`
}

// reviewGeneratedPayload is the JSON wire shape of an event.ReviewGenerated
// event. Unexported, matching the package's other event payload structs.
type reviewGeneratedPayload struct {
	RunID    string  `json:"run_id"`
	Score    float64 `json:"score"`
	Reviewer string  `json:"reviewer"`
}

// securityReviewedPayload is the JSON wire shape of an event.SecurityReviewed
// event. Unexported, matching the package's other event payload structs.
type securityReviewedPayload struct {
	RunID         string `json:"run_id"`
	FindingsCount int    `json:"findings_count"`
	HighCount     int    `json:"high_count"`
	Reviewer      string `json:"reviewer"`
}

func countHigh(fs []secreview.SecFinding) int {
	n := 0
	for _, f := range fs {
		if f.Severity == "high" {
			n++
		}
	}
	return n
}

// runReviewLens runs an optional advisory Reviewer lens (Performance,
// Refactoring) and emits the given observation-only event on success. It is
// best-effort: a nil reviewer or any Review error yields nil (no event, run
// status untouched). It reuses reviewGeneratedPayload — the event TYPE, not
// the payload, distinguishes the lens.
func runReviewLens(ctx context.Context, bus event.Bus, r judge.Reviewer, req judge.ReviewRequest, evType event.Type) *judge.Review {
	if r == nil {
		return nil
	}
	rv, err := r.Review(ctx, req)
	if err != nil {
		return nil
	}
	if bus != nil {
		raw, _ := json.Marshal(reviewGeneratedPayload{RunID: req.RunID, Score: rv.Score, Reviewer: rv.Reviewer})
		ev := event.New(evType, req.RunID, raw).WithCorrelation(req.RunID)
		_ = bus.Publish(ctx, ev)
	}
	return &rv
}

// Coordinate runs one Run and, post-await, an optional Reviewer then Reporter.
func Coordinate(
	ctx context.Context,
	sb sandbox.Provider,
	ag agent.Provider,
	au auth.Provider,
	opts CoordinateOptions,
) (<-chan agent.StreamEvent, func() CoordinateResult, error) {
	events, runAwait, err := Run(ctx, sb, ag, au, opts.RunOptions)
	if err != nil {
		return nil, nil, err
	}

	await := func() CoordinateResult {
		res := runAwait()

		// Reviewer (observational) runs first — before the Reporter — so the
		// report can include it. Best-effort: an error leaves status untouched.
		var review *judge.Review
		if opts.Reviewer != nil {
			rr := judge.ReviewRequest{
				RunID:        res.RunID,
				Prompt:       opts.Prompt,
				Diff:         res.Diff,
				VerifyRan:    res.LastVerify != nil,
				VerifyPassed: res.LastVerify != nil && res.LastVerify.Passed,
				VerifyTail:   tailOrEmpty(res.LastVerify),
			}
			if rv, err := opts.Reviewer.Review(ctx, rr); err == nil {
				review = &rv
				if opts.Bus != nil {
					raw, _ := json.Marshal(reviewGeneratedPayload{
						RunID:    res.RunID,
						Score:    rv.Score,
						Reviewer: rv.Reviewer,
					})
					ev := event.New(event.ReviewGenerated, res.RunID, raw).WithCorrelation(res.RunID)
					_ = opts.Bus.Publish(ctx, ev)
				}
			}
		}

		// SecurityReviewer (observational) runs after the Reviewer, before the
		// Reporter. Best-effort: an error leaves status untouched.
		var security *secreview.SecReport
		if opts.SecurityReviewer != nil {
			sreq := secreview.SecRequest{RunID: res.RunID, Prompt: opts.Prompt, Diff: res.Diff}
			if sr, err := opts.SecurityReviewer.Review(ctx, sreq); err == nil {
				security = &sr
				if opts.Bus != nil {
					raw, _ := json.Marshal(securityReviewedPayload{
						RunID:         res.RunID,
						FindingsCount: len(sr.Findings),
						HighCount:     countHigh(sr.Findings),
						Reviewer:      sr.Reviewer,
					})
					ev := event.New(event.SecurityReviewed, res.RunID, raw).WithCorrelation(res.RunID)
					_ = opts.Bus.Publish(ctx, ev)
				}
			}
		}

		// Advisory lenses (observational): Performance then Refactoring, after
		// the Security Reviewer and before the Reporter. Best-effort — see
		// runReviewLens. One shared ReviewRequest (same shape as the Reviewer
		// block) feeds both.
		var performance, refactoring *judge.Review
		if opts.PerformanceReviewer != nil || opts.RefactoringSpecialist != nil {
			lensReq := judge.ReviewRequest{
				RunID:        res.RunID,
				Prompt:       opts.Prompt,
				Diff:         res.Diff,
				VerifyRan:    res.LastVerify != nil,
				VerifyPassed: res.LastVerify != nil && res.LastVerify.Passed,
				VerifyTail:   tailOrEmpty(res.LastVerify),
			}
			performance = runReviewLens(ctx, opts.Bus, opts.PerformanceReviewer, lensReq, event.PerformanceReviewed)
			refactoring = runReviewLens(ctx, opts.Bus, opts.RefactoringSpecialist, lensReq, event.RefactoringReviewed)
		}

		if opts.Reporter == nil {
			return CoordinateResult{Run: res, Review: review, Security: security, Performance: performance, Refactoring: refactoring}
		}

		wt := ""
		if res.Worktree != nil {
			wt = res.Worktree.Path
		}
		out, rErr := opts.Reporter.Report(ctx, ReportInput{
			RunID:       res.RunID,
			Prompt:      opts.Prompt,
			Result:      res,
			Verify:      res.LastVerify,
			Review:      review,
			Security:    security,
			Performance: performance,
			Refactoring: refactoring,
			Worktree:    wt,
		})
		if rErr != nil {
			// Reporting is best-effort: never override the run's outcome.
			return CoordinateResult{Run: res, Review: review, Security: security, Performance: performance, Refactoring: refactoring}
		}
		if opts.Bus != nil {
			raw, _ := json.Marshal(reportGeneratedPayload{
				RunID:  res.RunID,
				Path:   out.Path,
				Bytes:  len(out.Markdown),
				Status: res.Status,
			})
			ev := event.New(event.ReportGenerated, res.RunID, raw).WithCorrelation(res.RunID)
			_ = opts.Bus.Publish(ctx, ev)
		}
		return CoordinateResult{Run: res, Report: &out, Review: review, Security: security, Performance: performance, Refactoring: refactoring}
	}
	return events, await, nil
}

// templateReporter is the default Reporter: a deterministic Markdown template.
// No LLM, no API key, no new deps. Writes REPORT.md to the worktree root when
// a worktree path is present; always returns the Markdown.
type templateReporter struct{}

func (templateReporter) Report(_ context.Context, in ReportInput) (ReportOutput, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "# Sandcode Run Report — %s\n\n", in.RunID)
	fmt.Fprintf(&b, "- **Status:** %s\n", in.Result.Status)
	fmt.Fprintf(&b, "- **Attempts:** %d\n", in.Result.Attempts)
	fmt.Fprintf(&b, "- **Exit code:** %d\n", in.Result.ExitCode)
	if !in.Result.Started.IsZero() && !in.Result.Finished.IsZero() {
		fmt.Fprintf(&b, "- **Duration:** %s\n", in.Result.Finished.Sub(in.Result.Started))
	}
	files := extractChangedFiles(in.Result.Diff)
	fmt.Fprintf(&b, "- **Diff size:** %d bytes / %d files\n\n", len(in.Result.Diff), len(files))

	fmt.Fprintf(&b, "## Task\n\n%s\n\n", in.Prompt)

	b.WriteString("## Verification\n\n")
	switch {
	case in.Verify == nil:
		b.WriteString("no verifier configured\n\n")
	case in.Verify.Passed:
		b.WriteString("passed\n\n")
	default:
		fmt.Fprintf(&b, "failed (exit %d)\n\n```\n%s\n```\n\n", in.Verify.ExitCode, strings.TrimSpace(in.Verify.StdoutTail))
	}

	if in.Review != nil {
		b.WriteString("## Review\n\n")
		fmt.Fprintf(&b, "- **Score:** %.2f\n", in.Review.Score)
		fmt.Fprintf(&b, "- **Reviewer:** %s\n\n", in.Review.Reviewer)
		if c := strings.TrimSpace(in.Review.Comments); c != "" {
			b.WriteString(c)
			b.WriteString("\n\n")
		}
	}

	if in.Security != nil {
		b.WriteString("## Security\n\n")
		fmt.Fprintf(&b, "- **Reviewer:** %s\n\n", in.Security.Reviewer)
		if len(in.Security.Findings) == 0 {
			b.WriteString("no findings\n\n")
		} else {
			for _, f := range in.Security.Findings {
				fmt.Fprintf(&b, "- [%s] %s — %s\n", f.Severity, f.Rule, f.Detail)
			}
			b.WriteString("\n")
		}
	}

	if in.Performance != nil {
		b.WriteString("## Performance\n\n")
		fmt.Fprintf(&b, "- **Score:** %.2f\n", in.Performance.Score)
		fmt.Fprintf(&b, "- **Reviewer:** %s\n\n", in.Performance.Reviewer)
		if c := strings.TrimSpace(in.Performance.Comments); c != "" {
			b.WriteString(c)
			b.WriteString("\n\n")
		}
	}

	if in.Refactoring != nil {
		b.WriteString("## Refactoring\n\n")
		fmt.Fprintf(&b, "- **Score:** %.2f\n", in.Refactoring.Score)
		fmt.Fprintf(&b, "- **Reviewer:** %s\n\n", in.Refactoring.Reviewer)
		if c := strings.TrimSpace(in.Refactoring.Comments); c != "" {
			b.WriteString(c)
			b.WriteString("\n\n")
		}
	}

	b.WriteString("## Changes\n\n")
	if len(files) == 0 {
		b.WriteString("_(no file changes)_\n")
	} else {
		for _, f := range files {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}

	md := b.String()
	out := ReportOutput{Markdown: md}
	if in.Worktree != "" {
		p := filepath.Join(in.Worktree, "REPORT.md")
		if err := os.WriteFile(p, []byte(md), 0o600); err != nil {
			return out, err // return markdown even on write failure
		}
		out.Path = p
	}
	return out, nil
}

// tailOrEmpty returns the verify stdout tail, or "" when no verify ran.
func tailOrEmpty(v *VerifyOutput) string {
	if v == nil {
		return ""
	}
	return v.StdoutTail
}

// DefaultReporter returns the built-in deterministic template Reporter.
// Used by the CLI to enable reporting via the --report flag.
func DefaultReporter() Reporter { return templateReporter{} }
