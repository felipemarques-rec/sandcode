package judge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/llmcli"
)

// Reviewer scores how well a diff fulfills a task prompt, given the verify
// result. It is observational — callers must never gate on its output.
type Reviewer interface {
	Review(ctx context.Context, req ReviewRequest) (Review, error)
}

// ReviewRequest is the flattened input a Reviewer consumes. Verify context is
// flattened into primitives because judge cannot import orchestrator types.
type ReviewRequest struct {
	RunID        string // run correlation id (carried for the caller/event; not sent to the model)
	Prompt       string // the task — the implicit definition-of-done
	Diff         string // the implementation diff to review
	VerifyRan    bool
	VerifyPassed bool
	VerifyTail   string
}

// Review is a Reviewer's verdict. Score is clamped to [0,1].
type Review struct {
	Score    float64
	Comments string
	Reviewer string // e.g. "llm:claude-haiku-4-5-20251001"
}

// LLMReviewer calls Anthropic's Messages API (forced tool use) to review a
// diff. Mirrors LLMJudge. System is the system prompt — distinct per lens
// (code review / performance / refactoring) but the tool + parse path are
// shared, so all lenses produce the same Review shape.
type LLMReviewer struct {
	APIKey  string
	Model   string
	BaseURL string
	System  string

	HTTPClient *http.Client

	// cli, when set, routes the review through the `claude` CLI (subscription
	// auth) instead of the HTTP API-key path.
	cli *llmcli.Client
}

// newLLMReviewerSub is the shared backer for the subscription lens constructors.
func newLLMReviewerSub(model, system string) *LLMReviewer {
	if model == "" {
		model = "claude-haiku-4-5-20251001"
	}
	return &LLMReviewer{Model: model, System: system, cli: llmcli.New(model)}
}

// newLLMReviewer is the shared backer for all lens constructors. model
// defaults to the fast Haiku model; system is the lens-specific prompt.
func newLLMReviewer(apiKey, model, system string) *LLMReviewer {
	if model == "" {
		model = "claude-haiku-4-5-20251001"
	}
	return &LLMReviewer{
		APIKey:     apiKey,
		Model:      model,
		BaseURL:    "https://api.anthropic.com",
		System:     system,
		HTTPClient: &http.Client{Timeout: 60 * time.Second},
	}
}

// keyFromEnv reads ANTHROPIC_API_KEY or errors when unset. Shared by the
// three *FromEnv constructors.
func keyFromEnv() (string, error) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return "", errors.New("ANTHROPIC_API_KEY required for the LLM reviewer")
	}
	return key, nil
}

// NewLLMReviewer constructs a code-review reviewer; model defaults to the fast
// Haiku model. (Public behavior unchanged — System defaults to reviewSystem.)
func NewLLMReviewer(apiKey, model string) *LLMReviewer {
	return newLLMReviewer(apiKey, model, reviewSystem)
}

// NewLLMReviewerFromEnv reads ANTHROPIC_API_KEY or errors when unset.
func NewLLMReviewerFromEnv(model string) (*LLMReviewer, error) {
	key, err := keyFromEnv()
	if err != nil {
		return nil, err
	}
	return NewLLMReviewer(key, model), nil
}

// NewLLMReviewerFromSubscription returns a code-review reviewer backed by the
// `claude` CLI (subscription auth) — no ANTHROPIC_API_KEY required.
func NewLLMReviewerFromSubscription(model string) *LLMReviewer {
	return newLLMReviewerSub(model, reviewSystem)
}

// NewPerformanceReviewer constructs a performance-lens reviewer.
func NewPerformanceReviewer(apiKey, model string) *LLMReviewer {
	return newLLMReviewer(apiKey, model, perfSystem)
}

// NewPerformanceReviewerFromEnv reads ANTHROPIC_API_KEY or errors when unset.
func NewPerformanceReviewerFromEnv(model string) (*LLMReviewer, error) {
	key, err := keyFromEnv()
	if err != nil {
		return nil, err
	}
	return NewPerformanceReviewer(key, model), nil
}

// NewPerformanceReviewerFromSubscription returns a performance-lens reviewer
// backed by the `claude` CLI (subscription auth).
func NewPerformanceReviewerFromSubscription(model string) *LLMReviewer {
	return newLLMReviewerSub(model, perfSystem)
}

// NewRefactoringReviewer constructs a refactoring-lens reviewer.
func NewRefactoringReviewer(apiKey, model string) *LLMReviewer {
	return newLLMReviewer(apiKey, model, refactorSystem)
}

// NewRefactoringReviewerFromEnv reads ANTHROPIC_API_KEY or errors when unset.
func NewRefactoringReviewerFromEnv(model string) (*LLMReviewer, error) {
	key, err := keyFromEnv()
	if err != nil {
		return nil, err
	}
	return NewRefactoringReviewer(key, model), nil
}

// NewRefactoringReviewerFromSubscription returns a refactoring-lens reviewer
// backed by the `claude` CLI (subscription auth).
func NewRefactoringReviewerFromSubscription(model string) *LLMReviewer {
	return newLLMReviewerSub(model, refactorSystem)
}

