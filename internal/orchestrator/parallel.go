package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/agent"
	"github.com/felipemarques-rec/sandcode/internal/auth"
	"github.com/felipemarques-rec/sandcode/internal/brain"
	"github.com/felipemarques-rec/sandcode/internal/budget"
	"github.com/felipemarques-rec/sandcode/internal/event"
	gitm "github.com/felipemarques-rec/sandcode/internal/git"
	"github.com/felipemarques-rec/sandcode/internal/governance"
	"github.com/felipemarques-rec/sandcode/internal/judge"
	"github.com/felipemarques-rec/sandcode/internal/kernel"
	"github.com/felipemarques-rec/sandcode/internal/langfuse"
	sclog "github.com/felipemarques-rec/sandcode/internal/log"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
	"github.com/felipemarques-rec/sandcode/internal/store"
	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"
)

// ParallelOptions configures a multi-agent fan-out execution.
type ParallelOptions struct {
	// Prompt is sent verbatim to every agent.
	Prompt string

	// CWD is the host repo we run against.
	CWD string

	// SandboxImage is shared across all sub-runs.
	SandboxImage string

	// SandboxWorkDir is the in-container working directory for each agent.
	SandboxWorkDir string

	// Strategy applies to each sub-run. With merge-to-head, only the winner
	// (selected by Phase 4 judge) should be merged — for Phase 3 the runner
	// uses StrategyBranch implicitly so multiple successful runs don't fight
	// for HEAD. Set explicitly to override.
	Strategy gitm.Strategy

	// KeepWorktrees keeps every worktree for inspection.
	KeepWorktrees bool

	// Timeout caps wall-clock per sub-run. Zero = no cap.
	Timeout time.Duration

	// Limits forwarded to each sandbox.
	Limits sandbox.Limits

	// Network forwarded to each sandbox.
	Network string

	// MaxConcurrency caps how many sub-runs may execute simultaneously.
	// 0 = run all concurrently. Useful when --auth-mode=bindmount + many
	// Claude Code agents to stay under shared-subscription rate limits.
	MaxConcurrency int

	// Agents is the list of agents to run. One sub-run per agent.
	Agents []agent.Provider

	// AgentOpts shared across all runs (model, effort, etc.). Per-agent
	// overrides aren't supported yet — keep it simple in Phase 3.
	AgentOpts agent.RunOptions

	// Store, when non-nil, persists parent + children + events.
	Store store.Store

	// Judge, when non-nil, ranks the sub-results after all sub-runs finish.
	// When set together with Strategy=merge-to-head, the winner's worktree
	// is merged back into HEAD; losers stay on their branches for review.
	Judge judge.Judge

	// MergeWinner forces the winner's worktree to be merged into HEAD even
	// when Strategy is "branch". Useful with --judge but custom strategy.
	MergeWinner bool

	// Brain, when non-nil, is forwarded to each child Run for post-run learning.
	Brain brain.Brain

	// Kernel, when non-nil, is forwarded to each child Run for cognitive
	// enrichment + event emission. Mutually superseding with Brain pre-run.
	Kernel *kernel.Kernel

	// Bus, when non-nil, receives lifecycle events from each child Run.
	Bus event.Bus

	// Governance / AuditLog / Budget are forwarded to each child Run.
	// All sub-runs share the same engine so policy decisions are uniform
	// and the same Budget accumulates across them (per-parent ceilings).
	Governance *governance.Engine
	AuditLog   governance.AuditLog
	Budget     *budget.Guard

	// Refine, when Enabled, applies independently to each sub-run.
	// Every agent runs its own verify+refine loop in its own worktree.
	// Cost multiplies by N (one loop per agent) — operators cap via
	// Budget/RetryLimit policies. The judge ranks the final attempt
	// of each agent regardless of which iteration produced it.
	Refine RefineOptions

	// Langfuse, when non-nil, provides OTel instrumentation for LLM observability.
	Langfuse *langfuse.Provider
}

// SubResult is the outcome of one fan-out branch.
type SubResult struct {
	Agent  string
	Result Result
	Events []agent.StreamEvent
}

// ParallelResult aggregates a fan-out execution.
type ParallelResult struct {
	ParentRunID string
	Started     time.Time
	Finished    time.Time
	Sub         []SubResult
	Ranking     *judge.Ranking // nil when no judge configured or it failed
	JudgeErr    error          // populated when judge ran but failed
	WinnerErr   error          // populated when winner-merge failed
}

