package orchestrator

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/agent"
	"github.com/felipemarques-rec/sandcode/internal/event"
	"github.com/felipemarques-rec/sandcode/internal/judge"
	"github.com/felipemarques-rec/sandcode/internal/planner"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
)

// promptAwareAgent fails when opts.Prompt matches failPattern; succeeds
// otherwise. Lets us deterministically fail specific chains in
// multi-root tests by encoding the failure trigger in the chain's
// node prompt.
type promptAwareAgent struct {
	failPattern string
}

func (*promptAwareAgent) Name() string { return "promptAware" }
func (p *promptAwareAgent) BuildCommand(opts agent.RunOptions) agent.Command {
	if p.failPattern != "" && strings.Contains(opts.Prompt, p.failPattern) {
		return agent.Command{Argv: []string{"sh", "-c", "echo failing; exit 1"}}
	}
	return agent.Command{Argv: []string{"sh", "-c", "echo ok"}}
}
func (*promptAwareAgent) ParseLine(line string) (agent.StreamEvent, bool) {
	if line == "" {
		return agent.StreamEvent{}, false
	}
	return agent.StreamEvent{Kind: agent.EventText, Text: line}, true
}
func (*promptAwareAgent) AuthHints() agent.AuthHints { return agent.AuthHints{} }

// rrJudge: round-robin / picks the first chain. Deterministic for tests.
type rrJudge struct{}

func (rrJudge) Name() string { return "rr" }
func (rrJudge) Rank(_ context.Context, _ string, cands []judge.Candidate) (judge.Ranking, error) {
	if len(cands) == 0 {
		return judge.Ranking{}, errors.New("no candidates")
	}
	scores := map[string]float64{}
	for i, c := range cands {
		scores[c.RunID] = float64(len(cands)-i) / float64(len(cands))
	}
	return judge.Ranking{
		Winner:    cands[0].RunID,
		Scores:    scores,
		Rationale: "rr picked first",
		Judge:     "rr",
	}, nil
}

// dagHarness sets up a real worktree-backed DAGOptions ready to feed
// DAGRun. CWD is a real git repo (initRepo). RunID + paths are unique
// per test.
type dagHarness struct {
	repo string
	opts DAGOptions
	rec  *recorder
}

func newDAGHarness(t *testing.T) *dagHarness {
	t.Helper()
	repo := initRepo(t)
	lb := event.NewLocalBus()
	t.Cleanup(func() { _ = lb.Close() })
	rec := &recorder{}
	lb.Subscribe("*", rec.handle)
	return &dagHarness{
		repo: repo,
		rec:  rec,
		opts: DAGOptions{
			Prompt:         "test-dag",
			CWD:            repo,
			SandboxImage:   "ignored-by-nosandbox",
			SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", t.Name(), "wd"),
			Bus:            lb,
			RunID:          "dag-" + t.Name(),
			Judge:          rrJudge{},
		},
	}
}

func TestDAGRun_TwoRootsTwoNodes_HappyPath(t *testing.T) {
	h := newDAGHarness(t)
	h.opts.Plan = planner.TaskDAG{Nodes: []planner.Node{
		{ID: "r0", Prompt: "root 0"},
		{ID: "n0", Prompt: "n0", DependsOn: []string{"r0"}},
		{ID: "r1", Prompt: "root 1"},
		{ID: "n1", Prompt: "n1", DependsOn: []string{"r1"}},
	}}
	_, await, err := DAGRun(context.Background(),
		sandbox.NewNoSandboxProvider(),
		&promptAwareAgent{},
		&noopAuth{},
		h.opts,
	)
	if err != nil {
		t.Fatalf("DAGRun: %v", err)
	}
	res := await()
	if res.Error != nil {
		t.Fatalf("DAGResult.Error: %v", res.Error)
	}
	if len(res.Chains) != 2 {
		t.Fatalf("expected 2 chains, got %d", len(res.Chains))
	}
	if res.Winner == "" {
		t.Errorf("Winner should be set")
	}
	if res.Synthesizer == nil {
		t.Errorf("synthesizer should have run for 2-chain success")
	}

	if h.rec.count(event.DAGStarted) != 1 {
		t.Errorf("DAGStarted count: got %d want 1", h.rec.count(event.DAGStarted))
	}
	if h.rec.count(event.DAGChainStarted) != 2 {
		t.Errorf("DAGChainStarted: got %d want 2", h.rec.count(event.DAGChainStarted))
	}
	if h.rec.count(event.DAGChainCompleted) != 2 {
		t.Errorf("DAGChainCompleted: got %d want 2", h.rec.count(event.DAGChainCompleted))
	}
	if h.rec.count(event.DAGCompleted) != 1 {
		t.Errorf("DAGCompleted: got %d want 1", h.rec.count(event.DAGCompleted))
	}
	if h.rec.count(event.DAGSynthesisStarted) != 1 {
		t.Errorf("DAGSynthesisStarted: got %d want 1", h.rec.count(event.DAGSynthesisStarted))
	}
}

