package orchestrator

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/agent"
	"github.com/felipemarques-rec/sandcode/internal/budget"
	"github.com/felipemarques-rec/sandcode/internal/event"
	gitm "github.com/felipemarques-rec/sandcode/internal/git"
	"github.com/felipemarques-rec/sandcode/internal/governance"
	"github.com/felipemarques-rec/sandcode/internal/governance/builtin"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
)

// alwaysDenyPolicy denies every action. Useful for asserting the
// pre-run gate aborts before any sandbox is provisioned.
type alwaysDenyPolicy struct{}

func (alwaysDenyPolicy) Name() string { return "always_deny" }
func (alwaysDenyPolicy) Evaluate(_ context.Context, _ governance.Action) (governance.Result, string, error) {
	return governance.Deny, "test: blanket deny", nil
}

func setupGovernanceRun(t *testing.T) (string, *event.LocalBus, *recorder, governance.AuditLog) {
	t.Helper()
	repo := initRepo(t)
	lb := event.NewLocalBus()
	t.Cleanup(func() { _ = lb.Close() })
	rec := &recorder{}
	lb.Subscribe("*", rec.handle)
	audit, err := governance.OpenAuditLog(filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatalf("OpenAuditLog: %v", err)
	}
	t.Cleanup(func() { _ = audit.Close() })
	return repo, lb, rec, audit
}

// TestGovernance_PreRunDenyAbortsBeforeSandbox confirms a blanket-deny
// policy stops the run BEFORE any worktree/sandbox provisioning. The
// returned error must mention "governance" and the audit log must
// contain the decision row.
func TestGovernance_PreRunDenyAbortsBeforeSandbox(t *testing.T) {
	repo, bus, rec, audit := setupGovernanceRun(t)
	engine := governance.NewEngine(alwaysDenyPolicy{})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	events, await, err := Run(ctx,
		sandbox.NewNoSandboxProvider(),
		&fakeAgent{script: `echo should-not-run`},
		&noopAuth{},
		RunOptions{
			Prompt:         "noop",
			CWD:            repo,
			SandboxImage:   "ignored",
			SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", "gov-deny", "0"),
			Strategy:       gitm.StrategyMergeToHead,
			AgentOpts:      agent.RunOptions{},
			Bus:            bus,
			Governance:     engine,
			AuditLog:       audit,
		},
	)
	if err == nil {
		t.Fatalf("expected pre-run denial error")
	}
	if !strings.Contains(err.Error(), "governance") {
		t.Fatalf("error should mention governance: %v", err)
	}
	if events != nil || await != nil {
		t.Fatalf("Run should return nils on pre-run deny: events_nil=%v await_nil=%v",
			events == nil, await == nil)
	}

	// Bus should have run.submitted, governance.denied, and run.failed —
	// but NOT sandbox.created (we aborted before provisioning).
	if rec.count(event.RunSubmitted) != 1 {
		t.Errorf("run.submitted count = %d, want 1", rec.count(event.RunSubmitted))
	}
	if rec.count(event.GovernanceDenied) != 1 {
		t.Errorf("governance.denied count = %d, want 1", rec.count(event.GovernanceDenied))
	}
	if rec.count(event.RunFailed) != 1 {
		t.Errorf("run.failed count = %d, want 1", rec.count(event.RunFailed))
	}
	if rec.count(event.SandboxCreated) != 0 {
		t.Errorf("sandbox.created fired despite pre-run deny — wasted setup")
	}
	if rec.count(event.AgentExecuting) != 0 {
		t.Errorf("agent.executing fired despite pre-run deny")
	}

	// Audit log should record the decision. Aggregate row + per-policy row.
	rows, err := audit.ListByRun(ctx, "")
	_ = rows
	_ = err
	// We don't know the runID without parsing the recorded event; just
	// confirm there is at least one row in some run.
	// (Iterate all known runs from recorded events.)
	gotAudit := false
	for _, ev := range rec.evs {
		rows, _ := audit.ListByRun(ctx, ev.RunID)
		if len(rows) > 0 {
			gotAudit = true
			break
		}
	}
	if !gotAudit {
		t.Errorf("audit log empty after governance deny")
	}
}

