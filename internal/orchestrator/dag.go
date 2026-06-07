// Package orchestrator — DAG executor (W12 Slice 4).
//
// DAGRun walks a planner.TaskDAG by spawning one chain per plan.Roots()
// entry. Each chain runs in its own worktree (Modelo B); nodes within a
// chain run sequentially with structured handoff. After chains finish
// the judge ranks survivors and a synthesizer agent consolidates work
// in the winner's worktree.
//
// This file defines the public types. The DAGRun entry point and
// helpers live in dag_chain.go / dag_synthesizer.go / etc.
package orchestrator

import (
	"errors"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/agent"
	"github.com/felipemarques-rec/sandcode/internal/budget"
	"github.com/felipemarques-rec/sandcode/internal/event"
	gitm "github.com/felipemarques-rec/sandcode/internal/git"
	"github.com/felipemarques-rec/sandcode/internal/governance"
	"github.com/felipemarques-rec/sandcode/internal/judge"
	"github.com/felipemarques-rec/sandcode/internal/kernel"
	"github.com/felipemarques-rec/sandcode/internal/langfuse"
	"github.com/felipemarques-rec/sandcode/internal/planner"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
	"github.com/felipemarques-rec/sandcode/internal/store"
)

// Sentinel errors surfaced by DAGRun and validation.
var (
	ErrEmptyDAGOpts              = errors.New("orchestrator: empty DAGOptions")
	ErrEmptyPlan                 = errors.New("orchestrator: empty plan")
	ErrDiamondNotSupported       = errors.New("orchestrator: dag has node with multiple dependencies (diamond shape) — not supported in this slice")
	ErrJudgeRequiredForMultiRoot = errors.New("orchestrator: dag with >1 roots requires a Judge")
	ErrAllChainsFailed           = errors.New("orchestrator: all chains failed")
	ErrAllOuterCopiesFailed      = errors.New("orchestrator: all outer copies failed")
)

// DAGOptions configures one DAG execution. Fields shared with RunOptions
// (Prompt, CWD, SandboxImage/WorkDir, Strategy, KeepWorktree, Timeout,
// Limits, Network, AgentOpts, Store, ParentRunID, Kernel, Bus, Refine,
// Governance, AuditLog, Budget, RunID) carry identical semantics. Fields
// new to the DAG executor are documented below.
type DAGOptions struct {
	// Shared with RunOptions:
	Prompt         string
	CWD            string
	SandboxImage   string
	SandboxWorkDir string
	Strategy       gitm.Strategy
	KeepWorktree   bool
	Timeout        time.Duration
	Limits         sandbox.Limits
	Network        string
	AgentOpts      agent.RunOptions
	Store          store.Store
	ParentRunID    string
	Kernel         *kernel.Kernel
	Bus            event.Bus

	// Langfuse, when non-nil and Enabled(), nests DAG chains/nodes and
	// the judge pass under the active trace span carried by ctx.
	Langfuse *langfuse.Provider

	Refine     RefineOptions
	Governance *governance.Engine
	AuditLog   governance.AuditLog
	Budget     *budget.Guard
	RunID      string

	// Plan is the TaskDAG to execute. Required.
	Plan planner.TaskDAG

	// Agents optionally configures multi-agent within-DAG round-robin.
	// When non-empty AND len > 1, each chain is assigned Agents[i % len]
	// via a chainSpec.AssignedAgent field set during decomposeChains.
	// Empty or single-element falls back to the `ag` parameter passed
	// to DAGRun — preserves Slice 4 behavior for single-agent callers.
	Agents []agent.Provider

	// Judge ranks chain results when len(Plan.Roots()) > 1. Required in
	// that case; ignored when there is only one root chain (no peers to
	// rank against).
	Judge judge.Judge

	// Synthesizer configures the final consolidation pass. Default
	// (zero value) means: enabled, default prompt template, respects
	// the same RefineOptions configured for the run.
	Synthesizer SynthesizerOptions

	// OuterCopyIndex is set by the outer fan-out wrapper when running
	// under --parallel N or multi-agent. Zero for solo runs. Embedded
	// in events for cross-copy correlation.
	OuterCopyIndex int
}

// SynthesizerOptions controls the consolidation pass. Zero value =
// enabled with default prompt template. Disabled=true skips the
// synthesizer entirely; the DAGResult.Synthesizer will be nil.
type SynthesizerOptions struct {
	Disabled bool
}

// DAGResult is the outcome of one DAGRun.
type DAGResult struct {
	Plan           planner.TaskDAG
	Chains         []ChainResult
	Synthesizer    *AgentInvocationResult // nil when synthesizer skipped or no chain succeeded
	Winner         string                 // ChainID the judge selected; "" if no winner
	JudgeRationale string
	OuterCopyIndex int
	Cost           CostSummary
	Duration       time.Duration
	Started        time.Time
	Finished       time.Time
	Error          error // ErrAllChainsFailed et al.
}

// ChainResult is the outcome of one root chain.
type ChainResult struct {
	ChainID    string       // stable id, e.g. "chain-0"
	RootNodeID string       // the planner.Node.ID at the root of this chain
	Nodes      []NodeResult // execution order (topo within the chain)
	Worktree   string       // absolute path to the chain's worktree
	Branch     string
	Success    bool
	FailedAt   string // NodeID where the chain broke; empty when Success
	Cost       CostSummary
	Started    time.Time
	Finished   time.Time
}

// NodeResult is the outcome of one node execution within a chain.
type NodeResult struct {
	NodeID   string
	Prompt   string                // actual prompt sent (handoff included)
	Attempts int                   // 1 with no refine; 1..MaxAttempts with refine
	Result   AgentInvocationResult // last attempt (refine winner if applicable)
	Diff     string                // git diff <chainBase>..HEAD captured after this node
	Cost     CostSummary
}

// AgentInvocationResult captures the outcome of one agent invocation —
// either a single node attempt or the synthesizer pass. Lighter than
// the orchestrator's run-level Result: no worktree/diff/merge concerns
// (those are chain/run-level).
type AgentInvocationResult struct {
	ExitCode   int
	Completion string // accumulated text from the agent stream
	Err        error
	Duration   time.Duration
}

// CostSummary aggregates per-step token + dollar usage.
type CostSummary struct {
	Tokens int64
	USD    float64
}

// DAGEvent is the envelope for the dag.* events streamed on the
// channel returned by DAGRun. ChainID/NodeID are empty for DAG-level
// events. Stream is populated when forwarding agent output.
type DAGEvent struct {
	Type    event.Type
	ChainID string
	NodeID  string
	Stream  agent.StreamEvent
	Payload any
}
