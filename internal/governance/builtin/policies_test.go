package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/governance"
)

func TestRetryLimit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		policy RetryLimit
		action governance.Action
		want   governance.Result
	}{
		{"disabled zero allows", RetryLimit{}, governance.Action{Type: governance.ActionRefine, Attempt: 99}, governance.Allow},
		{"disabled negative allows", RetryLimit{MaxAttempts: -1}, governance.Action{Type: governance.ActionRefine, Attempt: 99}, governance.Allow},
		{"under cap allows", RetryLimit{MaxAttempts: 3}, governance.Action{Type: governance.ActionRefine, Attempt: 2}, governance.Allow},
		{"at cap allows", RetryLimit{MaxAttempts: 3}, governance.Action{Type: governance.ActionRefine, Attempt: 3}, governance.Allow},
		{"over cap denies", RetryLimit{MaxAttempts: 3}, governance.Action{Type: governance.ActionRefine, Attempt: 4}, governance.Deny},
		{"non-refine ignored", RetryLimit{MaxAttempts: 1}, governance.Action{Type: governance.ActionExecute, Attempt: 99}, governance.Allow},
		{"merge ignored", RetryLimit{MaxAttempts: 1}, governance.Action{Type: governance.ActionMerge, Attempt: 99}, governance.Allow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := tc.policy.Evaluate(context.Background(), tc.action)
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got=%s want=%s", got, tc.want)
			}
		})
	}
}

func TestRetryLimit_ReasonMessageMentionsAttemptAndMax(t *testing.T) {
	t.Parallel()
	r := RetryLimit{MaxAttempts: 2}
	_, reason, _ := r.Evaluate(context.Background(),
		governance.Action{Type: governance.ActionRefine, Attempt: 5})
	if !strings.Contains(reason, "attempt=5") || !strings.Contains(reason, "max=2") {
		t.Fatalf("reason missing context: %q", reason)
	}
}

func TestDiffSize(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		policy DiffSize
		action governance.Action
		want   governance.Result
	}{
		{"disabled", DiffSize{}, governance.Action{Type: governance.ActionMerge, DiffSize: 1000}, governance.Allow},
		{"under threshold allow", DiffSize{ReviewAboveBytes: 500}, governance.Action{Type: governance.ActionMerge, DiffSize: 400}, governance.Allow},
		{"at threshold allow", DiffSize{ReviewAboveBytes: 500}, governance.Action{Type: governance.ActionMerge, DiffSize: 500}, governance.Allow},
		{"over threshold review", DiffSize{ReviewAboveBytes: 500}, governance.Action{Type: governance.ActionMerge, DiffSize: 501}, governance.Review},
		{"refine over threshold review", DiffSize{ReviewAboveBytes: 500}, governance.Action{Type: governance.ActionRefine, DiffSize: 1000}, governance.Review},
		{"execute ignored even over", DiffSize{ReviewAboveBytes: 100}, governance.Action{Type: governance.ActionExecute, DiffSize: 9999}, governance.Allow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := tc.policy.Evaluate(context.Background(), tc.action)
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got=%s want=%s", got, tc.want)
			}
		})
	}
}

func TestBudget(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		policy Budget
		action governance.Action
		want   governance.Result
	}{
		{"both disabled", Budget{}, governance.Action{TokensUsed: 1e9, CostUSD: 999}, governance.Allow},
		{"tokens disabled, under cost", Budget{MaxCostUSD: 5.0}, governance.Action{CostUSD: 4.99}, governance.Allow},
		{"tokens disabled, over cost", Budget{MaxCostUSD: 5.0}, governance.Action{CostUSD: 5.01}, governance.Deny},
		{"cost disabled, under tokens", Budget{MaxTokens: 1000}, governance.Action{TokensUsed: 999}, governance.Allow},
		{"cost disabled, over tokens", Budget{MaxTokens: 1000}, governance.Action{TokensUsed: 1001}, governance.Deny},
		{"both set, both under", Budget{MaxTokens: 1000, MaxCostUSD: 5.0}, governance.Action{TokensUsed: 100, CostUSD: 1.0}, governance.Allow},
		{"tokens first", Budget{MaxTokens: 1000, MaxCostUSD: 5.0}, governance.Action{TokensUsed: 2000, CostUSD: 1.0}, governance.Deny},
		{"cost when tokens ok", Budget{MaxTokens: 1000, MaxCostUSD: 5.0}, governance.Action{TokensUsed: 500, CostUSD: 6.0}, governance.Deny},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := tc.policy.Evaluate(context.Background(), tc.action)
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got=%s want=%s", got, tc.want)
			}
		})
	}
}

func TestBudget_ReasonMentionsDimension(t *testing.T) {
	t.Parallel()
	b := Budget{MaxTokens: 100, MaxCostUSD: 1.0}

	// Tokens over → reason about tokens.
	_, tokReason, _ := b.Evaluate(context.Background(),
		governance.Action{TokensUsed: 1000})
	if !strings.Contains(tokReason, "tokens") {
		t.Fatalf("tokens reason missing 'tokens': %q", tokReason)
	}

	// Cost over → reason about cost.
	_, costReason, _ := b.Evaluate(context.Background(),
		governance.Action{CostUSD: 2.0})
	if !strings.Contains(strings.ToLower(costReason), "cost") {
		t.Fatalf("cost reason missing 'cost': %q", costReason)
	}
}
