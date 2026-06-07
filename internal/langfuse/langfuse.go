// Package langfuse provides Langfuse integration for LLM observability.
//
// Langfuse is used to instrument, trace, and evaluate all LLM interactions
// in sandcode. It uses the OpenTelemetry OTLP/HTTP protocol to send traces
// to the Langfuse backend, with GenAI semantic conventions for proper
// prompt/completion/token visualization.
//
// Configuration is via environment variables (12-Factor):
//
//	LANGFUSE_HOST        — Langfuse server URL (default: https://cloud.langfuse.com)
//	LANGFUSE_PUBLIC_KEY  — Public API key
//	LANGFUSE_SECRET_KEY  — Secret API key
//	LANGFUSE_ENABLED     — Set to "true" to enable (default: false)
package langfuse

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// GenAI semantic convention attribute keys.
// See: https://opentelemetry.io/docs/specs/semconv/gen-ai/
const (
	attrGenAISystem            = "gen_ai.system"
	attrGenAIRequestModel      = "gen_ai.request.model"
	attrGenAIResponseModel     = "gen_ai.response.model"
	attrGenAIUsageInputTokens  = "gen_ai.usage.input_tokens"
	attrGenAIUsageOutputTokens = "gen_ai.usage.output_tokens"
	attrGenAIUsageTotalTokens  = "gen_ai.usage.total_tokens"
	attrGenAIPrompt            = "gen_ai.prompt"
	attrGenAICompletion        = "gen_ai.completion"

	// Langfuse-specific attributes for metadata mapping.
	attrLangfuseSessionID = "langfuse.trace.session_id"
	attrLangfuseUserID    = "langfuse.trace.user_id"
	attrLangfuseVersion   = "langfuse.trace.version"
	attrLangfuseTags      = "langfuse.trace.tags"
)

// Config holds the Langfuse connection configuration.
type Config struct {
	Host      string // Langfuse server URL
	PublicKey string // Public API key
	SecretKey string // Secret API key
	Enabled   bool   // Whether tracing is active
}

