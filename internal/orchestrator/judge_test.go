package orchestrator

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/agent"
	gitm "github.com/felipemarques-rec/sandcode/internal/git"
	"github.com/felipemarques-rec/sandcode/internal/judge"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
	"github.com/felipemarques-rec/sandcode/internal/store"
)

// fakeJudge picks the candidate whose diff is shortest, breaking ties on
// the first arrival. Deterministic and offline so we can exercise the
// merge-winner code path without hitting any LLM.
type fakeJudge struct{ name string }

func (f *fakeJudge) Name() string { return f.name }
func (f *fakeJudge) Rank(_ context.Context, _ string, cands []judge.Candidate) (judge.Ranking, error) {
	winner := cands[0]
	scores := map[string]float64{}
	for _, c := range cands {
		scores[c.RunID] = 1.0 - float64(len(c.Diff))/10000.0
		if len(c.Diff) < len(winner.Diff) {
			winner = c
		}
	}
	return judge.Ranking{
		Winner:    winner.RunID,
		Scores:    scores,
		Rationale: "shortest diff wins",
		Judge:     f.name,
	}, nil
}

func TestParallelRun_WithJudge_PicksWinnerAndMerges(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// alice writes a tiny file; bob writes a bigger one. fakeJudge picks alice.
	agents := []agent.Provider{
		&scriptedAgent{name: "alice", script: `echo "x" > a.txt`},
		&scriptedAgent{name: "bob", script: `printf 'lots of bytes\n%.0s' {1..30} > b.txt`},
	}

	events, await, err := ParallelRun(ctx,
		sandbox.NewNoSandboxProvider(),
		&noopAuth{},
		ParallelOptions{
			Prompt:         "shortest wins",
			CWD:            repo,
			SandboxImage:   "x",
			SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", "j", "0"),
			Strategy:       gitm.StrategyMergeToHead, // forces winner-merge
			Agents:         agents,
			Store:          db,
			Judge:          &fakeJudge{name: "fake"},
		},
	)
	if err != nil {
		t.Fatalf("ParallelRun: %v", err)
	}
	for range events {
	}
	pr := await()

	if pr.Ranking == nil {
		t.Fatal("expected ranking")
	}
	if pr.WinnerErr != nil {
		t.Fatalf("winner-merge failed: %v", pr.WinnerErr)
	}

	// alice's file must be on main; bob's must NOT be.
	if _, err := stat(filepath.Join(repo, "a.txt")); err != nil {
		t.Fatalf("a.txt should be merged into main: %v", err)
	}
	if _, err := stat(filepath.Join(repo, "b.txt")); err == nil {
		t.Fatal("b.txt should NOT be on main")
	}

	// Ranking persisted
	rk, err := db.GetRanking(ctx, pr.ParentRunID)
	if err != nil {
		t.Fatalf("GetRanking: %v", err)
	}
	if rk.WinnerRunID != pr.Ranking.Winner {
		t.Fatalf("winner mismatch: %s vs %s", rk.WinnerRunID, pr.Ranking.Winner)
	}
	if time.Since(rk.CreatedAt) > time.Minute {
		t.Fatalf("ranking created_at suspicious: %v", rk.CreatedAt)
	}
}

func stat(p string) (any, error) {
	// thin wrapper so the test doesn't need to import os twice; signature
	// matches what the test cares about.
	return nil, statFile(p)
}
