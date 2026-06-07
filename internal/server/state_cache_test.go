package server

import (
	"context"
	"strconv"
	"sync"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/event"
	"github.com/felipemarques-rec/sandcode/internal/runtime"
)

func TestStateCacheTracksRunFromBus(t *testing.T) {
	c := NewStateCache(0)
	bus := event.NewLocalBus()
	defer bus.Close()
	sub := c.Attach(bus)
	defer sub.Cancel()

	mustPublish(t, bus, event.New(event.RunSubmitted, "r1", nil))
	mustPublish(t, bus, event.New(event.SandboxCreated, "r1", nil))
	mustPublish(t, bus, event.New(event.AgentExecuting, "r1", nil))
	mustPublish(t, bus, event.New(event.AgentCompleted, "r1", nil))

	st, err := c.Get("r1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if st.Phase != runtime.PhaseAgentCompleted {
		t.Errorf("phase = %s, want %s", st.Phase, runtime.PhaseAgentCompleted)
	}
	// run.submitted, sandbox.created, agent.executing, agent.completed
	if st.EventCount != 4 {
		t.Errorf("event count = %d, want 4", st.EventCount)
	}
}

func TestStateCacheIgnoresEventsWithoutRunID(t *testing.T) {
	c := NewStateCache(0)
	bus := event.NewLocalBus()
	defer bus.Close()
	sub := c.Attach(bus)
	defer sub.Cancel()

	mustPublish(t, bus, event.New(event.RunSubmitted, "", nil))
	if c.Len() != 0 {
		t.Errorf("len = %d, want 0", c.Len())
	}
}

func TestStateCacheUnknownRun(t *testing.T) {
	c := NewStateCache(0)
	if _, err := c.Get("nope"); err != ErrUnknownRun {
		t.Errorf("err = %v, want ErrUnknownRun", err)
	}
}

func TestStateCacheList(t *testing.T) {
	c := NewStateCache(0)
	for i := 0; i < 3; i++ {
		c.apply(event.New(event.RunSubmitted, "r"+strconv.Itoa(i), nil))
	}
	got := c.List()
	if len(got) != 3 {
		t.Fatalf("list len = %d, want 3", len(got))
	}
	for i, st := range got {
		want := "r" + strconv.Itoa(i)
		if st.RunID != want {
			t.Errorf("list[%d].RunID = %s, want %s", i, st.RunID, want)
		}
	}
}

func TestStateCacheForget(t *testing.T) {
	c := NewStateCache(0)
	c.apply(event.New(event.RunSubmitted, "r1", nil))
	c.Forget("r1")
	if c.Len() != 0 {
		t.Errorf("len after Forget = %d, want 0", c.Len())
	}
	c.Forget("r1") // idempotent — must not panic
}

func TestStateCacheEvictsTerminalEntriesFirst(t *testing.T) {
	c := NewStateCache(2)

	// r1: brought to terminal (failed)
	c.apply(event.New(event.RunSubmitted, "r1", nil))
	c.apply(event.New(event.RunFailed, "r1", nil))

	// r2: still in flight at submitted
	c.apply(event.New(event.RunSubmitted, "r2", nil))

	// r3: pushes us over capacity → must evict the terminal entry (r1),
	// preserving the in-flight r2.
	c.apply(event.New(event.RunSubmitted, "r3", nil))

	if c.Len() != 2 {
		t.Fatalf("len = %d, want 2", c.Len())
	}
	if _, err := c.Get("r1"); err != ErrUnknownRun {
		t.Errorf("r1 should have been evicted (terminal preferred)")
	}
	if _, err := c.Get("r2"); err != nil {
		t.Errorf("r2 should still be present: %v", err)
	}
	if _, err := c.Get("r3"); err != nil {
		t.Errorf("r3 should be present: %v", err)
	}
}

func TestStateCacheEvictsOldestWhenNoTerminal(t *testing.T) {
	c := NewStateCache(2)
	c.apply(event.New(event.RunSubmitted, "r1", nil))
	c.apply(event.New(event.RunSubmitted, "r2", nil))
	c.apply(event.New(event.RunSubmitted, "r3", nil)) // evicts r1 (oldest)

	if _, err := c.Get("r1"); err != ErrUnknownRun {
		t.Errorf("r1 should have been evicted")
	}
	if _, err := c.Get("r2"); err != nil {
		t.Errorf("r2 should be present: %v", err)
	}
}

func TestStateCacheRace(t *testing.T) {
	c := NewStateCache(0)
	bus := event.NewLocalBus()
	defer bus.Close()
	sub := c.Attach(bus)
	defer sub.Cancel()

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				runID := "r" + strconv.Itoa(w) + "-" + strconv.Itoa(i)
				_ = bus.Publish(context.Background(), event.New(event.RunSubmitted, runID, nil))
				_ = bus.Publish(context.Background(), event.New(event.SandboxCreated, runID, nil))
				_, _ = c.Get(runID)
			}
		}(w)
	}
	wg.Wait()
}

func mustPublish(t *testing.T, bus event.Bus, ev event.Event) {
	t.Helper()
	if err := bus.Publish(context.Background(), ev); err != nil {
		t.Fatalf("publish: %v", err)
	}
}
