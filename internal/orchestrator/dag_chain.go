package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/felipemarques-rec/sandcode/internal/agent"
	"github.com/felipemarques-rec/sandcode/internal/auth"
	"github.com/felipemarques-rec/sandcode/internal/event"
	gitm "github.com/felipemarques-rec/sandcode/internal/git"
	"github.com/felipemarques-rec/sandcode/internal/planner"
	"github.com/felipemarques-rec/sandcode/internal/redact"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
)

// chainSpec is the per-chain payload computed at DAGRun fan-out time
// and passed into runChain. It captures the chain id (stable for events),
// the root node id, and the topo-ordered node sequence.
type chainSpec struct {
	ChainID    string
	RootNodeID string
	Nodes      []planner.Node

	// AssignedAgent overrides the `ag` parameter passed to runChain when
	// DAGOptions.Agents is multi-element. nil falls back to `ag`.
	// Set by decomposeChains in round-robin order.
	AssignedAgent agent.Provider
}

// runChain executes one root chain end-to-end. Chains are fail-isolated:
// a node failure breaks the chain (descendants skipped) and is reflected
// via ChainResult.Success=false + FailedAt=NodeID. The function returns
// a non-nil error ONLY when something prevents the chain from starting
// (worktree provision, sandbox provision, auth) — not for in-chain node
// failures, which are part of the result by design.
func runChain(
	ctx context.Context,
	sb sandbox.Provider,
	ag agent.Provider,
	au auth.Provider,
	spec chainSpec,
	opts DAGOptions,
) (ChainResult, error) {
	started := time.Now()

	// Per-chain span: defer is correct here because runChain is fully
	// synchronous (returns ChainResult, error — no channel/goroutine handoff).
	// The span lifetime is exactly the function lifetime.
	if opts.Langfuse != nil && opts.Langfuse.Enabled() {
		var chainSpan trace.Span
		ctx, chainSpan = opts.Langfuse.SpanBrain(ctx, "dag.chain",
			attribute.String("sandcode.run_id", opts.RunID),
			attribute.String("sandcode.dag.chain_id", spec.ChainID),
		)
		defer chainSpan.End()
	}

	// Resolve the effective agent for this chain. Multi-agent within DAG
	// is round-robin: decomposeChains set AssignedAgent. Single-agent or
	// the Slice 4 default leaves AssignedAgent nil → fall back to `ag`.
	chainAgent := ag
	if spec.AssignedAgent != nil {
		chainAgent = spec.AssignedAgent
	}

	res := ChainResult{
		ChainID:    spec.ChainID,
		RootNodeID: spec.RootNodeID,
		Started:    started,
	}

	// 1. Worktree per chain.
	wtMgr := gitm.NewManager()
	wtDir := filepath.Join(opts.CWD, ".sandcode", "work", opts.RunID, "dag", spec.ChainID)
	branch := fmt.Sprintf("sandcode/dag-%s-%s", opts.RunID, spec.ChainID)
	wt, err := wtMgr.Create(ctx, opts.CWD, wtDir, branch)
	if err != nil {
		emitDAG(opts.Bus, event.DAGChainStarted, opts.RunID, chainStartedPayload{
			ChainID: spec.ChainID, RootNodeID: spec.RootNodeID, NodeIDs: nodeIDs(spec.Nodes),
		})
		res.Success = false
		res.FailedAt = spec.RootNodeID
		res.Finished = time.Now()
		emitDAG(opts.Bus, event.DAGChainCompleted, opts.RunID, chainCompletedPayload{
			ChainID: spec.ChainID, Success: false, FailedAt: spec.RootNodeID,
		})
		return res, fmt.Errorf("chain %s worktree: %w", spec.ChainID, err)
	}
	res.Worktree = wt.Path
	res.Branch = wt.Branch
	defer func() {
		// Cleanup on success only; failed worktrees stay for inspection
		// (matches Run's KeepWorktree semantics).
		if !opts.KeepWorktree && res.Success {
			_ = wtMgr.Remove(context.Background(), wt, false)
		}
	}()

	emitDAG(opts.Bus, event.DAGChainStarted, opts.RunID, chainStartedPayload{
		ChainID: spec.ChainID, RootNodeID: spec.RootNodeID, NodeIDs: nodeIDs(spec.Nodes),
	})

	// 2. One sandbox per chain — reused across nodes. Cheaper than
	// per-node sandbox; matches the chain's shared-worktree topology.
	sandboxSpec := sandbox.SandboxSpec{
		Image:   opts.SandboxImage,
		WorkDir: opts.SandboxWorkDir,
		Mounts: []sandbox.Mount{
			{Source: wt.Path, Target: opts.SandboxWorkDir, ReadOnly: false},
		},
		Env:     map[string]string{},
		Network: opts.Network,
		Limits:  opts.Limits,
		Labels: map[string]string{
			"sandcode.run":   opts.RunID,
			"sandcode.chain": spec.ChainID,
			"sandcode.agent": ag.Name(),
		},
	}
	if au != nil {
		if err := au.Apply(&sandboxSpec, ag.AuthHints()); err != nil {
			res.Success = false
			res.FailedAt = spec.RootNodeID
			res.Finished = time.Now()
			emitDAG(opts.Bus, event.DAGChainCompleted, opts.RunID, chainCompletedPayload{
				ChainID: spec.ChainID, Success: false, FailedAt: spec.RootNodeID,
			})
			return res, fmt.Errorf("chain %s auth: %w", spec.ChainID, err)
		}
	}
	box, err := sb.Create(ctx, sandboxSpec)
	if err != nil {
		res.Success = false
		res.FailedAt = spec.RootNodeID
		res.Finished = time.Now()
		emitDAG(opts.Bus, event.DAGChainCompleted, opts.RunID, chainCompletedPayload{
			ChainID: spec.ChainID, Success: false, FailedAt: spec.RootNodeID,
		})
		return res, fmt.Errorf("chain %s sandbox: %w", spec.ChainID, err)
	}
	defer box.Close(context.Background())

	// 3. Iterate nodes sequentially. Each node sees the cumulative
	// worktree state + a structured handoff from the previous node.
	for i := range spec.Nodes {
		node := spec.Nodes[i]
		prompt := buildHandoffPrompt(node, res.Nodes)

		nr := runNode(ctx, box, chainAgent, runNodeArgs{
			ChainID:        spec.ChainID,
			Node:           node,
			Prompt:         prompt,
			SandboxWorkDir: opts.SandboxWorkDir,
			AgentOpts:      opts.AgentOpts,
			Refine:         opts.Refine,
			Bus:            opts.Bus,
			RunID:          opts.RunID,
		})
		res.Nodes = append(res.Nodes, nr)
		res.Cost.Tokens += nr.Cost.Tokens
		res.Cost.USD += nr.Cost.USD

		if !nodeSucceeded(nr) {
			res.Success = false
			res.FailedAt = node.ID
			res.Finished = time.Now()
			emitDAG(opts.Bus, event.DAGChainCompleted, opts.RunID, chainCompletedPayload{
				ChainID: spec.ChainID, Success: false, FailedAt: node.ID,
			})
			return res, nil // fail-isolated, no error
		}

		// Capture cumulative diff for the next node's handoff (redacted: it
		// feeds the handoff prompt, the store, and any LLM judge/reviewer).
		if diff, derr := wtMgr.Diff(ctx, wt); derr == nil {
			res.Nodes[len(res.Nodes)-1].Diff = redact.Redact(diff)
		}
	}

	res.Success = true
	res.Finished = time.Now()
	emitDAG(opts.Bus, event.DAGChainCompleted, opts.RunID, chainCompletedPayload{
		ChainID: spec.ChainID, Success: true,
	})
	return res, nil
}

