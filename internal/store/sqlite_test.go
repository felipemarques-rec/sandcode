package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTemp(t *testing.T) *SQLite {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestRunRoundTrip(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()

	now := time.Now()
	r := Run{
		ID:        "abc12345",
		Agent:     "claude-code",
		Sandbox:   "docker",
		Prompt:    "do thing",
		CWD:       "/tmp/x",
		Strategy:  "merge-to-head",
		Status:    StatusRunning,
		StartedAt: now,
	}
	if err := db.CreateRun(ctx, r); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	got, err := db.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Agent != "claude-code" || got.Status != StatusRunning {
		t.Fatalf("got=%+v", got)
	}

	// finish
	r.Status = StatusSuccess
	r.FinishedAt = now.Add(time.Second)
	r.ExitCode = 0
	if err := db.UpdateRun(ctx, r); err != nil {
		t.Fatalf("UpdateRun: %v", err)
	}
	got, _ = db.GetRun(ctx, r.ID)
	if got.Status != StatusSuccess || got.FinishedAt.IsZero() {
		t.Fatalf("update missed: %+v", got)
	}
}

func TestEventsRoundTrip(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	if err := db.CreateRun(ctx, Run{ID: "r1", Agent: "claude", Sandbox: "noop", Prompt: "p", CWD: "/x", Status: StatusRunning, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := db.AppendEvent(ctx, "r1", Event{
			Seq:       int64(i),
			Timestamp: time.Now(),
			Kind:      "text",
			Payload:   `{"text":"hi"}`,
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	events, err := db.ListEvents(ctx, "r1")
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	for i, e := range events {
		if e.Seq != int64(i) {
			t.Fatalf("seq[%d]=%d", i, e.Seq)
		}
	}
}

func TestListRunsFilters(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	now := time.Now()
	mk := func(id, agent, parent string, st RunStatus, off time.Duration) {
		if err := db.CreateRun(ctx, Run{
			ID: id, Agent: agent, Sandbox: "docker", Prompt: "p", CWD: "/x",
			Status: st, ParentID: parent, StartedAt: now.Add(off),
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk("a1", "claude-code", "", StatusSuccess, 0)
	mk("a2", "codex", "", StatusFailure, time.Millisecond)
	mk("a3", "claude-code", "a1", StatusSuccess, 2*time.Millisecond) // sub-run

	// default: no parent — top-level only
	got, err := db.ListRuns(ctx, ListFilter{Limit: 10})
	if err != nil || len(got) != 2 {
		t.Fatalf("top-level: %d %v", len(got), err)
	}
	// status filter
	got, _ = db.ListRuns(ctx, ListFilter{Status: StatusFailure})
	if len(got) != 1 || got[0].ID != "a2" {
		t.Fatalf("status filter: %+v", got)
	}
	// agent filter
	got, _ = db.ListRuns(ctx, ListFilter{Agent: "claude-code", ParentID: "*"})
	if len(got) != 2 {
		t.Fatalf("agent filter: %d", len(got))
	}
}

func TestGetRunNotFound(t *testing.T) {
	db := openTemp(t)
	if _, err := db.GetRun(context.Background(), "missing"); err == nil {
		t.Fatal("expected not-found error")
	}
}
