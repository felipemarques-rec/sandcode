package orchestrator

import (
	"context"
	"path/filepath"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	gitm "github.com/felipemarques-rec/sandcode/internal/git"
	"github.com/felipemarques-rec/sandcode/internal/langfuse"
	"github.com/felipemarques-rec/sandcode/internal/planner"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
)

func TestDAGRun_JudgeSpanCarriesRealScore(t *testing.T) {
	repo := initRepo(t)
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	defer func() { _ = tp.Shutdown(context.Background()) }()
	lf := langfuse.NewProviderForTest(tp)

	ctx, root := lf.SpanBrain(context.Background(), "execute")
	plan := planner.TaskDAG{Nodes: []planner.Node{
		{ID: "a", Prompt: "approach a"},
		{ID: "b", Prompt: "approach b"},
	}}
	dopts := DAGOptions{
		Prompt:         "solve",
		CWD:            repo,
		SandboxImage:   "ignored-by-nosandbox",
		SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", t.Name()),
		Strategy:       gitm.StrategyBranch,
		RunID:          "dagscore",
		Plan:           plan,
		Judge:          &fakeJudge{name: "fake"},
		Langfuse:       lf,
	}
	events, await, err := DAGRun(ctx, sandbox.NewNoSandboxProvider(), &fakeAgent{script: `echo x > x.txt`}, &noopAuth{}, dopts)
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	_ = await()
	root.End()

	var found bool
	var score float64
	for _, s := range exp.GetSpans() {
		if s.Name != "sandcode.judge.ranking" {
			continue
		}
		for _, a := range s.Attributes {
			if string(a.Key) == "evaluation.score" {
				found = true
				score = a.Value.AsFloat64()
			}
		}
	}
	if !found {
		t.Fatalf("sandcode.judge.ranking span missing evaluation.score attribute")
	}
	if score <= 0 {
		t.Fatalf("evaluation.score = %v, want > 0 (real judge score, not the old hardcoded 0)", score)
	}
}

func TestDAGRun_SynthesizerSpanNestsUnderDag(t *testing.T) {
	repo := initRepo(t)
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	defer func() { _ = tp.Shutdown(context.Background()) }()
	lf := langfuse.NewProviderForTest(tp)

	ctx, root := lf.SpanBrain(context.Background(), "execute")

	// Two independent roots → 2 successful chains → judge ranks them →
	// synthesizer consolidates (default: case, Synthesizer not disabled).
	plan := planner.TaskDAG{Nodes: []planner.Node{
		{ID: "a", Prompt: "approach a"},
		{ID: "b", Prompt: "approach b"},
	}}
	dopts := DAGOptions{
		Prompt:         "solve",
		CWD:            repo,
		SandboxImage:   "ignored-by-nosandbox",
		SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", t.Name()),
		Strategy:       gitm.StrategyBranch,
		RunID:          "dagsyn",
		Plan:           plan,
		Judge:          &fakeJudge{name: "fake"},
		Langfuse:       lf,
	}
	events, await, err := DAGRun(ctx, sandbox.NewNoSandboxProvider(), &fakeAgent{script: `echo x > x.txt`}, &noopAuth{}, dopts)
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	_ = await()
	root.End()

	var dagID, synParent string
	synSpans := 0
	for _, s := range exp.GetSpans() {
		switch s.Name {
		case "sandcode.brain.dag":
			dagID = s.SpanContext.SpanID().String()
		case "sandcode.brain.dag.synthesizer":
			synSpans++
			synParent = s.Parent.SpanID().String()
		}
	}
	if synSpans != 1 {
		t.Fatalf("want exactly 1 sandcode.brain.dag.synthesizer span, got %d", synSpans)
	}
	if dagID == "" {
		t.Fatalf("missing sandcode.brain.dag span")
	}
	if synParent != dagID {
		t.Fatalf("synthesizer span parent=%q, want dag span=%q", synParent, dagID)
	}
}

