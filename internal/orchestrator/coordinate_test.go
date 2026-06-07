package orchestrator

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/agent"
	"github.com/felipemarques-rec/sandcode/internal/event"
	gitm "github.com/felipemarques-rec/sandcode/internal/git"
	"github.com/felipemarques-rec/sandcode/internal/judge"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
	"github.com/felipemarques-rec/sandcode/internal/secreview"
)

func baseCoordinateOpts(t *testing.T, repo string) CoordinateOptions {
	return CoordinateOptions{RunOptions: RunOptions{
		Prompt:         "do the thing",
		CWD:            repo,
		SandboxImage:   "ignored",
		SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", t.Name(), "0"),
		Strategy:       gitm.StrategyMergeToHead,
		AgentOpts:      agent.RunOptions{},
	}}
}

func TestCoordinate_NilReporter_PassThrough(t *testing.T) {
	repo := initRepo(t)
	lb := event.NewLocalBus()
	t.Cleanup(func() { _ = lb.Close() })
	rec := &recorder{}
	lb.Subscribe("*", rec.handle)

	opts := baseCoordinateOpts(t, repo)
	opts.Bus = lb
	events, await, err := Coordinate(context.Background(),
		sandbox.NewNoSandboxProvider(), &fakeAgent{script: "echo hi"}, &noopAuth{}, opts)
	if err != nil {
		t.Fatalf("Coordinate: %v", err)
	}
	for range events {
	}
	res := await()
	if res.Report != nil {
		t.Fatalf("expected nil Report on pass-through, got %+v", res.Report)
	}
	if rec.count(event.ReportGenerated) != 0 {
		t.Fatalf("pass-through must not emit report.generated")
	}
	if res.Run.Status != "success" {
		t.Fatalf("run status: %q", res.Run.Status)
	}
}

func TestCoordinate_WithReporter_EmitsReportAfterAwait(t *testing.T) {
	repo := initRepo(t)
	lb := event.NewLocalBus()
	t.Cleanup(func() { _ = lb.Close() })
	rec := &recorder{}
	lb.Subscribe("*", rec.handle)

	opts := baseCoordinateOpts(t, repo)
	opts.Bus = lb
	opts.KeepWorktree = true
	opts.Reporter = templateReporter{}

	events, await, err := Coordinate(context.Background(),
		sandbox.NewNoSandboxProvider(), &fakeAgent{script: "echo hi > out.txt"}, &noopAuth{}, opts)
	if err != nil {
		t.Fatalf("Coordinate: %v", err)
	}
	if rec.count(event.ReportGenerated) != 0 {
		t.Fatalf("report emitted before await")
	}
	for range events {
	}
	res := await()
	if res.Report == nil || !strings.Contains(res.Report.Markdown, "Sandcode Run Report") {
		t.Fatalf("missing report markdown: %+v", res.Report)
	}
	if rec.count(event.ReportGenerated) != 1 {
		t.Fatalf("want exactly 1 report.generated, got %d", rec.count(event.ReportGenerated))
	}
	if res.Report.Path == "" {
		t.Fatalf("expected REPORT.md path when KeepWorktree set")
	}
	if err := statFile(res.Report.Path); err != nil {
		t.Fatalf("REPORT.md not written: %v", err)
	}
}

func TestTemplateReporter_Deterministic(t *testing.T) {
	in := ReportInput{
		RunID:  "run-123",
		Prompt: "add a feature",
		Result: Result{Status: "success", ExitCode: 0, Attempts: 2, Diff: "diff --git a/x.go b/x.go\n+foo\n"},
		Verify: &VerifyOutput{Passed: true, ExitCode: 0, StdoutTail: "ok"},
	}
	a, err := templateReporter{}.Report(context.Background(), in)
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	b, _ := templateReporter{}.Report(context.Background(), in)
	if a.Markdown != b.Markdown {
		t.Fatalf("templateReporter not deterministic")
	}
	if !strings.Contains(a.Markdown, "run-123") || !strings.Contains(a.Markdown, "Attempts:** 2") {
		t.Fatalf("report missing fields: %s", a.Markdown)
	}
}

