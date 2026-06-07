package event

import (
	"context"
	"log/slog"
)

// PersistTo subscribes a wildcard handler to bus that appends every event
// to store. Returns the Subscription so the caller can Cancel() it on
// shutdown.
//
// Persistence failures are logged via slog.Default() and never propagated:
// observability must never block the run, and the LocalBus contract
// already promises that subscriber errors don't affect publishers.
//
// Usage:
//
//	store, err := event.OpenStore(path)
//	if err != nil { … }
//	defer store.Close()
//	sub := event.PersistTo(bus, store)
//	defer sub.Cancel()
//	// … run sandcode … every published event is now durably appended.
func PersistTo(bus Bus, store Store) Subscription {
	return bus.Subscribe("*", func(ctx context.Context, ev Event) error {
		if err := store.Append(ctx, ev); err != nil {
			slog.Default().Error("event store: persist failed",
				"error", err,
				"event_id", ev.ID,
				"event_type", string(ev.Type),
				"run_id", ev.RunID,
			)
		}
		return nil // never propagate — see LocalBus.Publish for contract
	})
}
