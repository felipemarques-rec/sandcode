package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/agent"
	"github.com/felipemarques-rec/sandcode/internal/event"
	"github.com/felipemarques-rec/sandcode/internal/mcp"
	"github.com/felipemarques-rec/sandcode/internal/planner"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
)

// chainHarness builds a DAGOptions + plumbed bus recorder ready to feed
// runChain. Reuses initRepo + nosandbox + fakeAgent from the existing
// test infrastructure (see run_test.go / refine_integration_test.go).
type chainHarness struct {
	repo string
	opts DAGOptions
	rec  *recorder
}

func newChainHarness(t *testing.T, refine RefineOptions) *chainHarness {
	t.Helper()
	repo := initRepo(t)
	lb := event.NewLocalBus()
	t.Cleanup(func() { _ = lb.Close() })
	rec := &recorder{}
	lb.Subscribe("*", rec.handle)

	return &chainHarness{
		repo: repo,
		rec:  rec,
		opts: DAGOptions{
			Prompt:         "chain-harness",
			CWD:            repo,
			SandboxImage:   "ignored-by-nosandbox",
			SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", t.Name(), "wd"),
			Bus:            lb,
			RunID:          "chain-" + t.Name(),
			Refine:         refine,
		},
	}
}

func TestRunChain_LinearSuccess(t *testing.T) {
	h := newChainHarness(t, RefineOptions{})
	ctx := context.Background()

	plan := []planner.Node{
		{ID: "a", Prompt: "create user"},
		{ID: "b", Prompt: "add validation", DependsOn: []string{"a"}},
	}

	res, err := runChain(ctx,
		sandbox.NewNoSandboxProvider(),
		&fakeAgent{script: `echo "node ran"`},
		&noopAuth{},
		chainSpec{ChainID: "chain-0", RootNodeID: "a", Nodes: plan},
		h.opts,
	)
	if err != nil {
		t.Fatalf("runChain: %v", err)
	}
	if !res.Success {
		t.Fatalf("chain should succeed, got: %+v", res)
	}
	if len(res.Nodes) != 2 {
		t.Fatalf("got %d node results, want 2", len(res.Nodes))
	}
	if res.Nodes[0].NodeID != "a" || res.Nodes[1].NodeID != "b" {
		t.Errorf("node order: got %s,%s", res.Nodes[0].NodeID, res.Nodes[1].NodeID)
	}

	if h.rec.count(event.DAGChainStarted) != 1 {
		t.Errorf("DAGChainStarted: got %d want 1", h.rec.count(event.DAGChainStarted))
	}
	if h.rec.count(event.DAGNodeStarted) != 2 {
		t.Errorf("DAGNodeStarted: got %d want 2", h.rec.count(event.DAGNodeStarted))
	}
	if h.rec.count(event.DAGNodeCompleted) != 2 {
		t.Errorf("DAGNodeCompleted: got %d want 2", h.rec.count(event.DAGNodeCompleted))
	}
	if h.rec.count(event.DAGChainCompleted) != 1 {
		t.Errorf("DAGChainCompleted: got %d want 1", h.rec.count(event.DAGChainCompleted))
	}
}

// agentScript counts invocations via a worktree-local file and exits 1
// on the n-th call. Used to simulate mid-chain failures deterministically.
const failingOnSecondCall = `
counter=".chain_counter"
[ -f "$counter" ] && c=$(cat "$counter") || c=0
c=$((c+1))
echo "$c" > "$counter"
echo "call $c"
if [ "$c" -eq 2 ]; then
  exit 1
fi
`

func TestRunChain_FailIsolatedMidChain(t *testing.T) {
	h := newChainHarness(t, RefineOptions{})
	ctx := context.Background()

	plan := []planner.Node{
		{ID: "a", Prompt: "p-a"},
		{ID: "b", Prompt: "p-b"},
		{ID: "c", Prompt: "p-c"},
	}

	res, err := runChain(ctx,
		sandbox.NewNoSandboxProvider(),
		&fakeAgent{script: failingOnSecondCall},
		&noopAuth{},
		chainSpec{ChainID: "chain-0", RootNodeID: "a", Nodes: plan},
		h.opts,
	)
	if err != nil {
		t.Fatalf("runChain returned error (should swallow node failures): %v", err)
	}
	if res.Success {
		t.Errorf("chain should be Success=false")
	}
	if res.FailedAt != "b" {
		t.Errorf("FailedAt: got %q want %q", res.FailedAt, "b")
	}
	if len(res.Nodes) != 2 {
		t.Errorf("expected 2 NodeResults (a success, b fail), got %d", len(res.Nodes))
	}
	if res.Nodes[0].Result.Err != nil || res.Nodes[0].Result.ExitCode != 0 {
		t.Errorf("node a should have succeeded: %+v", res.Nodes[0].Result)
	}
	if res.Nodes[1].Result.ExitCode == 0 {
		t.Errorf("node b should have non-zero exit, got %d", res.Nodes[1].Result.ExitCode)
	}
}

