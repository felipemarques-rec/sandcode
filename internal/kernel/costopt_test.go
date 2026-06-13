package kernel

import (
	"context"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/costopt"
	"github.com/felipemarques-rec/sandcode/internal/event"
)

// TestProcess_ModelRouted asserts the router sets ProcessResult.Model and emits
// the observation-only result event on the direct path.
func TestProcess_ModelRouted(t *testing.T) {
	const prompt = "design the distributed architecture migration across services" // divergent/high → strong
	bus := event.NewLocalBus()
	t.Cleanup(func() { _ = bus.Close() })
	rec := newRecorder()
	bus.Subscribe(event.RunModelRouted, rec.handler())

	res := New(openTestBrain(t), WithBus(bus), WithModelRouter(costopt.New())).
		Process(context.Background(), ProcessRequest{Prompt: prompt, CWD: t.TempDir(), RunID: "m"})

	if res.Model != costopt.ModelStrong {
		t.Fatalf("Model = %q, want %q", res.Model, costopt.ModelStrong)
	}
	rec.requireType(t, event.RunModelRouted)
}

// TestProcess_NoRouterEmptyModel asserts byte-identical default: no router ⇒ empty Model.
func TestProcess_NoRouterEmptyModel(t *testing.T) {
	res := New(openTestBrain(t)).
		Process(context.Background(), ProcessRequest{Prompt: "fix typo", CWD: t.TempDir(), RunID: "m"})
	if res.Model != "" {
		t.Fatalf("Model = %q, want empty (no router)", res.Model)
	}
}

// TestReactiveModelRoute_MatchesDirect asserts reactive == direct for the new stage.
func TestReactiveModelRoute_MatchesDirect(t *testing.T) {
	const prompt = "fix a small convergent bug" // low/convergent → fast
	direct := New(openTestBrain(t), WithModelRouter(costopt.New())).
		Process(context.Background(), ProcessRequest{Prompt: prompt, CWD: t.TempDir(), RunID: "d"})

	bus := event.NewLocalBus()
	t.Cleanup(func() { _ = bus.Close() })
	rec := newRecorder()
	bus.Subscribe(event.ModelRouteRequested, rec.handler())
	bus.Subscribe(event.RunModelRouted, rec.handler())

	reactive := New(openTestBrain(t), WithBus(bus), WithReactive(), WithModelRouter(costopt.New())).
		Process(context.Background(), ProcessRequest{Prompt: prompt, CWD: t.TempDir(), RunID: "r"})

	if reactive.Model != direct.Model {
		t.Fatalf("reactive Model %q != direct %q", reactive.Model, direct.Model)
	}
	rec.requireType(t, event.ModelRouteRequested)
	rec.requireType(t, event.RunModelRouted)
}
