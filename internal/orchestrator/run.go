// Package orchestrator wires sandbox + agent + auth + git into a single Run.
package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/agent"
	"github.com/felipemarques-rec/sandcode/internal/auth"
	"github.com/felipemarques-rec/sandcode/internal/brain"
	"github.com/felipemarques-rec/sandcode/internal/budget"
	"github.com/felipemarques-rec/sandcode/internal/event"
	gitm "github.com/felipemarques-rec/sandcode/internal/git"
	"github.com/felipemarques-rec/sandcode/internal/governance"
	"github.com/felipemarques-rec/sandcode/internal/kernel"
	"github.com/felipemarques-rec/sandcode/internal/langfuse"
	sclog "github.com/felipemarques-rec/sandcode/internal/log"
	"github.com/felipemarques-rec/sandcode/internal/redact"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
	"github.com/felipemarques-rec/sandcode/internal/store"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// RunOptions configures a single-agent invocation.
type RunOptions struct {
	// Prompt sent to the agent.
	Prompt string

	// CWD is the host repo we run against.
	CWD string

	// SandboxImage is the container image to launch.
	SandboxImage string

	// SandboxWorkDir is the path the worktree will be mounted at inside the sandbox.
	SandboxWorkDir string

	// Strategy is the git worktree handling mode.
	Strategy gitm.Strategy

	// KeepWorktree skips the cleanup step at the end (handy for debugging).
	KeepWorktree bool

	// Timeout caps the total wall-clock time of the run. Zero = no cap.
	Timeout time.Duration

	// Limits forwarded to the sandbox.
	Limits sandbox.Limits

	// Network forwarded to the sandbox.
	Network string

	// Agent invocation knobs.
	AgentOpts agent.RunOptions

	// Store, when non-nil, persists the run lifecycle and events. The Run
	// works without a Store; this just turns on history.
	Store store.Store

	// ParentRunID, when set, marks this run as a child of a parallel parent.
	// Used by orchestrator.ParallelRun in Phase 3.
	ParentRunID string

	// Brain, when non-nil, enables the cognitive learning loop.
	// Pre-run: enriches the prompt with system context and lessons.
	// Post-run: extracts lessons from the outcome.
	// The Run works without a Brain; this is opt-in via --learn.
	Brain brain.Brain

	// Kernel, when non-nil, owns the cognitive pre-run pipeline
	// (classify → recall → enrich) and emits structured events on its bus.
	// Mutually superseding with Brain.Enrich: if Kernel is set, the
	// orchestrator calls Kernel.Process() instead of opts.Brain.Enrich().
	// Brain is still used post-run for Learn().
	Kernel *kernel.Kernel

	// Bus, when non-nil, receives run-lifecycle events:
	// run.submitted, sandbox.created/destroyed, agent.executing/completed,
	// run.completed/failed/cancelled. Cognitive events (run.classified,
	// run.enriched, brain.lesson_recalled) are emitted by the Kernel.
	Bus event.Bus

	// Refine, when Enabled and configured with a VerifyCmd, runs the
	// verifier in the sandbox after each agent attempt and re-invokes
	// the agent with failure feedback up to MaxAttempts times. See
	// internal/orchestrator/refine.go for the lifecycle contract.
	Refine RefineOptions

	// Governance, when non-nil, gates ActionExecute (before agent start)
	// and ActionRefine (before each refine iteration). A Deny aborts the
	// run with run.failed; a Review proceeds with a logged warning.
	// AuditLog (optional) persists every decision.
	Governance *governance.Engine

	// AuditLog, when non-nil, receives one audit row per governance
	// decision. Failures are logged but never block the run.
	AuditLog governance.AuditLog

	// Budget, when non-nil, accumulates per-run consumption (attempts,
	// tokens, cost) that policies can read via Action.TokensUsed /
	// Action.CostUSD. The orchestrator records each agent attempt; LLM
	// callers (kernel, judge) record tokens/cost when configured.
	Budget *budget.Guard

	// RunID, when set, is used as the run's unique identifier instead of
	// the auto-generated UUID. Callers (e.g. the HTTP server) pre-allocate
	// it so they can return it in the 202 Location header before Run()
	// starts emitting events. Must be unique per run; the orchestrator
	// does not validate that.
	RunID string

	// Langfuse, when non-nil, instruments the run with LLM observability
	// traces exported to Langfuse via OpenTelemetry. Captures:
	//   - Full run lifecycle span (sandcode.orchestrator.run)
	//   - Agent execution as LLM generation (gen_ai.* attributes)
	//   - Brain enrichment + learning spans
	//   - Judge evaluation scores
	Langfuse *langfuse.Provider

	// Registry, when non-nil, enables opt-in role-based agent resolution.
	// Only the Implementer role is consulted (SP1 — Agent Role Registry phase 1;
	// see docs/superpowers/specs/2026-05-17-agent-role-registry-sp1-design.md).
	// nil ⇒ legacy positional-agent behavior (byte-identical). If Resolve
	// returns ErrRoleNotFound the positional ag argument is used unchanged.
	Registry agent.Registry

	// Verifier, when non-nil, replaces the default command-based verification
	// (RunOptions.Refine.VerifyCmd) for this run (SP2a — coordination spine).
	// nil ⇒ Run builds the default cmdVerifier from Refine.VerifyCmd, which is
	// byte-identical to the legacy behavior. Single-run only: ParallelOptions
	// and DAGOptions deliberately do NOT carry this field.
	Verifier Verifier
}