// progressiveVerify: agent always succeeds; verifier writes .fixed on
// 2nd attempt so the first verify fails and the second passes. Tests
// per-node refine cascade.
const progressiveVerifyAgent = `
counter=".verify_counter"
[ -f "$counter" ] && c=$(cat "$counter") || c=0
c=$((c+1))
echo "$c" > "$counter"
echo "agent attempt $c"
if [ "$c" -ge 2 ]; then
  echo fixed > .fixed
fi
`

func TestRunChain_RefineCascadesPerNode(t *testing.T) {
	h := newChainHarness(t, RefineOptions{
		Enabled:     true,
		VerifyCmd:   []string{"sh", "-c", "test -f .fixed"},
		MaxAttempts: 3,
	})
	ctx := context.Background()

	plan := []planner.Node{{ID: "only", Prompt: "do work"}}

	res, err := runChain(ctx,
		sandbox.NewNoSandboxProvider(),
		&fakeAgent{script: progressiveVerifyAgent},
		&noopAuth{},
		chainSpec{ChainID: "chain-0", RootNodeID: "only", Nodes: plan},
		h.opts,
	)
	if err != nil {
		t.Fatalf("runChain: %v", err)
	}
	if !res.Success {
		t.Fatalf("chain should succeed after refine: %+v err=%v", res, res.Nodes[0].Result.Err)
	}
	if res.Nodes[0].Attempts != 2 {
		t.Errorf("expected Attempts=2, got %d", res.Nodes[0].Attempts)
	}
	if h.rec.count(event.DAGNodeStarted) != 2 {
		t.Errorf("DAGNodeStarted: got %d want 2 (one per attempt)", h.rec.count(event.DAGNodeStarted))
	}
	if h.rec.count(event.DAGNodeCompleted) != 1 {
		t.Errorf("DAGNodeCompleted: got %d want 1 (terminal only)", h.rec.count(event.DAGNodeCompleted))
	}
}

func TestRunChain_WorktreeCleanedOnSuccess(t *testing.T) {
	h := newChainHarness(t, RefineOptions{})
	ctx := context.Background()

	plan := []planner.Node{{ID: "a", Prompt: "x"}}
	res, err := runChain(ctx,
		sandbox.NewNoSandboxProvider(),
		&fakeAgent{script: `echo ok`},
		&noopAuth{},
		chainSpec{ChainID: "chain-0", RootNodeID: "a", Nodes: plan},
		h.opts,
	)
	if err != nil {
		t.Fatalf("runChain: %v", err)
	}
	if !res.Success {
		t.Fatalf("chain should succeed")
	}
	if res.Worktree == "" {
		t.Fatalf("worktree path empty")
	}
	// Cleanup is best-effort (KeepWorktree=false). Stat should fail.
	if err := statFile(res.Worktree); err == nil {
		t.Errorf("worktree dir still exists after cleanup: %s", res.Worktree)
	}
}

func TestRunChain_KeepWorktreePreservesOnSuccess(t *testing.T) {
	h := newChainHarness(t, RefineOptions{})
	h.opts.KeepWorktree = true
	ctx := context.Background()

	plan := []planner.Node{{ID: "a", Prompt: "x"}}
	res, err := runChain(ctx,
		sandbox.NewNoSandboxProvider(),
		&fakeAgent{script: `echo ok`},
		&noopAuth{},
		chainSpec{ChainID: "chain-0", RootNodeID: "a", Nodes: plan},
		h.opts,
	)
	if err != nil {
		t.Fatalf("runChain: %v", err)
	}
	if !res.Success {
		t.Fatalf("chain should succeed")
	}
	if err := statFile(res.Worktree); err != nil {
		t.Errorf("worktree should be preserved with KeepWorktree=true: %v", err)
	}
}

