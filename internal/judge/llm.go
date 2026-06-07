package judge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/llmcli"
)

// LLMJudge calls Anthropic's Messages API to rank candidates. It uses tool
// use forced output so we get structured JSON without prompt-engineering
// the model into emitting valid syntax.
//
// The system prompt is cache-marked so repeated calls within the cache TTL
// avoid re-billing the (largish) judge instructions.
type LLMJudge struct {
	APIKey  string
	Model   string
	BaseURL string

	// HTTPClient is used to call the API. Default: 60s timeout.
	HTTPClient *http.Client

	// cli, when set, routes the call through the `claude` CLI (subscription
	// auth) instead of the HTTP API-key path. Set by NewLLMJudgeFromSubscription.
	cli *llmcli.Client
}

// NewLLMJudge constructs an LLM judge with the provided API key and model.
// model defaults to claude-haiku-4-5-20251001 — fast and cheap, well-suited
// for evaluating short diffs.
func NewLLMJudge(apiKey, model string) *LLMJudge {
	if model == "" {
		model = "claude-haiku-4-5-20251001"
	}
	return &LLMJudge{
		APIKey:     apiKey,
		Model:      model,
		BaseURL:    "https://api.anthropic.com",
		HTTPClient: &http.Client{Timeout: 60 * time.Second},
	}
}

// NewLLMJudgeFromEnv looks up ANTHROPIC_API_KEY and returns a judge or error
// when the env var is unset.
func NewLLMJudgeFromEnv(model string) (*LLMJudge, error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return nil, errors.New("ANTHROPIC_API_KEY required for the LLM judge")
	}
	return NewLLMJudge(key, model), nil
}

// NewLLMJudgeFromSubscription returns a judge that ranks via the `claude` CLI
// using the host's Claude subscription — no ANTHROPIC_API_KEY required.
func NewLLMJudgeFromSubscription(model string) *LLMJudge {
	if model == "" {
		model = "claude-haiku-4-5-20251001"
	}
	return &LLMJudge{Model: model, cli: llmcli.New(model)}
}

func (j *LLMJudge) Name() string { return "llm:" + j.Model }

const judgeSystem = `You are a strict code-review judge.
You receive: a TASK PROMPT, and 2+ CANDIDATE solutions produced by different coding agents.
Each candidate includes its agent name, a unified diff, a short tail of its output, and exit metadata.

Your job: rank the candidates from best to worst against the task prompt.

Criteria (in order of importance):
1. Correctness — does the diff actually accomplish the task?
2. Minimality — smallest diff that achieves the goal wins ties.
3. Code quality — naming, style, error handling, no obvious bugs.
4. Determinism — agents that fail (non-zero exit) lose unless their diff is plainly correct.

Return your decision via the rank tool. Score every candidate in [0.0, 1.0]
with at most one 1.0. Pick the candidate with the highest score as winner.
Be concise in the rationale (1-3 sentences).`

// rank tool schema — forced via tool_choice.
var rankTool = map[string]any{
	"name":        "rank",
	"description": "Submit your ranking of the candidate solutions.",
	"input_schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"winner": map[string]any{
				"type":        "string",
				"description": "RunID of the best candidate.",
			},
			"scores": map[string]any{
				"type":        "object",
				"description": "RunID -> score in [0,1].",
				"additionalProperties": map[string]any{
					"type":    "number",
					"minimum": 0.0,
					"maximum": 1.0,
				},
			},
			"rationale": map[string]any{
				"type":        "string",
				"description": "Brief explanation (1-3 sentences).",
			},
		},
		"required": []string{"winner", "scores", "rationale"},
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

