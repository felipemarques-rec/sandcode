package orchestrator

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/approval"
	"github.com/felipemarques-rec/sandcode/internal/event"
	gitm "github.com/felipemarques-rec/sandcode/internal/git"
	"github.com/felipemarques-rec/sandcode/internal/governance"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
)

// reviewPolicy always returns Review.
type reviewPolicy struct{}

func (reviewPolicy) Name() string { return "always-review" }
func (reviewPolicy) Evaluate(_ context.Context, _ governance.Action) (governance.Result, string, error) {
	return governance.Review, "needs human approval", nil
}

type stubApprover struct {
	decision approval.Decision
	err      error
}

func (s stubApprover) RequestApproval(_ context.Context, _ approval.Request) (approval.Decision, error) {
	return s.decision, s.err
}

func runReviewGated(t *testing.T, ap approval.Approver, timeout time.Duration) (Result, *recorder) {
	t.Helper()
	repo := initRepo(t)
	lb := event.NewLocalBus()
	t.Cleanup(func() { _ = lb.Close() })
	rec := &recorder{}
	lb.Subscribe("*", rec.handle)
	eng := governance.NewEngine(reviewPolicy{})
	_, await, err := Run(context.Background(),
		sandbox.NewNoSandboxProvider(),
		&fakeAgent{script: `echo hi > out.txt`},
		&noopAuth{},
		RunOptions{
			Prompt:          "noop",
			CWD:             repo,
			SandboxImage:    "x",
			SandboxWorkDir:  filepath.Join(repo, ".sandcode", "work", t.Name(), "0"),
			Strategy:        gitm.StrategyMergeToHead,
			Bus:             lb,
			Governance:      eng,
			Approver:        ap,
			ApprovalTimeout: timeout,
		},
	)
	// Fail-closed gate returns (nil, nil, err): no await closure to call.
	if err != nil || await == nil {
		return Result{Status: "failure"}, rec
	}
	return await(), rec
}

func TestApproval_GrantedProceeds(t *testing.T) {
	res, rec := runReviewGated(t, stubApprover{decision: approval.Decision{Approved: true, Approver: "alice"}}, 2*time.Second)
	if res.Status != "success" {
		t.Fatalf("status = %s, want success", res.Status)
	}
	if rec.count(event.GovernanceApprovalRequired) != 1 {
		t.Fatal("expected GovernanceApprovalRequired")
	}
	if rec.count(event.GovernanceApproved) != 1 {
		t.Fatal("expected GovernanceApproved on grant")
	}
}

func TestApproval_RejectedFailsClosed(t *testing.T) {
	res, rec := runReviewGated(t, stubApprover{decision: approval.Decision{Approved: false}}, 2*time.Second)
	if res.Status == "success" {
		t.Fatal("rejected run must not succeed")
	}
	if rec.count(event.GovernanceApproved) != 0 {
		t.Fatal("no GovernanceApproved on reject")
	}
}

func TestApproval_TimeoutFailsClosed(t *testing.T) {
	blocking := approverFunc(func(ctx context.Context, _ approval.Request) (approval.Decision, error) {
		<-ctx.Done()
		return approval.Decision{}, ctx.Err()
	})
	res, _ := runReviewGated(t, blocking, 50*time.Millisecond)
	if res.Status == "success" {
		t.Fatal("timed-out run must not succeed")
	}
}

func TestApproval_NilApproverFailsClosed(t *testing.T) {
	res, _ := runReviewGated(t, nil, 2*time.Second)
	if res.Status == "success" {
		t.Fatal("nil approver + Review must fail closed")
	}
}

func TestApproval_NoGovernanceByteIdentical(t *testing.T) {
	repo := initRepo(t)
	lb := event.NewLocalBus()
	t.Cleanup(func() { _ = lb.Close() })
	rec := &recorder{}
	lb.Subscribe("*", rec.handle)
	_, await, err := Run(context.Background(),
		sandbox.NewNoSandboxProvider(), &fakeAgent{script: `echo hi > out.txt`}, &noopAuth{},
		RunOptions{
			Prompt: "noop", CWD: repo, SandboxImage: "x",
			SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", t.Name(), "0"),
			Strategy:       gitm.StrategyMergeToHead, Bus: lb,
		})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	res := await()
	if res.Status != "success" {
		t.Fatalf("status = %s, want success", res.Status)
	}
	if rec.count(event.GovernanceApprovalRequired) != 0 {
		t.Fatal("no approval events without governance (byte-identical)")
	}
}

type approverFunc func(context.Context, approval.Request) (approval.Decision, error)

func (f approverFunc) RequestApproval(ctx context.Context, r approval.Request) (approval.Decision, error) {
	return f(ctx, r)
}
