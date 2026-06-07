package kernel

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/brain"
	"github.com/felipemarques-rec/sandcode/internal/event"
)

// TestProcessEmitsClassifiedAndEnriched verifies that Kernel.Process publishes
// run.classified and run.enriched events when a Bus is configured.
func TestProcessEmitsClassifiedAndEnriched(t *testing.T) {
	t.Parallel()

	br := openTestBrain(t)
	bus := event.NewLocalBus()
	t.Cleanup(func() { _ = bus.Close() })

	rec := newRecorder()
	bus.Subscribe(event.RunClassified, rec.handler())
	bus.Subscribe(event.RunEnriched, rec.handler())
	bus.Subscribe(event.BrainLessonRecalled, rec.handler())

	k := New(br, WithBus(bus))

	res := k.Process(context.Background(), ProcessRequest{
		Prompt: "fix the off-by-one bug in pagination",
		CWD:    t.TempDir(),
		RunID:  "run-test-001",
	})

	if res.Classification.Type == "" {
		t.Fatalf("expected non-empty classification, got %+v", res.Classification)
	}
	if res.EnrichedPrompt == "" || res.EnrichedPrompt == "fix the off-by-one bug in pagination" {
		t.Fatalf("expected enriched prompt to differ from raw, got %q", res.EnrichedPrompt)
	}

	rec.requireType(t, event.RunClassified)
	rec.requireType(t, event.RunEnriched)
	// No lessons stored, so BrainLessonRecalled must NOT fire.
	if rec.has(event.BrainLessonRecalled) {
		t.Fatalf("BrainLessonRecalled fired with empty brain")
	}

	// run.classified payload should carry the classification fields.
	cls := rec.first(event.RunClassified)
	if cls.RunID != "run-test-001" {
		t.Fatalf("RunClassified.RunID = %q, want run-test-001", cls.RunID)
	}
	if cls.CorrelationID != "run-test-001" {
		t.Fatalf("RunClassified.CorrelationID = %q, want run-test-001", cls.CorrelationID)
	}
	var clsPayload struct {
		Type       string `json:"type"`
		Complexity string `json:"complexity"`
	}
	if err := json.Unmarshal(cls.Payload, &clsPayload); err != nil {
		t.Fatalf("unmarshal classified payload: %v", err)
	}
	if clsPayload.Type == "" || clsPayload.Complexity == "" {
		t.Fatalf("RunClassified payload missing fields: %+v", clsPayload)
	}

	// run.enriched payload should report length growth.
	enr := rec.first(event.RunEnriched)
	var enrPayload struct {
		OriginalLen int `json:"original_len"`
		EnrichedLen int `json:"enriched_len"`
		LessonsUsed int `json:"lessons_used"`
	}
	if err := json.Unmarshal(enr.Payload, &enrPayload); err != nil {
		t.Fatalf("unmarshal enriched payload: %v", err)
	}
	if enrPayload.EnrichedLen <= enrPayload.OriginalLen {
		t.Fatalf("expected enriched_len > original_len, got %+v", enrPayload)
	}
}

// TestProcessEmitsLessonRecalledWhenLessonsExist confirms BrainLessonRecalled
// fires (and only fires) when Recall returns at least one lesson.
func TestProcessEmitsLessonRecalledWhenLessonsExist(t *testing.T) {
	t.Parallel()

	br := openTestBrain(t)
	ctx := context.Background()
	if err := br.Store(ctx, brain.Lesson{
		ID:         "lesson-1",
		RunID:      "seed-run",
		Category:   brain.CategorySkill,
		Tags:       []string{"pagination", "bug"},
		Content:    "When fixing pagination off-by-one, always test the empty-list edge",
		Evidence:   "production incident 2026-04",
		Confidence: 0.9,
	}); err != nil {
		t.Fatalf("seed lesson: %v", err)
	}

	bus := event.NewLocalBus()
	t.Cleanup(func() { _ = bus.Close() })
	rec := newRecorder()
	bus.Subscribe(event.BrainLessonRecalled, rec.handler())

	k := New(br, WithBus(bus))
	_ = k.Process(ctx, ProcessRequest{
		Prompt: "fix the pagination off-by-one bug",
		CWD:    t.TempDir(),
		RunID:  "run-recall-001",
	})

	rec.requireType(t, event.BrainLessonRecalled)
	rcl := rec.first(event.BrainLessonRecalled)
	var payload struct {
		Count     int      `json:"count"`
		LessonIDs []string `json:"lesson_ids"`
	}
	if err := json.Unmarshal(rcl.Payload, &payload); err != nil {
		t.Fatalf("unmarshal recall payload: %v", err)
	}
	if payload.Count == 0 {
		t.Fatalf("recall count was 0 despite seeded lesson")
	}
	found := false
	for _, id := range payload.LessonIDs {
		if id == "lesson-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("lesson_ids did not include seeded lesson-1: %v", payload.LessonIDs)
	}
}

// TestProcessWithoutBusIsNoOp confirms that omitting WithBus leaves the
// pipeline fully functional — Process must still return a valid result.
func TestProcessWithoutBusIsNoOp(t *testing.T) {
	t.Parallel()

	br := openTestBrain(t)
	k := New(br) // no WithBus

	res := k.Process(context.Background(), ProcessRequest{
		Prompt: "add a logging statement",
		CWD:    t.TempDir(),
		RunID:  "run-no-bus",
	})
	if res.EnrichedPrompt == "" {
		t.Fatalf("expected non-empty enriched prompt even without bus")
	}
}

// --- test helpers ---

type recorder struct {
	mu     sync.Mutex
	events []event.Event
}

func newRecorder() *recorder { return &recorder{} }

func (r *recorder) handler() event.Handler {
	return func(_ context.Context, ev event.Event) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.events = append(r.events, ev)
		return nil
	}
}

func (r *recorder) has(typ event.Type) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ev := range r.events {
		if ev.Type == typ {
			return true
		}
	}
	return false
}

func (r *recorder) first(typ event.Type) event.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ev := range r.events {
		if ev.Type == typ {
			return ev
		}
	}
	return event.Event{}
}

func (r *recorder) requireType(t *testing.T, typ event.Type) {
	t.Helper()
	if !r.has(typ) {
		t.Fatalf("expected event %q not emitted; got: %v", typ, r.types())
	}
}

func (r *recorder) types() []event.Type {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]event.Type, 0, len(r.events))
	for _, ev := range r.events {
		out = append(out, ev.Type)
	}
	return out
}

func openTestBrain(t *testing.T) brain.Brain {
	t.Helper()
	path := filepath.Join(t.TempDir(), "brain.db")
	b, err := brain.OpenBrain(path)
	if err != nil {
		t.Fatalf("OpenBrain: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}
