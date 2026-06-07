// Package orchestrator — Execute is the unified top-level entry point
// that auto-routes to Run / ParallelRun / DAGRun based on kernel output
// (W12 Slice 5). Run / ParallelRun / DAGRun remain callable directly
// for callers that want to skip dispatch (e.g. the CLI's explicit --dag
// path).
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/felipemarques-rec/sandcode/internal/agent"
	"github.com/felipemarques-rec/sandcode/internal/auth"
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
	"github.com/felipemarques-rec/sandcode/internal/strategy"
)

// DispatchKind enumerates the four destinations Execute can route to.
type DispatchKind int

const (
	DispatchSingle   DispatchKind = iota // → Run (no refine cascade)
	DispatchRefine                       // → Run (with refine cascade)
	DispatchParallel                     // → ParallelRun
	DispatchDAG                          // → DAGRun
)

func (d DispatchKind) String() string {
	switch d {
	case DispatchSingle:
		return "single"
	case DispatchRefine:
		return "refine"
	case DispatchParallel:
		return "parallel"
	case DispatchDAG:
		return "dag"
	default:
		return fmt.Sprintf("unknown(%d)", int(d))
	}
}

// Entry-time errors returned by Execute as the third return value.
var (
	ErrExecuteEmptyPrompt       = errors.New("orchestrator: Execute: empty Prompt")
	ErrExecuteEmptyCWD          = errors.New("orchestrator: Execute: empty CWD")
	ErrExecuteEmptySandboxImage = errors.New("orchestrator: Execute: empty SandboxImage")
	ErrExecuteNoAgent           = errors.New("orchestrator: Execute: no Agent or Agents")
	ErrExecuteParallelAndAgents = errors.New("orchestrator: Execute: ParallelN > 1 and len(Agents) > 1 simultaneously are mutually exclusive")
)

// ExecuteOptions is the unified caller-facing config. Fields shared
// across Run / ParallelRun / DAGRun are deduplicated; destination-
// specific fields (Plan, Judge, Synthesizer for DAG; Agents for
// Parallel/DAG) are optional. Kernel drives dispatch when configured;
// nil falls back to Run.
type ExecuteOptions struct {
	// Required
	Prompt       string
	CWD          string
	SandboxImage string

	// Common
	SandboxWorkDir string
	Strategy       gitm.Strategy
	KeepWorktree   bool
	Timeout        time.Duration
	Limits         sandbox.Limits
	Network        string
	AgentOpts      agent.RunOptions
	Store          store.Store
	Bus            event.Bus
	Refine         RefineOptions
	Governance     *governance.Engine
	AuditLog       governance.AuditLog
	Budget         *budget.Guard
	RunID          string
	Kernel         *kernel.Kernel

	// Langfuse, when non-nil and Enabled(), wraps the whole Execute
	// (kernel stages + dispatch + dispatched run) in one OTel trace.
	Langfuse *langfuse.Provider

	// Registry, when non-nil, enables opt-in role-based agent resolution on
	// the Single/Refine dispatch path ONLY. nil ⇒ legacy positional-agent
	// behavior (byte-identical). NOT forwarded to Parallel or DAG paths.
	Registry agent.Registry

	// Single-agent base (used by DispatchSingle/Refine, and as fallback
	// agent for DAGRun when Agents is empty/single).
	Agent agent.Provider

	// Multi-agent fan-out (Parallel) or within-DAG round-robin (DAG).
	// When non-empty AND len > 1, takes precedence over Agent.
	Agents []agent.Provider

	// ParallelN forces N copies of the same Agent (outer fan-out style).
	// Mutually exclusive with len(Agents) > 1.
	ParallelN int

	// DAG-mode hooks (forwarded only when DAGRun is chosen).
	Plan        planner.TaskDAG
	Judge       judge.Judge
	Synthesizer SynthesizerOptions
}

// ExecuteResult is the polymorphic envelope. Exactly one of Run /
// Parallel / DAG is non-nil based on Kind.
type ExecuteResult struct {
	Kind           DispatchKind
	DispatchReason string          // human-readable why this dispatch fired
	Run            *Result         // populated when Kind ∈ {Single, Refine}
	Parallel       *ParallelResult // populated when Kind == Parallel
	DAG            *DAGResult      // populated when Kind == DAG
}

