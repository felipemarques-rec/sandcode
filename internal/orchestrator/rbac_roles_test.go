package orchestrator

import (
	"context"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/event"
	gitm "github.com/felipemarques-rec/sandcode/internal/git"
	"github.com/felipemarques-rec/sandcode/internal/governance"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
)

// rolesRecordingPolicy captures the Action it is asked to evaluate at the
// ActionExecute gate so the test can assert the principal roles were threaded
// through. It always Allows so the run proceeds (byte-identical to no policy).
type rolesRecordingPolicy struct {
	mu    sync.Mutex
	roles []string
	seen  bool
}

func (p *rolesRecordingPolicy) Name() string { return "roles-recorder" }

func (p *rolesRecordingPolicy) Evaluate(_ context.Context, a governance.Action) (governance.Result, string, error) {
	if a.Type == governance.ActionExecute {
		p.mu.Lock()
		// copy to avoid aliasing the orchestrator's slice
		p.roles = append([]string(nil), a.Roles...)
		p.seen = true
		p.mu.Unlock()
	}
	return governance.Allow, "ok", nil
}

func (p *rolesRecordingPolicy) captured() ([]string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.roles, p.seen
}

func TestRBACRoles_ThreadedIntoExecuteAction(t *testing.T) {
	repo := initRepo(t)
	lb := event.NewLocalBus()
	t.Cleanup(func() { _ = lb.Close() })
	pol := &rolesRecordingPolicy{}
	eng := governance.NewEngine(pol)

	_, await, err := Run(context.Background(),
		sandbox.NewNoSandboxProvider(),
		&fakeAgent{script: `echo hi > out.txt`},
		&noopAuth{},
		RunOptions{
			Prompt:         "noop",
			CWD:            repo,
			SandboxImage:   "x",
			SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", t.Name(), "0"),
			Strategy:       gitm.StrategyMergeToHead,
			Bus:            lb,
			Governance:     eng,
			Roles:          []string{"operator"},
		})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	res := await()
	if res.Status != "success" {
		t.Fatalf("status = %s, want success", res.Status)
	}
	roles, seen := pol.captured()
	if !seen {
		t.Fatal("policy never saw the ActionExecute gate")
	}
	if !slices.Equal(roles, []string{"operator"}) {
		t.Fatalf("Action.Roles = %v, want [operator]", roles)
	}
}

// TestRBACRoles_SortedDeterministic confirms the orchestrator sorts the roles
// defensively before logging the Action, regardless of caller order.
func TestRBACRoles_SortedDeterministic(t *testing.T) {
	repo := initRepo(t)
	lb := event.NewLocalBus()
	t.Cleanup(func() { _ = lb.Close() })
	pol := &rolesRecordingPolicy{}
	eng := governance.NewEngine(pol)

	_, await, err := Run(context.Background(),
		sandbox.NewNoSandboxProvider(),
		&fakeAgent{script: `echo hi > out.txt`},
		&noopAuth{},
		RunOptions{
			Prompt:         "noop",
			CWD:            repo,
			SandboxImage:   "x",
			SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", t.Name(), "0"),
			Strategy:       gitm.StrategyMergeToHead,
			Bus:            lb,
			Governance:     eng,
			Roles:          []string{"writer", "admin", "operator"},
		})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res := await(); res.Status != "success" {
		t.Fatalf("status = %s, want success", res.Status)
	}
	roles, seen := pol.captured()
	if !seen {
		t.Fatal("policy never saw the ActionExecute gate")
	}
	if !slices.Equal(roles, []string{"admin", "operator", "writer"}) {
		t.Fatalf("Action.Roles = %v, want sorted [admin operator writer]", roles)
	}
}

// TestRBACRoles_EmptyByteIdentical confirms a Run with no Roles and no tool
// policy behaves exactly as today: the gate Action carries empty Roles and the
// run succeeds.
func TestRBACRoles_EmptyByteIdentical(t *testing.T) {
	repo := initRepo(t)
	lb := event.NewLocalBus()
	t.Cleanup(func() { _ = lb.Close() })
	pol := &rolesRecordingPolicy{}
	eng := governance.NewEngine(pol)

	_, await, err := Run(context.Background(),
		sandbox.NewNoSandboxProvider(),
		&fakeAgent{script: `echo hi > out.txt`},
		&noopAuth{},
		RunOptions{
			Prompt:         "noop",
			CWD:            repo,
			SandboxImage:   "x",
			SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", t.Name(), "0"),
			Strategy:       gitm.StrategyMergeToHead,
			Bus:            lb,
			Governance:     eng,
		})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res := await(); res.Status != "success" {
		t.Fatalf("status = %s, want success", res.Status)
	}
	roles, seen := pol.captured()
	if !seen {
		t.Fatal("policy never saw the ActionExecute gate")
	}
	if len(roles) != 0 {
		t.Fatalf("Action.Roles = %v, want empty", roles)
	}
}
