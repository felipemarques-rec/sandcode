package scheduler

import (
	"context"
	"errors"
	"time"
)

// Priority orders the admission queue. Zero value = PriorityNormal.
type Priority int

const (
	PriorityLow Priority = iota
	PriorityNormal
	PriorityHigh
	PriorityCritical
)

func (p Priority) String() string {
	switch p {
	case PriorityLow:
		return "low"
	case PriorityHigh:
		return "high"
	case PriorityCritical:
		return "critical"
	default:
		return "normal"
	}
}

// ParsePriority maps a wire string to a Priority. "" => PriorityNormal.
// Unknown => error (the HTTP layer turns this into a 400).
func ParsePriority(s string) (Priority, error) {
	switch s {
	case "":
		return PriorityNormal, nil
	case "low":
		return PriorityLow, nil
	case "normal":
		return PriorityNormal, nil
	case "high":
		return PriorityHigh, nil
	case "critical":
		return PriorityCritical, nil
	default:
		return PriorityNormal, errors.New("priority: must be one of low|normal|high|critical")
	}
}

// LaunchFunc is the work one slot performs. The server supplies a closure
// over its Launcher + server-lifetime ctx. The scheduler never sees the
// run payload (decoupling seam).
type LaunchFunc func(ctx context.Context, runID string) error

// Scheduler admits and runs whole runs under a fixed concurrency bound.
type Scheduler interface {
	Submit(runID string, p Priority) error
	Cancel(runID string) bool
	Status(runID string) (RunStatus, bool)
	Start()
	Stop(ctx context.Context) error
}

// RunStatus is the queue-side view of a run (not its execution phase).
type RunStatus struct {
	State    string // "queued" | "running"
	Priority Priority
	Position int // 0-based queue position; 0 while running
	Enqueued time.Time
}

// Config holds the two operator knobs. Both must be > 0 for the server
// to construct a scheduler at all.
type Config struct {
	PoolSize int // concurrent runs
	QueueCap int // bounded waiting queue
}

var (
	ErrQueueFull = errors.New("scheduler: queue full")
	ErrStopped   = errors.New("scheduler: stopped")
	ErrDuplicate = errors.New("scheduler: run already submitted")
)
