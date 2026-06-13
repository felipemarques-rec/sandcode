package stepback

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

// LLMStepBack calls Anthropic's Messages API with forced tool use to distill
// reframing principles. Mirrors LLMArchitect.
type LLMStepBack struct {
	APIKey  string
	Model   string
	BaseURL string

	HTTPClient *http.Client

	// cli, when set, routes through the `claude` CLI (subscription auth)
	// instead of the HTTP API-key path.
	cli *llmcli.Client
}

// NewLLMStepBack constructs a step-back reasoner; model defaults to fast Haiku.
func NewLLMStepBack(apiKey, model string) *LLMStepBack {
	if model == "" {
		model = "claude-haiku-4-5-20251001"
	}
	return &LLMStepBack{
		APIKey:     apiKey,
		Model:      model,
		BaseURL:    "https://api.anthropic.com",
		HTTPClient: &http.Client{Timeout: 60 * time.Second},
	}
}

// NewLLMStepBackFromEnv reads ANTHROPIC_API_KEY or errors when unset.
func NewLLMStepBackFromEnv(model string) (*LLMStepBack, error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return nil, errors.New("ANTHROPIC_API_KEY required for the LLM step-back reasoner")
	}
	return NewLLMStepBack(key, model), nil
}

// NewLLMStepBackFromSubscription returns a reasoner backed by the `claude` CLI
// (subscription auth) — no ANTHROPIC_API_KEY required.
func NewLLMStepBackFromSubscription(model string) *LLMStepBack {
	if model == "" {
		model = "claude-haiku-4-5-20251001"
	}
	return &LLMStepBack{Model: model, cli: llmcli.New(model)}
}

func (s *LLMStepBack) Name() string { return "llm:" + s.Model }

const stepBackSystem = `You are reasoning about a software task by stepping back.
You receive a TASK PROMPT plus its PROBLEM TYPE and COMPLEXITY.

Before any solution, identify the 2-4 high-level PRINCIPLES, abstractions, or
analogous problem classes that reframe this task — the deeper "what is really being
asked" beneath the surface request. Do NOT write code and do NOT produce a step-by-step
plan; produce the reframing principles a strong engineer would recall first.

Each principle should be a concise, concrete sentence specific to this task (not a
generic platitude). Submit them via the step_back tool.`

var stepBackTool = map[string]any{
	"name":        "step_back",
	"description": "Submit the high-level reframing principles for this task.",
	"input_schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"principles": map[string]any{
				"type":        "array",
				"description": "2-4 high-level principles/abstractions that reframe the task.",
				"items":       map[string]any{"type": "string"},
			},
		},
		"required": []string{"principles"},
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

// Reason calls the LLM and returns a Result. Errors on empty key, API failure,
// or malformed tool input (degraded path — never panics).
func (s *LLMStepBack) Reason(ctx context.Context, rr ReasonRequest) (Result, error) {
	if s.cli == nil && s.APIKey == "" {
		return Result{}, errors.New("stepback: empty API key")
	}

	user := "TASK PROMPT:\n" + rr.Prompt +
		"\n\nPROBLEM TYPE: " + rr.ProblemType +
		"\nCOMPLEXITY: " + rr.Complexity +
		"\n\nUse the `step_back` tool to submit your principles."

	if s.cli != nil {
		schema, _ := stepBackTool["input_schema"].(map[string]any)
		raw, err := s.cli.Structured(ctx, stepBackSystem, user, "step_back", schema)
		if err != nil {
			return Result{}, err
		}
		return parseStepBack(raw, s.Name())
	}

	req := apiRequest{
		Model:     s.Model,
		MaxTokens: 1024,
		System: []apiSystemBlock{
			{Type: "text", Text: stepBackSystem, CacheControl: map[string]any{"type": "ephemeral"}},
		},
		Messages:   []apiMessage{{Role: "user", Content: user}},
		Tools:      []any{stepBackTool},
		ToolChoice: map[string]any{"type": "tool", "name": "step_back"},
	}

	body, err := json.Marshal(req)
	if err != nil {
		return Result{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", s.BaseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", s.APIKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := s.HTTPClient.Do(httpReq)
	if err != nil {
		return Result{}, fmt.Errorf("stepback api: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	var apiResp apiResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return Result{}, fmt.Errorf("stepback: bad response: %w (body=%s)", err, truncate(string(respBody), 300))
	}
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("stepback http %d: %s", resp.StatusCode, apiResp.Error.Message)
	}
	for _, c := range apiResp.Content {
		if c.Type == "tool_use" && c.Name == "step_back" {
			return parseStepBack(c.Input, s.Name())
		}
	}
	return Result{}, errors.New("stepback: no step_back tool_use in response")
}

// parseStepBack unmarshals the step_back tool input into a Result stamped with name.
func parseStepBack(raw json.RawMessage, name string) (Result, error) {
	var parsed struct {
		Principles []string `json:"principles"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Result{}, fmt.Errorf("stepback: bad tool input: %w", err)
	}
	return Result{Principles: parsed.Principles, Reasoner: name}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
