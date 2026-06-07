// internal/orchestrator/kernel_tracer.go
package orchestrator

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/felipemarques-rec/sandcode/internal/kernel"
	"github.com/felipemarques-rec/sandcode/internal/langfuse"
)

// kernelTracer adapts a *langfuse.Provider to kernel.Tracer. Kept here
// (not in internal/kernel) so the kernel imports neither otel nor
// langfuse. Safe with a nil or disabled provider: spans become inert.
type kernelTracer struct{ lf *langfuse.Provider }

// Compile-time proof the adapter satisfies the kernel's tracing seam.
var _ kernel.Tracer = kernelTracer{}

// NewKernelTracer returns a kernel.Tracer backed by lf. When lf is nil
// or disabled, every span is a no-op and TraceID returns "".
func NewKernelTracer(lf *langfuse.Provider) kernel.Tracer {
	return kernelTracer{lf: lf}
}

func (t kernelTracer) Start(ctx context.Context, name string, attrs map[string]string) (context.Context, func(error)) {
	if t.lf == nil || !t.lf.Enabled() {
		return ctx, func(error) {}
	}
	kv := make([]attribute.KeyValue, 0, len(attrs)+1)
	kv = append(kv, attribute.String("sandcode.component", "kernel"))
	for k, v := range attrs {
		kv = append(kv, attribute.String(k, v))
	}
	ctx, span := t.lf.Tracer().Start(ctx, name)
	span.SetAttributes(kv...)
	return ctx, func(err error) {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}
}

func (t kernelTracer) TraceID(ctx context.Context) string {
	if t.lf == nil || !t.lf.Enabled() {
		return ""
	}
	return langfuse.TraceIDFromContext(ctx)
}
