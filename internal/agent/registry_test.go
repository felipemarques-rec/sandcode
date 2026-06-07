package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// stubProvider is a minimal Provider for use in nil/identity checks where
// the real providers would also work but a stub is clearer.
type stubProvider struct{ name string }

func (s *stubProvider) Name() string                         { return s.name }
func (s *stubProvider) BuildCommand(RunOptions) Command      { return Command{} }
func (s *stubProvider) ParseLine(string) (StreamEvent, bool) { return StreamEvent{}, false }
func (s *stubProvider) AuthHints() AuthHints                 { return AuthHints{} }

// Test 1: Register then Resolve returns that provider; unregistered role → ErrRoleNotFound.
func TestRegistry_RegisterResolve(t *testing.T) {
	r := NewRegistry()
	p := NewClaudeCode()

	if err := r.Register(RoleImplementer, p); err != nil {
		t.Fatalf("Register: unexpected error: %v", err)
	}

	got, err := r.Resolve(context.Background(), RoleImplementer)
	if err != nil {
		t.Fatalf("Resolve: unexpected error: %v", err)
	}
	if got != p {
		t.Errorf("Resolve returned wrong provider: got %v want %v", got, p)
	}

	// Unregistered role.
	_, err = r.Resolve(context.Background(), RolePlanner)
	if !errors.Is(err, ErrRoleNotFound) {
		t.Errorf("Resolve unregistered role: got %v, want ErrRoleNotFound", err)
	}
}

// Test 2: First-registered semantics — Resolve always returns the first provider (no rotation).
// Uses stubProvider with distinct Name() values so that first-registered semantics are asserted
// by a stable observable property rather than only pointer identity.
func TestRegistry_FirstRegisteredSemantics(t *testing.T) {
	r := NewRegistry()
	a := &stubProvider{name: "a"}
	b := &stubProvider{name: "b"}

	r.Register(RoleImplementer, a)
	r.Register(RoleImplementer, b)

	// Call Resolve multiple times; must always return the provider whose Name()=="a" (first registered).
	for i := 0; i < 5; i++ {
		got, err := r.Resolve(context.Background(), RoleImplementer)
		if err != nil {
			t.Fatalf("iteration %d: Resolve error: %v", i, err)
		}
		if got.Name() != "a" {
			t.Errorf("iteration %d: Resolve returned provider with Name()=%q, want \"a\" (first registered)", i, got.Name())
		}
	}
}

// Test 3: ErrNilProvider for nil provider and for empty Role.
func TestRegistry_ErrNilProvider(t *testing.T) {
	r := NewRegistry()

	err := r.Register(RoleImplementer, nil)
	if !errors.Is(err, ErrNilProvider) {
		t.Errorf("Register nil provider: got %v, want ErrNilProvider", err)
	}

	err = r.Register("", NewClaudeCode())
	if !errors.Is(err, ErrNilProvider) {
		t.Errorf("Register empty role: got %v, want ErrNilProvider", err)
	}
}

// Test 4: List returns all registered providers, including duplicates.
func TestRegistry_List(t *testing.T) {
	r := NewRegistry()
	cc := NewClaudeCode()
	cx := NewCodex()
	cu := NewCursor()

	r.Register(RoleImplementer, cc)
	r.Register(RoleImplementer, cx) // duplicate role, second provider
	r.Register(RoleReviewer, cu)
	r.Register(RoleImplementer, cc) // same provider registered twice on same role

	m := r.List()

	impl, ok := m[RoleImplementer]
	if !ok {
		t.Fatal("List: missing RoleImplementer")
	}
	if len(impl) != 3 {
		t.Errorf("RoleImplementer: got %d providers, want 3", len(impl))
	}
	if impl[0] != cc {
		t.Errorf("RoleImplementer[0]: got %v, want cc", impl[0])
	}
	if impl[1] != cx {
		t.Errorf("RoleImplementer[1]: got %v, want cx", impl[1])
	}
	if impl[2] != cc {
		t.Errorf("RoleImplementer[2]: got %v, want cc (duplicate)", impl[2])
	}

	rev, ok := m[RoleReviewer]
	if !ok {
		t.Fatal("List: missing RoleReviewer")
	}
	if len(rev) != 1 || rev[0] != cu {
		t.Errorf("RoleReviewer: unexpected contents %v", rev)
	}
}

// Test 5: List returns a defensive copy — mutations do not affect registry internals.
func TestRegistry_ListDefensiveCopy(t *testing.T) {
	r := NewRegistry()
	cc := NewClaudeCode()
	r.Register(RoleImplementer, cc)

	m1 := r.List()

	// Mutate the outer map: add a foreign key.
	m1[RolePlanner] = []Provider{NewCodex()}

	// Mutate a returned slice: append to it.
	m1[RoleImplementer] = append(m1[RoleImplementer], NewCursor())

	// Re-query to confirm internals are unchanged.
	m2 := r.List()
	if _, found := m2[RolePlanner]; found {
		t.Error("defensive copy failure: mutated outer map affected registry (RolePlanner present)")
	}
	if len(m2[RoleImplementer]) != 1 {
		t.Errorf("defensive copy failure: RoleImplementer slice length = %d, want 1", len(m2[RoleImplementer]))
	}

	// Also verify Resolve is unaffected.
	got, err := r.Resolve(context.Background(), RoleImplementer)
	if err != nil || got != cc {
		t.Errorf("Resolve after mutation: got (%v, %v), want (%v, nil)", got, err, cc)
	}
}

// Test 6: Race-clean concurrency — N goroutines doing Register + Resolve + List simultaneously.
func TestRegistry_ConcurrencyRace(t *testing.T) {
	r := NewRegistry()
	providers := []Provider{NewClaudeCode(), NewCodex(), NewCursor()}
	roles := []Role{RoleImplementer, RoleReviewer, RolePlanner}

	const goroutines = 30
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			role := roles[i%len(roles)]
			p := providers[i%len(providers)]

			// Register
			_ = r.Register(role, p)

			// Resolve (may or may not have a provider yet; either is fine).
			// If successful, the returned provider must be non-nil and carry a known name.
			if got, err := r.Resolve(context.Background(), role); err == nil {
				if got == nil {
					t.Errorf("goroutine %d: Resolve returned nil provider with nil error", i)
				} else {
					name := got.Name()
					if name != "claude-code" && name != "codex" && name != "cursor" {
						t.Errorf("goroutine %d: Resolve returned unexpected provider name %q", i, name)
					}
				}
			}

			// List
			m := r.List()
			_ = m
		}()
	}

	wg.Wait()
}
