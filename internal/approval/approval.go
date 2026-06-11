// Package approval provides the rendezvous between a run blocked on a
// governance Review verdict and the decision that unblocks it (an HTTP
// approval endpoint in server mode, or a terminal prompt in CLI mode).
// It has no HTTP or orchestrator dependency so it can be tested in isolation.
package approval

import (
	"context"
	"errors"
	"sync"
)

// Request describes a pending approval surfaced to an approver.
type Request struct {
	RunID   string
	Action  string // e.g. "execute"
	Attempt int
	Reasons []string // governance reasons that triggered the Review
}

// Decision is the outcome an approver returns.
type Decision struct {
	Approved bool
	Approver string // who decided (free-form id; not authorized here)
	Reason   string
}

// Approver blocks until a decision arrives or ctx is done.
type Approver interface {
	RequestApproval(ctx context.Context, req Request) (Decision, error)
}

// ErrAlreadyWaiting is returned when a run is already awaiting approval.
var ErrAlreadyWaiting = errors.New("approval: run already awaiting approval")

// Registry is the shared rendezvous used in server mode. It implements
// Approver: the blocked run goroutine calls RequestApproval; the HTTP
// handler calls Resolve. Safe for concurrent use.
type Registry struct {
	mu      sync.Mutex
	waiters map[string]chan Decision
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{waiters: make(map[string]chan Decision)}
}

// RequestApproval registers a waiter for req.RunID and blocks until Resolve
// delivers a decision or ctx is done. A second concurrent wait on the same
// RunID returns ErrAlreadyWaiting.
func (r *Registry) RequestApproval(ctx context.Context, req Request) (Decision, error) {
	ch := make(chan Decision, 1)
	r.mu.Lock()
	if _, exists := r.waiters[req.RunID]; exists {
		r.mu.Unlock()
		return Decision{}, ErrAlreadyWaiting
	}
	r.waiters[req.RunID] = ch
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		delete(r.waiters, req.RunID)
		r.mu.Unlock()
	}()

	select {
	case d := <-ch:
		return d, nil
	case <-ctx.Done():
		return Decision{}, ctx.Err()
	}
}

// Resolve delivers a decision to a waiting run. Returns false if none waiting.
func (r *Registry) Resolve(runID string, d Decision) bool {
	r.mu.Lock()
	ch, ok := r.waiters[runID]
	if ok {
		delete(r.waiters, runID)
	}
	r.mu.Unlock()
	if !ok {
		return false
	}
	ch <- d // buffered (cap 1) — never blocks
	return true
}

// Pending returns the runIDs currently awaiting approval.
func (r *Registry) Pending() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.waiters))
	for k := range r.waiters {
		out = append(out, k)
	}
	return out
}
