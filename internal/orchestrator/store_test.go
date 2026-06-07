package orchestrator

import (
	"context"
	"path/filepath"
	"testing"

	gitm "github.com/felipemarques-rec/sandcode/internal/git"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
	"github.com/felipemarques-rec/sandcode/internal/store"
)

func TestRun_PersistsRunAndEvents(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	db, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	script := `echo "first line"; echo "second line"; echo "third" > out.txt`

	events, await, err := Run(ctx,
		sandbox.NewNoSandboxProvider(),
		&fakeAgent{script: script},
		&noopAuth{},
		RunOptions{
			Prompt:         "do",
			CWD:            repo,
			SandboxImage:   "x",
			SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", "store-test", "0"),
			Strategy:       gitm.StrategyMergeToHead,
			Store:          db,
		},
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for range events {
	}
	res := await()
	if res.Status != "success" {
		t.Fatalf("status=%s err=%v", res.Status, res.Err)
	}

	got, err := db.GetRun(ctx, res.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Status != store.StatusSuccess {
		t.Fatalf("status=%s", got.Status)
	}
	if got.Agent != "fake" || got.Sandbox != "nosandbox" {
		t.Fatalf("metadata: %+v", got)
	}

	evs, err := db.ListEvents(ctx, res.RunID)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(evs) < 2 {
		t.Fatalf("expected ≥2 persisted events, got %d", len(evs))
	}
}
