package kernel

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/architect"
	"github.com/felipemarques-rec/sandcode/internal/event"
	"github.com/felipemarques-rec/sandcode/internal/planner"
)

// fakeArchitect is a deterministic stand-in for architect.Architect.
type fakeArchitect struct {
	mu      sync.Mutex
	calls   []architect.DesignRequest
	wantErr error
	wantAP  architect.ArchPlan
}

func (f *fakeArchitect) Design(_ context.Context, req architect.DesignRequest) (architect.ArchPlan, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.mu.Unlock()
	if f.wantErr != nil {
		return architect.ArchPlan{}, f.wantErr
	}
	return f.wantAP, nil
}

func (f *fakeArchitect) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// High-complexity prompt → architect AND planner both run; the captured
// planner prompt proves the guidance was threaded into the working prompt.
func TestProcess_ArchitectGuidanceThreadedToPlanner(t *testing.T) {
	br := openTestBrain(t)
	bus := event.NewLocalBus()
	t.Cleanup(func() { _ = bus.Close() })
	rec := newRecorder()
	bus.Subscribe(event.RunArchitected, rec.handler())

	fa := &fakeArchitect{wantAP: architect.ArchPlan{Approach: "layered cache", Files: []string{"cache.go"}, Risks: []string{"stale reads"}, Architect: "llm:x"}}
	fp := &fakePlanner{wantDAG: planner.TaskDAG{Nodes: []planner.Node{{ID: "root", Prompt: "x"}}}}
	k := New(br, WithBus(bus), WithArchitect(fa), WithPlanner(fp))

	res := k.Process(context.Background(), ProcessRequest{
		Prompt: "add distributed cache invalidation",
		CWD:    t.TempDir(),
		RunID:  "arch-001",
	})

	if fa.callCount() != 1 {
		t.Fatalf("architect called %d times, want 1", fa.callCount())
	}
	if fp.callCount() != 1 {
		t.Fatalf("planner called %d times, want 1", fp.callCount())
	}
	got := fp.calls[0]
	if !strings.Contains(got, "Architecture guidance") || !strings.Contains(got, "layered cache") {
		t.Fatalf("planner did not receive guided prompt: %q", got)
	}
	if !strings.HasSuffix(got, "add distributed cache invalidation") {
		t.Fatalf("raw prompt must remain at the end: %q", got)
	}
	// The enricher is the second consumer of the working prompt — prove the
	// guidance reached it too (the enricher embeds the prompt under "## Task").
	if !strings.Contains(res.EnrichedPrompt, "Architecture guidance") {
		t.Fatalf("enricher did not receive guided prompt: %q", res.EnrichedPrompt)
	}
	// The DesignRequest must carry the flattened classification (the reason
	// architect does not import internal/brain). "add distributed cache
	// invalidation" classifies convergent + high.
	if fa.calls[0].ProblemType != "convergent" || fa.calls[0].Complexity != "high" {
		t.Fatalf("DesignRequest not flattened: %+v", fa.calls[0])
	}
	if res.Arch == nil || res.Arch.Approach != "layered cache" {
		t.Fatalf("ProcessResult.Arch missing: %+v", res.Arch)
	}
	if !rec.has(event.RunArchitected) {
		t.Fatalf("run.architected not emitted")
	}
}

// Divergent + LOW complexity → architect runs even though the planner gate
// (high-only) does not fire.
func TestProcess_ArchitectFiresOnDivergentLow(t *testing.T) {
	br := openTestBrain(t)
	bus := event.NewLocalBus()
	t.Cleanup(func() { _ = bus.Close() })
	rec := newRecorder()
	bus.Subscribe(event.RunArchitected, rec.handler())

	fa := &fakeArchitect{wantAP: architect.ArchPlan{Approach: "spike then choose", Architect: "llm:x"}}
	fp := &fakePlanner{}
	k := New(br, WithBus(bus), WithArchitect(fa), WithPlanner(fp))

	res := k.Process(context.Background(), ProcessRequest{
		Prompt: "explore options",
		CWD:    t.TempDir(),
		RunID:  "arch-002",
	})

	if fa.callCount() != 1 {
		t.Fatalf("architect not called on divergent-low, got %d", fa.callCount())
	}
	if fp.callCount() != 0 {
		t.Fatalf("planner must stay gated (high-only) on low complexity, got %d", fp.callCount())
	}
	if res.Arch == nil {
		t.Fatalf("Arch nil on divergent-low")
	}
	if !rec.has(event.RunArchitected) {
		t.Fatalf("run.architected not emitted on divergent-low")
	}
}

