package langfuse

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace/noop"
)

// capturedRequest records a single HTTP request the Langfuse provider
// would have made to the backend. The recorder is goroutine-safe so
// async paths (SubmitScore goroutine, OTLP batcher) can write
// concurrently with the test reading.
type capturedRequest struct {
	Method string
	Path   string
	Auth   string
	Body   []byte
}

type recorder struct {
	mu  sync.Mutex
	got []capturedRequest
}

func (r *recorder) record(req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.got = append(r.got, capturedRequest{
		Method: req.Method,
		Path:   req.URL.Path,
		Auth:   req.Header.Get("Authorization"),
		Body:   body,
	})
	r.mu.Unlock()
}

func (r *recorder) snapshot() []capturedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]capturedRequest, len(r.got))
	copy(out, r.got)
	return out
}

func (r *recorder) countByPath(path string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.got {
		if c.Path == path {
			n++
		}
	}
	return n
}

// fakeLangfuseServer stands in for cloud.langfuse.com. Accepts every
// request with 200 and records it.
func fakeLangfuseServer(t *testing.T) (*httptest.Server, *recorder) {
	t.Helper()
	r := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.record(req)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, r
}

// resetOTelGlobalProvider restores a noop provider after a test that
// called Init. otel.SetTracerProvider is global; leaving an httptest-
// pointing provider in place would leak across tests.
func resetOTelGlobalProvider(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		otel.SetTracerProvider(noop.NewTracerProvider())
	})
}

// expectedBasicAuth builds the same base64(pub:sec) header the real
// provider sends, so tests can compare against the captured value.
func expectedBasicAuth(pub, sec string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(pub+":"+sec))
}

// --- Init enabled path -------------------------------------------------------

