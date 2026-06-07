package orchestrator

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/agent"
	"github.com/felipemarques-rec/sandcode/internal/event"
	gitm "github.com/felipemarques-rec/sandcode/internal/git"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
)

// errStubSandbox is a minimal sandbox.Sandbox stub whose Exec always returns
// the configured error. All other methods are no-ops.
type errStubSandbox struct {
	execErr error
}

func (s errStubSandbox) Exec(_ context.Context, _ []string, _ io.Reader, _ sandbox.ExecOptions) (<-chan sandbox.ExecLine, sandbox.Wait, error) {
	return nil, nil, s.execErr
}
func (s errStubSandbox) CopyIn(_ context.Context, _, _ string) error  { return nil }
func (s errStubSandbox) CopyOut(_ context.Context, _, _ string) error { return nil }
func (s errStubSandbox) Close(_ context.Context) error                { return nil }

func TestCmdVerifier_PassOnExitZero(t *testing.T) {
	box, err := sandbox.NewNoSandboxProvider().Create(context.Background(), sandbox.SandboxSpec{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	t.Cleanup(func() { _ = box.Close(context.Background()) })

	v := cmdVerifier{cmd: []string{"sh", "-c", "echo ok; exit 0"}}
	out, err := v.Verify(context.Background(), VerifyInput{Box: box, Attempt: 1, TailBytes: 2000})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !out.Passed || out.ExitCode != 0 {
		t.Fatalf("want pass/exit0, got passed=%v exit=%d", out.Passed, out.ExitCode)
	}
	if !strings.Contains(out.StdoutTail, "ok") {
		t.Fatalf("tail missing output: %q", out.StdoutTail)
	}
}

func TestCmdVerifier_FailOnNonZeroAndTruncatesTail(t *testing.T) {
	box, err := sandbox.NewNoSandboxProvider().Create(context.Background(), sandbox.SandboxSpec{WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	t.Cleanup(func() { _ = box.Close(context.Background()) })

	v := cmdVerifier{cmd: []string{"sh", "-c", "for i in $(seq 1 200); do echo LINE$i; done; exit 7"}}
	out, err := v.Verify(context.Background(), VerifyInput{Box: box, Attempt: 2, TailBytes: 64})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if out.Passed || out.ExitCode != 7 {
		t.Fatalf("want fail/exit7, got passed=%v exit=%d", out.Passed, out.ExitCode)
	}
	if len(out.StdoutTail) > 64+len("...(truncated)\n") {
		t.Fatalf("tail not truncated to ~64 bytes: len=%d", len(out.StdoutTail))
	}
}

func TestCmdVerifier_ExecErrorPropagates(t *testing.T) {
	wantErr := errors.New("sandbox exec failed")
	stub := errStubSandbox{execErr: wantErr}

	v := cmdVerifier{cmd: []string{"anything"}}
	out, err := v.Verify(context.Background(), VerifyInput{Box: stub, TailBytes: 100})

	if err == nil {
		t.Fatal("expected non-nil error, got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("error does not wrap wantErr: got %v", err)
	}
	// VerifyOutput must be the zero value.
	if out.Passed {
		t.Errorf("want Passed=false, got true")
	}
	if out.ExitCode != 0 {
		t.Errorf("want ExitCode=0, got %d", out.ExitCode)
	}
	if out.StdoutTail != "" {
		t.Errorf("want StdoutTail empty, got %q", out.StdoutTail)
	}
}

// recordingVerifier counts calls and returns a fixed verdict; proves Run
// delegates to opts.Verifier instead of running Refine.VerifyCmd.
type recordingVerifier struct {
	calls  int32
	passed bool
}

func (r *recordingVerifier) Verify(_ context.Context, in VerifyInput) (VerifyOutput, error) {
	atomic.AddInt32(&r.calls, 1)
	ec := 1
	if r.passed {
		ec = 0
	}
	return VerifyOutput{Passed: r.passed, ExitCode: ec, StdoutTail: "custom"}, nil
}

func TestRun_NilVerifier_EmitsLegacyVerifyEvents(t *testing.T) {
	repo := initRepo(t)
	lb := event.NewLocalBus()
	t.Cleanup(func() { _ = lb.Close() })
	rec := &recorder{}
	lb.Subscribe("*", rec.handle)

	opts := RunOptions{
		Prompt:         "noop",
		CWD:            repo,
		SandboxImage:   "ignored",
		SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", t.Name(), "0"),
		Strategy:       gitm.StrategyMergeToHead,
		AgentOpts:      agent.RunOptions{},
		Bus:            lb,
		// No Verifier set → Run must build cmdVerifier from Refine.VerifyCmd.
		Refine: RefineOptions{Enabled: true, VerifyCmd: []string{"true"}, MaxAttempts: 1},
	}
	events, await, err := Run(context.Background(),
		sandbox.NewNoSandboxProvider(), &fakeAgent{script: "echo hi"}, &noopAuth{}, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for range events {
	}
	_ = await()
	if rec.count(event.VerifyStarted) != 1 || rec.count(event.VerifyPassed) != 1 {
		t.Fatalf("legacy verify events not emitted: started=%d passed=%d",
			rec.count(event.VerifyStarted), rec.count(event.VerifyPassed))
	}
}

func TestRun_CustomVerifier_OverridesVerifyCmd(t *testing.T) {
	repo := initRepo(t)
	rv := &recordingVerifier{passed: true}
	opts := RunOptions{
		Prompt:         "noop",
		CWD:            repo,
		SandboxImage:   "ignored",
		SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", t.Name(), "0"),
		Strategy:       gitm.StrategyMergeToHead,
		AgentOpts:      agent.RunOptions{},
		Verifier:       rv,
		// VerifyCmd present but MUST be ignored in favor of rv.
		Refine: RefineOptions{Enabled: true, VerifyCmd: []string{"false"}, MaxAttempts: 1},
	}
	events, await, err := Run(context.Background(),
		sandbox.NewNoSandboxProvider(), &fakeAgent{script: "echo hi"}, &noopAuth{}, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for range events {
	}
	res := await()
	if atomic.LoadInt32(&rv.calls) != 1 {
		t.Fatalf("custom verifier not called exactly once: %d", rv.calls)
	}
	if res.Status != "success" {
		t.Fatalf("custom verifier passed=true but status=%q", res.Status)
	}
}

func TestRun_SetsLastVerifyOnPass(t *testing.T) {
	repo := initRepo(t)
	opts := RunOptions{
		Prompt:         "noop",
		CWD:            repo,
		SandboxImage:   "ignored",
		SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", t.Name(), "0"),
		Strategy:       gitm.StrategyMergeToHead,
		AgentOpts:      agent.RunOptions{},
		Refine:         RefineOptions{Enabled: true, VerifyCmd: []string{"true"}, MaxAttempts: 1},
	}
	events, await, err := Run(context.Background(),
		sandbox.NewNoSandboxProvider(), &fakeAgent{script: "echo hi"}, &noopAuth{}, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for range events {
	}
	res := await()
	if res.LastVerify == nil {
		t.Fatalf("LastVerify should be set when verify ran")
	}
	if !res.LastVerify.Passed || res.LastVerify.ExitCode != 0 {
		t.Fatalf("expected passed verify, got %+v", res.LastVerify)
	}
}

func TestRun_LastVerifyNilWhenRefineInactive(t *testing.T) {
	repo := initRepo(t)
	opts := RunOptions{
		Prompt:         "noop",
		CWD:            repo,
		SandboxImage:   "ignored",
		SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", t.Name(), "0"),
		Strategy:       gitm.StrategyMergeToHead,
		AgentOpts:      agent.RunOptions{},
		// No Refine → no verify → LastVerify must stay nil (byte-identical).
	}
	events, await, err := Run(context.Background(),
		sandbox.NewNoSandboxProvider(), &fakeAgent{script: "echo hi"}, &noopAuth{}, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for range events {
	}
	res := await()
	if res.LastVerify != nil {
		t.Fatalf("LastVerify must be nil when refine inactive, got %+v", res.LastVerify)
	}
}

// flipVerifier fails the first call, passes thereafter — to test that
// Result.LastVerify reflects the LAST verify of a multi-attempt run.
type flipVerifier struct{ calls int }

func (f *flipVerifier) Verify(_ context.Context, _ VerifyInput) (VerifyOutput, error) {
	f.calls++
	if f.calls == 1 {
		return VerifyOutput{Passed: false, ExitCode: 1, StdoutTail: "first fail"}, nil
	}
	return VerifyOutput{Passed: true, ExitCode: 0, StdoutTail: "second ok"}, nil
}

func TestRun_LastVerifyReflectsFinalAttempt(t *testing.T) {
	repo := initRepo(t)
	fv := &flipVerifier{}
	opts := RunOptions{
		Prompt:         "noop",
		CWD:            repo,
		SandboxImage:   "ignored",
		SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", t.Name(), "0"),
		Strategy:       gitm.StrategyMergeToHead,
		AgentOpts:      agent.RunOptions{},
		Verifier:       fv,
		Refine:         RefineOptions{Enabled: true, VerifyCmd: []string{"ignored"}, MaxAttempts: 3},
	}
	events, await, err := Run(context.Background(),
		sandbox.NewNoSandboxProvider(), &fakeAgent{script: "echo hi"}, &noopAuth{}, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for range events {
	}
	res := await()
	if fv.calls < 2 {
		t.Fatalf("expected at least 2 verify calls (fail then pass), got %d", fv.calls)
	}
	if res.LastVerify == nil || !res.LastVerify.Passed || res.LastVerify.ExitCode != 0 {
		t.Fatalf("LastVerify should reflect the final PASSING verify, got %+v", res.LastVerify)
	}
	if res.Status != "success" {
		t.Fatalf("run should succeed after refine passes, got %q", res.Status)
	}
}
