package planner

import (
	"context"
	"testing"
)

func TestNoOpPlanner_SingleNodeEchosPrompt(t *testing.T) {
	const prompt = "write a haiku about goroutines"
	d, err := NoOpPlanner{}.Decompose(context.Background(), prompt)
	if err != nil {
		t.Fatalf("Decompose: %v", err)
	}
	if len(d.Nodes) != 1 {
		t.Fatalf("len(Nodes) = %d, want 1", len(d.Nodes))
	}
	got := d.Nodes[0]
	if got.ID != NoOpNodeID {
		t.Errorf("ID = %q, want %q", got.ID, NoOpNodeID)
	}
	if got.Prompt != prompt {
		t.Errorf("Prompt = %q, want %q", got.Prompt, prompt)
	}
	if len(got.DependsOn) != 0 {
		t.Errorf("DependsOn = %v, want empty", got.DependsOn)
	}
}

func TestNoOpPlanner_EmptyPromptStillFailsValidation(t *testing.T) {
	// NoOpPlanner runs Validate before returning, so an empty prompt
	// surfaces as ErrEmptyPrompt rather than producing an invalid DAG.
	// This documents the contract: the LLM impl must do the same.
	_, err := NoOpPlanner{}.Decompose(context.Background(), "")
	if err == nil {
		t.Fatal("err = nil, want non-nil for empty prompt")
	}
}

// Compile-time assertion that NoOpPlanner satisfies Planner.
var _ Planner = NoOpPlanner{}
