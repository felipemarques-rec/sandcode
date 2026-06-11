package orchestrator

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/agent"
	"github.com/felipemarques-rec/sandcode/internal/event"
	gitm "github.com/felipemarques-rec/sandcode/internal/git"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
)

// driveCycleRun runs one agent through the refine/lint cycle with the reactive
// flag set explicitly, returning the Result and the ordered event-type log with
// the SP3.2 observation-only command events filtered out (so the imperative and
// reactive runs can be compared event-for-event).
func driveCycleRun(t *testing.T, script string, refine RefineOptions, lintCmd []string, reactive bool) (Result, []event.Type) {
	t.Helper()
	repo := initRepo(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	lb := event.NewLocalBus()
	t.Cleanup(func() { _ = lb.Close() })
	rec := &recorder{}
	lb.Subscribe("*", rec.handle)

	events, await, err := Run(ctx,
		sandbox.NewNoSandboxProvider(),
		&fakeAgent{script: script},
		&noopAuth{},
		RunOptions{
			Prompt:         "noop",
			CWD:            repo,
			SandboxImage:   "ignored",
			SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", t.Name(), "0"),
			Strategy:       gitm.StrategyMergeToHead,
			AgentOpts:      agent.RunOptions{},
			Bus:            lb,
			Refine:         refine,
			LintCmd:        lintCmd,
			Reactive:       reactive,
		},
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for range events { // drain
	}
	res := await()

	var filtered []event.Type
	for _, typ := range rec.types() {
		switch typ {
		case event.ExecuteRequested, event.VerifyRequested, event.LintRequested:
			// SP3.2 cycle commands: present only in reactive mode; ignore.
		default:
			filtered = append(filtered, typ)
		}
	}
	return res, filtered
}

// TestReactiveCycle_EquivalentToImperative (SP3.2) asserts the reactive refine
// cycle produces the SAME Result and the SAME result-event sequence as the
// imperative attempt loop across the full range of outcomes.
func TestReactiveCycle_EquivalentToImperative(t *testing.T) {
	const (
		progressive = `
c=$(cat .c 2>/dev/null || echo 0); c=$((c+1)); echo "$c" > .c
if [ "$c" -ge 2 ]; then echo fixed > .fixed; fi
`
		progressiveLint = `
c=$(cat .lc 2>/dev/null || echo 0); c=$((c+1)); echo "$c" > .lc
if [ "$c" -ge 2 ]; then echo ok > .lint-ok; fi
`
	)
	cases := []struct {
		name    string
		script  string
		refine  RefineOptions
		lintCmd []string
	}{
		{"verify-pass-first", `echo ok > fix.txt`,
			RefineOptions{Enabled: true, VerifyCmd: []string{"sh", "-c", "test -f fix.txt"}, MaxAttempts: 3}, nil},
		{"verify-fail-then-pass", progressive,
			RefineOptions{Enabled: true, VerifyCmd: []string{"sh", "-c", "test -f .fixed"}, MaxAttempts: 3}, nil},
		{"verify-exhaust", `echo nope`,
			RefineOptions{Enabled: true, VerifyCmd: []string{"sh", "-c", "test -f .never"}, MaxAttempts: 2}, nil},
		{"agent-crash", `echo starting; exit 7`,
			RefineOptions{Enabled: true, VerifyCmd: []string{"true"}, MaxAttempts: 3}, nil},
		{"lint-fail-then-pass", progressiveLint,
			RefineOptions{Enabled: true, MaxAttempts: 3}, []string{"sh", "-c", "test -f .lint-ok"}},
		{"verify-and-lint-pass", `echo ok > fix.txt; echo ok > .lint-ok`,
			RefineOptions{Enabled: true, VerifyCmd: []string{"sh", "-c", "test -f fix.txt"}, MaxAttempts: 3},
			[]string{"sh", "-c", "test -f .lint-ok"}},
		{"lint-exhaust", `echo nope`,
			RefineOptions{Enabled: true, MaxAttempts: 2}, []string{"sh", "-c", "test -f .never"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			impRes, impEvents := driveCycleRun(t, tc.script, tc.refine, tc.lintCmd, false)
			reaRes, reaEvents := driveCycleRun(t, tc.script, tc.refine, tc.lintCmd, true)

			if impRes.Status != reaRes.Status {
				t.Errorf("Status: imperative=%s reactive=%s", impRes.Status, reaRes.Status)
			}
			if impRes.ExitCode != reaRes.ExitCode {
				t.Errorf("ExitCode: imperative=%d reactive=%d", impRes.ExitCode, reaRes.ExitCode)
			}
			if impRes.Attempts != reaRes.Attempts {
				t.Errorf("Attempts: imperative=%d reactive=%d", impRes.Attempts, reaRes.Attempts)
			}
			if (impRes.Err == nil) != (reaRes.Err == nil) {
				t.Errorf("Err presence: imperative=%v reactive=%v", impRes.Err, reaRes.Err)
			}
			if impRes.Err != nil && reaRes.Err != nil && impRes.Err.Error() != reaRes.Err.Error() {
				t.Errorf("Err message:\n imperative=%q\n reactive=%q", impRes.Err.Error(), reaRes.Err.Error())
			}
			if (impRes.LastVerify == nil) != (reaRes.LastVerify == nil) {
				t.Errorf("LastVerify presence: imperative=%v reactive=%v", impRes.LastVerify, reaRes.LastVerify)
			}
			if impRes.LastVerify != nil && reaRes.LastVerify != nil {
				if impRes.LastVerify.Passed != reaRes.LastVerify.Passed || impRes.LastVerify.ExitCode != reaRes.LastVerify.ExitCode {
					t.Errorf("LastVerify: imperative=%+v reactive=%+v", *impRes.LastVerify, *reaRes.LastVerify)
				}
			}

			if len(impEvents) != len(reaEvents) {
				t.Fatalf("event count: imperative=%d reactive=%d\n imp=%v\n rea=%v",
					len(impEvents), len(reaEvents), impEvents, reaEvents)
			}
			for i := range impEvents {
				if impEvents[i] != reaEvents[i] {
					t.Fatalf("event[%d]: imperative=%s reactive=%s\n imp=%v\n rea=%v",
						i, impEvents[i], reaEvents[i], impEvents, reaEvents)
				}
			}
		})
	}
}

// TestReactiveCycle_EmitsCommandEvents verifies the reactive cycle actually
// drives via the observation-only command events (so it's the reactor path, not
// a silent fallback to the imperative loop).
func TestReactiveCycle_EmitsCommandEvents(t *testing.T) {
	repo := initRepo(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	lb := event.NewLocalBus()
	t.Cleanup(func() { _ = lb.Close() })
	rec := &recorder{}
	lb.Subscribe("*", rec.handle)

	events, await, err := Run(ctx, sandbox.NewNoSandboxProvider(),
		&fakeAgent{script: `echo ok > fix.txt`}, &noopAuth{},
		RunOptions{
			Prompt: "noop", CWD: repo, SandboxImage: "ignored",
			SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", t.Name(), "0"),
			Strategy:       gitm.StrategyMergeToHead, AgentOpts: agent.RunOptions{}, Bus: lb,
			Refine:   RefineOptions{Enabled: true, VerifyCmd: []string{"sh", "-c", "test -f fix.txt"}, MaxAttempts: 3},
			LintCmd:  []string{"sh", "-c", "test -f fix.txt"},
			Reactive: true,
		})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for range events {
	}
	_ = await()

	for _, typ := range []event.Type{event.ExecuteRequested, event.VerifyRequested, event.LintRequested} {
		if rec.count(typ) == 0 {
			t.Errorf("reactive cycle did not emit %s; events: %v", typ, rec.types())
		}
	}
}