// dispatchKind is the pure 9-row dispatch policy table (Slice 5 spec
// §"Dispatch policy table"). Returns the chosen Kind plus a short
// human-readable reason that flows into ExecuteResult.DispatchReason
// AND a slog.Warn whenever the chosen Kind is a fallback (not the
// kernel's first-choice destination).
//
// Inputs are pre-resolved values, not the raw ProcessResult, so the
// caller (Execute) can substitute defaults (e.g. ForcePlan output for
// pr.Plan).
func dispatchKind(
	strat strategy.Strategy,
	plan planner.TaskDAG,
	agentCount int,
	parallelN int,
	hasJudge bool,
	refineActive bool,
) (DispatchKind, string) {
	rootCount := len(plan.Roots())

	switch strat {
	case strategy.StrategyRefine:
		if refineActive {
			return DispatchRefine, "kernel selected refine"
		}
		return DispatchSingle, "refine suggested but RefineOptions disabled; falling back to single"

	case strategy.StrategyParallel:
		if rootCount > 1 {
			if hasJudge {
				return DispatchDAG, "kernel selected parallel + plan multi-root"
			}
			if agentCount > 1 {
				return DispatchParallel, "dag requires judge; falling back to parallel"
			}
			return DispatchSingle, "dag requires judge and parallel needs >1 agent; falling back to single"
		}
		// Plan empty or single-root.
		if agentCount > 1 {
			return DispatchParallel, "kernel selected parallel; multi-agent fan-out"
		}
		if parallelN > 1 {
			return DispatchParallel, "kernel selected parallel; --parallel replication"
		}
		return DispatchSingle, "parallel suggested but no agents; falling back to single"

	case strategy.StrategySingle:
		return DispatchSingle, "kernel selected single"
	}

	// Empty / unknown strategy: default to single.
	return DispatchSingle, "no strategy from kernel; defaulting to single"
}

// validateExecute checks required fields. Returns nil on success.
func validateExecute(opts ExecuteOptions) error {
	if opts.Prompt == "" {
		return ErrExecuteEmptyPrompt
	}
	if opts.CWD == "" {
		return ErrExecuteEmptyCWD
	}
	if opts.SandboxImage == "" {
		return ErrExecuteEmptySandboxImage
	}
	if opts.Agent == nil && len(opts.Agents) == 0 {
		return ErrExecuteNoAgent
	}
	if opts.ParallelN > 1 && len(opts.Agents) > 1 {
		return ErrExecuteParallelAndAgents
	}
	return nil
}

// isFallback detects fallback dispatch reasons emitted by dispatchKind.
// Tight coupling to the reason strings; acceptable for slog
// classification. If reason text changes, update both at once.
func isFallback(reason string) bool {
	return strings.Contains(reason, "falling back") ||
		strings.Contains(reason, "force plan failed")
}

// Execute is the unified top-level entry point. When Kernel is
// configured, calls kernel.Process once and dispatches based on
// Strategy + Plan + caller-provided agents/parallel hints. When
// Kernel is nil, behaves exactly like Run with the configured single
// Agent (DispatchSingle).
//
// Double-emission defense: Execute calls kernel.Process ONCE; the
// forwarded options carry the EnrichedPrompt and Kernel=nil so the
// destination (Run/ParallelRun/DAGRun) does NOT re-invoke the kernel.
// TestExecute_KernelProcessOnlyCalledOnce is the regression gate.
func Execute(
	ctx context.Context,
	sb sandbox.Provider,
	au auth.Provider,
	opts ExecuteOptions,
) (<-chan agent.StreamEvent, func() ExecuteResult, error) {
	var rootSpan trace.Span

	if err := validateExecute(opts); err != nil {
		return nil, nil, err
	}

	// (a) Root span born here, BEFORE kernel.Process, so kernel-stage
	// spans + the dispatched run nest under it via ctx. Ended when the
	// terminal result is produced (wrapped result func / error path),
	// NOT via defer — Execute returns before the run completes.
	var rootEnd func()
	if opts.Langfuse != nil && opts.Langfuse.Enabled() {
		var rs trace.Span
		ctx, rs = opts.Langfuse.SpanBrain(ctx, "execute",
			attribute.String("sandcode.run_id", opts.RunID),
		)
		rootEnd = func() { rs.End() }
		rootSpan = rs
	}

	var (
		ev   <-chan agent.StreamEvent
		w    func() ExecuteResult
		derr error
	)

	if opts.Kernel == nil {
		ev, w, derr = runDirect(ctx, sb, au, opts, "no kernel configured")
	} else {
		pr := opts.Kernel.Process(ctx, kernel.ProcessRequest{
			Prompt: opts.Prompt,
			CWD:    opts.CWD,
			RunID:  opts.RunID,
		})

		resolvedPlan := pr.Plan
		if pr.Strategy == strategy.StrategyParallel && len(resolvedPlan.Nodes) == 0 {
			if plan, err := opts.Kernel.ForcePlan(ctx, opts.Prompt); err == nil {
				resolvedPlan = plan
			} else {
				slog.Warn("execute: force plan failed; proceeding with empty plan",
					"run_id", opts.RunID, "error", err)
			}
		}

		agentCount := len(opts.Agents)
		if agentCount == 0 && opts.Agent != nil {
			agentCount = 1
		}
		kind, reason := dispatchKind(
			pr.Strategy,
			resolvedPlan,
			agentCount,
			opts.ParallelN,
			opts.Judge != nil,
			opts.Refine.active(),
		)

		if isFallback(reason) {
			slog.Warn("execute: dispatch fallback",
				"run_id", opts.RunID,
				"requested_strategy", string(pr.Strategy),
				"chosen_kind", kind.String(),
				"reason", reason,
			)
		}

		// Dispatch decision recorded two ways: as attributes + a span
		// event on the root span, AND as a dedicated child span.
		if rootSpan != nil {
			rootSpan.SetAttributes(
				attribute.String("sandcode.dispatch.kind", kind.String()),
				attribute.String("sandcode.dispatch.reason", reason),
				attribute.String("sandcode.strategy", string(pr.Strategy)),
			)
			rootSpan.AddEvent("dispatch_decided")
			_, dsp := opts.Langfuse.SpanBrain(ctx, "dispatch",
				attribute.String("sandcode.dispatch.kind", kind.String()),
				attribute.String("sandcode.dispatch.reason", reason),
			)
			dsp.End() // intentionally near-zero duration (decision c)
		}

		enriched := pr.EnrichedPrompt
		if enriched == "" {
			enriched = opts.Prompt
		}
		switch kind {
		case DispatchSingle, DispatchRefine:
			ev, w, derr = forwardToRun(ctx, sb, au, opts, enriched, kind, reason)
		case DispatchParallel:
			ev, w, derr = forwardToParallel(ctx, sb, au, opts, enriched, reason)
		case DispatchDAG:
			ev, w, derr = forwardToDAG(ctx, sb, au, opts, enriched, resolvedPlan, reason)
		default:
			ev, w, derr = runDirect(ctx, sb, au, opts, reason)
		}
	}

	if derr != nil {
		if rootEnd != nil {
			rootEnd() // dispatch failed before producing a result
		}
		return nil, nil, derr
	}
	if rootEnd != nil {
		inner := w
		w = func() ExecuteResult {
			r := inner()
			rootEnd()
			return r
		}
	}
	return ev, w, nil
}

