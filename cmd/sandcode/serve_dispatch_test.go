package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/agent"
	"github.com/felipemarques-rec/sandcode/internal/auth"
	"github.com/felipemarques-rec/sandcode/internal/brain"
	"github.com/felipemarques-rec/sandcode/internal/event"
	"github.com/felipemarques-rec/sandcode/internal/judge"
	"github.com/felipemarques-rec/sandcode/internal/kernel"
	"github.com/felipemarques-rec/sandcode/internal/planner"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
	"github.com/felipemarques-rec/sandcode/internal/server"
	"github.com/felipemarques-rec/sandcode/internal/strategy"
)

// dispatchFakeAgent runs an arbitrary shell script.
type dispatchFakeAgent struct{ script string }

func (*dispatchFakeAgent) Name() string { return "dispatch-fake" }
func (f *dispatchFakeAgent) BuildCommand(_ agent.RunOptions) agent.Command {
	return agent.Command{Argv: []string{"sh", "-c", f.script}}
}
func (*dispatchFakeAgent) ParseLine(line string) (agent.StreamEvent, bool) {
	if line == "" {
		return agent.StreamEvent{}, false
	}
	return agent.StreamEvent{Kind: agent.EventText, Text: line}, true
}
func (*dispatchFakeAgent) AuthHints() agent.AuthHints { return agent.AuthHints{} }

type dispatchNoopAuth struct{}

func (*dispatchNoopAuth) Name() string                                                 { return "noop" }
func (*dispatchNoopAuth) Apply(spec *sandbox.SandboxSpec, hints agent.AuthHints) error { return nil }

var _ auth.Provider = (*dispatchNoopAuth)(nil)

// dispatchFakeSelector forces a deterministic strategy. Satisfies
// strategy.Selector.
type dispatchFakeSelector struct {
	strat  strategy.Strategy
	reason string
}

func (s dispatchFakeSelector) Select(_ brain.Classification, _ planner.TaskDAG) (strategy.Strategy, string) {
	return s.strat, s.reason
}

