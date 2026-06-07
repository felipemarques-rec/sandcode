package scheduler

import "testing"

func TestPQ_PriorityThenFIFO(t *testing.T) {
	pq := &pq{}
	// submitSeq encodes arrival order; higher Priority must pop first,
	// ties broken by lower submitSeq (stable FIFO within a priority).
	pq.push(&entry{runID: "n1", priority: PriorityNormal, submitSeq: 1})
	pq.push(&entry{runID: "c1", priority: PriorityCritical, submitSeq: 2})
	pq.push(&entry{runID: "n2", priority: PriorityNormal, submitSeq: 3})
	pq.push(&entry{runID: "h1", priority: PriorityHigh, submitSeq: 4})
	pq.push(&entry{runID: "c2", priority: PriorityCritical, submitSeq: 5})

	var got []string
	for pq.len() > 0 {
		got = append(got, pq.pop().runID)
	}
	want := []string{"c1", "c2", "h1", "n1", "n2"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order: got %v, want %v", got, want)
		}
	}
}

func TestPQ_PositionOf(t *testing.T) {
	pq := &pq{}
	pq.push(&entry{runID: "a", priority: PriorityNormal, submitSeq: 1})
	pq.push(&entry{runID: "b", priority: PriorityCritical, submitSeq: 2})
	// b (Critical) is ahead of a (Normal): position is 0-based pop order.
	if p := pq.positionOf("b"); p != 0 {
		t.Fatalf("positionOf(b)=%d want 0", p)
	}
	if p := pq.positionOf("a"); p != 1 {
		t.Fatalf("positionOf(a)=%d want 1", p)
	}
	if p := pq.positionOf("missing"); p != -1 {
		t.Fatalf("positionOf(missing)=%d want -1", p)
	}
}

func TestPQ_RemoveByID(t *testing.T) {
	pq := &pq{}
	pq.push(&entry{runID: "a", priority: PriorityNormal, submitSeq: 1})
	pq.push(&entry{runID: "b", priority: PriorityNormal, submitSeq: 2})
	if !pq.remove("a") {
		t.Fatal("remove(a) = false, want true")
	}
	if pq.remove("a") {
		t.Fatal("remove(a) second time = true, want false")
	}
	if pq.len() != 1 || pq.pop().runID != "b" {
		t.Fatal("after remove(a), only b should remain")
	}
}
