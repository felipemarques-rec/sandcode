package orchestrator

import (
	"context"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/langfuse"
)

func TestNewKernelTracer_NilProvider(t *testing.T) {
	tr := NewKernelTracer(nil)
	ctx, end := tr.Start(context.Background(), "kernel.classify", nil)
	if ctx == nil {
		t.Fatal("nil ctx")
	}
	end(nil) // no panic
	if tr.TraceID(context.Background()) != "" {
		t.Fatal("nil provider must yield empty trace id")
	}
}

func TestNewKernelTracer_DisabledProvider(t *testing.T) {
	p, _ := langfuse.Init(context.Background(), langfuse.Config{Enabled: false})
	tr := NewKernelTracer(p)
	ctx, end := tr.Start(context.Background(), "kernel.plan", map[string]string{"k": "v"})
	end(nil)
	if id := tr.TraceID(ctx); id != "" {
		t.Fatalf("disabled provider trace id = %q, want empty", id)
	}
}