// Result is the outcome of a single Run.
type Result struct {
	RunID    string
	Worktree *gitm.Worktree
	Diff     string
	ExitCode int
	Status   string // "success" | "failure" | "cancelled"
	Err      error
	Started  time.Time
	Finished time.Time

	// Attempts is the total number of agent invocations performed —
	// 1 for runs without refine, 1..MaxAttempts when refine is active.
	Attempts int

	// LastVerify is the verdict of the most recent Verifier run during this
	// run, or nil when no verification ran (refine inactive / no Verifier).
	// Surfaced so a Reporter can describe verification (SP2a.1).
	LastVerify *VerifyOutput
}

// Run executes one agent invocation in one sandbox. Events are streamed on
// the returned channel; the channel closes when the run is over. The Result
// is delivered separately via the callback so the caller can handle both
// streaming UI and final state.
//
// Langfuse ordering contract: when opts.Langfuse is enabled, callers MUST
// drain the events channel to completion before invoking the result
// callback. The run span is ended inside the streaming goroutine right
// before close(events); draining first guarantees the (child) run span has
// ended — and synchronously exported — before any enclosing span (e.g. the
// orchestrator.Execute root) ends in the wrapped result callback. Calling
// the callback without draining risks a parent span closing before its
// child, the same happens-before hazard that motivated the G1 fix.
func Run(
	ctx context.Context,
	sb sandbox.Provider,
	ag agent.Provider,
	au auth.Provider,
	opts RunOptions,
) (<-chan agent.StreamEvent, func() Result, error) {
	if opts.Prompt == "" {
		return nil, nil, errors.New("orchestrator: empty prompt")
	}
	if opts.CWD == "" {
		return nil, nil, errors.New("orchestrator: empty CWD")
	}
	if opts.SandboxImage == "" {
		return nil, nil, errors.New("orchestrator: empty SandboxImage")
	}
	if opts.SandboxWorkDir == "" {
		opts.SandboxWorkDir = "/workspace"
	}
	if opts.Strategy == "" {
		opts.Strategy = gitm.StrategyMergeToHead
	}

	// Role-based Implementer resolution (SP1). Opt-in: only active when
	// opts.Registry is non-nil. On success the resolved provider replaces
	// the positional ag for the remainder of this run. Any Resolve error
	// (only ErrRoleNotFound is possible in SP1) leaves ag unchanged —
	// preserving byte-identical legacy behavior.
	if opts.Registry != nil {
		if p, err := opts.Registry.Resolve(ctx, agent.RoleImplementer); err == nil {
			ag = p
		}
	}

	runID := opts.RunID
	if runID == "" {
		runID = uuid.New().String()[:8]
	}
	started := time.Now()

	// Set up correlation-aware logging for the entire run.
	ctx = sclog.WithCorrelation(ctx, runID)
	logger := sclog.Logger(ctx)

	// Langfuse: start root trace span for the entire run. The span is
	// ended inside the streaming goroutine (after the run completes),
	// NOT here — Run() returns immediately after spawning that goroutine,
	// so a func-level defer would end the span before the agent runs.
	var runSpan trace.Span
	if opts.Langfuse != nil && opts.Langfuse.Enabled() {
		ctx, runSpan = opts.Langfuse.SpanBrain(ctx, "run",
			attribute.String("sandcode.run_id", runID),
			attribute.String("sandcode.agent", ag.Name()),
			attribute.String("sandcode.sandbox", sb.Name()),
			attribute.String("sandcode.strategy", string(opts.Strategy)),
		)
	}

	// publish emits a lifecycle event on the configured bus. No-op when
	// opts.Bus is nil. Marshal/publish failures are logged but never
	// fail the run — events are observability, not correctness.
	publish := func(ctx context.Context, typ event.Type, payload any) {
		if opts.Bus == nil {
			return
		}
		var raw []byte
		if payload != nil {
			b, err := json.Marshal(payload)
			if err != nil {
				logger.Warn("event marshal failed", "error", err, "event_type", string(typ))
				b = []byte("{}")
			}
			raw = b
		}
		ev := event.New(typ, runID, raw).WithCorrelation(runID)
		if opts.ParentRunID != "" {
			ev.ParentRunID = opts.ParentRunID
		}
		if err := opts.Bus.Publish(ctx, ev); err != nil {
			logger.Warn("event publish failed", "error", err, "event_type", string(typ))
		}
	}

	// markFailedEarly persists a failure row when an early-return error
	// happens after we'd already announced the run. Used to keep the store
	// consistent for runs that never reached the streaming stage.
	markFailedEarly := func(reason string) {
		if opts.Store == nil {
			return
		}
		if err := opts.Store.CreateRun(context.Background(), store.Run{
			ID:         runID,
			ParentID:   opts.ParentRunID,
			Agent:      ag.Name(),
			Sandbox:    sb.Name(),
			Prompt:     opts.Prompt,
			CWD:        opts.CWD,
			Strategy:   string(opts.Strategy),
			Status:     store.StatusFailure,
			StartedAt:  started,
			FinishedAt: time.Now(),
			ExitCode:   -1,
			DiffPath:   "early-fail: " + reason,
		}); err != nil {
			logger.Error("store: failed to persist early failure", "error", err, "run_id", runID)
		}
	}

	// cleanupWorktree removes the worktree on error-return paths. Cleanup
	// failures are not fatal (the caller already has the originating error)
	// but they ARE surfaced via Warn so we don't lose diagnostics silently.
	cleanupWorktree := func(mgr *gitm.Manager, wt *gitm.Worktree, reason string) {
		if err := mgr.Remove(context.Background(), wt, true); err != nil {
			logger.Warn("worktree cleanup failed", "error", err, "run_id", runID, "reason", reason)
		}
	}

	// closeSandbox releases the sandbox during cleanup, logging cleanup errors.
	closeSandbox := func(box sandbox.Sandbox, reason string) {
		if err := box.Close(context.Background()); err != nil {
			logger.Warn("sandbox close failed", "error", err, "run_id", runID, "reason", reason)
		}
	}

	publish(ctx, event.RunSubmitted, submittedPayload{
		Agent:    ag.Name(),
		Sandbox:  sb.Name(),
		Strategy: string(opts.Strategy),
	})

	// evaluatePolicy is the per-call-site wrapper that builds an Action,
	// runs the engine, persists the audit row, and emits the appropriate
	// event. It returns the decision so callers can branch on Allow /
	// Review / Deny. Defensive: when no Governance is configured, returns
	// an Allow Decision so callers can use the same code path uniformly.
	evaluatePolicy := func(actionType governance.ActionType, attempt, diffSize int) governance.Decision {
		if opts.Governance == nil {
			return governance.Decision{Result: governance.Allow}
		}
		act := governance.Action{
			Type:     actionType,
			RunID:    runID,
			Agent:    ag.Name(),
			Strategy: string(opts.Strategy),
			Prompt:   opts.Prompt,
			Attempt:  attempt,
			DiffSize: diffSize,
		}
		if opts.Budget != nil {
			rep := opts.Budget.Report(runID)
			act.TokensUsed = rep.Tokens
			act.CostUSD = rep.CostUSD
		}
		d := opts.Governance.Evaluate(ctx, act)
		if opts.AuditLog != nil {
			if err := governance.LogDecision(ctx, opts.AuditLog, runID, act, d); err != nil {
				logger.Warn("audit: log decision failed",
					"error", err, "run_id", runID, "action", string(actionType))
			}
		}
		switch d.Result {
		case governance.Deny:
			publish(ctx, event.GovernanceDenied, governanceDeniedPayload{
				Action:  string(actionType),
				Attempt: attempt,
				Reasons: d.Reasons,
			})
		case governance.Review:
			publish(ctx, event.GovernanceApprovalRequired, governanceReviewPayload{
				Action:  string(actionType),
				Attempt: attempt,
				Reasons: d.Reasons,
			})
		}
		return d
	}

	// Pre-run governance gate. A Deny here is the cheapest possible
	// rejection — no worktree, no sandbox, no API spend.
	if d := evaluatePolicy(governance.ActionExecute, 1, 0); d.Result == governance.Deny {
		reason := strings.Join(d.Reasons, "; ")
		markFailedEarly("governance: " + reason)
		publish(ctx, event.RunFailed, failedPayload{Reason: "governance", Error: reason})
		return nil, nil, fmt.Errorf("governance: denied: %s", reason)
	}

	// 1. Create worktree
	wtMgr := gitm.NewManager()
	wtDir := filepath.Join(opts.CWD, ".sandcode", "work", runID, "0")
	branch := fmt.Sprintf("sandcode/run-%s-%s", runID, ag.Name())
	wt, err := wtMgr.Create(ctx, opts.CWD, wtDir, branch)
	if err != nil {
		markFailedEarly("worktree: " + err.Error())
		publish(ctx, event.RunFailed, failedPayload{Reason: "worktree", Error: err.Error()})
		return nil, nil, fmt.Errorf("worktree create: %w", err)
	}

	// 2. Build sandbox spec — bind-mount worktree into container
	spec := sandbox.SandboxSpec{
		Image:   opts.SandboxImage,
		WorkDir: opts.SandboxWorkDir,
		Mounts: []sandbox.Mount{
			{Source: wt.Path, Target: opts.SandboxWorkDir, ReadOnly: false},
		},
		Env:     map[string]string{},
		Network: opts.Network,
		Limits:  opts.Limits,
		Labels:  map[string]string{"sandcode.run": runID, "sandcode.agent": ag.Name()},
	}

	// 3. Apply auth
	if au != nil {
		if err := au.Apply(&spec, ag.AuthHints()); err != nil {
			cleanupWorktree(wtMgr, wt, "auth-failed")
			markFailedEarly("auth: " + err.Error())
			return nil, nil, fmt.Errorf("auth: %w", err)
		}
	}

	// 4. Apply timeout
	runCtx := ctx
	var cancel context.CancelFunc
	if opts.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
	} else {
		runCtx, cancel = context.WithCancel(ctx)
	}

	// 5. Launch sandbox
	box, err := sb.Create(runCtx, spec)
	if err != nil {
		cancel()
		cleanupWorktree(wtMgr, wt, "sandbox-create-failed")
		markFailedEarly("sandbox: " + err.Error())
		publish(ctx, event.RunFailed, failedPayload{Reason: "sandbox", Error: err.Error()})
		return nil, nil, fmt.Errorf("sandbox create: %w", err)
	}
	publish(ctx, event.SandboxCreated, sandboxCreatedPayload{
		Image:   opts.SandboxImage,
		WorkDir: opts.SandboxWorkDir,
	})

	// 6. Enrich prompt. Kernel.Process() is the preferred path — it emits
	// run.classified / run.enriched / brain.lesson_recalled events. The
	// direct Brain.Enrich() path remains for callers that haven't migrated.
	enrichedPrompt := opts.Prompt
	switch {
	case opts.Kernel != nil:
		pr := opts.Kernel.Process(ctx, kernel.ProcessRequest{
			Prompt: opts.Prompt,
			CWD:    opts.CWD,
			RunID:  runID,
		})
		enrichedPrompt = pr.EnrichedPrompt
	case opts.Brain != nil:
		if ep, err := opts.Brain.Enrich(ctx, opts.Prompt, opts.CWD); err != nil {
			logger.Warn("brain enrichment failed, using raw prompt", "error", err)
		} else {
			enrichedPrompt = ep
			logger.Info("prompt enriched by brain",
				"original_len", len(opts.Prompt),
				"enriched_len", len(enrichedPrompt))
		}
	}

	// 7. Build agent command
	agentOpts := opts.AgentOpts
	agentOpts.Prompt = enrichedPrompt
	agentOpts.WorkDir = opts.SandboxWorkDir
	cmd := ag.BuildCommand(agentOpts)

	publish(ctx, event.AgentExecuting, agentExecutingPayload{
		Agent: ag.Name(),
		Model: opts.AgentOpts.Model,
	})

	lines, wait, err := box.Exec(runCtx, cmd.Argv, cmd.Stdin, sandbox.ExecOptions{Env: cmd.Env})
	if err != nil {
		closeSandbox(box, "exec-failed")
		cancel()
		cleanupWorktree(wtMgr, wt, "exec-failed")
		markFailedEarly("exec: " + err.Error())
		publish(ctx, event.RunFailed, failedPayload{Reason: "exec", Error: err.Error()})
		return nil, nil, fmt.Errorf("agent exec: %w", err)
	}

	// Now that the run is genuinely under way, persist the canonical row
	// (status=running). Subsequent UpdateRun in the finalizer goroutine
	// flips it to success/failure/cancelled.
	if opts.Store != nil {
		if err := opts.Store.CreateRun(context.Background(), store.Run{
			ID:        runID,
			ParentID:  opts.ParentRunID,
			Agent:     ag.Name(),
			Sandbox:   sb.Name(),
			Prompt:    opts.Prompt,
			CWD:       opts.CWD,
			Strategy:  string(opts.Strategy),
			Status:    store.StatusRunning,
			StartedAt: started,
		}); err != nil {
			logger.Error("store: failed to create run", "error", err, "run_id", runID)
		}
	}

	events := make(chan agent.StreamEvent, 64)
	resultCh := make(chan Result, 1)

	// 7+8. Combined attempt-loop + finalize. The two phases live in one
	// goroutine so we can iterate (verify → refine → re-exec) without juggling
	// inter-goroutine handoffs. The stream/wait pair is consumed sequentially
	// per attempt; only the public `events` channel is shared with the caller.
	go func() {
		// Defer order is load-bearing: close(events) is registered first
		// so it runs LAST; runSpan.End() is registered second so it runs
		// BEFORE close(events). The synchronous span exporter flushes on
		// End, so ending the span before the events channel closes gives
		// callers a deterministic happens-before: once the events channel
		// is drained, the run span is already exported. Flipping this
		// (span ends after close) loses that guarantee and makes
		// span-observing tests flaky under scheduler contention. The
		// production effect of the order is nanoseconds and either order
		// fully covers the run (all real work completes before any defer).
		defer close(events)
		if runSpan != nil {
			defer runSpan.End()
		}

		// drainStream pulls every line emitted by an Exec invocation,
		// applies redaction, persists to the store when configured, and
		// forwards parsed events to the caller. Returns when the exec
		// closes the lines channel — this happens AFTER the command exits.
		var seq int64
		drainStream := func(in <-chan sandbox.ExecLine) {
			for ln := range in {
				ev, ok := ag.ParseLine(redact.Redact(ln.Text))
				if !ok {
					continue
				}
				// Re-redact parsed event payloads in case the agent re-wraps
				// secrets that survived line-level redaction (e.g. inside JSON).
				ev.Text = redact.Redact(ev.Text)
				ev.ToolInput = redact.Redact(ev.ToolInput)
				if opts.Store != nil {
					payload, err := json.Marshal(ev)
					if err != nil {
						logger.Warn("event marshal failed",
							"error", err, "run_id", runID, "kind", ev.Kind.String())
						payload = []byte("{}")
					}
					if err := opts.Store.AppendEvent(context.Background(), runID, store.Event{
						Seq:       seq,
						Timestamp: ev.Timestamp,
						Kind:      ev.Kind.String(),
						Payload:   string(payload),
					}); err != nil {
						// Storing the event failed — log loudly. We do NOT abort the
						// run: the agent stream must keep flowing or the sandbox stalls.
						logger.Error("store: failed to append event",
							"error", err, "run_id", runID, "seq", seq, "kind", ev.Kind.String())
					}
					seq++
				}
				events <- ev
			}
		}

		refineCfg := opts.Refine.effective()
		attempts := 1
		currentLines := lines
		currentWait := wait
		var execRes sandbox.ExecResult
		var lastVerify *VerifyOutput

		// Record attempt 1 — subsequent attempts are recorded after the
		// refine governance check passes (see below).
		if opts.Budget != nil {
			opts.Budget.RecordAttempt(runID)
		}

	attemptLoop:
		for {
			// 1. Drain this attempt's agent output + wait for its exit.
			drainStream(currentLines)
			execRes = currentWait()

			// 2. Emit agent.completed for THIS attempt. The state machine
			//    needs the per-attempt signal to model the
			//    executing → agent_completed → verifying loop correctly.
			//    DiffSize is filled later (after commit) for the final emission;
			//    here we use 0 because the diff is computed once in finalize.
			publish(ctx, event.AgentCompleted, agentCompletedPayload{
				ExitCode: execRes.ExitCode,
			})

			// 3. Refine considerations.
			if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
				break attemptLoop // timeout — caller-side cancellation
			}
			if !opts.Refine.active() {
				break attemptLoop // single-attempt mode
			}
			if execRes.ExitCode != 0 {
				// Agent crashed — refining a broken agent won't help. Bail
				// out to surface the original failure.
				break attemptLoop
			}

			// 4. Run verify in the same sandbox (worktree state intact).
			publish(ctx, event.VerifyStarted, verifyStartedPayload{
				Cmd:     opts.Refine.VerifyCmd,
				Attempt: attempts,
			})
			vStart := time.Now()

			// Resolve the Verifier: explicit opts.Verifier wins; otherwise the
			// default cmdVerifier reproduces the legacy VerifyCmd behavior.
			verifier := opts.Verifier
			if verifier == nil {
				verifier = cmdVerifier{cmd: opts.Refine.VerifyCmd}
			}

			vOut, vErr := verifier.Verify(runCtx, VerifyInput{
				Box:       box,
				Attempt:   attempts,
				Timeout:   refineCfg.VerifyTimeout,
				TailBytes: refineCfg.VerifyTailBytes,
			})
			if vErr != nil {
				publish(ctx, event.VerifyFailed, verifyFailedPayload{
					Attempt: attempts,
					Error:   vErr.Error(),
				})
				execRes.Err = fmt.Errorf("verify exec: %w", vErr)
				execRes.ExitCode = -1
				break attemptLoop
			}
			lv := vOut
			lastVerify = &lv
			vTail := vOut.StdoutTail
			vDuration := time.Since(vStart).Milliseconds()

			if vOut.Passed {
				publish(ctx, event.VerifyPassed, verifyPassedPayload{
					Attempt:    attempts,
					DurationMS: vDuration,
				})
				break attemptLoop // success
			}

			publish(ctx, event.VerifyFailed, verifyFailedPayload{
				Attempt:    attempts,
				ExitCode:   vOut.ExitCode,
				StdoutTail: vTail,
			})

			// 5. Verify failed. Refine if local cap allows.
			if attempts >= refineCfg.MaxAttempts {
				// Exhausted — surface as run failure with verify context.
				execRes.ExitCode = vOut.ExitCode
				execRes.Err = fmt.Errorf("verify failed after %d attempt(s): exit %d",
					attempts, vOut.ExitCode)
				break attemptLoop
			}

			// 5a. Governance gate on the upcoming refine iteration. A Deny
			//     here short-circuits the loop with run.failed; useful when
			//     BudgetPolicy or RetryLimit caps fire before the local
			//     RefineOptions.MaxAttempts limit would have.
			nextAttempt := attempts + 1
			if d := evaluatePolicy(governance.ActionRefine, nextAttempt, 0); d.Result == governance.Deny {
				reason := strings.Join(d.Reasons, "; ")
				execRes.ExitCode = vOut.ExitCode
				execRes.Err = fmt.Errorf("governance denied refine: %s", reason)
				break attemptLoop
			}

			attempts++
			publish(ctx, event.RefineTriggered, refineTriggeredPayload{
				Attempt:     attempts,
				MaxAttempts: refineCfg.MaxAttempts,
			})

			// Budget: record the new attempt now that policies have approved it.
			if opts.Budget != nil {
				opts.Budget.RecordAttempt(runID)
			}

			// 6. Build refined prompt and re-exec the agent.
			refinedPrompt := buildRefinePrompt(
				enrichedPrompt, vTail, attempts, refineCfg.MaxAttempts, opts.Refine.VerifyCmd,
			)
			nextAgentOpts := opts.AgentOpts
			nextAgentOpts.Prompt = refinedPrompt
			nextAgentOpts.WorkDir = opts.SandboxWorkDir
			nextCmd := ag.BuildCommand(nextAgentOpts)

			publish(ctx, event.AgentExecuting, agentExecutingPayload{
				Agent: ag.Name(),
				Model: opts.AgentOpts.Model,
			})

			nextLines, nextWait, eErr := box.Exec(runCtx, nextCmd.Argv, nextCmd.Stdin,
				sandbox.ExecOptions{Env: nextCmd.Env})
			if eErr != nil {
				execRes.ExitCode = -1
				execRes.Err = fmt.Errorf("refine exec: %w", eErr)
				break attemptLoop
			}
			currentLines = nextLines
			currentWait = nextWait
		}

		// === Single-pass finalization (runs once regardless of attempts) ===

		cancel() // release runCtx now that all attempts are done

		status := "success"
		if execRes.ExitCode != 0 || execRes.Err != nil {
			status = "failure"
		}
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			status = "cancelled"
		}

		// Commit changes inside the worktree so diff/merge see them as commits.
		// Best-effort: an empty worktree (no agent changes) is a normal outcome
		// that surfaces here as a "nothing to commit" error from git.
		if err := wtMgr.CommitAll(context.Background(), wt,
			fmt.Sprintf("sandcode: %s run %s", ag.Name(), runID)); err != nil {
			logger.Debug("worktree commit (best-effort) returned",
				"error", err, "run_id", runID)
		}

		diff, _ := wtMgr.Diff(context.Background(), wt)
		// Redact secrets once at the source: this diff flows to the store,
		// REPORT.md, the brain, and the LLM judge/reviewer (incl. the API).
		diff = redact.Redact(diff)

		// Apply strategy on success
		if status == "success" {
			switch opts.Strategy {
			case gitm.StrategyMergeToHead:
				if err := wtMgr.MergeToHead(context.Background(), wt); err != nil {
					status = "failure"
					execRes.Err = fmt.Errorf("merge-to-head: %w", err)
				}
			case gitm.StrategyBranch:
				// keep branch around, no-op
			}
		}

		closeSandbox(box, "run-finalize")
		publish(ctx, event.SandboxDestroyed, sandboxDestroyedPayload{
			DurationMS: time.Since(started).Milliseconds(),
		})

		if !opts.KeepWorktree {
			cleanupWorktree(wtMgr, wt, "run-finalize")
		}

		finished := time.Now()
		if opts.Store != nil {
			if err := opts.Store.UpdateRun(context.Background(), store.Run{
				ID:         runID,
				Status:     store.RunStatus(status),
				FinishedAt: finished,
				ExitCode:   execRes.ExitCode,
			}); err != nil {
				logger.Error("store: failed to update run", "error", err, "run_id", runID)
			}
		}

		// Post-run learning: extract lessons from the outcome.
		if opts.Brain != nil {
			outcome := brain.Outcome{
				RunID:    runID,
				Agent:    ag.Name(),
				Prompt:   redact.Redact(opts.Prompt), // diff already redacted above
				Diff:     diff,
				Status:   status,
				ExitCode: execRes.ExitCode,
				Duration: finished.Sub(started),
			}
			if n, err := opts.Brain.Learn(context.Background(), outcome); err != nil {
				logger.Warn("brain learning failed", "error", err, "run_id", runID)
			} else if n > 0 {
				logger.Info("brain extracted lessons", "count", n, "run_id", runID)
			}
		}

		switch status {
		case "success":
			publish(ctx, event.RunCompleted, completedPayload{
				ExitCode:   execRes.ExitCode,
				DurationMS: finished.Sub(started).Milliseconds(),
				DiffSize:   len(diff),
			})
		case "cancelled":
			publish(ctx, event.RunCancelled, completedPayload{
				ExitCode:   execRes.ExitCode,
				DurationMS: finished.Sub(started).Milliseconds(),
			})
		default:
			errMsg := ""
			if execRes.Err != nil {
				errMsg = execRes.Err.Error()
			}
			publish(ctx, event.RunFailed, failedPayload{
				Reason: "agent",
				Error:  errMsg,
			})
		}

		resultCh <- Result{
			RunID:      runID,
			Worktree:   wt,
			Diff:       diff,
			ExitCode:   execRes.ExitCode,
			Status:     status,
			Err:        execRes.Err,
			Started:    started,
			Finished:   finished,
			Attempts:   attempts,
			LastVerify: lastVerify,
		}
		close(resultCh)
	}()

	awaitResult := func() Result { return <-resultCh }
	return events, awaitResult, nil
}