func TestDAGRun_FailIsolated_OneChainFailsOthersComplete(t *testing.T) {
	h := newDAGHarness(t)
	h.opts.Plan = planner.TaskDAG{Nodes: []planner.Node{
		{ID: "r0", Prompt: "ok-r0"},
		{ID: "r1", Prompt: "fail-here-r1"},
		{ID: "r2", Prompt: "ok-r2"},
	}}
	_, await, err := DAGRun(context.Background(),
		sandbox.NewNoSandboxProvider(),
		&promptAwareAgent{failPattern: "fail-here"},
		&noopAuth{},
		h.opts,
	)
	if err != nil {
		t.Fatalf("DAGRun: %v", err)
	}
	res := await()
	succ := 0
	for _, c := range res.Chains {
		if c.Success {
			succ++
		}
	}
	if succ != 2 {
		t.Errorf("expected 2 succeeded chains, got %d (chains: %+v)", succ, res.Chains)
	}
	if res.Winner == "" {
		t.Errorf("expected a winner among the 2 succeeded chains")
	}
	if res.Synthesizer == nil {
		t.Errorf("expected synthesizer to run with 2 successful chains")
	}
}

func TestDAGRun_AllChainsFail_NoSynthesizer(t *testing.T) {
	h := newDAGHarness(t)
	h.opts.Plan = planner.TaskDAG{Nodes: []planner.Node{
		{ID: "r0", Prompt: "fail-r0"},
		{ID: "r1", Prompt: "fail-r1"},
	}}
	_, await, _ := DAGRun(context.Background(),
		sandbox.NewNoSandboxProvider(),
		&promptAwareAgent{failPattern: "fail"},
		&noopAuth{},
		h.opts,
	)
	res := await()
	if !errors.Is(res.Error, ErrAllChainsFailed) {
		t.Errorf("expected ErrAllChainsFailed, got %v", res.Error)
	}
	if res.Synthesizer != nil {
		t.Errorf("synthesizer should be skipped when all chains fail")
	}
}

func TestDAGRun_SingleNodeFallsBackToRun(t *testing.T) {
	h := newDAGHarness(t)
	h.opts.Judge = nil // single node — no judge needed
	h.opts.Plan = planner.TaskDAG{Nodes: []planner.Node{
		{ID: "only", Prompt: "do thing"},
	}}
	_, await, err := DAGRun(context.Background(),
		sandbox.NewNoSandboxProvider(),
		&promptAwareAgent{},
		&noopAuth{},
		h.opts,
	)
	if err != nil {
		t.Fatalf("DAGRun: %v", err)
	}
	res := await()
	if res.Error != nil {
		t.Errorf("single-node DAG: unexpected error: %v", res.Error)
	}
	if res.Synthesizer != nil {
		t.Errorf("synthesizer should be skipped for single-node DAG")
	}
	if h.rec.count(event.DAGSynthesisStarted) != 0 {
		t.Errorf("DAGSynthesisStarted: got %d want 0", h.rec.count(event.DAGSynthesisStarted))
	}
}

func TestDAGRun_SingleChainSkipsSynthesizer(t *testing.T) {
	h := newDAGHarness(t)
	h.opts.Judge = nil // single root — judge not required
	h.opts.Plan = planner.TaskDAG{Nodes: []planner.Node{
		{ID: "a", Prompt: "a"},
		{ID: "b", Prompt: "b", DependsOn: []string{"a"}},
	}}
	_, await, err := DAGRun(context.Background(),
		sandbox.NewNoSandboxProvider(),
		&promptAwareAgent{},
		&noopAuth{},
		h.opts,
	)
	if err != nil {
		t.Fatalf("DAGRun: %v", err)
	}
	res := await()
	if res.Synthesizer != nil {
		t.Errorf("synthesizer should be skipped for single-chain DAG")
	}
	if !res.Chains[0].Success {
		t.Errorf("single chain should have succeeded")
	}
	if h.rec.count(event.DAGSynthesisStarted) != 0 {
		t.Errorf("DAGSynthesisStarted: got %d want 0", h.rec.count(event.DAGSynthesisStarted))
	}
}

