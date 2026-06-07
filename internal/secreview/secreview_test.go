package secreview

import (
	"context"
	"strings"
	"testing"
)

func TestScanner_FlagsSecretInAddedLine(t *testing.T) {
	diff := "diff --git a/c.go b/c.go\n--- a/c.go\n+++ b/c.go\n@@\n+const key = \"sk-ant-abcdefghijklmnopqrstuvwxyz123\"\n"
	r, err := NewScanner().Review(context.Background(), SecRequest{RunID: "r", Diff: diff})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	var found bool
	for _, f := range r.Findings {
		if f.Rule == "anthropic" {
			found = true
			if f.Severity != "high" {
				t.Fatalf("severity: %q", f.Severity)
			}
			if strings.Contains(f.Detail, "sk-ant") {
				t.Fatalf("Detail leaked the secret: %q", f.Detail)
			}
		}
	}
	if !found {
		t.Fatalf("anthropic rule not flagged: %+v", r.Findings)
	}
	if r.Reviewer != "deterministic:secrets" {
		t.Fatalf("reviewer: %q", r.Reviewer)
	}
}

func TestScanner_IgnoresContextAndRemovedLines(t *testing.T) {
	diff := " const k = \"sk-ant-abcdefghijklmnopqrstuvwxyz123\"\n-const o = \"sk-ant-zzzzzzzzzzzzzzzzzzzzzzzzzz\"\n"
	r, _ := NewScanner().Review(context.Background(), SecRequest{Diff: diff})
	if len(r.Findings) != 0 {
		t.Fatalf("must ignore non-added lines: %+v", r.Findings)
	}
}

func TestScanner_IgnoresFileHeaderAndCleanDiff(t *testing.T) {
	diff := "+++ b/main.go\n+fmt.Println(\"hello world\")\n"
	r, _ := NewScanner().Review(context.Background(), SecRequest{Diff: diff})
	if len(r.Findings) != 0 {
		t.Fatalf("clean added line + +++ header must yield no findings: %+v", r.Findings)
	}
	if r.Reviewer != "deterministic:secrets" {
		t.Fatalf("reviewer: %q", r.Reviewer)
	}
}

func TestScanner_DeduplicatesByRule(t *testing.T) {
	diff := "+a = \"sk-ant-aaaaaaaaaaaaaaaaaaaaaaaa\"\n+b = \"sk-ant-bbbbbbbbbbbbbbbbbbbbbbbb\"\n"
	r, _ := NewScanner().Review(context.Background(), SecRequest{Diff: diff})
	n := 0
	for _, f := range r.Findings {
		if f.Rule == "anthropic" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want one anthropic finding (deduped), got %d: %+v", n, r.Findings)
	}
}

func TestScanner_MultipleRulesOneLine(t *testing.T) {
	// One added line carrying two distinct secret kinds → two findings.
	// Built by concatenation so no literal key shape sits in source.
	diff := "+x := \"sk-ant-aaaaaaaaaaaaaaaaaaaaaaaa\"; y := \"AKIA" + "ABCDEFGHIJKLMNOP\"\n"
	r, _ := NewScanner().Review(context.Background(), SecRequest{Diff: diff})
	rules := map[string]bool{}
	for _, f := range r.Findings {
		rules[f.Rule] = true
	}
	if !rules["anthropic"] || !rules["aws_access_key"] {
		t.Fatalf("want both anthropic + aws_access_key from one line: %+v", r.Findings)
	}
}

func TestScanner_CRLFDiffStillFlags(t *testing.T) {
	// CRLF line endings leave a trailing \r after split; unanchored regexes
	// must still match the secret on the added line.
	diff := "+const key = \"sk-ant-abcdefghijklmnopqrstuvwxyz123\"\r\n"
	r, _ := NewScanner().Review(context.Background(), SecRequest{Diff: diff})
	var found bool
	for _, f := range r.Findings {
		if f.Rule == "anthropic" {
			found = true
		}
	}
	if !found {
		t.Fatalf("CRLF diff must still flag the secret: %+v", r.Findings)
	}
}

func TestScanner_EmptyDiff(t *testing.T) {
	r, err := NewScanner().Review(context.Background(), SecRequest{Diff: ""})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if len(r.Findings) != 0 {
		t.Fatalf("empty diff must yield no findings: %+v", r.Findings)
	}
}