// TestRunChain_MCPInjected verifies a configured MCP manager writes .mcp.json
// into the chain worktree (visible to the agent) and emits mcp.injected.
func TestRunChain_MCPInjected(t *testing.T) {
	h := newChainHarness(t, RefineOptions{})
	h.opts.KeepWorktree = true
	mgr := mcp.NewManager(mcp.DefaultConfigs())
	mgr.Enable("context7")
	h.opts.MCP = mgr
	ctx := context.Background()

	plan := []planner.Node{{ID: "a", Prompt: "x"}}
	res, err := runChain(ctx,
		sandbox.NewNoSandboxProvider(),
		&fakeAgent{script: `if [ -f .mcp.json ]; then echo present > saw.txt; else echo absent > saw.txt; fi`},
		&noopAuth{},
		chainSpec{ChainID: "chain-0", RootNodeID: "a", Nodes: plan},
		h.opts,
	)
	if err != nil {
		t.Fatalf("runChain: %v", err)
	}
	if !res.Success {
		t.Fatalf("chain should succeed")
	}
	// .mcp.json present in the chain worktree.
	if _, err := os.Stat(filepath.Join(res.Worktree, ".mcp.json")); err != nil {
		t.Fatalf(".mcp.json missing from chain worktree: %v", err)
	}
	// Agent saw it.
	saw, _ := os.ReadFile(filepath.Join(res.Worktree, "saw.txt"))
	if string(saw) == "" {
		t.Fatal("agent marker not written")
	}
	if n := h.rec.count(event.MCPInjected); n != 1 {
		t.Fatalf("mcp.injected count = %d, want 1", n)
	}
}

func TestRunChain_FailedWorktreeNotCleaned(t *testing.T) {
	// Failed chains leave the worktree intact for inspection.
	h := newChainHarness(t, RefineOptions{})
	ctx := context.Background()

	plan := []planner.Node{
		{ID: "a", Prompt: "p-a"},
		{ID: "b", Prompt: "p-b"},
	}
	res, err := runChain(ctx,
		sandbox.NewNoSandboxProvider(),
		&fakeAgent{script: failingOnSecondCall},
		&noopAuth{},
		chainSpec{ChainID: "chain-0", RootNodeID: "a", Nodes: plan},
		h.opts,
	)
	if err != nil || res.Success {
		t.Fatalf("test fixture broken: err=%v success=%v", err, res.Success)
	}
	if err := statFile(res.Worktree); err != nil {
		t.Errorf("worktree should be preserved on failure: %v", err)
	}
}

// Sanity assertion: the single sandbox per chain pattern doesn't try
// to provision a second container per node. Verified indirectly: total
// time for a 3-node chain with `sleep 0.1` per node should NOT scale
// linearly with sandbox provisioning overhead. Smoke-test only — not a
// hard timing guarantee.
func TestRunChain_NoPerNodeSandbox(t *testing.T) {
	h := newChainHarness(t, RefineOptions{})
	ctx := context.Background()
	plan := []planner.Node{
		{ID: "a", Prompt: "a"},
		{ID: "b", Prompt: "b"},
		{ID: "c", Prompt: "c"},
	}
	t0 := time.Now()
	res, err := runChain(ctx,
		sandbox.NewNoSandboxProvider(),
		&fakeAgent{script: `echo ok`},
		&noopAuth{},
		chainSpec{ChainID: "chain-0", RootNodeID: "a", Nodes: plan},
		h.opts,
	)
	if err != nil || !res.Success {
		t.Fatalf("chain failed: err=%v res=%+v", err, res)
	}
	if elapsed := time.Since(t0); elapsed > 5*time.Second {
		t.Errorf("3-node chain took %s — likely per-node sandbox overhead", elapsed)
	}
}

func TestDecomposeChains_RoundRobinAgents(t *testing.T) {
	t.Parallel()

	plan := planner.TaskDAG{Nodes: []planner.Node{
		{ID: "r0", Prompt: "p0"},
		{ID: "r1", Prompt: "p1"},
		{ID: "r2", Prompt: "p2"},
		{ID: "r3", Prompt: "p3"},
		{ID: "r4", Prompt: "p4"},
	}}
	a0 := &fakeAgent{script: "echo a0"}
	a1 := &fakeAgent{script: "echo a1"}

	specs := decomposeChains(plan, []agent.Provider{a0, a1})

	if len(specs) != 5 {
		t.Fatalf("want 5 chains, got %d", len(specs))
	}
	want := []agent.Provider{a0, a1, a0, a1, a0}
	for i, s := range specs {
		if s.AssignedAgent != want[i] {
			t.Errorf("chain %d: got AssignedAgent=%v, want %v (pointer-identity)", i, s.AssignedAgent, want[i])
		}
	}
}

