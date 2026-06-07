package secreview

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

// LLMSecurityReviewer calls Anthropic's Messages API (forced tool use) to
// review a diff for vulnerabilities and leaked secrets. Mirrors LLMArchitect.
type LLMSecurityReviewer struct {
	APIKey  string
	Model   string
	BaseURL string

	HTTPClient *http.Client

	// cli, when set, routes the review through the `claude` CLI (subscription
	// auth) instead of the HTTP API-key path.
	cli *llmcli.Client
}

// NewLLMSecurityReviewer constructs a reviewer; model defaults to the fast Haiku model.
func NewLLMSecurityReviewer(apiKey, model string) *LLMSecurityReviewer {
	if model == "" {
		model = "claude-haiku-4-5-20251001"
	}
	return &LLMSecurityReviewer{
		APIKey:     apiKey,
		Model:      model,
		BaseURL:    "https://api.anthropic.com",
		HTTPClient: &http.Client{Timeout: 60 * time.Second},
	}
}

// NewLLMSecurityReviewerFromEnv reads ANTHROPIC_API_KEY or errors when unset.
func NewLLMSecurityReviewerFromEnv(model string) (*LLMSecurityReviewer, error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return nil, errors.New("ANTHROPIC_API_KEY required for the LLM security reviewer")
	}
	return NewLLMSecurityReviewer(key, model), nil
}

// NewLLMSecurityReviewerFromSubscription returns a reviewer backed by the
// `claude` CLI (subscription auth) — no ANTHROPIC_API_KEY required.
func NewLLMSecurityReviewerFromSubscription(model string) *LLMSecurityReviewer {
	if model == "" {
		model = "claude-haiku-4-5-20251001"
	}
	return &LLMSecurityReviewer{Model: model, cli: llmcli.New(model)}
}

func (r *LLMSecurityReviewer) Name() string { return "llm:" + r.Model }

const securitySystem = `You are a senior application security reviewer.
You receive a unified DIFF. Review it for security vulnerabilities (injection, auth/authz
mistakes, unsafe deserialization, path traversal, weak crypto, etc.) and any leaked secrets
or credentials.

For each issue, report a short rule/category, a severity ("high" | "medium" | "low"), and a
concise detail. Do NOT echo secret values — describe the kind of secret instead. If the diff is
clean, return an empty findings list. Submit via the security_review tool.`

var securityTool = map[string]any{
	"name":        "security_review",
	"description": "Submit the security findings for this diff.",
	"input_schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"findings": map[string]any{
				"type":        "array",
				"description": "Security findings; empty when the diff is clean.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"rule":     map[string]any{"type": "string", "description": "Issue category."},
						"severity": map[string]any{"type": "string", "description": "high | medium | low."},
						"detail":   map[string]any{"type": "string", "description": "Concise description; never echo secret values."},
					},
					"required": []string{"rule", "severity", "detail"},
				},
			},
		},
		"required": []string{"findings"},
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

// Review calls the LLM and returns a SecReport. Errors on empty key, API
// failure, or malformed tool input (degraded path — never panics).
func (r *LLMSecurityReviewer) Review(ctx context.Context, req SecRequest) (SecReport, error) {
	if r.cli == nil && r.APIKey == "" {
		return SecReport{}, errors.New("secreview: empty API key")
	}

	user := "DIFF:\n" + truncate(req.Diff, 6000) + "\n\nUse the `security_review` tool to submit your findings."

	if r.cli != nil {
		schema, _ := securityTool["input_schema"].(map[string]any)
		raw, err := r.cli.Structured(ctx, securitySystem, user, "security_review", schema)
		if err != nil {
			return SecReport{}, err
		}
		return parseSecReport(raw, r.Name())
	}

	areq := apiRequest{
		Model:     r.Model,
		MaxTokens: 1024,
		System: []apiSystemBlock{
			{Type: "text", Text: securitySystem, CacheControl: map[string]any{"type": "ephemeral"}},
		},
		Messages: []apiMessage{
			{Role: "user", Content: user},
		},
		Tools:      []any{securityTool},
		ToolChoice: map[string]any{"type": "tool", "name": "security_review"},
	}

	body, err := json.Marshal(areq)
	if err != nil {
		return SecReport{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", r.BaseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return SecReport{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", r.APIKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := r.HTTPClient.Do(httpReq)
	if err != nil {
		return SecReport{}, fmt.Errorf("secreview api: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	var apiResp apiResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return SecReport{}, fmt.Errorf("secreview: bad response: %w (body=%s)", err, truncate(string(respBody), 300))
	}
	if resp.StatusCode != http.StatusOK {
		return SecReport{}, fmt.Errorf("secreview http %d: %s", resp.StatusCode, apiResp.Error.Message)
	}
	for _, c := range apiResp.Content {
		if c.Type == "tool_use" && c.Name == "security_review" {
			return parseSecReport(c.Input, r.Name())
		}
	}
	return SecReport{}, errors.New("secreview: no security_review tool_use in response")
}

// parseSecReport unmarshals the security_review tool input (from either
// transport) into a SecReport stamped with the reviewer name.
func parseSecReport(raw json.RawMessage, name string) (SecReport, error) {
	var parsed struct {
		Findings []SecFinding `json:"findings"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return SecReport{}, fmt.Errorf("secreview: bad tool input: %w", err)
	}
	return SecReport{Findings: parsed.Findings, Reviewer: name}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
