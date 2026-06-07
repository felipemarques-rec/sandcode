package kernel

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/event"
	"github.com/felipemarques-rec/sandcode/internal/planner"
	"github.com/felipemarques-rec/sandcode/internal/strategy"
)

func TestNoopTracer_StartReturnsCtxAndNoPanic(t *testing.T) {
	var tr Tracer = noopTracer{}
	base := context.Background()
	ctx, end := tr.Start(base, "x", nil)
	if ctx != base {
		t.Fatal("noop tracer should return ctx unchanged")
	}
	end(nil) // must not panic
}

func TestNoopTracer_TraceIDEmpty(t *testing.T) {
	if got := (noopTracer{}).TraceID(context.Background()); got != "" {
		t.Fatalf("noop TraceID = %q, want empty", got)
	}
}

type sentinelTracer struct{ noopTracer }

func TestWithTracer_SetsField(t *testing.T) {
	k := New(nil, WithTracer(sentinelTracer{}))
	if _, ok := k.tracer.(sentinelTracer); !ok {
		t.Fatalf("WithTracer did not wire the provided tracer; got %T", k.tracer)
	}
}

type recTracer struct {
	mu    sync.Mutex
	spans []string
	id    string
}

func (r *recTracer) Start(ctx context.Context, name string, _ map[string]string) (context.Context, func(error)) {
	r.mu.Lock()
	r.spans = append(r.spans, name)
	r.mu.Unlock()
	return ctx, func(error) {}
}
func (r *recTracer) TraceID(context.Context) string { return r.id }

type recBus struct {
	mu  sync.Mutex
	evs []event.Event
}

func (b *recBus) Publish(_ context.Context, e event.Event) error {
	b.mu.Lock()
	b.evs = append(b.evs, e)
	b.mu.Unlock()
	return nil
}
func (b *recBus) Subscribe(event.Type, event.Handler) event.Subscription { return nil }
func (b *recBus) Close() error                                           { return nil }

func TestProcess_EmitsStageSpans(t *testing.T) {
	tr := &recTracer{id: "trace-xyz"}
	k := New(nil, WithTracer(tr))
	k.Process(context.Background(), ProcessRequest{Prompt: "hello", RunID: "r1"})
	tr.mu.Lock()
	defer tr.mu.Unlock()
	found := false
	for _, s := range tr.spans {
		if s == "kernel.classify" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected kernel.classify span, got %v", tr.spans)
	}
}

func TestProcess_StampsTraceIDOnEvents(t *testing.T) {
	tr := &recTracer{id: "trace-xyz"}
	bus := &recBus{}
	k := New(nil, WithBus(bus), WithTracer(tr))
	k.Process(context.Background(), ProcessRequest{Prompt: "hello", RunID: "r1"})
	bus.mu.Lock()
	defer bus.mu.Unlock()
	if len(bus.evs) == 0 {
		t.Fatal("no events published")
	}
	for _, e := range bus.evs {
		if e.TraceID != "trace-xyz" {
			t.Fatalf("event %s TraceID = %q, want trace-xyz", e.Type, e.TraceID)
		}
	}
}

func TestProcess_NilTracerNoPanic(t *testing.T) {
	bus := &recBus{}
	k := New(nil, WithBus(bus)) // default noopTracer
	k.Process(context.Background(), ProcessRequest{Prompt: "x", RunID: "r1"})
	for _, e := range bus.evs {
		if e.TraceID != "" {
			t.Fatalf("noop tracer must leave TraceID empty, got %q", e.TraceID)
		}
	}
}

func TestProcess_AllStageSpansOrdered(t *testing.T) {
	br := openTestBrain(t)
	tr := &recTracer{id: "trace-all"}
	fp := &fakePlanner{wantDAG: planner.TaskDAG{Nodes: []planner.Node{
		{ID: "root", Prompt: "redesign the whole system architecture end to end"},
	}}}
	k := New(br, WithTracer(tr), WithPlanner(fp), WithSelector(strategy.New()))

	k.Process(context.Background(), ProcessRequest{
		Prompt: "redesign the whole system architecture end to end",
		RunID:  "r1",
	})

	tr.mu.Lock()
	defer tr.mu.Unlock()
	want := []string{"kernel.classify", "kernel.plan", "kernel.strategy", "kernel.enrich"}
	if !reflect.DeepEqual(tr.spans, want) {
		t.Fatalf("span order = %v, want %v", tr.spans, want)
	}
}
