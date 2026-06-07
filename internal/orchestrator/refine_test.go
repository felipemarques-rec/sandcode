package orchestrator

import (
	"strings"
	"testing"
)

func TestRefineOptions_Effective_Defaults(t *testing.T) {
	t.Parallel()
	got := RefineOptions{}.effective()
	if got.MaxAttempts != 3 {
		t.Fatalf("MaxAttempts default = %d, want 3", got.MaxAttempts)
	}
	if got.VerifyTailBytes != 2000 {
		t.Fatalf("VerifyTailBytes default = %d, want 2000", got.VerifyTailBytes)
	}
}

func TestRefineOptions_Effective_PreservesExplicitValues(t *testing.T) {
	t.Parallel()
	r := RefineOptions{MaxAttempts: 5, VerifyTailBytes: 500}.effective()
	if r.MaxAttempts != 5 || r.VerifyTailBytes != 500 {
		t.Fatalf("explicit values not preserved: %+v", r)
	}
}

func TestRefineOptions_Active(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		r    RefineOptions
		want bool
	}{
		{"disabled-empty", RefineOptions{}, false},
		{"enabled-no-cmd", RefineOptions{Enabled: true}, false},
		{"disabled-with-cmd", RefineOptions{VerifyCmd: []string{"x"}}, false},
		{"enabled-and-cmd", RefineOptions{Enabled: true, VerifyCmd: []string{"x"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.r.active(); got != tc.want {
				t.Fatalf("active() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBuildRefinePrompt_IncludesContextAndFeedback(t *testing.T) {
	t.Parallel()
	got := buildRefinePrompt(
		"Original task: add a CSV exporter",
		"FAIL: TestCSV_HeaderOrder — expected [name,email] got [email,name]",
		2, 3,
		[]string{"go", "test", "./..."},
	)
	// Must mention attempt counter.
	if !strings.Contains(got, "Attempt 2 of 3") {
		t.Errorf("missing attempt header: %s", got)
	}
	// Must include verify command.
	if !strings.Contains(got, "go test ./...") {
		t.Errorf("missing verify command: %s", got)
	}
	// Must include the failure tail.
	if !strings.Contains(got, "FAIL: TestCSV_HeaderOrder") {
		t.Errorf("missing failure tail")
	}
	// Must preserve the original enriched prompt at the bottom.
	if !strings.Contains(got, "Original task: add a CSV exporter") {
		t.Errorf("missing original prompt")
	}
	// Must instruct the agent NOT to re-run the verifier itself.
	if !strings.Contains(got, "Do NOT re-run the verifier") {
		t.Errorf("missing don't-rerun-verifier directive")
	}
}

func TestTail_NoTruncationWhenWithinLimit(t *testing.T) {
	t.Parallel()
	in := "short string"
	if got := tail(in, 100); got != in {
		t.Fatalf("tail untruncated: got=%q want=%q", got, in)
	}
}

func TestTail_TruncatesAndPrefixes(t *testing.T) {
	t.Parallel()
	in := strings.Repeat("x", 1000) + "TAIL_MARKER"
	got := tail(in, 20)
	if !strings.HasPrefix(got, "...(truncated)\n") {
		t.Fatalf("tail missing truncation prefix: %q", got[:30])
	}
	if !strings.Contains(got, "TAIL_MARKER") {
		t.Fatalf("tail dropped the last bytes")
	}
	if len(got) > 20+len("...(truncated)\n") {
		t.Fatalf("tail exceeded limit: len=%d", len(got))
	}
}

func TestTail_ZeroOrNegativeLimitIsPassthrough(t *testing.T) {
	t.Parallel()
	in := "anything"
	if got := tail(in, 0); got != in {
		t.Fatalf("tail(s,0) = %q, want passthrough", got)
	}
	if got := tail(in, -1); got != in {
		t.Fatalf("tail(s,-1) = %q, want passthrough", got)
	}
}
