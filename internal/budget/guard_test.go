package budget

import (
	"sync"
	"testing"
)

func TestGuard_EmptyReportIsZero(t *testing.T) {
	t.Parallel()
	g := New()
	r := g.Report("unseen")
	if r.RunID != "unseen" {
		t.Fatalf("RunID = %s, want unseen", r.RunID)
	}
	if r.Attempts != 0 || r.Tokens != 0 || r.CostUSD != 0 {
		t.Fatalf("zero-value Report dirty: %+v", r)
	}
}

func TestGuard_RecordAttempt_Increments(t *testing.T) {
	t.Parallel()
	g := New()
	if n := g.RecordAttempt("r1"); n != 1 {
		t.Fatalf("first RecordAttempt = %d, want 1", n)
	}
	if n := g.RecordAttempt("r1"); n != 2 {
		t.Fatalf("second RecordAttempt = %d, want 2", n)
	}
	if r := g.Report("r1"); r.Attempts != 2 {
		t.Fatalf("Report Attempts = %d, want 2", r.Attempts)
	}
}

func TestGuard_RecordTokens_AccumulatesAndClampsNegative(t *testing.T) {
	t.Parallel()
	g := New()
	g.RecordTokens("r1", 100)
	g.RecordTokens("r1", 50)
	g.RecordTokens("r1", -999) // clamped to 0
	if r := g.Report("r1"); r.Tokens != 150 {
		t.Fatalf("Tokens = %d, want 150", r.Tokens)
	}
}

func TestGuard_RecordCost_AccumulatesAndClampsNegative(t *testing.T) {
	t.Parallel()
	g := New()
	g.RecordCost("r1", 0.5)
	g.RecordCost("r1", 1.25)
	g.RecordCost("r1", -3.0) // clamped
	r := g.Report("r1")
	if r.CostUSD < 1.749 || r.CostUSD > 1.751 {
		t.Fatalf("CostUSD = %f, want ~1.75", r.CostUSD)
	}
}

func TestGuard_IsolatesByRunID(t *testing.T) {
	t.Parallel()
	g := New()
	g.RecordAttempt("rA")
	g.RecordAttempt("rB")
	g.RecordAttempt("rB")
	g.RecordTokens("rA", 10)
	g.RecordTokens("rB", 20)

	if r := g.Report("rA"); r.Attempts != 1 || r.Tokens != 10 {
		t.Fatalf("rA dirty: %+v", r)
	}
	if r := g.Report("rB"); r.Attempts != 2 || r.Tokens != 20 {
		t.Fatalf("rB dirty: %+v", r)
	}
}

func TestGuard_Forget_RemovesReport(t *testing.T) {
	t.Parallel()
	g := New()
	g.RecordAttempt("r1")
	g.RecordTokens("r1", 100)
	g.Forget("r1")
	if r := g.Report("r1"); r.Attempts != 0 || r.Tokens != 0 {
		t.Fatalf("Forget did not clear: %+v", r)
	}
}

func TestGuard_Report_ReturnsCopy(t *testing.T) {
	t.Parallel()
	g := New()
	g.RecordAttempt("r1")
	r1 := g.Report("r1")
	r1.Attempts = 999 // mutate copy
	if r := g.Report("r1"); r.Attempts != 1 {
		t.Fatalf("Report mutation leaked into Guard: %d", r.Attempts)
	}
}

// TestGuard_ConcurrentRecordsAreRaceClean hammers the Guard from many
// goroutines and asserts the final totals are exact. Catches missing
// locks under -race.
func TestGuard_ConcurrentRecordsAreRaceClean(t *testing.T) {
	t.Parallel()
	g := New()
	const workers = 16
	const each = 200
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				g.RecordAttempt("hot")
				g.RecordTokens("hot", 1)
				g.RecordCost("hot", 0.01)
			}
		}()
	}
	wg.Wait()
	r := g.Report("hot")
	if r.Attempts != workers*each {
		t.Fatalf("Attempts = %d, want %d", r.Attempts, workers*each)
	}
	if r.Tokens != int64(workers*each) {
		t.Fatalf("Tokens = %d, want %d", r.Tokens, workers*each)
	}
	wantCost := float64(workers*each) * 0.01
	if r.CostUSD < wantCost-0.001 || r.CostUSD > wantCost+0.001 {
		t.Fatalf("CostUSD = %f, want ~%f", r.CostUSD, wantCost)
	}
}
