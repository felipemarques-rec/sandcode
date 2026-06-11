package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/agent"
	"github.com/felipemarques-rec/sandcode/internal/approval"
	"github.com/felipemarques-rec/sandcode/internal/auth"
	"github.com/felipemarques-rec/sandcode/internal/brain"
	"github.com/felipemarques-rec/sandcode/internal/budget"
	"github.com/felipemarques-rec/sandcode/internal/event"
	gitm "github.com/felipemarques-rec/sandcode/internal/git"
	"github.com/felipemarques-rec/sandcode/internal/governance"
	"github.com/felipemarques-rec/sandcode/internal/governance/builtin"
	"github.com/felipemarques-rec/sandcode/internal/judge"
	"github.com/felipemarques-rec/sandcode/internal/kernel"
	"github.com/felipemarques-rec/sandcode/internal/langfuse"
	"github.com/felipemarques-rec/sandcode/internal/mcp"
	"github.com/felipemarques-rec/sandcode/internal/metrics"
	"github.com/felipemarques-rec/sandcode/internal/orchestrator"
	"github.com/felipemarques-rec/sandcode/internal/rbac"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
	"github.com/felipemarques-rec/sandcode/internal/scheduler"
	"github.com/felipemarques-rec/sandcode/internal/server"
	"github.com/felipemarques-rec/sandcode/internal/store"
	"github.com/spf13/cobra"
)

// gateFlags carries the governance / budget / refine knobs shared by
// both `sandcode serve` and `sandcode run`. Embedded into each command's
// flag struct so we register and consume the same set of CLI knobs in
// both call sites — see buildGates for the wiring contract.
type gateFlags struct {
	maxTokens         int64
	maxCostUSD        float64
	retryLimit        int
	refine            bool
	refineVerify      []string
	refineMaxAttempts int
	lintCmd           []string // Linter Gate command (E1.5b); engages the refine loop
}

type serveFlags struct {
	addr           string
	apiToken       string
	cwd            string
	sandboxKind    string
	defaultImage   string
	defaultAgent   string
	authMode       string
	noStore        bool
	noEventStore   bool
	noAudit        bool
	noLearn        bool
	stateCacheSize int
	judgeKind      string
	judgeModel     string
	roles          []string
	mcp            []string // --mcp: MCP servers to enable (e.g. context7)
	rbacConfig     string   // --rbac-config: RBAC config JSON; supersedes --api-token

	rateLimit   float64  // --rate-limit: req/s per client IP (0 = disabled)
	rateBurst   int      // --rate-burst: token-bucket capacity
	corsOrigins []string // --cors-origin: exact-match allowlist (repeatable)

	maxConcurrentRuns int
	queueCapacity     int

	approvalTimeout time.Duration

	gateFlags
}

