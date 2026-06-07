package architect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/llmcli"
)

// LLMArchitect calls Anthropic's Messages API with forced tool use to design
// solution structure. Mirrors LLMPlanner.
type LLMArchitect struct {
	APIKey  string
	Model   string
	BaseURL string

	// HTTPClient is used to call the API. Default: 60s timeout.
	HTTPClient *http.Client

	// cli, when set, routes the design through the `claude` CLI (subscription
	// auth) instead of the HTTP API-key path.
	cli *llmcli.Client
}

// NewLLMArchitect constructs an architect; model defaults to the fast Haiku model.
func NewLLMArchitect(apiKey, model string) *LLMArchitect {
	if model == "" {
		model = "claude-haiku-4-5-20251001"
	}
	return &LLMArchitect{
		APIKey:     apiKey,
		Model:      model,
		BaseURL:    "https://api.anthropic.com",
		HTTPClient: &http.Client{Timeout: 60 * time.Second},
	}
}

// NewLLMArchitectFromEnv reads ANTHROPIC_API_KEY or errors when unset.
func NewLLMArchitectFromEnv(model string) (*LLMArchitect, error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return nil, errors.New("ANTHROPIC_API_KEY required for the LLM architect")
	}
	return NewLLMArchitect(key, model), nil
}

// NewLLMArchitectFromSubscription returns an architect backed by the `claude`
// CLI (subscription auth) — no ANTHROPIC_API_KEY required.
func NewLLMArchitectFromSubscription(model string) *LLMArchitect {
	if model == "" {
		model = "claude-haiku-4-5-20251001"
	}
	return &LLMArchitect{Model: model, cli: llmcli.New(model)}
}

func (a *LLMArchitect) Name() string { return "llm:" + a.Model }

const architectSystem = `You are a senior software architect.
You receive a TASK PROMPT plus its PROBLEM TYPE and COMPLEXITY.

Design the solution structure — do NOT write code. Produce:
1. APPROACH: a concise (2-5 sentence) description of how to structure the solution.
2. FILES: the files most likely to be created or modified.
3. RISKS: the main risks, pitfalls, or edge cases the implementer must watch.

Apply these principles in the design (only where they genuinely fit the task — do not over-engineer small tasks):
- Clean Architecture / dependency rule: dependencies point inward; domain/business logic stays independent of frameworks, IO, and transport; isolate side effects (DB, network, filesystem) behind ports/interfaces (hexagonal).
- SOLID: single responsibility per unit; depend on abstractions at boundaries (DIP); small, focused interfaces (ISP); prefer extension over modification (OCP).
- 12-Factor (when relevant): config via environment, not code; stateless processes; backing services as attached resources; logs as event streams; dev/prod parity.

Reflect these in APPROACH (name the layers/seams) and RISKS (call out likely violations: leaking IO into the domain, god-objects, hardcoded config). Be concrete and specific to the task. Submit via the design tool.`

var designTool = map[string]any{
	"name":        "design",
	"description": "Submit the solution design for this task.",
	"input_schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"approach": map[string]any{
				"type":        "string",
				"description": "Concise description of the solution structure/approach.",
			},
			"files": map[string]any{
				"type":        "array",
				"description": "Files likely to be created or modified.",
				"items":       map[string]any{"type": "string"},
			},
			"risks": map[string]any{
				"type":        "array",
				"description": "Key risks/pitfalls to watch.",
				"items":       map[string]any{"type": "string"},
			},
		},
		"required": []string{"approach"},
	},
}

type apiMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type apiSystemBlock struct {
	Type         string         `json:"type"`
	Text         string         `json:"text"`
	CacheControl map[string]any `json:"cache_control,omitempty"`
}

type apiRequest struct {
	Model      string           `json:"model"`
	MaxTokens  int              `json:"max_tokens"`
	System     []apiSystemBlock `json:"system"`
	Messages   []apiMessage     `json:"messages"`
	Tools      []any            `json:"tools"`
	ToolChoice map[string]any   `json:"tool_choice"`
}

type apiResponse struct {
	Content []struct {
		Type  string          `json:"type"`
		Name  string          `json:"name,omitempty"`
		Input json.RawMessage `json:"input,omitempty"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Error      struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Design calls the LLM and returns an ArchPlan. Errors on empty key, API
// failure, or malformed tool input (degraded path — never panics).
func (a *LLMArchitect) Design(ctx context.Context, rr DesignRequest) (ArchPlan, error) {
	if a.cli == nil && a.APIKey == "" {
		return ArchPlan{}, errors.New("architect: empty API key")
	}

	user := "TASK PROMPT:\n" + rr.Prompt +
		"\n\nPROBLEM TYPE: " + rr.ProblemType +
		"\nCOMPLEXITY: " + rr.Complexity +
		"\n\nUse the `design` tool to submit your design."

	if a.cli != nil {
		schema, _ := designTool["input_schema"].(map[string]any)
		raw, err := a.cli.Structured(ctx, architectSystem, user, "design", schema)
		if err != nil {
			return ArchPlan{}, err
		}
		return parseArchPlan(raw, a.Name())
	}

	req := apiRequest{
		Model:     a.Model,
		MaxTokens: 1024,
		System: []apiSystemBlock{
			{Type: "text", Text: architectSystem, CacheControl: map[string]any{"type": "ephemeral"}},
		},
		Messages: []apiMessage{
			{Role: "user", Content: user},
		},
		Tools:      []any{designTool},
		ToolChoice: map[string]any{"type": "tool", "name": "design"},
	}

	body, err := json.Marshal(req)
	if err != nil {
		return ArchPlan{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", a.BaseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return ArchPlan{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", a.APIKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := a.HTTPClient.Do(httpReq)
	if err != nil {
		return ArchPlan{}, fmt.Errorf("architect api: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	var apiResp apiResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return ArchPlan{}, fmt.Errorf("architect: bad response: %w (body=%s)", err, truncate(string(respBody), 300))
	}
	if resp.StatusCode != http.StatusOK {
		return ArchPlan{}, fmt.Errorf("architect http %d: %s", resp.StatusCode, apiResp.Error.Message)
	}
	for _, c := range apiResp.Content {
		if c.Type == "tool_use" && c.Name == "design" {
			return parseArchPlan(c.Input, a.Name())
		}
	}
	return ArchPlan{}, errors.New("architect: no design tool_use in response")
}

// parseArchPlan unmarshals the design tool input (from either transport) into an
// ArchPlan stamped with the architect name.
func parseArchPlan(raw json.RawMessage, name string) (ArchPlan, error) {
	var parsed struct {
		Approach string   `json:"approach"`
		Files    []string `json:"files"`
		Risks    []string `json:"risks"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ArchPlan{}, fmt.Errorf("architect: bad tool input: %w", err)
	}
	return ArchPlan{
		Approach:  parsed.Approach,
		Files:     parsed.Files,
		Risks:     parsed.Risks,
		Architect: name,
	}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
