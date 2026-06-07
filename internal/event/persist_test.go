package event

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestPersistTo_RoundTrip publishes events via the bus and asserts the
// store sees them in order.
func TestPersistTo_RoundTrip(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	bus := NewLocalBus()
	t.Cleanup(func() { _ = bus.Close() })

	sub := PersistTo(bus, s)
	t.Cleanup(sub.Cancel)

	ctx := context.Background()
	start := time.Unix(1_700_000_000, 0)
	events := []Event{
		newEvent(RunSubmitted, "run-1", start, nil),
		newEvent(RunClassified, "run-1", start.Add(50*time.Millisecond), []byte(`{"type":"convergent"}`)),
		newEvent(RunCompleted, "run-1", start.Add(time.Second), nil),
	}
	for _, ev := range events {
		if err := bus.Publish(ctx, ev); err != nil {
			t.Fatalf("Publish(%s): %v", ev.Type, err)
		}
	}

	got, err := s.LoadRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if len(got) != len(events) {
		t.Fatalf("got %d events from store, want %d", len(got), len(events))
	}
	for i, want := range events {
		if got[i].Type != want.Type {
			t.Fatalf("position %d: got=%s want=%s", i, got[i].Type, want.Type)
		}
		if got[i].ID != want.ID {
			t.Fatalf("position %d ID mismatch", i)
		}
	}
}

// TestPersistTo_CancelStopsPersistence asserts Subscription.Cancel halts
// the bridge — events published after Cancel must NOT land in the store.
func TestPersistTo_CancelStopsPersistence(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	bus := NewLocalBus()
	t.Cleanup(func() { _ = bus.Close() })

	sub := PersistTo(bus, s)

	ctx := context.Background()
	if err := bus.Publish(ctx, newEvent(RunSubmitted, "run-1", time.Now(), nil)); err != nil {
		t.Fatal(err)
	}
	sub.Cancel()
	if err := bus.Publish(ctx, newEvent(RunCompleted, "run-1", time.Now(), nil)); err != nil {
		t.Fatal(err)
	}

	got, _ := s.LoadRun(ctx, "run-1")
	if len(got) != 1 {
		t.Fatalf("after Cancel, store has %d events, want 1", len(got))
	}
	if got[0].Type != RunSubmitted {
		t.Fatalf("retained event = %s, want run.submitted", got[0].Type)
	}
}

// TestPersistTo_StoreFailureDoesNotBreakBus simulates a closed store
// and verifies publishes still succeed (subscriber errors swallowed
// per LocalBus contract).
func TestPersistTo_StoreFailureDoesNotBreakBus(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	bus := NewLocalBus()
	t.Cleanup(func() { _ = bus.Close() })

	sub := PersistTo(bus, s)
	t.Cleanup(sub.Cancel)

	_ = s.Close() // force every Append to fail

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		ev := newEvent(RunSubmitted, "run-broken", time.Now(), nil)
		// Publish MUST NOT return an error even when the store is dead.
		if err := bus.Publish(ctx, ev); err != nil {
			t.Fatalf("Publish failed on broken store (should be swallowed): %v", err)
		}
	}
}

// TestPersistTo_HandlesConcurrentPublishes hammers the bus from multiple
// goroutines and verifies the store ends with the expected number of rows.
// Catches races between Publish and the underlying *sql.DB.
func TestPersistTo_HandlesConcurrentPublishes(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	bus := NewLocalBus()
	t.Cleanup(func() { _ = bus.Close() })

	sub := PersistTo(bus, s)
	t.Cleanup(sub.Cancel)

	const workers = 8
	const each = 25
	done := make(chan error, workers)
	ctx := context.Background()
	for w := 0; w < workers; w++ {
		w := w
		go func() {
			var err error
			for i := 0; i < each; i++ {
				ev := New(SandboxDestroyed, fakeRun(w, i), nil)
				ev.Timestamp = time.Now()
				if e := bus.Publish(ctx, ev); e != nil {
					err = e
					break
				}
			}
			done <- err
		}()
	}
	for i := 0; i < workers; i++ {
		if err := <-done; err != nil {
			t.Fatalf("worker error: %v", err)
		}
	}
	n, err := s.Count(ctx)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != workers*each {
		t.Fatalf("Count = %d, want %d", n, workers*each)
	}
}

func fakeRun(w, i int) string {
	return rune2s('A'+w) + "-" + rune2s('0'+i%10) + rune2s('0'+(i/10)%10)
}

func rune2s(r int) string { return string(rune(r)) }

// (re-export check) — ensures PersistTo accepts any Bus impl, not just *LocalBus.
var _ = func() Subscription {
	var b Bus = NewLocalBus()
	var s Store = (*SQLiteStore)(nil)
	return PersistTo(b, s)
}

// guard import in case future test bodies need errors.Is.
var _ = errors.Is
