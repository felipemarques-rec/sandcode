package orchestrator

// Tests for the opt-in Role-based Implementer resolution wired into Run and
// forwarded by Execute's Single/Refine path (SP1 — Agent Role Registry phase 1;
// see docs/superpowers/specs/2026-05-17-agent-role-registry-sp1-design.md).
//
// Four scenarios:
//   1. Registry resolves Implementer → run uses resolved provider, not positional ag.
//   2. opts.Registry == nil → positional ag used unchanged (legacy byte-identical path).
//   3. Registry present but RoleImplementer not registered (ErrRoleNotFound) → fallback to positional ag.
//   4. Behavioral gate — Execute Single path consults the registry exactly once; the
//      Parallel/DAG structural invariant (no Registry field on ParallelOptions/DAGOptions)
//      is documented in the test and guaranteed at compile time.
//   5. End-to-end Execute → buildRunOptionsFromExecute → Run → registry resolve.

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/agent"
	gitm "github.com/felipemarques-rec/sandcode/internal/git"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
)

// namedAgent is a fakeAgent that emits a distinctive prefix line so tests can
// assert which provider actually executed, by inspecting streamed events.
// It also writes a marker file relative to its working directory.
type namedAgent struct {
	id string // "A" or "B" — appears in marker filename and emitted text
}

func (n *namedAgent) Name() string { return "named-" + n.id }
func (n *namedAgent) BuildCommand(opts agent.RunOptions) agent.Command {
	// Write a marker file; relative path so it lands in the sandbox WorkDir.
	return agent.Command{Argv: []string{"sh", "-c", "touch ran-by-" + n.id + " && echo AGENT=" + n.id}}
}
func (n *namedAgent) ParseLine(line string) (agent.StreamEvent, bool) {
	if line == "" {
		return agent.StreamEvent{}, false
	}
	return agent.StreamEvent{Kind: agent.EventText, Text: line}, true
}
func (*namedAgent) AuthHints() agent.AuthHints { return agent.AuthHints{} }

