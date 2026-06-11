package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/felipemarques-rec/sandcode/internal/agent"
	"github.com/felipemarques-rec/sandcode/internal/auth"
	"github.com/felipemarques-rec/sandcode/internal/event"
	"github.com/felipemarques-rec/sandcode/internal/judge"
	"github.com/felipemarques-rec/sandcode/internal/planner"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
	"github.com/google/uuid"
)

// DAGRun walks a planner.TaskDAG by spawning one root-chain per
// plan.Roots() entry. Chains run in parallel via sync.WaitGroup with
// no cancel-on-error (fail-isolated). After chains complete, the
// judge ranks survivors and a synthesizer agent consolidates work in
// the winner's worktree (skipped for single-chain or all-failed
// cases).
//
// Returns event channel + result callback + entry-time error. The
// returned error is non-nil only for entry-time failures (validation).
// Runtime failures (chain or synthesizer) are reflected in
// DAGResult.Error. The event channel is closed when the run completes
// — Slice 4 uses it as a completion signal; bus subscribers see the
// detailed dag.* events.
func DAGRun(
	ctx context.Context,
	sb sandbox.Provider,
	ag agent.Provider,
	au auth.Provider,
	opts DAGOptions,
) (<-chan DAGEvent, func() DAGResult, error) {
	if err := validateDAG(opts); err != nil {
		return nil, nil, err
	}
	if opts.RunID == "" {
		opts.RunID = uuid.New().String()[:8]
	}

	out := make(chan DAGEvent)

	// Degenerate: single root, single node → fall back to Run. Skip
	// the synthesizer + judge machinery entirely.
	if len(opts.Plan.Nodes) == 1 && len(opts.Plan.Roots()) == 1 {
		return runSingleNodeDAG(ctx, sb, ag, au, opts, out)
	}

	chains := decomposeChains(opts.Plan, opts.Agents)
	emitDAG(opts.Bus, event.DAGStarted, opts.RunID, dagStartedPayload{
		NodeCount:  len(opts.Plan.Nodes),
		RootCount:  len(opts.Plan.Roots()),
		ChainCount: len(chains),
	})

	// DAG grouping span: nests all per-chain spans under one parent.
	// ctx is rebound BEFORE the spawn loop so runChain(ctx, ...) inherits it.
	var dagSpan trace.Span
	if opts.Langfuse != nil && opts.Langfuse.Enabled() {
		ctx, dagSpan = opts.Langfuse.SpanBrain(ctx, "dag",
			attribute.String("sandcode.run_id", opts.RunID),
			attribute.Int("sandcode.dag.nodes", len(opts.Plan.Nodes)),
			attribute.Int("sandcode.dag.roots", len(opts.Plan.Roots())),
			attribute.Int("sandcode.dag.chains", len(chains)),
		)
	}

	started := time.Now()
	results := make([]ChainResult, len(chains))

	var wg sync.WaitGroup
	for i, ch := range chains {
		wg.Add(1)
		go func(idx int, spec chainSpec) {
			defer wg.Done()
			r, _ := runChain(ctx, sb, ag, au, spec, opts)
			results[idx] = r
		}(i, ch)
	}

	// Coordinator: waits for all chains, runs judge + synthesizer,
	// then closes the event channel.
	resultCh := make(chan DAGResult, 1)
	go func() {
		wg.Wait()

		res := DAGResult{
			Plan:           opts.Plan,
			Chains:         results,
			OuterCopyIndex: opts.OuterCopyIndex,
			Started:        started,
		}
		for _, c := range results {
			res.Cost.Tokens += c.Cost.Tokens
			res.Cost.USD += c.Cost.USD
		}

		var succ []ChainResult
		for _, c := range results {
			if c.Success {
				succ = append(succ, c)
			}
		}

		switch len(succ) {
		case 0:
			res.Error = ErrAllChainsFailed
		case 1:
			res.Winner = succ[0].ChainID
			// Skip synthesizer — no peers to consolidate from.
		default:
			jctx := ctx
			var finishEval func(float64, string, error)
			if opts.Langfuse != nil && opts.Langfuse.Enabled() {
				jctx, finishEval = opts.Langfuse.InstrumentJudge(ctx, "ranking", opts.RunID)
			}
			winner, rationale, score := runJudgeOverChains(jctx, opts.Judge, opts.Prompt, succ)
			if finishEval != nil {
				// Real winner score now flows to the Langfuse judge span
				// (evaluation.score). A judge error is folded into the
				// rationale internally and yields score 0, so nil error
				// here is still correct (caller can't distinguish it).
				finishEval(score, rationale, nil)
			}
			res.Winner = winner
			res.JudgeRationale = rationale
			if !opts.Synthesizer.Disabled {
				winnerChain := findChain(succ, winner)
				sctx := ctx
				var synSpan trace.Span
				if opts.Langfuse != nil && opts.Langfuse.Enabled() {
					sctx, synSpan = opts.Langfuse.SpanBrain(ctx, "dag.synthesizer",
						attribute.String("sandcode.run_id", opts.RunID))
				}
				synRes, _ := runSynthesizer(sctx, sb, ag, au, synthesizerArgs{
					WinnerWorktree: winnerChain.Worktree,
					Winner:         winnerChain,
					AllChains:      succ,
					JudgeRationale: rationale,
					OriginalPrompt: opts.Prompt,
					SandboxImage:   opts.SandboxImage,
					SandboxWorkDir: opts.SandboxWorkDir,
					Limits:         opts.Limits,
					Network:        opts.Network,
					AgentOpts:      opts.AgentOpts,
					Refine:         opts.Refine,
					Bus:            opts.Bus,
					RunID:          opts.RunID,
				})
				// Explicit End immediately after the synchronous
				// runSynthesizer returns — NEVER a defer in this
				// coordinator goroutine body. A defer would run after
				// resultCh<-res / close(out), the W13/W13.2 happens-before
				// flake class (a synchronous exporter must flush this span
				// before the caller can drain out + await the result).
				if synSpan != nil {
					synSpan.End()
				}
				res.Synthesizer = &synRes
			}
		}

		res.Finished = time.Now()
		res.Duration = res.Finished.Sub(res.Started)
		emitDAG(opts.Bus, event.DAGCompleted, opts.RunID, dagCompletedPayload{
			TotalNodes:       len(opts.Plan.Nodes),
			TotalAttempts:    sumAttempts(results),
			FailedChainCount: len(results) - len(succ),
			WinnerChainID:    res.Winner,
		})
		// End the dag span EXPLICITLY before resultCh<-res and close(out).
		// W13 happens-before invariant: with a synchronous exporter the span
		// MUST be flushed before the caller can drain out+await. Do NOT
		// convert this to a defer — a defer in the goroutine body runs AFTER
		// resultCh<-res/close(out), which is the W13 flake class.
		if dagSpan != nil {
			dagSpan.End()
		}
		resultCh <- res
		close(out)
	}()

	await := func() DAGResult {
		return <-resultCh
	}
	return out, await, nil
}

