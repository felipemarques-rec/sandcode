package metrics

import (
	"context"

	"github.com/felipemarques-rec/sandcode/internal/event"
)

// Subscriber translates events on a bus into metric increments. It owns
// a small set of Prometheus families that mirror the run / verify /
// refine / governance / budget event taxonomy.
//
// The subscriber is intentionally narrow: it only counts events. It
// does not parse payloads or derive durations — those belong in
// higher-layer recorders that have typed access to payload structs.
type Subscriber struct {
	reg *Registry

	eventsTotal      *Counter // {type}
	runsTotal        *Counter // {result}
	runDuration      *Histogram
	verifyTotal      *Counter // {result}
	refinesTotal     *Counter
	governanceTotal  *Counter // {result}
	budgetEvents     *Counter // {kind}
	sandboxesTotal   *Counter // {state}
	agentToolsCalled *Counter

	// run start tracking for duration histogram
	starts startTracker
}

// NewSubscriber registers metric families on reg and returns a
// Subscriber ready to be attached via Attach.
//
// Registering up-front (vs. lazy on first observation) ensures the
// /metrics endpoint reports zero-valued series before any traffic,
// which is the friendlier behaviour for dashboards and alert rules.
func NewSubscriber(reg *Registry) *Subscriber {
	s := &Subscriber{reg: reg}
	s.eventsTotal = reg.NewCounter(
		"sandcode_events_total",
		"Total events published on the bus, by event type.",
		[]string{"type"},
	)
	s.runsTotal = reg.NewCounter(
		"sandcode_runs_total",
		"Total runs that reached a terminal state, by result.",
		[]string{"result"},
	)
	s.runDuration = reg.NewHistogram(
		"sandcode_run_duration_seconds",
		"Wall-clock duration of completed runs in seconds.",
		[]string{"result"},
		nil, // DefaultBuckets
	)
	s.verifyTotal = reg.NewCounter(
		"sandcode_verify_total",
		"Verification outcomes, by result.",
		[]string{"result"},
	)
	s.refinesTotal = reg.NewCounter(
		"sandcode_refines_total",
		"Refine cycles triggered.",
		nil,
	)
	s.governanceTotal = reg.NewCounter(
		"sandcode_governance_decisions_total",
		"Governance policy decisions, by result.",
		[]string{"result"},
	)
	s.budgetEvents = reg.NewCounter(
		"sandcode_budget_events_total",
		"Budget guard signals, by kind.",
		[]string{"kind"},
	)
	s.sandboxesTotal = reg.NewCounter(
		"sandcode_sandboxes_total",
		"Sandbox lifecycle transitions, by state.",
		[]string{"state"},
	)
	s.agentToolsCalled = reg.NewCounter(
		"sandcode_agent_tools_called_total",
		"Tool calls observed from agent execution.",
		nil,
	)
	s.starts = newStartTracker()
	return s
}

// Attach subscribes the subscriber's handler to bus and returns the
// subscription so the caller can cancel it on shutdown.
func (s *Subscriber) Attach(bus event.Bus) event.Subscription {
	return bus.Subscribe("*", func(_ context.Context, ev event.Event) error {
		s.observe(ev)
		return nil
	})
}

// observe dispatches a single event to the metric families. Split out
// so tests can drive it directly without a bus.
func (s *Subscriber) observe(ev event.Event) {
	s.eventsTotal.Inc(string(ev.Type))

	switch ev.Type {
	case event.RunSubmitted:
		s.starts.set(ev.RunID, ev.Timestamp)
	case event.RunCompleted:
		s.runsTotal.Inc("completed")
		s.recordDuration(ev, "completed")
	case event.RunFailed:
		s.runsTotal.Inc("failed")
		s.recordDuration(ev, "failed")
	case event.RunCancelled:
		s.runsTotal.Inc("cancelled")
		s.recordDuration(ev, "cancelled")

	case event.VerifyPassed:
		s.verifyTotal.Inc("passed")
	case event.VerifyFailed:
		s.verifyTotal.Inc("failed")

	case event.RefineTriggered:
		s.refinesTotal.Inc()

	case event.GovernanceApproved:
		s.governanceTotal.Inc("approved")
	case event.GovernanceDenied:
		s.governanceTotal.Inc("denied")
	case event.GovernanceApprovalRequired:
		s.governanceTotal.Inc("required")

	case event.BudgetThresholdReached:
		s.budgetEvents.Inc("threshold")
	case event.BudgetExceeded:
		s.budgetEvents.Inc("exceeded")

	case event.SandboxCreated:
		s.sandboxesTotal.Inc("created")
	case event.SandboxDestroyed:
		s.sandboxesTotal.Inc("destroyed")

	case event.AgentToolCalled:
		s.agentToolsCalled.Inc()
	}
}

func (s *Subscriber) recordDuration(ev event.Event, result string) {
	start, ok := s.starts.take(ev.RunID)
	if !ok || ev.Timestamp.Before(start) {
		return
	}
	s.runDuration.Observe(ev.Timestamp.Sub(start).Seconds(), result)
}
