package orchestrator

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/agent"
	"github.com/felipemarques-rec/sandcode/internal/auth"
	"github.com/felipemarques-rec/sandcode/internal/event"
	gitm "github.com/felipemarques-rec/sandcode/internal/git"
	"github.com/felipemarques-rec/sandcode/internal/mcp"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
)

// fakeAgent invokes a shell snippet that writes a file inside the worktree.
// It exercises the full Run() path without needing Claude Code installed.
type fakeAgent struct{ script string }

func (*fakeAgent) Name() string { return "fake" }
func (f *fakeAgent) BuildCommand(opts agent.RunOptions) agent.Command {
	return agent.Command{Argv: []string{"sh", "-c", f.script}}
}
func (*fakeAgent) ParseLine(line string) (agent.StreamEvent, bool) {
	if line == "" {
		return agent.StreamEvent{}, false
	}
	return agent.StreamEvent{Kind: agent.EventText, Text: line}, true
}
func (*fakeAgent) AuthHints() agent.AuthHints { return agent.AuthHints{} }

// noopAuth applies nothing.
type noopAuth struct{}

func (*noopAuth) Name() string                                                 { return "noop" }
func (*noopAuth) Apply(spec *sandbox.SandboxSpec, hints agent.AuthHints) error { return nil }

var _ auth.Provider = (*noopAuth)(nil)

func initRepo(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = tmp
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(tmp, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "."},
		{"commit", "-q", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = tmp
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return tmp
}

func TestRun_NoSandbox_MergeToHead(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	// The fake agent runs in the host (nosandbox) but with WorkDir set to the
	// worktree path bound at SandboxWorkDir. With nosandbox, the script's $PWD
	// is the worktree on the host directly.
	script := `echo "writing file"; echo "world" > hello.txt; echo "done"`

	events, await, err := Run(ctx,
		sandbox.NewNoSandboxProvider(),
		&fakeAgent{script: script},
		&noopAuth{},
		RunOptions{
			Prompt:         "noop",
			CWD:            repo,
			SandboxImage:   "ignored-by-nosandbox",
			SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", "test", "0"),
			Strategy:       gitm.StrategyMergeToHead,
		},
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var got []string
	for ev := range events {
		got = append(got, ev.Text)
	}
	if len(got) < 2 {
		t.Fatalf("expected ≥2 events, got %v", got)
	}

	res := await()
	if res.Status != "success" {
		t.Fatalf("status=%s err=%v", res.Status, res.Err)
	}
	if !strings.Contains(res.Diff, "hello.txt") {
		t.Fatalf("diff missing hello.txt:\n%s", res.Diff)
	}

	// Verify merge landed on main
	if _, err := os.Stat(filepath.Join(repo, "hello.txt")); err != nil {
		t.Fatalf("hello.txt should exist on main after merge: %v", err)
	}
}

func TestRun_NoSandbox_BranchStrategyKeepsHEADClean(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	script := `echo done > newfile.txt`

	_, await, err := Run(ctx,
		sandbox.NewNoSandboxProvider(),
		&fakeAgent{script: script},
		&noopAuth{},
		RunOptions{
			Prompt:         "noop",
			CWD:            repo,
			SandboxImage:   "x",
			SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", "branch-test", "0"),
			Strategy:       gitm.StrategyBranch,
			KeepWorktree:   true, // so we can verify the branch survived
		},
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	res := await()
	if res.Status != "success" {
		t.Fatalf("status=%s err=%v", res.Status, res.Err)
	}
	if _, err := os.Stat(filepath.Join(repo, "newfile.txt")); err == nil {
		t.Fatal("file should NOT be on main with branch strategy")
	}
}

// TestRun_MCP_InjectsAndDoesNotLeak verifies that a configured MCP manager
// (1) writes .mcp.json into the worktree before the agent runs, (2) emits an
// observation-only mcp.injected event, and (3) does NOT leak .mcp.json into the
// merged repo (it is removed before commit).
func TestRun_MCP_InjectsAndDoesNotLeak(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	lb := event.NewLocalBus()
	t.Cleanup(func() { _ = lb.Close() })
	rec := &recorder{}
	lb.Subscribe("*", rec.handle)

	// The agent records whether .mcp.json was visible at its cwd into a marker
	// file. The marker IS the agent's work product (it merges to head); the
	// injected .mcp.json must NOT.
	script := `if [ -f .mcp.json ]; then echo present > saw_mcp.txt; else echo absent > saw_mcp.txt; fi`

	mgr := mcp.NewManager(mcp.DefaultConfigs())
	mgr.Enable("context7")

	_, await, err := Run(ctx,
		sandbox.NewNoSandboxProvider(),
		&fakeAgent{script: script},
		&noopAuth{},
		RunOptions{
			Prompt:         "noop",
			CWD:            repo,
			SandboxImage:   "x",
			SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", "mcp-test", "0"),
			Strategy:       gitm.StrategyMergeToHead,
			Bus:            lb,
			MCP:            mgr,
		},
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	res := await()
	if res.Status != "success" {
		t.Fatalf("status=%s err=%v", res.Status, res.Err)
	}

	// 1. .mcp.json was present while the agent ran.
	saw, err := os.ReadFile(filepath.Join(repo, "saw_mcp.txt"))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if strings.TrimSpace(string(saw)) != "present" {
		t.Fatalf(".mcp.json was not visible to the agent: marker=%q", saw)
	}

	// 2. exactly one mcp.injected event.
	if n := rec.count(event.MCPInjected); n != 1 {
		t.Fatalf("mcp.injected count = %d, want 1", n)
	}

	// 3. no leak: .mcp.json must not have merged into main.
	if _, err := os.Stat(filepath.Join(repo, ".mcp.json")); !os.IsNotExist(err) {
		t.Fatalf(".mcp.json leaked into main repo, stat err = %v", err)
	}
	if strings.Contains(res.Diff, ".mcp.json") {
		t.Fatalf(".mcp.json leaked into the run diff:\n%s", res.Diff)
	}
}

// TestRun_MCP_NilIsByteIdentical guards the nil-MCP legacy path: no .mcp.json,
// no mcp.injected event.
func TestRun_MCP_NilIsByteIdentical(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	lb := event.NewLocalBus()
	t.Cleanup(func() { _ = lb.Close() })
	rec := &recorder{}
	lb.Subscribe("*", rec.handle)

	script := `if [ -f .mcp.json ]; then echo present > saw.txt; else echo absent > saw.txt; fi`

	_, await, err := Run(ctx,
		sandbox.NewNoSandboxProvider(),
		&fakeAgent{script: script},
		&noopAuth{},
		RunOptions{
			Prompt:         "noop",
			CWD:            repo,
			SandboxImage:   "x",
			SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", "mcp-nil-test", "0"),
			Strategy:       gitm.StrategyMergeToHead,
			Bus:            lb,
			// MCP: nil
		},
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	res := await()
	if res.Status != "success" {
		t.Fatalf("status=%s err=%v", res.Status, res.Err)
	}
	saw, _ := os.ReadFile(filepath.Join(repo, "saw.txt"))
	if strings.TrimSpace(string(saw)) != "absent" {
		t.Fatalf("expected no .mcp.json with nil MCP, marker=%q", saw)
	}
	if n := rec.count(event.MCPInjected); n != 0 {
		t.Fatalf("mcp.injected count = %d, want 0 with nil MCP", n)
	}
}