// runWithRegistry calls Run, drains events (collecting text lines), and returns
// (result, collectedLines). Uses nosandbox + KeepWorktree so the caller can
// inspect wt.Path.
func runWithRegistry(
	t *testing.T,
	positional agent.Provider,
	reg agent.Registry,
) (Result, []string) {
	t.Helper()
	repo := initRepo(t)

	events, await, err := Run(
		context.Background(),
		sandbox.NewNoSandboxProvider(),
		positional,
		&noopAuth{},
		RunOptions{
			Prompt:         "noop",
			CWD:            repo,
			SandboxImage:   "ignored-by-nosandbox",
			SandboxWorkDir: "/workspace", // nosandbox maps this → wt.Path via mount
			Strategy:       gitm.StrategyBranch,
			KeepWorktree:   true,
			Registry:       reg,
		},
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var lines []string
	for ev := range events {
		if ev.Text != "" {
			lines = append(lines, ev.Text)
		}
	}
	res := await()
	return res, lines
}

// TestRun_Registry_ResolvesImplementer verifies that when opts.Registry is
// set and the Implementer role is registered with agentB, the run uses agentB
// (observable via the AGENT=B line it emits), NOT the positional agentA.
func TestRun_Registry_ResolvesImplementer(t *testing.T) {
	t.Parallel()

	agA := &namedAgent{id: "A"}
	agB := &namedAgent{id: "B"}

	reg := agent.NewRegistry()
	if err := reg.Register(agent.RoleImplementer, agB); err != nil {
		t.Fatalf("Register: %v", err)
	}

	res, lines := runWithRegistry(t, agA, reg)
	if res.Status != "success" {
		t.Fatalf("run status=%s err=%v", res.Status, res.Err)
	}

	// agB emits "AGENT=B"; agA would emit "AGENT=A".
	if !containsLine(lines, "AGENT=B") {
		t.Errorf("expected AGENT=B in events (agB did NOT run); lines=%v", lines)
	}
	if containsLine(lines, "AGENT=A") {
		t.Errorf("unexpected AGENT=A in events (agA ran instead of agB)")
	}

	// Also verify via the marker file in the worktree.
	if res.Worktree == nil {
		t.Fatal("Worktree is nil (KeepWorktree=true)")
	}
	if _, err := os.Stat(filepath.Join(res.Worktree.Path, "ran-by-B")); err != nil {
		t.Errorf("ran-by-B not found in worktree %s: %v", res.Worktree.Path, err)
	}
	if _, err := os.Stat(filepath.Join(res.Worktree.Path, "ran-by-A")); err == nil {
		t.Errorf("ran-by-A found (agA ran instead of agB — registry not consulted)")
	}
}

// TestRun_Registry_Nil_LegacyPath verifies that when opts.Registry is nil,
// the positional agent (agA) is used unchanged — byte-identical legacy path.
func TestRun_Registry_Nil_LegacyPath(t *testing.T) {
	t.Parallel()

	agA := &namedAgent{id: "A"}

	res, lines := runWithRegistry(t, agA, nil) // explicit nil registry
	if res.Status != "success" {
		t.Fatalf("run status=%s err=%v", res.Status, res.Err)
	}

	// agA emits "AGENT=A".
	if !containsLine(lines, "AGENT=A") {
		t.Errorf("expected AGENT=A in events (agA should run); lines=%v", lines)
	}

	// Verify via marker file.
	if res.Worktree == nil {
		t.Fatal("Worktree is nil")
	}
	if _, err := os.Stat(filepath.Join(res.Worktree.Path, "ran-by-A")); err != nil {
		t.Errorf("ran-by-A not found: agA did NOT run: %v", err)
	}
}

// TestRun_Registry_ErrRoleNotFound_FallsBackToPositional verifies that when
// a registry is supplied but RoleImplementer is NOT registered, the run falls
// back to the positional agent unchanged.
func TestRun_Registry_ErrRoleNotFound_FallsBackToPositional(t *testing.T) {
	t.Parallel()

	agA := &namedAgent{id: "A"}

	// Registry is non-nil but RoleImplementer is NOT registered.
	reg := agent.NewRegistry()
	// Register a different role — Implementer absent → Resolve returns ErrRoleNotFound.
	_ = reg.Register(agent.RolePlanner, agA)

	res, lines := runWithRegistry(t, agA, reg)
	if res.Status != "success" {
		t.Fatalf("run status=%s err=%v", res.Status, res.Err)
	}

	// agA should run as the positional fallback.
	if !containsLine(lines, "AGENT=A") {
		t.Errorf("expected AGENT=A (positional fallback after ErrRoleNotFound); lines=%v", lines)
	}

	if res.Worktree == nil {
		t.Fatal("Worktree is nil")
	}
	if _, err := os.Stat(filepath.Join(res.Worktree.Path, "ran-by-A")); err != nil {
		t.Errorf("ran-by-A not found: positional fallback did NOT run: %v", err)
	}
}

// countingRegistry wraps agent.NewRegistry() and records how many times
// Resolve has been called. Used in the regression gate test.
type countingRegistry struct {
	inner        agent.Registry
	resolveCalls int64
}

func newCountingRegistry() *countingRegistry {
	return &countingRegistry{inner: agent.NewRegistry()}
}
func (c *countingRegistry) Register(role agent.Role, p agent.Provider) error {
	return c.inner.Register(role, p)
}
func (c *countingRegistry) Resolve(ctx context.Context, role agent.Role) (agent.Provider, error) {
	atomic.AddInt64(&c.resolveCalls, 1)
	return c.inner.Resolve(ctx, role)
}
func (c *countingRegistry) List() map[agent.Role][]agent.Provider {
	return c.inner.List()
}
func (c *countingRegistry) calls() int64 {
	return atomic.LoadInt64(&c.resolveCalls)
}

// TestExecute_Registry_SinglePathConsultsOnce is the behavioral regression gate
// for SP1 registry integration.
//
// What this test proves:
//
//  1. Execute with no Kernel dispatches Single (runDirect → Run). The registry
//     is consulted exactly once by Run's Implementer resolution block.
//
//  2. The positional Agent (agA) is NOT used when a distinct provider (agB) is
//     registered as Implementer. The run is observably driven by agB — the
//     "AGENT=B" stream line and the "ran-by-B" marker file both appear, and
//     the "AGENT=A" / "ran-by-A" equivalents do NOT.
//
// Compile-time / structural invariant (Parallel/DAG paths never consult Registry):
// The definitive guarantee that forwardToParallel and forwardToDAG can never
// consult opts.Registry is structural and enforced at compile time: ParallelOptions
// and DAGOptions have no Registry field, and forwardToParallel / forwardToDAG
// copy ExecuteOptions fields into those structs explicitly — opts.Registry is
// simply not referenced and cannot be forwarded even by accident. This invariant
// is documented in the SP1 design spec:
// docs/superpowers/specs/2026-05-17-agent-role-registry-sp1-design.md.
// A runtime parallel-dispatch test with countingRegistry would add evidence
// but is deliberately omitted here: wiring fakeSelector + kernel + event.Bus
// imports into this file would pull in the full kernel/planner/strategy/event
// import set, making this focused test file significantly heavier. The
// structural guarantee is sufficient per the SP1 spec review.
func TestExecute_Registry_SinglePathConsultsOnce(t *testing.T) {
	t.Parallel()

	repo := initRepo(t)

	agA := &namedAgent{id: "A"}
	agB := &namedAgent{id: "B"}

	// Register agB (NOT agA) as Implementer so we can distinguish "registry
	// consulted and used B" from "registry skipped and positional A used".
	reg := newCountingRegistry()
	if err := reg.Register(agent.RoleImplementer, agB); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Execute with no Kernel → runDirect → Single path.
	// Positional Agent is agA; registry resolves agB as Implementer.
	events, await, err := Execute(
		context.Background(),
		sandbox.NewNoSandboxProvider(),
		&noopAuth{},
		ExecuteOptions{
			Prompt:         "noop",
			CWD:            repo,
			SandboxImage:   "ignored-by-nosandbox",
			SandboxWorkDir: "/workspace",
			Agent:          agA,
			Strategy:       gitm.StrategyBranch,
			KeepWorktree:   true,
			Registry:       reg,
			// No Kernel → runDirect → Single path → registry consulted once.
		},
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var lines []string
	for ev := range events {
		if ev.Text != "" {
			lines = append(lines, ev.Text)
		}
	}
	res := await()
	if res.Kind != DispatchSingle {
		t.Fatalf("Kind: got %v, want Single (no kernel)", res.Kind)
	}

	// (a) Registry consulted exactly once (by Run on the Single path).
	// > 1 would indicate forwardToParallel or another path is leaking the registry.
	if got := reg.calls(); got != 1 {
		t.Errorf("registry.Resolve called %d times; want exactly 1 (Single path only)", got)
	}

	// (b) agB ran — not agA. Proven via stream events AND marker files.
	if !containsLine(lines, "AGENT=B") {
		t.Errorf("expected AGENT=B in events (agB should run via registry); lines=%v", lines)
	}
	if containsLine(lines, "AGENT=A") {
		t.Errorf("unexpected AGENT=A in events (agA ran instead of agB — registry not consulted)")
	}

	if res.Run == nil {
		t.Fatal("Run result is nil")
	}
	if res.Run.Worktree == nil {
		t.Fatal("Worktree is nil (KeepWorktree=true)")
	}
	if _, err := os.Stat(filepath.Join(res.Run.Worktree.Path, "ran-by-B")); err != nil {
		t.Errorf("ran-by-B not found in worktree %s: %v", res.Run.Worktree.Path, err)
	}
	if _, err := os.Stat(filepath.Join(res.Run.Worktree.Path, "ran-by-A")); err == nil {
		t.Errorf("ran-by-A found (agA ran instead of agB — registry not consulted)")
	}
}

// TestExecute_Registry_ForwardedToSinglePath verifies the full Execute →
// buildRunOptionsFromExecute → Run → registry-resolve chain: positional agA,
// registry with agB as Implementer, Execute dispatches Single → agB runs.
func TestExecute_Registry_ForwardedToSinglePath(t *testing.T) {
	t.Parallel()

	repo := initRepo(t)

	agA := &namedAgent{id: "A"}
	agB := &namedAgent{id: "B"}

	reg := agent.NewRegistry()
	if err := reg.Register(agent.RoleImplementer, agB); err != nil {
		t.Fatalf("Register: %v", err)
	}

	events, await, err := Execute(
		context.Background(),
		sandbox.NewNoSandboxProvider(),
		&noopAuth{},
		ExecuteOptions{
			Prompt:         "noop",
			CWD:            repo,
			SandboxImage:   "ignored-by-nosandbox",
			SandboxWorkDir: "/workspace",
			Agent:          agA,
			Strategy:       gitm.StrategyBranch,
			KeepWorktree:   true,
			Registry:       reg,
		},
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var lines []string
	for ev := range events {
		if ev.Text != "" {
			lines = append(lines, ev.Text)
		}
	}
	res := await()
	if res.Kind != DispatchSingle {
		t.Fatalf("Kind: got %v, want Single", res.Kind)
	}
	if res.Run == nil || res.Run.Status != "success" {
		t.Fatalf("run failed: %+v", res.Run)
	}

	// agB emits "AGENT=B".
	if !containsLine(lines, "AGENT=B") {
		t.Errorf("expected AGENT=B (agB via registry); lines=%v", lines)
	}
	if containsLine(lines, "AGENT=A") {
		t.Errorf("unexpected AGENT=A (agA ran — registry not consulted by Execute→Run chain)")
	}

	// Marker file verification.
	if res.Run.Worktree == nil {
		t.Fatal("Worktree is nil")
	}
	if _, err := os.Stat(filepath.Join(res.Run.Worktree.Path, "ran-by-B")); err != nil {
		t.Errorf("ran-by-B not found: %v", err)
	}
	if _, err := os.Stat(filepath.Join(res.Run.Worktree.Path, "ran-by-A")); err == nil {
		t.Errorf("ran-by-A found (agA ran — registry not consulted)")
	}
}