// Event payload shapes — emitted via opts.Bus when configured.
// Stable JSON contract: subscribers (event store, metrics, CLI) depend on
// these field names. Add fields freely; never rename or remove without a
// schema version bump.

type submittedPayload struct {
	Agent    string `json:"agent"`
	Sandbox  string `json:"sandbox"`
	Strategy string `json:"strategy"`
}

type sandboxCreatedPayload struct {
	Image   string `json:"image"`
	WorkDir string `json:"workdir"`
}

type sandboxDestroyedPayload struct {
	DurationMS int64 `json:"duration_ms"`
}

type agentExecutingPayload struct {
	Agent string `json:"agent"`
	Model string `json:"model,omitempty"`
}

type agentCompletedPayload struct {
	ExitCode int `json:"exit_code"`
	DiffSize int `json:"diff_size"`
}

type completedPayload struct {
	ExitCode   int   `json:"exit_code"`
	DurationMS int64 `json:"duration_ms"`
	DiffSize   int   `json:"diff_size,omitempty"`
}

type failedPayload struct {
	Reason string `json:"reason"`
	Error  string `json:"error,omitempty"`
}

type verifyStartedPayload struct {
	Cmd     []string `json:"cmd"`
	Attempt int      `json:"attempt"`
}

type verifyPassedPayload struct {
	Attempt    int   `json:"attempt"`
	DurationMS int64 `json:"duration_ms"`
}

type verifyFailedPayload struct {
	Attempt    int    `json:"attempt"`
	ExitCode   int    `json:"exit_code"`
	StdoutTail string `json:"stdout_tail,omitempty"`
	Error      string `json:"error,omitempty"`
}

type refineTriggeredPayload struct {
	Attempt     int `json:"attempt"`
	MaxAttempts int `json:"max_attempts"`
}

type governanceDeniedPayload struct {
	Action  string   `json:"action"`
	Attempt int      `json:"attempt,omitempty"`
	Reasons []string `json:"reasons,omitempty"`
}

type governanceReviewPayload struct {
	Action  string   `json:"action"`
	Attempt int      `json:"attempt,omitempty"`
	Reasons []string `json:"reasons,omitempty"`
}
