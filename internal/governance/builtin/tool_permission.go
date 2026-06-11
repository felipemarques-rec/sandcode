package builtin

import (
	"context"
	"fmt"

	"github.com/felipemarques-rec/sandcode/internal/governance"
)

// ToolPermission denies an action that would grant or invoke an MCP tool the
// principal's security roles are not authorized for. It is the governance leaf
// of E2.2 RBAC: identity enters ONLY through the audited Action.Roles field, so
// the decision stays a pure, replayable function of its input — no ambient
// context is consulted.
//
// The policy is a no-op (Allow) unless BOTH Action.Tool and Action.Roles are
// populated. An empty Tool means the action grants no tool; empty Roles means
// no principal (CLI/legacy) ⇒ no restriction.
type ToolPermission struct {
	// Permits is injected as a pure func so this policy imports neither rbac nor
	// server — governance stays the lower leaf.
	Permits func(roles []string, tool string) bool
}

// Name returns the stable identifier used in audit rows.
func (p ToolPermission) Name() string { return "tool_permission" }

func (p ToolPermission) Evaluate(_ context.Context, a governance.Action) (governance.Result, string, error) {
	if a.Tool == "" || len(a.Roles) == 0 {
		return governance.Allow, "", nil // byte-identical no-op
	}
	if p.Permits(a.Roles, a.Tool) {
		return governance.Allow, "", nil
	}
	return governance.Deny, fmt.Sprintf("roles %v not permitted tool %q", a.Roles, a.Tool), nil
}
