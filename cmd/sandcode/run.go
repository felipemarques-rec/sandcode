package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/agent"
	"github.com/felipemarques-rec/sandcode/internal/approval"
	"github.com/felipemarques-rec/sandcode/internal/architect"
	"github.com/felipemarques-rec/sandcode/internal/auth"
	"github.com/felipemarques-rec/sandcode/internal/brain"
	"github.com/felipemarques-rec/sandcode/internal/budget"
	"github.com/felipemarques-rec/sandcode/internal/event"
	gitm "github.com/felipemarques-rec/sandcode/internal/git"
	"github.com/felipemarques-rec/sandcode/internal/governance"
	"github.com/felipemarques-rec/sandcode/internal/judge"
	"github.com/felipemarques-rec/sandcode/internal/kernel"
	"github.com/felipemarques-rec/sandcode/internal/langfuse"
	"github.com/felipemarques-rec/sandcode/internal/mcp"
	"github.com/felipemarques-rec/sandcode/internal/orchestrator"
	"github.com/felipemarques-rec/sandcode/internal/planner"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
	"github.com/felipemarques-rec/sandcode/internal/secreview"
	"github.com/felipemarques-rec/sandcode/internal/stepback"
	"github.com/felipemarques-rec/sandcode/internal/store"
	strat "github.com/felipemarques-rec/sandcode/internal/strategy"
	"github.com/spf13/cobra"
)

type runFlags struct {
	cwd          string
	sandboxKind  string
	image        string
	workdir      string
	agentName    string
	model        string
	authMode     string
	strategy     string
	keepWorktree bool
	timeout      time.Duration
	cpus         string
	memory       string
	pids         int
	network      string
	noStore      bool
	parallel     int
	maxConc      int
	judgeKind    string
	judgeModel   string
	llmAuth      string
	learn        bool
	architect    bool
	stepBack     bool
	planStage    bool // --plan: wire kernel planner (decompose high-complexity prompts)
	strategySel  bool // --strategy-select: wire kernel selector (pick single/refine/parallel)
	reactive     bool // --reactive: route classify through the deterministic reactor (SP3.0)
	roles        []string
	mcp          []string // --mcp: MCP servers to enable (e.g. context7)

	// DAG mode (W12 Slice 4).
	dag         bool
	dagFromFile string

	// SP2a coordination spine.
	report bool
	review bool

	// SP2-SecReviewer.
	securityReview    bool
	securityReviewLLM bool

	perfReview     bool
	refactorReview bool

	approvalTimeout time.Duration

	gateFlags
}

// validateDAGFlags enforces the constraints from the W12 Slice 4 spec
// before any worktree/sandbox spin-up. Caller passes the parsed flags;
// returns nil when the combination is valid.
func validateDAGFlags(f runFlags) error {
	if f.dagFromFile != "" && !f.dag {
		return fmt.Errorf("--dag-from-file requires --dag")
	}
	// γ composition: forbid the ambiguous combination of --parallel >1
	// AND multi --agent (comma-separated). Either alone is fine.
	if f.dag && f.parallel > 1 && strings.Contains(f.agentName, ",") {
		return fmt.Errorf("--dag with both --parallel >1 and multi --agent: use either --parallel or multi --agent, not both")
	}
	return nil
}

// resolveAgent maps a CLI name to an agent provider.
func resolveAgent(name string) (agent.Provider, error) {
	switch strings.TrimSpace(name) {
	case "claude-code", "claude":
		return agent.NewClaudeCode(), nil
	case "codex":
		return agent.NewCodex(), nil
	case "cursor", "cursor-agent":
		return agent.NewCursor(), nil
	default:
		return nil, fmt.Errorf("unknown agent %q", name)
	}
}

// validRoles is the known-role set for --role flag validation.
var validRoles = map[string]struct{}{
	string(agent.RolePlanner):               {},
	string(agent.RoleArchitect):             {},
	string(agent.RoleImplementer):           {},
	string(agent.RoleVerifier):              {},
	string(agent.RoleReviewer):              {},
	string(agent.RoleSecurityReviewer):      {},
	string(agent.RolePerformanceReviewer):   {},
	string(agent.RoleRefactoringSpecialist): {},
	string(agent.RoleReporter):              {},
}

// buildRoleRegistry parses a slice of "role=agent" spec strings (from
// a cobra repeatable --role flag) and returns a populated agent.Registry.
//
// Empty slice → (nil, nil): nil registry preserves the legacy path in the
// orchestrator without allocating an empty registry.
//
// Validation: each spec must split on the first '=' into a non-empty role
// token (must be a known role) and a non-empty agent token (resolved via
// resolveAgent). Any violation returns a descriptive error immediately.
func buildRoleRegistry(specs []string) (agent.Registry, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	var reg agent.Registry
	for _, spec := range specs {
		idx := strings.IndexByte(spec, '=')
		if idx < 0 {
			return nil, fmt.Errorf("invalid --role %q (want role=agent)", spec)
		}
		roleTok := strings.TrimSpace(spec[:idx])
		agentTok := strings.TrimSpace(spec[idx+1:])
		if roleTok == "" || agentTok == "" {
			return nil, fmt.Errorf("invalid --role %q (want role=agent)", spec)
		}
		if _, ok := validRoles[roleTok]; !ok {
			return nil, fmt.Errorf("unknown role %q (valid: planner, architect, implementer, verifier, reviewer, security_reviewer, performance_reviewer, refactoring_specialist, reporter)", roleTok)
		}
		p, err := resolveAgent(agentTok)
		if err != nil {
			return nil, err
		}
		if reg == nil {
			reg = agent.NewRegistry()
		}
		if err := reg.Register(agent.Role(roleTok), p); err != nil {
			return nil, err
		}
	}
	return reg, nil
}

