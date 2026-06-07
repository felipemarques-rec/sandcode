package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/event"
)

func TestPriority_String(t *testing.T) {
	cases := map[Priority]string{
		PriorityLow:      "low",
		PriorityNormal:   "normal",
		PriorityHigh:     "high",
		PriorityCritical: "critical",
		Priority(99):     "normal", // unknown clamps to normal
	}
	for p, want := range cases {
		if got := p.String(); got != want {
			t.Fatalf("Priority(%d).String()=%q want %q", p, got, want)
		}
	}
}

func TestParsePriority(t *testing.T) {
	ok := map[string]Priority{
		"":         PriorityNormal,
		"low":      PriorityLow,
		"normal":   PriorityNormal,
		"high":     PriorityHigh,
		"critical": PriorityCritical,
	}
	for in, want := range ok {
		got, err := ParsePriority(in)
		if err != nil || got != want {
			t.Fatalf("ParsePriority(%q)=(%v,%v) want (%v,nil)", in, got, err, want)
		}
	}
	if _, err := ParsePriority("urgent"); err == nil {
		t.Fatal("ParsePriority(urgent) err=nil, want error")
	}
}

func TestSentinelsAreDistinct(t *testing.T) {
	if errors.Is(ErrQueueFull, ErrStopped) || errors.Is(ErrStopped, ErrDuplicate) {
		t.Fatal("sentinels must be distinct error values")
	}
}

// recBus records every published event. event.NewLocalBus is the
// production in-memory bus; "*" subscribes to all types. Handler is
// func(ctx, Event) error (see internal/event/bus.go); the returned
// Subscription is intentionally ignored (bus lives for the test).
func newRecBus(t *testing.T) (*event.LocalBus, func() []event.Event) {
	t.Helper()
	bus := event.NewLocalBus()
	var mu sync.Mutex
	var got []event.Event
	_ = bus.Subscribe("*", func(_ context.Context, e event.Event) error {
		mu.Lock()
		got = append(got, e)
		mu.Unlock()
		return nil
	})
	return bus, func() []event.Event {
		mu.Lock()
		defer mu.Unlock()
		out := make([]event.Event, len(got))
		copy(out, got)
		return out
	}
}

