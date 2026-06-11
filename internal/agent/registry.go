package agent

import (
	"context"
	"errors"
	"sync"
)

// Role is an open string type (extensible; validation lives at callers, not here).
type Role string

const (
	RolePlanner          Role = "planner"
	RoleArchitect        Role = "architect"
	RoleImplementer      Role = "implementer"
	RoleVerifier         Role = "verifier"
	RoleReviewer         Role = "reviewer"
	RoleSecurityReviewer Role = "security_reviewer"
	// RolePerformanceReviewer and RoleRefactoringSpecialist mirror the diagram's
	// Multi-Agent Collaboration roles. They are wired today as judge lenses via
	// RunOptions (PerformanceReviewer/RefactoringSpecialist); these constants give
	// them role identity for --role selection and forward-compat parity.
	RolePerformanceReviewer   Role = "performance_reviewer"
	RoleRefactoringSpecialist Role = "refactoring_specialist"
	RoleReporter              Role = "reporter"
)

// Registry binds named roles to Provider instances. Interface is verbatim
// from master plan §6.1 — do not change the signatures.
//
// Multiple Register calls for the same role accumulate: the role keeps all
// registered providers in registration order, and Resolve returns the first.
type Registry interface {
	Register(role Role, p Provider) error
	Resolve(ctx context.Context, role Role) (Provider, error)
	List() map[Role][]Provider
}

var (
	ErrRoleNotFound = errors.New("agent: role has no registered provider")
	ErrNilProvider  = errors.New("agent: nil provider or empty role")
)

// NewRegistry returns a new, empty Registry implementation.
func NewRegistry() Registry {
	return &registry{
		providers: make(map[Role][]Provider),
	}
}

type registry struct {
	mu        sync.RWMutex
	providers map[Role][]Provider
}

// Register appends p to the slice for role. Returns ErrNilProvider if p is nil
// or role is empty. Duplicate registrations are allowed.
func (r *registry) Register(role Role, p Provider) error {
	if p == nil || role == "" {
		return ErrNilProvider
	}
	r.mu.Lock()
	r.providers[role] = append(r.providers[role], p)
	r.mu.Unlock()
	return nil
}

// Resolve returns the first registered provider for the role.
// Returns ErrRoleNotFound if the role has no providers.
// ctx is accepted for interface/forward-compat and intentionally unused for in-memory resolution.
func (r *registry) Resolve(ctx context.Context, role Role) (Provider, error) {
	r.mu.RLock()
	ps := r.providers[role]
	var first Provider
	if len(ps) > 0 {
		first = ps[0]
	}
	r.mu.RUnlock()
	if first == nil {
		return nil, ErrRoleNotFound
	}
	return first, nil
}

// List returns a defensive copy of the full registry map (new outer map +
// newly-allocated copied slices) so callers cannot mutate registry internals.
func (r *registry) List() map[Role][]Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[Role][]Provider, len(r.providers))
	for role, ps := range r.providers {
		cp := make([]Provider, len(ps))
		copy(cp, ps)
		out[role] = cp
	}
	return out
}
