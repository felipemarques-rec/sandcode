package event

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEvent_WithTrace_SetsField(t *testing.T) {
	ev := New(RunClassified, "run-1", nil).WithTrace("abc123")
	if ev.TraceID != "abc123" {
		t.Fatalf("TraceID = %q, want abc123", ev.TraceID)
	}
}

func TestEvent_WithTrace_EmptyIsOmitted(t *testing.T) {
	ev := New(RunClassified, "run-1", nil)
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "trace_id") {
		t.Fatalf("empty TraceID must be omitted, got %s", b)
	}
}

func TestEvent_WithTrace_PresentWhenSet(t *testing.T) {
	b, _ := json.Marshal(New(RunClassified, "run-1", nil).WithTrace("t-1"))
	if !strings.Contains(string(b), `"trace_id":"t-1"`) {
		t.Fatalf("expected trace_id in JSON, got %s", b)
	}
}

func TestEvent_WithTrace_Immutable(t *testing.T) {
	a := New(RunClassified, "run-1", nil)
	b := a.WithTrace("x")
	if a.TraceID != "" {
		t.Fatalf("WithTrace mutated receiver: %q", a.TraceID)
	}
	if b.TraceID != "x" {
		t.Fatalf("copy missing TraceID")
	}
}
