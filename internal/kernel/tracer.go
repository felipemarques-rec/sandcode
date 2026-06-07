package kernel

import "context"

// Tracer is the minimal tracing seam the Kernel needs. The Kernel owns
// this interface deliberately: it imports neither OpenTelemetry nor
// internal/langfuse. A langfuse-backed adapter lives in the orchestrator
// package. When no tracer is configured the Kernel uses noopTracer, so
// every call site is unconditional and panic-free. Attributes are
// string-only by design — Langfuse metadata is stringly-typed; adapters
// stringify typed values.
type Tracer interface {
	// Start begins a child span named `name` carrying the given string
	// attributes (may be nil). The returned end func closes the span;
	// pass a non-nil error to mark it failed. The returned context
	// carries the new span for downstream propagation.
	Start(ctx context.Context, name string, attrs map[string]string) (context.Context, func(err error))

	// TraceID returns the active trace id carried in ctx, or "" when no
	// trace is active. Used to stamp event.Event.TraceID without the
	// Kernel importing otel.
	TraceID(ctx context.Context) string
}

// noopTracer is the zero-cost default. Every method is inert.
type noopTracer struct{}

func (noopTracer) Start(ctx context.Context, _ string, _ map[string]string) (context.Context, func(error)) {
	return ctx, func(error) {}
}
func (noopTracer) TraceID(context.Context) string { return "" }

// WithTracer wires a Tracer. When unset the Kernel uses noopTracer.
func WithTracer(t Tracer) Option {
	return func(k *Kernel) {
		if t != nil { // unlike WithBus/etc, a nil Tracer would panic: no nil-safe call sites
			k.tracer = t
		}
	}
}
