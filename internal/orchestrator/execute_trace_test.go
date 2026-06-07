package orchestrator

import (
	"context"
	"path/filepath"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	gitm "github.com/felipemarques-rec/sandcode/internal/git"
	"github.com/felipemarques-rec/sandcode/internal/kernel"
	"github.com/felipemarques-rec/sandcode/internal/langfuse"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
)

func TestExecute_RootSpanWrapsKernelAndDispatch(t *testing.T) {
	repo := initRepo(t)
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	defer func() { _ = tp.Shutdown(context.Background()) }()
	lf := langfuse.NewProviderForTest(tp)

	// brain may be nil — classify always runs, which is enough to prove
	// nesting. Kernel must carry the langfuse-backed tracer.
	kn := kernel.New(nil, kernel.WithTracer(NewKernelTracer(lf)))

	events, await, err := Execute(context.Background(),
		sandbox.NewNoSandboxProvider(), &noopAuth{},
		ExecuteOptions{
			Prompt:         "hello world",
			CWD:            repo,
			SandboxImage:   "ignored-by-nosandbox",
			SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", t.Name(), "0"),
			Strategy:       gitm.StrategyMergeToHead,
			Kernel:         kn,
			Agent:          &fakeAgent{script: `echo hi`},
			Langfuse:       lf,
		})
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	_ = await() // ends the root span (synchronous exporter flushes here)

	var rootID, classifyParent string
	names := map[string]bool{}
	var execAttrs []attribute.KeyValue
	for _, s := range exp.GetSpans() {
		names[s.Name] = true
		switch s.Name {
		case "sandcode.brain.execute":
			rootID = s.SpanContext.SpanID().String()
			execAttrs = s.Attributes
		case "kernel.classify":
			classifyParent = s.Parent.SpanID().String()
		}
	}
	if !names["sandcode.brain.execute"] {
		t.Fatalf("missing root span sandcode.brain.execute; got %v", names)
	}
	if !names["kernel.classify"] {
		t.Fatalf("missing kernel.classify span; got %v", names)
	}
	if !names["sandcode.brain.dispatch"] {
		t.Fatalf("missing dispatch child span; got %v", names)
	}
	if classifyParent == "" || classifyParent != rootID {
		t.Fatalf("kernel.classify parent=%q, want execute root=%q", classifyParent, rootID)
	}
	var gotKind bool
	for _, a := range execAttrs {
		if string(a.Key) == "sandcode.dispatch.kind" && a.Value.AsString() != "" {
			gotKind = true
		}
	}
	if !gotKind {
		t.Fatalf("execute root span missing non-empty sandcode.dispatch.kind attribute; attrs=%v", execAttrs)
	}
}
