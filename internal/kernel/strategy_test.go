package kernel

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/event"
	"github.com/felipemarques-rec/sandcode/internal/planner"
	"github.com/felipemarques-rec/sandcode/internal/strategy"
)

func TestProcess_SelectorNotConfigured_NoEventOrStrategy(t *testing.T) {
	t.Parallel()

	br := openTestBrain(t)
	bus := event.NewLocalBus()
	t.Cleanup(func() { _ = bus.Close() })

	rec := newRecorder()
	bus.Subscribe(event.RunStrategySelected, rec.handler())

	k := New(br, WithBus(bus))
	res := k.Process(context.Background(), ProcessRequest{
		Prompt: "redesign auth", // high complexity, would otherwise refine
		CWD:    t.TempDir(),
		RunID:  "strat-test-001",
	})

	if rec.has(event.RunStrategySelected) {
		t.Errorf("event.RunStrategySelected fired without a selector configured")
	}
	if res.Strategy != "" {
		t.Errorf("Strategy = %q, want empty", res.Strategy)
	}
	if res.StrategyReason != "" {
		t.Errorf("StrategyReason = %q, want empty", res.StrategyReason)
	}
}

func TestProcess_SelectorChoosesRefineOnHighComplexity(t *testing.T) {
	t.Parallel()

	br := openTestBrain(t)
	bus := event.NewLocalBus()
	t.Cleanup(func() { _ = bus.Close() })

	rec := newRecorder()
	bus.Subscribe(event.RunStrategySelected, rec.handler())

	k := New(br, WithBus(bus), WithSelector(strategy.New()))
	res := k.Process(context.Background(), ProcessRequest{
		// "redesign" triggers ComplexityHigh in brain/classifier.go
		Prompt: "redesign the auth system for multi-tenant support",
		CWD:    t.TempDir(),
		RunID:  "strat-test-002",
	})

	if res.Strategy != strategy.StrategyRefine {
		t.Errorf("Strategy = %s, want %s", res.Strategy, strategy.StrategyRefine)
	}
	if res.StrategyReason == "" {
		t.Errorf("StrategyReason empty, want non-empty")
	}

	rec.requireType(t, event.RunStrategySelected)
	ev := rec.first(event.RunStrategySelected)
	if ev.RunID != "strat-test-002" {
		t.Errorf("event.RunID = %q", ev.RunID)
	}
	var payload struct {
		Strategy string `json:"strategy"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Strategy != string(strategy.StrategyRefine) {
		t.Errorf("payload.Strategy = %q, want %q", payload.Strategy, strategy.StrategyRefine)
	}
	if payload.Reason == "" {
		t.Errorf("payload.Reason empty")
	}
}

func TestProcess_SelectorChoosesParallelOnMultiRootPlan(t *testing.T) {
	t.Parallel()

	br := openTestBrain(t)
	bus := event.NewLocalBus()
	t.Cleanup(func() { _ = bus.Close() })

	// Planner returns a multi-root DAG so the selector's first rule fires.
	multiRoot := planner.TaskDAG{Nodes: []planner.Node{
		{ID: "a", Prompt: "x"},
		{ID: "b", Prompt: "y"},
	}}
	fp := &fakePlanner{wantDAG: multiRoot}

	k := New(br, WithBus(bus), WithPlanner(fp), WithSelector(strategy.New()))
	res := k.Process(context.Background(), ProcessRequest{
		Prompt: "redesign distributed cache layer", // high complexity → planner fires
		CWD:    t.TempDir(),
		RunID:  "strat-test-003",
	})

	if res.Strategy != strategy.StrategyParallel {
		t.Errorf("Strategy = %s, want %s (multi-root plan must outrank refine)",
			res.Strategy, strategy.StrategyParallel)
	}
}

func TestProcess_SelectorChoosesSingleOnLowComplexity(t *testing.T) {
	t.Parallel()

	br := openTestBrain(t)
	bus := event.NewLocalBus()
	t.Cleanup(func() { _ = bus.Close() })

	k := New(br, WithBus(bus), WithSelector(strategy.New()))
	res := k.Process(context.Background(), ProcessRequest{
		Prompt: "fix typo in README",
		CWD:    t.TempDir(),
		RunID:  "strat-test-004",
	})

	if res.Strategy != strategy.StrategySingle {
		t.Errorf("Strategy = %s, want %s", res.Strategy, strategy.StrategySingle)
	}
}
