package metrics

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/event"
)

func TestSubscriberCountsEvents(t *testing.T) {
	reg := NewRegistry()
	s := NewSubscriber(reg)

	cases := []struct {
		typ  event.Type
		runs int
	}{
		{event.RunSubmitted, 5},
		{event.RunCompleted, 3},
		{event.RunFailed, 1},
		{event.RunCancelled, 1},
		{event.VerifyPassed, 4},
		{event.VerifyFailed, 2},
		{event.RefineTriggered, 6},
		{event.GovernanceApproved, 7},
		{event.GovernanceDenied, 1},
		{event.BudgetThresholdReached, 2},
		{event.BudgetExceeded, 1},
		{event.SandboxCreated, 3},
		{event.SandboxDestroyed, 3},
		{event.AgentToolCalled, 9},
	}

	for _, tc := range cases {
		for i := 0; i < tc.runs; i++ {
			s.observe(event.Event{Type: tc.typ, RunID: "r"})
		}
	}

	checks := map[string]float64{
		"sandcode_runs_total{result=completed}":                3,
		"sandcode_runs_total{result=failed}":                   1,
		"sandcode_runs_total{result=cancelled}":                1,
		"sandcode_verify_total{result=passed}":                 4,
		"sandcode_verify_total{result=failed}":                 2,
		"sandcode_refines_total{}":                             6,
		"sandcode_governance_decisions_total{result=approved}": 7,
		"sandcode_governance_decisions_total{result=denied}":   1,
		"sandcode_budget_events_total{kind=threshold}":         2,
		"sandcode_budget_events_total{kind=exceeded}":          1,
		"sandcode_sandboxes_total{state=created}":              3,
		"sandcode_sandboxes_total{state=destroyed}":            3,
		"sandcode_agent_tools_called_total{}":                  9,
		"sandcode_events_total{type=run.submitted}":            5,
		"sandcode_events_total{type=run.completed}":            3,
	}

	for key, want := range checks {
		if got := lookup(t, s, key); got != want {
			t.Errorf("%s = %v, want %v", key, got, want)
		}
	}
}

func TestSubscriberRecordsRunDuration(t *testing.T) {
	reg := NewRegistry()
	s := NewSubscriber(reg)

	start := time.Now()
	s.observe(event.Event{Type: event.RunSubmitted, RunID: "r1", Timestamp: start})
	s.observe(event.Event{Type: event.RunCompleted, RunID: "r1", Timestamp: start.Add(2500 * time.Millisecond)})

	cum, sum, count := s.runDuration.With("completed").Snapshot(s.runDuration.buckets)
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
	if sum < 2.4 || sum > 2.6 {
		t.Errorf("sum = %v, want ~2.5", sum)
	}
	// DefaultBuckets contains 2.5; cumulative at that bucket should be 1.
	idx := -1
	for i, ub := range s.runDuration.buckets {
		if ub == 2.5 {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatal("DefaultBuckets missing 2.5 — test assumption broken")
	}
	if cum[idx] != 1 {
		t.Errorf("cumulative at le=2.5 = %d, want 1", cum[idx])
	}
}

func TestSubscriberAttachWithLocalBus(t *testing.T) {
	reg := NewRegistry()
	s := NewSubscriber(reg)

	bus := event.NewLocalBus()
	defer bus.Close()
	sub := s.Attach(bus)
	defer sub.Cancel()

	for i := 0; i < 3; i++ {
		if err := bus.Publish(context.Background(), event.New(event.RefineTriggered, "r", nil)); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	if got := s.refinesTotal.With().Value(); got != 3 {
		t.Errorf("refines = %v, want 3", got)
	}
}

func TestStartTrackerCapacityEviction(t *testing.T) {
	tr := newStartTracker()
	tr.capacity = 3
	now := time.Now()
	tr.set("a", now)
	tr.set("b", now)
	tr.set("c", now)
	tr.set("d", now) // evicts "a"

	if _, ok := tr.take("a"); ok {
		t.Error("a should have been evicted")
	}
	if _, ok := tr.take("d"); !ok {
		t.Error("d should be present")
	}
}

// lookup renders the registry and finds the value for a metric line
// matching name and label set. key format: "name{k=v,k=v}" or "name{}".
func lookup(t *testing.T, s *Subscriber, key string) float64 {
	t.Helper()
	var buf strings.Builder
	if err := s.reg.Render(&buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	name, labels := splitKey(key)
	for _, line := range strings.Split(out, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		ln, lbls, val := parseSample(line)
		if ln == name && lbls == labels {
			return val
		}
	}
	t.Fatalf("no sample for %s in:\n%s", key, out)
	return 0
}

func splitKey(key string) (name, labels string) {
	i := strings.IndexByte(key, '{')
	if i < 0 {
		return key, ""
	}
	return key[:i], key[i:]
}

func parseSample(line string) (name, labels string, value float64) {
	// "<name>[<labels>] <value>"
	sp := strings.LastIndexByte(line, ' ')
	if sp < 0 {
		return "", "", 0
	}
	head := line[:sp]
	tail := line[sp+1:]
	// parse float
	var v float64
	for i := 0; i < len(tail); i++ {
		c := tail[i]
		if c == '-' || c == '+' || c == '.' || (c >= '0' && c <= '9') {
			continue
		}
		tail = tail[:i]
		break
	}
	_, _ = parseFloatStrict(tail, &v)
	// split name from labels
	if i := strings.IndexByte(head, '{'); i >= 0 {
		return head[:i], normalizeLabels(head[i:]), v
	}
	return head, "{}", v
}

// parseFloatStrict is a tiny strconv shim that avoids importing strconv
// just for the test. It supports the cases the registry actually emits.
func parseFloatStrict(s string, out *float64) (bool, error) {
	var sign float64 = 1
	if len(s) > 0 && s[0] == '-' {
		sign = -1
		s = s[1:]
	} else if len(s) > 0 && s[0] == '+' {
		s = s[1:]
	}
	dot := strings.IndexByte(s, '.')
	var intPart, fracPart string
	if dot < 0 {
		intPart = s
	} else {
		intPart = s[:dot]
		fracPart = s[dot+1:]
	}
	var ip int64
	for _, c := range intPart {
		if c < '0' || c > '9' {
			return false, nil
		}
		ip = ip*10 + int64(c-'0')
	}
	frac := 0.0
	denom := 1.0
	for _, c := range fracPart {
		if c < '0' || c > '9' {
			return false, nil
		}
		denom *= 10
		frac += float64(c-'0') / denom
	}
	*out = sign * (float64(ip) + frac)
	return true, nil
}

// normalizeLabels strips quotes around values so the test can compare
// `{type=run.completed}` style keys against `{type="run.completed"}`.
func normalizeLabels(s string) string {
	return strings.ReplaceAll(s, `"`, "")
}