// runDirect is the Kernel-nil path: build RunOptions, call Run, wrap
// in ExecuteResult{Kind: Single}.
func runDirect(
	ctx context.Context,
	sb sandbox.Provider,
	au auth.Provider,
	opts ExecuteOptions,
	reason string,
) (<-chan agent.StreamEvent, func() ExecuteResult, error) {
	ag := opts.Agent
	if ag == nil && len(opts.Agents) > 0 {
		ag = opts.Agents[0]
	}
	ropts := buildRunOptionsFromExecute(opts, opts.Prompt)
	events, await, err := Run(ctx, sb, ag, au, ropts)
	if err != nil {
		return nil, nil, err
	}
	wrap := func() ExecuteResult {
		r := await()
		return ExecuteResult{Kind: DispatchSingle, DispatchReason: reason, Run: &r}
	}
	return events, wrap, nil
}

// forwardToRun handles DispatchSingle and DispatchRefine. Refine is
// already configured on opts.Refine — the dispatch label is for the
// envelope, not for re-wiring.
func forwardToRun(
	ctx context.Context,
	sb sandbox.Provider,
	au auth.Provider,
	opts ExecuteOptions,
	enriched string,
	kind DispatchKind,
	reason string,
) (<-chan agent.StreamEvent, func() ExecuteResult, error) {
	ag := opts.Agent
	if ag == nil && len(opts.Agents) > 0 {
		ag = opts.Agents[0]
	}
	ropts := buildRunOptionsFromExecute(opts, enriched)
	ropts.Kernel = nil // suppress second kernel.Process call
	events, await, err := Run(ctx, sb, ag, au, ropts)
	if err != nil {
		return nil, nil, err
	}
	wrap := func() ExecuteResult {
		r := await()
		return ExecuteResult{Kind: kind, DispatchReason: reason, Run: &r}
	}
	return events, wrap, nil
}