// runSingleNodeDAG handles the degenerate "1 root, 1 node" case by
// delegating to Run. The DAGResult wraps the run's outcome as a
// single-chain result with no synthesizer pass.
func runSingleNodeDAG(
	ctx context.Context,
	sb sandbox.Provider,
	ag agent.Provider,
	au auth.Provider,
	opts DAGOptions,
	out chan DAGEvent,
) (<-chan DAGEvent, func() DAGResult, error) {
	// No sandcode.brain.dag grouping span here: the degenerate 1-node
	// path delegates entirely to Run, whose sandcode.brain.run span is
	// the trace container. A dag span would misrepresent the execution
	// model (no chains/judge/synthesizer ran). Asymmetry is intentional.
	emitDAG(opts.Bus, event.DAGStarted, opts.RunID, dagStartedPayload{
		NodeCount:  1,
		RootCount:  1,
		ChainCount: 1,
	})

	runOpts := RunOptions{
		Prompt:         opts.Plan.Nodes[0].Prompt,
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
		ParentRunID:    opts.ParentRunID,
		Kernel:         opts.Kernel,
		Bus:            opts.Bus,
		Langfuse:       opts.Langfuse,
		Refine:         opts.Refine,
		Governance:     opts.Governance,
		AuditLog:       opts.AuditLog,
		Budget:         opts.Budget,
		RunID:          opts.RunID,
		MCP:            opts.MCP,
	}

	started := time.Now()
	events, runAwait, err := Run(ctx, sb, ag, au, runOpts)
	if err != nil {
		close(out)
		return out, func() DAGResult {
			return DAGResult{Plan: opts.Plan, OuterCopyIndex: opts.OuterCopyIndex, Error: err}
		}, nil
	}

	resultCh := make(chan DAGResult, 1)
	go func() {
		// Drain the run's event channel — the orchestrator emits its
		// own events on the bus, which is what subscribers see. We
		// don't forward these into the DAGEvent channel for now.
		for range events {
		}
		runRes := runAwait()

		nodeRes := NodeResult{
			NodeID:   opts.Plan.Nodes[0].ID,
			Prompt:   opts.Plan.Nodes[0].Prompt,
			Attempts: runRes.Attempts,
			Result: AgentInvocationResult{
				ExitCode: runRes.ExitCode,
				Err:      runRes.Err,
				Duration: runRes.Finished.Sub(runRes.Started),
			},
			Diff: runRes.Diff,
		}
		chain := ChainResult{
			ChainID:    "chain-0",
			RootNodeID: opts.Plan.Nodes[0].ID,
			Nodes:      []NodeResult{nodeRes},
			Branch:     "",
			Success:    runRes.Status == "success",
			Started:    runRes.Started,
			Finished:   runRes.Finished,
		}
		if runRes.Worktree != nil {
			chain.Worktree = runRes.Worktree.Path
			chain.Branch = runRes.Worktree.Branch
		}
		if !chain.Success {
			chain.FailedAt = opts.Plan.Nodes[0].ID
		}

		dagRes := DAGResult{
			Plan:           opts.Plan,
			Chains:         []ChainResult{chain},
			OuterCopyIndex: opts.OuterCopyIndex,
			Started:        started,
			Finished:       time.Now(),
		}
		dagRes.Duration = dagRes.Finished.Sub(dagRes.Started)
		if !chain.Success {
			dagRes.Error = ErrAllChainsFailed
		} else {
			dagRes.Winner = chain.ChainID
		}

		emitDAG(opts.Bus, event.DAGCompleted, opts.RunID, dagCompletedPayload{
			TotalNodes:       1,
			TotalAttempts:    runRes.Attempts,
			FailedChainCount: boolToInt(!chain.Success),
			WinnerChainID:    dagRes.Winner,
		})
		resultCh <- dagRes
		close(out)
	}()

	return out, func() DAGResult { return <-resultCh }, nil
}

