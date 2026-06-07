// Package event defines the event bus abstraction for sandcode.
//
// Every state change in the system is modeled as an immutable event.
// Events enable: audit trails, replay/debugging, loose coupling between
// bounded contexts, and future event sourcing.
package event

import (
	"time"

	"github.com/google/uuid"
)

// Type classifies events by bounded context and operation.
type Type string

// Run lifecycle events.
const (
	RunSubmitted        Type = "run.submitted"
	RunClassified       Type = "run.classified"
	RunPlanned          Type = "run.planned"
	RunStrategySelected Type = "run.strategy_selected"
	RunEnriched         Type = "run.enriched"
	RunScheduled        Type = "run.scheduled"
	RunDequeued         Type = "run.dequeued"
	RunCompleted        Type = "run.completed"
	RunFailed           Type = "run.failed"
	RunCancelled        Type = "run.cancelled"
)

// Agent execution events.
const (
	AgentExecuting  Type = "agent.executing"
	AgentToolCalled Type = "agent.tool_called"
	AgentCompleted  Type = "agent.completed"
)

// Sandbox events.
const (
	SandboxCreated   Type = "sandbox.created"
	SandboxDestroyed Type = "sandbox.destroyed"
)

// Verification events.
const (
	VerifyStarted Type = "verify.started"
	VerifyPassed  Type = "verify.passed"
	VerifyFailed  Type = "verify.failed"
)

// Refine events.
const (
	RefineTriggered Type = "refine.triggered"
)

// DAG executor lifecycle events. All observation-only — they increment
// EventCount but do not transition phase. Slice 4 (W12) introduces these.
const (
	DAGStarted            Type = "dag.started"
	DAGChainStarted       Type = "dag.chain_started"
	DAGNodeStarted        Type = "dag.node_started"
	DAGNodeCompleted      Type = "dag.node_completed"
	DAGChainCompleted     Type = "dag.chain_completed"
	DAGSynthesisStarted   Type = "dag.synthesis_started"
	DAGSynthesisCompleted Type = "dag.synthesis_completed"
	DAGCompleted          Type = "dag.completed"
)

// Report events.
const (
	// ReportGenerated is emitted (observation-only) after a Reporter produces
	// a run report artifact. Carries no trace_id by construction; correlate
	// via run_id on the SSE stream.
	ReportGenerated Type = "report.generated"

	// ReviewGenerated is emitted (observation-only) after a Reviewer scores a
	// run's diff against the prompt. It never changes run status.
	ReviewGenerated Type = "review.generated"

	// RunArchitected is emitted (observation-only) after an Architect designs
	// solution guidance for a run. It never changes run status or phase.
	RunArchitected Type = "run.architected"

	// SecurityReviewed is emitted (observation-only) after a Security Reviewer
	// scans a run's diff. It never changes run status.
	SecurityReviewed Type = "security.reviewed"

	// PerformanceReviewed is emitted (observation-only) after a Performance
	// Reviewer scores a run's diff. It never changes run status.
	PerformanceReviewed Type = "perf.reviewed"

	// RefactoringReviewed is emitted (observation-only) after a Refactoring
	// Specialist scores a run's diff. It never changes run status.
	RefactoringReviewed Type = "refactor.reviewed"
)

// Brain events.
const (
	BrainLessonExtracted Type = "brain.lesson_extracted"
	BrainLessonRecalled  Type = "brain.lesson_recalled"
)

// Governance events.
const (
	GovernanceApprovalRequired Type = "governance.approval_required"
	GovernanceApproved         Type = "governance.approved"
	GovernanceDenied           Type = "governance.denied"
)

// Budget events.
const (
	BudgetThresholdReached Type = "budget.threshold_reached"
	BudgetExceeded         Type = "budget.exceeded"
)

// Event is an immutable record of something that happened in the system.
// Events are append-only and ordered by timestamp within a run.
type Event struct {
	// ID is a globally unique identifier (UUIDv4).
	ID string `json:"id"`

	// RunID links this event to a specific run.
	RunID string `json:"run_id"`

	// ParentRunID links to the parent run (for parallel sub-runs).
	ParentRunID string `json:"parent_run_id,omitempty"`

	// Type classifies the event.
	Type Type `json:"type"`

	// Payload is JSON-encoded type-specific data.
	Payload []byte `json:"payload,omitempty"`

	// Timestamp is when the event occurred.
	Timestamp time.Time `json:"timestamp"`

	// CorrelationID links related events across boundaries.
	CorrelationID string `json:"correlation_id"`

	// CausationID is the ID of the event that caused this one.
	// Empty for the root event of a run.
	CausationID string `json:"causation_id,omitempty"`

	// TraceID links this event to the Langfuse/OTel trace for the run.
	// Empty when tracing is disabled. Stamped from context at publish
	// time; this package stays otel-free (the value is an opaque string).
	TraceID string `json:"trace_id,omitempty"`
}

// New creates an event with auto-generated ID and timestamp.
func New(typ Type, runID string, payload []byte) Event {
	return Event{
		ID:        uuid.New().String(),
		RunID:     runID,
		Type:      typ,
		Payload:   payload,
		Timestamp: time.Now(),
	}
}

// WithCorrelation returns a copy with the correlation ID set.
func (e Event) WithCorrelation(id string) Event {
	e.CorrelationID = id
	return e
}

// WithCausation returns a copy with the causation ID set.
func (e Event) WithCausation(id string) Event {
	e.CausationID = id
	return e
}

// WithTrace returns a copy with the trace ID set. No-op semantics for
// callers: passing "" leaves the (omitempty) field absent.
func (e Event) WithTrace(id string) Event {
	e.TraceID = id
	return e
}
