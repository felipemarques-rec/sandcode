package auth

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/felipemarques-rec/sandcode/internal/agent"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
)

// BindMount injects each credential directory declared by the agent into the
// sandbox as a read-only mount, so the containerized agent uses the host
// user's existing logins (Claude Code subscription, Cursor OAuth, codex
// session, etc.).
//
// This is the default auth mode: it works offline-equivalent (no API key)
// and reuses Pro/Max subscriptions. Running many parallel sessions sharing
// one OAuth token can hit rate limits — the CLI surfaces a warning when the
// caller spins up >2 same-agent sub-runs with bindmount auth.
type BindMount struct {
	// HostHome is the host home directory used to resolve credential paths.
	// Default $HOME of the current process.
	HostHome string

	// ContainerHome is the in-container home where the agent expects to find
	// its credential directories. Default "/root" — most images run as root.
	ContainerHome string

	// HostClaudeDir is kept for backwards compatibility with code that
	// constructed BindMount{HostClaudeDir: "/custom/.claude"}. When set, it
	// overrides the host-side path used for Claude Code's credentials.
	HostClaudeDir string
}

// NewBindMount returns a BindMount configured with sensible defaults.
func NewBindMount() *BindMount {
	home, _ := os.UserHomeDir()
	return &BindMount{
		HostHome:      home,
		ContainerHome: "/root",
	}
}

func (*BindMount) Name() string { return "bindmount" }

func (b *BindMount) Apply(spec *sandbox.SandboxSpec, hints agent.AuthHints) error {
	dirs := hints.CredentialDirs
	// Honor the legacy NeedsClaudeCredentials flag for older callers.
	if hints.NeedsClaudeCredentials {
		hasClaude := false
		for _, d := range dirs {
			if d == ".claude" {
				hasClaude = true
			}
		}
		if !hasClaude {
			dirs = append(dirs, ".claude")
		}
	}
	if len(dirs) == 0 {
		return nil
	}
	for _, d := range dirs {
		host := b.resolveHost(d)
		if _, err := os.Stat(host); err != nil {
			return fmt.Errorf("auth(bindmount): %s not found — sign in with the agent's CLI on the host first: %w", host, err)
		}
		spec.Mounts = append(spec.Mounts, sandbox.Mount{
			Source:   host,
			Target:   filepath.Join(b.ContainerHome, d),
			ReadOnly: true,
		})
	}
	return nil
}

// resolveHost picks the host path for a credential dir. The legacy
// HostClaudeDir override only applies to ".claude" so it doesn't accidentally
// reroute other agents' credentials.
func (b *BindMount) resolveHost(dir string) string {
	if dir == ".claude" && b.HostClaudeDir != "" {
		return b.HostClaudeDir
	}
	home := b.HostHome
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	return filepath.Join(home, dir)
}
