package orchestrator

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/agent"
	"github.com/felipemarques-rec/sandcode/internal/event"
	gitm "github.com/felipemarques-rec/sandcode/internal/git"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
)

// driveLintRun is the lint-gate analogue of driveRefineRun: it runs a single
// agent with an explicit Linter Gate command (and optional verify command).
func driveLintRun(t *testing.T, agentScript string, refine RefineOptions, lintCmd []string) (Result, *recorder) {
	t.Helper()
	repo := initRepo(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	lb := event.NewLocalBus()
	t.Cleanup(func() { _ = lb.Close() })
	rec := &recorder{}
	lb.Subscribe("*", rec.handle)

	opts := RunOptions{
		Prompt:         "noop",
		CWD:            repo,
		SandboxImage:   "ignored",
		SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", t.Name(), "0"),
		Strategy:       gitm.StrategyMergeToHead,
		AgentOpts:      agent.RunOptions{},
		Bus:            lb,
		Refine:         refine,
		LintCmd:        lintCmd,
	}

	events, await, err := Run(ctx,
		sandbox.NewNoSandboxProvider(),
		&fakeAgent{script: agentScript},
		&noopAuth{},
		opts,
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for range events { // drain
	}
	return await(), rec
}

// progressiveLintScript writes the lint-ok marker only on attempt >= 2, so the
// lint gate fails the first attempt and passes the second.
const progressiveLintScript = `
counter=.lint-counter
c=$(cat "$counter" 2>/dev/null || echo 0)
c=$((c+1))
echo "$c" > "$counter"
echo "attempt $c"
if [ "$c" -ge 2 ]; then
  echo ok > .lint-ok
  echo "wrote .lint-ok on attempt $c"
fi
`

// TestLintGate_PassesAfterVerify — verify passes, then the lint gate passes:
// one attempt, lint.started + lint.passed, no refine.
func TestLintGate_PassesAfterVerify(t *testing.T) {
	script := `echo ok > fix.txt; echo ok > .lint-ok`
	res, rec := driveLintRun(t, script, RefineOptions{
		Enabled:     true,
		VerifyCmd:   []string{"sh", "-c", "test -f fix.txt"},
		MaxAttempts: 3,
	}, []string{"sh", "-c", "test -f .lint-ok"})

	if res.Status != "success" {
		t.Fatalf("status=%s err=%v", res.Status, res.Err)
	}
	if res.Attempts != 1 {
		t.Fatalf("Attempts=%d, want 1", res.Attempts)
	}
	for _, e := range []event.Type{event.VerifyPassed, event.LintStarted, event.LintPassed} {
		if rec.count(e) != 1 {
			t.Errorf("%s count = %d, want 1; events: %v", e, rec.count(e), rec.types())
		}
	}
	if rec.count(event.LintFailed) != 0 || rec.count(event.RefineTriggered) != 0 {
		t.Errorf("no lint.failed/refine expected; events: %v", rec.types())
	}
}

// TestLintGate_FailsThenRefinePasses — lint fails attempt 1 (no verify cmd),
// triggers a refine, and passes attempt 2.
func TestLintGate_FailsThenRefinePasses(t *testing.T) {
	res, rec := driveLintRun(t, progressiveLintScript, RefineOptions{
		Enabled:     true,
		MaxAttempts: 3,
	}, []string{"sh", "-c", "test -f .lint-ok"})

	if res.Status != "success" {
		t.Fatalf("status=%s err=%v", res.Status, res.Err)
	}
	if res.Attempts != 2 {
		t.Fatalf("Attempts=%d, want 2 (lint failed once, then passed)", res.Attempts)
	}
	if rec.count(event.LintFailed) != 1 {
		t.Errorf("lint.failed = %d, want 1; events: %v", rec.count(event.LintFailed), rec.types())
	}
	if rec.count(event.RefineTriggered) != 1 {
		t.Errorf("refine.triggered = %d, want 1", rec.count(event.RefineTriggered))
	}
	if rec.count(event.LintPassed) != 1 {
		t.Errorf("lint.passed = %d, want 1", rec.count(event.LintPassed))
	}
	if rec.count(event.AgentExecuting) != 2 {
		t.Errorf("agent.executing = %d, want 2 (initial + 1 refine)", rec.count(event.AgentExecuting))
	}
}

// TestLintGate_ExhaustsFailsRun — lint always fails; once attempts are
// exhausted the run fails with a lint-specific error (a true gate).
func TestLintGate_ExhaustsFailsRun(t *testing.T) {
	res, rec := driveLintRun(t, `echo "no marker written"`, RefineOptions{
		Enabled:     true,
		MaxAttempts: 2,
	}, []string{"sh", "-c", "test -f .never-exists"})

	if res.Status != "failure" {
		t.Fatalf("status=%s, want failure; err=%v", res.Status, res.Err)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "lint failed after") {
		t.Fatalf("err = %v, want 'lint failed after ...'", res.Err)
	}
	if rec.count(event.LintFailed) != 2 {
		t.Errorf("lint.failed = %d, want 2 (one per attempt)", rec.count(event.LintFailed))
	}
	if rec.count(event.LintPassed) != 0 {
		t.Errorf("lint.passed should not fire")
	}
}

// TestLintGate_InactiveWhenNoCmd — no LintCmd ⇒ no lint events (byte-identical
// to the legacy verify-only path).
func TestLintGate_InactiveWhenNoCmd(t *testing.T) {
	script := `echo ok > fix.txt`
	_, rec := driveLintRun(t, script, RefineOptions{
		Enabled:     true,
		VerifyCmd:   []string{"sh", "-c", "test -f fix.txt"},
		MaxAttempts: 3,
	}, nil)

	for _, e := range []event.Type{event.LintStarted, event.LintPassed, event.LintFailed} {
		if rec.count(e) != 0 {
			t.Errorf("%s fired with no LintCmd; events: %v", e, rec.types())
		}
	}
}
