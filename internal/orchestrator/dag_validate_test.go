package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/judge"
	"github.com/felipemarques-rec/sandcode/internal/planner"
)

// stubJudge satisfies judge.Judge for shape-validation tests that
// don't actually run any ranking. The real judge isn't exercised here;
// validateDAG only checks "is a Judge present when needed?".
type stubJudge struct{}

func (stubJudge) Name() string { return "stub" }
func (stubJudge) Rank(_ context.Context, _ string, _ []judge.Candidate) (judge.Ranking, error) {
	return judge.Ranking{}, nil
}

func TestValidateDAG_EmptyPlan(t *testing.T) {
	t.Parallel()
	err := validateDAG(DAGOptions{Plan: planner.TaskDAG{}})
	if !errors.Is(err, ErrEmptyPlan) {
		t.Errorf("expected ErrEmptyPlan, got %v", err)
	}
}

func TestValidateDAG_RejectsCycle(t *testing.T) {
	t.Parallel()
	plan := planner.TaskDAG{Nodes: []planner.Node{
		{ID: "a", Prompt: "a", DependsOn: []string{"b"}},
		{ID: "b", Prompt: "b", DependsOn: []string{"a"}},
	}}
	err := validateDAG(DAGOptions{Plan: plan})
	if !errors.Is(err, planner.ErrCycle) {
		t.Errorf("expected planner.ErrCycle, got %v", err)
	}
}

func TestValidateDAG_RejectsDiamond(t *testing.T) {
	t.Parallel()
	// a → b, a → c, b+c → d  (d has 2 deps = diamond)
	plan := planner.TaskDAG{Nodes: []planner.Node{
		{ID: "a", Prompt: "a"},
		{ID: "b", Prompt: "b", DependsOn: []string{"a"}},
		{ID: "c", Prompt: "c", DependsOn: []string{"a"}},
		{ID: "d", Prompt: "d", DependsOn: []string{"b", "c"}},
	}}
	err := validateDAG(DAGOptions{Plan: plan, Judge: stubJudge{}})
	if !errors.Is(err, ErrDiamondNotSupported) {
		t.Errorf("expected ErrDiamondNotSupported, got %v", err)
	}
}

func TestValidateDAG_MultiRootRequiresJudge(t *testing.T) {
	t.Parallel()
	plan := planner.TaskDAG{Nodes: []planner.Node{
		{ID: "r1", Prompt: "r1"},
		{ID: "r2", Prompt: "r2"},
	}}
	err := validateDAG(DAGOptions{Plan: plan, Judge: nil})
	if !errors.Is(err, ErrJudgeRequiredForMultiRoot) {
		t.Errorf("expected ErrJudgeRequiredForMultiRoot, got %v", err)
	}

	// Same plan with a judge → shape passes.
	if err := validateDAG(DAGOptions{Plan: plan, Judge: stubJudge{}}); err != nil {
		t.Errorf("multi-root with judge: unexpected error: %v", err)
	}
}

func TestValidateDAG_SingleRootSingleNode_OK(t *testing.T) {
	t.Parallel()
	plan := planner.TaskDAG{Nodes: []planner.Node{
		{ID: "only", Prompt: "do thing"},
	}}
	if err := validateDAG(DAGOptions{Plan: plan}); err != nil {
		t.Errorf("single-root single-node: unexpected error: %v", err)
	}
}

func TestValidateDAG_SingleRootChain_OK(t *testing.T) {
	t.Parallel()
	plan := planner.TaskDAG{Nodes: []planner.Node{
		{ID: "a", Prompt: "a"},
		{ID: "b", Prompt: "b", DependsOn: []string{"a"}},
		{ID: "c", Prompt: "c", DependsOn: []string{"b"}},
	}}
	if err := validateDAG(DAGOptions{Plan: plan}); err != nil {
		t.Errorf("single-chain: unexpected error: %v", err)
	}
}

func TestValidateDAG_FullFanOut_OK(t *testing.T) {
	t.Parallel()
	plan := planner.TaskDAG{Nodes: []planner.Node{
		{ID: "r0", Prompt: "r0"},
		{ID: "r1", Prompt: "r1"},
		{ID: "r2", Prompt: "r2"},
	}}
	if err := validateDAG(DAGOptions{Plan: plan, Judge: stubJudge{}}); err != nil {
		t.Errorf("full fan-out: unexpected error: %v", err)
	}
}
