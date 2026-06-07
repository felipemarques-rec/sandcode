package metrics

import (
	"strings"
	"sync"
	"testing"
)

func TestCounterBasic(t *testing.T) {
	c := NewCounter("c", "h", []string{"k"})
	c.Inc("a")
	c.Inc("a")
	c.Add(3, "b")

	if got := c.With("a").Value(); got != 2 {
		t.Errorf("a value = %v, want 2", got)
	}
	if got := c.With("b").Value(); got != 3 {
		t.Errorf("b value = %v, want 3", got)
	}
}

func TestCounterIgnoresNegativeAndNaN(t *testing.T) {
	c := NewCounter("c", "h", nil)
	c.Inc()
	c.Add(-5)
	c.Add(0)
	if got := c.With().Value(); got != 1 {
		t.Errorf("value = %v, want 1 (negative/zero adds must be no-ops)", got)
	}
}

func TestCounterLabelArityPanic(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on label arity mismatch")
		}
	}()
	c := NewCounter("c", "h", []string{"a", "b"})
	c.Inc("only-one")
}

func TestCounterRace(t *testing.T) {
	c := NewCounter("c", "h", []string{"k"})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				c.Inc("x")
			}
		}()
	}
	wg.Wait()
	if got := c.With("x").Value(); got != 8000 {
		t.Errorf("value = %v, want 8000", got)
	}
}

func TestGaugeOps(t *testing.T) {
	g := NewGauge("g", "h", nil)
	g.Set(5)
	g.Inc()
	g.Inc()
	g.Dec()
	g.Add(-2)
	if got := g.With().Value(); got != 4 {
		t.Errorf("value = %v, want 4", got)
	}
}

func TestHistogramObserve(t *testing.T) {
	h := NewHistogram("h", "help", nil, []float64{1, 5, 10})
	h.Observe(0.5)
	h.Observe(1)
	h.Observe(4)
	h.Observe(7)
	h.Observe(20)

	cum, sum, count := h.With().Snapshot(h.buckets)
	// flat: bucket(1)=2 (0.5, 1), bucket(5)=1 (4), bucket(10)=1 (7), inf=1 (20)
	want := []uint64{2, 3, 4, 5}
	if len(cum) != len(want) {
		t.Fatalf("cum len = %d, want %d", len(cum), len(want))
	}
	for i, v := range want {
		if cum[i] != v {
			t.Errorf("cum[%d] = %d, want %d", i, cum[i], v)
		}
	}
	if count != 5 {
		t.Errorf("count = %d, want 5", count)
	}
	if sum != 32.5 {
		t.Errorf("sum = %v, want 32.5", sum)
	}
}

func TestHistogramBucketValidation(t *testing.T) {
	tests := []struct {
		name    string
		buckets []float64
	}{
		{"non-increasing", []float64{1, 1, 2}},
		{"decreasing", []float64{5, 3, 1}},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("expected panic for %s buckets", tc.name)
				}
			}()
			_ = NewHistogram("h", "help", nil, tc.buckets)
		})
	}
}

func TestRegistryRenderGolden(t *testing.T) {
	reg := NewRegistry()
	c := reg.NewCounter("sandcode_events_total", "Total events.", []string{"type"})
	c.Inc("run.completed")
	c.Inc("run.completed")
	c.Inc("run.failed")

	g := reg.NewGauge("sandcode_active_runs", "Active runs.", nil)
	g.Set(3)

	h := reg.NewHistogram("sandcode_duration_seconds", "Run latency.", nil, []float64{1, 5})
	h.Observe(0.5)
	h.Observe(7)

	var buf strings.Builder
	if err := reg.Render(&buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	got := buf.String()

	const want = `# HELP sandcode_active_runs Active runs.
# TYPE sandcode_active_runs gauge
sandcode_active_runs 3
# HELP sandcode_duration_seconds Run latency.
# TYPE sandcode_duration_seconds histogram
sandcode_duration_seconds_bucket{le="1"} 1
sandcode_duration_seconds_bucket{le="5"} 1
sandcode_duration_seconds_bucket{le="+Inf"} 2
sandcode_duration_seconds_sum 7.5
sandcode_duration_seconds_count 2
# HELP sandcode_events_total Total events.
# TYPE sandcode_events_total counter
sandcode_events_total{type="run.completed"} 2
sandcode_events_total{type="run.failed"} 1
`
	if got != want {
		t.Errorf("render mismatch.\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestGaugeFuncRecomputesOnRender(t *testing.T) {
	reg := NewRegistry()
	var v float64
	reg.NewGaugeFunc("dynamic", "h", func() float64 { return v })

	v = 7
	var buf strings.Builder
	if err := reg.Render(&buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(buf.String(), "dynamic 7") {
		t.Errorf("missing 'dynamic 7':\n%s", buf.String())
	}

	v = 42
	buf.Reset()
	if err := reg.Render(&buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(buf.String(), "dynamic 42") {
		t.Errorf("missing 'dynamic 42':\n%s", buf.String())
	}
}

func TestGaugeFuncNilFnSafe(t *testing.T) {
	g := NewGaugeFunc("g", "h", nil)
	if g == nil {
		t.Fatal("nil GaugeFunc")
	}
	var buf strings.Builder
	if err := g.write(&buf); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !strings.Contains(buf.String(), "g 0") {
		t.Errorf("expected 'g 0':\n%s", buf.String())
	}
}

func TestRegistryDuplicateRegistration(t *testing.T) {
	reg := NewRegistry()
	reg.NewCounter("dup", "x", nil)
	if err := reg.Register(NewCounter("dup", "y", nil)); err == nil {
		t.Fatal("expected error on duplicate registration")
	}
}

func TestLabelEscaping(t *testing.T) {
	reg := NewRegistry()
	c := reg.NewCounter("escaped", "help", []string{"k"})
	c.Inc(`back\slash and "quote" and
newline`)

	var buf strings.Builder
	if err := reg.Render(&buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, `escaped{k="back\\slash and \"quote\" and\nnewline"} 1`) {
		t.Errorf("expected escaped label not found in:\n%s", got)
	}
}

func TestFormatFloat(t *testing.T) {
	cases := map[float64]string{
		0:    "0",
		1:    "1",
		-3:   "-3",
		1.5:  "1.5",
		0.1:  "0.1",
		1e15: "1000000000000000",
	}
	for v, want := range cases {
		if got := formatFloat(v); got != want {
			t.Errorf("formatFloat(%v) = %q, want %q", v, got, want)
		}
	}
}