// decomposeChains builds one chainSpec per root via DFS in topo order.
// Each chain includes the root + all transitive descendants. Diamonds
// are rejected at validateDAG, so each non-root node has exactly one
// parent — no node appears in more than one chain.
//
// When agents is non-empty and len > 1, each chain gets
// agents[chainIndex % len(agents)] as AssignedAgent (round-robin,
// deterministic — same plan + same agents always produces the same
// assignment). When agents is empty or single-element, AssignedAgent
// stays nil and runChain falls back to its `ag` parameter.
func decomposeChains(plan planner.TaskDAG, agents []agent.Provider) []chainSpec {
	roots := plan.Roots()
	specs := make([]chainSpec, 0, len(roots))

	children := map[string][]string{}
	byID := map[string]planner.Node{}
	for _, n := range plan.Nodes {
		byID[n.ID] = n
		for _, dep := range n.DependsOn {
			children[dep] = append(children[dep], n.ID)
		}
	}

	multiAgent := len(agents) > 1
	for i, root := range roots {
		var nodes []planner.Node
		var visit func(string)
		visit = func(id string) {
			nodes = append(nodes, byID[id])
			for _, c := range children[id] {
				visit(c)
			}
		}
		visit(root)
		spec := chainSpec{
			ChainID:    fmt.Sprintf("chain-%d", i),
			RootNodeID: root,
			Nodes:      nodes,
		}
		if multiAgent {
			spec.AssignedAgent = agents[i%len(agents)]
		}
		specs = append(specs, spec)
	}
	return specs
}

// runJudgeOverChains adapts ChainResults into judge.Candidates and
// invokes the judge. Returns (winnerChainID, rationale, score). On judge
// failure / no judge, defaults to the first successful chain with a
// note in the rationale and score 0 — keeps execution moving without a
// brittle hard-fail.
func runJudgeOverChains(ctx context.Context, j judge.Judge, prompt string, chains []ChainResult) (winner string, rationale string, score float64) {
	if j == nil {
		return chains[0].ChainID, "no judge configured; defaulted to first successful chain", 0
	}
	cands := make([]judge.Candidate, 0, len(chains))
	for _, c := range chains {
		var lastDiff, lastStdout string
		exitCode := 0
		if len(c.Nodes) > 0 {
			last := c.Nodes[len(c.Nodes)-1]
			lastDiff = last.Diff
			lastStdout = last.Result.Completion
			exitCode = last.Result.ExitCode
		}
		cands = append(cands, judge.Candidate{
			RunID:    c.ChainID,
			Agent:    "chain",
			ExitCode: exitCode,
			Status:   "success",
			Duration: c.Finished.Sub(c.Started),
			Diff:     lastDiff,
			Stdout:   tail(lastStdout, 1500),
		})
	}
	ranking, err := j.Rank(ctx, prompt, cands)
	if err != nil {
		return chains[0].ChainID, fmt.Sprintf("judge failed: %v; defaulted to first successful chain", err), 0
	}
	if ranking.Winner == "" {
		return chains[0].ChainID, "judge returned empty winner; defaulted to first successful chain", 0
	}
	return ranking.Winner, ranking.Rationale, ranking.Scores[ranking.Winner]
}

func findChain(chains []ChainResult, id string) ChainResult {
	for _, c := range chains {
		if c.ChainID == id {
			return c
		}
	}
	return ChainResult{}
}

func sumAttempts(chains []ChainResult) int {
	total := 0
	for _, c := range chains {
		for _, n := range c.Nodes {
			total += n.Attempts
		}
	}
	return total
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Sentinel: if errors.Is is needed to surface ErrEmptyDAGOpts in
// future code, this assertion keeps it exported.
var _ = errors.Is

// Compile-time assertion the interfaces still resolve.
var _ agent.Provider = (agent.Provider)(nil)
var _ auth.Provider = (auth.Provider)(nil)
var _ sandbox.Provider = (sandbox.Provider)(nil)

type dagStartedPayload struct {
	NodeCount  int `json:"node_count"`
	RootCount  int `json:"root_count"`
	ChainCount int `json:"chain_count"`
}

type dagCompletedPayload struct {
	TotalNodes       int    `json:"total_nodes"`
	TotalAttempts    int    `json:"total_attempts"`
	FailedChainCount int    `json:"failed_chain_count"`
	WinnerChainID    string `json:"winner_chain_id,omitempty"`
}
