package architect

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// designServer echoes a forced design tool_use with the given fields.
func designServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

// TestLLMArchitect_SendsForcedToolRequest asserts the outgoing request contract:
// POST /v1/messages, x-api-key header, and a forced tool_choice of "design".
func TestLLMArchitect_SendsForcedToolRequest(t *testing.T) {
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
		_, _ = w.Write([]byte(`{"stop_reason":"tool_use","content":[{"type":"tool_use","name":"design","input":{"approach":"x"}}]}`))
	}))
	defer srv.Close()
	a := NewLLMArchitect("secret-key", "x")
	a.BaseURL = srv.URL
	if _, err := a.Design(context.Background(), DesignRequest{Prompt: "p"}); err != nil {
		t.Fatalf("Design: %v", err)
	}
	if gotMethod != "POST" || gotPath != "/v1/messages" {
		t.Fatalf("request line: %s %s", gotMethod, gotPath)
	}
	if gotKey != "secret-key" {
		t.Fatalf("x-api-key: %q", gotKey)
	}
	if gotToolChoice != "design" {
		t.Fatalf("tool_choice.name: %q", gotToolChoice)
	}
}

// TestLLMArchitect_OptionalArraysAbsent confirms files/risks are optional:
// absent arrays unmarshal to nil with no error (schema requires only approach).
func TestLLMArchitect_OptionalArraysAbsent(t *testing.T) {
	srv := designServer(t, `{"stop_reason":"tool_use","content":[{"type":"tool_use","name":"design","input":{"approach":"just the approach"}}]}`)
	defer srv.Close()
	a := NewLLMArchitect("k", "x")
	a.BaseURL = srv.URL
	ap, err := a.Design(context.Background(), DesignRequest{Prompt: "p"})
	if err != nil {
		t.Fatalf("Design: %v", err)
	}
	if ap.Approach != "just the approach" {
		t.Fatalf("approach: %q", ap.Approach)
	}
	if ap.Files != nil || ap.Risks != nil {
		t.Fatalf("absent arrays must be nil: files=%v risks=%v", ap.Files, ap.Risks)
	}
}

func TestLLMArchitect_HappyPath(t *testing.T) {
	srv := designServer(t, `{"stop_reason":"tool_use","content":[{"type":"tool_use","name":"design","input":{"approach":"layered cache","files":["cache.go","cache_test.go"],"risks":["stale reads"]}}]}`)
	defer srv.Close()
	a := NewLLMArchitect("k", "x")
	a.BaseURL = srv.URL
	ap, err := a.Design(context.Background(), DesignRequest{Prompt: "add cache", ProblemType: "convergent", Complexity: "high"})
	if err != nil {
		t.Fatalf("Design: %v", err)
	}
	if ap.Approach != "layered cache" {
		t.Fatalf("approach: %q", ap.Approach)
	}
	if len(ap.Files) != 2 || ap.Files[0] != "cache.go" {
		t.Fatalf("files: %v", ap.Files)
	}
	if len(ap.Risks) != 1 || ap.Risks[0] != "stale reads" {
		t.Fatalf("risks: %v", ap.Risks)
	}
	if ap.Architect != "llm:x" {
		t.Fatalf("architect name: %q", ap.Architect)
	}
}

func TestLLMArchitect_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"bad model"}}`))
	}))
	defer srv.Close()
	a := NewLLMArchitect("k", "x")
	a.BaseURL = srv.URL
	if _, err := a.Design(context.Background(), DesignRequest{Prompt: "p"}); err == nil {
		t.Fatal("expected api error")
	}
}

func TestLLMArchitect_NoToolUse(t *testing.T) {
	srv := designServer(t, `{"stop_reason":"end_turn","content":[{"type":"text"}]}`)
	defer srv.Close()
	a := NewLLMArchitect("k", "x")
	a.BaseURL = srv.URL
	if _, err := a.Design(context.Background(), DesignRequest{Prompt: "p"}); err == nil {
		t.Fatal("expected missing-tool error")
	}
}

func TestLLMArchitect_EmptyKey(t *testing.T) {
	a := &LLMArchitect{Model: "x", BaseURL: "http://unused", HTTPClient: http.DefaultClient}
	if _, err := a.Design(context.Background(), DesignRequest{Prompt: "p"}); err == nil {
		t.Fatal("expected empty-key error")
	}
}
