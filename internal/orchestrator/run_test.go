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
	gitm "github.com/felipemarques-rec/sandcode/internal/git"
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