// runNodeArgs aggregates the per-node-execution context to keep the
// runNode signature manageable.
type runNodeArgs struct {
	ChainID        string
	Node           planner.Node
	Prompt         string
	SandboxWorkDir string
	AgentOpts      agent.RunOptions
	Refine         RefineOptions
	Bus            event.Bus
	RunID          string
}

// runNode executes one node — one agent invocation, optionally with a
// per-node refine cascade. Returns a populated NodeResult (always);
// callers branch on nodeSucceeded(nr) to decide chain continuation.
//
// The attempt loop mirrors the one in run.go but is scoped to a single
// node: chain-level finalization (commit, full diff, cleanup) happens
// in runChain, not here.
func runNode(ctx context.Context, box sandbox.Sandbox, ag agent.Provider, args runNodeArgs) NodeResult {
	emitDAG(args.Bus, event.DAGNodeStarted, args.RunID, nodeStartedPayload{
		ChainID: args.ChainID, NodeID: args.Node.ID, Attempt: 0,
	})

	attempt := 0
	currentPrompt := args.Prompt
	var last AgentInvocationResult

	for {
		attempt++

		agentOpts := args.AgentOpts
		agentOpts.Prompt = currentPrompt
		agentOpts.WorkDir = args.SandboxWorkDir
		cmd := ag.BuildCommand(agentOpts)

		execStart := time.Now()
		lines, wait, eErr := box.Exec(ctx, cmd.Argv, cmd.Stdin, sandbox.ExecOptions{Env: cmd.Env})
		if eErr != nil {
			last = AgentInvocationResult{
				ExitCode: -1,
				Err:      fmt.Errorf("agent exec: %w", eErr),
				Duration: time.Since(execStart),
			}
			break
		}

		var compBuilder strings.Builder
		for ln := range lines {
			compBuilder.WriteString(ln.Text)
			compBuilder.WriteByte('\n')
		}
		ex := wait()
		last = AgentInvocationResult{
			ExitCode:   ex.ExitCode,
			Completion: compBuilder.String(),
			Err:        ex.Err,
			Duration:   time.Since(execStart),
		}

		// Refine? Only when configured AND the agent itself succeeded.
		// A crashed agent (non-zero exit) doesn't get retried — refining
		// a broken agent won't help.
		if !args.Refine.active() || ex.ExitCode != 0 || ex.Err != nil {
			break
		}

		// Verify in same sandbox.
		vLines, vWait, vErr := box.Exec(ctx, args.Refine.VerifyCmd, nil, sandbox.ExecOptions{})
		if vErr != nil {
			last.Err = fmt.Errorf("verify exec: %w", vErr)
			last.ExitCode = -1
			break
		}
		var vOut strings.Builder
		for ln := range vLines {
			vOut.WriteString(ln.Text)
			vOut.WriteByte('\n')
		}
		vRes := vWait()
		if vRes.ExitCode == 0 {
			break // verify passed → success
		}

		// Verify failed; refine if cap allows.
		if attempt >= args.Refine.MaxAttempts {
			last.ExitCode = vRes.ExitCode
			last.Err = fmt.Errorf("verify failed after %d attempt(s): exit %d", attempt, vRes.ExitCode)
			break
		}

		// Build refined prompt and emit a node_started for the next attempt.
		currentPrompt = buildRefinePrompt(args.Prompt, tail(vOut.String(), 4096), attempt+1, args.Refine.MaxAttempts, args.Refine.VerifyCmd)
		emitDAG(args.Bus, event.DAGNodeStarted, args.RunID, nodeStartedPayload{
			ChainID: args.ChainID, NodeID: args.Node.ID, Attempt: attempt,
		})
	}

	success := last.ExitCode == 0 && last.Err == nil
	emitDAG(args.Bus, event.DAGNodeCompleted, args.RunID, nodeCompletedPayload{
		ChainID: args.ChainID, NodeID: args.Node.ID, Attempt: attempt - 1, Success: success,
	})

	return NodeResult{
		NodeID:   args.Node.ID,
		Prompt:   args.Prompt,
		Attempts: attempt,
		Result:   last,
	}
}

