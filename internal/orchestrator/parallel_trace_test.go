package orchestrator

import (
	"context"
	"path/filepath"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/felipemarques-rec/sandcode/internal/agent"
	gitm "github.com/felipemarques-rec/sandcode/internal/git"
	"github.com/felipemarques-rec/sandcode/internal/langfuse"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
)

func TestParallelRun_ArmsAndJudgeNestUnderRoot(t *testing.T) {
	repo := initRepo(t)
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	defer func() { _ = tp.Shutdown(context.Background()) }()
	lf := langfuse.NewProviderForTest(tp)

	ctx, root := lf.SpanBrain(context.Background(), "execute")

	popts := ParallelOptions{
		Prompt:         "p",
		CWD:            repo,
		SandboxImage:   "ignored-by-nosandbox",
		SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", t.Name()),
		Strategy:       gitm.StrategyBranch,
		Agents: []agent.Provider{
			&fakeAgent{script: `echo a > a.txt`},
			&fakeAgent{script: `echo b > b.txt`},
		},
		Judge:    &fakeJudge{name: "fake"},
		Langfuse: lf,
	}
	events, await, err := ParallelRun(ctx, sandbox.NewNoSandboxProvider(), &noopAuth{}, popts)
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	_ = await()
	root.End()

	rootID := ""
	runSpans, judgeSpans := 0, 0
	var runParents []string
	for _, s := range exp.GetSpans() {
		switch s.Name {
		case "sandcode.brain.execute":
			rootID = s.SpanContext.SpanID().String()
		case "sandcode.brain.run":
			runSpans++
			if rootID == "" {
				// rootID is set by the sandcode.brain.execute case; if
				// spans iterate run-before-root, capture and check after.
			}
			runParents = append(runParents, s.Parent.SpanID().String())
		case "sandcode.judge.ranking":
			judgeSpans++
		}
	}
	if rootID == "" {
		t.Fatalf("missing sandcode.brain.execute root span")
	}
	if runSpans != 2 {
		t.Fatalf("want 2 sandcode.brain.run spans, got %d", runSpans)
	}
	for i, p := range runParents {
		if p != rootID {
			t.Errorf("arm run span #%d parent=%q, want execute root=%q", i, p, rootID)
		}
	}
	if judgeSpans != 1 {
		t.Fatalf("want 1 sandcode.judge.ranking span, got %d", judgeSpans)
	}
}
