package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/approval"
	"github.com/felipemarques-rec/sandcode/internal/event"
	"github.com/felipemarques-rec/sandcode/internal/governance"
	"github.com/felipemarques-rec/sandcode/internal/metrics"
	"github.com/felipemarques-rec/sandcode/internal/ratelimit"
	"github.com/felipemarques-rec/sandcode/internal/rbac"
	"github.com/felipemarques-rec/sandcode/internal/scheduler"
)

// Options configures Server construction.
type Options struct {
	// Registry serves /metrics. Required.
	Registry *metrics.Registry

	// StateCache backs /v1/runs/{id}. Required.
	StateCache *StateCache

	// Bus backs /v1/runs/{id}/events (SSE). Optional — if nil the SSE
	// endpoint returns 503 ServiceUnavailable. POST /v1/runs is the
	// other side of the same bus: when Launcher is wired, it should
	// publish through this same bus so SSE consumers see the events.
	Bus event.Bus

	// Store is the append-only event log backing replay on
	// /v1/runs/{id}/events?from=<event_id>. Optional — when nil the
	// SSE endpoint behaves as live-tail-only and rejects any request
	// that specifies `from` with 503 ServiceUnavailable.
	Store event.Store

	// Audit is the governance audit log backing
	// GET /v1/runs/{id}/audit. Optional — when nil the endpoint
	// returns 503 ServiceUnavailable.
	Audit governance.AuditLog

	// Launcher kicks off runs accepted via POST /v1/runs. Optional —
	// if nil the POST endpoint returns 503.
	Launcher Launcher

	// Approvals, when non-nil, backs POST /v1/approvals/{id} (E2.3). nil ⇒ the
	// endpoint returns 503. Shared with the launcher so a blocked run and the
	// HTTP handler rendezvous on the same registry.
	Approvals *approval.Registry

	// AllowedCWDRoots constrains the host directories a RunRequest.CWD may
	// target. When non-empty, req.CWD must equal or be a subpath of one of
	// these roots (symlinks resolved). When empty, any CWD is accepted —
	// callers SHOULD set this (cmd serve pins it to the server's --cwd) to
	// stop a client from running an agent against arbitrary host paths.
	AllowedCWDRoots []string

	// AuthToken, when non-empty, requires every request (except GET
	// /healthz) to carry "Authorization: Bearer <AuthToken>". When empty,
	// no auth is enforced — which is only permitted on a loopback bind
	// (serve() refuses a non-loopback listener without a token). This gates
	// the RCE surface (POST /v1/runs) and the data-disclosure reads.
	AuthToken string

	// Keyring, when non-nil, enables RBAC: every request (except GET /healthz)
	// must carry "Authorization: Bearer <token>" matching a keyring entry, and
	// per-route capability gates apply. A configured Keyring SUPERSEDES
	// AuthToken (the legacy single-token path is ignored). nil ⇒ legacy
	// single-token path (byte-identical to pre-RBAC behavior).
	Keyring *rbac.Keyring

	// CORS, when non-nil and with a non-empty allowlist, enables CORS response
	// headers and preflight handling. nil/empty ⇒ no CORS (byte-identical).
	CORS *CORSConfig

	// RateLimit, when non-nil, enables per-client-IP token-bucket rate limiting.
	// nil ⇒ disabled (byte-identical).
	RateLimit *RateLimitConfig

	// Logger used for request and lifecycle logs. Defaults to slog.Default().
	Logger *slog.Logger

	// ReadHeaderTimeout caps how long the server waits for a request's
	// headers before timing out the connection. Defaults to 5s — guards
	// against Slowloris-style attacks even on localhost.
	ReadHeaderTimeout time.Duration

	// ShutdownTimeout caps how long graceful shutdown will wait for
	// in-flight requests. Defaults to 10s.
	ShutdownTimeout time.Duration

	// SSEKeepalive is the interval at which an SSE comment is sent to
	// keep proxies and load balancers from idling the connection out.
	// Defaults to 15s; set negative to disable.
	SSEKeepalive time.Duration

	// LaunchDrainTimeout caps how long the server waits for in-flight
	// Launcher invocations to finish after the HTTP listener has been
	// shut down. Defaults to 30s. The drain context is cancelled when
	// the timeout fires, so well-behaved launchers honouring ctx will
	// unwind quickly; uncooperative ones are leaked.
	LaunchDrainTimeout time.Duration

	// SchedulerConfig, when non-nil, enables the in-process run
	// scheduler (bounded, priority-ordered admission). Nil => the
	// legacy unbounded launchAsync path (byte-identical to pre-scheduler).
	SchedulerConfig *scheduler.Config
}

