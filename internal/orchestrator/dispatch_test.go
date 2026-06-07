package orchestrator

import (
	"strings"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/planner"
	"github.com/felipemarques-rec/sandcode/internal/strategy"
)

func TestDispatchKind_Table(t *testing.T) {
	t.Parallel()

	singleRoot := planner.TaskDAG{Nodes: []planner.Node{{ID: "a", Prompt: "x"}}}
	multiRoot := planner.TaskDAG{Nodes: []planner.Node{
		{ID: "a", Prompt: "x"},
		{ID: "b", Prompt: "y"},
	}}
	emptyPlan := planner.TaskDAG{}

	cases := []struct {
		name         string
		strat        strategy.Strategy
		plan         planner.TaskDAG
		agentCount   int
		parallelN    int
		hasJudge     bool
		refineActive bool
		wantKind     DispatchKind
		wantReason   string // substring match
	}{
		{"single-strategy", strategy.StrategySingle, emptyPlan, 1, 0, false, false, DispatchSingle, "kernel selected single"},
		{"empty-strategy-default", "", emptyPlan, 1, 0, false, false, DispatchSingle, "no strategy"},

		{"refine-active", strategy.StrategyRefine, emptyPlan, 1, 0, false, true, DispatchRefine, "kernel selected refine"},
		{"refine-disabled-falls-back-to-single", strategy.StrategyRefine, emptyPlan, 1, 0, false, false, DispatchSingle, "RefineOptions disabled"},

		{"parallel-multiroot-with-judge-multi-agent", strategy.StrategyParallel, multiRoot, 2, 0, true, false, DispatchDAG, "parallel + plan multi-root"},
		{"parallel-multiroot-with-judge-single-agent", strategy.StrategyParallel, multiRoot, 1, 0, true, false, DispatchDAG, "parallel + plan multi-root"},
		{"parallel-multiroot-no-judge-multi-agent", strategy.StrategyParallel, multiRoot, 2, 0, false, false, DispatchParallel, "dag requires judge; falling back to parallel"},
		{"parallel-multiroot-no-judge-single-agent", strategy.StrategyParallel, multiRoot, 1, 0, false, false, DispatchSingle, "falling back to single"},

		{"parallel-singleroot-multi-agent", strategy.StrategyParallel, singleRoot, 3, 0, false, false, DispatchParallel, "multi-agent fan-out"},
		{"parallel-singleroot-parallelN", strategy.StrategyParallel, singleRoot, 1, 4, false, false, DispatchParallel, "--parallel replication"},
		{"parallel-singleroot-no-agents", strategy.StrategyParallel, singleRoot, 1, 1, false, false, DispatchSingle, "no agents; falling back to single"},
		{"parallel-emptyplan-multi-agent", strategy.StrategyParallel, emptyPlan, 2, 0, false, false, DispatchParallel, "multi-agent fan-out"},
		{"parallel-emptyplan-no-agents", strategy.StrategyParallel, emptyPlan, 1, 0, false, false, DispatchSingle, "no agents; falling back to single"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotKind, gotReason := dispatchKind(tc.strat, tc.plan, tc.agentCount, tc.parallelN, tc.hasJudge, tc.refineActive)
			if gotKind != tc.wantKind {
				t.Errorf("kind: got %v, want %v (reason=%q)", gotKind, tc.wantKind, gotReason)
			}
			if !strings.Contains(gotReason, tc.wantReason) {
				t.Errorf("reason: got %q, want substring %q", gotReason, tc.wantReason)
			}
		})
	}
}

func TestDispatchKind_String(t *testing.T) {
	t.Parallel()
	cases := map[DispatchKind]string{
		DispatchSingle:   "single",
		DispatchRefine:   "refine",
		DispatchParallel: "parallel",
		DispatchDAG:      "dag",
	}
	for k, want := range cases {
		if k.String() != want {
			t.Errorf("DispatchKind(%d).String() = %q, want %q", int(k), k.String(), want)
		}
	}
}
