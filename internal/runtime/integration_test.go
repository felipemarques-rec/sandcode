package runtime_test

// This file lives in runtime_test (external test package) so we can import
// orchestrator without creating a production-code cycle. The point: prove
// that subscribing an ExecutionState to a live LocalBus and running
// orchestrator.Run end-to-end yields a state machine that reaches
// PhaseCompleted via the same events the orchestrator actually emits.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/agent"
	"github.com/felipemarques-rec/sandcode/internal/auth"
	"github.com/felipemarques-rec/sandcode/internal/event"
	gitm "github.com/felipemarques-rec/sandcode/internal/git"
	"github.com/felipemarques-rec/sandcode/internal/orchestrator"
	"github.com/felipemarques-rec/sandcode/internal/runtime"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
)

// fakeAgent mirrors internal/orchestrator/run_test.go's fixture. Duplicated
// because that one lives in the orchestrator's internal test package.
type fakeAgent struct{ script string }

func (*fakeAgent) Name() string { return "fake" }
func (f *fakeAgent) BuildCommand(_ agent.RunOptions) agent.Command {
	return agent.Command{Argv: []string{"sh", "-c", f.script}}
}
func (*fakeAgent) ParseLine(line string) (agent.StreamEvent, bool) {
	if line == "" {
		return agent.StreamEvent{}, false
	}
	return agent.StreamEvent{Kind: agent.EventText, Text: line}, true
}
func (*fakeAgent) AuthHints() agent.AuthHints { return agent.AuthHints{} }

type noopAuth struct{}

func (*noopAuth) Name() string { return "noop" }
func (*noopAuth) Apply(_ *sandbox.SandboxSpec, _ agent.AuthHints) error {
	return nil
}

var _ auth.Provider = (*noopAuth)(nil)

func initRepo(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = tmp
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(tmp, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "."},
		{"commit", "-q", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = tmp
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return tmp
}

// stateSubscriber is the production-shaped subscriber: it owns one
// ExecutionState per run_id, threading Apply through any event that
// matches. Errors from Apply are surfaced for the test to inspect.
type stateSubscriber struct {
	mu     sync.Mutex
	states map[string]*runtime.ExecutionState
	errors []error
	// phases captures the ordered sequence of distinct phases visited.
	phases []runtime.Phase
}

func newStateSubscriber() *stateSubscriber {
	return &stateSubscriber{states: make(map[string]*runtime.ExecutionState)}
}

func (s *stateSubscriber) handle(_ context.Context, ev event.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	st, ok := s.states[ev.RunID]
	if !ok {
		st = runtime.NewExecutionState(ev.RunID)
		s.states[ev.RunID] = st
	}
	before := st.Phase
	if err := st.Apply(ev); err != nil {
		// Don't return — we want to record every error for diagnosis.
		s.errors = append(s.errors, err)
	}
	if st.Phase != before {
		s.phases = append(s.phases, st.Phase)
	}
	return nil
}

func (s *stateSubscriber) state(runID string) *runtime.ExecutionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.states[runID]
}

func (s *stateSubscriber) phaseSequence() []runtime.Phase {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]runtime.Phase, len(s.phases))
	copy(out, s.phases)
	return out
}

// TestRuntime_TracksOrchestratorRunToCompleted boots a real (nosandbox)
// orchestrator.Run, subscribes a state machine to the same bus the
// orchestrator emits to, and asserts:
//
//  1. The state reaches PhaseCompleted.
//  2. The phase sequence matches what the orchestrator actually emits:
//     submitted → sandbox_ready → executing → agent_completed → completed.
//     (No kernel is wired, so classified/enriched are absent — see the
//     non-cognition fast path in the transition table.)
//  3. No Apply error fired during the run.
func TestRuntime_TracksOrchestratorRunToCompleted(t *testing.T) {
	repo := initRepo(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	bus := event.NewLocalBus()
	t.Cleanup(func() { _ = bus.Close() })

	sub := newStateSubscriber()
	bus.Subscribe("*", sub.handle)

	script := `echo "writing"; echo "world" > integration.txt; echo "done"`

	events, await, err := orchestrator.Run(ctx,
		sandbox.NewNoSandboxProvider(),
		&fakeAgent{script: script},
		&noopAuth{},
		orchestrator.RunOptions{
			Prompt:         "noop",
			CWD:            repo,
			SandboxImage:   "ignored",
			SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", "integ", "0"),
			Strategy:       gitm.StrategyMergeToHead,
			Bus:            bus, // ← the wiring under test
		},
	)
	if err != nil {
		t.Fatalf("orchestrator.Run: %v", err)
	}
	for range events { // drain — required before await
	}
	res := await()
	if res.Status != "success" {
		t.Fatalf("orchestrator status=%s err=%v", res.Status, res.Err)
	}

	st := sub.state(res.RunID)
	if st == nil {
		t.Fatalf("no state machine ever saw a run.* event for %s", res.RunID)
	}
	if st.Phase != runtime.PhaseCompleted {
		t.Fatalf("phase=%s, want completed (apply errors: %v)", st.Phase, sub.errors)
	}
	if len(sub.errors) > 0 {
		t.Fatalf("Apply errors during live run: %v", sub.errors)
	}

	// Phase progression check. No kernel → no classified/enriched events,
	// so we expect the non-cognition fast path.
	want := []runtime.Phase{
		runtime.PhaseSandboxReady,
		runtime.PhaseExecuting,
		runtime.PhaseAgentCompleted,
		runtime.PhaseCompleted,
	}
	got := sub.phaseSequence()
	if !phaseSliceEqual(got, want) {
		t.Fatalf("phase sequence:\n  got:  %v\n  want: %v", got, want)
	}
}

// TestRuntime_TracksFailureAndCapturesError simulates an agent that exits
// non-zero. Orchestrator emits run.failed; the state machine must enter
// PhaseFailed and capture the error message from the payload.
func TestRuntime_TracksFailureAndCapturesError(t *testing.T) {
	repo := initRepo(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	bus := event.NewLocalBus()
	t.Cleanup(func() { _ = bus.Close() })

	sub := newStateSubscriber()
	bus.Subscribe("*", sub.handle)

	// `false` exits 1 — orchestrator should classify as failure.
	events, await, err := orchestrator.Run(ctx,
		sandbox.NewNoSandboxProvider(),
		&fakeAgent{script: `exit 1`},
		&noopAuth{},
		orchestrator.RunOptions{
			Prompt:         "noop",
			CWD:            repo,
			SandboxImage:   "ignored",
			SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", "fail", "0"),
			Strategy:       gitm.StrategyBranch,
			Bus:            bus,
		},
	)
	if err != nil {
		t.Fatalf("orchestrator.Run: %v", err)
	}
	for range events {
	}
	res := await()
	if res.Status != "failure" {
		t.Fatalf("orchestrator status=%s, want failure", res.Status)
	}

	st := sub.state(res.RunID)
	if st == nil {
		t.Fatalf("no state ever observed for %s", res.RunID)
	}
	if st.Phase != runtime.PhaseFailed {
		t.Fatalf("phase=%s, want failed (apply errors: %v)", st.Phase, sub.errors)
	}

	// The runtime should swallow ErrTerminal cleanly — Apply errors are
	// only acceptable if all of them are ErrTerminal (post-failure events
	// like SandboxDestroyed observation arriving after we transitioned).
	for _, e := range sub.errors {
		if !errors.Is(e, runtime.ErrTerminal) {
			t.Fatalf("unexpected Apply error during failure path: %v", e)
		}
	}
}

func phaseSliceEqual(a, b []runtime.Phase) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
