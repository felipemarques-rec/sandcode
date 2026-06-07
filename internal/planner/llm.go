package planner

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

// LLMPlanner calls Anthropic's Messages API to decompose a prompt into
// a TaskDAG. Like LLMJudge, it uses forced tool-use so the model emits
// structured JSON without any prompt-engineered parsing.
//
// The system prompt is cache-marked so repeated decompositions within
// the cache TTL avoid re-billing the (largish) decomposition rubric.
type LLMPlanner struct {
	APIKey  string
	Model   string
	BaseURL string

	// HTTPClient is used to call the API. Default: 60s timeout.
	HTTPClient *http.Client

	// cli, when set, routes decomposition through the `claude` CLI
	// (subscription auth) instead of the HTTP API-key path.
	cli *llmcli.Client
}

// NewLLMPlanner constructs a planner with the provided API key and
// model. model defaults to claude-haiku-4-5-20251001 — decomposition
// is a short, well-structured task and Haiku is fast and cheap for it.
func NewLLMPlanner(apiKey, model string) *LLMPlanner {
	if model == "" {
		model = "claude-haiku-4-5-20251001"
	}
	return &LLMPlanner{
		APIKey:     apiKey,
		Model:      model,
		BaseURL:    "https://api.anthropic.com",
		HTTPClient: &http.Client{Timeout: 60 * time.Second},
	}
}

// NewLLMPlannerFromEnv looks up ANTHROPIC_API_KEY and returns a planner
// or an error when the env var is unset.
func NewLLMPlannerFromEnv(model string) (*LLMPlanner, error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return nil, errors.New("ANTHROPIC_API_KEY required for the LLM planner")
	}
	return NewLLMPlanner(key, model), nil
}

// NewLLMPlannerFromSubscription returns a planner backed by the `claude` CLI
// (subscription auth) — no ANTHROPIC_API_KEY required.
func NewLLMPlannerFromSubscription(model string) *LLMPlanner {
	if model == "" {
		model = "claude-haiku-4-5-20251001"
	}
	return &LLMPlanner{Model: model, cli: llmcli.New(model)}
}

// Name is a small convenience so callers can log/audit which planner
// produced a DAG.
func (p *LLMPlanner) Name() string { return "llm:" + p.Model }

const plannerSystem = `You are a senior engineering planner.
You receive a TASK PROMPT and decompose it into the smallest useful DAG of subtasks.

Decomposition rules:

1. SIMPLE PROMPTS: if the task is a single coherent change that one engineer can do in one sitting, return ONE node named "root" with the original prompt verbatim. Do NOT invent multi-step plans for simple tasks — that adds latency and confusion.

2. MULTI-STEP PROMPTS: only decompose when there are genuinely independent or sequential pieces — e.g. "add a CRUD endpoint AND write its tests" → 2 nodes; "refactor module X then update its consumers" → 2 sequential nodes; "implement feature A in parallel with feature B" → 2 root nodes (no deps between them).

3. IDs: short, snake_case, unique within the DAG. Examples: "schema", "endpoints", "tests".

4. DEPENDENCIES: only when the downstream node truly needs the upstream node's output. Over-sequencing kills parallelism.

5. PROMPTS: each node's prompt must be self-contained — an engineer reading just that prompt should know what to do. Reference dependency outputs explicitly ("Building on the schema from the previous step...").

6. ROLES: leave empty unless the task obviously calls for a non-default role (e.g. "reviewer" for an explicit review step).

Return your plan via the decompose tool. Be conservative — when in doubt, emit a single root node.`

var decomposeTool = map[string]any{
	"name":        "decompose",
	"description": "Submit the TaskDAG for this prompt.",
	"input_schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"nodes": map[string]any{
				"type":        "array",
				"description": "List of subtask nodes. At minimum one node.",
				"minItems":    1,
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id": map[string]any{
							"type":        "string",
							"description": "Unique short ID for this node.",
						},
						"prompt": map[string]any{
							"type":        "string",
							"description": "Self-contained instruction for the executor.",
						},
						"role": map[string]any{
							"type":        "string",
							"description": "Optional agent role (implementer, reviewer, etc).",
						},
						"depends_on": map[string]any{
							"type":        "array",
							"description": "IDs of nodes that must finish before this one starts.",
							"items":       map[string]any{"type": "string"},
						},
					},
					"required": []string{"id", "prompt"},
				},
			},
		},
		"required": []string{"nodes"},
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

// Decompose calls the LLM and returns a validated TaskDAG. Errors when:
//
//   - The prompt is empty (no point calling the API).
//   - The API rejects the call.
//   - The tool input fails to unmarshal.
//   - The returned DAG fails Validate() (cycles, dangling deps, etc.).
//
// The DAG is Validate()d before return so callers downstream can rely
// on a well-formed graph without re-checking.
func (p *LLMPlanner) Decompose(ctx context.Context, prompt string) (TaskDAG, error) {
	if prompt == "" {
		return TaskDAG{}, ErrEmptyPrompt
	}
	if p.cli == nil && p.APIKey == "" {
		return TaskDAG{}, errors.New("planner: empty API key")
	}

	user := "TASK PROMPT:\n" + prompt + "\n\nUse the `decompose` tool to submit your plan."

	if p.cli != nil {
		schema, _ := decomposeTool["input_schema"].(map[string]any)
		raw, err := p.cli.Structured(ctx, plannerSystem, user, "decompose", schema)
		if err != nil {
			return TaskDAG{}, err
		}
		return parseDAG(raw)
	}

	req := apiRequest{
		Model:     p.Model,
		MaxTokens: 2048,
		System: []apiSystemBlock{
			{
				Type:         "text",
				Text:         plannerSystem,
				CacheControl: map[string]any{"type": "ephemeral"},
			},
		},
		Messages: []apiMessage{
			{Role: "user", Content: user},
		},
		Tools:      []any{decomposeTool},
		ToolChoice: map[string]any{"type": "tool", "name": "decompose"},
	}

	body, err := json.Marshal(req)
	if err != nil {
		return TaskDAG{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.BaseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return TaskDAG{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", p.APIKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	resp, err := p.HTTPClient.Do(httpReq)
	if err != nil {
		return TaskDAG{}, fmt.Errorf("planner api: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	var apiResp apiResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return TaskDAG{}, fmt.Errorf("planner: bad response: %w (body=%s)", err, truncate(string(respBody), 300))
	}
	if resp.StatusCode != http.StatusOK {
		return TaskDAG{}, fmt.Errorf("planner http %d: %s", resp.StatusCode, apiResp.Error.Message)
	}
	for _, c := range apiResp.Content {
		if c.Type == "tool_use" && c.Name == "decompose" {
			return parseDAG(c.Input)
		}
	}
	return TaskDAG{}, errors.New("planner: no decompose tool_use in response")
}

// parseDAG unmarshals the decompose tool input (from either transport) into a
// validated TaskDAG.
func parseDAG(raw json.RawMessage) (TaskDAG, error) {
	var parsed struct {
		Nodes []Node `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return TaskDAG{}, fmt.Errorf("planner: bad tool input: %w", err)
	}
	dag := TaskDAG{Nodes: parsed.Nodes}
	if err := dag.Validate(); err != nil {
		return TaskDAG{}, fmt.Errorf("planner: invalid dag from LLM: %w", err)
	}
	return dag, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
