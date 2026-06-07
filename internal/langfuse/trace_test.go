package langfuse

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestTraceIDFromContext_EmptyOffTrace(t *testing.T) {
	if id := TraceIDFromContext(context.Background()); id != "" {
		t.Fatalf("off-trace id = %q, want empty", id)
	}
}

func TestTraceIDFromContext_NonEmptyOnTrace(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	defer func() { _ = tp.Shutdown(context.Background()) }()
	ctx, span := tp.Tracer("test").Start(context.Background(), "s")
	defer span.End()

	id := TraceIDFromContext(ctx)
	if len(id) != 32 { // otel trace id hex string
		t.Fatalf("on-trace id = %q (len %d), want 32-char hex", id, len(id))
	}
}