func TestDAGRun_RejectsDiamondAtEntry(t *testing.T) {
	h := newDAGHarness(t)
	h.opts.Plan = planner.TaskDAG{Nodes: []planner.Node{
		{ID: "a", Prompt: "a"},
		{ID: "b", Prompt: "b", DependsOn: []string{"a"}},
		{ID: "c", Prompt: "c", DependsOn: []string{"a"}},
		{ID: "d", Prompt: "d", DependsOn: []string{"b", "c"}},
	}}
	_, _, err := DAGRun(context.Background(),
		sandbox.NewNoSandboxProvider(),
		&promptAwareAgent{},
		&noopAuth{},
		h.opts,
	)
	if !errors.Is(err, ErrDiamondNotSupported) {
		t.Errorf("expected ErrDiamondNotSupported, got %v", err)
	}
}

func TestDAGRun_PreservesWinnerOnSynthesizerFailure(t *testing.T) {
	h := newDAGHarness(t)
	h.opts.Plan = planner.TaskDAG{Nodes: []planner.Node{
		{ID: "r0", Prompt: "ok-r0"},
		{ID: "r1", Prompt: "ok-r1"},
	}}
	// promptAwareAgent succeeds on chain prompts; synthesizer prompt
	// contains "consolidating" → fails.
	_, await, err := DAGRun(context.Background(),
		sandbox.NewNoSandboxProvider(),
		&promptAwareAgent{failPattern: "consolidating"},
		&noopAuth{},
		h.opts,
	)
	if err != nil {
		t.Fatalf("DAGRun: %v", err)
	}
	res := await()
	if res.Winner == "" {
		t.Errorf("Winner should be preserved even when synthesizer fails")
	}
	if res.Synthesizer == nil || res.Synthesizer.ExitCode == 0 {
		t.Errorf("expected non-nil synthesizer with non-zero exit, got %+v", res.Synthesizer)
	}
}

func TestDAGRun_OuterCopyIndexPropagates(t *testing.T) {
	h := newDAGHarness(t)
	h.opts.Plan = planner.TaskDAG{Nodes: []planner.Node{
		{ID: "r0", Prompt: "ok-r0"},
		{ID: "r1", Prompt: "ok-r1"},
	}}
	h.opts.OuterCopyIndex = 7
	_, await, _ := DAGRun(context.Background(),
		sandbox.NewNoSandboxProvider(),
		&promptAwareAgent{},
		&noopAuth{},
		h.opts,
	)
	res := await()
	if res.OuterCopyIndex != 7 {
		t.Errorf("OuterCopyIndex: got %d want 7", res.OuterCopyIndex)
	}
}

func TestDAGRun_ConcurrentOuterCopiesIndependent(t *testing.T) {
	const copies = 3
	plan := planner.TaskDAG{Nodes: []planner.Node{
		{ID: "r0", Prompt: "ok-r0"},
		{ID: "r1", Prompt: "ok-r1"},
	}}

	results := make([]DAGResult, copies)
	var wg sync.WaitGroup
	for i := 0; i < copies; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			h := newDAGHarness(t)
			h.opts.Plan = plan
			h.opts.OuterCopyIndex = idx
			h.opts.RunID = h.opts.RunID + "-" + string(rune('a'+idx))

			_, await, err := DAGRun(context.Background(),
				sandbox.NewNoSandboxProvider(),
				&promptAwareAgent{},
				&noopAuth{},
				h.opts,
			)
			if err != nil {
				t.Errorf("copy %d DAGRun: %v", idx, err)
				return
			}
			results[idx] = await()
		}(i)
	}
	wg.Wait()

	for i, r := range results {
		if r.OuterCopyIndex != i {
			t.Errorf("copy %d: OuterCopyIndex mismatch %d", i, r.OuterCopyIndex)
		}
		if r.Winner == "" {
			t.Errorf("copy %d: no winner", i)
		}
	}
}

func TestDecomposeChains_Branching(t *testing.T) {
	t.Parallel()
	plan := planner.TaskDAG{Nodes: []planner.Node{
		{ID: "r0", Prompt: "r0"},
		{ID: "a", Prompt: "a", DependsOn: []string{"r0"}},
		{ID: "b", Prompt: "b", DependsOn: []string{"a"}},
		{ID: "r1", Prompt: "r1"},
	}}
	chains := decomposeChains(plan, nil)
	if len(chains) != 2 {
		t.Fatalf("expected 2 chains, got %d", len(chains))
	}
	if chains[0].RootNodeID != "r0" || len(chains[0].Nodes) != 3 {
		t.Errorf("chain 0: root=%s nodes=%d, want r0 / 3", chains[0].RootNodeID, len(chains[0].Nodes))
	}
	if chains[1].RootNodeID != "r1" || len(chains[1].Nodes) != 1 {
		t.Errorf("chain 1: root=%s nodes=%d, want r1 / 1", chains[1].RootNodeID, len(chains[1].Nodes))
	}
}