// TestGovernance_AllowedRunCompletesAndAudits confirms the happy path:
// engine has policies but they all Allow → run completes successfully,
// audit log STILL records the decisions for traceability.
func TestGovernance_AllowedRunCompletesAndAudits(t *testing.T) {
	repo, bus, rec, audit := setupGovernanceRun(t)
	// Configure policies that all return Allow for the action types used.
	engine := governance.NewEngine(
		builtin.RetryLimit{MaxAttempts: 10},
		builtin.DiffSize{ReviewAboveBytes: 1_000_000},
		builtin.Budget{MaxTokens: 1_000_000, MaxCostUSD: 1000},
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	events, await, err := Run(ctx,
		sandbox.NewNoSandboxProvider(),
		&fakeAgent{script: `echo done > out.txt`},
		&noopAuth{},
		RunOptions{
			Prompt:         "noop",
			CWD:            repo,
			SandboxImage:   "ignored",
			SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", "gov-allow", "0"),
			Strategy:       gitm.StrategyMergeToHead,
			AgentOpts:      agent.RunOptions{},
			Bus:            bus,
			Governance:     engine,
			AuditLog:       audit,
		},
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for range events {
	}
	res := await()
	if res.Status != "success" {
		t.Fatalf("status=%s err=%v", res.Status, res.Err)
	}
	if rec.count(event.GovernanceDenied) != 0 {
		t.Errorf("governance.denied fired on an allowed run")
	}

	// Audit log should have at least 1 row (the ActionExecute decision).
	rows, _ := audit.ListByRun(ctx, res.RunID)
	if len(rows) == 0 {
		t.Errorf("audit log missing rows for successful run")
	}
}

// TestGovernance_BudgetStopsRefineLoop drives the canonical Stage-2
// cost-ceiling scenario: refine loop wants to iterate but Budget policy
// rejects the next attempt before it starts. Run terminates as failure.
func TestGovernance_BudgetStopsRefineLoop(t *testing.T) {
	repo, bus, rec, audit := setupGovernanceRun(t)

	guard := budget.New()
	// Configure budget engine so the SECOND attempt would push attempts
	// over MaxAttempts (testing RetryLimit via governance instead of
	// the orchestrator's local cap). We set the orchestrator's local
	// RefineOptions.MaxAttempts high enough that ONLY the governance
	// engine should stop the loop.
	engine := governance.NewEngine(
		builtin.RetryLimit{MaxAttempts: 1}, // any refine attempt → deny
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	events, await, err := Run(ctx,
		sandbox.NewNoSandboxProvider(),
		// Agent makes progress but verifier always fails — would loop
		// forever without governance.
		&fakeAgent{script: `echo "tried" > log.txt`},
		&noopAuth{},
		RunOptions{
			Prompt:         "noop",
			CWD:            repo,
			SandboxImage:   "ignored",
			SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", "gov-budget", "0"),
			Strategy:       gitm.StrategyMergeToHead,
			AgentOpts:      agent.RunOptions{},
			Bus:            bus,
			Governance:     engine,
			AuditLog:       audit,
			Budget:         guard,
			Refine: RefineOptions{
				Enabled:     true,
				VerifyCmd:   []string{"sh", "-c", "test -f .never"},
				MaxAttempts: 10, // intentionally high; gov should fire first
			},
		},
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for range events {
	}
	res := await()
	if res.Status != "failure" {
		t.Fatalf("status=%s, want failure", res.Status)
	}
	if res.Attempts != 1 {
		t.Fatalf("Attempts=%d, want 1 (governance denied refine at attempt 2)", res.Attempts)
	}

	if rec.count(event.GovernanceDenied) < 1 {
		t.Errorf("governance.denied not emitted on refine-gate")
	}
	if rec.count(event.RefineTriggered) != 0 {
		t.Errorf("refine.triggered should NOT fire when governance denies the next attempt")
	}
	if rec.count(event.RunFailed) != 1 {
		t.Errorf("run.failed count = %d, want 1", rec.count(event.RunFailed))
	}
	if rec.count(event.AgentExecuting) != 1 {
		t.Errorf("agent.executing count = %d, want 1 (no refine round)", rec.count(event.AgentExecuting))
	}

	// Budget guard should show attempt=1 (the initial attempt; refine never happened).
	if r := guard.Report(res.RunID); r.Attempts != 1 {
		t.Errorf("budget Attempts = %d, want 1", r.Attempts)
	}

	// Audit log should have the refine-Deny row.
	rows, _ := audit.ListByRun(ctx, res.RunID)
	gotDeny := false
	for _, r := range rows {
		if r.Result == governance.Deny && r.ActionType == governance.ActionRefine {
			gotDeny = true
		}
	}
	if !gotDeny {
		t.Errorf("audit log missing refine-Deny row; got %d rows", len(rows))
	}
}
