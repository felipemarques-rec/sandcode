package orchestrator

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/agent"
	"github.com/felipemarques-rec/sandcode/internal/brain"
	"github.com/felipemarques-rec/sandcode/internal/event"
	"github.com/felipemarques-rec/sandcode/internal/judge"
	"github.com/felipemarques-rec/sandcode/internal/kernel"
	"github.com/felipemarques-rec/sandcode/internal/planner"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
	"github.com/felipemarques-rec/sandcode/internal/strategy"
)

// fakeSelector forces a specific Strategy regardless of inputs. Used
// to make Execute's routing decisions deterministic in tests.
type fakeSelector struct {
	strat  strategy.Strategy
	reason string
}

func (s fakeSelector) Select(_ brain.Classification, _ planner.TaskDAG) (strategy.Strategy, string) {
	return s.strat, s.reason
}

// fakePlanner returns a pre-built TaskDAG on every Decompose call and
// tracks invocation count for assertions.
type fakePlanner struct {
	dag            planner.TaskDAG
	decomposeCalls int32
}

func (p *fakePlanner) Decompose(_ context.Context, _ string) (planner.TaskDAG, error) {
	atomic.AddInt32(&p.decomposeCalls, 1)
	return p.dag, nil
}

func (p *fakePlanner) calls() int32 {
	return atomic.LoadInt32(&p.decomposeCalls)
}

// stubJudgePicksFirst always picks the first candidate.
type stubJudgePicksFirst struct{}

func (stubJudgePicksFirst) Name() string { return "stub-first" }
func (stubJudgePicksFirst) Rank(_ context.Context, _ string, cands []judge.Candidate) (judge.Ranking, error) {
	if len(cands) == 0 {
		return judge.Ranking{}, errors.New("no candidates")
	}
	return judge.Ranking{
		Winner:    cands[0].RunID,
		Rationale: "first",
		Judge:     "stub-first",
	}, nil
}

// newCountingBus returns a *event.LocalBus and a thread-safe snapshot
// fn that reports how many events of targetType have been published.
func newCountingBus(targetType event.Type) (*event.LocalBus, func() int) {
	lb := event.NewLocalBus()
	var count int64
	lb.Subscribe(event.Type("*"), func(_ context.Context, ev event.Event) error {
		if ev.Type == targetType {
			atomic.AddInt64(&count, 1)
		}
		return nil
	})
	return lb, func() int { return int(atomic.LoadInt64(&count)) }
}

// --- Entry-time validation tests ----------------------------------------

func TestExecute_EntryValidation_RejectsEmptyPrompt(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := Execute(ctx, sandbox.NewNoSandboxProvider(), &noopAuth{}, ExecuteOptions{
		CWD: "/tmp", SandboxImage: "img", Agent: &fakeAgent{script: "echo ok"},
	})
	if !errors.Is(err, ErrExecuteEmptyPrompt) {
		t.Fatalf("want ErrExecuteEmptyPrompt, got %v", err)
	}
}

func TestExecute_EntryValidation_RejectsEmptyCWD(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := Execute(ctx, sandbox.NewNoSandboxProvider(), &noopAuth{}, ExecuteOptions{
		Prompt: "p", SandboxImage: "img", Agent: &fakeAgent{script: "echo ok"},
	})
	if !errors.Is(err, ErrExecuteEmptyCWD) {
		t.Fatalf("want ErrExecuteEmptyCWD, got %v", err)
	}
}

func TestExecute_EntryValidation_RejectsEmptySandboxImage(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := Execute(ctx, sandbox.NewNoSandboxProvider(), &noopAuth{}, ExecuteOptions{
		Prompt: "p", CWD: "/tmp", Agent: &fakeAgent{script: "echo ok"},
	})
	if !errors.Is(err, ErrExecuteEmptySandboxImage) {
		t.Fatalf("want ErrExecuteEmptySandboxImage, got %v", err)
	}
}

func TestExecute_EntryValidation_RejectsNoAgent(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := Execute(ctx, sandbox.NewNoSandboxProvider(), &noopAuth{}, ExecuteOptions{
		Prompt: "p", CWD: "/tmp", SandboxImage: "img",
	})
	if !errors.Is(err, ErrExecuteNoAgent) {
		t.Fatalf("want ErrExecuteNoAgent, got %v", err)
	}
}

func TestExecute_EntryValidation_RejectsParallelPlusMultiAgent(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a := &fakeAgent{script: "echo a"}
	b := &fakeAgent{script: "echo b"}
	_, _, err := Execute(ctx, sandbox.NewNoSandboxProvider(), &noopAuth{}, ExecuteOptions{
		Prompt: "p", CWD: "/tmp", SandboxImage: "img",
		Agents: []agent.Provider{a, b}, ParallelN: 3,
	})
	if !errors.Is(err, ErrExecuteParallelAndAgents) {
		t.Fatalf("want ErrExecuteParallelAndAgents, got %v", err)
	}
}