func newServeCmd() *cobra.Command {
	var f serveFlags
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the sandcode HTTP API server",
		Long: `Start the sandcode HTTP API + observability server.

Endpoints:
  GET  /healthz                       — liveness probe
  GET  /metrics                       — Prometheus text format
  POST /v1/runs                       — submit a run (202 + Location)
  GET  /v1/runs                       — list recent runs (?limit, ?phase)
  GET  /v1/runs/{id}                  — current state machine snapshot
  GET  /v1/runs/{id}/events           — Server-Sent Events live tail
                                        ?from=<event_id> replays history
                                        Last-Event-ID header also honored
  GET  /v1/runs/{id}/audit            — governance decisions (allow/deny/review)
                                        ?result=allow|deny|review filters by verdict

Server-wide gates (applied to every launched run):
  --max-tokens / --max-cost-usd       — Budget policy ceilings
  --retry-limit                       — RetryLimit policy on refine
  --refine + --refine-verify          — enable verify+refine loop
  --refine-max-attempts               — cap refine iterations`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			return runServe(ctx, f)
		},
	}

	cmd.Flags().StringVar(&f.addr, "addr", "127.0.0.1:8080", "address to bind (loopback by default; a non-loopback bind requires --api-token or --rbac-config)")
	cmd.Flags().StringVar(&f.apiToken, "api-token", "", "bearer token required on all endpoints except /healthz (or set SANDCODE_API_TOKEN). Required for a non-loopback bind unless --rbac-config is set")
	cmd.Flags().StringVar(&f.cwd, "cwd", "", "host repository to run against (default: current dir)")
	cmd.Flags().StringVar(&f.sandboxKind, "sandbox", "docker", "sandbox provider: docker|podman|nosandbox")
	cmd.Flags().StringVar(&f.defaultImage, "default-image", "sandcode-default:latest", "fallback container image when the request omits one")
	cmd.Flags().StringVar(&f.defaultAgent, "agent", "claude-code", "agent to invoke for every run")
	cmd.Flags().StringVar(&f.authMode, "auth-mode", "bindmount", "auth strategy: bindmount|api-key")
	cmd.Flags().BoolVar(&f.noStore, "no-store", false, "disable run/event store persistence")
	cmd.Flags().BoolVar(&f.noEventStore, "no-event-store", false, "disable the append-only event_log (separate from --no-store)")
	cmd.Flags().BoolVar(&f.noAudit, "no-audit", false, "disable the governance audit_log (allow/deny/review decisions)")
	cmd.Flags().BoolVar(&f.noLearn, "no-learn", false, "disable cognitive brain wiring")
	cmd.Flags().IntVar(&f.stateCacheSize, "state-cache-size", 0, "max in-memory state machines (0 = default 1024)")
	cmd.Flags().IntVar(&f.maxConcurrentRuns, "max-concurrent-runs", 0, "bounded concurrent runs via the in-process scheduler (0 = disabled, unbounded goroutine-per-run)")
	cmd.Flags().IntVar(&f.queueCapacity, "queue-capacity", 256, "scheduler waiting-queue capacity; full => 429 (only used when --max-concurrent-runs > 0)")
	cmd.Flags().StringVar(&f.judgeKind, "judge", "none", "rank multi-root DAG chains: none|llm (required for HTTP DAG auto-dispatch)")
	cmd.Flags().StringVar(&f.judgeModel, "judge-model", "claude-haiku-4-5-20251001", "model used by --judge=llm")
	cmd.Flags().StringArrayVar(&f.roles, "role", nil, "bind a role to an agent: --role role=agent (repeatable; SP1 only acts on implementer)")
	cmd.Flags().StringSliceVar(&f.mcp, "mcp", nil, "enable MCP servers by name (repeatable): context7|claude-mem")
	cmd.Flags().StringVar(&f.rbacConfig, "rbac-config", "", "path to an RBAC config JSON (multi-key identity + role grants); when set, supersedes --api-token")
	cmd.Flags().Float64Var(&f.rateLimit, "rate-limit", 0, "per-client-IP request rate limit in req/s (0 = disabled)")
	cmd.Flags().IntVar(&f.rateBurst, "rate-burst", 0, "rate-limit token-bucket capacity (default: ceil(--rate-limit))")
	cmd.Flags().StringArrayVar(&f.corsOrigins, "cors-origin", nil, "allowed CORS origin, exact match (repeatable; \"*\" allows any). Empty = CORS disabled")
	cmd.Flags().DurationVar(&f.approvalTimeout, "approval-timeout", 5*time.Minute, "max wait for a governance Review approval before failing the run")

	registerGateFlags(cmd, &f.gateFlags)
	return cmd
}

