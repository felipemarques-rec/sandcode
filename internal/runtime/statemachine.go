package runtime

import (
	"errors"
	"fmt"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/event"
)

// ErrTerminal is returned when Apply is called on a state already in a
// terminal phase (completed, failed, cancelled). Replaying past a terminal
// phase would silently corrupt history, so we reject loudly.
var ErrTerminal = errors.New("runtime: state is terminal")

// ErrMismatchedRunID is returned when an event's RunID does not match the
// state's RunID. Mixing event streams across runs is a programmer error.
var ErrMismatchedRunID = errors.New("runtime: event run_id mismatch")

// ExecutionState is the deterministic, replay-safe state of a single run.
//
// Lifecycle:
//
//   - NewExecutionState(runID) returns a state in PhaseSubmitted.
//   - Apply(event) is the ONLY mutator. It either transitions Phase per the
//     closed table, increments Attempt on refine.triggered, or ignores
//     observation-only events.
//   - Replay(events) is sugar over a loop of Apply calls; same invariants.
//
// The struct is intentionally serializable to JSON so checkpoints (Stage 2
// disk persistence) reuse the same shape.
type ExecutionState struct {
	RunID       string        `json:"run_id"`
	Phase       Phase         `json:"phase"`
	Attempt     int           `json:"attempt"`
	MaxAttempts int           `json:"max_attempts"`
	EventCount  int           `json:"event_count"` // total events Apply has accepted (incl. observation-only)
	CreatedAt   time.Time     `json:"created_at"`
	UpdatedAt   time.Time     `json:"updated_at"`
	LastEventID string        `json:"last_event_id,omitempty"`
	LastError   string        `json:"last_error,omitempty"` // populated from failedPayload, when available
	Duration    time.Duration `json:"duration,omitempty"`   // set when terminal
}

// NewExecutionState constructs a fresh state in PhaseSubmitted.
// MaxAttempts defaults to 3 — the conventional refine cap.
func NewExecutionState(runID string) *ExecutionState {
	now := time.Now()
	return &ExecutionState{
		RunID:       runID,
		Phase:       PhaseSubmitted,
		Attempt:     1,
		MaxAttempts: 3,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

// Apply updates the state in response to a single event. Behavior:
//
//   - The event's RunID must match the state's RunID (or be empty).
//   - Observation-only events bump EventCount and UpdatedAt but do not
//     transition Phase. Required for audit completeness.
//   - run.failed/run.cancelled from any non-terminal phase transition to
//     PhaseFailed/PhaseCancelled and set Duration.
//   - refine.triggered increments Attempt (replay-deterministic). If
//     Attempt would exceed MaxAttempts the state goes to PhaseFailed
//     instead of looping forever.
//   - Apply on a terminal state returns ErrTerminal.
//
// Apply never panics on a malformed event — it returns an error and leaves
// the state unmodified.
func (s *ExecutionState) Apply(ev event.Event) error {
	if s.Phase.IsTerminal() {
		return fmt.Errorf("%w: phase=%s event=%s", ErrTerminal, s.Phase, ev.Type)
	}
	if ev.RunID != "" && s.RunID != "" && ev.RunID != s.RunID {
		return fmt.Errorf("%w: state=%s event=%s", ErrMismatchedRunID, s.RunID, ev.RunID)
	}

	// Observation-only events: record but don't transition.
	if IsObservationOnly(ev.Type) {
		s.recordEvent(ev)
		return nil
	}

	next, err := LookupTransition(s.Phase, ev.Type)
	if err != nil {
		return err
	}

	// refine.triggered loops back to executing AND increments Attempt.
	// When the next attempt would exceed MaxAttempts, the state machine
	// short-circuits to PhaseFailed — Apply is the authoritative gate, no
	// caller needs to enforce the cap.
	if ev.Type == event.RefineTriggered {
		if s.Attempt >= s.MaxAttempts {
			s.Phase = PhaseFailed
			s.LastError = fmt.Sprintf("refine cap exceeded: attempt=%d max=%d", s.Attempt, s.MaxAttempts)
			s.markTerminal(ev.Timestamp)
			s.recordEvent(ev)
			return nil
		}
		s.Attempt++
	}

	s.Phase = next
	s.recordEvent(ev)

	// Capture optional terminal metadata from RunFailed payload.
	if next == PhaseFailed {
		if msg := extractErrorMessage(ev); msg != "" {
			s.LastError = msg
		}
	}

	if next.IsTerminal() {
		s.markTerminal(ev.Timestamp)
	}
	return nil
}

// Replay constructs an ExecutionState by applying a list of events in
// order. The first event's RunID is used as the state's RunID (if the
// list is empty, the runID argument is used).
//
// Replay is strict: any invalid transition or terminal-replay error
// aborts the operation and returns the partial state plus the error.
// Callers that need leniency should call Apply per-event and decide
// what to do with errors themselves.
func Replay(runID string, events []event.Event) (*ExecutionState, error) {
	if len(events) > 0 && events[0].RunID != "" {
		runID = events[0].RunID
	}
	if runID == "" {
		return nil, errors.New("runtime: Replay requires a non-empty runID")
	}

	state := NewExecutionState(runID)
	// Anchor CreatedAt to the first event's timestamp when available, so
	// Duration math matches the original run.
	if len(events) > 0 && !events[0].Timestamp.IsZero() {
		state.CreatedAt = events[0].Timestamp
		state.UpdatedAt = events[0].Timestamp
	}

	for i, ev := range events {
		if err := state.Apply(ev); err != nil {
			return state, fmt.Errorf("replay step %d (event=%s): %w", i, ev.Type, err)
		}
	}
	return state, nil
}

// recordEvent bumps EventCount and stamps the state with the event's metadata.
func (s *ExecutionState) recordEvent(ev event.Event) {
	s.EventCount++
	s.LastEventID = ev.ID
	if !ev.Timestamp.IsZero() {
		s.UpdatedAt = ev.Timestamp
	} else {
		s.UpdatedAt = time.Now()
	}
}

// markTerminal sets Duration from CreatedAt to the terminal event's time.
func (s *ExecutionState) markTerminal(eventAt time.Time) {
	end := eventAt
	if end.IsZero() {
		end = time.Now()
	}
	if !s.CreatedAt.IsZero() {
		s.Duration = end.Sub(s.CreatedAt)
	}
}

// extractErrorMessage attempts to pull the "error" field from a JSON
// payload. Failure modes (no payload, malformed JSON) return "" — Apply
// is best-effort about preserving error text, never fatal.
func extractErrorMessage(ev event.Event) string {
	if len(ev.Payload) == 0 {
		return ""
	}
	// Hand-roll a minimal JSON scan to avoid pulling encoding/json just
	// for this hot-path helper. Look for "error":"..." pattern.
	// We accept "" as a valid match (treated as no message).
	const key = `"error":"`
	idx := indexOf(string(ev.Payload), key)
	if idx < 0 {
		return ""
	}
	rest := string(ev.Payload)[idx+len(key):]
	end := indexOf(rest, `"`)
	if end < 0 {
		return ""
	}
	return rest[:end]
}

// indexOf is a tiny strings.Index without the import.
func indexOf(s, sub string) int {
	n, m := len(s), len(sub)
	if m == 0 {
		return 0
	}
	if m > n {
		return -1
	}
	for i := 0; i <= n-m; i++ {
		if s[i:i+m] == sub {
			return i
		}
	}
	return -1
}