// errReporter is a stub Reporter that always fails.
type errReporter struct{}

func (errReporter) Report(_ context.Context, _ ReportInput) (ReportOutput, error) {
	return ReportOutput{}, errors.New("boom")
}

func TestCoordinate_ReporterError_NoEventNoOverride(t *testing.T) {
	repo := initRepo(t)
	lb := event.NewLocalBus()
	t.Cleanup(func() { _ = lb.Close() })
	rec := &recorder{}
	lb.Subscribe("*", rec.handle)

	opts := baseCoordinateOpts(t, repo)
	opts.Bus = lb
	opts.Reporter = errReporter{}

	events, await, err := Coordinate(context.Background(),
		sandbox.NewNoSandboxProvider(), &fakeAgent{script: "echo hi"}, &noopAuth{}, opts)
	if err != nil {
		t.Fatalf("Coordinate: %v", err)
	}
	for range events {
	}
	res := await()

	// Best-effort: reporter error must not override the run outcome.
	if res.Run.Status != "success" {
		t.Fatalf("run status should be success, got %q", res.Run.Status)
	}
	// No report attached when reporter returns an error.
	if res.Report != nil {
		t.Fatalf("expected nil Report on reporter error, got %+v", res.Report)
	}
	// No report.generated event on error.
	if rec.count(event.ReportGenerated) != 0 {
		t.Fatalf("want 0 report.generated on error, got %d", rec.count(event.ReportGenerated))
	}
}

func TestTemplateReporter_ContentOnlyWhenNoWorktree(t *testing.T) {
	out, err := templateReporter{}.Report(context.Background(), ReportInput{
		RunID:  "r1",
		Prompt: "p",
		Result: Result{Status: "success"},
	})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	// No worktree → no file written.
	if out.Path != "" {
		t.Fatalf("expected empty Path when no worktree, got %q", out.Path)
	}
	// Markdown must be populated and contain the expected header.
	if out.Markdown == "" || !strings.Contains(out.Markdown, "Sandcode Run Report") {
		t.Fatalf("expected non-empty markdown with header, got %q", out.Markdown)
	}
}

func TestCoordinate_ReportShowsVerificationResult(t *testing.T) {
	repo := initRepo(t)
	opts := baseCoordinateOpts(t, repo)
	opts.KeepWorktree = true
	opts.Reporter = templateReporter{}
	opts.Refine = RefineOptions{Enabled: true, VerifyCmd: []string{"true"}, MaxAttempts: 1}

	events, await, err := Coordinate(context.Background(),
		sandbox.NewNoSandboxProvider(), &fakeAgent{script: "echo hi"}, &noopAuth{}, opts)
	if err != nil {
		t.Fatalf("Coordinate: %v", err)
	}
	for range events {
	}
	res := await()
	if res.Report == nil {
		t.Fatalf("expected a report")
	}
	if strings.Contains(res.Report.Markdown, "no verifier configured") {
		t.Fatalf("report should reflect the verify result, not 'no verifier configured':\n%s", res.Report.Markdown)
	}
	if !strings.Contains(res.Report.Markdown, "passed") {
		t.Fatalf("report Verification section should say passed:\n%s", res.Report.Markdown)
	}
}

func TestCoordinate_ReportShowsVerificationFailure(t *testing.T) {
	repo := initRepo(t)
	opts := baseCoordinateOpts(t, repo)
	opts.KeepWorktree = true
	opts.Reporter = templateReporter{}
	// Verify command exits non-zero → run ends on verify failure.
	opts.Refine = RefineOptions{Enabled: true, VerifyCmd: []string{"false"}, MaxAttempts: 1}

	events, await, err := Coordinate(context.Background(),
		sandbox.NewNoSandboxProvider(), &fakeAgent{script: "echo hi"}, &noopAuth{}, opts)
	if err != nil {
		t.Fatalf("Coordinate: %v", err)
	}
	for range events {
	}
	res := await()
	if res.Report == nil {
		t.Fatalf("expected a report even on verify failure")
	}
	if strings.Contains(res.Report.Markdown, "no verifier configured") {
		t.Fatalf("verify ran and FAILED — report must not say 'no verifier configured':\n%s", res.Report.Markdown)
	}
	if !strings.Contains(res.Report.Markdown, "failed") {
		t.Fatalf("report Verification section should say failed:\n%s", res.Report.Markdown)
	}
	if res.Run.LastVerify == nil || res.Run.LastVerify.Passed {
		t.Fatalf("expected a failed LastVerify verdict, got %+v", res.Run.LastVerify)
	}
}