// buildGates assembles the governance / budget / refine configuration
// from a gateFlags. Returns nil pointers (and a zero-value
// RefineOptions) when no flags are set so the orchestrator skips those
// pipelines entirely — disabled flags must cost zero at runtime.
//
// Validation catches operator mistakes at startup so we don't accept
// requests then deny every refine cycle with a "verify cmd missing"
// log.
func buildGates(g gateFlags) (*governance.Engine, *budget.Guard, orchestrator.RefineOptions, error) {
	if g.refine && len(g.refineVerify) == 0 && len(g.lintCmd) == 0 {
		return nil, nil, orchestrator.RefineOptions{},
			fmt.Errorf("--refine requires --refine-verify (e.g. --refine-verify go,test,./...) or --lint-cmd")
	}

	var policies []governance.Policy
	if g.maxTokens > 0 || g.maxCostUSD > 0 {
		policies = append(policies, builtin.Budget{
			MaxTokens:  g.maxTokens,
			MaxCostUSD: g.maxCostUSD,
		})
	}
	if g.retryLimit > 0 {
		policies = append(policies, builtin.RetryLimit{MaxAttempts: g.retryLimit})
	}
	var engine *governance.Engine
	if len(policies) > 0 {
		engine = governance.NewEngine(policies...)
	}

	var guard *budget.Guard
	if g.maxTokens > 0 || g.maxCostUSD > 0 {
		guard = budget.New()
	}

	// A configured Linter Gate engages the refine loop even without an explicit
	// --refine (the loop carries the attempt budget the lint gate refines on).
	refineOpts := orchestrator.RefineOptions{
		Enabled:     g.refine || len(g.lintCmd) > 0,
		VerifyCmd:   g.refineVerify,
		MaxAttempts: g.refineMaxAttempts,
	}

	return engine, guard, refineOpts, nil
}

// registerGateFlags attaches the shared gate knobs to a cobra command's
// flag set. Both `serve` and `run` call this to stay in lock-step on
// names and defaults.
func registerGateFlags(cmd *cobra.Command, g *gateFlags) {
	cmd.Flags().Int64Var(&g.maxTokens, "max-tokens", 0, "per-run token ceiling enforced by governance Budget policy (0 = disabled)")
	cmd.Flags().Float64Var(&g.maxCostUSD, "max-cost-usd", 0, "per-run USD ceiling enforced by governance Budget policy (0 = disabled)")
	cmd.Flags().IntVar(&g.retryLimit, "retry-limit", 0, "max refine attempts enforced by governance RetryLimit policy (0 = disabled, defers to --refine-max-attempts)")
	cmd.Flags().BoolVar(&g.refine, "refine", false, "enable the verify+refine loop on every run (requires --refine-verify; applies per-agent in parallel mode)")
	cmd.Flags().StringSliceVar(&g.refineVerify, "refine-verify", nil, "verify command for the refine loop, e.g. 'go,test,./...' or 'pytest,-x'")
	cmd.Flags().IntVar(&g.refineMaxAttempts, "refine-max-attempts", 0, "cap on refine iterations (0 = orchestrator default of 3)")
	cmd.Flags().StringSliceVar(&g.lintCmd, "lint-cmd", nil, "Linter Gate command run in the sandbox after a passing verify, e.g. 'golangci-lint,run'; lint failure triggers refine and fails the run when attempts are exhausted (engages the refine loop)")
}