// forwardToParallel handles DispatchParallel. The caller may have
// supplied len(Agents) > 1 OR ParallelN > 1 (replication).
func forwardToParallel(
	ctx context.Context,
	sb sandbox.Provider,
	au auth.Provider,
	opts ExecuteOptions,
	enriched string,
	reason string,
) (<-chan agent.StreamEvent, func() ExecuteResult, error) {
	agents := opts.Agents
	if len(agents) <= 1 && opts.ParallelN > 1 && opts.Agent != nil {
		agents = make([]agent.Provider, opts.ParallelN)
		for i := range agents {
			agents[i] = opts.Agent
		}
	}
	popts := ParallelOptions{
		Prompt:         enriched,
		CWD:            opts.CWD,
		SandboxImage:   opts.SandboxImage,
		SandboxWorkDir: opts.SandboxWorkDir,
		Strategy:       opts.Strategy,
		KeepWorktrees:  opts.KeepWorktree,
		Timeout:        opts.Timeout,
		Limits:         opts.Limits,
		Network:        opts.Network,
		Agents:         agents,
		AgentOpts:      opts.AgentOpts,
		Store:          opts.Store,
		Judge:          opts.Judge,
		Kernel:         nil, // suppress second kernel.Process call
		Bus:            opts.Bus,
		Langfuse:       opts.Langfuse,
		Governance:     opts.Governance,
		AuditLog:       opts.AuditLog,
		Budget:         opts.Budget,
		Refine:         opts.Refine,
	}
	subEvents, pAwait, err := ParallelRun(ctx, sb, au, popts)
	if err != nil {
		return nil, nil, err
	}
	out := make(chan agent.StreamEvent, 64*len(agents))
	go func() {
		defer close(out)
		for ev := range subEvents {
			out <- ev.Event
		}
	}()
	wrap := func() ExecuteResult {
		p := pAwait()
		return ExecuteResult{Kind: DispatchParallel, DispatchReason: reason, Parallel: &p}
	}
	return out, wrap, nil
}

// forwardToDAG handles DispatchDAG. Uses the pre-resolved plan
// (possibly from ForcePlan) and forwards Agents for within-DAG
// round-robin.
func forwardToDAG(
	ctx context.Context,
	sb sandbox.Provider,
	au auth.Provider,
	opts ExecuteOptions,
	enriched string,
	plan planner.TaskDAG,
	reason string,
) (<-chan agent.StreamEvent, func() ExecuteResult, error) {
	ag := opts.Agent
	if ag == nil && len(opts.Agents) > 0 {
		ag = opts.Agents[0]
	}
	dopts := DAGOptions{
		Prompt:         enriched,
		CWD:            opts.CWD,
		SandboxImage:   opts.SandboxImage,
		SandboxWorkDir: opts.SandboxWorkDir,
		Strategy:       opts.Strategy,
		KeepWorktree:   opts.KeepWorktree,
		Timeout:        opts.Timeout,
		Limits:         opts.Limits,
		Network:        opts.Network,
		AgentOpts:      opts.AgentOpts,
		Store:          opts.Store,
		Kernel:         nil, // suppress second kernel.Process call
		Bus:            opts.Bus,
		Langfuse:       opts.Langfuse,
		Refine:         opts.Refine,
		Governance:     opts.Governance,
		AuditLog:       opts.AuditLog,
		Budget:         opts.Budget,
		RunID:          opts.RunID,
		Plan:           plan,
		Judge:          opts.Judge,
		Synthesizer:    opts.Synthesizer,
		Agents:         opts.Agents,
	}
	dagEvents, dAwait, err := DAGRun(ctx, sb, ag, au, dopts)
	if err != nil {
		return nil, nil, err
	}
	// DAGRun currently does not write into the returned event channel
	// (it emits dag.* events on the bus and just closes the channel
	// when the run is done). Drain to unblock the close signal.
	out := make(chan agent.StreamEvent)
	go func() {
		defer close(out)
		for range dagEvents {
		}
	}()
	wrap := func() ExecuteResult {
		d := dAwait()
		return ExecuteResult{Kind: DispatchDAG, DispatchReason: reason, DAG: &d}
	}
	return out, wrap, nil
}

// buildRunOptionsFromExecute is the shared transform from
// ExecuteOptions → RunOptions for the Single/Refine paths.
func buildRunOptionsFromExecute(opts ExecuteOptions, prompt string) RunOptions {
	return RunOptions{
		Prompt:         prompt,
		CWD:            opts.CWD,
		SandboxImage:   opts.SandboxImage,
		SandboxWorkDir: opts.SandboxWorkDir,
		Strategy:       opts.Strategy,
		KeepWorktree:   opts.KeepWorktree,
		Timeout:        opts.Timeout,
		Limits:         opts.Limits,
		Network:        opts.Network,
		AgentOpts:      opts.AgentOpts,
		Store:          opts.Store,
		Kernel:         opts.Kernel, // overwritten to nil by forwardToRun
		Bus:            opts.Bus,
		Refine:         opts.Refine,
		Governance:     opts.Governance,
		AuditLog:       opts.AuditLog,
		Budget:         opts.Budget,
		RunID:          opts.RunID,
		Langfuse:       opts.Langfuse,
		Registry:       opts.Registry, // forwarded to Single/Refine path only
	}
}