// Rank calls the LLM and returns a Ranking. Errors when the API rejects the
// call or returns malformed tool input.
func (j *LLMJudge) Rank(ctx context.Context, prompt string, cands []Candidate) (Ranking, error) {
	if len(cands) < 2 {
		return Ranking{}, errors.New("judge: need at least 2 candidates")
	}
	if j.cli == nil && j.APIKey == "" {
		return Ranking{}, errors.New("judge: empty API key")
	}

	userMsg := buildJudgePrompt(prompt, cands)
	req := apiRequest{
		Model:     j.Model,
		MaxTokens: 1024,
		System: []apiSystemBlock{
			{
				Type:         "text",
				Text:         judgeSystem,
				CacheControl: map[string]any{"type": "ephemeral"},
			},
		},
		Messages: []apiMessage{
			{Role: "user", Content: userMsg},
		},
		Tools:      []any{rankTool},
		ToolChoice: map[string]any{"type": "tool", "name": "rank"},
	}

	raw, err := callTool(ctx, j.cli, j.HTTPClient, j.BaseURL, j.APIKey, req, "rank")
	if err != nil {
		return Ranking{}, err
	}
	var parsed struct {
		Winner    string             `json:"winner"`
		Scores    map[string]float64 `json:"scores"`
		Rationale string             `json:"rationale"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Ranking{}, fmt.Errorf("judge: bad tool input: %w", err)
	}
	return Ranking{
		Winner:    parsed.Winner,
		Scores:    parsed.Scores,
		Rationale: parsed.Rationale,
		Judge:     j.Name(),
	}, nil
}

// callTool runs a forced-tool request via the subscription CLI when cli != nil,
// otherwise via the HTTP API-key path. The CLI path reconstructs the (system,
// user, schema) triple from req so callers reuse one tool definition for both
// transports. Shared by LLMJudge.Rank and LLMReviewer.Review.
func callTool(ctx context.Context, cli *llmcli.Client, client *http.Client, baseURL, apiKey string, req apiRequest, toolName string) (json.RawMessage, error) {
	if cli != nil {
		var system string
		if len(req.System) > 0 {
			system = req.System[0].Text
		}
		user, _ := req.Messages[0].Content.(string)
		var schema map[string]any
		if len(req.Tools) > 0 {
			if tm, ok := req.Tools[0].(map[string]any); ok {
				schema, _ = tm["input_schema"].(map[string]any)
			}
		}
		return cli.Structured(ctx, system, user, toolName, schema)
	}
	return forceToolCall(ctx, client, baseURL, apiKey, req, toolName)
}

// forceToolCall posts a forced single-tool request to the Anthropic Messages
// API and returns the raw JSON input of the named tool_use block. Shared by
// LLMJudge.Rank and LLMReviewer.Review.
func forceToolCall(ctx context.Context, client *http.Client, baseURL, apiKey string, req apiRequest, toolName string) (json.RawMessage, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic api: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	var apiResp apiResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, fmt.Errorf("anthropic: bad response: %w (body=%s)", err, truncate(string(respBody), 300))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("anthropic http %d: %s", resp.StatusCode, apiResp.Error.Message)
	}
	for _, c := range apiResp.Content {
		if c.Type == "tool_use" && c.Name == toolName {
			return c.Input, nil
		}
	}
	return nil, fmt.Errorf("anthropic: no %s tool_use in response", toolName)
}

func buildJudgePrompt(taskPrompt string, cands []Candidate) string {
	var b strings.Builder
	b.WriteString("TASK PROMPT:\n")
	b.WriteString(taskPrompt)
	b.WriteString("\n\nCANDIDATES:\n\n")
	for i, c := range cands {
		fmt.Fprintf(&b, "--- candidate %d (run_id=%s, agent=%s, status=%s, exit=%d, duration=%s) ---\n",
			i+1, c.RunID, c.Agent, c.Status, c.ExitCode, c.Duration.Round(time.Millisecond))
		b.WriteString("DIFF:\n")
		b.WriteString(truncate(c.Diff, 6000))
		if c.Stdout != "" {
			b.WriteString("\nSTDOUT TAIL:\n")
			b.WriteString(truncate(c.Stdout, 1500))
		}
		b.WriteString("\n\n")
	}
	b.WriteString("Use the `rank` tool to submit your ranking.")
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
