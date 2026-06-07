package event

import "context"

// Handler processes a single event. Returning an error stops delivery
// for that handler but does not affect other subscribers.
type Handler func(ctx context.Context, event Event) error

// Subscription represents an active subscription that can be cancelled.
type Subscription interface {
	// Cancel stops event delivery for this subscription.
	Cancel()
}

// Bus is the event distribution abstraction. Phase 1 uses an in-process
// implementation (LocalBus); future phases may use NATS or similar.
type Bus interface {
	// Publish emits an event to all matching subscribers.
	// Publishing is best-effort: subscriber errors are logged but do not
	// propagate back to the publisher.
	Publish(ctx context.Context, event Event) error

	// Subscribe registers a handler for events matching the given type.
	// Use "*" to subscribe to all event types.
	Subscribe(typ Type, handler Handler) Subscription

	// Close shuts down the bus and cancels all subscriptions.
	Close() error
}