func countType(evs []event.Event, typ event.Type) int {
	n := 0
	for _, e := range evs {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func TestPool_SubmitDispatchesAndEmits(t *testing.T) {
	bus, snap := newRecBus(t)
	var ran atomic.Int32
	s := New(Config{PoolSize: 2, QueueCap: 8},
		func(ctx context.Context, runID string) error { ran.Add(1); return nil },
		bus)
	s.Start()
	defer s.Stop(context.Background())

	if err := s.Submit("r1", PriorityNormal); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitFor(t, func() bool { return ran.Load() == 1 })

	evs := snap()
	if countType(evs, event.RunScheduled) != 1 {
		t.Fatalf("want 1 run.scheduled, got %d", countType(evs, event.RunScheduled))
	}
	if countType(evs, event.RunDequeued) != 1 {
		t.Fatalf("want 1 run.dequeued, got %d", countType(evs, event.RunDequeued))
	}
}

func TestPool_RunScheduledPayloadIsZeroBased(t *testing.T) {
	bus, snap := newRecBus(t)
	release := make(chan struct{})
	s := New(Config{PoolSize: 1, QueueCap: 8},
		func(ctx context.Context, runID string) error { <-release; return nil },
		bus)
	s.Start()
	defer func() { close(release); s.Stop(context.Background()) }()

	mustSubmit(t, s, "occupy", PriorityNormal) // takes the only slot
	waitFor(t, func() bool { st, ok := s.Status("occupy"); return ok && st.State == "running" })
	mustSubmit(t, s, "first", PriorityHigh) // front of the (empty) queue

	var raw []byte
	for _, e := range snap() {
		if e.Type == event.RunScheduled && e.RunID == "first" {
			raw = e.Payload
		}
	}
	if raw == nil {
		t.Fatal("no RunScheduled event for 'first'")
	}
	var p struct {
		Priority      string `json:"priority"`
		QueuePosition int    `json:"queue_position"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if p.Priority != "high" {
		t.Fatalf("priority=%q want high", p.Priority)
	}
	// 0-based by design (spec §"Position convention"): front of queue == 0.
	if p.QueuePosition != 0 {
		t.Fatalf("queue_position=%d want 0 (0-based; front of queue)", p.QueuePosition)
	}
}

func TestPool_QueueFull(t *testing.T) {
	bus, _ := newRecBus(t)
	release := make(chan struct{})
	s := New(Config{PoolSize: 1, QueueCap: 1},
		func(ctx context.Context, runID string) error { <-release; return nil },
		bus)
	s.Start()
	defer func() { close(release); s.Stop(context.Background()) }()

	// 1 occupies the single pool slot, 1 fills the QueueCap=1 queue.
	mustSubmit(t, s, "running", PriorityNormal)
	waitFor(t, func() bool { st, ok := s.Status("running"); return ok && st.State == "running" })
	mustSubmit(t, s, "queued", PriorityNormal)

	if err := s.Submit("rejected", PriorityNormal); err != ErrQueueFull {
		t.Fatalf("Submit over cap: err=%v want ErrQueueFull", err)
	}
	if _, ok := s.Status("rejected"); ok {
		t.Fatal("rejected run must not be tracked")
	}
}

// TestPool_PriorityPreemption verifies the single worker drains the
// queue in (priority desc, FIFO) order. "blocker" holds the only slot
// via gate until low/crit/norm are all queued; closing gate lets the
// worker drain them. NOTE: this test must NOT use Stop to drain the
// queue — Stop CANCELS queued runs (see TestPool_StopCancelsQueuedAndDrains
// and the spec §Shutdown). All four runs complete on their own; Stop is
// deferred only for cleanup (queue already empty by then).
func TestPool_PriorityPreemption(t *testing.T) {
	bus, _ := newRecBus(t)
	gate := make(chan struct{}) // only "blocker" waits on this
	var order []string
	var mu sync.Mutex
	var doneWG sync.WaitGroup
	s := New(Config{PoolSize: 1, QueueCap: 8},
		func(ctx context.Context, runID string) error {
			if runID == "blocker" {
				<-gate // hold the single slot until the queue is loaded
			}
			mu.Lock()
			order = append(order, runID)
			mu.Unlock()
			doneWG.Done()
			return nil
		}, bus)
	s.Start()
	defer s.Stop(context.Background())

	doneWG.Add(4)
	mustSubmit(t, s, "blocker", PriorityNormal)
	waitFor(t, func() bool { st, ok := s.Status("blocker"); return ok && st.State == "running" })
	mustSubmit(t, s, "low", PriorityLow)
	mustSubmit(t, s, "crit", PriorityCritical)
	mustSubmit(t, s, "norm", PriorityNormal)
	close(gate)   // blocker returns; worker then drains queue by priority
	doneWG.Wait() // all 4 ran — no Stop-driven cancellation

	mu.Lock()
	defer mu.Unlock()
	// blocker ran first (already dispatched); then crit > norm > low
	// (Critical first; among the rest Normal > Low).
	want := []string{"blocker", "crit", "norm", "low"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("dispatch order=%v want %v", order, want)
		}
	}
}

func TestPool_CancelQueuedAndRunning(t *testing.T) {
	bus, snap := newRecBus(t)
	release := make(chan struct{})
	s := New(Config{PoolSize: 1, QueueCap: 8},
		func(ctx context.Context, runID string) error { <-release; return nil },
		bus)
	s.Start()
	defer func() { close(release); s.Stop(context.Background()) }()

	mustSubmit(t, s, "running", PriorityNormal)
	waitFor(t, func() bool { st, ok := s.Status("running"); return ok && st.State == "running" })
	mustSubmit(t, s, "waiting", PriorityNormal)

	if !s.Cancel("waiting") {
		t.Fatal("Cancel(waiting) = false, want true (it was queued)")
	}
	if s.Cancel("running") {
		t.Fatal("Cancel(running) = true, want false (already dispatched)")
	}
	if s.Cancel("nope") {
		t.Fatal("Cancel(unknown) = true, want false")
	}
	if countType(snap(), event.RunCancelled) != 1 {
		t.Fatalf("want 1 run.cancelled, got %d", countType(snap(), event.RunCancelled))
	}
}

func TestPool_StopCancelsQueuedAndDrains(t *testing.T) {
	bus, snap := newRecBus(t)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	s := New(Config{PoolSize: 1, QueueCap: 8},
		func(ctx context.Context, runID string) error {
			started <- struct{}{}
			<-release
			return nil
		}, bus)
	s.Start()

	mustSubmit(t, s, "inflight", PriorityNormal)
	<-started
	mustSubmit(t, s, "q1", PriorityNormal)
	mustSubmit(t, s, "q2", PriorityNormal)

	go func() { time.Sleep(20 * time.Millisecond); close(release) }()
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// q1, q2 were queued -> cancelled on Stop. inflight drained normally.
	if c := countType(snap(), event.RunCancelled); c != 2 {
		t.Fatalf("want 2 run.cancelled from Stop, got %d", c)
	}
	if err := s.Submit("after", PriorityNormal); err != ErrStopped {
		t.Fatalf("Submit after Stop: err=%v want ErrStopped", err)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop must be a nil no-op, got %v", err)
	}
}

func TestPool_PanicIsolation(t *testing.T) {
	bus, _ := newRecBus(t)
	var ok2 atomic.Bool
	s := New(Config{PoolSize: 1, QueueCap: 8},
		func(ctx context.Context, runID string) error {
			if runID == "boom" {
				panic("intentional")
			}
			ok2.Store(true)
			return nil
		}, bus)
	s.Start()
	defer s.Stop(context.Background())

	mustSubmit(t, s, "boom", PriorityNormal)
	mustSubmit(t, s, "ok", PriorityNormal)
	waitFor(t, func() bool { return ok2.Load() }) // pool survived the panic
}

func TestPool_DuplicateSubmit(t *testing.T) {
	bus, _ := newRecBus(t)
	release := make(chan struct{})
	s := New(Config{PoolSize: 1, QueueCap: 8},
		func(ctx context.Context, runID string) error { <-release; return nil },
		bus)
	s.Start()
	defer func() { close(release); s.Stop(context.Background()) }()

	mustSubmit(t, s, "dup", PriorityNormal)
	if err := s.Submit("dup", PriorityNormal); err != ErrDuplicate {
		t.Fatalf("duplicate Submit: err=%v want ErrDuplicate", err)
	}
}

func TestPool_RaceContention(t *testing.T) {
	bus, _ := newRecBus(t)
	var done atomic.Int64
	s := New(Config{PoolSize: 8, QueueCap: 1024},
		func(ctx context.Context, runID string) error { done.Add(1); return nil },
		bus)
	s.Start()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = s.Submit("r-"+strconv.Itoa(g)+"-"+strconv.Itoa(j), PriorityNormal)
			}
		}(i)
	}
	wg.Wait()
	s.Stop(context.Background())
	if done.Load() == 0 {
		t.Fatal("no runs dispatched under contention")
	}
}

// --- small test helpers ---

func mustSubmit(t *testing.T, s Scheduler, id string, p Priority) {
	t.Helper()
	if err := s.Submit(id, p); err != nil {
		t.Fatalf("Submit(%s): %v", id, err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within 2s")
}

func TestPool_DispatchOrderDeterministic(t *testing.T) {
	// Same submit sequence (mixed priorities) → same dispatch order,
	// every iteration. PoolSize 1 + a gate held until the full set is
	// queued, so the single worker drains the heap in a fully
	// determined (priority desc, submitSeq asc) order.
	type sub struct {
		id string
		p  Priority
	}
	seq := []sub{
		{"a", PriorityNormal}, {"b", PriorityLow}, {"c", PriorityCritical},
		{"d", PriorityNormal}, {"e", PriorityHigh}, {"f", PriorityLow},
		{"g", PriorityCritical}, {"h", PriorityNormal},
	}
	var want []string
	for iter := 0; iter < 25; iter++ {
		bus, _ := newRecBus(t)
		gate := make(chan struct{})
		var order []string
		var mu sync.Mutex
		var wg sync.WaitGroup
		s := New(Config{PoolSize: 1, QueueCap: 64},
			func(ctx context.Context, runID string) error {
				if runID == "gateholder" {
					<-gate
				} else {
					mu.Lock()
					order = append(order, runID)
					mu.Unlock()
				}
				wg.Done()
				return nil
			}, bus)
		s.Start()
		wg.Add(1 + len(seq))
		mustSubmit(t, s, "gateholder", PriorityCritical)
		waitFor(t, func() bool { st, ok := s.Status("gateholder"); return ok && st.State == "running" })
		for _, x := range seq {
			mustSubmit(t, s, x.id, x.p)
		}
		close(gate)
		wg.Wait()
		_ = s.Stop(context.Background())

		mu.Lock()
		got := append([]string(nil), order...)
		mu.Unlock()
		if iter == 0 {
			want = got
			// Sanity: priority desc, FIFO within priority.
			exp := []string{"c", "g", "e", "a", "d", "h", "b", "f"}
			if len(want) != len(exp) {
				t.Fatalf("len(order)=%d want %d (%v)", len(want), len(exp), want)
			}
			for i := range exp {
				if want[i] != exp[i] {
					t.Fatalf("iter0 order=%v want %v", want, exp)
				}
			}
			continue
		}
		if len(got) != len(want) {
			t.Fatalf("iter %d len=%d want %d", iter, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("iter %d order=%v != iter0 %v (non-deterministic)", iter, got, want)
			}
		}
	}
}
