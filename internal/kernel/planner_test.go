package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/event"
	"github.com/felipemarques-rec/sandcode/internal/planner"
)

// fakePlanner is a deterministic stand-in for planner.Planner. Calls()
// returns the prompts seen, decompose can be made to error, and the
// returned DAG is configurable.
type fakePlanner struct {
	mu      sync.Mutex
	calls   []string
	wantErr error
	wantDAG planner.TaskDAG
}

func (f *fakePlanner) Decompose(_ context.Context, prompt string) (planner.TaskDAG, error) {
	f.mu.Lock()
	f.calls = append(f.calls, prompt)
	f.mu.Unlock()
	if f.wantErr != nil {
		return planner.TaskDAG{}, f.wantErr
	}
	return f.wantDAG, nil
}

func (f *fakePlanner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// TestProcess_PlannerNotConfigured_NoInvocationOrEvent confirms that
// omitting WithPlanner leaves the new pipeline silent.
func TestProcess_PlannerNotConfigured_NoInvocationOrEvent(t *testing.T) {
	t.Parallel()

	br := openTestBrain(t)
	bus := event.NewLocalBus()
	t.Cleanup(func() { _ = bus.Close() })

	rec := newRecorder()
	bus.Subscribe(event.RunPlanned, rec.handler())

	k := New(br, WithBus(bus))
	res := k.Process(context.Background(), ProcessRequest{
		// "redesign" triggers ComplexityHigh — but without a planner
		// configured, decomposition is skipped entirely.
		Prompt: "redesign the auth system",
		CWD:    t.TempDir(),
		RunID:  "plan-test-001",
	})

	if rec.has(event.RunPlanned) {
		t.Errorf("event.RunPlanned fired without a planner configured")
	}
	if len(res.Plan.Nodes) != 0 {
		t.Errorf("Plan.Nodes = %v, want empty", res.Plan.Nodes)
	}
}

// TestProcess_PlannerNotInvokedForLowComplexity confirms the gate: a
// planner is configured but the classification is below the threshold.
func TestProcess_PlannerNotInvokedForLowComplexity(t *testing.T) {
	t.Parallel()

	br := openTestBrain(t)
	bus := event.NewLocalBus()
	t.Cleanup(func() { _ = bus.Close() })

	rec := newRecorder()
	bus.Subscribe(event.RunPlanned, rec.handler())

	fp := &fakePlanner{wantDAG: planner.TaskDAG{Nodes: []planner.Node{{ID: "root", Prompt: "x"}}}}
	k := New(br, WithBus(bus), WithPlanner(fp))

	k.Process(context.Background(), ProcessRequest{
		Prompt: "fix typo in README",
		CWD:    t.TempDir(),
		RunID:  "plan-test-002",
	})

	if fp.callCount() != 0 {
		t.Errorf("planner called %d times for low-complexity prompt, want 0", fp.callCount())
	}
	if rec.has(event.RunPlanned) {
		t.Errorf("event.RunPlanned fired below complexity threshold")
	}
}

// TestProcess_PlannerInvokedForHighComplexity is the happy path:
// classify=high → planner runs → event emitted → ProcessResult.Plan set.
func TestProcess_PlannerInvokedForHighComplexity(t *testing.T) {
	t.Parallel()

	br := openTestBrain(t)
	bus := event.NewLocalBus()
	t.Cleanup(func() { _ = bus.Close() })

	rec := newRecorder()
	bus.Subscribe(event.RunPlanned, rec.handler())

	dag := planner.TaskDAG{Nodes: []planner.Node{
		{ID: "schema", Prompt: "design schema"},
		{ID: "endpoints", Prompt: "build endpoints", DependsOn: []string{"schema"}},
	}}
	fp := &fakePlanner{wantDAG: dag}
	k := New(br, WithBus(bus), WithPlanner(fp))

	res := k.Process(context.Background(), ProcessRequest{
		// "redesign" → ComplexityHigh per brain/classifier.go
		Prompt: "redesign the auth system for multi-tenant support",
		CWD:    t.TempDir(),
		RunID:  "plan-test-003",
	})

	if fp.callCount() != 1 {
		t.Fatalf("planner called %d times, want 1", fp.callCount())
	}
	if len(res.Plan.Nodes) != 2 {
		t.Errorf("Plan.Nodes len = %d, want 2", len(res.Plan.Nodes))
	}

	rec.requireType(t, event.RunPlanned)
	planned := rec.first(event.RunPlanned)
	if planned.RunID != "plan-test-003" {
		t.Errorf("RunPlanned.RunID = %q", planned.RunID)
	}
	var payload struct {
		NodeCount int `json:"node_count"`
		RootCount int `json:"root_count"`
	}
	if err := json.Unmarshal(planned.Payload, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.NodeCount != 2 || payload.RootCount != 1 {
		t.Errorf("payload = %+v, want NodeCount=2 RootCount=1", payload)
	}
}

// TestProcess_PlannerErrorDegrades confirms that a planner error
// degrades gracefully: no event fired, empty plan returned, the rest
// of the pipeline (classification, enrichment) still completes.
func TestProcess_PlannerErrorDegrades(t *testing.T) {
	t.Parallel()

	br := openTestBrain(t)
	bus := event.NewLocalBus()
	t.Cleanup(func() { _ = bus.Close() })

	rec := newRecorder()
	bus.Subscribe(event.RunPlanned, rec.handler())
	bus.Subscribe(event.RunClassified, rec.handler())

	fp := &fakePlanner{wantErr: errors.New("api unreachable")}
	k := New(br, WithBus(bus), WithPlanner(fp))

	res := k.Process(context.Background(), ProcessRequest{
		Prompt: "redesign distributed cache layer",
		CWD:    t.TempDir(),
		RunID:  "plan-test-004",
	})

	if fp.callCount() != 1 {
		t.Errorf("planner not invoked")
	}
	// The error path must NOT emit RunPlanned.
	if rec.has(event.RunPlanned) {
		t.Errorf("RunPlanned fired despite planner error")
	}
	// But the rest of the pipeline must still produce a classification
	// event — proves the kernel didn't bail early.
	rec.requireType(t, event.RunClassified)
	if len(res.Plan.Nodes) != 0 {
		t.Errorf("Plan.Nodes = %v, want empty on error", res.Plan.Nodes)
	}
	if res.Classification.Type == "" {
		t.Errorf("Classification missing — pipeline aborted on planner error")
	}
}

// TestForcePlan_BypassesComplexityGate confirms ForcePlan invokes the
// planner regardless of the complexity gate that Process applies.
func TestForcePlan_BypassesComplexityGate(t *testing.T) {
	t.Parallel()
	br := openTestBrain(t)
	fp := &fakePlanner{wantDAG: planner.TaskDAG{Nodes: []planner.Node{
		{ID: "root", Prompt: "trivial"},
	}}}
	k := New(br, WithPlanner(fp))

	plan, err := k.ForcePlan(context.Background(), "trivial prompt low complexity")
	if err != nil {
		t.Fatalf("ForcePlan: %v", err)
	}
	if fp.callCount() != 1 {
		t.Errorf("planner not invoked")
	}
	if len(plan.Nodes) != 1 || plan.Nodes[0].ID != "root" {
		t.Errorf("unexpected plan: %+v", plan)
	}
}

// TestForcePlan_NoPlannerReturnsErrNoPlanner confirms the sentinel
// error fires when the kernel has no planner configured.
func TestForcePlan_NoPlannerReturnsErrNoPlanner(t *testing.T) {
	t.Parallel()
	br := openTestBrain(t)
	k := New(br) // no WithPlanner
	_, err := k.ForcePlan(context.Background(), "anything")
	if !errors.Is(err, ErrNoPlanner) {
		t.Errorf("expected ErrNoPlanner, got %v", err)
	}
}

// TestForcePlan_PropagatesPlannerError confirms ForcePlan wraps
// planner errors through the standard errors.Is chain.
func TestForcePlan_PropagatesPlannerError(t *testing.T) {
	t.Parallel()
	br := openTestBrain(t)
	wantErr := errors.New("api unreachable")
	fp := &fakePlanner{wantErr: wantErr}
	k := New(br, WithPlanner(fp))

	_, err := k.ForcePlan(context.Background(), "anything")
	if !errors.Is(err, wantErr) {
		t.Errorf("expected wrapped wantErr, got %v", err)
	}
}
