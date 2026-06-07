package governance

import (
	"context"
	"errors"
	"testing"
)

// stubPolicy is a test fixture that returns a pre-configured verdict.
type stubPolicy struct {
	name   string
	result Result
	reason string
	err    error
}

func (s stubPolicy) Name() string { return s.name }
func (s stubPolicy) Evaluate(_ context.Context, _ Action) (Result, string, error) {
	return s.result, s.reason, s.err
}

func TestEngine_EmptyEngineAllowsByDefault(t *testing.T) {
	t.Parallel()
	d := NewEngine().Evaluate(context.Background(), Action{Type: ActionExecute, RunID: "r"})
	if d.Result != Allow {
		t.Fatalf("empty engine result = %s, want allow", d.Result)
	}
	if len(d.Reasons) != 0 || len(d.PerPolicy) != 0 {
		t.Fatalf("empty engine produced reasons: %+v", d)
	}
}

func TestEngine_AllAllowProducesAllow(t *testing.T) {
	t.Parallel()
	e := NewEngine(
		stubPolicy{name: "p1", result: Allow},
		stubPolicy{name: "p2", result: Allow, reason: "noop"},
	)
	d := e.Evaluate(context.Background(), Action{Type: ActionExecute, RunID: "r"})
	if d.Result != Allow {
		t.Fatalf("got %s, want allow", d.Result)
	}
	if len(d.PerPolicy) != 2 {
		t.Fatalf("PerPolicy len = %d, want 2", len(d.PerPolicy))
	}
}

func TestEngine_DenyShortCircuits(t *testing.T) {
	t.Parallel()
	called := false
	e := NewEngine(
		stubPolicy{name: "first", result: Deny, reason: "boom"},
		stubPolicy{name: "second", result: Allow}, // must NOT run
	)
	// Inject a probe that mutates a captured flag — fail if reached.
	e.policies = append(e.policies, &probePolicy{name: "probe", called: &called})

	d := e.Evaluate(context.Background(), Action{Type: ActionExecute, RunID: "r"})
	if d.Result != Deny {
		t.Fatalf("got %s, want deny", d.Result)
	}
	if called {
		t.Fatalf("Deny did not short-circuit: probe policy ran")
	}
	if len(d.PerPolicy) != 1 {
		t.Fatalf("PerPolicy after deny = %d, want 1 (only the denying policy)", len(d.PerPolicy))
	}
	if d.PerPolicy[0].Policy != "first" || d.PerPolicy[0].Result != Deny {
		t.Fatalf("PerPolicy[0] = %+v", d.PerPolicy[0])
	}
}

func TestEngine_ReviewAccumulates(t *testing.T) {
	t.Parallel()
	e := NewEngine(
		stubPolicy{name: "p1", result: Review, reason: "big diff"},
		stubPolicy{name: "p2", result: Allow},
		stubPolicy{name: "p3", result: Review, reason: "critical path"},
	)
	d := e.Evaluate(context.Background(), Action{Type: ActionExecute, RunID: "r"})
	if d.Result != Review {
		t.Fatalf("got %s, want review", d.Result)
	}
	if len(d.Reasons) != 2 {
		t.Fatalf("Reasons len = %d, want 2 (two review verdicts)", len(d.Reasons))
	}
	if len(d.PerPolicy) != 3 {
		t.Fatalf("PerPolicy len = %d, want 3 (all evaluated)", len(d.PerPolicy))
	}
}

func TestEngine_DenyAfterReviewStillShortCircuits(t *testing.T) {
	t.Parallel()
	e := NewEngine(
		stubPolicy{name: "p1", result: Review, reason: "diff size"},
		stubPolicy{name: "p2", result: Deny, reason: "budget"},
		stubPolicy{name: "p3", result: Allow},
	)
	d := e.Evaluate(context.Background(), Action{Type: ActionExecute, RunID: "r"})
	if d.Result != Deny {
		t.Fatalf("got %s, want deny (deny overrides accumulated reviews)", d.Result)
	}
	if len(d.PerPolicy) != 2 {
		t.Fatalf("PerPolicy len = %d, want 2 (p3 should be skipped)", len(d.PerPolicy))
	}
}

func TestEngine_PolicyErrorBecomesDeny(t *testing.T) {
	t.Parallel()
	e := NewEngine(
		stubPolicy{name: "broken", err: errors.New("db unreachable")},
		stubPolicy{name: "next", result: Allow}, // must NOT run
	)
	d := e.Evaluate(context.Background(), Action{Type: ActionExecute, RunID: "r"})
	if d.Result != Deny {
		t.Fatalf("got %s, want deny on policy error", d.Result)
	}
	if len(d.PerPolicy) != 1 {
		t.Fatalf("PerPolicy len = %d, want 1 (error short-circuits)", len(d.PerPolicy))
	}
}

func TestEngine_AddPolicy_RejectsDuplicateName(t *testing.T) {
	t.Parallel()
	e := NewEngine(stubPolicy{name: "x"})
	if err := e.AddPolicy(stubPolicy{name: "y"}); err != nil {
		t.Fatalf("AddPolicy y: %v", err)
	}
	if err := e.AddPolicy(stubPolicy{name: "x"}); err == nil {
		t.Fatalf("expected duplicate-name error")
	}
}

func TestEngine_PoliciesReturnsCopy(t *testing.T) {
	t.Parallel()
	e := NewEngine(stubPolicy{name: "a"}, stubPolicy{name: "b"})
	ps := e.Policies()
	if len(ps) != 2 {
		t.Fatalf("Policies len = %d, want 2", len(ps))
	}
	// Mutate the copy; engine internals must be unaffected.
	ps[0] = nil
	if e.Policies()[0] == nil {
		t.Fatalf("Policies() returned a live slice, not a copy")
	}
}

// probePolicy records whether Evaluate was called. Used in
// TestEngine_DenyShortCircuits to prove short-circuit behavior.
type probePolicy struct {
	name   string
	called *bool
}

func (p *probePolicy) Name() string { return p.name }
func (p *probePolicy) Evaluate(_ context.Context, _ Action) (Result, string, error) {
	*p.called = true
	return Allow, "", nil
}
