package stepback

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// stepBackServer echoes a forced step_back tool_use with the given body.
func stepBackServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

func TestLLMStepBack_SendsForcedToolRequest(t *testing.T) {
	var gotMethod, gotPath, gotKey, gotToolChoice string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotKey = r.Method, r.URL.Path, r.Header.Get("x-api-key")
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			ToolChoice struct {
				Name string `json:"name"`
			} `json:"tool_choice"`
		}
		_ = json.Unmarshal(raw, &req)
		gotToolChoice = req.ToolChoice.Name
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"stop_reason":"tool_use","content":[{"type":"tool_use","name":"step_back","input":{"principles":["p1"]}}]}`))
	}))
	defer srv.Close()
	s := NewLLMStepBack("secret-key", "x")
	s.BaseURL = srv.URL
	if _, err := s.Reason(context.Background(), ReasonRequest{Prompt: "p"}); err != nil {
		t.Fatalf("Reason: %v", err)
	}
	if gotMethod != "POST" || gotPath != "/v1/messages" {
		t.Fatalf("request line: %s %s", gotMethod, gotPath)
	}
	if gotKey != "secret-key" {
		t.Fatalf("x-api-key: %q", gotKey)
	}
	if gotToolChoice != "step_back" {
		t.Fatalf("tool_choice.name: %q", gotToolChoice)
	}
}

func TestLLMStepBack_HappyPath(t *testing.T) {
	srv := stepBackServer(t, `{"stop_reason":"tool_use","content":[{"type":"tool_use","name":"step_back","input":{"principles":["separate policy from mechanism","idempotent retries"]}}]}`)
	defer srv.Close()
	s := NewLLMStepBack("k", "x")
	s.BaseURL = srv.URL
	res, err := s.Reason(context.Background(), ReasonRequest{Prompt: "add retries", ProblemType: "convergent", Complexity: "high"})
	if err != nil {
		t.Fatalf("Reason: %v", err)
	}
	if len(res.Principles) != 2 || res.Principles[0] != "separate policy from mechanism" {
		t.Fatalf("principles: %+v", res.Principles)
	}
	if res.Reasoner != "llm:x" {
		t.Fatalf("reasoner: %q", res.Reasoner)
	}
}

func TestLLMStepBack_EmptyKeyErrors(t *testing.T) {
	s := &LLMStepBack{Model: "x"} // no key, no cli
	if _, err := s.Reason(context.Background(), ReasonRequest{Prompt: "p"}); err == nil {
		t.Fatal("expected error on empty key")
	}
}

func TestParseStepBack_Malformed(t *testing.T) {
	if _, err := parseStepBack([]byte(`not json`), "llm:x"); err == nil {
		t.Fatal("expected parse error")
	}
}