func nodeSucceeded(n NodeResult) bool {
	return n.Result.ExitCode == 0 && n.Result.Err == nil
}

func nodeIDs(ns []planner.Node) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = n.ID
	}
	return out
}

// emitDAG publishes a dag.* event payload on the bus. No-op when bus
// is nil. Marshal failures are logged-and-swallowed (events are
// observability, not correctness).
func emitDAG(bus event.Bus, typ event.Type, runID string, payload any) {
	if bus == nil {
		return
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		raw = []byte("{}")
	}
	ev := event.New(typ, runID, raw).WithCorrelation(runID)
	_ = bus.Publish(context.Background(), ev)
}

// Payload structs — JSON-tagged for stable wire shape on the bus.

type chainStartedPayload struct {
	ChainID    string   `json:"chain_id"`
	RootNodeID string   `json:"root_node_id"`
	NodeIDs    []string `json:"node_ids"`
}

type chainCompletedPayload struct {
	ChainID  string `json:"chain_id"`
	Success  bool   `json:"success"`
	FailedAt string `json:"failed_at,omitempty"`
}

type nodeStartedPayload struct {
	ChainID string `json:"chain_id"`
	NodeID  string `json:"node_id"`
	Attempt int    `json:"attempt"`
}

type nodeCompletedPayload struct {
	ChainID string `json:"chain_id"`
	NodeID  string `json:"node_id"`
	Attempt int    `json:"attempt"`
	Success bool   `json:"success"`
}
