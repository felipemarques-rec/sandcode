// Package server hosts the HTTP API surface for sandcode.
//
// The package is intentionally thin: routes, request decoding, response
// encoding, and a small in-memory cache of per-run state machines.
// Business logic lives in the orchestrator/runtime/governance packages
// that the server composes.
package server

import (
	"context"
	"errors"
	"sync"

	"github.com/felipemarques-rec/sandcode/internal/event"
	"github.com/felipemarques-rec/sandcode/internal/runtime"
)

// ErrUnknownRun is returned by StateCache.Get when no state machine has
// been observed for the given run ID.
var ErrUnknownRun = errors.New("server: unknown run id")

// DefaultStateCacheCapacity is the LRU-style cap used when capacity is
// omitted at construction. The HTTP server holds one ExecutionState per
// live run; 1024 is well above realistic in-flight concurrency on a
// single node and bounded enough that runaway clients cannot OOM us.
const DefaultStateCacheCapacity = 1024

// StateCache keeps a deterministic ExecutionState per run ID. It is
// fed by an event-bus wildcard subscription (Attach) and surfaces the
// state to HTTP handlers via Get/List.
//
// Eviction policy: bounded insertion order. When Apply pushes a new
// run past capacity, the oldest entry is dropped — preferring entries
// already in a terminal phase if any exist.
//
// All public methods are safe for concurrent use.
type StateCache struct {
	mu       sync.RWMutex
	states   map[string]*runtime.ExecutionState
	order    []string // FIFO of insertion order for eviction
	capacity int
}

// NewStateCache constructs a cache with the given capacity. capacity <= 0
// falls back to DefaultStateCacheCapacity.
func NewStateCache(capacity int) *StateCache {
	if capacity <= 0 {
		capacity = DefaultStateCacheCapacity
	}
	return &StateCache{
		states:   make(map[string]*runtime.ExecutionState),
		capacity: capacity,
	}
}

// Attach subscribes the cache to bus and returns the subscription so
// the caller can Cancel() it on shutdown. The handler is a no-op for
// events with no RunID (system events).
func (c *StateCache) Attach(bus event.Bus) event.Subscription {
	return bus.Subscribe("*", func(_ context.Context, ev event.Event) error {
		if ev.RunID == "" {
			return nil
		}
		c.apply(ev)
		return nil
	})
}

// apply is the cache's single mutator. Exposed package-private so tests
// can drive it without a bus.
func (c *StateCache) apply(ev event.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()

	st, ok := c.states[ev.RunID]
	if !ok {
		st = runtime.NewExecutionState(ev.RunID)
		c.states[ev.RunID] = st
		c.order = append(c.order, ev.RunID)
		c.evictIfFull()
	}
	// runtime.Apply rejects events on a terminal state. We silently
	// swallow that — the cache is a projection, not the authority on
	// what's "valid". The event store still holds the full audit log.
	_ = st.Apply(ev)
}

// evictIfFull drops the oldest entry when over capacity, preferring
// terminal entries to keep in-flight runs queryable. Caller must hold
// c.mu.
func (c *StateCache) evictIfFull() {
	if len(c.states) <= c.capacity {
		return
	}
	// First pass: terminal entries in insertion order.
	for i, id := range c.order {
		st, ok := c.states[id]
		if !ok {
			continue
		}
		if st.Phase.IsTerminal() {
			c.order = append(c.order[:i], c.order[i+1:]...)
			delete(c.states, id)
			return
		}
	}
	// No terminal entry → drop the oldest regardless.
	oldest := c.order[0]
	c.order = c.order[1:]
	delete(c.states, oldest)
}

// Get returns a snapshot copy of the state for runID. ErrUnknownRun is
// returned when the run is not in the cache.
func (c *StateCache) Get(runID string) (runtime.ExecutionState, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	st, ok := c.states[runID]
	if !ok {
		return runtime.ExecutionState{}, ErrUnknownRun
	}
	return *st, nil // value copy — callers may not mutate the cache
}

// List returns a snapshot of every cached state in insertion order
// (oldest first). The returned slice contains value copies, safe to
// retain or mutate.
func (c *StateCache) List() []runtime.ExecutionState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]runtime.ExecutionState, 0, len(c.order))
	for _, id := range c.order {
		if st, ok := c.states[id]; ok {
			out = append(out, *st)
		}
	}
	return out
}

// Forget removes a run from the cache. Idempotent. Useful for callers
// that want to release memory when the run is done streaming.
func (c *StateCache) Forget(runID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.states[runID]; !ok {
		return
	}
	delete(c.states, runID)
	for i, id := range c.order {
		if id == runID {
			c.order = append(c.order[:i], c.order[i+1:]...)
			return
		}
	}
}

// Len returns the current number of cached states (test helper).
func (c *StateCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.states)
}
