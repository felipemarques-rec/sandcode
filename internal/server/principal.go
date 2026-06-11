package server

import (
	"context"

	"github.com/felipemarques-rec/sandcode/internal/rbac"
)

// principalCtxKey is the unexported context key under which the authenticated
// rbac.Principal is stashed by withAuth and read by requireCapability.
type principalCtxKey struct{}

// withPrincipal returns a copy of ctx carrying p.
func withPrincipal(ctx context.Context, p rbac.Principal) context.Context {
	return context.WithValue(ctx, principalCtxKey{}, p)
}

// principalFrom extracts the principal injected by withAuth. The bool is false
// when no principal is present (e.g. the legacy no-auth path).
func principalFrom(ctx context.Context) (rbac.Principal, bool) {
	p, ok := ctx.Value(principalCtxKey{}).(rbac.Principal)
	return p, ok
}