func runServe(ctx context.Context, f serveFlags) error {
	cwd := f.cwd
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return err
		}
	}

	sb, err := resolveSandbox(f.sandboxKind)
	if err != nil {
		return err
	}
	ag, err := resolveAgent(f.defaultAgent)
	if err != nil {
		return err
	}
	au, err := resolveAuth(f.authMode)
	if err != nil {
		return err
	}

	bus := event.NewLocalBus()
	defer bus.Close()

	// Persistence: run store (rows) and the append-only event log are
	// independent. The CLI can disable either.
	var runStore store.Store
	if !f.noStore {
		db, err := store.Open(resolveStorePath(cwd))
		if err != nil {
			return fmt.Errorf("open store: %w", err)
		}
		defer db.Close()
		runStore = db
	}
	var eventStore event.Store
	if !f.noEventStore {
		es, err := event.OpenStore(resolveEventStorePath(cwd))
		if err != nil {
			return fmt.Errorf("open event store: %w", err)
		}
		defer es.Close()
		sub := event.PersistTo(bus, es)
		defer sub.Cancel()
		eventStore = es
	}

	var auditLog governance.AuditLog
	if !f.noAudit {
		al, err := governance.OpenAuditLog(resolveAuditPath(cwd))
		if err != nil {
			return fmt.Errorf("open audit log: %w", err)
		}
		defer al.Close()
		auditLog = al
	}

	// Langfuse LLM observability (auto-detected from env)
	var lf *langfuse.Provider
	lfCfg := langfuse.ConfigFromEnv()
	if lfCfg.Enabled {
		prov, lerr := langfuse.Init(ctx, lfCfg)
		if lerr != nil {
			fmt.Fprintf(os.Stderr, "warning: langfuse init failed: %v\n", lerr)
		} else {
			lf = prov
			defer func() {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = lf.Shutdown(shutdownCtx)
			}()
		}
	}

	// Brain (opt-in): wires kernel + lesson learning into every run.
	// If the run store is also enabled, hand it to the brain as the
	// episodic-memory tier so Enrich() blends lessons + past runs.
	var br brain.Brain
	var kn *kernel.Kernel
	if !f.noLearn {
		bdb, err := brain.OpenBrain(resolveBrainPath(cwd))
		if err != nil {
			return fmt.Errorf("open brain: %w", err)
		}
		defer bdb.Close()
		if sqliteStore, ok := runStore.(*store.SQLite); ok && sqliteStore != nil {
			bdb.WithEpisodic(sqliteStore.AsTier())
		}
		br = bdb
		kn = kernel.New(br,
			kernel.WithBus(bus),
			kernel.WithTracer(orchestrator.NewKernelTracer(lf)),
		)
	}

	// State cache: per-run ExecutionState surfaced by /v1/runs/{id}.
	stateCache := server.NewStateCache(f.stateCacheSize)
	defer stateCache.Attach(bus).Cancel()

	// Metrics: register the standard families + attach the event subscriber.
	// The state-cache size gauge is a GaugeFunc that re-reads at scrape
	// time, so /metrics always shows the current cache occupancy without
	// any push path from the cache mutator.
	reg := metrics.NewRegistry()
	metricsSub := metrics.NewSubscriber(reg)
	defer metricsSub.Attach(bus).Cancel()
	reg.NewGaugeFunc(
		"sandcode_state_cache_runs",
		"Current number of run state machines held in the server's in-memory cache.",
		func() float64 { return float64(stateCache.Len()) },
	)

	engine, guard, refineOpts, err := buildGates(f.gateFlags)
	if err != nil {
		return err
	}

	// Optional judge — required only when DAG auto-dispatch fires on a
	// multi-root plan. nil is fine for single/refine/parallel paths.
	var jg judge.Judge
	switch strings.ToLower(f.judgeKind) {
	case "", "none", "off":
		// no judge
	case "llm":
		j, err := judge.NewLLMJudgeFromEnv(f.judgeModel)
		if err != nil {
			return fmt.Errorf("--judge=llm: %w", err)
		}
		jg = j
	default:
		return fmt.Errorf("unknown --judge %q (want none|llm)", f.judgeKind)
	}

	// Build role registry from --role flags (single-agent path only).
	// Returns nil when no --role flags are provided (legacy path).
	roleReg, err := buildRoleRegistry(f.roles)
	if err != nil {
		return err
	}

	// Build the MCP manager once from --mcp (validates names); nil ⇒ legacy.
	mcpMgr, err := buildMCPManager(f.mcp)
	if err != nil {
		return err
	}

	// Load the RBAC keyring from --rbac-config. Empty flag ⇒ nil keyring ⇒
	// byte-identical legacy auth (--api-token / loopback). A parse/validation
	// error is fatal to startup, mirroring the other serve-setup failures.
	var kr *rbac.Keyring
	if f.rbacConfig != "" {
		kr, err = rbac.LoadConfig(f.rbacConfig)
		if err != nil {
			return fmt.Errorf("--rbac-config: %w", err)
		}
	}

	// When a keyring is configured, append the ToolPermission governance policy
	// to the launcher's engine. If buildGates produced no engine (no budget /
	// retry flags), create one holding just ToolPermission — so non-RBAC runs
	// stay byte-identical (nil engine) while RBAC always gets the audit seam.
	//
	// ToolPermission gates per-tool governance Actions; at today's execute/refine
	// gates Action.Tool is empty so it no-ops — the active per-tool enforcement is
	// the MCP --allowedTools boundary (permittedFor). This policy is the
	// replay-safe audit seam + forward hook.
	if kr != nil {
		permits := func(roles []string, tool string) bool {
			return kr.RoleSet().Resolve(rbac.Principal{Roles: roles}).AllowsTool(tool)
		}
		toolPerm := builtin.ToolPermission{Permits: permits}
		if engine == nil {
			engine = governance.NewEngine(toolPerm)
		} else if aerr := engine.AddPolicy(toolPerm); aerr != nil {
			return fmt.Errorf("--rbac-config: %w", aerr)
		}
	}

	// One shared approval registry: the HTTP server records approve/deny
	// decisions against it and the launcher's runs block on it.
	approvalReg := approval.NewRegistry()

	launcher := &orchestratorLauncher{
		sb:           sb,
		ag:           ag,
		au:           au,
		runStore:     runStore,
		bus:          bus,
		brain:        br,
		kernel:       kn,
		defaultImage: f.defaultImage,
		governance:   engine,
		budget:       guard,
		refine:       refineOpts,
		lintCmd:      f.lintCmd,
		audit:        auditLog,
		judge:        jg,
		langfuse:     lf,
		registry:     roleReg,
		mcp:          mcpMgr,
		roleSet:      roleSetOf(kr),

		approvals:       approvalReg,
		approvalTimeout: approvalTimeoutOrDefault(f.approvalTimeout),
	}

	apiToken := f.apiToken
	if apiToken == "" {
		apiToken = os.Getenv("SANDCODE_API_TOKEN")
	}

	srv := server.New(server.Options{
		Registry:   reg,
		StateCache: stateCache,
		Bus:        bus,
		Store:      eventStore,
		Audit:      auditLog,
		Launcher:   launcher,
		Approvals:  approvalReg,
		Keyring:    kr,

		// Security: require a token unless bound to loopback (enforced in
		// serve()), and pin client-supplied CWD to the server's own repo. A
		// configured keyring is the sole auth source and supersedes --api-token.
		AuthToken:       effectiveAuthToken(apiToken, kr),
		AllowedCWDRoots: []string{cwd},

		SchedulerConfig: buildSchedulerConfig(f.maxConcurrentRuns, f.queueCapacity),

		RateLimit: buildRateLimitConfig(f.rateLimit, f.rateBurst),
		CORS:      buildCORSConfig(f.corsOrigins),
	})

	fmt.Printf("\033[1;36m[sandcode]\033[0m serving on %s — sandbox=%s agent=%s\n",
		f.addr, sb.Name(), ag.Name())
	if err := srv.Run(ctx, f.addr); err != nil {
		return err
	}
	return nil
}