// Server is the HTTP front-end. Construct with New; start with Run.
type Server struct {
	opts Options
	mux  *http.ServeMux
	// handler is the mux wrapped with the auth middleware. Served by serve()
	// and returned by Handler(). When AuthToken is empty it is the bare mux.
	handler http.Handler
	// limiter backs withRateLimit. nil ⇒ rate limiting disabled. Built once in New.
	limiter *ratelimit.Limiter

	// inFlight tracks launcher goroutines so shutdown can wait on them.
	inFlight sync.WaitGroup

	// launchCtx is the context handed to every Launcher invocation. It
	// is rooted at serve()'s ctx so SIGINT propagates, but is decoupled
	// from individual HTTP request lifetimes (a client disconnect does
	// NOT cancel an in-progress run).
	launchMu     sync.Mutex
	launchCtx    context.Context
	launchCancel context.CancelFunc

	// sched is the opt-in run scheduler. Built by startScheduler before
	// the listener accepts; niled by stopScheduler at shutdown so a
	// re-serve() rebuilds a fresh one (symmetric with the launchCtx
	// reset). Set/cleared only on the serve() lifecycle thread (no HTTP
	// handler runs after Shutdown returns), so the unguarded access is
	// happens-before-safe. nil ⇒ legacy unbounded launchAsync path.
	// pending stages a run's RunRequest between handleCreateRun's Submit
	// and the pool slot's launchFunc (the scheduler never sees RunRequest
	// — decoupling seam). Every access is guarded by pendingMu.
	sched     scheduler.Scheduler
	pendingMu sync.Mutex
	pending   map[string]RunRequest
}

// New constructs a Server with routes wired up. It panics if Registry
// or StateCache are nil — those are programmer errors, not runtime
// conditions worth a fallible API.
func New(opts Options) *Server {
	if opts.Registry == nil {
		panic("server: Options.Registry is required")
	}
	if opts.StateCache == nil {
		panic("server: Options.StateCache is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.ReadHeaderTimeout <= 0 {
		opts.ReadHeaderTimeout = 5 * time.Second
	}
	if opts.ShutdownTimeout <= 0 {
		opts.ShutdownTimeout = 10 * time.Second
	}
	if opts.SSEKeepalive == 0 {
		opts.SSEKeepalive = 15 * time.Second
	}
	if opts.LaunchDrainTimeout <= 0 {
		opts.LaunchDrainTimeout = 30 * time.Second
	}

	s := &Server{opts: opts, mux: http.NewServeMux()}
	// Pre-initialise launchCtx so that handlers invoked through Handler()
	// (in tests) have a valid context even when Serve/Run never runs.
	s.launchCtx, s.launchCancel = context.WithCancel(context.Background())
	if opts.RateLimit != nil {
		ttl := opts.RateLimit.TTL
		if ttl <= 0 {
			ttl = 10 * time.Minute
		}
		s.limiter = ratelimit.New(opts.RateLimit.RequestsPerSecond, opts.RateLimit.Burst, ttl)
	}
	s.routes()
	s.handler = s.withCORS(s.withRateLimit(s.withAuth(s.mux)))
	return s
}

// withAuth wraps h with authentication. It has three branches:
//
//  1. Keyring nil && AuthToken == "" — no auth: the bare handler is returned,
//     byte-identical to the pre-auth behavior. serve() guarantees that only
//     happens on a loopback bind.
//  2. Keyring nil && AuthToken != "" — legacy single-token path: the exact
//     constant-time bearer compare as before, GET /healthz exempt, identical
//     401 + WWW-Authenticate: Bearer. The ONLY addition is that a SUCCESSFUL
//     compare injects an admin principal into the request context (capability
//     gates short-circuit allow; legacy handlers ignore it ⇒ byte-identical).
//  3. Keyring != nil — RBAC path: GET /healthz exempt, otherwise the
//     Authorization header is looked up in the keyring; a miss returns the
//     identical 401, a hit injects the resolved principal into the context.
func (s *Server) withAuth(h http.Handler) http.Handler {
	if s.opts.Keyring != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/healthz" {
				h.ServeHTTP(w, r)
				return
			}
			p, ok := s.opts.Keyring.Lookup(r.Header.Get("Authorization"))
			if !ok {
				w.Header().Set("WWW-Authenticate", `Bearer`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			h.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), p)))
		})
	}
	if s.opts.AuthToken == "" {
		return h
	}
	expected := []byte("Bearer " + s.opts.AuthToken)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			h.ServeHTTP(w, r)
			return
		}
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, expected) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), rbac.AdminPrincipal())))
	})
}

