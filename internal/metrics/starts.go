package metrics

import (
	"sync"
	"time"
)

// startTracker remembers run start timestamps so the duration
// histogram can compute completion latency without payload parsing.
//
// Entries are removed on take(); a defensive size cap (capacity) drops
// the oldest entry when exceeded so a flood of orphan RunSubmitted
// events (terminal event never arrives) cannot grow memory unbounded.
type startTracker struct {
	mu       sync.Mutex
	starts   map[string]time.Time
	order    []string // insertion order for cap eviction
	capacity int
}

func newStartTracker() startTracker {
	return startTracker{
		starts:   make(map[string]time.Time),
		capacity: 4096,
	}
}

func (t *startTracker) set(runID string, ts time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, exists := t.starts[runID]; !exists {
		t.order = append(t.order, runID)
		if len(t.order) > t.capacity {
			old := t.order[0]
			t.order = t.order[1:]
			delete(t.starts, old)
		}
	}
	t.starts[runID] = ts
}

func (t *startTracker) take(runID string) (time.Time, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ts, ok := t.starts[runID]
	if !ok {
		return time.Time{}, false
	}
	delete(t.starts, runID)
	for i, id := range t.order {
		if id == runID {
			t.order = append(t.order[:i], t.order[i+1:]...)
			break
		}
	}
	return ts, true
}
