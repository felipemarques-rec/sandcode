package langfuse

import (
	"context"
	"errors"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// --- Config / env helpers ----------------------------------------------------

func TestEnvOrDefault_UnsetReturnsFallback(t *testing.T) {
	t.Setenv("SANDCODE_TEST_LANGFUSE_UNSET", "")
	if got := envOrDefault("SANDCODE_TEST_LANGFUSE_UNSET", "fallback"); got != "fallback" {
		t.Errorf("got %q, want %q", got, "fallback")
	}
}

func TestEnvOrDefault_SetReturnsValue(t *testing.T) {
	t.Setenv("SANDCODE_TEST_LANGFUSE_SET", "actual")
	if got := envOrDefault("SANDCODE_TEST_LANGFUSE_SET", "fallback"); got != "actual" {
		t.Errorf("got %q, want %q", got, "actual")
	}
}

func TestConfigFromEnv_DefaultsWhenUnset(t *testing.T) {
	// Force clean slate for the four langfuse vars.
	t.Setenv("LANGFUSE_HOST", "")
	t.Setenv("LANGFUSE_PUBLIC_KEY", "")
	t.Setenv("LANGFUSE_SECRET_KEY", "")
	t.Setenv("LANGFUSE_ENABLED", "")

	cfg := ConfigFromEnv()
	if cfg.Host != "https://cloud.langfuse.com" {
		t.Errorf("Host: got %q, want default", cfg.Host)
	}
	if cfg.PublicKey != "" || cfg.SecretKey != "" {
		t.Errorf("keys: got pub=%q sec=%q, want empty", cfg.PublicKey, cfg.SecretKey)
	}
	if cfg.Enabled {
		t.Error("Enabled: got true, want false (default)")
	}
}

func TestConfigFromEnv_ReadsAllFields(t *testing.T) {
	t.Setenv("LANGFUSE_HOST", "https://example.test")
	t.Setenv("LANGFUSE_PUBLIC_KEY", "pk-xyz")
	t.Setenv("LANGFUSE_SECRET_KEY", "sk-xyz")
	t.Setenv("LANGFUSE_ENABLED", "true")

	cfg := ConfigFromEnv()
	if cfg.Host != "https://example.test" {
		t.Errorf("Host: got %q", cfg.Host)
	}
	if cfg.PublicKey != "pk-xyz" {
		t.Errorf("PublicKey: got %q", cfg.PublicKey)
	}
	if cfg.SecretKey != "sk-xyz" {
		t.Errorf("SecretKey: got %q", cfg.SecretKey)
	}
	if !cfg.Enabled {
		t.Error("Enabled: got false, want true")
	}
}

func TestConfigFromEnv_EnabledCaseInsensitive(t *testing.T) {
	t.Setenv("LANGFUSE_ENABLED", "TRUE")
	if !ConfigFromEnv().Enabled {
		t.Error("LANGFUSE_ENABLED=TRUE should parse as enabled")
	}
	t.Setenv("LANGFUSE_ENABLED", "True")
	if !ConfigFromEnv().Enabled {
		t.Error("LANGFUSE_ENABLED=True should parse as enabled")
	}
	t.Setenv("LANGFUSE_ENABLED", "1")
	if ConfigFromEnv().Enabled {
		t.Error("LANGFUSE_ENABLED=1 must NOT parse as enabled (strict 'true')")
	}
	t.Setenv("LANGFUSE_ENABLED", "yes")
	if ConfigFromEnv().Enabled {
		t.Error("LANGFUSE_ENABLED=yes must NOT parse as enabled (strict 'true')")
	}
}

// --- Init disabled path ------------------------------------------------------

func TestInit_DisabledByDefault_ReturnsNoOpProvider(t *testing.T) {
	p, err := Init(context.Background(), Config{Enabled: false})
	if err != nil {
		t.Fatalf("Init disabled: unexpected err: %v", err)
	}
	if p == nil {
		t.Fatal("Init returned nil provider")
	}
	if p.Enabled() {
		t.Error("Provider.Enabled() = true on disabled config")
	}
}

func TestInit_EnabledButMissingKeys_ReturnsNoOpProvider(t *testing.T) {
	// Spec: PublicKey/SecretKey required even when Enabled=true.
	p, err := Init(context.Background(), Config{Enabled: true})
	if err != nil {
		t.Fatalf("Init missing keys: unexpected err: %v", err)
	}
	if p.Enabled() {
		t.Error("Provider.Enabled() = true when keys are missing")
	}

	p2, err := Init(context.Background(), Config{Enabled: true, PublicKey: "pk-only"})
	if err != nil {
		t.Fatalf("Init half-key: unexpected err: %v", err)
	}
	if p2.Enabled() {
		t.Error("Provider.Enabled() = true with only PublicKey")
	}
}

// --- Provider no-op surface --------------------------------------------------

func TestProvider_Tracer_NoOpReturnsNonNil(t *testing.T) {
	p := &Provider{} // zero value
	tr := p.Tracer()
	if tr == nil {
		t.Fatal("Tracer() returned nil on no-op provider")
	}
}

func TestProvider_Shutdown_NoOpIsSafe(t *testing.T) {
	p := &Provider{} // tp == nil
	if err := p.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown on no-op returned err: %v", err)
	}
}