// buildRateLimitConfig maps the --rate-limit/--rate-burst flags to a server config.
// Returns nil when rate <= 0 (rate limiting disabled, byte-identical). Burst defaults
// to ceil(rate) (>= 1) when unset.
func buildRateLimitConfig(rate float64, burst int) *server.RateLimitConfig {
	if rate <= 0 {
		return nil
	}
	if burst <= 0 {
		burst = int(math.Ceil(rate))
		if burst < 1 {
			burst = 1
		}
	}
	return &server.RateLimitConfig{RequestsPerSecond: rate, Burst: burst}
}

// buildCORSConfig maps the --cors-origin flag to a server config. Empty ⇒ nil (CORS
// disabled, byte-identical).
func buildCORSConfig(origins []string) *server.CORSConfig {
	if len(origins) == 0 {
		return nil
	}
	return &server.CORSConfig{AllowedOrigins: origins}
}

// roleSetOf returns the keyring's RoleSet, or nil when kr is nil (no RBAC ⇒
// the launcher's tool filter stays nil ⇒ byte-identical).
func roleSetOf(kr *rbac.Keyring) rbac.RoleSet {
	if kr == nil {
		return nil
	}
	return kr.RoleSet()
}

// effectiveAuthToken returns the legacy bearer token to install on the server,
// or "" when a keyring is configured — the keyring then becomes the sole auth
// source, superseding --api-token / SANDCODE_API_TOKEN.
func effectiveAuthToken(apiToken string, kr *rbac.Keyring) string {
	if kr != nil {
		return ""
	}
	return apiToken
}