// stubReviewer returns a fixed Review (or error when errMsg != "").
type stubReviewer struct {
	rv     judge.Review
	errMsg string
}

func (s stubReviewer) Review(_ context.Context, _ judge.ReviewRequest) (judge.Review, error) {
	if s.errMsg != "" {
		return judge.Review{}, errors.New(s.errMsg)
	}
	return s.rv, nil
}

func TestCoordinate_ReviewThreadedIntoReportAndEvent(t *testing.T) {
	repo := initRepo(t)
	lb := event.NewLocalBus()
	t.Cleanup(func() { _ = lb.Close() })
	rec := &recorder{}
	lb.Subscribe("*", rec.handle)

	opts := baseCoordinateOpts(t, repo)
	opts.Bus = lb
	opts.KeepWorktree = true
	opts.Reporter = templateReporter{}
	opts.Reviewer = stubReviewer{rv: judge.Review{Score: 0.75, Comments: "tidy diff", Reviewer: "llm:x"}}

	events, await, err := Coordinate(context.Background(),
		sandbox.NewNoSandboxProvider(), &fakeAgent{script: "echo hi > out.txt"}, &noopAuth{}, opts)
	if err != nil {
		t.Fatalf("Coordinate: %v", err)
	}
	for range events {
	}
	res := await()
	if res.Review == nil || res.Review.Score != 0.75 {
		t.Fatalf("CoordinateResult.Review missing: %+v", res.Review)
	}
	if res.Report == nil || !strings.Contains(res.Report.Markdown, "## Review") ||
		!strings.Contains(res.Report.Markdown, "tidy diff") ||
		!strings.Contains(res.Report.Markdown, "0.75") {
		t.Fatalf("report missing Review section/score: %+v", res.Report)
	}
	if rec.count(event.ReviewGenerated) != 1 {
		t.Fatalf("want exactly 1 review.generated, got %d", rec.count(event.ReviewGenerated))
	}
}

func TestCoordinate_NilReviewer_NoReviewSection(t *testing.T) {
	repo := initRepo(t)
	lb := event.NewLocalBus()
	t.Cleanup(func() { _ = lb.Close() })
	rec := &recorder{}
	lb.Subscribe("*", rec.handle)

	opts := baseCoordinateOpts(t, repo)
	opts.Bus = lb
	opts.KeepWorktree = true
	opts.Reporter = templateReporter{}

	events, await, err := Coordinate(context.Background(),
		sandbox.NewNoSandboxProvider(), &fakeAgent{script: "echo hi"}, &noopAuth{}, opts)
	if err != nil {
		t.Fatalf("Coordinate: %v", err)
	}
	for range events {
	}
	res := await()
	if res.Review != nil {
		t.Fatalf("expected nil Review, got %+v", res.Review)
	}
	if res.Report == nil || strings.Contains(res.Report.Markdown, "## Review") {
		t.Fatalf("nil reviewer must omit Review section")
	}
	if rec.count(event.ReviewGenerated) != 0 {
		t.Fatalf("nil reviewer must not emit review.generated")
	}
}

func TestCoordinate_ReviewerError_BestEffort(t *testing.T) {
	repo := initRepo(t)
	lb := event.NewLocalBus()
	t.Cleanup(func() { _ = lb.Close() })
	rec := &recorder{}
	lb.Subscribe("*", rec.handle)

	opts := baseCoordinateOpts(t, repo)
	opts.Bus = lb
	opts.Reporter = templateReporter{}
	opts.Reviewer = stubReviewer{errMsg: "boom"}

	events, await, err := Coordinate(context.Background(),
		sandbox.NewNoSandboxProvider(), &fakeAgent{script: "echo hi"}, &noopAuth{}, opts)
	if err != nil {
		t.Fatalf("Coordinate: %v", err)
	}
	for range events {
	}
	res := await()
	if res.Review != nil {
		t.Fatalf("reviewer error must leave Review nil, got %+v", res.Review)
	}
	if rec.count(event.ReviewGenerated) != 0 {
		t.Fatalf("reviewer error must not emit review.generated")
	}
	if res.Run.Status != "success" {
		t.Fatalf("reviewer error must not change run status: %q", res.Run.Status)
	}
}

