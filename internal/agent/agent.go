// Package agent defines coding-agent providers (Claude Code, Codex, Cursor).
// An AgentProvider knows how to invoke its binary inside a sandbox and how to
// parse its stdout into structured stream events.
package agent

import (
	"io"
	"time"
)

// RunOptions configures a single agent invocation.
type RunOptions struct {
	// Prompt is the user message sent to the agent.
	Prompt string

	// WorkDir is the agent's cwd inside the sandbox.
	WorkDir string

	// Model overrides the agent's default model when supported (e.g. claude-haiku-4-5).
	Model string

	// Effort/reasoning level when supported (e.g. "low"|"medium"|"high"|"xhigh").
	Effort string

	// SessionID resumes a previous session when supported.
	SessionID string

	// ExtraArgs are appended verbatim to the agent command line.
	ExtraArgs []string
}

// Command describes how to launch the agent inside the sandbox.
type Command struct {
	Argv  []string
	Stdin io.Reader
	Env   map[string]string
}

// AuthHints declare what auth materials the agent needs at runtime.
// The orchestrator consults the configured auth.Provider to satisfy them
// (e.g. bind-mount ~/.claude or inject ANTHROPIC_API_KEY).
type AuthHints struct {
	// CredentialDirs is a list of directory names (relative to $HOME) the
	// agent expects to find at runtime. e.g. [".claude"] for Claude Code,
	// [".cursor"] for Cursor. The bind-mount auth provider mounts each one
	// read-only into the container's home directory.
	CredentialDirs []string

	// AcceptedEnvVars lists env vars that satisfy auth (any one is enough).
	AcceptedEnvVars []string

	// NeedsClaudeCredentials is a legacy hint kept for backwards
	// compatibility with auth.BindMount's older API. New agents should use
	// CredentialDirs instead.
	//
	// Deprecated: set CredentialDirs = []string{".claude"}.
	NeedsClaudeCredentials bool
}

// StreamEvent is a structured event parsed from the agent's stdout.
type StreamEvent struct {
	Kind      EventKind
	Timestamp time.Time

	// Text is the free-form payload for Text/Warning kinds.
	Text string

	// ToolName / ToolInput populated for ToolCall events.
	ToolName  string
	ToolInput string

	// SessionID populated for Session events.
	SessionID string
}

// EventKind enumerates the structured event types we surface.
type EventKind int

const (
	EventText EventKind = iota
	EventToolCall
	EventWarning
	EventSession
	EventRaw // unparsed / unknown line, surfaced verbatim
)

func (k EventKind) String() string {
	switch k {
	case EventText:
		return "text"
	case EventToolCall:
		return "tool_call"
	case EventWarning:
		return "warning"
	case EventSession:
		return "session"
	default:
		return "raw"
	}
}

// Provider is the contract every coding agent implements.
type Provider interface {
	// Name is the canonical short name (e.g. "claude-code", "codex", "cursor").
	Name() string

	// BuildCommand produces the argv/stdin/env to invoke the agent for a single run.
	BuildCommand(opts RunOptions) Command

	// ParseLine converts one stdout/stderr line into a StreamEvent. Returns ok=false
	// when the line should be discarded (kept-alive, blank, etc.).
	ParseLine(line string) (StreamEvent, bool)

	// AuthHints declare what the agent needs to authenticate inside the sandbox.
	AuthHints() AuthHints
}
