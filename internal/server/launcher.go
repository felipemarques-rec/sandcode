package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/felipemarques-rec/sandcode/internal/rbac"
)

// RunRequest is the JSON body of POST /v1/runs.
//
// Only fields that make sense over HTTP are exposed. Sandbox/agent/auth
// provider selection, governance, budget, brain wiring and the like
// stay on the server-side Launcher: callers configure the launcher
// once at start-up; per-request payload is intentionally small.
type RunRequest struct {
	// Prompt sent to the agent. Required.
	Prompt string `json:"prompt"`

	// CWD is the absolute path of the host repository to run against.
	// Required.
	CWD string `json:"cwd"`

	// SandboxImage is the container image the sandbox provider will
	// launch. Required.
	SandboxImage string `json:"sandbox_image"`

	// SandboxWorkDir is the path inside the sandbox where the worktree
	// is mounted. Optional — defaults to /workspace.
	SandboxWorkDir string `json:"sandbox_work_dir,omitempty"`

	// Strategy selects the git worktree handling mode. Optional —
	// defaults to "merge-to-head".
	Strategy string `json:"strategy,omitempty"`

	// KeepWorktree skips the worktree cleanup step. Optional.
	KeepWorktree bool `json:"keep_worktree,omitempty"`

	// TimeoutSeconds caps total wall-clock time of the run. Optional —
	// 0 means no cap.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`

	// Network is the sandbox network mode. Optional.
	Network string `json:"network,omitempty"`

	// Priority is an optional scheduler hint: ""|low|normal|high|critical.
	// Empty => normal. Ignored entirely when the scheduler is disabled.
	// Additive — pre-existing clients omitting it are unaffected.
	Priority string `json:"priority,omitempty"`

	// Principal is the authenticated identity that created the run, set
	// server-side from the request context (NEVER decoded from the body — the
	// json:"-" tag guarantees a client cannot inject roles). Zero value ⇒ no
	// principal (legacy/no-auth) ⇒ empty roles ⇒ byte-identical.
	Principal rbac.Principal `json:"-"`
}

// Validate returns the first reason the request would be rejected,
// or nil if it can be launched.
func (r RunRequest) Validate() error {
	if strings.TrimSpace(r.Prompt) == "" {
		return errors.New("prompt: required")
	}
	if strings.TrimSpace(r.CWD) == "" {
		return errors.New("cwd: required")
	}
	if strings.TrimSpace(r.SandboxImage) == "" {
		return errors.New("sandbox_image: required")
	}
	if r.TimeoutSeconds < 0 {
		return errors.New("timeout_seconds: must be >= 0")
	}
	return nil
}

// pathWithinAny returns nil when p resolves to a path equal to or below one of
// roots (symlinks resolved where the paths exist). Used to constrain a
// client-supplied CWD to an allowlisted host subtree, preventing traversal to
// arbitrary host directories.
func pathWithinAny(p string, roots []string) error {
	abs := resolvePath(p)
	for _, root := range roots {
		rabs := resolvePath(root)
		if abs == rabs || strings.HasPrefix(abs, rabs+string(os.PathSeparator)) {
			return nil
		}
	}
	return fmt.Errorf("cwd %q is outside the allowed root(s)", p)
}

func resolvePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return filepath.Clean(abs)
}

// checkRunPolicy enforces server-side policy on an HTTP-supplied RunRequest
// beyond structural Validate(): CWD containment and network-mode allowlist.
func (s *Server) checkRunPolicy(req RunRequest) error {
	if len(s.opts.AllowedCWDRoots) > 0 {
		if err := pathWithinAny(req.CWD, s.opts.AllowedCWDRoots); err != nil {
			return err
		}
	}
	switch req.Network {
	case "", "bridge", "none":
	default:
		return fmt.Errorf("network %q not allowed over the API (use bridge or none)", req.Network)
	}
	return nil
}

// Launcher is the server's hand-off into actual run execution. The
// HTTP handler is responsible for validation, ID allocation, and the
// 202 response; the Launcher is responsible for everything that
// happens after — sandbox setup, agent invocation, persistence.
//
// Launch is called from a goroutine the handler owns; it MUST return
// quickly with a setup error if one is detectable up front, or block
// for the run lifetime (the goroutine is fire-and-forget from the
// handler's perspective).
//
// Returning a non-nil error indicates the launch could not begin.
// Once the run is in progress, lifecycle is observed via the bus and
// the state cache — Launch's return value no longer matters.
type Launcher interface {
	Launch(ctx context.Context, runID string, req RunRequest) error
}

// LauncherFunc adapts a plain function to the Launcher interface,
// mirroring http.HandlerFunc.
type LauncherFunc func(ctx context.Context, runID string, req RunRequest) error

// Launch implements Launcher.
func (f LauncherFunc) Launch(ctx context.Context, runID string, req RunRequest) error {
	return f(ctx, runID, req)
}