// TestInit_Enabled_BuildsProviderAndExporter wires Init against a fake
// Langfuse host and asserts the returned Provider is enabled + has a
// tracer + has a non-nil HTTP client. Side-effect: otel global tracer
// provider is set; restored on cleanup.
func TestInit_Enabled_BuildsProviderAndExporter(t *testing.T) {
	resetOTelGlobalProvider(t)
	srv, _ := fakeLangfuseServer(t)

	p, err := Init(context.Background(), Config{
		Enabled:   true,
		Host:      srv.URL,
		PublicKey: "pk-test",
		SecretKey: "sk-test",
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if !p.Enabled() {
		t.Error("Provider.Enabled() = false on enabled+keys config")
	}
	if p.Tracer() == nil {
		t.Error("Tracer() returned nil")
	}
	if p.http == nil {
		t.Error("http client is nil")
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

// TestInit_HostTrailingSlash_StrippedFromOTLPEndpoint ensures Host with
// a trailing slash doesn't produce a "https://x//api/public/otel"
// double-slash. Verified indirectly via the trimmed endpoint flow: we
// emit a span and capture the OTLP path the exporter hits.
func TestInit_HostTrailingSlash_StrippedFromOTLPEndpoint(t *testing.T) {
	resetOTelGlobalProvider(t)
	srv, rec := fakeLangfuseServer(t)

	// Trailing slash on host. Internal Init trims with strings.TrimSuffix.
	p, err := Init(context.Background(), Config{
		Enabled:   true,
		Host:      srv.URL + "/",
		PublicKey: "pk-trim",
		SecretKey: "sk-trim",
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	// Emit a span so the batch exporter has something to flush.
	ctx, span := p.SpanGeneration(context.Background(), "test.gen", GenerationOpts{
		System: "anthropic", Model: "claude-test",
	})
	EndGeneration(span, GenerationResult{Model: "claude-test", InputTokens: 1, OutputTokens: 1})
	_ = ctx
	// Force flush; Shutdown drains the BatchSpanProcessor synchronously.
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// Look for an OTLP POST to /api/public/otel (the no-double-slash form).
	for _, req := range rec.snapshot() {
		if strings.HasPrefix(req.Path, "//") {
			t.Errorf("OTLP request used double-slash path: %q", req.Path)
		}
	}
}

// --- SubmitScore enabled path ------------------------------------------------

// TestSubmitScore_Enabled_PostsToScoresAPI verifies the REST score
// submission path hits /api/public/scores with the right Basic Auth
// header and a JSON body containing the score fields.
func TestSubmitScore_Enabled_PostsToScoresAPI(t *testing.T) {
	resetOTelGlobalProvider(t)
	srv, rec := fakeLangfuseServer(t)

	p, err := Init(context.Background(), Config{
		Enabled:   true,
		Host:      srv.URL,
		PublicKey: "pk-score",
		SecretKey: "sk-score",
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	p.SubmitScore("trace-abc", "judge_ranking", 0.875, "test rationale")

	// SubmitScore fires a goroutine — poll briefly for the request.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rec.countByPath("/api/public/scores") >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	gotPosts := rec.countByPath("/api/public/scores")
	if gotPosts < 1 {
		t.Fatalf("/api/public/scores POST count: got %d, want ≥1", gotPosts)
	}

	// Pull the score request and inspect it.
	var scoreReq *capturedRequest
	for i := range rec.snapshot() {
		c := rec.snapshot()[i]
		if c.Path == "/api/public/scores" {
			scoreReq = &c
			break
		}
	}
	if scoreReq == nil {
		t.Fatal("score request not captured")
	}
	if scoreReq.Method != "POST" {
		t.Errorf("method: got %q, want POST", scoreReq.Method)
	}
	if want := expectedBasicAuth("pk-score", "sk-score"); scoreReq.Auth != want {
		t.Errorf("auth header: got %q, want %q", scoreReq.Auth, want)
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(scoreReq.Body, &payload); err != nil {
		t.Fatalf("decode score body: %v body=%s", err, scoreReq.Body)
	}
	if got := payload["traceId"]; got != "trace-abc" {
		t.Errorf("traceId: got %v, want trace-abc", got)
	}
	if got := payload["name"]; got != "judge_ranking" {
		t.Errorf("name: got %v, want judge_ranking", got)
	}
	if got, _ := payload["value"].(float64); got != 0.875 {
		t.Errorf("value: got %v, want 0.875", got)
	}
	if got := payload["comment"]; got != "test rationale" {
		t.Errorf("comment: got %v, want 'test rationale'", got)
	}
}

// TestSubmitScore_EnabledButServerErrors_DoesNotPanic — the goroutine
// inside SubmitScore must swallow non-2xx responses gracefully.
func TestSubmitScore_EnabledButServerErrors_DoesNotPanic(t *testing.T) {
	resetOTelGlobalProvider(t)

	var hits int32
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(errSrv.Close)

	p, err := Init(context.Background(), Config{
		Enabled:   true,
		Host:      errSrv.URL,
		PublicKey: "pk-err",
		SecretKey: "sk-err",
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	p.SubmitScore("trace-x", "eval", 0.1, "")

	// Wait for the async goroutine to complete its HTTP attempt.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&hits) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&hits); got < 1 {
		t.Errorf("server hit count: got %d, want ≥1 (goroutine should have posted)", got)
	}
	// Test passes when no panic + no goroutine deadlock.
}

// --- Instrument helpers enabled path ----------------------------------------

// TestInstrumentJudge_EnabledSendsScore exercises the InstrumentJudge
// finish callback and verifies it triggers a /api/public/scores POST
// (covers the score-API integration path Slice 5+ relies on for
// judge ranking telemetry).
func TestInstrumentJudge_EnabledSendsScore(t *testing.T) {
	resetOTelGlobalProvider(t)
	srv, rec := fakeLangfuseServer(t)

	p, err := Init(context.Background(), Config{
		Enabled:   true,
		Host:      srv.URL,
		PublicKey: "pk-judge",
		SecretKey: "sk-judge",
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	ctx, finish := p.InstrumentJudge(context.Background(), "judge_ranking", "run-789")
	if ctx == nil {
		t.Fatal("returned ctx is nil")
	}
	finish(0.92, "great work", nil)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rec.countByPath("/api/public/scores") >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := rec.countByPath("/api/public/scores"); got < 1 {
		t.Errorf("/api/public/scores count: got %d, want ≥1 (InstrumentJudge.finish should submit score)", got)
	}
}

// TestInstrumentJudge_ErrorSkipsScoreSubmit — when finish is called
// with err != nil, the implementation must NOT submit a score (only
// records the error attribute on the span).
func TestInstrumentJudge_ErrorSkipsScoreSubmit(t *testing.T) {
	resetOTelGlobalProvider(t)
	srv, rec := fakeLangfuseServer(t)

	p, err := Init(context.Background(), Config{
		Enabled:   true,
		Host:      srv.URL,
		PublicKey: "pk-err",
		SecretKey: "sk-err",
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	_, finish := p.InstrumentJudge(context.Background(), "judge_failed", "run-err")
	finish(0, "rationale-ignored-on-err", io.EOF)

	// Give any erroneous goroutine a window to fire.
	time.Sleep(150 * time.Millisecond)

	if got := rec.countByPath("/api/public/scores"); got != 0 {
		t.Errorf("/api/public/scores count: got %d, want 0 (err finish must skip score submit)", got)
	}
}