// ConfigFromEnv reads Langfuse configuration from environment variables.
func ConfigFromEnv() Config {
	return Config{
		Host:      envOrDefault("LANGFUSE_HOST", "https://cloud.langfuse.com"),
		PublicKey: os.Getenv("LANGFUSE_PUBLIC_KEY"),
		SecretKey: os.Getenv("LANGFUSE_SECRET_KEY"),
		Enabled:   strings.ToLower(os.Getenv("LANGFUSE_ENABLED")) == "true",
	}
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Provider wraps the OpenTelemetry TracerProvider configured for Langfuse.
type Provider struct {
	tp     *sdktrace.TracerProvider
	tracer trace.Tracer
	config Config
	http   *http.Client
}

// Init creates and registers a TracerProvider that exports to Langfuse
// via OTLP/HTTP. Returns a no-op provider if Langfuse is not configured.
func Init(ctx context.Context, cfg Config) (*Provider, error) {
	if !cfg.Enabled || cfg.PublicKey == "" || cfg.SecretKey == "" {
		slog.Info("langfuse: disabled (set LANGFUSE_ENABLED=true to enable)")
		return &Provider{config: cfg}, nil
	}

	// Build Basic Auth header: base64(publicKey:secretKey)
	authString := base64.StdEncoding.EncodeToString(
		[]byte(cfg.PublicKey + ":" + cfg.SecretKey),
	)

	endpoint := strings.TrimSuffix(cfg.Host, "/") + "/api/public/otel"

	exporter, err := otlptracehttp.New(ctx,
		otlptracehttp.WithEndpointURL(endpoint),
		otlptracehttp.WithHeaders(map[string]string{
			"Authorization": "Basic " + authString,
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("langfuse: create exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter,
			sdktrace.WithMaxExportBatchSize(50),
			sdktrace.WithBatchTimeout(5*time.Second),
		),
		sdktrace.WithResource(resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName("sandcode"),
			semconv.ServiceVersion("0.1.0"),
			attribute.String("deployment.environment", envOrDefault("SANDCODE_ENV", "development")),
		)),
	)

	otel.SetTracerProvider(tp)
	tracer := tp.Tracer("sandcode")

	slog.Info("langfuse: initialized",
		"host", cfg.Host,
		"endpoint", endpoint,
	)

	return &Provider{
		tp:     tp,
		tracer: tracer,
		config: cfg,
		http:   &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// Shutdown flushes pending traces and shuts down the provider.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p.tp == nil {
		return nil
	}
	return p.tp.Shutdown(ctx)
}

// Tracer returns the OTel tracer. Safe to call even if disabled (returns no-op).
func (p *Provider) Tracer() trace.Tracer {
	if p.tracer != nil {
		return p.tracer
	}
	return otel.Tracer("sandcode")
}

// Enabled returns whether Langfuse tracing is active.
func (p *Provider) Enabled() bool {
	return p.config.Enabled && p.tp != nil
}

// ── Score API ────────────────────────────────────────────────────────────

// SubmitScore asynchronously sends an evaluation score to Langfuse's REST API,
// attaching it to the current trace.
func (p *Provider) SubmitScore(traceID string, name string, value float64, comment string) {
	if !p.Enabled() {
		return
	}

	payload := map[string]interface{}{
		"traceId": traceID,
		"name":    name,
		"value":   value,
		"comment": comment,
	}
	body, _ := json.Marshal(payload)

	go func() {
		req, err := http.NewRequest("POST", strings.TrimSuffix(p.config.Host, "/")+"/api/public/scores", bytes.NewReader(body))
		if err != nil {
			return
		}

		authString := base64.StdEncoding.EncodeToString([]byte(p.config.PublicKey + ":" + p.config.SecretKey))
		req.Header.Set("Authorization", "Basic "+authString)
		req.Header.Set("Content-Type", "application/json")

		resp, err := p.http.Do(req)
		if err != nil {
			slog.Warn("langfuse: failed to submit score", "error", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			slog.Warn("langfuse: score API returned error", "status", resp.Status)
		}
	}()
}

// ── Span Helpers ─────────────────────────────────────────────────────────

// SpanGeneration creates a span representing an LLM generation (call).
// This maps to a Langfuse "generation" observation with proper GenAI attributes.
func (p *Provider) SpanGeneration(ctx context.Context, name string, opts GenerationOpts) (context.Context, trace.Span) {
	attrs := []attribute.KeyValue{
		attribute.String(attrGenAISystem, opts.System),
		attribute.String(attrGenAIRequestModel, opts.Model),
	}

	if opts.SessionID != "" {
		attrs = append(attrs, attribute.String(attrLangfuseSessionID, opts.SessionID))
	}
	if opts.UserID != "" {
		attrs = append(attrs, attribute.String(attrLangfuseUserID, opts.UserID))
	}

	ctx, span := p.Tracer().Start(ctx, name,
		trace.WithAttributes(attrs...),
		trace.WithSpanKind(trace.SpanKindClient),
	)
	return ctx, span
}

// EndGeneration records the completion result on a generation span.
func EndGeneration(span trace.Span, result GenerationResult) {
	span.SetAttributes(
		attribute.String(attrGenAIResponseModel, result.Model),
		attribute.Int(attrGenAIUsageInputTokens, result.InputTokens),
		attribute.Int(attrGenAIUsageOutputTokens, result.OutputTokens),
		attribute.Int(attrGenAIUsageTotalTokens, result.InputTokens+result.OutputTokens),
	)
	if result.Prompt != "" {
		// Truncate to avoid massive spans
		prompt := result.Prompt
		if len(prompt) > 4000 {
			prompt = prompt[:4000] + "...(truncated)"
		}
		span.SetAttributes(attribute.String(attrGenAIPrompt, prompt))
	}
	if result.Completion != "" {
		completion := result.Completion
		if len(completion) > 4000 {
			completion = completion[:4000] + "...(truncated)"
		}
		span.SetAttributes(attribute.String(attrGenAICompletion, completion))
	}
	span.End()
}

// SpanEvaluation creates a span representing an evaluation (e.g., judge scoring).
// This maps to a Langfuse "score" observation.
func (p *Provider) SpanEvaluation(ctx context.Context, name string, opts EvaluationOpts) (context.Context, trace.Span) {
	attrs := []attribute.KeyValue{
		attribute.String("langfuse.observation.type", "evaluation"),
		attribute.String("evaluation.name", opts.Name),
	}
	if opts.RunID != "" {
		attrs = append(attrs, attribute.String("sandcode.run_id", opts.RunID))
	}

	ctx, span := p.Tracer().Start(ctx, name,
		trace.WithAttributes(attrs...),
	)
	return ctx, span
}

// EndEvaluation records the evaluation result on the span.
func EndEvaluation(span trace.Span, score float64, comment string) {
	span.SetAttributes(
		attribute.Float64("evaluation.score", score),
		attribute.String("evaluation.comment", comment),
	)
	span.End()
}

// SpanBrain creates a span for brain operations (recall, enrich, learn).
func (p *Provider) SpanBrain(ctx context.Context, operation string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	allAttrs := append([]attribute.KeyValue{
		attribute.String("sandcode.component", "brain"),
		attribute.String("sandcode.operation", operation),
	}, attrs...)

	return p.Tracer().Start(ctx, "sandcode.brain."+operation,
		trace.WithAttributes(allAttrs...),
	)
}

// ── Options structs ──────────────────────────────────────────────────────

// GenerationOpts configures a generation span.
type GenerationOpts struct {
	System    string // "anthropic", "openai", etc.
	Model     string // "claude-sonnet-4-20250514", "gpt-4o", etc.
	SessionID string // groups traces in Langfuse
	UserID    string // for per-user analytics
}

// GenerationResult captures the output of an LLM call.
type GenerationResult struct {
	Model        string
	InputTokens  int
	OutputTokens int
	Prompt       string
	Completion   string
}

// EvaluationOpts configures an evaluation span.
type EvaluationOpts struct {
	Name  string // e.g., "judge_ranking", "brain_confidence"
	RunID string
}
