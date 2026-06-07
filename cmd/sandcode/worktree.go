package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

func newWorktreeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "worktree",
		Short: "Inspect and clean up sandcode worktrees",
	}
	cmd.AddCommand(newWorktreeListCmd())
	cmd.AddCommand(newWorktreeCleanCmd())
	return cmd
}

func newWorktreeListCmd() *cobra.Command {
	var cwd string
	c := &cobra.Command{
		Use:   "list",
		Short: "Show sandcode worktrees still on disk",
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd = resolveCWD(cwd)
			wts, err := listSandcodeWorktrees(cmd.Context(), cwd)
			if err != nil {
				return err
			}
			if len(wts) == 0 {
				fmt.Println("(no sandcode worktrees on disk)")
				return nil
			}
			for _, w := range wts {
				fmt.Println(w)
			}
			return nil
		},
	}
	c.Flags().StringVar(&cwd, "cwd", "", "project directory (default: current)")
	return c
}

func newWorktreeCleanCmd() *cobra.Command {
	var cwd string
	var dryRun bool
	c := &cobra.Command{
		Use:   "clean",
		Short: "Remove sandcode worktrees and prune git's worktree records",
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd = resolveCWD(cwd)
			wts, err := listSandcodeWorktrees(cmd.Context(), cwd)
			if err != nil {
				return err
			}
			if len(wts) == 0 {
				fmt.Println("(nothing to clean)")
				return nil
			}
			for _, w := range wts {
				if dryRun {
					fmt.Printf("would remove %s\n", w)
					continue
				}
				if _, err := runGitCleanup(cmd.Context(), cwd, "worktree", "remove", "--force", w); err != nil {
					// Fall back to manual rm — git may have already lost track.
					_ = os.RemoveAll(w)
				}
				fmt.Printf("removed %s\n", w)
			}
			if !dryRun {
				if _, err := runGitCleanup(cmd.Context(), cwd, "worktree", "prune"); err != nil {
					return fmt.Errorf("git worktree prune: %w", err)
				}
				// Also wipe .sandcode/work entirely if it's left dangling.
				_ = os.RemoveAll(filepath.Join(cwd, ".sandcode", "work"))
			}
			return nil
		},
	}
	c.Flags().StringVar(&cwd, "cwd", "", "project directory (default: current)")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "list what would be removed without changing anything")
	return c
}

func resolveCWD(cwd string) string {
	if cwd != "" {
		return cwd
	}
	d, err := os.Getwd()
	if err != nil {
		return "."
	}
	return d
}

// listSandcodeWorktrees returns the absolute paths of worktrees rooted under
// .sandcode/work/. We rely on `git worktree list --porcelain` so we surface
// only worktrees git itself still tracks (plus any orphan dirs).
func listSandcodeWorktrees(ctx context.Context, repo string) ([]string, error) {
	root := filepath.Join(repo, ".sandcode", "work")
	out, err := runGitCleanup(ctx, repo, "worktree", "list", "--porcelain")
	if err != nil {
		// If `git worktree list` fails (not a repo, etc.), still try to scrub
		// dangling dirs.
		return scanWorktreeDir(root), nil
	}
	tracked := []string{}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "worktree ") {
			p := strings.TrimSpace(strings.TrimPrefix(line, "worktree "))
			if strings.HasPrefix(p, root) {
				tracked = append(tracked, p)
			}
		}
	}
	// Add any orphaned dirs git no longer tracks.
	for _, d := range scanWorktreeDir(root) {
		seen := false
		for _, t := range tracked {
			if t == d {
				seen = true
				break
			}
		}
		if !seen {
			tracked = append(tracked, d)
		}
	}
	return tracked, nil
}

func scanWorktreeDir(root string) []string {
	if _, err := os.Stat(root); err != nil {
		return nil
	}
	var out []string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || !info.IsDir() {
			return nil
		}
		// .sandcode/work/<runID>/<slot>/  — depth = 4 below root
		rel, _ := filepath.Rel(root, p)
		depth := strings.Count(rel, string(filepath.Separator)) + 1
		if rel != "." && depth == 2 {
			out = append(out, p)
			return filepath.SkipDir
		}
		return nil
	})
	return out
}

func runGitCleanup(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
