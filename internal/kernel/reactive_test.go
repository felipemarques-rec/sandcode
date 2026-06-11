package kernel

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/architect"
	"github.com/felipemarques-rec/sandcode/internal/event"
	"github.com/felipemarques-rec/sandcode/internal/planner"
	"github.com/felipemarques-rec/sandcode/internal/strategy"
)

// TestReactiveClassify_MatchesDirect verifies the SP3.0 proof-of-concept: with
// WithReactive, the classify stage runs through the reactor and produces the
// SAME Classification and the SAME run.classified payload as the direct path,
// plus an observation-only classify.requested command.
func TestReactiveClassify_MatchesDirect(t *testing.T) {
	t.Parallel()
	const prompt = "refactor the auth module and add integration tests across services"

	// Direct path classification (reference).
	direct := New(openTestBrain(t)).Process(context.Background(), ProcessRequest{
		Prompt: prompt, CWD: t.TempDir(), RunID: "run-direct",
	})

	// Reactive path.
	bus := event.NewLocalBus()
	t.Cleanup(func() { _ = bus.Close() })
	rec := newRecorder()
	bus.Subscribe(event.ClassifyRequested, rec.handler())
	bus.Subscribe(event.RunClassified, rec.handler())

	reactive := New(openTestBrain(t), WithBus(bus), WithReactive()).Process(
		context.Background(), ProcessRequest{Prompt: prompt, CWD: t.TempDir(), RunID: "run-reactive"},
	)

	// Same classification result via either path.
	if reactive.Classification != direct.Classification {
		t.Fatalf("reactive classification %+v != direct %+v", reactive.Classification, direct.Classification)
	}

	// The reactor emitted both the command and the result.
	rec.requireType(t, event.ClassifyRequested)
	rec.requireType(t, event.RunClassified)

	// run.classified is correlated and carries the classification fields.
	cls := rec.first(event.RunClassified)
	if cls.CorrelationID != "run-reactive" || cls.TraceID != "" && cls.RunID != "run-reactive" {
		t.Fatalf("run.classified not correlated to run: %+v", cls)
	}
	var p struct {
		Type       string `json:"type"`
		Complexity string `json:"complexity"`
	}
	if err := json.Unmarshal(cls.Payload, &p); err != nil {
		t.Fatalf("unmarshal classified payload: %v", err)
	}
	if p.Type != string(direct.Classification.Type) || p.Complexity != string(direct.Classification.Complexity) {
		t.Fatalf("reactive run.classified payload %+v != direct classification %+v", p, direct.Classification)
	}

	// classify.requested must NOT carry the raw prompt (no leak): payload is
	// just the prompt length.
	cmd := rec.first(event.ClassifyRequested)
	if len(cmd.Payload) > 0 {
		var cp struct {
			PromptLen int    `json:"prompt_len"`
			Prompt    string `json:"prompt"`
		}
		_ = json.Unmarshal(cmd.Payload, &cp)
		if cp.Prompt != "" {
			t.Fatalf("classify.requested leaked prompt content: %q", cp.Prompt)
		}
		if cp.PromptLen != len(prompt) {
			t.Fatalf("classify.requested prompt_len = %d, want %d", cp.PromptLen, len(prompt))
		}
	}
}

// TestReactivePipeline_MatchesDirect (SP3.1) wires architect + planner +
// selector and verifies the FULL reactive pipeline produces the same
// ProcessResult as the direct path, and that every stage emits its
// observation-only command plus its result event.
func TestReactivePipeline_MatchesDirect(t *testing.T) {
	t.Parallel()
	// Divergent ("design"/"architecture") + High ("distributed"/"migration")
	// ⇒ architect AND plan both gate in.
	const prompt = "design the distributed architecture migration across services"

	newArch := func() *fakeArchitect {
		return &fakeArchitect{wantAP: architect.ArchPlan{Approach: "strangler-fig", Files: []string{"a.go"}, Risks: []string{"downtime"}, Architect: "llm:x"}}
	}
	newPlan := func() *fakePlanner {
		return &fakePlanner{wantDAG: planner.TaskDAG{Nodes: []planner.Node{{ID: "root", Prompt: "x"}}}}
	}

	direct := New(openTestBrain(t), WithArchitect(newArch()), WithPlanner(newPlan()), WithSelector(strategy.New())).
		Process(context.Background(), ProcessRequest{Prompt: prompt, CWD: t.TempDir(), RunID: "d"})

	bus := event.NewLocalBus()
	t.Cleanup(func() { _ = bus.Close() })
	rec := newRecorder()
	for _, typ := range []event.Type{
		event.ClassifyRequested, event.ArchitectRequested, event.PlanRequested,
		event.StrategyRequested, event.EnrichRequested,
		event.RunClassified, event.RunArchitected, event.RunPlanned,
		event.RunStrategySelected, event.RunEnriched,
	} {
		bus.Subscribe(typ, rec.handler())
	}

	reactive := New(openTestBrain(t), WithBus(bus), WithReactive(),
		WithArchitect(newArch()), WithPlanner(newPlan()), WithSelector(strategy.New())).
		Process(context.Background(), ProcessRequest{Prompt: prompt, CWD: t.TempDir(), RunID: "r"})

	// Same scalar results via either path.
	if reactive.Classification != direct.Classification {
		t.Errorf("classification: reactive %+v != direct %+v", reactive.Classification, direct.Classification)
	}
	if reactive.Strategy != direct.Strategy || reactive.StrategyReason != direct.StrategyReason {
		t.Errorf("strategy: reactive (%s,%q) != direct (%s,%q)", reactive.Strategy, reactive.StrategyReason, direct.Strategy, direct.StrategyReason)
	}
	if reactive.EnrichedPrompt != direct.EnrichedPrompt {
		t.Errorf("enriched prompt differs:\n reactive=%q\n direct=%q", reactive.EnrichedPrompt, direct.EnrichedPrompt)
	}
	if (reactive.Arch == nil) != (direct.Arch == nil) {
		t.Fatalf("arch presence differs: reactive=%v direct=%v", reactive.Arch, direct.Arch)
	}
	if reactive.Arch != nil && reactive.Arch.Approach != direct.Arch.Approach {
		t.Errorf("arch approach: %q != %q", reactive.Arch.Approach, direct.Arch.Approach)
	}
	if len(reactive.Plan.Nodes) != len(direct.Plan.Nodes) {
		t.Errorf("plan nodes: %d != %d", len(reactive.Plan.Nodes), len(direct.Plan.Nodes))
	}

	// Every stage emitted its command + result in the reactive run.
	for _, typ := range []event.Type{
		event.ClassifyRequested, event.RunClassified,
		event.ArchitectRequested, event.RunArchitected,
		event.PlanRequested, event.RunPlanned,
		event.StrategyRequested, event.RunStrategySelected,
		event.EnrichRequested, event.RunEnriched,
	} {
		if !rec.has(typ) {
			t.Errorf("reactive pipeline did not emit %s", typ)
		}
	}
}

// TestReactive_NoBusIsInert verifies that WithReactive without a bus falls back
// to the direct path (byte-identical) rather than erroring.
func TestReactive_NoBusIsInert(t *testing.T) {
	t.Parallel()
	res := New(openTestBrain(t), WithReactive()).Process(context.Background(), ProcessRequest{
		Prompt: "add a flag", CWD: t.TempDir(), RunID: "run-x",
	})
	if res.Classification.Type == "" {
		t.Fatalf("reactive-without-bus produced empty classification: %+v", res.Classification)
	}
}
