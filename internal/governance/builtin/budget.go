package builtin

import (
	"context"
	"fmt"

	"github.com/felipemarques-rec/sandcode/internal/governance"
)

// Budget denies further work when accumulated tokens or cost would
// exceed configured per-run limits. The orchestrator must populate
// Action.TokensUsed / Action.CostUSD from its budget.Guard before
// calling Engine.Evaluate.
//
// Either limit zero-or-negative disables that dimension — useful for
// staged rollouts (e.g. enforce cost ceilings before token ceilings).
type Budget struct {
	MaxTokens  int64
	MaxCostUSD float64
}

// Name returns the stable identifier used in audit rows.
func (b Budget) Name() string { return "budget" }

func (b Budget) Evaluate(_ context.Context, a governance.Action) (governance.Result, string, error) {
	if b.MaxTokens > 0 && a.TokensUsed > b.MaxTokens {
		return governance.Deny,
			fmt.Sprintf("tokens used %d > limit %d", a.TokensUsed, b.MaxTokens),
			nil
	}
	if b.MaxCostUSD > 0 && a.CostUSD > b.MaxCostUSD {
		return governance.Deny,
			fmt.Sprintf("cost $%.4f > limit $%.4f", a.CostUSD, b.MaxCostUSD),
			nil
	}
	return governance.Allow, "", nil
}
