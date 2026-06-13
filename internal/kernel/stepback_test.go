package kernel

import (
	"context"
	"strings"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/event"
	"github.com/felipemarques-rec/sandcode/internal/planner"
	"github.com/felipemarques-rec/sandcode/internal/stepback"
)

// fakeStepBack is a deterministic stand-in for stepback.StepBack.
type fakeStepBack struct {
	res   stepback.Result
	calls int
}

func (f *fakeStepBack) Reason(_ context.Context, _ stepback.ReasonRequest) (stepback.Result, error) {
	f.calls++
	return f.res, nil
}

// TestProcess_StepBackGuidanceThreaded asserts the principles are prepended to the
// working prompt feeding the planner + enricher on a divergent/high prompt.
func TestProcess_StepBackGuidanceThreaded(t *testing.T) {
	const prompt = "design the distributed architecture migration across services"
	br := openTestBrain(t)
	fp := &fakePlanner{wantDAG: planner.TaskDAG{Nodes: []planner.Node{{ID: "root", Prompt: "x"}}}}
	fs := &fakeStepBack{res: stepback.Result{Principles: []string{"separate policy from mechanism"}, Reasoner: "llm:x"}}

	res := New(br, WithStepBack(fs), WithPlanner(fp)).
		Process(context.Background(), ProcessRequest{Prompt: prompt, CWD: t.TempDir(), RunID: "s"})

	if fs.calls != 1 {
		t.Fatalf("step-back calls = %d, want 1", fs.calls)
	}
	got := fp.calls[0]
	if !strings.Contains(got, "Step-back principles") || !strings.Contains(got, "separate policy from mechanism") {
		t.Fatalf("planner did not receive step-back guidance:\n%s", got)
	}
	if !strings.Contains(res.EnrichedPrompt, "Step-back principles") {
		t.Fatalf("enriched prompt missing step-back guidance:\n%s", res.EnrichedPrompt)
	}
}

// TestProcess_StepBackSkippedWhenSimple asserts the gate: a low-complexity
// convergent prompt does NOT invoke step-back.
func TestProcess_StepBackSkippedWhenSimple(t *testing.T) {
	fs := &fakeStepBack{res: stepback.Result{Principles: []string{"x"}}}
	New(openTestBrain(t), WithStepBack(fs)).
		Process(context.Background(), ProcessRequest{Prompt: "fix typo", CWD: t.TempDir(), RunID: "s"})
	if fs.calls != 0 {
		t.Fatalf("step-back invoked on simple prompt: calls = %d", fs.calls)
	}
}

// TestReactiveStepBack_MatchesDirect asserts the reactive path emits the
// observation-only command + result and threads identical guidance.
func TestReactiveStepBack_MatchesDirect(t *testing.T) {
	const prompt = "design the distributed architecture migration across services"
	res := stepback.Result{Principles: []string{"idempotent retries"}, Reasoner: "llm:x"}

	direct := New(openTestBrain(t), WithStepBack(&fakeStepBack{res: res})).
		Process(context.Background(), ProcessRequest{Prompt: prompt, CWD: t.TempDir(), RunID: "d"})

	bus := event.NewLocalBus()
	t.Cleanup(func() { _ = bus.Close() })
	rec := newRecorder()
	bus.Subscribe(event.StepBackRequested, rec.handler())
	bus.Subscribe(event.RunSteppedBack, rec.handler())

	reactive := New(openTestBrain(t), WithBus(bus), WithReactive(), WithStepBack(&fakeStepBack{res: res})).
		Process(context.Background(), ProcessRequest{Prompt: prompt, CWD: t.TempDir(), RunID: "r"})

	rec.requireType(t, event.StepBackRequested)
	rec.requireType(t, event.RunSteppedBack)
	if reactive.EnrichedPrompt != direct.EnrichedPrompt {
		t.Fatalf("enriched prompt differs:\n reactive=%q\n direct=%q", reactive.EnrichedPrompt, direct.EnrichedPrompt)
	}
}