// buildMCPManager builds an MCP manager from the --mcp server names, enabling
// each requested server out of the built-in DefaultConfigs. An empty slice
// returns (nil, nil) so the orchestrator keeps the byte-identical legacy path.
// An unknown server name is a hard error before any worktree/sandbox spin-up.
func buildMCPManager(names []string) (*mcp.Manager, error) {
	if len(names) == 0 {
		return nil, nil
	}
	m := mcp.NewManager(mcp.DefaultConfigs())
	for _, n := range names {
		if !m.Enable(strings.TrimSpace(n)) {
			return nil, fmt.Errorf("unknown --mcp server %q (valid: context7, claude-mem)", n)
		}
	}
	return m, nil
}

// mcpExtraArgs returns the claude-specific flags that make the injected
// .mcp.json usable headlessly: it scopes config to the injected file
// (--strict-mcp-config --mcp-config) and auto-allows each enabled server's
// tools (--allowedTools "mcp__<server> …"), least-privilege. Returns nil for a
// non-Claude agent or an empty manager — those get the file only (or nothing).
//
// permitted, when non-nil, gates each enabled server by name (RBAC); a server
// is allow-listed only if permitted(server.Name) is true. A nil filter permits
// every enabled server (byte-identical to the unfiltered behavior). When the
// filter rejects every server the result is nil, just like an empty manager.
func mcpExtraArgs(m *mcp.Manager, ag agent.Provider, permitted func(tool string) bool) []string {
	if m == nil || ag == nil || ag.Name() != "claude-code" {
		return nil
	}
	enabled := m.ListEnabled(context.Background())
	if len(enabled) == 0 {
		return nil
	}
	allow := make([]string, 0, len(enabled))
	for _, c := range enabled {
		if permitted == nil || permitted(c.Name) {
			allow = append(allow, "mcp__"+c.Name)
		}
	}
	if len(allow) == 0 {
		return nil
	}
	return []string{
		"--strict-mcp-config",
		"--mcp-config", ".mcp.json",
		"--allowedTools", strings.Join(allow, " "),
	}
}

// LLM auth transport modes for the Go-side LLM features.
const (
	llmAuthAuto         = "auto"
	llmAuthSubscription = "subscription"
	llmAuthAPIKey       = "api-key"
)

// resolveLLMAuth picks the transport for the Go-side LLM features (judge,
// reviewer, architect, planner, security reviewer). "auto" prefers
// ANTHROPIC_API_KEY when set, else the `claude` subscription CLI. It is only
// called when an LLM feature is actually requested, so plain runs never error.
func resolveLLMAuth(mode string) (string, error) {
	switch strings.TrimSpace(mode) {
	case "", llmAuthAuto:
		if os.Getenv("ANTHROPIC_API_KEY") != "" {
			return llmAuthAPIKey, nil
		}
		if _, err := exec.LookPath("claude"); err == nil {
			return llmAuthSubscription, nil
		}
		return "", errors.New("no LLM auth available: set ANTHROPIC_API_KEY or install the `claude` CLI (subscription), or pass --llm-auth")
	case llmAuthSubscription:
		if _, err := exec.LookPath("claude"); err != nil {
			return "", errors.New("--llm-auth subscription requires the `claude` CLI on PATH")
		}
		return llmAuthSubscription, nil
	case llmAuthAPIKey, "apikey":
		if os.Getenv("ANTHROPIC_API_KEY") == "" {
			return "", errors.New("--llm-auth api-key requires ANTHROPIC_API_KEY")
		}
		return llmAuthAPIKey, nil
	default:
		return "", fmt.Errorf("unknown --llm-auth %q (want auto|subscription|api-key)", mode)
	}
}

// The newLLM* builders return the right transport variant for f.llmAuth. Each
// resolves the mode (idempotent, cheap) so callers stay one-liners regardless of
// auth source.

func newJudge(f runFlags) (judge.Judge, error) {
	mode, err := resolveLLMAuth(f.llmAuth)
	if err != nil {
		return nil, err
	}
	if mode == llmAuthSubscription {
		return judge.NewLLMJudgeFromSubscription(f.judgeModel), nil
	}
	return judge.NewLLMJudgeFromEnv(f.judgeModel)
}

func newReviewer(f runFlags) (judge.Reviewer, error) {
	mode, err := resolveLLMAuth(f.llmAuth)
	if err != nil {
		return nil, err
	}
	if mode == llmAuthSubscription {
		return judge.NewLLMReviewerFromSubscription(f.judgeModel), nil
	}
	return judge.NewLLMReviewerFromEnv(f.judgeModel)
}

func newPerfReviewer(f runFlags) (judge.Reviewer, error) {
	mode, err := resolveLLMAuth(f.llmAuth)
	if err != nil {
		return nil, err
	}
	if mode == llmAuthSubscription {
		return judge.NewPerformanceReviewerFromSubscription(f.judgeModel), nil
	}
	return judge.NewPerformanceReviewerFromEnv(f.judgeModel)
}

func newRefactorReviewer(f runFlags) (judge.Reviewer, error) {
	mode, err := resolveLLMAuth(f.llmAuth)
	if err != nil {
		return nil, err
	}
	if mode == llmAuthSubscription {
		return judge.NewRefactoringReviewerFromSubscription(f.judgeModel), nil
	}
	return judge.NewRefactoringReviewerFromEnv(f.judgeModel)
}

func newSecurityReviewerLLM(f runFlags) (secreview.SecurityReviewer, error) {
	mode, err := resolveLLMAuth(f.llmAuth)
	if err != nil {
		return nil, err
	}
	if mode == llmAuthSubscription {
		return secreview.NewLLMSecurityReviewerFromSubscription(f.judgeModel), nil
	}
	return secreview.NewLLMSecurityReviewerFromEnv(f.judgeModel)
}