// requireCapability gates next behind the named RBAC capability. When the
// keyring is nil it short-circuits straight through (byte-identical legacy
// behavior). Otherwise it reads the principal injected by withAuth: a missing
// principal is a 401 (defense in depth — withAuth should have rejected first),
// and a principal whose resolved grant lacks the capability is a 403.
func (s *Server) requireCapability(capName string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.opts.Keyring == nil {
			next(w, r)
			return
		}
		p, ok := principalFrom(r.Context())
		if !ok {
			writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
			return
		}
		if !s.opts.Keyring.RoleSet().Resolve(p).AllowsCapability(capName) {
			writeJSON(w, http.StatusForbidden, errorResponse{Error: "forbidden"})
			return
		}
		next(w, r)
	}
}

// isLoopbackAddr reports whether a listener address is bound to loopback only.
// A non-loopback (or wildcard) bind without an AuthToken is rejected by serve().
func isLoopbackAddr(addr net.Addr) bool {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false // hostname — can't prove loopback
	}
	return ip.IsLoopback()
}

// launchAsync spawns a launcher invocation in a tracked goroutine. The
// goroutine receives the server-lifetime launchCtx, NOT the request
// context: a client disconnect should not abort an in-progress run.
func (s *Server) launchAsync(runID string, req RunRequest) {
	s.launchMu.Lock()
	ctx := s.launchCtx
	s.launchMu.Unlock()
	s.inFlight.Add(1)
	go func() {
		defer s.inFlight.Done()
		if err := s.opts.Launcher.Launch(ctx, runID, req); err != nil {
			s.opts.Logger.Error("server: launch failed",
				"error", err, "run_id", runID,
			)
		}
	}()
}

// startScheduler builds and starts the scheduler if SchedulerConfig is
// set. Idempotent. Called by serve() and by tests (Handler()-only path).
func (s *Server) startScheduler() {
	if s.opts.SchedulerConfig == nil || s.sched != nil {
		return
	}
	// pendingMu is uncontended here (runs before the listener accepts and
	// before any worker), but take it anyway so EVERY s.pending access is
	// uniformly lock-guarded — no special-case the next reader must prove.
	s.pendingMu.Lock()
	s.pending = make(map[string]RunRequest)
	s.pendingMu.Unlock()
	s.sched = scheduler.New(*s.opts.SchedulerConfig, s.launchFunc, s.opts.Bus)
	s.sched.Start()
}

// stopScheduler stops the scheduler (draining in-flight, cancelling
// queued), purges s.pending, and clears s.sched so a re-serve()
// rebuilds a fresh one. No-op when scheduler-nil.
func (s *Server) stopScheduler(ctx context.Context) error {
	if s.sched == nil {
		return nil
	}
	err := s.sched.Stop(ctx)
	// After Stop the scheduler dispatches nothing more, so launchFunc
	// will never run for runs that were still queued (Stop cancelled
	// them). Their staged RunRequests are now dead. In-flight runs'
	// entries were already removed by launchFunc's takePending BEFORE
	// Launch, so whatever remains in s.pending is exactly the
	// Stop-cancelled set (or shutdown-racing submits that will 503).
	// Purge it to honor the "pending map can't leak on Stop-drop"
	// invariant — server-side, since the scheduler must not import
	// internal/server to call back. Safe under concurrent shutdown-
	// racing handleCreateRun: its takePending on a cleared map is a
	// harmless no-op and that request gets 503.
	s.pendingMu.Lock()
	s.pending = make(map[string]RunRequest)
	s.pendingMu.Unlock()
	// Clear s.sched so a second serve() (the Server is explicitly
	// designed to be re-Run() — serve() resets launchCtx for exactly
	// that) goes through startScheduler again and rebuilds a fresh,
	// running scheduler. Without this, startScheduler's `s.sched != nil`
	// guard would leave the STOPPED scheduler installed and every
	// POST /v1/runs would 503 forever. Symmetric with the launchCtx
	// reset. Safe: awaitDrain already chose its branch from the
	// pre-call s.sched != nil; after Shutdown no new HTTP handlers run;
	// a second stopScheduler hits the s.sched == nil guard (nil no-op).
	s.sched = nil
	return err
}

// launchFunc is the work one pool slot performs: resolve the stored
// RunRequest for runID and invoke the configured Launcher under the
// server-lifetime ctx.
func (s *Server) launchFunc(_ context.Context, runID string) error {
	s.launchMu.Lock()
	ctx := s.launchCtx
	s.launchMu.Unlock()
	req, ok := s.takePending(runID)
	if !ok {
		s.opts.Logger.Error("server: no pending request for scheduled run", "run_id", runID)
		return nil
	}
	return s.opts.Launcher.Launch(ctx, runID, req)
}

func (s *Server) putPending(runID string, req RunRequest) {
	s.pendingMu.Lock()
	s.pending[runID] = req
	s.pendingMu.Unlock()
}

func (s *Server) takePending(runID string) (RunRequest, bool) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	req, ok := s.pending[runID]
	if ok {
		delete(s.pending, runID)
	}
	return req, ok
}