// initLauncherRepo creates a real git repo so the orchestrator's
// worktree manager has something to work with.
func initLauncherRepo(t *testing.T) string {
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

// newDispatchCountingBus returns a *LocalBus + snapshot fn.
func newDispatchCountingBus(target event.Type) (*event.LocalBus, func() int) {
	lb := event.NewLocalBus()
	var count int64
	lb.Subscribe(event.Type("*"), func(_ context.Context, ev event.Event) error {
		if ev.Type == target {
			atomic.AddInt64(&count, 1)
		}
		return nil
	})
	return lb, func() int { return int(atomic.LoadInt64(&count)) }
}

// TestLauncher_UsesExecuteWhenKernelConfigured asserts that when a
// kernel is wired into the launcher, an incoming RunRequest fires
// run.classified events (proving Execute called kernel.Process).
func TestLauncher_UsesExecuteWhenKernelConfigured(t *testing.T) {
	t.Parallel()

	repo := initLauncherRepo(t)

	bus, classifiedCount := newDispatchCountingBus(event.RunClassified)
	t.Cleanup(func() { _ = bus.Close() })

	kn := kernel.New(nil,
		kernel.WithBus(bus),
		kernel.WithSelector(dispatchFakeSelector{strat: strategy.StrategySingle, reason: "test-single"}),
	)

	l := &orchestratorLauncher{
		sb:           sandbox.NewNoSandboxProvider(),
		ag:           &dispatchFakeAgent{script: `echo "launcher-with-kernel"`},
		au:           &dispatchNoopAuth{},
		runStore:     nil,
		bus:          bus,
		kernel:       kn,
		defaultImage: "ignored-by-nosandbox",
	}

	err := l.Launch(context.Background(), "launcher-kn", server.RunRequest{
		Prompt:         "p",
		CWD:            repo,
		SandboxImage:   "ignored",
		SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", t.Name()),
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if got := classifiedCount(); got != 1 {
		t.Errorf("run.classified count: got %d, want 1", got)
	}
}

// TestLauncher_UsesExecuteWhenKernelNil_ActsAsRun asserts that no
// kernel.* events fire when the launcher has no kernel — Execute is
// still in the path but degenerates to direct Run behavior.
func TestLauncher_UsesExecuteWhenKernelNil_ActsAsRun(t *testing.T) {
	t.Parallel()

	repo := initLauncherRepo(t)

	bus, classifiedCount := newDispatchCountingBus(event.RunClassified)
	t.Cleanup(func() { _ = bus.Close() })

	l := &orchestratorLauncher{
		sb:           sandbox.NewNoSandboxProvider(),
		ag:           &dispatchFakeAgent{script: `echo "launcher-no-kernel"`},
		au:           &dispatchNoopAuth{},
		runStore:     nil,
		bus:          bus,
		kernel:       nil,
		defaultImage: "ignored-by-nosandbox",
	}

	err := l.Launch(context.Background(), "launcher-nokn", server.RunRequest{
		Prompt:         "p",
		CWD:            repo,
		SandboxImage:   "ignored",
		SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", t.Name()),
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if got := classifiedCount(); got != 0 {
		t.Errorf("run.classified count: got %d, want 0 (no kernel configured)", got)
	}
}

// dispatchFakePlanner returns a fixed TaskDAG on Decompose.
type dispatchFakePlanner struct {
	dag planner.TaskDAG
}

func (p *dispatchFakePlanner) Decompose(_ context.Context, _ string) (planner.TaskDAG, error) {
	return p.dag, nil
}

// dispatchStubJudge picks the first chain — enough to drive DAG to
// completion without an LLM call.
type dispatchStubJudge struct{}

func (dispatchStubJudge) Name() string { return "dispatch-stub" }
func (dispatchStubJudge) Rank(_ context.Context, _ string, cands []judge.Candidate) (judge.Ranking, error) {
	if len(cands) == 0 {
		return judge.Ranking{}, fmt.Errorf("no candidates")
	}
	return judge.Ranking{
		Winner:    cands[0].RunID,
		Rationale: "stub",
		Judge:     "dispatch-stub",
	}, nil
}

// TestLauncher_DAGAutoDispatch_E2E exercises the full HTTP-equivalent
// path through to DAGRun. With Slice 6's --judge wiring shipped, the
// launcher holds a Judge field and Execute auto-dispatch routes
// multi-root plans to DAGRun. The launcher is built with a forced
// Parallel selector + a 2-root fake planner + a stub judge; Execute
// should emit dag.started events.
func TestLauncher_DAGAutoDispatch_E2E(t *testing.T) {
	t.Parallel()

	repo := initLauncherRepo(t)

	bus, dagStartedCount := newDispatchCountingBus(event.DAGStarted)
	t.Cleanup(func() { _ = bus.Close() })

	twoRoot := planner.TaskDAG{Nodes: []planner.Node{
		{ID: "r0", Prompt: "p0"},
		{ID: "r1", Prompt: "p1"},
	}}
	kn := kernel.New(nil,
		kernel.WithBus(bus),
		kernel.WithSelector(dispatchFakeSelector{strat: strategy.StrategyParallel, reason: "test-parallel"}),
		kernel.WithPlanner(&dispatchFakePlanner{dag: twoRoot}),
	)

	l := &orchestratorLauncher{
		sb:           sandbox.NewNoSandboxProvider(),
		ag:           &dispatchFakeAgent{script: `echo "launcher-dag-auto"`},
		au:           &dispatchNoopAuth{},
		runStore:     nil,
		bus:          bus,
		kernel:       kn,
		defaultImage: "ignored-by-nosandbox",
		judge:        dispatchStubJudge{},
	}

	err := l.Launch(context.Background(), "launcher-dag", server.RunRequest{
		Prompt:         "auto-dispatch to dag",
		CWD:            repo,
		SandboxImage:   "ignored",
		SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", t.Name()),
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if got := dagStartedCount(); got != 1 {
		t.Errorf("dag.started count: got %d, want 1 (Execute should have routed to DAGRun)", got)
	}
}
