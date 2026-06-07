package secreview

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func secServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

func TestLLMSecurityReviewer_HappyPath(t *testing.T) {
	srv := secServer(t, `{"stop_reason":"tool_use","content":[{"type":"tool_use","name":"security_review","input":{"findings":[{"rule":"sql_injection","severity":"high","detail":"unparameterized query"},{"rule":"weak_hash","severity":"medium","detail":"md5 used"}]}}]}`)
	defer srv.Close()
	r := NewLLMSecurityReviewer("k", "x")
	r.BaseURL = srv.URL
	rep, err := r.Review(context.Background(), SecRequest{RunID: "r", Diff: "+ db.Query(q)"})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if len(rep.Findings) != 2 || rep.Findings[0].Rule != "sql_injection" || rep.Findings[0].Severity != "high" {
		t.Fatalf("findings: %+v", rep.Findings)
	}
	if rep.Reviewer != "llm:x" {
		t.Fatalf("reviewer: %q", rep.Reviewer)
	}
}

func TestLLMSecurityReviewer_NoFindings(t *testing.T) {
	srv := secServer(t, `{"stop_reason":"tool_use","content":[{"type":"tool_use","name":"security_review","input":{"findings":[]}}]}`)
	defer srv.Close()
	r := NewLLMSecurityReviewer("k", "x")
	r.BaseURL = srv.URL
	rep, err := r.Review(context.Background(), SecRequest{Diff: "+ clean code"})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if len(rep.Findings) != 0 {
		t.Fatalf("want no findings, got %+v", rep.Findings)
	}
}

func TestLLMSecurityReviewer_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"bad model"}}`))
	}))
	defer srv.Close()
	r := NewLLMSecurityReviewer("k", "x")
	r.BaseURL = srv.URL
	if _, err := r.Review(context.Background(), SecRequest{Diff: "d"}); err == nil {
		t.Fatal("expected api error")
	}
}

func TestLLMSecurityReviewer_EmptyKey(t *testing.T) {
	r := &LLMSecurityReviewer{Model: "x", BaseURL: "http://unused", HTTPClient: http.DefaultClient}
	if _, err := r.Review(context.Background(), SecRequest{Diff: "d"}); err == nil {
		t.Fatal("expected empty-key error")
	}
}
