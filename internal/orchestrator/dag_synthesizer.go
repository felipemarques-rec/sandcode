package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/agent"
	"github.com/felipemarques-rec/sandcode/internal/auth"
	"github.com/felipemarques-rec/sandcode/internal/event"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
)

// synthesizerArgs aggregates the per-synthesizer-execution context.
type synthesizerArgs struct {
	WinnerWorktree string        // host path; mounted at SandboxWorkDir
	Winner         ChainResult   // for prompt context
	AllChains      []ChainResult // includes Winner; used to expose alternative approaches
	JudgeRationale string
	OriginalPrompt string
	SandboxImage   string
	SandboxWorkDir string
	Limits         sandbox.Limits
	Network        string
	AgentOpts      agent.RunOptions
	Refine         RefineOptions
	Bus            event.Bus
	RunID          string
}

// runSynthesizer executes one consolidation pass. The agent runs in
// the winner's worktree and receives full visibility into all
// approaches via a structured prompt. Respects RefineOptions: when
// enabled, this is the "quality gate" pass for the entire DAG run.
//
// Returns AgentInvocationResult always; an error indicates an
// infrastructure failure (sandbox provision, auth) rather than an
// agent-level failure (which is captured in the result).
func runSynthesizer(
	ctx context.Context,
	sb sandbox.Provider,
	ag agent.Provider,
	au auth.Provider,
	args synthesizerArgs,
) (AgentInvocationResult, error) {
	emitDAG(args.Bus, event.DAGSynthesisStarted, args.RunID, synthesisStartedPayload{
		WinnerChainID: args.Winner.ChainID,
	})

	prompt := buildSynthesizerPrompt(args)

	spec := sandbox.SandboxSpec{
		Image:   args.SandboxImage,
		WorkDir: args.SandboxWorkDir,
		Mounts: []sandbox.Mount{
			{Source: args.WinnerWorktree, Target: args.SandboxWorkDir, ReadOnly: false},
		},
		Env:     map[string]string{},
		Network: args.Network,
		Limits:  args.Limits,
		Labels: map[string]string{
			"sandcode.run":   args.RunID,
			"sandcode.role":  "synthesizer",
			"sandcode.agent": ag.Name(),
		},
	}
	if au != nil {
		if err := au.Apply(&spec, ag.AuthHints()); err != nil {
			emitDAG(args.Bus, event.DAGSynthesisCompleted, args.RunID, synthesisCompletedPayload{Success: false})
			return AgentInvocationResult{ExitCode: -1, Err: fmt.Errorf("synthesizer auth: %w", err)}, nil
		}
	}
	box, err := sb.Create(ctx, spec)
	if err != nil {
		emitDAG(args.Bus, event.DAGSynthesisCompleted, args.RunID, synthesisCompletedPayload{Success: false})
		return AgentInvocationResult{ExitCode: -1, Err: fmt.Errorf("synthesizer sandbox: %w", err)}, nil
	}
	defer box.Close(context.Background())

	// Per-attempt loop with synthesizer-specific prompt.
	attempt := 0
	currentPrompt := prompt
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
				Err:      fmt.Errorf("synthesizer exec: %w", eErr),
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

		if !args.Refine.active() || ex.ExitCode != 0 || ex.Err != nil {
			break
		}

		vLines, vWait, vErr := box.Exec(ctx, args.Refine.VerifyCmd, nil, sandbox.ExecOptions{})
		if vErr != nil {
			last.Err = fmt.Errorf("synthesizer verify: %w", vErr)
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
			break
		}
		if attempt >= args.Refine.MaxAttempts {
			last.ExitCode = vRes.ExitCode
			last.Err = fmt.Errorf("synthesizer verify failed after %d attempts: exit %d", attempt, vRes.ExitCode)
			break
		}
		currentPrompt = buildRefinePrompt(prompt, tail(vOut.String(), 4096), attempt+1, args.Refine.MaxAttempts, args.Refine.VerifyCmd)
	}

	success := last.ExitCode == 0 && last.Err == nil
	emitDAG(args.Bus, event.DAGSynthesisCompleted, args.RunID, synthesisCompletedPayload{Success: success})
	return last, nil
}

// buildSynthesizerPrompt composes the consolidation prompt. Lists
// every chain (winner first by tag) with a summary derived from the
// last node's diff + completion tail.
func buildSynthesizerPrompt(args synthesizerArgs) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are consolidating work from %d parallel approaches to: %s\n",
		len(args.AllChains), args.OriginalPrompt)
	fmt.Fprintf(&b, "The judge selected approach %s as the strongest.\n\n", args.Winner.ChainID)
	fmt.Fprintf(&b, "Original goal: %s\n\n", args.OriginalPrompt)
	if args.JudgeRationale != "" {
		fmt.Fprintf(&b, "Judge rationale: %s\n\n", args.JudgeRationale)
	}
	b.WriteString("Approaches:\n")
	for _, c := range args.AllChains {
		tag := fmt.Sprintf("[Chain %s]", c.ChainID)
		if c.ChainID == args.Winner.ChainID {
			tag = fmt.Sprintf("[Winner: %s]", c.ChainID)
		}
		fmt.Fprintf(&b, "  %s %s\n", tag, summarizeChain(c))
	}
	b.WriteString("\nYour task: Improve the current worktree (which contains the winner's work) ")
	b.WriteString("by incorporating valuable insights from other approaches. You may modify files, ")
	b.WriteString("add tests, fix gaps. Justify changes briefly in your final response.\n")
	return b.String()
}

// summarizeChain produces a one-line snapshot of a chain's outcome
// suitable for embedding in the synthesizer prompt.
func summarizeChain(c ChainResult) string {
	if len(c.Nodes) == 0 {
		return "(no nodes)"
	}
	last := c.Nodes[len(c.Nodes)-1]
	files := extractChangedFiles(last.Diff)
	return fmt.Sprintf("Final note: %s | Files: %s",
		truncate(stripBackticks(last.Result.Completion), 400),
		strings.Join(files, ", "))
}

type synthesisStartedPayload struct {
	WinnerChainID string `json:"winner_chain_id"`
}

type synthesisCompletedPayload struct {
	Success bool `json:"success"`
}