func newArchitect(f runFlags) (architect.Architect, error) {
	mode, err := resolveLLMAuth(f.llmAuth)
	if err != nil {
		return nil, err
	}
	if mode == llmAuthSubscription {
		return architect.NewLLMArchitectFromSubscription(f.judgeModel), nil
	}
	return architect.NewLLMArchitectFromEnv(f.judgeModel)
}

func newStepBack(f runFlags) (stepback.StepBack, error) {
	mode, err := resolveLLMAuth(f.llmAuth)
	if err != nil {
		return nil, err
	}
	if mode == llmAuthSubscription {
		return stepback.NewLLMStepBackFromSubscription(f.judgeModel), nil
	}
	return stepback.NewLLMStepBackFromEnv(f.judgeModel)
}

func newPlanner(f runFlags) (planner.Planner, error) {
	mode, err := resolveLLMAuth(f.llmAuth)
	if err != nil {
		return nil, err
	}
	if mode == llmAuthSubscription {
		return planner.NewLLMPlannerFromSubscription(f.judgeModel), nil
	}
	return planner.NewLLMPlannerFromEnv(f.judgeModel)
}

func newRunCmd() *cobra.Command {
	var f runFlags
	cmd := &cobra.Command{
		Use:   "run <prompt>",
		Short: "Run a coding agent on a prompt inside a sandbox",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			prompt := strings.Join(args, " ")
			ctx := cmd.Context()

			if f.architect && !f.learn {
				return errors.New("--architect requires --learn (kernel path)")
			}
			if f.stepBack && !f.learn {
				return errors.New("--step-back requires --learn (kernel path)")
			}
			if f.planStage && !f.learn {
				return errors.New("--plan requires --learn (kernel path)")
			}
			if f.strategySel && !f.learn {
				return errors.New("--strategy-select requires --learn (kernel path)")
			}
			if f.reactive && !f.learn {
				return errors.New("--reactive requires --learn (kernel path)")
			}

			cwd := f.cwd
			if cwd == "" {
				var err error
				cwd, err = os.Getwd()
				if err != nil {
					return err
				}
			}

			// Resolve sandbox provider
			var sb sandbox.Provider
			switch f.sandboxKind {
			case "docker":
				sb = sandbox.NewDockerProvider()
			case "podman":
				sb = sandbox.NewPodmanProvider()
			case "nosandbox":
				sb = sandbox.NewNoSandboxProvider()
			default:
				return fmt.Errorf("unknown --sandbox %q (want docker|podman|nosandbox)", f.sandboxKind)
			}

			// Resolve agent(s) — comma-separated allows multi-agent fan-out.
			var agents []agent.Provider
			for _, name := range strings.Split(f.agentName, ",") {
				name = strings.TrimSpace(name)
				if name == "" {
					continue
				}
				ag, err := resolveAgent(name)
				if err != nil {
					return err
				}
				agents = append(agents, ag)
			}
			if len(agents) == 0 {
				return errors.New("at least one --agent required")
			}
			// If --parallel N > 1 with a single agent name, replicate that agent N times.
			if f.parallel > 1 && len(agents) == 1 {
				ag := agents[0]
				agents = nil
				for i := 0; i < f.parallel; i++ {
					agents = append(agents, ag)
				}
			}

			// Resolve auth
			var au auth.Provider
			switch f.authMode {
			case "bindmount":
				au = auth.NewBindMount()
			case "api-key", "apikey":
				au = auth.NewAPIKey()
			default:
				return fmt.Errorf("unknown --auth-mode %q (want bindmount|api-key)", f.authMode)
			}

			strategy := gitm.Strategy(f.strategy)
			if strategy != gitm.StrategyMergeToHead && strategy != gitm.StrategyBranch {
				return fmt.Errorf("unknown --strategy %q", f.strategy)
			}

			var st store.Store
			if !f.noStore {
				db, err := store.Open(resolveStorePath(cwd))
				if err != nil {
					return fmt.Errorf("open store: %w", err)
				}
				defer db.Close()
				st = db
			}

			limits := sandbox.Limits{CPUs: f.cpus, Memory: f.memory, PidsLimit: f.pids}

			// Langfuse LLM observability (auto-detected from env)
			var lf *langfuse.Provider
			lfCfg := langfuse.ConfigFromEnv()
			if lfCfg.Enabled {
				prov, err := langfuse.Init(ctx, lfCfg)
				if err != nil {
					fmt.Fprintf(os.Stderr, "warning: langfuse init failed: %v\n", err)
				} else {
					lf = prov
					defer func() {
						shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						defer cancel()
						_ = lf.Shutdown(shutdownCtx)
					}()
				}
			}

			// Brain (opt-in via --learn) + Cognitive Kernel + Event Bus
			var br brain.Brain
			var kn *kernel.Kernel
			var bus event.Bus
			if f.learn {
				db, err := brain.OpenBrain(resolveBrainPath(cwd))
				if err != nil {
					return fmt.Errorf("open brain: %w", err)
				}
				defer db.Close()
				// When a run store is also live, wire it as the
				// episodic-memory tier so Enrich blends semantic
				// (lessons) with episodic (past runs).
				if sqliteStore, ok := st.(*store.SQLite); ok && sqliteStore != nil {
					db.WithEpisodic(sqliteStore.AsTier())
				}
				br = db

				// LocalBus drives event emission. Closed at the end so any
				// pending subscriber goroutines can finish.
				lb := event.NewLocalBus()
				defer lb.Close()
				bus = lb
				kopts := []kernel.Option{
					kernel.WithBus(bus),
					kernel.WithTracer(orchestrator.NewKernelTracer(lf)),
				}
				if f.architect {
					arch, aerr := newArchitect(f)
					if aerr != nil {
						return aerr
					}
					kopts = append(kopts, kernel.WithArchitect(arch))
				}
				if f.stepBack {
					sbr, serr := newStepBack(f)
					if serr != nil {
						return serr
					}
					kopts = append(kopts, kernel.WithStepBack(sbr))
				}
				// Wire the kernel planner when the user opts into the Plan
				// stage (--plan) or the DAG path (--dag relies on
				// kn.ForcePlan). Without either, the planner stays nil and
				// the Plan stage is skipped (byte-identical legacy).
				if f.planStage || f.dag {
					pl, perr := newPlanner(f)
					if perr != nil {
						return perr
					}
					kopts = append(kopts, kernel.WithPlanner(pl))
				}
				// Wire the deterministic strategy selector when the user
				// opts into kernel-decided dispatch (--strategy-select).
				// nil selector ⇒ empty Strategy ⇒ Execute defaults to single.
				if f.strategySel {
					kopts = append(kopts, kernel.WithSelector(strat.New()))
				}
				// Wire the deterministic reactor for the classify stage (SP3.0
				// PoC). Needs the bus (already wired above). Off ⇒ direct path.
				if f.reactive {
					kopts = append(kopts, kernel.WithReactive())
				}
				kn = kernel.New(br, kopts...)
			}

			engine, guard, refineOpts, err := buildGates(f.gateFlags)
			if err != nil {
				return err
			}

			// When governance is configured, a Review verdict may pause the
			// run pending human approval; the terminal approver prompts on
			// stdin/stderr. nil approver ⇒ byte-identical legacy path.
			var approver approval.Approver
			if engine != nil {
				approver = &approval.TerminalApprover{In: os.Stdin, Out: os.Stderr}
			}

			// Build role registry from --role flags (single-agent paths only).
			// Returns nil when no --role flags are provided (legacy path).
			reg, err := buildRoleRegistry(f.roles)
			if err != nil {
				return err
			}

			// W12 Slice 4 — DAG mode dispatch. Validates flags first
			// (cheap), then resolves the plan (file or planner LLM),
			// builds DAGOptions, and dispatches single or outer-fanned-out.
			if f.dag {
				if err := validateDAGFlags(f); err != nil {
					return err
				}
				return runDAGCommand(ctx, sb, agents, au, st, kn, bus, engine, guard, refineOpts, prompt, cwd, strategy, limits, f)
			}

			// Multi-agent fan-out
			if len(agents) > 1 {
				return runParallel(ctx, sb, au, st, br, kn, bus, engine, guard, refineOpts, agents, prompt, cwd, strategy, limits, f)
			}

			ag := agents[0]

			// MCP wiring: build the manager from --mcp (validates names) and
			// the claude-specific ExtraArgs that make the injected .mcp.json
			// usable headlessly. nil manager ⇒ byte-identical legacy.
			mcpMgr, err := buildMCPManager(f.mcp)
			if err != nil {
				return err
			}
			agentOpts := agent.RunOptions{Model: f.model}
			agentOpts.ExtraArgs = mcpExtraArgs(mcpMgr, ag, nil)

			// When the kernel is configured (--learn), route through
			// orchestrator.Execute so kernel-decided dispatch happens.
			// kn == nil → fall through to direct orchestrator.Run, no
			// behavioral change.
			if kn != nil {
				eopts := orchestrator.ExecuteOptions{
					Prompt:          prompt,
					CWD:             cwd,
					SandboxImage:    f.image,
					SandboxWorkDir:  f.workdir,
					Strategy:        strategy,
					KeepWorktree:    f.keepWorktree,
					Timeout:         f.timeout,
					Network:         f.network,
					Limits:          limits,
					AgentOpts:       agentOpts,
					Store:           st,
					Kernel:          kn,
					Bus:             bus,
					Langfuse:        lf,
					Governance:      engine,
					Approver:        approver,
					ApprovalTimeout: f.approvalTimeout,
					Budget:          guard,
					Refine:          refineOpts,
					LintCmd:         f.lintCmd,
					Reactive:        f.reactive,
					Agent:           ag,
					Registry:        reg,
					MCP:             mcpMgr,
				}
				fmt.Printf("\033[1;36m[sandcode]\033[0m running %s on %s (auto-dispatch) — prompt: %q\n", ag.Name(), sb.Name(), truncate(prompt, 80))
				events, await, err := orchestrator.Execute(ctx, sb, au, eopts)
				if err != nil {
					return err
				}
				for ev := range events {
					renderEvent(ev)
				}
				res := await()
				fmt.Println()
				fmt.Printf("\033[1;36m[sandcode]\033[0m dispatch kind=%s reason=%q\n", res.Kind, res.DispatchReason)
				if res.Run != nil {
					fmt.Printf("\033[1;36m[sandcode]\033[0m run %s — status=%s exit=%d duration=%s\n",
						res.Run.RunID, res.Run.Status, res.Run.ExitCode, res.Run.Finished.Sub(res.Run.Started).Round(time.Millisecond))
					if res.Run.Diff != "" {
						fmt.Println("\033[1;36m[sandcode]\033[0m diff applied:")
						fmt.Println(res.Run.Diff)
					}
					if res.Run.Err != nil {
						return res.Run.Err
					}
					if res.Run.Status != "success" {
						return errors.New(res.Run.Status)
					}
				}
				if res.Parallel != nil {
					fmt.Printf("\033[1;36m[sandcode]\033[0m parallel run completed with %d sub-runs\n", len(res.Parallel.Sub))
				}
				if res.DAG != nil {
					fmt.Printf("\033[1;36m[sandcode]\033[0m dag run completed: winner=%s, %d chains\n", res.DAG.Winner, len(res.DAG.Chains))
					if res.DAG.Error != nil {
						return res.DAG.Error
					}
				}
				return nil
			}

			runOpts := orchestrator.RunOptions{
				Prompt:          prompt,
				CWD:             cwd,
				SandboxImage:    f.image,
				SandboxWorkDir:  f.workdir,
				Strategy:        strategy,
				KeepWorktree:    f.keepWorktree,
				Timeout:         f.timeout,
				Network:         f.network,
				Limits:          limits,
				AgentOpts:       agentOpts,
				Store:           st,
				Brain:           br,
				Kernel:          kn,
				Bus:             bus,
				Langfuse:        lf,
				Governance:      engine,
				Approver:        approver,
				ApprovalTimeout: f.approvalTimeout,
				Budget:          guard,
				Refine:          refineOpts,
				LintCmd:         f.lintCmd,
				Reactive:        f.reactive,
				Registry:        reg,
				MCP:             mcpMgr,
			}

			fmt.Printf("\033[1;36m[sandcode]\033[0m running %s on %s — prompt: %q\n", ag.Name(), sb.Name(), truncate(prompt, 80))

			if f.report || f.review || f.securityReview || f.securityReviewLLM || f.perfReview || f.refactorReview {
				// Route through Coordinate: --report writes a deterministic
				// REPORT.md (SP2a); --review runs an opt-in LLM reviewer (SP2b).
				var reviewer judge.Reviewer
				if f.review {
					lr, rerr := newReviewer(f)
					if rerr != nil {
						return rerr
					}
					reviewer = lr
				}
				var reporter orchestrator.Reporter
				if f.report {
					reporter = orchestrator.DefaultReporter()
				}
				var secReviewer secreview.SecurityReviewer
				if f.securityReviewLLM {
					lsr, serr := newSecurityReviewerLLM(f)
					if serr != nil {
						return serr
					}
					secReviewer = lsr
				} else if f.securityReview {
					secReviewer = secreview.NewScanner()
				}
				var perfReviewer judge.Reviewer
				if f.perfReview {
					pr, perr := newPerfReviewer(f)
					if perr != nil {
						return perr
					}
					perfReviewer = pr
				}
				var refactorReviewer judge.Reviewer
				if f.refactorReview {
					rfr, rferr := newRefactorReviewer(f)
					if rferr != nil {
						return rferr
					}
					refactorReviewer = rfr
				}
				copts := orchestrator.CoordinateOptions{
					RunOptions:            runOpts,
					Reporter:              reporter,
					Reviewer:              reviewer,
					SecurityReviewer:      secReviewer,
					PerformanceReviewer:   perfReviewer,
					RefactoringSpecialist: refactorReviewer,
				}
				events, awaitC, err := orchestrator.Coordinate(ctx, sb, ag, au, copts)
				if err != nil {
					return err
				}
				for ev := range events {
					renderEvent(ev)
				}
				cres := awaitC()
				res := cres.Run
				fmt.Println()
				fmt.Printf("\033[1;36m[sandcode]\033[0m run %s — status=%s exit=%d duration=%s\n",
					res.RunID, res.Status, res.ExitCode, res.Finished.Sub(res.Started).Round(time.Millisecond))
				if res.Diff != "" {
					fmt.Println("\033[1;36m[sandcode]\033[0m diff applied:")
					fmt.Println(res.Diff)
				} else {
					fmt.Println("\033[1;36m[sandcode]\033[0m no changes produced.")
				}
				if cres.Report != nil {
					fmt.Printf("\033[1;36m[sandcode]\033[0m report written to %s\n", cres.Report.Path)
				}
				if res.Err != nil {
					return res.Err
				}
				if res.Status != "success" {
					return errors.New(res.Status)
				}
				return nil
			}

			events, await, err := orchestrator.Run(ctx, sb, ag, au, runOpts)
			if err != nil {
				return err
			}

			for ev := range events {
				renderEvent(ev)
			}

			res := await()
			fmt.Println()
			fmt.Printf("\033[1;36m[sandcode]\033[0m run %s — status=%s exit=%d duration=%s\n",
				res.RunID, res.Status, res.ExitCode, res.Finished.Sub(res.Started).Round(time.Millisecond))
			if res.Diff != "" {
				fmt.Println("\033[1;36m[sandcode]\033[0m diff applied:")
				fmt.Println(res.Diff)
			} else {
				fmt.Println("\033[1;36m[sandcode]\033[0m no changes produced.")
			}
			if res.Err != nil {
				return res.Err
			}
			if res.Status != "success" {
				return errors.New(res.Status)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&f.cwd, "cwd", "", "host repository to run against (default: current dir)")
	cmd.Flags().StringVar(&f.sandboxKind, "sandbox", "docker", "sandbox provider: docker|podman|nosandbox")
	cmd.Flags().StringVar(&f.image, "image", "sandcode-default:latest", "container image")
	cmd.Flags().StringVar(&f.workdir, "workdir", "/workspace", "in-container working directory")
	cmd.Flags().StringVar(&f.agentName, "agent", "claude-code", "agent to invoke")
	cmd.Flags().StringVar(&f.model, "model", "", "override model (e.g. claude-haiku-4-5)")
	cmd.Flags().StringVar(&f.authMode, "auth-mode", "bindmount", "auth strategy: bindmount|api-key")
	cmd.Flags().StringVar(&f.strategy, "strategy", "merge-to-head", "git worktree strategy: merge-to-head|branch")
	cmd.Flags().BoolVar(&f.keepWorktree, "keep-worktree", false, "do not remove the worktree after the run (debug)")
	cmd.Flags().DurationVar(&f.timeout, "timeout", 0, "wall-clock timeout (e.g. 10m); 0 = none")
	cmd.Flags().StringVar(&f.cpus, "cpus", "", "CPU limit (e.g. 2)")
	cmd.Flags().StringVar(&f.memory, "memory", "", "memory limit (e.g. 4g)")
	cmd.Flags().IntVar(&f.pids, "pids-limit", 1024, "max processes in the sandbox (0 = unlimited; default guards against fork bombs)")
	cmd.Flags().StringVar(&f.network, "network", "bridge", "container network mode (bridge|host|none)")
	cmd.Flags().BoolVar(&f.noStore, "no-store", false, "do not persist run/events to .sandcode/store.db")
	cmd.Flags().IntVar(&f.parallel, "parallel", 1, "replicate the agent N times in parallel (ignored when --agent is comma-separated)")
	cmd.Flags().IntVar(&f.maxConc, "max-concurrency", 0, "cap concurrent sub-runs (0 = run all at once)")
	cmd.Flags().StringVar(&f.judgeKind, "judge", "none", "rank parallel results: none|llm")
	cmd.Flags().StringVar(&f.judgeModel, "judge-model", "claude-haiku-4-5-20251001", "model used by --judge=llm")
	cmd.Flags().StringVar(&f.llmAuth, "llm-auth", "auto", "auth for Go-side LLM features (judge/review/architect/etc): auto|subscription|api-key. auto = api-key when ANTHROPIC_API_KEY is set, else the claude subscription CLI")
	cmd.Flags().BoolVar(&f.learn, "learn", false, "enable cognitive learning: enrich prompts with past lessons and learn from outcomes")
	cmd.Flags().BoolVar(&f.architect, "architect", false,
		"design solution guidance before the run (kernel-stage; needs ANTHROPIC_API_KEY; requires --learn)")
	cmd.Flags().BoolVar(&f.stepBack, "step-back", false,
		"distill high-level reframing principles before the run (kernel-stage; needs ANTHROPIC_API_KEY; requires --learn)")
	cmd.Flags().BoolVar(&f.planStage, "plan", false,
		"kernel decomposes high-complexity prompts into a TaskDAG (needs ANTHROPIC_API_KEY; requires --learn)")
	cmd.Flags().BoolVar(&f.strategySel, "strategy-select", false,
		"kernel picks execution shape single/refine/parallel from classification+plan (requires --learn)")
	cmd.Flags().BoolVar(&f.reactive, "reactive", false,
		"drive the run through the deterministic reactor (SP3 bus-mediation): cognitive pipeline (classify→enrich) and the execute/verify/lint/refine cycle. Byte-identical results; requires --learn")
	cmd.Flags().BoolVar(&f.dag, "dag", false, "execute as multi-node DAG plan; forces planner LLM if Plan empty after kernel. Cost scales as outerN × chains × nodes × refineMaxAttempts; use --max-cost-usd to cap.")
	cmd.Flags().StringVar(&f.dagFromFile, "dag-from-file", "", "load TaskDAG plan from JSON file (deterministic input, overrides planner output). Requires --dag.")
	cmd.Flags().BoolVar(&f.report, "report", false,
		"generate a deterministic REPORT.md after the run (writes to the worktree)")
	cmd.Flags().BoolVar(&f.review, "review", false,
		"LLM code review of the diff after the run (needs ANTHROPIC_API_KEY); adds a Review section/event")
	cmd.Flags().BoolVar(&f.securityReview, "security-review", false,
		"deterministic secret scan of the diff after the run (no key); adds a ## Security section/event")
	cmd.Flags().BoolVar(&f.securityReviewLLM, "security-review-llm", false,
		"LLM vuln+secret review of the diff (needs ANTHROPIC_API_KEY; overrides --security-review)")
	cmd.Flags().BoolVar(&f.perfReview, "perf-review", false,
		"LLM performance review of the diff after the run (needs ANTHROPIC_API_KEY); adds a ## Performance section/event")
	cmd.Flags().BoolVar(&f.refactorReview, "refactor-review", false,
		"LLM refactoring review of the diff after the run (needs ANTHROPIC_API_KEY); adds a ## Refactoring section/event")
	cmd.Flags().StringArrayVar(&f.roles, "role", nil, "bind a role to an agent: --role role=agent (repeatable; SP1 only acts on implementer)")
	cmd.Flags().StringSliceVar(&f.mcp, "mcp", nil, "enable MCP servers by name (repeatable): context7|claude-mem")
	cmd.Flags().DurationVar(&f.approvalTimeout, "approval-timeout", 5*time.Minute, "max wait for a governance Review approval (terminal prompt) before failing the run")
	registerGateFlags(cmd, &f.gateFlags)
	return cmd
}

// runParallel dispatches a multi-agent fan-out and renders interleaved output.
func runParallel(
	ctx context.Context,
	sb sandbox.Provider,
	au auth.Provider,
	st store.Store,
	br brain.Brain,
	kn *kernel.Kernel,
	bus event.Bus,
	engine *governance.Engine,
	guard *budget.Guard,
	refineOpts orchestrator.RefineOptions,
	agents []agent.Provider,
	prompt, cwd string,
	strategy gitm.Strategy,
	limits sandbox.Limits,
	f runFlags,
) error {
	// In parallel mode we default to branch strategy because multiple
	// successful runs cannot all merge to HEAD without conflicts. With a
	// judge configured, ParallelRun internally keeps the user's strategy
	// and merges only the winner.
	parStrat := strategy

	names := make([]string, len(agents))
	for i, ag := range agents {
		names[i] = ag.Name()
	}

	// Resolve judge.
	var jg judge.Judge
	switch strings.ToLower(f.judgeKind) {
	case "", "none", "off":
		// no judge
	case "llm":
		j, err := newJudge(f)
		if err != nil {
			return err
		}
		jg = j
	default:
		return fmt.Errorf("unknown --judge %q (want none|llm)", f.judgeKind)
	}

	fmt.Printf("\033[1;36m[sandcode]\033[0m parallel run on %s — agents=%v strategy=%s judge=%v\n",
		sb.Name(), names, parStrat, judgeName(jg))

	// MCP wiring (shared across all sub-runs). The claude ExtraArgs key off
	// agents[0] (the fan-out agents are homogeneous in practice).
	mcpMgr, err := buildMCPManager(f.mcp)
	if err != nil {
		return err
	}
	agentOpts := agent.RunOptions{Model: f.model}
	agentOpts.ExtraArgs = mcpExtraArgs(mcpMgr, agents[0], nil)

	popts := orchestrator.ParallelOptions{
		Prompt:         prompt,
		CWD:            cwd,
		SandboxImage:   f.image,
		SandboxWorkDir: f.workdir,
		Strategy:       parStrat,
		KeepWorktrees:  f.keepWorktree,
		Timeout:        f.timeout,
		Network:        f.network,
		Limits:         limits,
		MaxConcurrency: f.maxConc,
		Agents:         agents,
		AgentOpts:      agentOpts,
		Store:          st,
		Judge:          jg,
		Brain:          br,
		Kernel:         kn,
		Bus:            bus,
		Governance:     engine,
		Budget:         guard,
		Refine:         refineOpts,
		MCP:            mcpMgr,
	}

	events, await, err := orchestrator.ParallelRun(ctx, sb, au, popts)
	if err != nil {
		return err
	}
	for ev := range events {
		renderSubEvent(ev)
	}

	pr := await()
	fmt.Println()
	fmt.Printf("\033[1;36m[sandcode]\033[0m parent %s — duration=%s\n",
		pr.ParentRunID, pr.Finished.Sub(pr.Started).Round(time.Millisecond))
	for _, sub := range pr.Sub {
		dur := sub.Result.Finished.Sub(sub.Result.Started).Round(time.Millisecond)
		fmt.Printf("  • %-12s %s exit=%d %s\n", sub.Agent, sub.Result.Status, sub.Result.ExitCode, dur)
		if sub.Result.Worktree != nil {
			fmt.Printf("    branch=%s\n", sub.Result.Worktree.Branch)
		}
	}
	if pr.Ranking != nil {
		fmt.Println()
		fmt.Printf("\033[1;36m[sandcode]\033[0m ranking by %s\n", pr.Ranking.Judge)
		for _, sub := range pr.Sub {
			score := pr.Ranking.Scores[sub.Result.RunID]
			marker := "  "
			if sub.Result.RunID == pr.Ranking.Winner {
				marker = "★ "
			}
			fmt.Printf("  %s%-12s score=%.2f run=%s\n", marker, sub.Agent, score, sub.Result.RunID)
		}
		if pr.Ranking.Rationale != "" {
			fmt.Printf("  rationale: %s\n", pr.Ranking.Rationale)
		}
	} else if pr.JudgeErr != nil {
		fmt.Printf("\033[31m[sandcode]\033[0m judge failed: %v\n", pr.JudgeErr)
	}
	if pr.WinnerErr != nil {
		fmt.Printf("\033[31m[sandcode]\033[0m winner-merge failed: %v\n", pr.WinnerErr)
	}
	for _, sub := range pr.Sub {
		if sub.Result.Status != "success" {
			return fmt.Errorf("%s: %s", sub.Agent, sub.Result.Status)
		}
	}
	return nil
}

func judgeName(j judge.Judge) string {
	if j == nil {
		return "none"
	}
	return j.Name()
}

func renderSubEvent(ev orchestrator.SubEvent) {
	prefix := fmt.Sprintf("\033[2m[%s/%d]\033[0m ", ev.Agent, ev.Slot)
	switch ev.Event.Kind {
	case agent.EventText:
		fmt.Printf("%s%s\n", prefix, ev.Event.Text)
	case agent.EventToolCall:
		fmt.Printf("%s\033[33m▶ %s\033[0m %s\n", prefix, ev.Event.ToolName, truncate(ev.Event.ToolInput, 160))
	case agent.EventWarning:
		fmt.Printf("%s\033[31m! %s\033[0m\n", prefix, ev.Event.Text)
	case agent.EventSession:
		// hide noisy session pings
	}
}

func renderEvent(ev agent.StreamEvent) {
	switch ev.Kind {
	case agent.EventText:
		fmt.Println(ev.Text)
	case agent.EventToolCall:
		fmt.Printf("\033[33m▶ %s\033[0m %s\n", ev.ToolName, truncate(ev.ToolInput, 200))
	case agent.EventWarning:
		fmt.Printf("\033[31m! %s\033[0m\n", ev.Text)
	case agent.EventSession:
		fmt.Printf("\033[2msession=%s\033[0m\n", ev.SessionID)
	default:
		// raw — drop verbose JSON
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// loadDAGFromFile reads a JSON file matching planner.TaskDAG and
// validates it. Used by `sandcode run --dag --dag-from-file <path>`
// for deterministic plan input (tests, demos, reproducing bugs).
func loadDAGFromFile(path string) (planner.TaskDAG, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return planner.TaskDAG{}, fmt.Errorf("read dag file: %w", err)
	}
	var plan planner.TaskDAG
	if err := json.Unmarshal(data, &plan); err != nil {
		return planner.TaskDAG{}, fmt.Errorf("parse dag json: %w", err)
	}
	if err := plan.Validate(); err != nil {
		return planner.TaskDAG{}, fmt.Errorf("dag validate: %w", err)
	}
	return plan, nil
}

// runDAGCommand is the CLI-side entry point for `--dag` mode. Resolves
// the plan, builds DAGOptions, and dispatches single or outer-fanned-out
// DAGRun. Outer fan-out is engaged when --parallel >1; otherwise it
// runs one DAG and returns.
func runDAGCommand(
	ctx context.Context,
	sb sandbox.Provider,
	agents []agent.Provider,
	au auth.Provider,
	st store.Store,
	kn *kernel.Kernel,
	bus event.Bus,
	engine *governance.Engine,
	guard *budget.Guard,
	refineOpts orchestrator.RefineOptions,
	prompt, cwd string,
	strategy gitm.Strategy,
	limits sandbox.Limits,
	f runFlags,
) error {
	return runDAGCommandWithJudge(ctx, sb, agents, au, st, kn, bus, engine, guard, refineOpts, prompt, cwd, strategy, limits, f, nil)
}

// runDAGCommandWithJudge is the test-injectable variant of runDAGCommand.
// When optionalJudge is non-nil, it overrides the f.judgeKind-driven
// LLM judge construction — letting tests substitute a stub judge that
// doesn't require ANTHROPIC_API_KEY. Production callers should use
// runDAGCommand (which passes nil).
func runDAGCommandWithJudge(
	ctx context.Context,
	sb sandbox.Provider,
	agents []agent.Provider,
	au auth.Provider,
	st store.Store,
	kn *kernel.Kernel,
	bus event.Bus,
	engine *governance.Engine,
	guard *budget.Guard,
	refineOpts orchestrator.RefineOptions,
	prompt, cwd string,
	strategy gitm.Strategy,
	limits sandbox.Limits,
	f runFlags,
	optionalJudge judge.Judge,
) error {
	// agents[0] is the fallback for single-agent chains; len > 1 enables
	// within-DAG round-robin via DAGOptions.Agents (Slice 5 / Slice 6).
	if len(agents) == 0 {
		return fmt.Errorf("--dag: no agents resolved")
	}
	ag := agents[0]

	// MCP wiring (shared across all chains). nil ⇒ byte-identical legacy.
	mcpMgr, err := buildMCPManager(f.mcp)
	if err != nil {
		return err
	}
	agentOpts := agent.RunOptions{Model: f.model}
	agentOpts.ExtraArgs = mcpExtraArgs(mcpMgr, ag, nil)

	// 1. Resolve plan.
	var plan planner.TaskDAG
	switch {
	case f.dagFromFile != "":
		plan, err = loadDAGFromFile(f.dagFromFile)
		if err != nil {
			return err
		}
	case kn != nil:
		// Reuse kernel's planner via ForcePlan (bypasses complexity gate).
		plan, err = kn.ForcePlan(ctx, prompt)
		if err != nil {
			return fmt.Errorf("force plan via kernel: %w", err)
		}
	default:
		// No kernel (no --learn); construct an LLM planner directly.
		pl, err := newPlanner(f)
		if err != nil {
			return fmt.Errorf("--dag without --learn needs a planner (ANTHROPIC_API_KEY or claude subscription): %w", err)
		}
		plan, err = pl.Decompose(ctx, prompt)
		if err != nil {
			return fmt.Errorf("planner decompose: %w", err)
		}
	}

	// 2. Resolve judge — required when plan has multiple roots.
	// optionalJudge wins (tests inject stubs); else fall back to the
	// f.judgeKind-driven LLM construction (production path).
	var jud judge.Judge
	if len(plan.Roots()) > 1 {
		switch {
		case optionalJudge != nil:
			jud = optionalJudge
		case f.judgeKind == "llm":
			jud, err = newJudge(f)
			if err != nil {
				return fmt.Errorf("--dag with multi-root plan: judge: %w", err)
			}
		default:
			return fmt.Errorf("--dag with multi-root plan requires --judge=llm")
		}
	}

	dagOpts := orchestrator.DAGOptions{
		Prompt:         prompt,
		CWD:            cwd,
		SandboxImage:   f.image,
		SandboxWorkDir: f.workdir,
		Strategy:       strategy,
		KeepWorktree:   f.keepWorktree,
		Timeout:        f.timeout,
		Limits:         limits,
		Network:        f.network,
		AgentOpts:      agentOpts,
		Store:          st,
		Kernel:         kn,
		Bus:            bus,
		Refine:         refineOpts,
		Governance:     engine,
		Budget:         guard,
		Plan:           plan,
		Judge:          jud,
		MCP:            mcpMgr,
	}
	if len(agents) > 1 {
		dagOpts.Agents = agents
	}

	return runDAGWithOuter(ctx, sb, ag, au, dagOpts, f)
}

// runDAGWithOuter handles γ composition: when --parallel >1, replicates
// the entire DAG pipeline outerN times in parallel and reports each
// copy's outcome. Single-copy path is the common case.
func runDAGWithOuter(
	ctx context.Context,
	sb sandbox.Provider,
	ag agent.Provider,
	au auth.Provider,
	opts orchestrator.DAGOptions,
	f runFlags,
) error {
	outerN := f.parallel
	if outerN < 1 {
		outerN = 1
	}

	if outerN == 1 {
		fmt.Printf("\033[1;36m[sandcode]\033[0m DAG run: %d nodes / %d roots\n",
			len(opts.Plan.Nodes), len(opts.Plan.Roots()))
		_, await, err := orchestrator.DAGRun(ctx, sb, ag, au, opts)
		if err != nil {
			return err
		}
		res := await()
		printDAGResult(res)
		return res.Error
	}

	fmt.Printf("\033[1;36m[sandcode]\033[0m DAG outer fan-out: %d copies × %d nodes / %d roots\n",
		outerN, len(opts.Plan.Nodes), len(opts.Plan.Roots()))

	results := make([]orchestrator.DAGResult, outerN)
	var wg sync.WaitGroup
	for i := 0; i < outerN; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			cp := opts
			cp.OuterCopyIndex = idx
			cp.RunID = fmt.Sprintf("dag-%d", idx)
			_, await, err := orchestrator.DAGRun(ctx, sb, ag, au, cp)
			if err != nil {
				results[idx] = orchestrator.DAGResult{Error: err, OuterCopyIndex: idx}
				return
			}
			results[idx] = await()
		}(i)
	}
	wg.Wait()

	for _, r := range results {
		printDAGResult(r)
	}
	for _, r := range results {
		if r.Error == nil {
			return nil
		}
	}
	return orchestrator.ErrAllOuterCopiesFailed
}

func printDAGResult(r orchestrator.DAGResult) {
	fmt.Printf("\033[1;36m[sandcode]\033[0m DAG copy %d: chains=%d winner=%s err=%v duration=%s\n",
		r.OuterCopyIndex, len(r.Chains), r.Winner, r.Error, r.Duration.Round(time.Millisecond))
}