func TestCoordinate_ReviewWithoutReporter(t *testing.T) {
	repo := initRepo(t)
	lb := event.NewLocalBus()
	t.Cleanup(func() { _ = lb.Close() })
	rec := &recorder{}
	lb.Subscribe("*", rec.handle)

	opts := baseCoordinateOpts(t, repo)
	opts.Bus = lb
	opts.Reviewer = stubReviewer{rv: judge.Review{Score: 0.5, Comments: "ok", Reviewer: "llm:x"}}

	events, await, err := Coordinate(context.Background(),
		sandbox.NewNoSandboxProvider(), &fakeAgent{script: "echo hi"}, &noopAuth{}, opts)
	if err != nil {
		t.Fatalf("Coordinate: %v", err)
	}
	for range events {
	}
	res := await()
	if res.Report != nil {
		t.Fatalf("no reporter ⇒ nil Report, got %+v", res.Report)
	}
	if res.Review == nil || res.Review.Score != 0.5 {
		t.Fatalf("review must surface without a reporter: %+v", res.Review)
	}
	if rec.count(event.ReviewGenerated) != 1 {
		t.Fatalf("want 1 review.generated, got %d", rec.count(event.ReviewGenerated))
	}
}

type stubSecReviewer struct {
	rep    secreview.SecReport
	errMsg string
}

func (s stubSecReviewer) Review(_ context.Context, _ secreview.SecRequest) (secreview.SecReport, error) {
	if s.errMsg != "" {
		return secreview.SecReport{}, errors.New(s.errMsg)
	}
	return s.rep, nil
}

func TestCoordinate_SecurityThreadedIntoReportAndEvent(t *testing.T) {
	repo := initRepo(t)
	lb := event.NewLocalBus()
	t.Cleanup(func() { _ = lb.Close() })
	rec := &recorder{}
	lb.Subscribe("*", rec.handle)

	opts := baseCoordinateOpts(t, repo)
	opts.Bus = lb
	opts.KeepWorktree = true
	opts.Reporter = templateReporter{}
	opts.SecurityReviewer = stubSecReviewer{rep: secreview.SecReport{
		Findings: []secreview.SecFinding{{Rule: "anthropic", Severity: "high", Detail: "secret pattern introduced"}},
		Reviewer: "deterministic:secrets",
	}}

	events, await, err := Coordinate(context.Background(),
		sandbox.NewNoSandboxProvider(), &fakeAgent{script: "echo hi > out.txt"}, &noopAuth{}, opts)
	if err != nil {
		t.Fatalf("Coordinate: %v", err)
	}
	for range events {
	}
	res := await()
	if res.Security == nil || len(res.Security.Findings) != 1 {
		t.Fatalf("CoordinateResult.Security missing: %+v", res.Security)
	}
	if res.Report == nil || !strings.Contains(res.Report.Markdown, "## Security") ||
		!strings.Contains(res.Report.Markdown, "anthropic") {
		t.Fatalf("report missing Security section: %+v", res.Report)
	}
	if rec.count(event.SecurityReviewed) != 1 {
		t.Fatalf("want exactly 1 security.reviewed, got %d", rec.count(event.SecurityReviewed))
	}
}

