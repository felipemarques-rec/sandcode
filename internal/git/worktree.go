// Package git provides minimal git-worktree management used to isolate each
// agent run in its own worktree without touching the user's working tree.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// repoMu serializes access to the same git repository directory across
// concurrent worktree create/merge/remove calls. Git's own internal locking
// is not safe under concurrent workspace mutations from different processes
// of our own (it can race on .git/worktrees/, refs/, packed-refs).
var (
	repoMuMu sync.Mutex
	repoMus  = map[string]*sync.Mutex{}
)

func repoLock(path string) *sync.Mutex {
	repoMuMu.Lock()
	defer repoMuMu.Unlock()
	if m, ok := repoMus[path]; ok {
		return m
	}
	m := &sync.Mutex{}
	repoMus[path] = m
	return m
}

// Strategy decides what happens to a worktree once the agent finishes.
type Strategy string

const (
	// StrategyMergeToHead merges the worktree's branch back into the source
	// HEAD on success. Mirrors sandcastle's "merge-to-head" mode.
	StrategyMergeToHead Strategy = "merge-to-head"

	// StrategyBranch leaves the run on a named branch and does NOT touch HEAD.
	// The user can review and merge/PR manually. Mirrors sandcastle's "branch".
	StrategyBranch Strategy = "branch"
)

// Worktree represents one isolated working copy created from a source repo.
type Worktree struct {
	// SourceRepo is the original git repository that hosts the worktree.
	SourceRepo string

	// Path is the absolute path of the new worktree on disk.
	Path string

	// Branch is the branch checked out in the worktree.
	Branch string

	// BaseRef is the commit/branch the worktree was created from. Used for
	// diff capture and merge-to-head fast-forward checks.
	BaseRef string
}

// Manager creates, captures, and disposes of worktrees.
type Manager struct{}

func NewManager() *Manager { return &Manager{} }

// Create makes a new worktree at dir based on the current HEAD of sourceRepo,
// checking out a fresh branch named branch.
func (*Manager) Create(ctx context.Context, sourceRepo, dir, branch string) (*Worktree, error) {
	mu := repoLock(sourceRepo)
	mu.Lock()
	defer mu.Unlock()

	if err := ensureGitRepo(ctx, sourceRepo); err != nil {
		return nil, err
	}
	baseRef, err := runGit(ctx, sourceRepo, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, err
	}
	if _, err := runGit(ctx, sourceRepo, "worktree", "add", "-b", branch, dir, strings.TrimSpace(baseRef)); err != nil {
		return nil, fmt.Errorf("git worktree add: %w", err)
	}
	return &Worktree{
		SourceRepo: sourceRepo,
		Path:       dir,
		Branch:     branch,
		BaseRef:    strings.TrimSpace(baseRef),
	}, nil
}

// Diff returns the diff of the worktree against its base ref. Empty string
// means no changes.
func (*Manager) Diff(ctx context.Context, wt *Worktree) (string, error) {
	out, err := runGit(ctx, wt.Path, "diff", wt.BaseRef+"..HEAD")
	if err != nil {
		return "", err
	}
	return out, nil
}

// HasChanges returns true when the worktree has new commits, staged, or
// unstaged changes relative to BaseRef. We use a lightweight check that
// compares HEAD against BaseRef and queries the working tree status.
func (*Manager) HasChanges(ctx context.Context, wt *Worktree) (bool, error) {
	headOut, err := runGit(ctx, wt.Path, "rev-parse", "HEAD")
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(headOut) != wt.BaseRef {
		return true, nil
	}
	statusOut, err := runGit(ctx, wt.Path, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(statusOut) != "", nil
}

// CommitAll stages all changes (including untracked) and commits with the
// given message. No-op when there is nothing to commit.
func (m *Manager) CommitAll(ctx context.Context, wt *Worktree, message string) error {
	changed, err := m.HasChanges(ctx, wt)
	if err != nil || !changed {
		return err
	}
	if _, err := runGit(ctx, wt.Path, "add", "-A"); err != nil {
		return fmt.Errorf("git add: %w", err)
	}
	// Configure a local identity for the commit so we don't depend on a host config.
	_, _ = runGit(ctx, wt.Path, "config", "user.email", "sandcode@local")
	_, _ = runGit(ctx, wt.Path, "config", "user.name", "sandcode")
	if _, err := runGit(ctx, wt.Path, "commit", "-m", message); err != nil {
		// `commit` returns non-zero when there's nothing to commit even after
		// `git add` (e.g. the index already matched HEAD); treat that as benign.
		if changed {
			return fmt.Errorf("git commit: %w", err)
		}
	}
	return nil
}

// MergeToHead fast-forwards or merges the worktree's branch back into the
// source repo's current branch. Returns an error on conflict.
func (*Manager) MergeToHead(ctx context.Context, wt *Worktree) error {
	mu := repoLock(wt.SourceRepo)
	mu.Lock()
	defer mu.Unlock()

	if _, err := runGit(ctx, wt.SourceRepo, "merge", "--no-ff", "--no-edit", wt.Branch); err != nil {
		return fmt.Errorf("git merge: %w", err)
	}
	return nil
}

// Remove tears down the worktree. force removes even when dirty.
func (*Manager) Remove(ctx context.Context, wt *Worktree, force bool) error {
	mu := repoLock(wt.SourceRepo)
	mu.Lock()
	defer mu.Unlock()

	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, wt.Path)
	if _, err := runGit(ctx, wt.SourceRepo, args...); err != nil {
		// fall back to manual cleanup so we don't leave orphaned dirs
		_ = os.RemoveAll(wt.Path)
		return fmt.Errorf("git worktree remove: %w", err)
	}
	return nil
}

// runGit runs a git command in cwd and returns its stdout.
func runGit(ctx context.Context, cwd string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = cwd
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func ensureGitRepo(ctx context.Context, dir string) error {
	out, err := runGit(ctx, dir, "rev-parse", "--is-inside-work-tree")
	if err != nil || !strings.HasPrefix(strings.TrimSpace(out), "true") {
		return errors.New("git: not a git repository (run `git init` first)")
	}
	return nil
}