// approvalTimeoutOrDefault returns d, or 5m when d <= 0.
func approvalTimeoutOrDefault(d time.Duration) time.Duration {
	if d <= 0 {
		return 5 * time.Minute
	}
	return d
}

// buildSchedulerConfig returns a scheduler config only when both knobs
// are > 0; otherwise nil (the server then keeps the legacy unbounded path).
func buildSchedulerConfig(poolSize, queueCap int) *scheduler.Config {
	if poolSize <= 0 || queueCap <= 0 {
		return nil
	}
	return &scheduler.Config{PoolSize: poolSize, QueueCap: queueCap}
}

func resolveSandbox(kind string) (sandbox.Provider, error) {
	switch kind {
	case "docker":
		return sandbox.NewDockerProvider(), nil
	case "podman":
		return sandbox.NewPodmanProvider(), nil
	case "nosandbox":
		return sandbox.NewNoSandboxProvider(), nil
	default:
		return nil, fmt.Errorf("unknown --sandbox %q (want docker|podman|nosandbox)", kind)
	}
}

func resolveAuth(mode string) (auth.Provider, error) {
	switch mode {
	case "bindmount":
		return auth.NewBindMount(), nil
	case "api-key", "apikey":
		return auth.NewAPIKey(), nil
	default:
		return nil, fmt.Errorf("unknown --auth-mode %q (want bindmount|api-key)", mode)
	}
}

// resolveEventStorePath returns the SQLite path used by the append-only
// event_log. Co-located with the run store under .sandcode/ so a single
// directory holds everything sandcode persists per project.
func resolveEventStorePath(cwd string) string {
	return filepath.Join(cwd, ".sandcode", "events.db")
}

// resolveAuditPath returns the SQLite path used by the governance
// audit_log. Lives in the same .sandcode/ dir as the run + event stores.
func resolveAuditPath(cwd string) string {
	return filepath.Join(cwd, ".sandcode", "audit.db")
}

// orchestratorLauncher is the production server.Launcher implementation.
// It captures the long-lived providers and per-server resources, then on
// each Launch builds a RunOptions and invokes orchestrator.Run.
//
// Launch is invoked from a goroutine the HTTP handler owns. The contract
// (see server.Launcher) is "block for the run lifetime"; we therefore do
// not return until orchestrator.Run's await() completes.
type orchestratorLauncher struct {
	sb           sandbox.Provider
	ag           agent.Provider
	au           auth.Provider
	runStore     store.Store
	bus          event.Bus
	brain        brain.Brain
	kernel       *kernel.Kernel
	defaultImage string

	// Server-wide gates applied to every Launch. Nil values disable
	// the corresponding pipeline; see buildGates.
	governance *governance.Engine
	budget     *budget.Guard
	refine     orchestrator.RefineOptions
	lintCmd    []string // Linter Gate command (E1.5b); single/refine dispatch only
	audit      governance.AuditLog

	// approvals is the shared approval registry; the HTTP server resolves
	// pending approvals against the same instance. approvalTimeout caps how
	// long a launched run blocks on a governance Review approval.
	approvals       *approval.Registry
	approvalTimeout time.Duration

	// judge ranks multi-root DAG chains when Execute auto-dispatches to
	// DAGRun. nil disables DAG path (multi-root plans fall back to
	// parallel or single via the dispatch table).
	judge judge.Judge

	// langfuse, when non-nil and Enabled(), traces every Launch'd run.
	langfuse *langfuse.Provider

	// registry, when non-nil, enables opt-in role-based agent resolution
	// (SP1 — single-agent path only). Built once at serve startup from
	// --role flags and shared across all launches.
	registry agent.Registry

	// mcp, when non-nil, injects a .mcp.json into every launched run's
	// worktree. Built once at startup from --mcp; nil ⇒ legacy.
	mcp *mcp.Manager

	// roleSet, when non-nil, enables RBAC tool gating: the staged principal
	// is resolved against it to build the per-run tool-permission filter.
	// nil ⇒ no RBAC ⇒ byte-identical (nil tool filter, empty roles).
	// Task 9 populates this from the loaded keyring.
	roleSet rbac.RoleSet
}

