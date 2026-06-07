package scheduler

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/event"
)

type sched struct {
	cfg    Config
	launch LaunchFunc
	bus    event.Bus

	mu      sync.Mutex
	cond    *sync.Cond
	queue   *pq
	runs    map[string]*entry
	seq     uint64
	stopped bool

	pool sync.WaitGroup
}

// New constructs a scheduler. launch is invoked once per admitted run on
// a pool goroutine; bus may be nil (events are then dropped). Call Start
// before Submit.
func New(cfg Config, launch LaunchFunc, bus event.Bus) Scheduler {
	s := &sched{
		cfg:    cfg,
		launch: launch,
		bus:    bus,
		queue:  &pq{},
		runs:   make(map[string]*entry),
	}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *sched) Start() {
	for i := 0; i < s.cfg.PoolSize; i++ {
		s.pool.Add(1)
		go s.worker()
	}
}

func (s *sched) Submit(runID string, p Priority) error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return ErrStopped
	}
	if _, dup := s.runs[runID]; dup {
		s.mu.Unlock()
		return ErrDuplicate
	}
	if s.queue.len() >= s.cfg.QueueCap {
		s.mu.Unlock()
		return ErrQueueFull
	}
	s.seq++
	e := &entry{
		runID:     runID,
		priority:  p,
		submitSeq: s.seq,
		enqueued:  time.Now(),
	}
	s.runs[runID] = e
	s.queue.push(e)
	pos := s.queue.positionOf(runID) // 0-based; consistent with RunStatus.Position
	s.cond.Signal()
	// Release the lock BEFORE emitting: bus.Publish invokes subscribers
	// synchronously and some may re-enter the scheduler (Status/Cancel).
	// Mirrors the out-of-lock emit in Cancel/Stop. NOT a defer-unlock —
	// emitting under s.mu risks subscriber-reentrancy deadlock.
	s.mu.Unlock()
	s.emit(event.RunScheduled, runID, runScheduledPayload{
		Priority:      p.String(),
		QueuePosition: pos,
	})
	return nil
}

func (s *sched) Cancel(runID string) bool {
	s.mu.Lock()
	e, ok := s.runs[runID]
	if !ok || e.running {
		s.mu.Unlock()
		return false
	}
	s.queue.remove(runID)
	delete(s.runs, runID)
	s.mu.Unlock()
	// out-of-lock emit (subscriber-reentrancy safety); see Submit
	s.emit(event.RunCancelled, runID, nil)
	return true
}

func (s *sched) Status(runID string) (RunStatus, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.runs[runID]
	if !ok {
		return RunStatus{}, false
	}
	st := RunStatus{Priority: e.priority, Enqueued: e.enqueued}
	if e.running {
		st.State = "running"
		st.Position = 0
	} else {
		st.State = "queued"
		// Invariant: a runs[] entry with running==false is always in the
		// heap, so positionOf returns >= 0 here (never the -1 not-found).
		st.Position = s.queue.positionOf(runID)
	}
	return st, true
}

func (s *sched) Stop(ctx context.Context) error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil
	}
	s.stopped = true
	// Cancel everything still queued (never started => safe to cancel).
	var cancelled []string
	for s.queue.len() > 0 {
		e := s.queue.pop()
		delete(s.runs, e.runID)
		cancelled = append(cancelled, e.runID)
	}
	s.cond.Broadcast() // wake idle workers so they observe stopped
	s.mu.Unlock()

	for _, id := range cancelled {
		// out-of-lock emit (subscriber-reentrancy safety); see Submit
		s.emit(event.RunCancelled, id, nil)
	}

	// On ctx timeout we return ctx.Err() but this waiter goroutine
	// keeps blocking on pool.Wait() until the (wedged) worker returns.
	// Deliberate: a stuck launch cannot be force-killed; the goroutine
	// is bounded by process lifetime and we are shutting down anyway.
	// Do NOT "fix" this into a synchronous wait — that reintroduces the
	// unbounded-shutdown hang Stop's ctx bound exists to prevent.
	done := make(chan struct{})
	go func() { s.pool.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// worker is one pool goroutine: pop highest-priority work, run it, repeat.
func (s *sched) worker() {
	defer s.pool.Done()
	for {
		s.mu.Lock()
		for s.queue.len() == 0 && !s.stopped {
			s.cond.Wait()
		}
		if s.stopped && s.queue.len() == 0 {
			s.mu.Unlock()
			return
		}
		e := s.queue.pop()
		e.running = true
		wait := time.Since(e.enqueued)
		s.mu.Unlock()

		s.emit(event.RunDequeued, e.runID, runDequeuedPayload{
			WaitMS: wait.Milliseconds(),
		})
		s.runOne(e)

		s.mu.Lock()
		delete(s.runs, e.runID)
		s.mu.Unlock()
	}
}

// runOne invokes launch with panic isolation so one bad run cannot drain
// a pool goroutine (a resilience the old goroutine-per-run path lacked).
func (s *sched) runOne(e *entry) {
	defer func() { _ = recover() }()
	_ = s.launch(context.Background(), e.runID)
}

func (s *sched) emit(typ event.Type, runID string, payload any) {
	if s.bus == nil {
		return
	}
	var raw []byte
	if payload != nil {
		raw, _ = json.Marshal(payload)
	}
	// event.New auto-generates ID + timestamp. Publish takes a ctx and
	// returns an error; scheduler emission is fire-and-forget (the bus
	// is the observation channel, not a control path).
	_ = s.bus.Publish(context.Background(), event.New(typ, runID, raw))
}

type runScheduledPayload struct {
	Priority      string `json:"priority"`
	QueuePosition int    `json:"queue_position"`
}

type runDequeuedPayload struct {
	WaitMS int64 `json:"wait_ms"`
}