func TestProvider_Enabled_ZeroValueIsFalse(t *testing.T) {
	p := &Provider{}
	if p.Enabled() {
		t.Error("zero-value Provider.Enabled() = true, want false")
	}
}

// --- Instrument helpers no-op when disabled ----------------------------------

func TestInstrumentJudge_DisabledReturnsNoOpFinish(t *testing.T) {
	p := &Provider{} // disabled
	ctx := context.Background()

	gotCtx, finish := p.InstrumentJudge(ctx, "judge_ranking", "run-1")
	if gotCtx == nil {
		t.Fatal("returned ctx is nil")
	}
	if finish == nil {
		t.Fatal("returned finish fn is nil")
	}
	// finish must not panic / not attempt HTTP submit.
	finish(0.42, "ok", nil)
	finish(0, "", errors.New("boom"))
}

func TestInstrumentLLMCall_DisabledReturnsNoOpFinish(t *testing.T) {
	p := &Provider{} // disabled
	ctx := context.Background()

	gotCtx, finish := p.InstrumentLLMCall(ctx, "test", "anthropic", "model-x")
	if gotCtx == nil {
		t.Fatal("returned ctx is nil")
	}
	if finish == nil {
		t.Fatal("returned finish fn is nil")
	}
	finish(GenerationResult{
		Model:        "model-x",
		InputTokens:  100,
		OutputTokens: 50,
		Prompt:       "hello",
		Completion:   "world",
	})
}

func TestInstrumentBrainOp_DisabledReturnsNoOpFinish(t *testing.T) {
	p := &Provider{} // disabled
	ctx := context.Background()

	gotCtx, finish := p.InstrumentBrainOp(ctx, "recall", "run-1")
	if gotCtx == nil {
		t.Fatal("returned ctx is nil")
	}
	if finish == nil {
		t.Fatal("returned finish fn is nil")
	}
	finish(7, nil)
	finish(0, errors.New("recall failed"))
}

// --- SubmitScore disabled path -----------------------------------------------

// SubmitScore on a disabled provider must NOT spawn a goroutine that tries to
// hit the network. Hard to assert directly without httptest plumbing; the
// minimum-viable test is that the call returns without panic and synchronously.
func TestSubmitScore_DisabledIsNoOp(t *testing.T) {
	p := &Provider{} // disabled
	// Must not panic; goroutine path is gated by Enabled().
	p.SubmitScore("trace-1", "eval", 0.5, "comment")
}

// --- Regression: nil http client in NewProviderForTest -----------------------

func TestNewProviderForTest_JudgeSuccessDoesNotPanic(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	defer func() { _ = tp.Shutdown(context.Background()) }()
	p := NewProviderForTest(tp)

	// InstrumentJudge's finish(err=nil) calls SubmitScore, which runs an
	// async goroutine doing p.http.Do(...). Pre-fix, p.http was nil and
	// that goroutine panicked, crashing the process. This must not panic.
	_, finish := p.InstrumentJudge(context.Background(), "ranking", "run-1")
	finish(0.9, "ok", nil)

	// Give the async SubmitScore goroutine time to run so a panic (if
	// reintroduced) would crash the test process before it exits.
	time.Sleep(100 * time.Millisecond)
}