// ParallelRun runs `agents` concurrently against the same prompt, each in
// its own worktree slot. Events are streamed back per-agent on the returned
// channel; reading the channel to completion is required (drain it) before
// calling the await closure.
func ParallelRun(
	ctx context.Context,
	sb sandbox.Provider,
	au auth.Provider,
	opts ParallelOptions,
) (<-chan SubEvent, func() ParallelResult, error) {
	if len(opts.Agents) == 0 {
		return nil, nil, errors.New("parallel: no agents")
	}
	if opts.SandboxImage == "" {
		return nil, nil, errors.New("parallel: empty SandboxImage")
	}
	if opts.SandboxWorkDir == "" {
		opts.SandboxWorkDir = "/workspace"
	}
	if opts.Strategy == "" {
		// Default to branch in parallel: choosing a winner happens in Phase 4.
		opts.Strategy = gitm.StrategyBranch
	}

	// When the judge will need to merge a winner we MUST keep worktrees
	// around — otherwise per-sub-run cleanup removes the branches before
	// we can merge. The judge prompt also benefits from comparing real diffs,
	// which are captured before cleanup either way, but the merge step needs
	// the live branch.
	willMergeWinner := opts.Judge != nil &&
		(opts.Strategy == gitm.StrategyMergeToHead || opts.MergeWinner)
	if willMergeWinner {
		opts.KeepWorktrees = true
	}

	// Inside Run() each sub-run interprets Strategy literally. With merge-to-head
	// + parallel, multiple successful runs would each try to merge to HEAD and
	// conflict. Force per-sub-run strategy to "branch"; the parent merges only
	// the winner. The user-facing Strategy stays as configured for reporting.
	subStrategy := opts.Strategy
	if subStrategy == gitm.StrategyMergeToHead && len(opts.Agents) > 1 {
		subStrategy = gitm.StrategyBranch
	}

	parentID := uuid.New().String()[:8]
	started := time.Now()
	logger := sclog.Logger(sclog.WithCorrelation(ctx, parentID))

	if opts.Store != nil {
		if err := opts.Store.CreateRun(ctx, store.Run{
			ID:        parentID,
			Agent:     "parallel",
			Sandbox:   sb.Name(),
			Prompt:    opts.Prompt,
			CWD:       opts.CWD,
			Strategy:  string(opts.Strategy),
			Status:    store.StatusRunning,
			StartedAt: started,
		}); err != nil {
			logger.Error("store: failed to create parallel run", "error", err)
		}
	}

	out := make(chan SubEvent, 64*len(opts.Agents))
	results := make([]SubResult, len(opts.Agents))

	g, gctx := errgroup.WithContext(ctx)
	if opts.MaxConcurrency > 0 {
		g.SetLimit(opts.MaxConcurrency)
	}

	var mu sync.Mutex // guards results slot writes

	for i, ag := range opts.Agents {
		i, ag := i, ag
		g.Go(func() error {
			runOpts := RunOptions{
				Prompt:         opts.Prompt,
				CWD:            opts.CWD,
				SandboxImage:   opts.SandboxImage,
				SandboxWorkDir: opts.SandboxWorkDir,
				Strategy:       subStrategy,
				KeepWorktree:   opts.KeepWorktrees,
				Timeout:        opts.Timeout,
				Network:        opts.Network,
				Limits:         opts.Limits,
				AgentOpts:      opts.AgentOpts,
				Store:          opts.Store,
				ParentRunID:    parentID,
				Brain:          opts.Brain,
				Kernel:         opts.Kernel,
				Bus:            opts.Bus,
				Langfuse:       opts.Langfuse,
				Governance:     opts.Governance,
				AuditLog:       opts.AuditLog,
				Budget:         opts.Budget,
				Refine:         opts.Refine,
			}
			runOpts.AgentOpts.WorkDir = filepath.Join(opts.SandboxWorkDir)

			events, await, err := Run(gctx, sb, ag, au, runOpts)
			if err != nil {
				return fmt.Errorf("%s: %w", ag.Name(), err)
			}
			var collected []agent.StreamEvent
			for ev := range events {
				collected = append(collected, ev)
				// Always relay events; out is generously buffered. Dropping on
				// gctx.Done() (errgroup-cancel after a sibling failure) loses
				// useful diagnostics from the still-running sub-runs.
				out <- SubEvent{Agent: ag.Name(), Slot: i, Event: ev}
			}
			res := await()

			mu.Lock()
			results[i] = SubResult{Agent: ag.Name(), Result: res, Events: collected}
			mu.Unlock()
			return nil
		})
	}

	// Close `out` as soon as all sub-runs are done so the consumer's
	// `for ev := range events` terminates without needing to call await first.
	// We don't propagate g.Wait()'s error — failures are already captured in
	// per-sub Result.Status — but we DO surface it at Warn so transport/setup
	// failures (e.g. sandbox creation) are not silently swallowed.
	doneCh := make(chan struct{})
	go func() {
		if err := g.Wait(); err != nil {
			logger.Warn("parallel: at least one sub-run errored", "error", err, "parent_id", parentID)
		}
		close(out)
		close(doneCh)
	}()

	awaitFn := func() ParallelResult {
		<-doneCh
		finished := time.Now()

		pr := ParallelResult{
			ParentRunID: parentID,
			Started:     started,
			Finished:    finished,
			Sub:         results,
		}

		// Run the judge once everything has settled. Failures here do not
		// fail the overall run — they're surfaced via JudgeErr.
		if opts.Judge != nil {
			var finishEval func(float64, string, error)
			if opts.Langfuse != nil && opts.Langfuse.Enabled() {
				ctx, finishEval = opts.Langfuse.InstrumentJudge(ctx, "ranking", parentID)
			}
			ranking, err := runJudge(ctx, opts.Judge, opts.Prompt, results)
			if finishEval != nil {
				if err != nil {
					finishEval(0, "", err)
				} else if score, ok := ranking.Scores[ranking.Winner]; ok {
					finishEval(score, ranking.Rationale, nil)
				} else {
					finishEval(0, ranking.Rationale, nil)
				}
			}
			if err != nil {
				pr.JudgeErr = err
			} else {
				pr.Ranking = &ranking
				if opts.Store != nil {
					if err := opts.Store.SaveRanking(context.Background(), store.Ranking{
						ParentRunID: parentID,
						Judge:       ranking.Judge,
						WinnerRunID: ranking.Winner,
						Scores:      ranking.Scores,
						Rationale:   ranking.Rationale,
						CreatedAt:   time.Now(),
					}); err != nil {
						logger.Error("store: failed to save ranking", "error", err)
					}
				}
				// Optionally merge the winner back into HEAD.
				if opts.Strategy == gitm.StrategyMergeToHead || opts.MergeWinner {
					if err := mergeWinner(opts.CWD, results, ranking.Winner); err != nil {
						pr.WinnerErr = err
					}
				}
			}
		}

		if opts.Store != nil {
			status := store.StatusSuccess
			for _, r := range results {
				if r.Result.Status != "success" {
					status = store.StatusFailure
					break
				}
			}
			if err := opts.Store.UpdateRun(context.Background(), store.Run{
				ID:         parentID,
				Status:     status,
				FinishedAt: finished,
			}); err != nil {
				logger.Error("store: failed to update parallel run", "error", err)
			}
		}
		return pr
	}
	return out, awaitFn, nil
}

