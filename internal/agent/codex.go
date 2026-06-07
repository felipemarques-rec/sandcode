package agent

import (
	"encoding/json"
	"strings"
	"time"
)

// Codex invokes OpenAI's `codex` CLI. It accepts the prompt on stdin via
// `codex exec --json -` and emits one JSON object per line we can parse.
//
// Reference: https://github.com/openai/codex
type Codex struct{}

func NewCodex() *Codex { return &Codex{} }

func (*Codex) Name() string { return "codex" }

func (*Codex) BuildCommand(opts RunOptions) Command {
	argv := []string{"codex", "exec", "--json"}
	if opts.Effort != "" {
		argv = append(argv, "--reasoning", opts.Effort)
	}
	if opts.Model != "" {
		argv = append(argv, "--model", opts.Model)
	}
	argv = append(argv, opts.ExtraArgs...)
	// Pass prompt as final positional argument; codex exec accepts it via argv.
	argv = append(argv, "-", "--", opts.Prompt)
	return Command{Argv: argv, Stdin: strings.NewReader(opts.Prompt)}
}

type codexJSONLine struct {
	Type string `json:"type"`
	Msg  struct {
		Type string          `json:"type"`
		Text string          `json:"text,omitempty"`
		Name string          `json:"name,omitempty"`
		Args json.RawMessage `json:"args,omitempty"`
	} `json:"msg,omitempty"`
}

func (*Codex) ParseLine(line string) (StreamEvent, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return StreamEvent{}, false
	}
	now := time.Now()
	if !strings.HasPrefix(line, "{") {
		return StreamEvent{Kind: EventText, Text: line, Timestamp: now}, true
	}
	var raw codexJSONLine
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return StreamEvent{Kind: EventRaw, Text: line, Timestamp: now}, true
	}
	switch raw.Msg.Type {
	case "agent_message", "assistant_message":
		return StreamEvent{Kind: EventText, Text: raw.Msg.Text, Timestamp: now}, true
	case "tool_call", "function_call":
		return StreamEvent{
			Kind:      EventToolCall,
			ToolName:  raw.Msg.Name,
			ToolInput: string(raw.Msg.Args),
			Timestamp: now,
		}, true
	}
	return StreamEvent{Kind: EventRaw, Text: line, Timestamp: now}, true
}

func (*Codex) AuthHints() AuthHints {
	// Codex authenticates via OPENAI_API_KEY (preferred) or OAuth state in
	// ~/.codex when the user ran `codex login` on the host.
	return AuthHints{
		CredentialDirs:  []string{".codex"},
		AcceptedEnvVars: []string{"OPENAI_API_KEY"},
	}
}