func TestCoordinate_NilSecurityReviewer_NoSection(t *testing.T) {
	repo := initRepo(t)
	lb := event.NewLocalBus()
	t.Cleanup(func() { _ = lb.Close() })
	rec := &recorder{}
	lb.Subscribe("*", rec.handle)

	opts := baseCoordinateOpts(t, repo)
	opts.Bus = lb
	opts.KeepWorktree = true
	opts.Reporter = templateReporter{}

	events, await, err := Coordinate(context.Background(),
		sandbox.NewNoSandboxProvider(), &fakeAgent{script: "echo hi"}, &noopAuth{}, opts)
	if err != nil {
		t.Fatalf("Coordinate: %v", err)
	}
	for range events {
	}
	res := await()
	if res.Security != nil {
		t.Fatalf("expected nil Security, got %+v", res.Security)
	}
	if res.Report == nil || strings.Contains(res.Report.Markdown, "## Security") {
		t.Fatalf("nil security reviewer must omit Security section")
	}
	if rec.count(event.SecurityReviewed) != 0 {
		t.Fatalf("nil security reviewer must not emit security.reviewed")
	}
}

func TestCoordinate_SecurityReviewerError_BestEffort(t *testing.T) {
	repo := initRepo(t)
	lb := event.NewLocalBus()
	t.Cleanup(func() { _ = lb.Close() })
	rec := &recorder{}
	lb.Subscribe("*", rec.handle)

	opts := baseCoordinateOpts(t, repo)
	opts.Bus = lb
	opts.Reporter = templateReporter{}
	opts.SecurityReviewer = stubSecReviewer{errMsg: "boom"}

	events, await, err := Coordinate(context.Background(),
		sandbox.NewNoSandboxProvider(), &fakeAgent{script: "echo hi"}, &noopAuth{}, opts)
	if err != nil {
		t.Fatalf("Coordinate: %v", err)
	}
	for range events {
	}
	res := await()
	if res.Security != nil {
		t.Fatalf("error must leave Security nil, got %+v", res.Security)
	}
	if rec.count(event.SecurityReviewed) != 0 {
		t.Fatalf("error must not emit security.reviewed")
	}
	if res.Run.Status != "success" {
		t.Fatalf("error must not change run status: %q", res.Run.Status)
	}
}

func TestCoordinate_SecurityNoFindings_RendersNoFindings(t *testing.T) {
	repo := initRepo(t)
	opts := baseCoordinateOpts(t, repo)
	opts.KeepWorktree = true
	opts.Reporter = templateReporter{}
	opts.SecurityReviewer = stubSecReviewer{rep: secreview.SecReport{Reviewer: "deterministic:secrets"}}

	events, await, err := Coordinate(context.Background(),
		sandbox.NewNoSandboxProvider(), &fakeAgent{script: "echo hi"}, &noopAuth{}, opts)
	if err != nil {
		t.Fatalf("Coordinate: %v", err)
	}
	for range events {
	}
	res := await()
	if res.Report == nil || !strings.Contains(res.Report.Markdown, "## Security") ||
		!strings.Contains(res.Report.Markdown, "no findings") {
		t.Fatalf("clean report must render '## Security' + 'no findings': %+v", res.Report)
	}
}

func TestCoordinate_PerformanceThreadedIntoReportAndEvent(t *testing.T) {
	repo := initRepo(t)
	lb := event.NewLocalBus()
	t.Cleanup(func() { _ = lb.Close() })
	rec := &recorder{}
	lb.Subscribe("*", rec.handle)

	opts := baseCoordinateOpts(t, repo)
	opts.Bus = lb
	opts.KeepWorktree = true
	opts.Reporter = templateReporter{}
	opts.PerformanceReviewer = stubReviewer{rv: judge.Review{Score: 0.6, Comments: "allocates in a loop", Reviewer: "llm:x"}}

	events, await, err := Coordinate(context.Background(),
		sandbox.NewNoSandboxProvider(), &fakeAgent{script: "echo hi > out.txt"}, &noopAuth{}, opts)
	if err != nil {
		t.Fatalf("Coordinate: %v", err)
	}
	for range events {
	}
	res := await()
	if res.Performance == nil || res.Performance.Score != 0.6 {
		t.Fatalf("CoordinateResult.Performance missing: %+v", res.Performance)
	}
	if res.Report == nil || !strings.Contains(res.Report.Markdown, "## Performance") ||
		!strings.Contains(res.Report.Markdown, "allocates in a loop") {
		t.Fatalf("report missing Performance section: %+v", res.Report)
	}
	if rec.count(event.PerformanceReviewed) != 1 {
		t.Fatalf("want exactly 1 perf.reviewed, got %d", rec.count(event.PerformanceReviewed))
	}
}

