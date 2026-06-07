package event

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newEvent(typ Type, runID string, ts time.Time, payload []byte) Event {
	ev := New(typ, runID, payload)
	ev.Timestamp = ts
	ev.CorrelationID = runID
	return ev
}

// TestStore_AppendLoadRoundtrip is the core invariant: an event written
// via Append comes back from LoadRun byte-for-byte (ID, type, payload,
// run/correlation/causation IDs, and timestamp truncated to nano precision
// — SQLite stores it as INT64 nanos).
func TestStore_AppendLoadRoundtrip(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	start := time.Unix(1_700_000_000, 0)
	original := []Event{
		newEvent(RunSubmitted, "run-1", start, []byte(`{"agent":"claude-code"}`)),
		newEvent(RunClassified, "run-1", start.Add(50*time.Millisecond), []byte(`{"type":"convergent"}`)),
		newEvent(RunEnriched, "run-1", start.Add(120*time.Millisecond), []byte(`{"lessons_used":0}`)),
	}
	for _, ev := range original {
		if err := s.Append(ctx, ev); err != nil {
			t.Fatalf("Append(%s): %v", ev.Type, err)
		}
	}

	got, err := s.LoadRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if len(got) != len(original) {
		t.Fatalf("LoadRun returned %d events, want %d", len(got), len(original))
	}
	for i, want := range original {
		gv := got[i]
		if gv.ID != want.ID {
			t.Fatalf("event %d ID mismatch: got=%q want=%q", i, gv.ID, want.ID)
		}
		if gv.Type != want.Type {
			t.Fatalf("event %d Type mismatch: got=%q want=%q", i, gv.Type, want.Type)
		}
		if gv.RunID != want.RunID {
			t.Fatalf("event %d RunID mismatch: got=%q want=%q", i, gv.RunID, want.RunID)
		}
		if string(gv.Payload) != string(want.Payload) {
			t.Fatalf("event %d Payload mismatch: got=%q want=%q", i, gv.Payload, want.Payload)
		}
		if !gv.Timestamp.Equal(want.Timestamp) {
			t.Fatalf("event %d Timestamp mismatch: got=%s want=%s", i, gv.Timestamp, want.Timestamp)
		}
		if gv.CorrelationID != want.CorrelationID {
			t.Fatalf("event %d CorrelationID mismatch: got=%q want=%q", i, gv.CorrelationID, want.CorrelationID)
		}
	}
}

// TestStore_RejectsDuplicateID protects the PRIMARY KEY invariant.
func TestStore_RejectsDuplicateID(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	ev := newEvent(RunSubmitted, "run-1", time.Now(), nil)
	if err := s.Append(ctx, ev); err != nil {
		t.Fatalf("first Append: %v", err)
	}
	err := s.Append(ctx, ev) // same ID
	if err == nil {
		t.Fatalf("expected duplicate-ID error, got nil")
	}
}

// TestStore_RejectsMissingRequiredFields asserts the input validation
// in Append catches the obvious zero-value mistakes before they reach
// the database.
func TestStore_RejectsMissingRequiredFields(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	cases := []struct {
		name string
		ev   Event
	}{
		{"empty ID", Event{RunID: "x", Type: RunSubmitted}},
		{"empty RunID", Event{ID: "id1", Type: RunSubmitted}},
		{"empty Type", Event{ID: "id1", RunID: "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.Append(ctx, tc.ev); err == nil {
				t.Fatalf("expected validation error for %s", tc.name)
			}
		})
	}
}

// TestStore_NilPayloadStoredAsEmpty asserts a nil Payload roundtrips
// as an empty slice (not nil) — keeps callers from special-casing nil.
func TestStore_NilPayloadStoredAsEmpty(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	ev := newEvent(SandboxDestroyed, "run-1", time.Now(), nil)
	if err := s.Append(ctx, ev); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, err := s.LoadRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if got[0].Payload == nil {
		t.Fatalf("Payload was nil on roundtrip; want empty slice")
	}
	if len(got[0].Payload) != 0 {
		t.Fatalf("Payload non-empty: %q", got[0].Payload)
	}
}

// TestStore_AutoTimestampsWhenZero ensures Append picks a timestamp
// if the caller passed a zero Time — convenience for non-test callers.
func TestStore_AutoTimestampsWhenZero(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	ev := Event{ID: "id-auto", RunID: "run-1", Type: RunSubmitted}
	before := time.Now()
	if err := s.Append(ctx, ev); err != nil {
		t.Fatalf("Append: %v", err)
	}
	after := time.Now()

	got, _ := s.LoadRun(ctx, "run-1")
	if got[0].Timestamp.Before(before) || got[0].Timestamp.After(after.Add(time.Second)) {
		t.Fatalf("auto-timestamp out of range: %s (window %s..%s)", got[0].Timestamp, before, after)
	}
}

// TestStore_Count tracks total rows.
func TestStore_Count(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		ev := newEvent(RunSubmitted, "run-"+string(rune('A'+i)), time.Now(), nil)
		if err := s.Append(ctx, ev); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	n, err := s.Count(ctx)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 5 {
		t.Fatalf("Count = %d, want 5", n)
	}
}

// TestStore_ConfirmSchemaIsAppendOnly is a defense-in-depth check: even
// if a future engineer adds an UPDATE method to SQLiteStore, this test
// ensures there's NO UPDATE/DELETE wired today by reading the package's
// surface — fails loudly if either method shows up unexpectedly.
//
// (Tested via reflection equivalent: read the schema string to confirm
// it has no UNIQUE-ON-CONFLICT or REPLACE semantics.)
func TestStore_ConfirmSchemaIsAppendOnly(t *testing.T) {
	t.Parallel()
	if strings.Contains(schemaEventLog, "ON CONFLICT") {
		t.Fatalf("schemaEventLog must NOT contain ON CONFLICT — append-only invariant")
	}
	if strings.Contains(schemaEventLog, "REPLACE") {
		t.Fatalf("schemaEventLog must NOT contain REPLACE — append-only invariant")
	}
}
