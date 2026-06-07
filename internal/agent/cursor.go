package agent

import (
	"strings"
	"time"
)

// Cursor invokes the `cursor-agent` CLI. The CLI is text-streaming by
// default — we surface lines verbatim and rely on the user-visible output.
type Cursor struct{}

func NewCursor() *Cursor { return &Cursor{} }

func (*Cursor) Name() string { return "cursor" }

func (*Cursor) BuildCommand(opts RunOptions) Command {
	argv := []string{"cursor-agent"}
	if opts.Model != "" {
		argv = append(argv, "--model", opts.Model)
	}
	argv = append(argv, opts.ExtraArgs...)
	// cursor-agent reads the prompt from stdin when no positional args are given.
	return Command{Argv: argv, Stdin: strings.NewReader(opts.Prompt)}
}

func (*Cursor) ParseLine(line string) (StreamEvent, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return StreamEvent{}, false
	}
	return StreamEvent{Kind: EventText, Text: line, Timestamp: time.Now()}, true
}

func (*Cursor) AuthHints() AuthHints {
	// cursor-agent stores OAuth state in ~/.cursor/. The bind-mount auth
	// provider lifts that into the sandbox so the agent can resume the
	// host user's session without an API key.
	return AuthHints{
		CredentialDirs:  []string{".cursor"},
		AcceptedEnvVars: []string{"CURSOR_API_KEY"},
	}
}
