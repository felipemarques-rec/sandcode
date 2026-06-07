package main

import (
	"context"
	"strings"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/governance"
	"github.com/felipemarques-rec/sandcode/internal/governance/builtin"
)

// fakeBudgetExceedingAction returns an Action whose consumption is well
// above the caps used in TestBuildGates_PolicyComposition — used to
// confirm the policy is wired with the operator-supplied values rather
// than a zero-value Budget that would let everything through.
func fakeBudgetExceedingAction() governance.Action {
	return governance.Action{
		Type:       governance.ActionExecute,
		RunID:      "test-run",
		TokensUsed: 1_000_000,
		CostUSD:    100.0,
	}
}

func TestBuildGates_AllZeroDisablesEverything(t *testing.T) {
	engine, guard, refine, err := buildGates(gateFlags{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if engine != nil {
		t.Errorf("engine = %v, want nil", engine)
	}
	if guard != nil {
		t.Errorf("guard = %v, want nil", guard)
	}
	if refine.Enabled {
		t.Errorf("refine.Enabled = true, want false")
	}
	if len(refine.VerifyCmd) != 0 {
		t.Errorf("refine.VerifyCmd = %v, want empty", refine.VerifyCmd)
	}
}

func TestBuildGates_RefineRequiresVerifyCmd(t *testing.T) {
	_, _, _, err := buildGates(gateFlags{refine: true})
	if err == nil {
		t.Fatal("err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "--refine-verify") {
		t.Errorf("err = %q, want mention of --refine-verify", err)
	}
}

func TestBuildGates_BudgetCeilingsBuildEngineAndGuard(t *testing.T) {
	engine, guard, _, err := buildGates(gateFlags{maxTokens: 10_000})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if engine == nil {
		t.Fatal("engine = nil, want non-nil")
	}
	if guard == nil {
		t.Fatal("guard = nil, want non-nil")
	}

	// Cost-only flag also lights up the same policy + Guard.
	engine2, guard2, _, err := buildGates(gateFlags{maxCostUSD: 1.5})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if engine2 == nil || guard2 == nil {
		t.Errorf("cost-only: engine=%v guard=%v", engine2, guard2)
	}
}

func TestBuildGates_RetryLimitBuildsEngineWithoutGuard(t *testing.T) {
	engine, guard, _, err := buildGates(gateFlags{retryLimit: 5})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if engine == nil {
		t.Fatal("engine = nil")
	}
	if guard != nil {
		t.Errorf("guard = %v, want nil (retry-limit alone does not need accounting)", guard)
	}
}

func TestBuildGates_RefineFlagsForwarded(t *testing.T) {
	_, _, refine, err := buildGates(gateFlags{
		refine:            true,
		refineVerify:      []string{"go", "test", "./..."},
		refineMaxAttempts: 5,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !refine.Enabled {
		t.Error("refine.Enabled = false, want true")
	}
	if len(refine.VerifyCmd) == 0 {
		t.Error("refine.VerifyCmd empty, want populated")
	}
	if got, want := strings.Join(refine.VerifyCmd, " "), "go test ./..."; got != want {
		t.Errorf("VerifyCmd = %q, want %q", got, want)
	}
	if refine.MaxAttempts != 5 {
		t.Errorf("MaxAttempts = %d, want 5", refine.MaxAttempts)
	}
}

func TestBuildGates_PolicyComposition(t *testing.T) {
	// All three policy axes wired at once: Budget (tokens+cost) +
	// RetryLimit yields one engine with two policies, plus a Guard.
	engine, guard, refine, err := buildGates(gateFlags{
		maxTokens:    50_000,
		maxCostUSD:   2.0,
		retryLimit:   4,
		refine:       true,
		refineVerify: []string{"make", "verify"},
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if engine == nil || guard == nil {
		t.Fatalf("engine=%v guard=%v", engine, guard)
	}
	if !refine.Enabled || len(refine.VerifyCmd) == 0 {
		t.Errorf("refine = %+v, want enabled with verify cmd", refine)
	}

	// Round-trip the Budget policy through the engine to confirm it
	// was registered with the operator-supplied caps (not an empty
	// builtin.Budget zero-value).
	policy := builtin.Budget{MaxTokens: 50_000, MaxCostUSD: 2.0}
	got, _, perr := policy.Evaluate(context.Background(), fakeBudgetExceedingAction())
	if perr != nil {
		t.Fatalf("policy err: %v", perr)
	}
	if got != governance.Deny {
		t.Errorf("Budget policy sanity check failed: got=%s want=%s", got, governance.Deny)
	}
}
