package orchestrator

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	gitm "github.com/felipemarques-rec/sandcode/internal/git"
	"github.com/felipemarques-rec/sandcode/internal/langfuse"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
)

func TestRun_RootSpanEndsAfterRunCompletes(t *testing.T) {
	repo := initRepo(t)
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	defer func() { _ = tp.Shutdown(context.Background()) }()
	lf := langfuse.NewProviderForTest(tp)

	events, await, err := Run(context.Background(),
		sandbox.NewNoSandboxProvider(),
		&fakeAgent{script: `sleep 0.12; echo done`},
		&noopAuth{},
		RunOptions{
			Prompt:         "noop",
			CWD:            repo,
			SandboxImage:   "ignored-by-nosandbox",
			SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", t.Name(), "0"),
			Strategy:       gitm.StrategyMergeToHead,
			Langfuse:       lf,
		})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	_ = await()

	spans := exp.GetSpans()
	if len(spans) == 0 {
		t.Fatal("no spans exported")
	}
	var run tracetest.SpanStub
	for _, s := range spans {
		if s.Name == "sandcode.brain.run" {
			run = s
		}
	}
	if run.Name == "" {
		t.Fatalf("run span not found; got %d spans: %v", len(spans), spanNames(spans))
	}
	// 80ms cleanly bisects: G1-buggy span (setup-only, ~10ms) vs fixed
	// span that includes the 120ms agent sleep.
	if d := run.EndTime.Sub(run.StartTime); d < 80*time.Millisecond {
		t.Fatalf("run span duration %v too short — span ended before run completed (G1 bug)", d)
	}
}

func spanNames(spans tracetest.SpanStubs) []string {
	out := make([]string, len(spans))
	for i, s := range spans {
		out[i] = s.Name
	}
	return out
}
