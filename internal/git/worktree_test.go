package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

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

func TestWorktreeCreateDiffMergeRemove(t *testing.T) {
	repo := initRepo(t)
	mgr := NewManager()
	ctx := context.Background()

	wtDir := filepath.Join(repo, ".sandcode", "work", "abc", "0")
	wt, err := mgr.Create(ctx, repo, wtDir, "sandcode/test-1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if wt.BaseRef == "" {
		t.Fatal("BaseRef empty")
	}

	// modify
	if err := os.WriteFile(filepath.Join(wtDir, "hello.txt"), []byte("world\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	changed, err := mgr.HasChanges(ctx, wt)
	if err != nil || !changed {
		t.Fatalf("HasChanges=%v err=%v", changed, err)
	}

	if err := mgr.CommitAll(ctx, wt, "test commit"); err != nil {
		t.Fatalf("CommitAll: %v", err)
	}

	diff, err := mgr.Diff(ctx, wt)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if diff == "" {
		t.Fatal("expected non-empty diff")
	}

	if err := mgr.MergeToHead(ctx, wt); err != nil {
		t.Fatalf("MergeToHead: %v", err)
	}

	// Verify file landed on main
	content, err := os.ReadFile(filepath.Join(repo, "hello.txt"))
	if err != nil {
		t.Fatalf("file should exist on main: %v", err)
	}
	if string(content) != "world\n" {
		t.Fatalf("file content: %q", content)
	}

	if err := mgr.Remove(ctx, wt, true); err != nil {
		t.Fatalf("Remove: %v", err)
	}
}

func TestEnsureGitRepoFails(t *testing.T) {
	mgr := NewManager()
	_, err := mgr.Create(context.Background(), t.TempDir(), filepath.Join(t.TempDir(), "wt"), "x")
	if err == nil {
		t.Fatal("expected error for non-git dir")
	}
}
