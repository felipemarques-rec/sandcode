// Package llmcli provides a subscription-backed transport for the LLM features
// (judge, reviewer, architect, security reviewer, planner).
//
// The API-key path posts to https://api.anthropic.com/v1/messages with an
// x-api-key header and a forced tool_use call. That endpoint does NOT accept the
// OAuth tokens stored by a Claude subscription (~/.claude/.credentials.json).
//
// This transport instead shells out to the `claude` CLI in print mode, which
// already authenticates with the user's subscription and refreshes its OAuth
// token automatically. It mirrors the forced-tool contract: callers pass the
// system text, the user message, the tool name, and the tool's input schema, and
// Structured returns the raw JSON object the model produced — the same shape the
// API path returns from a tool_use block.
//
// It follows the shell-out pattern already used by internal/agent/claudecode.go.
package llmcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// DefaultBin is the CLI invoked when Client.Bin is empty.
const DefaultBin = "claude"

// DefaultTimeout bounds a single structured call.
const DefaultTimeout = 120 * time.Second

// Client invokes the `claude` CLI to obtain structured JSON output using the
// host's Claude subscription. The zero value is usable after New; Bin/Timeout
// fall back to the package defaults when empty.
type Client struct {
	// Bin is the claude executable name or path. Default: "claude" (resolved
	// via PATH).
	Bin string
	// Model is passed through with --model. Empty means the CLI's default.
	Model string
	// Timeout bounds each Structured call. Default: 120s.
	Timeout time.Duration
}

// New returns a Client for the given model using the default binary and timeout.
func New(model string) *Client {
	return &Client{Bin: DefaultBin, Model: model, Timeout: DefaultTimeout}
}

// cliResult is the envelope emitted by `claude --print --output-format json`.
type cliResult struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
}

// Structured asks the subscription-backed CLI to emit a single JSON object that
// matches schema, and returns it as raw JSON. toolName and schema describe the
// desired output — they mirror the forced tool_use contract of the API path so
// callers can reuse their existing tool definitions. The returned bytes are a
// JSON object suitable for json.Unmarshal into the caller's result struct.
func (c *Client) Structured(ctx context.Context, system, user, toolName string, schema map[string]any) (json.RawMessage, error) {
	prompt, err := buildPrompt(system, user, toolName, schema)
	if err != nil {
		return nil, err
	}

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	bin := c.Bin
	if bin == "" {
		bin = DefaultBin
	}
	args := []string{"--print", "--output-format", "json"}
	if c.Model != "" {
		args = append(args, "--model", c.Model)
	}

	cmd := exec.CommandContext(cctx, bin, args...)
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if cctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("llmcli: %s timed out after %s", bin, timeout)
		}
		return nil, fmt.Errorf("llmcli: %s failed: %w (stderr=%s)", bin, err, truncate(stderr.String(), 300))
	}

	var env cliResult
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		return nil, fmt.Errorf("llmcli: bad CLI envelope: %w (out=%s)", err, truncate(stdout.String(), 300))
	}
	if env.IsError || (env.Type == "result" && env.Subtype != "" && env.Subtype != "success") {
		return nil, fmt.Errorf("llmcli: CLI reported error (subtype=%s): %s", env.Subtype, truncate(env.Result, 300))
	}

	obj, err := extractJSONObject(env.Result)
	if err != nil {
		return nil, fmt.Errorf("llmcli: no JSON object in result: %w (result=%s)", err, truncate(env.Result, 300))
	}
	return obj, nil
}

// buildPrompt assembles a single text prompt that carries the system
// instructions, the JSON-only directive (with the schema), and the user message.
// The CLI has no separate system channel in print mode, so everything is folded
// into one prompt.
func buildPrompt(system, user, toolName string, schema map[string]any) (string, error) {
	var b strings.Builder
	if system != "" {
		b.WriteString(system)
		b.WriteString("\n\n")
	}
	b.WriteString("Respond with ONLY a single JSON object and nothing else — no prose, ")
	b.WriteString("no explanation, no markdown code fences. ")
	if toolName != "" {
		fmt.Fprintf(&b, "The object is the input for the %q operation ", toolName)
	}
	b.WriteString("and must conform to this JSON schema:\n")
	if schema != nil {
		sb, err := json.Marshal(schema)
		if err != nil {
			return "", fmt.Errorf("llmcli: marshal schema: %w", err)
		}
		b.Write(sb)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(user)
	return b.String(), nil
}

// extractJSONObject returns the first complete JSON object embedded in s. It
// tolerates surrounding prose and markdown fences: it first tries to parse s
// whole, then falls back to the substring between the first balanced { … }.
func extractJSONObject(s string) (json.RawMessage, error) {
	trimmed := strings.TrimSpace(stripFences(s))
	if json.Valid([]byte(trimmed)) && strings.HasPrefix(trimmed, "{") {
		return json.RawMessage(trimmed), nil
	}
	// Scan for the first balanced object, ignoring braces inside strings.
	start := strings.IndexByte(trimmed, '{')
	if start < 0 {
		return nil, errors.New("no '{' found")
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(trimmed); i++ {
		ch := trimmed[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case ch == '\\':
				esc = true
			case ch == '"':
				inStr = false
			}
			continue
		}
		switch ch {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				candidate := trimmed[start : i+1]
				if json.Valid([]byte(candidate)) {
					return json.RawMessage(candidate), nil
				}
				return nil, errors.New("braced substring is not valid JSON")
			}
		}
	}
	return nil, errors.New("unbalanced JSON object")
}

// stripFences removes a leading/trailing markdown code fence (```json … ```).
func stripFences(s string) string {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "```") {
		return s
	}
	// drop the opening fence line
	if nl := strings.IndexByte(t, '\n'); nl >= 0 {
		t = t[nl+1:]
	}
	if i := strings.LastIndex(t, "```"); i >= 0 {
		t = t[:i]
	}
	return t
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