// permittedFor returns a tool-permission filter for the principal's roles,
// or nil when rs is nil (no RBAC ⇒ allow all, byte-identical). An admin
// principal resolves to all-access; unknown roles contribute nothing.
func permittedFor(rs rbac.RoleSet, p rbac.Principal) func(tool string) bool {
	if rs == nil {
		return nil
	}
	grant := rs.Resolve(p)
	return func(tool string) bool { return grant.AllowsTool(tool) }
}

func (l *orchestratorLauncher) Launch(ctx context.Context, runID string, req server.RunRequest) error {
	image := req.SandboxImage
	if image == "" {
		image = l.defaultImage
	}
	strategy := gitm.Strategy(req.Strategy)
	if strategy == "" {
		strategy = gitm.StrategyMergeToHead
	}
	var timeout time.Duration
	if req.TimeoutSeconds > 0 {
		timeout = time.Duration(req.TimeoutSeconds) * time.Second
	}

	// Resolve the staged principal against the RoleSet into a tool-permission
	// filter. nil when l.roleSet == nil (no RBAC) ⇒ mcpExtraArgs allows all
	// ⇒ byte-identical.
	permitted := permittedFor(l.roleSet, req.Principal)

	opts := orchestrator.ExecuteOptions{
		RunID:           runID,
		Prompt:          req.Prompt,
		CWD:             req.CWD,
		SandboxImage:    image,
		SandboxWorkDir:  req.SandboxWorkDir,
		Strategy:        strategy,
		KeepWorktree:    req.KeepWorktree,
		Timeout:         timeout,
		Network:         req.Network,
		Store:           l.runStore,
		Agent:           l.ag,
		Kernel:          l.kernel,
		Bus:             l.bus,
		Governance:      l.governance,
		AuditLog:        l.audit,
		Approver:        l.approvals,
		ApprovalTimeout: l.approvalTimeout,
		Budget:          l.budget,
		Refine:          l.refine,
		LintCmd:         l.lintCmd,
		Judge:           l.judge,
		Langfuse:        l.langfuse,
		Registry:        l.registry,
		MCP:             l.mcp,
		Roles:           req.Principal.Roles, // empty when no principal ⇒ byte-identical
		AgentOpts:       agent.RunOptions{ExtraArgs: mcpExtraArgs(l.mcp, l.ag, permitted)},
	}
	if l.budget != nil {
		// Long-lived server: free per-run accounting once the run is
		// done so memory stays bounded. Forget is a no-op if the run
		// never recorded anything.
		defer l.budget.Forget(runID)
	}

	events, await, err := orchestrator.Execute(ctx, l.sb, l.au, opts)
	if err != nil {
		return err
	}
	// Drain the agent stream; HTTP clients subscribe via SSE for
	// lifecycle events. Streaming stdout would belong in a separate
	// "/v1/runs/{id}/stream" endpoint and is intentionally out of scope.
	for range events {
	}
	res := await()
	switch res.Kind {
	case orchestrator.DispatchSingle, orchestrator.DispatchRefine:
		if res.Run == nil {
			return errors.New("execute: run result missing")
		}
		if res.Run.Err != nil {
			return res.Run.Err
		}
		if res.Run.Status != "success" {
			return errors.New(res.Run.Status)
		}
	case orchestrator.DispatchParallel:
		if res.Parallel == nil {
			return errors.New("execute: parallel result missing")
		}
		for _, s := range res.Parallel.Sub {
			if s.Result.Err != nil {
				return s.Result.Err
			}
		}
	case orchestrator.DispatchDAG:
		if res.DAG == nil {
			return errors.New("execute: dag result missing")
		}
		if res.DAG.Error != nil {
			return res.DAG.Error
		}
	}
	return nil
}
