package planner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLLMPlanner_HappyPath_MultiNodeDAG(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/messages" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "test-key" {
			t.Errorf("missing api key header")
		}
		// Verify the request body asked for the decompose tool with
		// ToolChoice forced — that's the contract this test guards.
		var req apiRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.ToolChoice["name"] != "decompose" {
			t.Errorf("ToolChoice = %+v, want forced decompose", req.ToolChoice)
		}

		resp := map[string]any{
			"stop_reason": "tool_use",
			"content": []any{
				map[string]any{
					"type": "tool_use",
					"name": "decompose",
					"input": map[string]any{
						"nodes": []any{
							map[string]any{
								"id":     "schema",
								"prompt": "Design the SQLite schema",
							},
							map[string]any{
								"id":         "endpoints",
								"prompt":     "Implement CRUD endpoints over the schema",
								"depends_on": []string{"schema"},
							},
							map[string]any{
								"id":         "tests",
								"prompt":     "Cover the endpoints with integration tests",
								"depends_on": []string{"endpoints"},
							},
						},
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	p := NewLLMPlanner("test-key", "")
	p.BaseURL = server.URL

	dag, err := p.Decompose(context.Background(), "build a CRUD API for users")
	if err != nil {
		t.Fatalf("Decompose: %v", err)
	}
	if len(dag.Nodes) != 3 {
		t.Fatalf("len(Nodes) = %d, want 3", len(dag.Nodes))
	}
	order, err := dag.TopoSort()
	if err != nil {
		t.Fatalf("TopoSort: %v", err)
	}
	if !(order[0] == "schema" && order[1] == "endpoints" && order[2] == "tests") {
		t.Errorf("topo order = %v, want [schema endpoints tests]", order)
	}
}

func TestLLMPlanner_HappyPath_SingleRootDAG(t *testing.T) {
	// The system prompt instructs the model to fall back to a single
	// root node for simple tasks — assert we accept that shape too.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := map[string]any{
			"stop_reason": "tool_use",
			"content": []any{
				map[string]any{
					"type": "tool_use",
					"name": "decompose",
					"input": map[string]any{
						"nodes": []any{
							map[string]any{
								"id":     "root",
								"prompt": "Fix the typo in README.md",
							},
						},
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	p := NewLLMPlanner("k", "")
	p.BaseURL = server.URL

	dag, err := p.Decompose(context.Background(), "fix typo in README")
	if err != nil {
		t.Fatalf("Decompose: %v", err)
	}
	if len(dag.Nodes) != 1 || dag.Nodes[0].ID != "root" {
		t.Errorf("got %+v, want single 'root' node", dag.Nodes)
	}
}

func TestLLMPlanner_RejectsEmptyPrompt(t *testing.T) {
	p := NewLLMPlanner("k", "")
	_, err := p.Decompose(context.Background(), "")
	if err == nil {
		t.Fatal("err = nil, want non-nil")
	}
}

func TestLLMPlanner_RejectsEmptyAPIKey(t *testing.T) {
	p := NewLLMPlanner("", "")
	_, err := p.Decompose(context.Background(), "something")
	if err == nil || !strings.Contains(err.Error(), "API key") {
		t.Fatalf("err = %v, want API-key error", err)
	}
}

func TestLLMPlanner_PropagatesAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"bad model"}}`))
	}))
	defer server.Close()
	p := NewLLMPlanner("k", "")
	p.BaseURL = server.URL

	_, err := p.Decompose(context.Background(), "anything")
	if err == nil {
		t.Fatal("err = nil, want non-nil from 400 response")
	}
	if !strings.Contains(err.Error(), "bad model") {
		t.Errorf("err = %v, want propagated message", err)
	}
}

func TestLLMPlanner_RejectsInvalidDAGFromLLM(t *testing.T) {
	// LLM returns a DAG with a cycle. Decompose must catch it via
	// Validate() rather than handing a bad graph downstream.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := map[string]any{
			"stop_reason": "tool_use",
			"content": []any{
				map[string]any{
					"type": "tool_use",
					"name": "decompose",
					"input": map[string]any{
						"nodes": []any{
							map[string]any{"id": "a", "prompt": "x", "depends_on": []string{"b"}},
							map[string]any{"id": "b", "prompt": "y", "depends_on": []string{"a"}},
						},
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	p := NewLLMPlanner("k", "")
	p.BaseURL = server.URL

	_, err := p.Decompose(context.Background(), "x")
	if err == nil {
		t.Fatal("err = nil, want validation error for cyclic LLM output")
	}
	if !strings.Contains(err.Error(), "invalid dag") {
		t.Errorf("err = %v, want 'invalid dag from LLM' wrapper", err)
	}
}

func TestLLMPlanner_NoToolUseInResponse(t *testing.T) {
	// Defensive: if the LLM ignores the forced tool-use directive
	// and returns plain text, we must surface a clear error.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := map[string]any{
			"stop_reason": "end_turn",
			"content": []any{
				map[string]any{"type": "text", "text": "I refuse to decompose"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()
	p := NewLLMPlanner("k", "")
	p.BaseURL = server.URL

	_, err := p.Decompose(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "no decompose") {
		t.Errorf("err = %v, want 'no decompose tool_use' error", err)
	}
}

// Compile-time: LLMPlanner satisfies Planner.
var _ Planner = (*LLMPlanner)(nil)