// Handler exposes the configured handler (auth-wrapped when a token is set)
// for testing or external composition.
func (s *Server) Handler() http.Handler { return s.handler }

// routes registers every HTTP route. Keep this function the single
// source of truth so the route table is greppable.
func (s *Server) routes() {
	// /healthz and /metrics are never capability-gated (liveness + scrape).
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.Handle("GET /metrics", s.opts.Registry.Handler())
	// Per-route RBAC capability gates. requireCapability is a no-op
	// short-circuit when Options.Keyring is nil, so these wrappers are
	// byte-identical on the legacy path.
	s.mux.HandleFunc("POST /v1/runs", s.requireCapability(rbac.CapRunCreate, s.handleCreateRun))
	s.mux.HandleFunc("GET /v1/runs", s.requireCapability(rbac.CapRunRead, s.handleListRuns))
	s.mux.HandleFunc("GET /v1/runs/{id}", s.requireCapability(rbac.CapRunRead, s.handleGetRun))
	s.mux.HandleFunc("GET /v1/runs/{id}/events", s.requireCapability(rbac.CapRunRead, s.handleRunEventsSSE))
	s.mux.HandleFunc("GET /v1/runs/{id}/audit", s.requireCapability(rbac.CapAuditRead, s.handleListRunAudit))
	s.mux.HandleFunc("DELETE /v1/runs/{id}", s.requireCapability(rbac.CapRunCancel, s.handleCancelRun))
	s.mux.HandleFunc("POST /v1/approvals/{id}", s.requireCapability(rbac.CapApprove, s.handleApproveRun))
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok\n")
}

// Run binds the server to addr and serves until ctx is cancelled or
// ListenAndServe returns a fatal error. On ctx cancellation the server
// is shut down gracefully with ShutdownTimeout.
//
// Returns nil for a clean shutdown.
func (s *Server) Run(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("server: listen %s: %w", addr, err)
	}
	return s.serve(ctx, ln)
}

// Serve runs the server on a pre-bound listener. Useful for tests that
// want to assign port 0 and read back the resolved address.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	return s.serve(ctx, ln)
}

func (s *Server) serve(ctx context.Context, ln net.Listener) error {
	// Security gate: an unauthenticated server may only listen on loopback.
	// POST /v1/runs executes arbitrary agent code, so a wildcard/public bind
	// without a token would be RCE-as-a-service. Fail fast and loud.
	if s.opts.AuthToken == "" && s.opts.Keyring == nil && !isLoopbackAddr(ln.Addr()) {
		_ = ln.Close()
		return fmt.Errorf("server: refusing to listen on non-loopback address %s without an auth token (set Options.AuthToken / --api-token or SANDCODE_API_TOKEN) or an RBAC keyring", ln.Addr())
	}

	// Each serve() call resets the launch context so a re-Run() after
	// shutdown gets a fresh, uncancelled root.
	s.launchMu.Lock()
	s.launchCtx, s.launchCancel = context.WithCancel(ctx)
	s.launchMu.Unlock()

	s.startScheduler()

	srv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: s.opts.ReadHeaderTimeout,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	errCh := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	s.opts.Logger.Info("server: listening", "addr", ln.Addr().String())

	awaitDrain := func() {
		// Give launchers a chance to finish naturally. The launch ctx
		// is already a child of the serve ctx, so it has already been
		// signalled cancellation. After LaunchDrainTimeout we cut the
		// rope: a stuck launcher will keep running, but we stop
		// blocking Run() from returning.
		s.launchMu.Lock()
		cancel := s.launchCancel
		s.launchMu.Unlock()
		if s.sched != nil {
			dctx, dcancel := context.WithTimeout(context.Background(), s.opts.LaunchDrainTimeout)
			defer dcancel()
			if err := s.stopScheduler(dctx); err != nil {
				s.opts.Logger.Warn("server: scheduler drain timeout, abandoning queued/in-flight runs", "timeout", s.opts.LaunchDrainTimeout)
				cancel()
			}
			return
		}
		// ---- existing inFlight path unchanged below ----
		done := make(chan struct{})
		go func() {
			s.inFlight.Wait()
			close(done)
		}()
		select {
		case <-done:
			return
		case <-time.After(s.opts.LaunchDrainTimeout):
			s.opts.Logger.Warn("server: drain timeout, abandoning in-flight launches",
				"timeout", s.opts.LaunchDrainTimeout,
			)
			cancel() // unblock any launcher that honors ctx
			<-done   // still wait for graceful exits we triggered
		}
	}

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.opts.ShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			s.opts.Logger.Warn("server: shutdown error", "error", err)
			<-errCh
			awaitDrain()
			return err
		}
		<-errCh
		awaitDrain()
		return nil
	case err := <-errCh:
		awaitDrain()
		return err
	}
}