func (r *LLMReviewer) Name() string { return "llm:" + r.Model }

const reviewSystem = `You are a strict senior code reviewer.
You receive: a TASK PROMPT, the unified DIFF an agent produced to fulfill it, and the VERIFY RESULT (whether tests/lint ran and passed).

Judge how well the diff fulfills the task prompt, given the verify result.
Score in [0.0, 1.0]: 1.0 = fully fulfills the task, clean, verified; 0.0 = does not address the task or is broken.
Weigh correctness first, then completeness vs the prompt, then code quality. A failing verify caps the score low unless the diff is plainly correct and the failure is unrelated.

Return your decision via the review tool. Keep comments concise (1-4 sentences) and actionable.`

const perfSystem = `You are a senior performance engineer.
You receive: a TASK PROMPT, the unified DIFF an agent produced to fulfill it, and the VERIFY RESULT (whether tests/lint ran and passed).

Review the diff for performance concerns: algorithmic complexity, unnecessary allocations, hot-path inefficiencies, N+1 / redundant IO, and avoidable work.
Score in [0.0, 1.0]: 1.0 = performance-sound, no concerns; 0.0 = serious performance problems.

Return your decision via the review tool. Keep comments concise (1-4 sentences) and actionable.`

const refactorSystem = `You are a refactoring specialist.
You receive: a TASK PROMPT, the unified DIFF an agent produced to fulfill it, and the VERIFY RESULT (whether tests/lint ran and passed).

Review the diff for refactoring opportunities across:
- Cleanliness: duplication, weak cohesion, unclear names, dead code, oversized functions/files.
- SOLID violations: types/functions with more than one responsibility (SRP); concrete dependencies at boundaries that should be abstractions (DIP); fat interfaces forcing unused methods (ISP); type switches/flags that should be polymorphism (OCP).
- Clean Architecture: wrong dependency direction (domain importing framework/IO/transport); business logic tangled with DB/network/filesystem instead of behind a port; missing layer separation.
- 12-Factor smells: hardcoded config that should come from the environment; hidden global/mutable state that breaks statelessness.

Score in [0.0, 1.0]: 1.0 = clean, well-layered, SOLID-respecting; 0.0 = significant refactoring needed. Judge proportionally — a tiny diff need not exhibit full architecture.

Return your decision via the review tool. Keep comments concise (1-4 sentences) and actionable, naming the specific principle when relevant.`

var reviewTool = map[string]any{
	"name":        "review",
	"description": "Submit your code review.",
	"input_schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"score": map[string]any{
				"type":        "number",
				"minimum":     0.0,
				"maximum":     1.0,
				"description": "How well the diff fulfills the task, in [0,1].",
			},
			"comments": map[string]any{
				"type":        "string",
				"description": "Concise, actionable review comments.",
			},
		},
		"required": []string{"score", "comments"},
	},
}

// Review calls the LLM and returns a clamped Review. Errors on API failure or
// malformed tool input (degraded path — never panics).
func (r *LLMReviewer) Review(ctx context.Context, rr ReviewRequest) (Review, error) {
	if r.cli == nil && r.APIKey == "" {
		return Review{}, errors.New("review: empty API key")
	}
	req := apiRequest{
		Model:     r.Model,
		MaxTokens: 1024,
		System: []apiSystemBlock{
			{Type: "text", Text: r.System, CacheControl: map[string]any{"type": "ephemeral"}},
		},
		Messages: []apiMessage{
			{Role: "user", Content: buildReviewPrompt(rr)},
		},
		Tools:      []any{reviewTool},
		ToolChoice: map[string]any{"type": "tool", "name": "review"},
	}
	raw, err := callTool(ctx, r.cli, r.HTTPClient, r.BaseURL, r.APIKey, req, "review")
	if err != nil {
		return Review{}, err
	}
	var parsed struct {
		Score    float64 `json:"score"`
		Comments string  `json:"comments"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Review{}, fmt.Errorf("review: bad tool input: %w", err)
	}
	score := parsed.Score
	if score < 0 {
		score = 0
	} else if score > 1 {
		score = 1
	}
	return Review{Score: score, Comments: parsed.Comments, Reviewer: r.Name()}, nil
}

func buildReviewPrompt(rr ReviewRequest) string {
	var b strings.Builder
	b.WriteString("TASK PROMPT:\n")
	b.WriteString(rr.Prompt)
	b.WriteString("\n\nVERIFY RESULT:\n")
	switch {
	case !rr.VerifyRan:
		b.WriteString("no verifier ran\n")
	case rr.VerifyPassed:
		b.WriteString("passed\n")
	default:
		b.WriteString("FAILED\n")
		if rr.VerifyTail != "" {
			b.WriteString("verify output tail:\n")
			b.WriteString(truncate(rr.VerifyTail, 1500))
			b.WriteString("\n")
		}
	}
	b.WriteString("\nDIFF:\n")
	b.WriteString(truncate(rr.Diff, 6000))
	b.WriteString("\n\nUse the `review` tool to submit your review.")
	return b.String()
}
