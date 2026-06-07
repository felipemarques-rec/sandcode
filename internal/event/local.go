package event

import (
	"context"
	"log/slog"
	"sync"
)

// LocalBus is an in-process event bus using Go channels. It is the
// Phase 1 implementation — zero external dependencies, sufficient for
// single-process sandcode. Subscribers are invoked synchronously in the
// publisher's goroutine for simplicity and determinism.
type LocalBus struct {
	mu   sync.RWMutex
	subs map[Type][]subEntry
	all  []subEntry // wildcard subscribers
	seq  uint64
}

type subEntry struct {
	id      uint64
	handler Handler
}

// NewLocalBus creates an in-process event bus.
func NewLocalBus() *LocalBus {
	return &LocalBus{
		subs: make(map[Type][]subEntry),
	}
}

func (b *LocalBus) Publish(ctx context.Context, ev Event) error {
	b.mu.RLock()
	// Collect handlers under read lock; invoke outside to avoid deadlock.
	handlers := make([]Handler, 0, len(b.subs[ev.Type])+len(b.all))
	for _, s := range b.subs[ev.Type] {
		handlers = append(handlers, s.handler)
	}
	for _, s := range b.all {
		handlers = append(handlers, s.handler)
	}
	b.mu.RUnlock()

	for _, h := range handlers {
		if err := h(ctx, ev); err != nil {
			// Subscriber errors are logged but never propagated.
			slog.Default().Warn("event subscriber error",
				"event_type", string(ev.Type),
				"event_id", ev.ID,
				"error", err,
			)
		}
	}
	return nil
}

func (b *LocalBus) Subscribe(typ Type, handler Handler) Subscription {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.seq++
	entry := subEntry{id: b.seq, handler: handler}

	if typ == "*" {
		b.all = append(b.all, entry)
	} else {
		b.subs[typ] = append(b.subs[typ], entry)
	}

	return &localSub{bus: b, typ: typ, id: entry.id}
}

func (b *LocalBus) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs = make(map[Type][]subEntry)
	b.all = nil
	return nil
}

// removeSub removes a subscription by ID.
func (b *LocalBus) removeSub(typ Type, id uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if typ == "*" {
		b.all = removeByID(b.all, id)
		return
	}
	if entries, ok := b.subs[typ]; ok {
		b.subs[typ] = removeByID(entries, id)
	}
}

func removeByID(entries []subEntry, id uint64) []subEntry {
	for i, e := range entries {
		if e.id == id {
			return append(entries[:i], entries[i+1:]...)
		}
	}
	return entries
}

type localSub struct {
	bus  *LocalBus
	typ  Type
	id   uint64
	once sync.Once
}

func (s *localSub) Cancel() {
	s.once.Do(func() {
		s.bus.removeSub(s.typ, s.id)
	})
}