// runJudge calls the configured Judge over the sub-run results.
func runJudge(ctx context.Context, j judge.Judge, prompt string, results []SubResult) (judge.Ranking, error) {
	cands := make([]judge.Candidate, 0, len(results))
	for _, r := range results {
		cands = append(cands, judge.Candidate{
			RunID:    r.Result.RunID,
			Agent:    r.Agent,
			ExitCode: r.Result.ExitCode,
			Status:   r.Result.Status,
			Duration: r.Result.Finished.Sub(r.Result.Started),
			Diff:     r.Result.Diff,
			Stdout:   tailEventText(r.Events, 1500),
		})
	}
	return j.Rank(ctx, prompt, cands)
}

// tailEventText concatenates the textual events of a sub-run and returns
// the last ~n bytes — gives the judge a feel for the agent's output without
// blowing the context window.
func tailEventText(events []agent.StreamEvent, n int) string {
	var b []byte
	for _, ev := range events {
		if ev.Kind == agent.EventText {
			b = append(b, ev.Text...)
			b = append(b, '\n')
		}
	}
	if len(b) <= n {
		return string(b)
	}
	return string(b[len(b)-n:])
}

// mergeWinner merges the winner's worktree branch into HEAD of the source repo.
func mergeWinner(cwd string, results []SubResult, winnerID string) error {
	for _, r := range results {
		if r.Result.RunID != winnerID {
			continue
		}
		if r.Result.Worktree == nil {
			return fmt.Errorf("winner %s has no worktree to merge", winnerID)
		}
		mgr := gitm.NewManager()
		return mgr.MergeToHead(context.Background(), r.Result.Worktree)
	}
	return fmt.Errorf("winner %s not found in results", winnerID)
}

// SubEvent is one event from one of the parallel sub-runs.
type SubEvent struct {
	Agent string
	Slot  int
	Event agent.StreamEvent
}
