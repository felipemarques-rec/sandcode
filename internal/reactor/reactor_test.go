package reactor

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/event"
)

const (
	cmdA event.Type = "test.a"
	cmdB event.Type = "test.b"
	resC event.Type = "test.c" // a result type with no handler
)

// collectBus records every published event in order.
type collectBus struct {
	mu  sync.Mutex
	evs []event.Event
}

func (b *collectBus) Publish(_ context.Context, ev event.Event) error {
	b.mu.Lock()
	b.evs = append(b.evs, ev)
	b.mu.Unlock()
	return nil
}
func (b *collectBus) Subscribe(event.Type, event.Handler) event.Subscription { return nil }
func (b *collectBus) Close() error                                           { return nil }

func types(evs []event.Event) []event.Type {
	out := make([]event.Type, len(evs))
	for i, e := range evs {
		out[i] = e.Type
	}
	return out
}

func eqTypes(a, b []event.Type) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestDispatch_QuiescentNoHandler — a seed whose type has no handler produces a
// single-event log and is still published.
func TestDispatch_QuiescentNoHandler(t *testing.T) {
	bus := &collectBus{}
	r := New(bus)
	seed := event.New(resC, "run1", nil)
	log, err := r.Dispatch(context.Background(), "run1", seed)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !eqTypes(types(log), []event.Type{resC}) {
		t.Fatalf("log = %v, want [%s]", types(log), resC)
	}
	if !eqTypes(types(bus.evs), []event.Type{resC}) {
		t.Fatalf("bus = %v, want [%s]", types(bus.evs), resC)
	}
}

// TestDispatch_Chains — handler for A produces B; handler for B produces a
// result C. The ordered log is [A, B, C].
func TestDispatch_Chains(t *testing.T) {
	bus := &collectBus{}
	r := New(bus)
	r.Register(cmdA, func(_ context.Context, _ event.Event) ([]event.Event, error) {
		return []event.Event{event.New(cmdB, "run1", nil)}, nil
	})
	r.Register(cmdB, func(_ context.Context, _ event.Event) ([]event.Event, error) {
		return []event.Event{event.New(resC, "run1", nil)}, nil
	})
	log, err := r.Dispatch(context.Background(), "run1", event.New(cmdA, "run1", nil))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	want := []event.Type{cmdA, cmdB, resC}
	if !eqTypes(types(log), want) {
		t.Fatalf("log = %v, want %v", types(log), want)
	}
	if !eqTypes(types(bus.evs), want) {
		t.Fatalf("bus = %v, want %v", types(bus.evs), want)
	}
}

// TestDispatch_Deterministic — same seed + handlers ⇒ identical ordered log
// across runs (the replay contract). A handler that emits two events in a fixed
// order must preserve that order.
func TestDispatch_Deterministic(t *testing.T) {
	build := func() *Reactor {
		r := New(nil)
		r.Register(cmdA, func(_ context.Context, _ event.Event) ([]event.Event, error) {
			return []event.Event{event.New(cmdB, "run1", nil), event.New(resC, "run1", nil)}, nil
		})
		r.Register(cmdB, func(_ context.Context, _ event.Event) ([]event.Event, error) {
			return []event.Event{event.New(resC, "run1", nil)}, nil
		})
		return r
	}
	log1, _ := build().Dispatch(context.Background(), "run1", event.New(cmdA, "run1", nil))
	log2, _ := build().Dispatch(context.Background(), "run1", event.New(cmdA, "run1", nil))
	if !eqTypes(types(log1), types(log2)) {
		t.Fatalf("non-deterministic: %v vs %v", types(log1), types(log2))
	}
	// FIFO order: A, then A's two children (B, C), then B's child (C).
	want := []event.Type{cmdA, cmdB, resC, resC}
	if !eqTypes(types(log1), want) {
		t.Fatalf("log = %v, want %v (FIFO)", types(log1), want)
	}
}

// TestDispatch_ErrorStopsLoop — a handler error aborts the loop and returns the
// partial log plus the error; later events are not processed.
func TestDispatch_ErrorStopsLoop(t *testing.T) {
	sentinel := errors.New("boom")
	r := New(nil)
	r.Register(cmdA, func(_ context.Context, _ event.Event) ([]event.Event, error) {
		return nil, sentinel
	})
	log, err := r.Dispatch(context.Background(), "run1", event.New(cmdA, "run1", nil))
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	if !eqTypes(types(log), []event.Type{cmdA}) {
		t.Fatalf("partial log = %v, want [%s]", types(log), cmdA)
	}
}

// TestDispatch_NilBus — a nil bus collects the log without publishing.
func TestDispatch_NilBus(t *testing.T) {
	r := New(nil)
	r.Register(cmdA, func(_ context.Context, _ event.Event) ([]event.Event, error) {
		return []event.Event{event.New(resC, "run1", nil)}, nil
	})
	log, err := r.Dispatch(context.Background(), "run1", event.New(cmdA, "run1", nil))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !eqTypes(types(log), []event.Type{cmdA, resC}) {
		t.Fatalf("log = %v", types(log))
	}
}
