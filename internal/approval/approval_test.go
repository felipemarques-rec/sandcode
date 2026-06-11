package approval

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestRegistry_ResolveWakesWaiter(t *testing.T) {
	r := NewRegistry()
	done := make(chan Decision, 1)
	go func() {
		d, _ := r.RequestApproval(context.Background(), Request{RunID: "run1"})
		done <- d
	}()
	waitForPending(t, r, "run1")
	if !r.Resolve("run1", Decision{Approved: true, Approver: "alice"}) {
		t.Fatal("Resolve should find the waiter")
	}
	select {
	case d := <-done:
		if !d.Approved || d.Approver != "alice" {
			t.Fatalf("decision = %+v, want approved by alice", d)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter never woke")
	}
}

func TestRegistry_ContextCancelReturnsErr(t *testing.T) {
	r := NewRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { waitForPending(t, r, "run2"); cancel() }()
	_, err := r.RequestApproval(ctx, Request{RunID: "run2"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(r.Pending()) != 0 {
		t.Fatal("waiter should be cleaned up after cancel")
	}
}

func TestRegistry_ResolveNoWaiter(t *testing.T) {
	r := NewRegistry()
	if r.Resolve("nobody", Decision{Approved: true}) {
		t.Fatal("Resolve with no waiter should return false")
	}
}

func TestRegistry_DoubleWaitSameRun(t *testing.T) {
	r := NewRegistry()
	go func() { r.RequestApproval(context.Background(), Request{RunID: "dup"}) }()
	waitForPending(t, r, "dup")
	_, err := r.RequestApproval(context.Background(), Request{RunID: "dup"})
	if !errors.Is(err, ErrAlreadyWaiting) {
		t.Fatalf("second wait err = %v, want ErrAlreadyWaiting", err)
	}
}

func TestRegistry_ConcurrentWaitResolve(t *testing.T) {
	r := NewRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		id := string(rune('a'+i%26)) + string(rune('0'+i/26))
		wg.Add(1)
		go func() {
			defer wg.Done()
			go func() {
				for !r.Resolve(id, Decision{Approved: true}) {
					time.Sleep(time.Millisecond)
				}
			}()
			r.RequestApproval(context.Background(), Request{RunID: id})
		}()
	}
	wg.Wait()
}

func waitForPending(t *testing.T, r *Registry, runID string) {
	t.Helper()
	for i := 0; i < 1000; i++ {
		for _, p := range r.Pending() {
			if p == runID {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("run %q never registered", runID)
}
