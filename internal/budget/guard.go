// Package budget tracks per-run resource consumption (attempts, tokens,
// USD cost) so the governance.BudgetPolicy can enforce ceilings without
// having to thread accounting state through every call site.
//
// Design choices:
//
//   - Pure in-memory. No I/O. Budget state lives for the lifetime of a
//     run; on process restart it resets to zero (the event store is the
//     authoritative replay source anyway).
//   - Counters monotonically increase. There is no "decrement" — that
//     would break audit semantics if you ever needed to ask "how much
//     did run R consume?" historically.
//   - Concurrent-safe. Multiple goroutines may call Record* on the same
//     Guard simultaneously (parallel agents share the budget pool).
package budget

import (
	"sync"
)

// Report is the snapshot of one run's consumption.
type Report struct {
	RunID    string
	Attempts int
	Tokens   int64
	CostUSD  float64
}

// Guard accumulates consumption per run. The zero value is NOT useful —
// always construct via New().
type Guard struct {
	mu      sync.Mutex
	reports map[string]*Report
}

// New constructs an empty Guard.
func New() *Guard {
	return &Guard{reports: make(map[string]*Report)}
}

// RecordAttempt increments the run's agent-attempt counter and returns
// the new total. Useful for cap-arithmetic at the call site without
// needing a second Report() round-trip.
func (g *Guard) RecordAttempt(runID string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	r := g.ensure(runID)
	r.Attempts++
	return r.Attempts
}

// RecordTokens adds n tokens to the run's running total. n may be zero
// (no-op) but should not be negative; negative values are clamped to
// zero so the running total never drops.
func (g *Guard) RecordTokens(runID string, n int64) int64 {
	if n < 0 {
		n = 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	r := g.ensure(runID)
	r.Tokens += n
	return r.Tokens
}

// RecordCost adds usd to the run's running cost in USD. Negative values
// are clamped to zero.
func (g *Guard) RecordCost(runID string, usd float64) float64 {
	if usd < 0 {
		usd = 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	r := g.ensure(runID)
	r.CostUSD += usd
	return r.CostUSD
}

// Report returns a snapshot of the run's consumption. The returned
// value is a copy — safe to mutate.
func (g *Guard) Report(runID string) Report {
	g.mu.Lock()
	defer g.mu.Unlock()
	r, ok := g.reports[runID]
	if !ok {
		return Report{RunID: runID}
	}
	return *r // value copy
}

// Forget releases the report for a finished run. Optional — Guards are
// typically discarded with the orchestrator that owns them — but useful
// in long-lived processes (HTTP server) to keep memory bounded.
func (g *Guard) Forget(runID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.reports, runID)
}

// ensure lazily creates the per-run record. Caller must hold g.mu.
func (g *Guard) ensure(runID string) *Report {
	r, ok := g.reports[runID]
	if !ok {
		r = &Report{RunID: runID}
		g.reports[runID] = r
	}
	return r
}