func TestDAGRun_MultiRootJudgeSpanNestsUnderDag(t *testing.T) {
	repo := initRepo(t)
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	defer func() { _ = tp.Shutdown(context.Background()) }()
	lf := langfuse.NewProviderForTest(tp)

	ctx, root := lf.SpanBrain(context.Background(), "execute")

	// Two independent roots → 2 chains → judge ranks them.
	plan := planner.TaskDAG{Nodes: []planner.Node{
		{ID: "a", Prompt: "approach a"},
		{ID: "b", Prompt: "approach b"},
	}}
	dopts := DAGOptions{
		Prompt:         "solve",
		CWD:            repo,
		SandboxImage:   "ignored-by-nosandbox",
		SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", t.Name()),
		Strategy:       gitm.StrategyBranch,
		RunID:          "dagjudge",
		Plan:           plan,
		Judge:          &fakeJudge{name: "fake"},
		Langfuse:       lf,
	}
	events, await, err := DAGRun(ctx, sandbox.NewNoSandboxProvider(), &fakeAgent{script: `echo x > x.txt`}, &noopAuth{}, dopts)
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	_ = await()
	root.End()

	var dagID, judgeParent string
	judgeSpans := 0
	for _, s := range exp.GetSpans() {
		switch s.Name {
		case "sandcode.brain.dag":
			dagID = s.SpanContext.SpanID().String()
		case "sandcode.judge.ranking":
			judgeSpans++
			judgeParent = s.Parent.SpanID().String()
		}
	}
	if judgeSpans != 1 {
		t.Fatalf("want exactly 1 sandcode.judge.ranking span, got %d", judgeSpans)
	}
	if dagID == "" {
		t.Fatalf("missing sandcode.brain.dag span")
	}
	if judgeParent != dagID {
		t.Fatalf("judge span parent=%q, want dag span=%q", judgeParent, dagID)
	}
}

func TestDAGRun_GroupingAndChainSpansNestUnderRoot(t *testing.T) {
	repo := initRepo(t)
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	defer func() { _ = tp.Shutdown(context.Background()) }()
	lf := langfuse.NewProviderForTest(tp)

	ctx, root := lf.SpanBrain(context.Background(), "execute")

	plan := planner.TaskDAG{Nodes: []planner.Node{
		{ID: "n1", Prompt: "step one"},
		{ID: "n2", Prompt: "step two", DependsOn: []string{"n1"}},
	}}
	dopts := DAGOptions{
		Prompt:         "build it",
		CWD:            repo,
		SandboxImage:   "ignored-by-nosandbox",
		SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", t.Name()),
		Strategy:       gitm.StrategyMergeToHead,
		RunID:          "dagtrace",
		Plan:           plan,
		Langfuse:       lf,
	}
	events, await, err := DAGRun(ctx, sandbox.NewNoSandboxProvider(), &fakeAgent{script: `echo node`}, &noopAuth{}, dopts)
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	_ = await()
	root.End()

	var rootID, dagID, dagParent string
	chainSpans := 0
	var chainParents []string
	for _, s := range exp.GetSpans() {
		switch s.Name {
		case "sandcode.brain.execute":
			rootID = s.SpanContext.SpanID().String()
		case "sandcode.brain.dag":
			dagID = s.SpanContext.SpanID().String()
			dagParent = s.Parent.SpanID().String()
		case "sandcode.brain.dag.chain":
			chainSpans++
			chainParents = append(chainParents, s.Parent.SpanID().String())
		}
	}
	if rootID == "" {
		t.Fatalf("missing sandcode.brain.execute root span")
	}
	if dagID == "" {
		t.Fatalf("missing sandcode.brain.dag span")
	}
	if dagParent != rootID {
		t.Fatalf("dag span parent=%q, want execute root=%q", dagParent, rootID)
	}
	if chainSpans != 1 {
		t.Fatalf("want exactly 1 sandcode.brain.dag.chain span (single-root plan), got %d", chainSpans)
	}
	for i, p := range chainParents {
		if p != dagID {
			t.Errorf("chain span #%d parent=%q, want dag span=%q", i, p, dagID)
		}
	}
}
