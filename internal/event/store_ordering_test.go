package event

import (
	"context"
	"testing"
	"time"
)

// TestStore_LoadRun_OrdersByTimestampAsc asserts LoadRun returns events
// in chronological order even when they are appended out-of-order.
func TestStore_LoadRun_OrdersByTimestampAsc(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	start := time.Unix(1_700_000_000, 0)
	// Append in REVERSE timestamp order to confirm SQL ORDER BY wins.
	events := []Event{
		newEvent(RunCompleted, "run-1", start.Add(10*time.Second), nil),
		newEvent(AgentCompleted, "run-1", start.Add(8*time.Second), nil),
		newEvent(AgentExecuting, "run-1", start.Add(time.Second), nil),
		newEvent(SandboxCreated, "run-1", start.Add(500*time.Millisecond), nil),
		newEvent(RunEnriched, "run-1", start.Add(120*time.Millisecond), nil),
		newEvent(RunClassified, "run-1", start.Add(50*time.Millisecond), nil),
		newEvent(RunSubmitted, "run-1", start, nil),
	}
	for _, ev := range events {
		if err := s.Append(ctx, ev); err != nil {
			t.Fatalf("Append(%s): %v", ev.Type, err)
		}
	}

	got, err := s.LoadRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	wantOrder := []Type{
		RunSubmitted, RunClassified, RunEnriched, SandboxCreated,
		AgentExecuting, AgentCompleted, RunCompleted,
	}
	if len(got) != len(wantOrder) {
		t.Fatalf("LoadRun returned %d events, want %d", len(got), len(wantOrder))
	}
	for i, want := range wantOrder {
		if got[i].Type != want {
			t.Fatalf("position %d: got=%s want=%s", i, got[i].Type, want)
		}
	}
}

// TestStore_LoadRun_TiesBrokenByRowid verifies that events appended in
// the same nanosecond are returned in insertion order — replay safety
// depends on this when wall-clocks have insufficient resolution.
func TestStore_LoadRun_TiesBrokenByRowid(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	t0 := time.Unix(1_700_000_000, 0)
	ids := []string{"a", "b", "c", "d"}
	for _, id := range ids {
		ev := newEvent(SandboxDestroyed, "run-tie", t0, nil)
		ev.ID = id
		if err := s.Append(ctx, ev); err != nil {
			t.Fatalf("Append %s: %v", id, err)
		}
	}

	got, err := s.LoadRun(ctx, "run-tie")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	for i, id := range ids {
		if got[i].ID != id {
			t.Fatalf("position %d: got=%s want=%s", i, got[i].ID, id)
		}
	}
}

// TestStore_LoadRun_IsolatesByRunID asserts events from different runs
// don't leak into each other's LoadRun results.
func TestStore_LoadRun_IsolatesByRunID(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	for i, run := range []string{"run-A", "run-B"} {
		for j := 0; j < 3; j++ {
			ev := newEvent(RunSubmitted, run, time.Unix(int64(1_700_000_000+i*100+j), 0), nil)
			if err := s.Append(ctx, ev); err != nil {
				t.Fatalf("Append: %v", err)
			}
		}
	}

	a, _ := s.LoadRun(ctx, "run-A")
	b, _ := s.LoadRun(ctx, "run-B")
	if len(a) != 3 || len(b) != 3 {
		t.Fatalf("isolation broken: len(a)=%d len(b)=%d", len(a), len(b))
	}
	for _, ev := range a {
		if ev.RunID != "run-A" {
			t.Fatalf("run-A query returned RunID=%s", ev.RunID)
		}
	}
	for _, ev := range b {
		if ev.RunID != "run-B" {
			t.Fatalf("run-B query returned RunID=%s", ev.RunID)
		}
	}
}

// TestStore_LoadSince_FiltersByTimestamp verifies the time-window query.
func TestStore_LoadSince_FiltersByTimestamp(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	start := time.Unix(1_700_000_000, 0)
	for i := 0; i < 6; i++ {
		ev := newEvent(RunSubmitted, "run-x", start.Add(time.Duration(i)*time.Second), nil)
		if err := s.Append(ctx, ev); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	// 3s cutoff should return events at t+3, t+4, t+5 (3 events).
	cutoff := start.Add(3 * time.Second)
	got, err := s.LoadSince(ctx, cutoff, 0)
	if err != nil {
		t.Fatalf("LoadSince: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("LoadSince returned %d events, want 3", len(got))
	}
	for _, ev := range got {
		if ev.Timestamp.Before(cutoff) {
			t.Fatalf("LoadSince returned event before cutoff: %s < %s", ev.Timestamp, cutoff)
		}
	}
}

// TestStore_LoadSince_RespectsLimit checks the LIMIT clause.
func TestStore_LoadSince_RespectsLimit(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	start := time.Unix(1_700_000_000, 0)
	for i := 0; i < 10; i++ {
		ev := newEvent(RunSubmitted, "run-y", start.Add(time.Duration(i)*time.Second), nil)
		if err := s.Append(ctx, ev); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	got, err := s.LoadSince(ctx, start, 4)
	if err != nil {
		t.Fatalf("LoadSince: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("LoadSince(limit=4) returned %d events, want 4", len(got))
	}
}
