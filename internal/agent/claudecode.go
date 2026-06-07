package agent

import (
	"encoding/json"
	"strings"
	"time"
)

// ClaudeCode is the Provider for Anthropic's `claude` CLI (Claude Code).
//
// Invocation contract used here:
//
//	claude --print --output-format=stream-json --verbose
//	  [--model <model>] [--resume <session-id>] [<extra args>...]
//
// The prompt is delivered on stdin so we don't have to escape it through the
// shell. stream-json yields one JSON object per line which we parse into
// StreamEvent values.
type ClaudeCode struct{}

// NewClaudeCode returns a fresh provider instance. It is stateless.
func NewClaudeCode() *ClaudeCode { return &ClaudeCode{} }

func (*ClaudeCode) Name() string { return "claude-code" }

func (*ClaudeCode) BuildCommand(opts RunOptions) Command {
	argv := []string{
		"claude",
		"--print",
		"--output-format=stream-json",
		"--verbose",
	}
	if opts.Model != "" {
		argv = append(argv, "--model", opts.Model)
	}
	if opts.SessionID != "" {
		argv = append(argv, "--resume", opts.SessionID)
	}
	argv = append(argv, opts.ExtraArgs...)
	return Command{
		Argv:  argv,
		Stdin: strings.NewReader(opts.Prompt),
	}
}

// claudeStreamLine mirrors the subset of stream-json fields we consume.
type claudeStreamLine struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype,omitempty"`
	Message struct {
		Role    string          `json:"role,omitempty"`
		Content json.RawMessage `json:"content,omitempty"`
	} `json:"message,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Result    string `json:"result,omitempty"`
}

type claudeContentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

func (*ClaudeCode) ParseLine(line string) (StreamEvent, bool) {
	line = strings.TrimSpace(line)
	if line == "" || !strings.HasPrefix(line, "{") {
		return StreamEvent{}, false
	}
	var raw claudeStreamLine
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return StreamEvent{Kind: EventRaw, Text: line, Timestamp: time.Now()}, true
	}
	now := time.Now()
	switch raw.Type {
	case "system":
		// Surface only meaningful system events. Claude emits multiple
		// system pings with the same session_id — let the consumer dedupe.
		if raw.SessionID != "" && raw.Subtype == "init" {
			return StreamEvent{Kind: EventSession, SessionID: raw.SessionID, Timestamp: now}, true
		}
		return StreamEvent{}, false
	case "assistant":
		// content is an array of blocks
		var blocks []claudeContentBlock
		if err := json.Unmarshal(raw.Message.Content, &blocks); err == nil && len(blocks) > 0 {
			b := blocks[0]
			switch b.Type {
			case "text":
				return StreamEvent{Kind: EventText, Text: b.Text, Timestamp: now}, true
			case "tool_use":
				return StreamEvent{
					Kind:      EventToolCall,
					ToolName:  b.Name,
					ToolInput: string(b.Input),
					Timestamp: now,
				}, true
			}
		}
		return StreamEvent{}, false
	case "result":
		// Final summary — surface only the human-readable result text.
		text := raw.Result
		if text == "" {
			text = "(run finished)"
		}
		return StreamEvent{Kind: EventText, Text: text, Timestamp: now}, true
	case "user", "tool_result", "rate_limit_event":
		// Tool-result echoes and rate-limit pings are not user-facing.
		return StreamEvent{}, false
	}
	// Unknown type — drop silently rather than dumping JSON to the user.
	return StreamEvent{}, false
}

func (*ClaudeCode) AuthHints() AuthHints {
	return AuthHints{
		CredentialDirs:         []string{".claude"},
		AcceptedEnvVars:        []string{"ANTHROPIC_API_KEY"},
		NeedsClaudeCredentials: true, // legacy mirror
	}
}
