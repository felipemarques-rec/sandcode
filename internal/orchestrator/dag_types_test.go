package orchestrator

import (
	"testing"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/planner"
)

// TestDAGResult_ZeroValueShape pins down the zero-value contract of
// DAGResult so callers can rely on nil-safety for slices/pointers.
func TestDAGResult_ZeroValueShape(t *testing.T) {
	t.Parallel()
	var r DAGResult
	if r.Plan.Nodes != nil {
		t.Errorf("zero DAGResult.Plan.Nodes should be nil")
	}
	if r.Chains != nil {
		t.Errorf("zero DAGResult.Chains should be nil")
	}
	if r.Synthesizer != nil {
		t.Errorf("zero DAGResult.Synthesizer should be nil")
	}
	if r.Winner != "" || r.JudgeRationale != "" {
		t.Errorf("zero DAGResult winner/rationale should be empty strings")
	}
	if r.Duration != 0 {
		t.Errorf("zero DAGResult.Duration should be 0")
	}
	if r.Error != nil {
		t.Errorf("zero DAGResult.Error should be nil")
	}
}

func TestChainResult_FailureMarker(t *testing.T) {
	t.Parallel()
	c := ChainResult{ChainID: "c1", Success: false, FailedAt: "node-b"}
	if c.Success || c.FailedAt != "node-b" {
		t.Errorf("ChainResult failure marker shape unexpected: %+v", c)
	}
}

func TestNodeResult_AttemptsField(t *testing.T) {
	t.Parallel()
	n := NodeResult{NodeID: "x", Attempts: 3}
	if n.Attempts != 3 {
		t.Errorf("Attempts not preserved: %+v", n)
	}
}

func TestAgentInvocationResult_ZeroValue(t *testing.T) {
	t.Parallel()
	var r AgentInvocationResult
	if r.ExitCode != 0 || r.Completion != "" || r.Err != nil || r.Duration != 0 {
		t.Errorf("zero AgentInvocationResult unexpected: %+v", r)
	}
}

// Type sanity: confirm DAGResult composes the planner + duration types
// we expect, catching breakage if those upstream packages change shape.
var _ planner.TaskDAG = DAGResult{}.Plan
var _ time.Duration = DAGResult{}.Duration
