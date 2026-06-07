package orchestrator

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/agent"
	"github.com/felipemarques-rec/sandcode/internal/event"
	gitm "github.com/felipemarques-rec/sandcode/internal/git"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
	"github.com/felipemarques-rec/sandcode/internal/store"
)

// scriptedAgent runs a pre-set shell script inside the worktree, identifying
// itself by name so we can verify per-agent isolation.
type scriptedAgent struct {
	name   string
	script string
}

func (s *scriptedAgent) Name() string { return s.name }
func (s *scriptedAgent) BuildCommand(opts agent.RunOptions) agent.Command {
	return agent.Command{Argv: []string{"sh", "-c", s.script}}
}
func (*scriptedAgent) ParseLine(line string) (agent.StreamEvent, bool) {
	if line == "" {
		return agent.StreamEvent{}, false
	}
	return agent.StreamEvent{Kind: agent.EventText, Text: line, Timestamp: time.Now()}, true
}
func (*scriptedAgent) AuthHints() agent.AuthHints { return agent.AuthHints{} }

func TestParallelRun_ThreeAgents_NoSandbox(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	db, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	agents := []agent.Provider{
		&scriptedAgent{name: "alice", script: `echo alice; echo "alice-was-here" > alice.txt`},
		&scriptedAgent{name: "bob", script: `echo bob; echo "bob-was-here" > bob.txt`},
		&scriptedAgent{name: "carol", script: `echo carol; echo "carol-was-here" > carol.txt`},
	}

	events, await, err := ParallelRun(ctx,
		sandbox.NewNoSandboxProvider(),
		&noopAuth{},
		ParallelOptions{
			Prompt:         "noop",
			CWD:            repo,
			SandboxImage:   "x",
			SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", "p", "0"),
			Strategy:       gitm.StrategyBranch,
			Agents:         agents,
			Store:          db,
			KeepWorktrees:  true, // keep so we can verify per-branch state
		},
	)
	if err != nil {
		t.Fatalf("ParallelRun: %v", err)
	}

	// Drain events; ensure each agent emitted at least one event.
	seen := map[string]int{}
	for ev := range events {
		seen[ev.Agent]++
	}

	pr := await()
	if len(pr.Sub) != 3 {
		t.Fatalf("expected 3 sub-results, got %d", len(pr.Sub))
	}
	for _, s := range pr.Sub {
		if s.Result.Status != "success" {
			t.Fatalf("%s status=%s err=%v", s.Agent, s.Result.Status, s.Result.Err)
		}
		if seen[s.Agent] == 0 {
			t.Fatalf("agent %s emitted no events", s.Agent)
		}
	}

	// Parent + 3 children persisted.
	parents, err := db.ListRuns(ctx, store.ListFilter{ParentID: ""})
	if err != nil {
		t.Fatal(err)
	}
	if len(parents) != 1 || parents[0].Agent != "parallel" {
		t.Fatalf("parents: %+v", parents)
	}
	children, err := db.ListRuns(ctx, store.ListFilter{ParentID: parents[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 3 {
		t.Fatalf("expected 3 children, got %d", len(children))
	}
}

func TestParallelRun_MaxConcurrency(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()

	// Each agent sleeps for ~150ms; with MaxConcurrency=1 they must serialize.
	mkAgent := func(n string) agent.Provider {
		return &scriptedAgent{name: n, script: `sleep 0.15; echo done`}
	}
	agents := []agent.Provider{mkAgent("a"), mkAgent("b"), mkAgent("c")}

	t0 := time.Now()
	_, await, err := ParallelRun(ctx,
		sandbox.NewNoSandboxProvider(),
		&noopAuth{},
		ParallelOptions{
			Prompt:         "noop",
			CWD:            repo,
			SandboxImage:   "x",
			SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", "mc", "0"),
			Strategy:       gitm.StrategyBranch,
			Agents:         agents,
			MaxConcurrency: 1,
			KeepWorktrees:  true,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	// drain via await without reading events (channel is buffered)
	pr := drainAndAwait(t, nil, await)
	elapsed := time.Since(t0)
	if elapsed < 350*time.Millisecond {
		t.Fatalf("MaxConcurrency=1 should serialize: only took %s", elapsed)
	}
	if len(pr.Sub) != 3 {
		t.Fatalf("expected 3, got %d", len(pr.Sub))
	}
}

// drainAndAwait drains the events channel and returns the result.
func drainAndAwait(t *testing.T, events <-chan SubEvent, await func() ParallelResult) ParallelResult {
	t.Helper()
	var wg sync.WaitGroup
	if events != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range events {
			}
		}()
	}
	res := await()
	wg.Wait()
	return res
}

// TestParallelRun_RefineRunsPerAgent confirms that ParallelOptions.Refine
// is forwarded to every child Run and the verify pipeline executes per
// agent (each in its own worktree). All three agents create `fix.txt`
// in their worktree; the shared VerifyCmd succeeds on the first attempt
// for each, so no refine.triggered events should fire.
func TestParallelRun_RefineRunsPerAgent(t *testing.T) {
	repo := initRepo(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	lb := event.NewLocalBus()
	defer lb.Close()
	rec := &recorder{}
	lb.Subscribe("*", rec.handle)

	agents := []agent.Provider{
		&scriptedAgent{name: "alice", script: `echo "alice" > fix.txt`},
		&scriptedAgent{name: "bob", script: `echo "bob" > fix.txt`},
		&scriptedAgent{name: "carol", script: `echo "carol" > fix.txt`},
	}

	events, await, err := ParallelRun(ctx,
		sandbox.NewNoSandboxProvider(),
		&noopAuth{},
		ParallelOptions{
			Prompt:         "noop",
			CWD:            repo,
			SandboxImage:   "ignored",
			SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", t.Name(), "0"),
			Strategy:       gitm.StrategyBranch,
			Agents:         agents,
			Store:          db,
			Bus:            lb,
			KeepWorktrees:  true,
			Refine: RefineOptions{
				Enabled:     true,
				VerifyCmd:   []string{"sh", "-c", "test -f fix.txt"},
				MaxAttempts: 3,
			},
		},
	)
	if err != nil {
		t.Fatalf("ParallelRun: %v", err)
	}
	pr := drainAndAwait(t, events, await)

	if len(pr.Sub) != 3 {
		t.Fatalf("expected 3 sub-results, got %d", len(pr.Sub))
	}
	for _, s := range pr.Sub {
		if s.Result.Status != "success" {
			t.Errorf("%s status=%s err=%v", s.Agent, s.Result.Status, s.Result.Err)
		}
		if s.Result.Attempts != 1 {
			t.Errorf("%s attempts=%d, want 1 (verify passes first try)",
				s.Agent, s.Result.Attempts)
		}
	}

	// Verify ran for each agent — exactly 3 verify.started events
	// (one per sub-run). Each must pair with a verify.passed.
	if got := rec.count(event.VerifyStarted); got != 3 {
		t.Errorf("verify.started fired %d times, want 3", got)
	}
	if got := rec.count(event.VerifyPassed); got != 3 {
		t.Errorf("verify.passed fired %d times, want 3", got)
	}
	// No refine triggered — first attempt's verify passes for all.
	if got := rec.count(event.RefineTriggered); got != 0 {
		t.Errorf("refine.triggered fired %d times, want 0", got)
	}
}
