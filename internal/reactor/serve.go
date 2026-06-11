package reactor

import (
	"context"

	"github.com/felipemarques-rec/sandcode/internal/event"
)

// Serve and RequestReply (SP3.3) are the distribution seam: they let a reactor
// handler run as a BUS SUBSCRIBER (relocatable to any process subscribed to the
// bus) instead of an in-process Register'd handler, with the dispatcher awaiting
// the result over the bus. In-process the LocalBus delivers synchronously; in
// M6 the same code runs over NATS request/reply unchanged. Commands and results
// are correlated by CausationID (result.CausationID == command.ID), so multiple
// in-flight requests never cross wires.

// ServeHandler processes a command event and returns the single result event to
// publish back. It may run in any process subscribed to the bus.
type ServeHandler func(ctx context.Context, cmd event.Event) (event.Event, error)

// Serve registers h as a bus subscriber for cmdType. On each command it runs h
// and publishes the result with CausationID set to the command's ID (and RunID /
// CorrelationID defaulted from the command when the handler left them empty).
// Returns the Subscription so the caller can Cancel it. A handler error produces
// no reply — a RequestReply dispatcher then surfaces it as a context timeout.
func Serve(bus event.Bus, cmdType event.Type, h ServeHandler) event.Subscription {
	return bus.Subscribe(cmdType, func(ctx context.Context, cmd event.Event) error {
		res, err := h(ctx, cmd)
		if err != nil {
			return err
		}
		res.CausationID = cmd.ID
		if res.RunID == "" {
			res.RunID = cmd.RunID
		}
		if res.CorrelationID == "" {
			res.CorrelationID = cmd.CorrelationID
		}
		return bus.Publish(ctx, res)
	})
}

// RequestReply publishes cmd and blocks until a result event of resultType whose
// CausationID matches cmd.ID arrives on the bus, or ctx is done. It is the
// dispatch side of a Serve'd handler. cmd.ID must be set (event.New sets it).
// Pass a ctx with a deadline so a missing/erroring handler surfaces as a timeout
// rather than a hang.
func RequestReply(ctx context.Context, bus event.Bus, cmd event.Event, resultType event.Type) (event.Event, error) {
	replyCh := make(chan event.Event, 1)
	sub := bus.Subscribe(resultType, func(_ context.Context, ev event.Event) error {
		if ev.CausationID == cmd.ID {
			select {
			case replyCh <- ev:
			default: // a reply is already buffered; ignore duplicates
			}
		}
		return nil
	})
	defer sub.Cancel()

	if err := bus.Publish(ctx, cmd); err != nil {
		return event.Event{}, err
	}
	select {
	case ev := <-replyCh:
		return ev, nil
	case <-ctx.Done():
		return event.Event{}, ctx.Err()
	}
}
