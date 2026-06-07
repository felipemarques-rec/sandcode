package judge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestLLMJudgeHappyPath verifies the request body shape and tool-input parsing
// using a stub Anthropic API. We do NOT call the real API — that lives in the
// integration smoke test invoked separately.
func TestLLMJudgeHappyPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "test-key" {
			t.Errorf("missing api key header")
		}
		// Echo a fake tool_use response.
		resp := map[string]any{
			"stop_reason": "tool_use",
			"content": []any{
				map[string]any{
					"type": "tool_use",
					"name": "rank",
					"input": map[string]any{
						"winner": "abc",
						"scores": map[string]float64{
							"abc": 0.9,
							"def": 0.4,
						},
						"rationale": "abc has a smaller, correct diff",
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	j := NewLLMJudge("test-key", "claude-haiku-4-5-20251001")
	j.BaseURL = server.URL

	rk, err := j.Rank(context.Background(), "do thing", []Candidate{
		{RunID: "abc", Agent: "claude", Diff: "+x", Status: "success", Duration: time.Second},
		{RunID: "def", Agent: "codex", Diff: "+y", Status: "success", Duration: 2 * time.Second},
	})
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}
	if rk.Winner != "abc" {
		t.Fatalf("winner=%q", rk.Winner)
	}
	if rk.Scores["abc"] != 0.9 || rk.Scores["def"] != 0.4 {
		t.Fatalf("scores=%v", rk.Scores)
	}
	if rk.Judge == "" || rk.Rationale == "" {
		t.Fatalf("missing meta: %+v", rk)
	}
}

func TestLLMJudgeNeedsCandidates(t *testing.T) {
	j := NewLLMJudge("k", "")
	if _, err := j.Rank(context.Background(), "x", []Candidate{{RunID: "a"}}); err == nil {
		t.Fatal("expected error with <2 candidates")
	}
}

func TestLLMJudgeAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"bad model"}}`))
	}))
	defer server.Close()
	j := NewLLMJudge("k", "x")
	j.BaseURL = server.URL
	_, err := j.Rank(context.Background(), "p", []Candidate{
		{RunID: "a", Status: "success"},
		{RunID: "b", Status: "success"},
	})
	if err == nil {
		t.Fatal("expected api error")
	}
}
