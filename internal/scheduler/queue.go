// Package scheduler is the Stage-2 in-process run scheduler: a bounded
// priority queue feeding a fixed worker-goroutine pool. It bounds whole
// runs (run-level admission), not cognitive sub-tasks.
//
// The master-plan §3.1 Lease/Complete/Fail pull-protocol is deliberately
// NOT implemented here: the in-process pool pulls internally, so those
// methods would be speculative surface (master plan §14.2). The Scheduler
// interface is shaped so a Stage-3 SQLite/NATS implementation swaps in at
// the single scheduler.New construction point with no call-site change.
package scheduler

import (
	"container/heap"
	"time"
)

// entry is one queued/running run. It is the unit the pool dispatches.
type entry struct {
	runID     string
	priority  Priority
	submitSeq uint64 // monotonic arrival order; tie-breaker within a priority
	enqueued  time.Time
	running   bool // flips true when the pool pops it
	index     int  // heap.Interface bookkeeping; -1 when not in heap
}

// pq is a max-heap on (priority, -submitSeq). It is NOT goroutine-safe;
// the scheduler serializes all access under its own mutex.
type pq struct {
	items []*entry
}

func (p *pq) Len() int { return len(p.items) }

func (p *pq) Less(i, j int) bool {
	a, b := p.items[i], p.items[j]
	if a.priority != b.priority {
		return a.priority > b.priority // higher priority first
	}
	return a.submitSeq < b.submitSeq // FIFO within a priority
}

func (p *pq) Swap(i, j int) {
	p.items[i], p.items[j] = p.items[j], p.items[i]
	p.items[i].index = i
	p.items[j].index = j
}

func (p *pq) Push(x any) {
	e := x.(*entry)
	e.index = len(p.items)
	p.items = append(p.items, e)
}

func (p *pq) Pop() any {
	old := p.items
	n := len(old)
	e := old[n-1]
	old[n-1] = nil
	e.index = -1
	p.items = old[:n-1]
	return e
}

// Convenience wrappers so callers never touch heap.* directly.
func (p *pq) push(e *entry) { heap.Push(p, e) }
func (p *pq) pop() *entry   { return heap.Pop(p).(*entry) }
func (p *pq) len() int      { return p.Len() }

// remove deletes the entry with runID from the heap. Returns false if
// absent. O(n) scan + O(log n) fix — fine for a bounded admission queue.
func (p *pq) remove(runID string) bool {
	for i, e := range p.items {
		if e.runID == runID {
			heap.Remove(p, i)
			return true
		}
	}
	return false
}

// positionOf returns the 0-based pop position of runID without mutating
// the heap, or -1 if absent. O(n log n) (clones + drains a copy) — only
// called by Status, which is not hot.
func (p *pq) positionOf(runID string) int {
	// Value-copy each entry (not the pointers): heap.Pop below mutates
	// entry.index (sets -1) and reorders; sharing live *entry pointers
	// would corrupt the real heap. The clone is throwaway.
	clone := &pq{items: make([]*entry, len(p.items))}
	for i, e := range p.items {
		cp := *e
		clone.items[i] = &cp
	}
	pos := 0
	for clone.len() > 0 {
		if clone.pop().runID == runID {
			return pos
		}
		pos++
	}
	return -1
}