// Convergent + LOW → architect does NOT fire; byte-identical.
func TestProcess_ArchitectGatedOffForConvergentLow(t *testing.T) {
	br := openTestBrain(t)
	bus := event.NewLocalBus()
	t.Cleanup(func() { _ = bus.Close() })
	rec := newRecorder()
	bus.Subscribe(event.RunArchitected, rec.handler())

	fa := &fakeArchitect{wantAP: architect.ArchPlan{Approach: "noop"}}
	k := New(br, WithBus(bus), WithArchitect(fa))

	res := k.Process(context.Background(), ProcessRequest{
		Prompt: "fix typo in README",
		CWD:    t.TempDir(),
		RunID:  "arch-003",
	})

	if fa.callCount() != 0 {
		t.Fatalf("architect fired on convergent-low, want 0")
	}
	if res.Arch != nil {
		t.Fatalf("Arch must be nil when gate off: %+v", res.Arch)
	}
	if rec.has(event.RunArchitected) {
		t.Fatalf("run.architected emitted when gate off")
	}
}

// No WithArchitect → planner receives the RAW prompt (byte-identical threading).
func TestProcess_NilArchitect_RawPromptToPlanner(t *testing.T) {
	br := openTestBrain(t)
	fp := &fakePlanner{wantDAG: planner.TaskDAG{Nodes: []planner.Node{{ID: "root", Prompt: "x"}}}}
	k := New(br, WithPlanner(fp))

	res := k.Process(context.Background(), ProcessRequest{
		Prompt: "add distributed cache invalidation",
		CWD:    t.TempDir(),
		RunID:  "arch-004",
	})

	if fp.callCount() != 1 {
		t.Fatalf("planner not invoked")
	}
	if fp.calls[0] != "add distributed cache invalidation" {
		t.Fatalf("planner must get raw prompt with nil architect: %q", fp.calls[0])
	}
	if res.Arch != nil {
		t.Fatalf("Arch must be nil with no architect")
	}
}

// Architect error → graceful: no event, Arch nil, planner gets raw prompt,
// classification still produced.
func TestProcess_ArchitectErrorDegrades(t *testing.T) {
	br := openTestBrain(t)
	bus := event.NewLocalBus()
	t.Cleanup(func() { _ = bus.Close() })
	rec := newRecorder()
	bus.Subscribe(event.RunArchitected, rec.handler())
	bus.Subscribe(event.RunClassified, rec.handler())

	fa := &fakeArchitect{wantErr: errors.New("api unreachable")}
	fp := &fakePlanner{wantDAG: planner.TaskDAG{Nodes: []planner.Node{{ID: "root", Prompt: "x"}}}}
	k := New(br, WithBus(bus), WithArchitect(fa), WithPlanner(fp))

	res := k.Process(context.Background(), ProcessRequest{
		Prompt: "redesign distributed cache layer",
		CWD:    t.TempDir(),
		RunID:  "arch-005",
	})

	if fa.callCount() != 1 {
		t.Fatalf("architect not invoked")
	}
	if rec.has(event.RunArchitected) {
		t.Fatalf("run.architected emitted despite architect error")
	}
	if res.Arch != nil {
		t.Fatalf("Arch must be nil on error")
	}
	if fp.calls[0] != "redesign distributed cache layer" {
		t.Fatalf("planner must get raw prompt on architect error: %q", fp.calls[0])
	}
	rec.requireType(t, event.RunClassified)
}