// --- Kernel-nil fallback path -------------------------------------------

func TestExecute_NoKernelFallsBackToRun(t *testing.T) {
	t.Parallel()

	repo := initRepo(t)
	a := &fakeAgent{script: `echo "ok"; touch did-run`}

	events, await, err := Execute(
		context.Background(),
		sandbox.NewNoSandboxProvider(),
		&noopAuth{},
		ExecuteOptions{
			Prompt:         "do thing",
			CWD:            repo,
			SandboxImage:   "ignored-by-nosandbox",
			SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", t.Name(), "wd"),
			Agent:          a,
			RunID:          "no-kernel",
		},
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for range events {
	}
	res := await()
	if res.Kind != DispatchSingle {
		t.Errorf("Kind: got %v, want Single", res.Kind)
	}
	if res.Run == nil {
		t.Fatal("Run result is nil")
	}
	if res.Run.Status != "success" {
		t.Errorf("Run status: got %q, want success (err=%v)", res.Run.Status, res.Run.Err)
	}
	if res.DispatchReason == "" {
		t.Error("DispatchReason is empty")
	}
}

// --- Double-emission defense regression gate ----------------------------

func TestExecute_KernelProcessOnlyCalledOnce(t *testing.T) {
	t.Parallel()

	repo := initRepo(t)

	bus, classifiedCount := newCountingBus(event.RunClassified)
	t.Cleanup(func() { _ = bus.Close() })

	kn := kernel.New(nil,
		kernel.WithBus(bus),
		kernel.WithSelector(fakeSelector{strat: strategy.StrategySingle, reason: "test-forced-single"}),
	)

	a := &fakeAgent{script: `echo "ran"`}
	events, await, err := Execute(
		context.Background(),
		sandbox.NewNoSandboxProvider(),
		&noopAuth{},
		ExecuteOptions{
			Prompt:         "do thing",
			CWD:            repo,
			SandboxImage:   "ignored-by-nosandbox",
			SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", t.Name(), "wd"),
			Agent:          a,
			Kernel:         kn,
			Bus:            bus,
			RunID:          "double-emit-gate",
		},
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for range events {
	}
	res := await()
	if res.Run == nil || res.Run.Status != "success" {
		t.Fatalf("inner run failed: kind=%v run=%+v", res.Kind, res.Run)
	}

	if got := classifiedCount(); got != 1 {
		t.Errorf("run.classified count: got %d, want 1 (double-emission defense broken)", got)
	}
}

// --- Kernel-routed dispatch paths ---------------------------------------

func TestExecute_KernelSingleStrategy_RoutesToRun(t *testing.T) {
	t.Parallel()

	repo := initRepo(t)
	lb := event.NewLocalBus()
	t.Cleanup(func() { _ = lb.Close() })

	kn := kernel.New(nil,
		kernel.WithBus(lb),
		kernel.WithSelector(fakeSelector{strat: strategy.StrategySingle, reason: "test-single"}),
	)

	a := &fakeAgent{script: `echo "single-strat ran"`}
	events, await, err := Execute(
		context.Background(),
		sandbox.NewNoSandboxProvider(),
		&noopAuth{},
		ExecuteOptions{
			Prompt:         "p",
			CWD:            repo,
			SandboxImage:   "ignored-by-nosandbox",
			SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", t.Name(), "wd"),
			Agent:          a,
			Kernel:         kn,
			Bus:            lb,
			RunID:          "kernel-single",
		},
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for range events {
	}
	res := await()
	if res.Kind != DispatchSingle {
		t.Errorf("kind: got %v, want Single", res.Kind)
	}
	if res.Run == nil {
		t.Fatal("Run nil")
	}
	if res.DispatchReason != "kernel selected single" {
		t.Errorf("reason: got %q, want 'kernel selected single'", res.DispatchReason)
	}
}

func TestExecute_KernelParallelNoJudge_FallsBackToParallel(t *testing.T) {
	t.Parallel()

	repo := initRepo(t)
	lb := event.NewLocalBus()
	t.Cleanup(func() { _ = lb.Close() })

	twoRoot := planner.TaskDAG{Nodes: []planner.Node{
		{ID: "r0", Prompt: "p0"},
		{ID: "r1", Prompt: "p1"},
	}}
	kn := kernel.New(nil,
		kernel.WithBus(lb),
		kernel.WithSelector(fakeSelector{strat: strategy.StrategyParallel, reason: "test-parallel"}),
		kernel.WithPlanner(&fakePlanner{dag: twoRoot}),
	)

	a := &fakeAgent{script: `echo a`}
	b := &fakeAgent{script: `echo b`}

	events, await, err := Execute(
		context.Background(),
		sandbox.NewNoSandboxProvider(),
		&noopAuth{},
		ExecuteOptions{
			Prompt:         "do parallel work",
			CWD:            repo,
			SandboxImage:   "ignored-by-nosandbox",
			SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", t.Name(), "wd"),
			Agents:         []agent.Provider{a, b},
			Kernel:         kn,
			Bus:            lb,
			RunID:          "kernel-par-nojudge",
		},
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for range events {
	}
	res := await()
	if res.Kind != DispatchParallel {
		t.Errorf("kind: got %v, want Parallel", res.Kind)
	}
	if res.Parallel == nil {
		t.Fatal("Parallel nil")
	}
	if !strings.Contains(res.DispatchReason, "judge") {
		t.Errorf("reason: got %q, want it to mention 'judge'", res.DispatchReason)
	}
}

func TestExecute_KernelParallelSingleAgent_FallsBackToSingle(t *testing.T) {
	t.Parallel()

	repo := initRepo(t)
	lb := event.NewLocalBus()
	t.Cleanup(func() { _ = lb.Close() })

	kn := kernel.New(nil,
		kernel.WithBus(lb),
		kernel.WithSelector(fakeSelector{strat: strategy.StrategyParallel, reason: "test-parallel"}),
	)

	a := &fakeAgent{script: `echo solo`}

	events, await, err := Execute(
		context.Background(),
		sandbox.NewNoSandboxProvider(),
		&noopAuth{},
		ExecuteOptions{
			Prompt:         "p",
			CWD:            repo,
			SandboxImage:   "ignored-by-nosandbox",
			SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", t.Name(), "wd"),
			Agent:          a,
			Kernel:         kn,
			Bus:            lb,
			RunID:          "kernel-par-solo",
		},
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for range events {
	}
	res := await()
	if res.Kind != DispatchSingle {
		t.Errorf("kind: got %v, want Single (fallback)", res.Kind)
	}
	if !strings.Contains(res.DispatchReason, "falling back to single") {
		t.Errorf("reason: got %q, want substring 'falling back to single'", res.DispatchReason)
	}
}

// --- ForcePlan fallback -------------------------------------------------

// TestExecute_ForcePlanWhenStrategyParallelButPlanEmpty exercises the
// ForcePlan branch in Execute. fakeSelector forces Parallel; the
// brain classifier (Process step 1) returns low complexity for a
// short non-trigger prompt, so kernel.Process SKIPS the planner —
// pr.Plan is empty. Execute then calls ForcePlan, which routes to the
// planner regardless of complexity gate. Result: 2-root plan + judge
// configured → DAG dispatch. Assertion on decomposeCalls confirms
// the planner was called exactly once (via ForcePlan, not via
// Process's complexity-gated path).
func TestExecute_ForcePlanWhenStrategyParallelButPlanEmpty(t *testing.T) {
	t.Parallel()

	repo := initRepo(t)
	lb := event.NewLocalBus()
	t.Cleanup(func() { _ = lb.Close() })

	twoRoot := planner.TaskDAG{Nodes: []planner.Node{
		{ID: "r0", Prompt: "p0"},
		{ID: "r1", Prompt: "p1"},
	}}
	pl := &fakePlanner{dag: twoRoot}

	kn := kernel.New(nil,
		kernel.WithBus(lb),
		kernel.WithSelector(fakeSelector{strat: strategy.StrategyParallel, reason: "test-parallel"}),
		kernel.WithPlanner(pl),
	)

	a := &fakeAgent{script: `echo a`}
	b := &fakeAgent{script: `echo b`}

	events, await, err := Execute(
		context.Background(),
		sandbox.NewNoSandboxProvider(),
		&noopAuth{},
		ExecuteOptions{
			Prompt:         "ok",
			CWD:            repo,
			SandboxImage:   "ignored-by-nosandbox",
			SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", t.Name(), "wd"),
			Agents:         []agent.Provider{a, b},
			Judge:          stubJudgePicksFirst{},
			Kernel:         kn,
			Bus:            lb,
			RunID:          "force-plan",
			Synthesizer:    SynthesizerOptions{Disabled: true},
			KeepWorktree:   true,
		},
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for range events {
	}
	res := await()
	if res.Kind != DispatchDAG {
		t.Fatalf("kind: got %v, want DAG (force-plan should have populated the plan)", res.Kind)
	}
	if res.DAG == nil {
		t.Fatal("DAG nil")
	}
	if len(res.DAG.Plan.Roots()) != 2 {
		t.Errorf("plan roots: got %d, want 2", len(res.DAG.Plan.Roots()))
	}
	if calls := pl.calls(); calls != 1 {
		t.Errorf("planner.Decompose calls: got %d, want 1 (one ForcePlan, zero Process-gated)", calls)
	}
}