func TestDecomposeChains_SingleAgentFallback(t *testing.T) {
	t.Parallel()

	plan := planner.TaskDAG{Nodes: []planner.Node{
		{ID: "r0", Prompt: "p0"},
		{ID: "r1", Prompt: "p1"},
		{ID: "r2", Prompt: "p2"},
	}}
	a0 := &fakeAgent{script: "echo a0"}

	specs := decomposeChains(plan, []agent.Provider{a0})
	for i, s := range specs {
		if s.AssignedAgent != nil {
			t.Errorf("chain %d: single-agent input must leave AssignedAgent nil, got %v", i, s.AssignedAgent)
		}
	}

	specs = decomposeChains(plan, nil)
	for i, s := range specs {
		if s.AssignedAgent != nil {
			t.Errorf("chain %d: nil agents must leave AssignedAgent nil, got %v", i, s.AssignedAgent)
		}
	}
}

// TestDAGRun_MultiAgentWithinDAG_DifferentAgentsPerChain verifies that
// a 2-root plan with 2 agents routes each chain to a different agent
// (round-robin). Each agent writes a marker file into the chain's
// worktree; we then read each worktree and assert chain-0 has
// agent0.marker and chain-1 has agent1.marker — exactly what
// round-robin should produce.
func TestDAGRun_MultiAgentWithinDAG_DifferentAgentsPerChain(t *testing.T) {
	t.Parallel()

	repo := initRepo(t)
	lb := event.NewLocalBus()
	t.Cleanup(func() { _ = lb.Close() })

	plan := planner.TaskDAG{Nodes: []planner.Node{
		{ID: "root0", Prompt: "do a"},
		{ID: "root1", Prompt: "do b"},
	}}

	a0 := &fakeAgent{script: `echo "a0 running"; touch agent0.marker; echo done`}
	a1 := &fakeAgent{script: `echo "a1 running"; touch agent1.marker; echo done`}

	opts := DAGOptions{
		Prompt:         "two roots",
		CWD:            repo,
		SandboxImage:   "ignored-by-nosandbox",
		SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", t.Name(), "wd"),
		Plan:           plan,
		Judge:          rrJudge{},
		Agents:         []agent.Provider{a0, a1},
		Bus:            lb,
		RunID:          "dag-multi-agent",
		KeepWorktree:   true,
		Synthesizer:    SynthesizerOptions{Disabled: true},
	}

	_, await, err := DAGRun(context.Background(),
		sandbox.NewNoSandboxProvider(),
		a0, // fallback agent — must NOT be used since Agents has 2 entries
		&noopAuth{},
		opts,
	)
	if err != nil {
		t.Fatalf("DAGRun: %v", err)
	}
	res := await()
	if res.Error != nil {
		t.Fatalf("DAGRun result error: %v", res.Error)
	}
	if len(res.Chains) != 2 {
		t.Fatalf("want 2 chains, got %d", len(res.Chains))
	}

	want := map[string]string{
		"chain-0": "agent0.marker",
		"chain-1": "agent1.marker",
	}
	for _, c := range res.Chains {
		marker, ok := want[c.ChainID]
		if !ok {
			t.Errorf("unexpected chain id %q", c.ChainID)
			continue
		}
		if c.Worktree == "" {
			t.Errorf("chain %s: worktree path empty", c.ChainID)
			continue
		}
		if _, err := os.Stat(filepath.Join(c.Worktree, marker)); err != nil {
			t.Errorf("chain %s: expected marker %q in worktree %s: %v", c.ChainID, marker, c.Worktree, err)
		}
		otherMarker := want["chain-0"]
		if marker == otherMarker {
			otherMarker = want["chain-1"]
		}
		if _, err := os.Stat(filepath.Join(c.Worktree, otherMarker)); err == nil {
			t.Errorf("chain %s: unexpected cross-agent marker %q present in worktree %s", c.ChainID, otherMarker, c.Worktree)
		}
	}
}
