package orchestrator

import (
	"context"
	"path/filepath"
	"testing"

	gitm "github.com/felipemarques-rec/sandcode/internal/git"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
)

func TestNewRunID(t *testing.T) {
	a, b := NewRunID(), NewRunID()
	if a == "" || len(a) != 8 {
		t.Errorf("NewRunID = %q, want 8-char string", a)
	}
	if a == b {
		t.Errorf("two consecutive NewRunIDs collided: %q", a)
	}
}

// TestRun_HonorsCallerSuppliedRunID asserts that a non-empty
// RunOptions.RunID is preserved through to the final Result.
func TestRun_HonorsCallerSuppliedRunID(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	want := "deadbeef" // any 8-char-or-other valid string
	events, await, err := Run(ctx,
		sandbox.NewNoSandboxProvider(),
		&fakeAgent{script: `echo ok`},
		&noopAuth{},
		RunOptions{
			Prompt:         "noop",
			CWD:            repo,
			SandboxImage:   "ignored",
			SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", "runid", "0"),
			Strategy:       gitm.StrategyMergeToHead,
			RunID:          want,
		},
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for range events {
	}
	res := await()
	if res.RunID != want {
		t.Errorf("Result.RunID = %q, want %q", res.RunID, want)
	}
}
