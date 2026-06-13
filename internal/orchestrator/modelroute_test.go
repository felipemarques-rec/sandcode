package orchestrator

import (
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/agent"
)

func TestApplyRoutedModel(t *testing.T) {
	// Router chose a model → override (router wins, even over an explicit model).
	got := applyRoutedModel(agent.RunOptions{Model: "user-pinned"}, "routed-model")
	if got.Model != "routed-model" {
		t.Fatalf("with route: Model = %q, want routed-model", got.Model)
	}
	// No route (empty) → leave AgentOpts untouched (byte-identical).
	got = applyRoutedModel(agent.RunOptions{Model: "user-pinned"}, "")
	if got.Model != "user-pinned" {
		t.Fatalf("no route: Model = %q, want user-pinned", got.Model)
	}
}