func TestCoordinate_RefactoringThreadedIntoReportAndEvent(t *testing.T) {
	repo := initRepo(t)
	lb := event.NewLocalBus()
	t.Cleanup(func() { _ = lb.Close() })
	rec := &recorder{}
	lb.Subscribe("*", rec.handle)

	opts := baseCoordinateOpts(t, repo)
	opts.Bus = lb
	opts.KeepWorktree = true
	opts.Reporter = templateReporter{}
	opts.RefactoringSpecialist = stubReviewer{rv: judge.Review{Score: 0.3, Comments: "extract a helper", Reviewer: "llm:x"}}

	events, await, err := Coordinate(context.Background(),
		sandbox.NewNoSandboxProvider(), &fakeAgent{script: "echo hi > out.txt"}, &noopAuth{}, opts)
	if err != nil {
		t.Fatalf("Coordinate: %v", err)
	}
	for range events {
	}
	res := await()
	if res.Refactoring == nil || res.Refactoring.Score != 0.3 {
		t.Fatalf("CoordinateResult.Refactoring missing: %+v", res.Refactoring)
	}
	if res.Report == nil || !strings.Contains(res.Report.Markdown, "## Refactoring") ||
		!strings.Contains(res.Report.Markdown, "extract a helper") {
		t.Fatalf("report missing Refactoring section: %+v", res.Report)
	}
	if rec.count(event.RefactoringReviewed) != 1 {
		t.Fatalf("want exactly 1 refactor.reviewed, got %d", rec.count(event.RefactoringReviewed))
	}
}

func TestCoordinate_NilLenses_NoSections(t *testing.T) {
	repo := initRepo(t)
	lb := event.NewLocalBus()
	t.Cleanup(func() { _ = lb.Close() })
	rec := &recorder{}
	lb.Subscribe("*", rec.handle)

	opts := baseCoordinateOpts(t, repo)
	opts.Bus = lb
	opts.KeepWorktree = true
	opts.Reporter = templateReporter{}

	events, await, err := Coordinate(context.Background(),
		sandbox.NewNoSandboxProvider(), &fakeAgent{script: "echo hi"}, &noopAuth{}, opts)
	if err != nil {
		t.Fatalf("Coordinate: %v", err)
	}
	for range events {
	}
	res := await()
	if res.Performance != nil || res.Refactoring != nil {
		t.Fatalf("expected nil lenses, got perf=%+v refactor=%+v", res.Performance, res.Refactoring)
	}
	if res.Report == nil || strings.Contains(res.Report.Markdown, "## Performance") ||
		strings.Contains(res.Report.Markdown, "## Refactoring") {
		t.Fatalf("nil lenses must omit both sections")
	}
	if rec.count(event.PerformanceReviewed) != 0 || rec.count(event.RefactoringReviewed) != 0 {
		t.Fatalf("nil lenses must not emit events")
	}
}

func TestCoordinate_LensError_BestEffort(t *testing.T) {
	repo := initRepo(t)
	lb := event.NewLocalBus()
	t.Cleanup(func() { _ = lb.Close() })
	rec := &recorder{}
	lb.Subscribe("*", rec.handle)

	opts := baseCoordinateOpts(t, repo)
	opts.Bus = lb
	opts.Reporter = templateReporter{}
	opts.PerformanceReviewer = stubReviewer{errMsg: "boom"}

	events, await, err := Coordinate(context.Background(),
		sandbox.NewNoSandboxProvider(), &fakeAgent{script: "echo hi"}, &noopAuth{}, opts)
	if err != nil {
		t.Fatalf("Coordinate: %v", err)
	}
	for range events {
	}
	res := await()
	if res.Performance != nil {
		t.Fatalf("error must leave Performance nil, got %+v", res.Performance)
	}
	if rec.count(event.PerformanceReviewed) != 0 {
		t.Fatalf("error must not emit perf.reviewed")
	}
	if res.Run.Status != "success" {
		t.Fatalf("error must not change run status: %q", res.Run.Status)
	}
}
