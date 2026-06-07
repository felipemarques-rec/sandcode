// Package sandbox defines the contract for sandbox runtimes (Docker, Podman, etc.)
// in which coding agents are executed in isolation from the host.
package sandbox

import (
	"context"
	"io"
)

// SandboxSpec describes how to provision a sandbox. Mutable so AuthProviders
// and policy layers can append mounts/env/limits before Create is called.
type SandboxSpec struct {
	// Image is the container image (e.g. "sandcode-default:latest").
	Image string

	// WorkDir is the path inside the container that will be the agent's cwd.
	WorkDir string

	// Mounts is the list of host->container bind mounts.
	Mounts []Mount

	// Env is environment variables to set inside the container.
	Env map[string]string

	// User is the uid:gid (or username) the container runs as. Empty = image default.
	User string

	// Network is the container network mode ("bridge", "host", "none").
	Network string

	// Limits are runtime resource constraints.
	Limits Limits

	// Labels attached to the container for housekeeping/cleanup.
	Labels map[string]string
}

// Mount is a host->container bind mount.
type Mount struct {
	Source   string // host path
	Target   string // container path
	ReadOnly bool
}

// Limits are runtime resource constraints applied at container creation.
type Limits struct {
	CPUs      string // e.g. "2" or "1.5"
	Memory    string // e.g. "4g", "512m"
	PidsLimit int    // 0 = unlimited
}

// ExecOptions tweaks behavior of a single Exec call.
type ExecOptions struct {
	// Tty allocates a pseudo-TTY for the exec session.
	Tty bool
	// Env are extra env vars on top of those baked into the spec.
	Env map[string]string
}

// ExecLine is a single line of stdout/stderr emitted by the executed command.
type ExecLine struct {
	Stream Stream
	Text   string
}

// Stream is stdout or stderr.
type Stream int

const (
	StreamStdout Stream = iota
	StreamStderr
)

// ExecResult is the terminal status of an Exec call. It is delivered as
// a sentinel value on the events channel after the last ExecLine, or via
// the dedicated Wait method depending on the provider implementation.
type ExecResult struct {
	ExitCode int
	Err      error
}

// Provider creates new sandboxes. Implementations are stateless and safe
// for concurrent use.
type Provider interface {
	Name() string
	Create(ctx context.Context, spec SandboxSpec) (Sandbox, error)
}

// Sandbox is a live, isolated execution environment.
type Sandbox interface {
	// Exec runs a command and streams its lines on the returned channel.
	// The channel is closed when the command finishes; Wait returns the
	// final exit code.
	Exec(ctx context.Context, cmd []string, stdin io.Reader, opts ExecOptions) (<-chan ExecLine, Wait, error)

	// CopyIn copies a host file/dir into the sandbox.
	CopyIn(ctx context.Context, srcHost, dstContainer string) error

	// CopyOut copies a sandbox file/dir to the host.
	CopyOut(ctx context.Context, srcContainer, dstHost string) error

	// Close terminates the sandbox and releases its resources.
	Close(ctx context.Context) error
}

// Wait blocks until the corresponding Exec finishes and returns its exit code.
type Wait func() ExecResult
