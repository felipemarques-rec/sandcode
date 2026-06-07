package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/agent"
	"github.com/felipemarques-rec/sandcode/internal/auth"
	"github.com/felipemarques-rec/sandcode/internal/event"
	"github.com/felipemarques-rec/sandcode/internal/judge"
	"github.com/felipemarques-rec/sandcode/internal/orchestrator"
	"github.com/felipemarques-rec/sandcode/internal/planner"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
)

// runDAGFakeAgent is a per-call shell-script-based agent. Local to the
// CLI test package; identical shape to the orchestrator package's
// fakeAgent.
type runDAGFakeAgent struct{ script string }

func (*runDAGFakeAgent) Name() string { return "cli-dag-fake" }
func (f *runDAGFakeAgent) BuildCommand(_ agent.RunOptions) agent.Command {
	return agent.Command{Argv: []string{"sh", "-c", f.script}}
}
func (*runDAGFakeAgent) ParseLine(line string) (agent.StreamEvent, bool) {
	if line == "" {
		return agent.StreamEvent{}, false
	}
	return agent.StreamEvent{Kind: agent.EventText, Text: line}, true
}
func (*runDAGFakeAgent) AuthHints() agent.AuthHints { return agent.AuthHints{} }

type runDAGNoopAuth struct{}

func (*runDAGNoopAuth) Name() string                                                 { return "noop" }
func (*runDAGNoopAuth) Apply(spec *sandbox.SandboxSpec, hints agent.AuthHints) error { return nil }

var _ auth.Provider = (*runDAGNoopAuth)(nil)

// runDAGStubJudge picks the first chain as winner — enough to let the
// DAG complete and the synthesizer run without any LLM call.
type runDAGStubJudge struct{}

func (runDAGStubJudge) Name() string { return "cli-dag-stub" }
func (runDAGStubJudge) Rank(_ context.Context, _ string, cands []judge.Candidate) (judge.Ranking, error) {
	if len(cands) == 0 {
		return judge.Ranking{}, errors.New("no candidates")
	}
	return judge.Ranking{
		Winner:    cands[0].RunID,
		Rationale: "cli-dag-stub: picked first",
		Judge:     "cli-dag-stub",
	}, nil
}

func runDAGInitRepo(t *testing.T) string {
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

// TestRunDAGCommand_MultiAgentRoundRobin closes the CLI gap noted in
// the Slice 6 handoff: `sandcode run --dag --agent a,b,c` previously
// dropped agents[1:]; now `runDAGCommandWithJudge` threads the full
// slice into DAGOptions.Agents for within-DAG round-robin. Each fake
// agent writes a marker file into its chain's worktree; after the run
// we walk the chain directories and assert the marker per chain.
func TestRunDAGCommand_MultiAgentRoundRobin(t *testing.T) {
	t.Parallel()

	repo := runDAGInitRepo(t)

	// 2-root plan as JSON for --dag-from-file (deterministic, no LLM).
	plan := planner.TaskDAG{Nodes: []planner.Node{
		{ID: "root0", Prompt: "do a"},
		{ID: "root1", Prompt: "do b"},
	}}
	planPath := filepath.Join(t.TempDir(), "plan.json")
	planBytes, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	if err := os.WriteFile(planPath, planBytes, 0o644); err != nil {
		t.Fatalf("write plan: %v", err)
	}

	a0 := &runDAGFakeAgent{script: `echo "a0 running"; touch agent0.marker; echo done`}
	a1 := &runDAGFakeAgent{script: `echo "a1 running"; touch agent1.marker; echo done`}

	lb := event.NewLocalBus()
	t.Cleanup(func() { _ = lb.Close() })

	// Build runFlags with --dag --dag-from-file. judgeKind="none" since
	// we inject the stub judge directly via runDAGCommandWithJudge.
	f := runFlags{
		dag:          true,
		dagFromFile:  planPath,
		image:        "ignored-by-nosandbox",
		workdir:      filepath.Join(repo, ".sandcode", "work", t.Name(), "wd"),
		keepWorktree: true, // so we can inspect markers after the run
		judgeKind:    "none",
	}

	err = runDAGCommandWithJudge(
		context.Background(),
		sandbox.NewNoSandboxProvider(),
		[]agent.Provider{a0, a1},
		&runDAGNoopAuth{},
		nil, // store
		nil, // kernel
		lb,
		nil, // governance engine
		nil, // budget guard
		orchestrator.RefineOptions{},
		"two roots",
		repo,
		"branch", // gitm.Strategy
		sandbox.Limits{},
		f,
		runDAGStubJudge{}, // <-- injected stub judge
	)
	if err != nil {
		t.Fatalf("runDAGCommandWithJudge: %v", err)
	}

	// Find the chain worktrees under .sandcode/work/.../dag/chain-0 and
	// chain-1 and verify each agent's marker landed in its own chain.
	workRoot := filepath.Join(repo, ".sandcode", "work")
	want := map[string]string{
		"chain-0": "agent0.marker",
		"chain-1": "agent1.marker",
	}
	found := map[string]bool{}
	err = filepath.Walk(workRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		marker, ok := want[base]
		if !ok {
			return nil
		}
		// path should be the chain worktree itself.
		if _, err := os.Stat(filepath.Join(path, marker)); err == nil {
			found[base] = true
		} else {
			t.Errorf("chain %s: expected marker %q in worktree %s: %v", base, marker, path, err)
		}
		// Cross-check the OTHER agent's marker is NOT present.
		otherMarker := want["chain-0"]
		if marker == otherMarker {
			otherMarker = want["chain-1"]
		}
		if _, err := os.Stat(filepath.Join(path, otherMarker)); err == nil {
			t.Errorf("chain %s: unexpected cross-agent marker %q present", base, otherMarker)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk workRoot: %v", err)
	}
	for chainID := range want {
		if !found[chainID] {
			t.Errorf("chain %s worktree not found under %s", chainID, workRoot)
		}
	}
}
