package orchestrator

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/agent"
	"github.com/felipemarques-rec/sandcode/internal/event"
	gitm "github.com/felipemarques-rec/sandcode/internal/git"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
)

// recorder is a test-only event collector. It is thread-safe and orders
// events by arrival time — sufficient for asserting "X happened before Y"
// without dragging timestamps into every test assertion.
type recorder struct {
	mu  sync.Mutex
	evs []event.Event
}

func (r *recorder) handle(_ context.Context, ev event.Event) error {
	r.mu.Lock()
	r.evs = append(r.evs, ev)
	r.mu.Unlock()
	return nil
}

func (r *recorder) types() []event.Type {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]event.Type, len(r.evs))
	for i, e := range r.evs {
		out[i] = e.Type
	}
	return out
}

func (r *recorder) count(typ event.Type) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.evs {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func driveRefineRun(t *testing.T, agentScript string, refine RefineOptions, recordBus bool) (Result, *recorder) {
	t.Helper()
	repo := initRepo(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	var bus event.Bus
	var rec *recorder
	if recordBus {
		lb := event.NewLocalBus()
		t.Cleanup(func() { _ = lb.Close() })
		rec = &recorder{}
		lb.Subscribe("*", rec.handle)
		bus = lb
	}

	opts := RunOptions{
		Prompt:         "noop",
		CWD:            repo,
		SandboxImage:   "ignored",
		SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", t.Name(), "0"),
		Strategy:       gitm.StrategyMergeToHead,
		AgentOpts:      agent.RunOptions{},
		Bus:            bus,
		Refine:         refine,
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

// TestRefine_VerifyPassesFirstAttempt — the simplest refine case:
// VerifyCmd is configured and passes immediately. The state machine
// should see one attempt, one verify.started, one verify.passed, and
// no refine.triggered.
func TestRefine_VerifyPassesFirstAttempt(t *testing.T) {
	// Agent writes a file the verifier checks for.
	script := `echo "writing"; echo ok > fix.txt; echo "done"`

	res, rec := driveRefineRun(t, script, RefineOptions{
		Enabled:     true,
		VerifyCmd:   []string{"sh", "-c", "test -f fix.txt"},
		MaxAttempts: 3,
	}, true)

	if res.Status != "success" {
		t.Fatalf("status=%s err=%v", res.Status, res.Err)
	}
	if res.Attempts != 1 {
		t.Fatalf("Attempts=%d, want 1 (verify passed first try)", res.Attempts)
	}
	if rec.count(event.VerifyStarted) != 1 {
		t.Fatalf("verify.started count = %d, want 1; events: %v", rec.count(event.VerifyStarted), rec.types())
	}
	if rec.count(event.VerifyPassed) != 1 {
		t.Fatalf("verify.passed count = %d, want 1", rec.count(event.VerifyPassed))
	}
	if rec.count(event.VerifyFailed) != 0 {
		t.Fatalf("verify.failed should not fire when verify passes")
	}
	if rec.count(event.RefineTriggered) != 0 {
		t.Fatalf("refine.triggered should not fire when verify passes")
	}
	if rec.count(event.AgentExecuting) != 1 {
		t.Fatalf("agent.executing count = %d, want 1 (no refine)", rec.count(event.AgentExecuting))
	}
}

// TestRefine_DisabledByDefault — sanity check that legacy callers
// (Refine.Enabled = false) skip the verify pipeline entirely even when
// VerifyCmd is accidentally populated.
func TestRefine_DisabledByDefault(t *testing.T) {
	script := `echo ok > fix.txt`
	res, rec := driveRefineRun(t, script, RefineOptions{
		// Enabled is false — VerifyCmd must be ignored.
		VerifyCmd:   []string{"false"},
		MaxAttempts: 3,
	}, true)

	if res.Status != "success" {
		t.Fatalf("status=%s err=%v", res.Status, res.Err)
	}
	if res.Attempts != 1 {
		t.Fatalf("Attempts=%d, want 1", res.Attempts)
	}
	if rec.count(event.VerifyStarted) != 0 {
		t.Fatalf("verify.started fired with Refine disabled")
	}
}

// progressiveScript is a fake agent that "iterates" across attempts via
// a counter file in the worktree. First invocation increments and does
// NOT create the success-marker. Second invocation increments and DOES
// create the marker. Used by the refine-convergence test below.
const progressiveScript = `
counter=.attempt-counter
c=$(cat "$counter" 2>/dev/null || echo 0)
c=$((c+1))
echo "$c" > "$counter"
echo "attempt $c"
if [ "$c" -ge 2 ]; then
  echo fixed > .fixed
  echo "wrote .fixed on attempt $c"
fi
`

// TestRefine_VerifyFailsThenRefinePasses drives the canonical refine
// happy path:
//
//	attempt 1: agent runs, no .fixed → verify.failed → refine.triggered
//	attempt 2: agent runs, .fixed exists → verify.passed → completed
//
// Asserts the precise event sequence, attempt counter, and absence of
// any failure-tail leakage into the run's terminal status.
func TestRefine_VerifyFailsThenRefinePasses(t *testing.T) {
	res, rec := driveRefineRun(t, progressiveScript, RefineOptions{
		Enabled:     true,
		VerifyCmd:   []string{"sh", "-c", "test -f .fixed"},
		MaxAttempts: 3,
	}, true)

	if res.Status != "success" {
		t.Fatalf("status=%s err=%v (events: %v)", res.Status, res.Err, rec.types())
	}
	if res.Attempts != 2 {
		t.Fatalf("Attempts=%d, want 2", res.Attempts)
	}

	// Event count assertions.
	if rec.count(event.AgentExecuting) != 2 {
		t.Fatalf("agent.executing count = %d, want 2 (one per attempt)", rec.count(event.AgentExecuting))
	}
	if rec.count(event.AgentCompleted) != 2 {
		t.Fatalf("agent.completed count = %d, want 2", rec.count(event.AgentCompleted))
	}
	if rec.count(event.VerifyStarted) != 2 {
		t.Fatalf("verify.started count = %d, want 2", rec.count(event.VerifyStarted))
	}
	if rec.count(event.VerifyFailed) != 1 {
		t.Fatalf("verify.failed count = %d, want 1", rec.count(event.VerifyFailed))
	}
	if rec.count(event.VerifyPassed) != 1 {
		t.Fatalf("verify.passed count = %d, want 1", rec.count(event.VerifyPassed))
	}
	if rec.count(event.RefineTriggered) != 1 {
		t.Fatalf("refine.triggered count = %d, want 1", rec.count(event.RefineTriggered))
	}

	// Sequence assertion: refine.triggered must come BEFORE the second
	// agent.executing.
	wantSeq := []event.Type{
		event.AgentExecuting,
		event.AgentCompleted,
		event.VerifyStarted,
		event.VerifyFailed,
		event.RefineTriggered,
		event.AgentExecuting,
		event.AgentCompleted,
		event.VerifyStarted,
		event.VerifyPassed,
		event.RunCompleted,
	}
	requireOrderedSubsequence(t, rec.types(), wantSeq)
}

// requireOrderedSubsequence checks that `want` appears as a (not
// necessarily contiguous) subsequence within `got`. Useful when other
// events (sandbox.created/destroyed, agent.tool_called) interleave but
// the relative order of the listed events still matters.
func requireOrderedSubsequence(t *testing.T, got, want []event.Type) {
	t.Helper()
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	if i != len(want) {
		t.Fatalf("event order broken — got %v, wanted ordered subsequence %v (matched %d/%d)",
			got, want, i, len(want))
	}
}

// TestRefine_CapExhaustionFails drives the worst-case scenario: every
// verify fails. After MaxAttempts the run terminates as failure with
// run.failed (not verify.passed). State machine should NOT reach
// PhaseCompleted.
func TestRefine_CapExhaustionFails(t *testing.T) {
	// Agent makes a benign change. Verifier always fails — checks for a
	// file that never gets created.
	script := `echo "did work" > log.txt`

	res, rec := driveRefineRun(t, script, RefineOptions{
		Enabled:     true,
		VerifyCmd:   []string{"sh", "-c", "test -f .never-exists"},
		MaxAttempts: 2,
	}, true)

	if res.Status != "failure" {
		t.Fatalf("status=%s, want failure (events: %v)", res.Status, rec.types())
	}
	if res.Attempts != 2 {
		t.Fatalf("Attempts=%d, want 2 (cap hit)", res.Attempts)
	}

	// Each attempt should fire one verify pair + we should have exactly
	// one refine.triggered (between attempt 1 and attempt 2).
	if rec.count(event.AgentExecuting) != 2 {
		t.Fatalf("agent.executing count = %d, want 2", rec.count(event.AgentExecuting))
	}
	if rec.count(event.VerifyFailed) != 2 {
		t.Fatalf("verify.failed count = %d, want 2", rec.count(event.VerifyFailed))
	}
	if rec.count(event.VerifyPassed) != 0 {
		t.Fatalf("verify.passed count = %d, want 0", rec.count(event.VerifyPassed))
	}
	if rec.count(event.RefineTriggered) != 1 {
		t.Fatalf("refine.triggered count = %d, want 1 (cap=2 means 1 retry)", rec.count(event.RefineTriggered))
	}
	if rec.count(event.RunCompleted) != 0 {
		t.Fatalf("run.completed should NOT fire when cap exhausted")
	}
	if rec.count(event.RunFailed) != 1 {
		t.Fatalf("run.failed count = %d, want 1", rec.count(event.RunFailed))
	}

	// The Result error must carry the verify-failure context.
	if res.Err == nil {
		t.Fatalf("res.Err is nil; expected verify-cap error")
	}
}

// TestRefine_AgentCrashDoesNotTriggerRefine — if the agent itself exits
// non-zero, refine is bypassed (refining a crashed agent is not helpful).
// Verifies the orchestrator surfaces the agent failure directly rather
// than burning attempts on a broken agent.
func TestRefine_AgentCrashDoesNotTriggerRefine(t *testing.T) {
	script := `echo "starting"; exit 7`

	res, rec := driveRefineRun(t, script, RefineOptions{
		Enabled:     true,
		VerifyCmd:   []string{"true"},
		MaxAttempts: 3,
	}, true)

	if res.Status != "failure" {
		t.Fatalf("status=%s, want failure", res.Status)
	}
	if res.Attempts != 1 {
		t.Fatalf("Attempts=%d, want 1 (no refine on agent crash)", res.Attempts)
	}
	if res.ExitCode != 7 {
		t.Fatalf("ExitCode=%d, want 7 (agent crash propagated)", res.ExitCode)
	}
	if rec.count(event.VerifyStarted) != 0 {
		t.Fatalf("verify should not fire when the agent itself crashed")
	}
	if rec.count(event.RefineTriggered) != 0 {
		t.Fatalf("refine should not fire when the agent itself crashed")
	}
}
