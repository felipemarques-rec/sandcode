// Package reactor implements the deterministic reactive foundation for
// bus-mediated component coordination (SP3.0).
//
// The reactor inverts direct component calls into event reactions WITHOUT
// sacrificing determinism: it owns a per-run FIFO queue drained by a single
// goroutine (the caller's, inside Dispatch), so events are processed one at a
// time in a fixed order. Handlers registered for a command event type produce
// further events that are enqueued in order. Every processed event is also
// published to the supplied event.Bus so the existing observers (persistence,
// SSE, metrics) see the full reactive log — the LocalBus is unchanged.
//
// Determinism contract: given the same seed event and the same registered
// handlers, Dispatch processes the same events in the same order. This is what
// makes a reactive run replay-identical, the property SP3 must preserve as the
// pipeline is progressively inverted (SP3.1+).
package reactor

import (
	"context"

	"github.com/felipemarques-rec/sandcode/internal/event"
)

// Handler processes a command event and returns the events it produces
// (results and/or further commands), in deterministic order. A handler MUST be
// deterministic for replay to hold. Returning an error aborts the dispatch
// loop; the partial ordered log is still returned to the caller.
type Handler func(ctx context.Context, cmd event.Event) ([]event.Event, error)

// Reactor routes command events to registered handlers and serializes the
// resulting event flow per Dispatch call. It is safe to Register before any
// Dispatch; concurrent Dispatch calls on a shared Reactor each run their own
// FIFO queue but share the (read-only after setup) handler map — callers that
// need per-run handler state should use one Reactor per run.
type Reactor struct {
	bus      event.Bus
	handlers map[event.Type]Handler
}

// New returns a Reactor that publishes every processed event to bus. bus may be
// nil (events are then only collected into the returned log, not published).
func New(bus event.Bus) *Reactor {
	return &Reactor{bus: bus, handlers: make(map[event.Type]Handler)}
}

// Register binds a handler to a command event type. A later Register for the
// same type replaces the earlier handler.
func (r *Reactor) Register(cmdType event.Type, h Handler) {
	r.handlers[cmdType] = h
}

// Dispatch runs the per-run serialized reaction loop. It seeds a FIFO queue
// with seed, then drains it in this goroutine: each dequeued event is appended
// to the ordered log and published to the bus; if a handler is registered for
// the event's type it is invoked and its produced events are enqueued in order.
// The loop ends when the queue is empty (quiescent). Returns the ordered log of
// every processed event. If a handler errors, the loop stops and returns the
// log so far plus the error.
//
// runID is accepted for symmetry with the rest of the pipeline and forward
// compatibility (per-run routing once the bus is distributed); the in-process
// loop is already isolated per Dispatch call.
func (r *Reactor) Dispatch(ctx context.Context, runID string, seed event.Event) ([]event.Event, error) {
	queue := []event.Event{seed}
	log := make([]event.Event, 0, 4)
	for len(queue) > 0 {
		ev := queue[0]
		queue = queue[1:]
		log = append(log, ev)

		// Publish so the existing bus observers see the reactive event. Bus
		// publishing is best-effort by contract — a subscriber error never
		// aborts the reaction loop.
		if r.bus != nil {
			_ = r.bus.Publish(ctx, ev)
		}

		h, ok := r.handlers[ev.Type]
		if !ok {
			continue // a result event with no handler: just observed, no work
		}
		produced, err := h(ctx, ev)
		if err != nil {
			return log, err
		}
		queue = append(queue, produced...)
	}
	return log, nil
}
