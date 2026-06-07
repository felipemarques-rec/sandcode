// internal/langfuse/trace.go
package langfuse

import (
	"context"

	"go.opentelemetry.io/otel/trace"
)

// TraceIDFromContext returns the active OTel trace id from ctx as a
// lowercase hex string, or "" when no valid span is in ctx. Used by the
// orchestrator/kernel to stamp event.Event.TraceID for cross-system
// correlation without those packages importing otel directly.
func TraceIDFromContext(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.TraceID().IsValid() {
		return ""
	}
	return sc.TraceID().String()
}
