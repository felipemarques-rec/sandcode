package judge

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeClaudeOnPath installs a stub `claude` that ignores stdin and prints the
// given result-string inside the CLI's json envelope, then prepends it to PATH.
// This exercises the subscription transport end to end (callTool → llmcli).
func fakeClaudeOnPath(t *testing.T, result string) {
	t.Helper()
	dir := t.TempDir()
	// result must be embedded as a JSON string value.
	envelope := `{"type":"result","subtype":"success","is_error":false,"result":` + quote(result) + `}`
	outFile := filepath.Join(dir, "out.json")
	if err := os.WriteFile(outFile, []byte(envelope), 0o644); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "claude")
	body := "#!/bin/sh\ncat >/dev/null\ncat " + outFile + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func quote(s string) string {
	var b []byte
	b = append(b, '"')
	for _, r := range s {
		switch r {
		case '"':
			b = append(b, '\\', '"')
		case '\\':
			b = append(b, '\\', '\\')
		case '\n':
			b = append(b, '\\', 'n')
		default:
			b = append(b, string(r)...)
		}
	}
	b = append(b, '"')
	return string(b)
}

func TestLLMJudge_Subscription_RoutesThroughCLI(t *testing.T) {
	fakeClaudeOnPath(t, `{"winner":"a","scores":{"a":0.9,"b":0.2},"rationale":"a is cleaner"}`)
	j := NewLLMJudgeFromSubscription("")
	j.cli.Timeout = 5 * time.Second
	cands := []Candidate{{RunID: "a"}, {RunID: "b"}}
	rank, err := j.Rank(context.Background(), "do a thing", cands)
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}
	if rank.Winner != "a" || rank.Scores["a"] != 0.9 {
		t.Fatalf("got %+v", rank)
	}
}

func TestLLMReviewer_Subscription_RoutesThroughCLI(t *testing.T) {
	fakeClaudeOnPath(t, `{"score":0.75,"comments":"looks fine"}`)
	r := NewLLMReviewerFromSubscription("")
	r.cli.Timeout = 5 * time.Second
	rv, err := r.Review(context.Background(), ReviewRequest{Prompt: "p", Diff: "d"})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if rv.Score != 0.75 || rv.Comments != "looks fine" {
		t.Fatalf("got %+v", rv)
	}
}
