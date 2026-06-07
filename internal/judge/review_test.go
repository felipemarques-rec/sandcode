package judge

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// reviewServer returns an httptest server that echoes a forced review tool_use
// with the given score and comments.
func reviewServer(t *testing.T, score float64, comments string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"stop_reason":"tool_use","content":[{"type":"tool_use","name":"review","input":{"score":` +
			strconv.FormatFloat(score, 'g', -1, 64) + `,"comments":"` + comments + `"}}]}`))
	}))
}

func TestBuildReviewPrompt_VerifyBranches(t *testing.T) {
	// FAILED branch must surface the verify tail.
	p := buildReviewPrompt(ReviewRequest{Prompt: "p", Diff: "d", VerifyRan: true, VerifyPassed: false, VerifyTail: "boom-tail"})
	if !strings.Contains(p, "FAILED") || !strings.Contains(p, "boom-tail") {
		t.Fatalf("failed-branch prompt missing FAILED/tail: %q", p)
	}
	// no-verifier branch.
	if p := buildReviewPrompt(ReviewRequest{Prompt: "p", Diff: "d", VerifyRan: false}); !strings.Contains(p, "no verifier ran") {
		t.Fatalf("not-ran branch wrong: %q", p)
	}
	// passed branch.
	if p := buildReviewPrompt(ReviewRequest{Prompt: "p", Diff: "d", VerifyRan: true, VerifyPassed: true}); !strings.Contains(p, "passed") {
		t.Fatalf("passed branch wrong: %q", p)
	}
}

func TestLLMReviewer_HappyPath(t *testing.T) {
	srv := reviewServer(t, 0.8, "looks good")
	defer srv.Close()
	r := NewLLMReviewer("k", "x")
	r.BaseURL = srv.URL
	rv, err := r.Review(context.Background(), ReviewRequest{
		RunID: "run-1", Prompt: "add foo", Diff: "diff --git a/x b/x\n+foo\n",
		VerifyRan: true, VerifyPassed: true,
	})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if rv.Score != 0.8 || rv.Comments != "looks good" {
		t.Fatalf("got %+v", rv)
	}
	if rv.Reviewer != "llm:x" {
		t.Fatalf("reviewer name: %q", rv.Reviewer)
	}
}

func TestLLMReviewer_ClampsScoreHigh(t *testing.T) {
	srv := reviewServer(t, 1.5, "over")
	defer srv.Close()
	r := NewLLMReviewer("k", "x")
	r.BaseURL = srv.URL
	rv, err := r.Review(context.Background(), ReviewRequest{RunID: "r", Prompt: "p", Diff: "d"})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if rv.Score != 1.0 {
		t.Fatalf("want clamp to 1.0, got %v", rv.Score)
	}
}

func TestLLMReviewer_ClampsScoreLow(t *testing.T) {
	srv := reviewServer(t, -0.2, "under")
	defer srv.Close()
	r := NewLLMReviewer("k", "x")
	r.BaseURL = srv.URL
	rv, err := r.Review(context.Background(), ReviewRequest{RunID: "r", Prompt: "p", Diff: "d"})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if rv.Score != 0.0 {
		t.Fatalf("want clamp to 0.0, got %v", rv.Score)
	}
}

func TestLLMReviewer_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"bad model"}}`))
	}))
	defer srv.Close()
	r := NewLLMReviewer("k", "x")
	r.BaseURL = srv.URL
	if _, err := r.Review(context.Background(), ReviewRequest{RunID: "r", Prompt: "p", Diff: "d"}); err == nil {
		t.Fatal("expected api error")
	}
}

func TestLLMReviewer_EmptyKey(t *testing.T) {
	r := &LLMReviewer{Model: "x", BaseURL: "http://unused", HTTPClient: http.DefaultClient}
	if _, err := r.Review(context.Background(), ReviewRequest{RunID: "r", Prompt: "p", Diff: "d"}); err == nil {
		t.Fatal("expected empty-key error")
	}
}

// capturingReviewServer records the outgoing request body on a buffered
// channel (race-safe: the receive in the test happens-after the handler's
// send) and replies with a forced review tool_use carrying the given score.
func capturingReviewServer(t *testing.T, score float64) (*httptest.Server, <-chan string) {
	t.Helper()
	bodyCh := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodyCh <- string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"stop_reason":"tool_use","content":[{"type":"tool_use","name":"review","input":{"score":` +
			strconv.FormatFloat(score, 'g', -1, 64) + `,"comments":"ok"}}]}`))
	}))
	return srv, bodyCh
}

func TestNewPerformanceReviewer_SendsPerfSystem(t *testing.T) {
	srv, bodyCh := capturingReviewServer(t, 0.7)
	defer srv.Close()
	r := NewPerformanceReviewer("k", "x")
	r.BaseURL = srv.URL
	rv, err := r.Review(context.Background(), ReviewRequest{RunID: "r", Prompt: "p", Diff: "d"})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	body := <-bodyCh
	if !strings.Contains(body, "performance engineer") {
		t.Fatalf("perf system prompt not sent: %s", body)
	}
	if rv.Reviewer != "llm:x" {
		t.Fatalf("reviewer: %q", rv.Reviewer)
	}
	if rv.Score != 0.7 {
		t.Fatalf("score: %v", rv.Score)
	}
}

func TestNewRefactoringReviewer_SendsRefactorSystem(t *testing.T) {
	srv, bodyCh := capturingReviewServer(t, 0.4)
	defer srv.Close()
	r := NewRefactoringReviewer("k", "x")
	r.BaseURL = srv.URL
	if _, err := r.Review(context.Background(), ReviewRequest{RunID: "r", Prompt: "p", Diff: "d"}); err != nil {
		t.Fatalf("Review: %v", err)
	}
	body := <-bodyCh
	if !strings.Contains(body, "refactoring specialist") {
		t.Fatalf("refactor system prompt not sent: %s", body)
	}
}

func TestNewLLMReviewer_StillSendsReviewSystem(t *testing.T) {
	srv, bodyCh := capturingReviewServer(t, 0.9)
	defer srv.Close()
	r := NewLLMReviewer("k", "x")
	r.BaseURL = srv.URL
	if _, err := r.Review(context.Background(), ReviewRequest{RunID: "r", Prompt: "p", Diff: "d"}); err != nil {
		t.Fatalf("Review: %v", err)
	}
	body := <-bodyCh
	if !strings.Contains(body, "strict senior code reviewer") {
		t.Fatalf("default reviewer must still send reviewSystem: %s", body)
	}
}
