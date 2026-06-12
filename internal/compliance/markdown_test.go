package compliance

import (
	"strings"
	"testing"
)

func TestRenderMarkdown_Sections(t *testing.T) {
	md := Build(sampleInput()).RenderMarkdown()
	for _, want := range []string{
		"# Compliance Report — run run-1",
		"## Run",
		"## Decisions",
		"## Summary",
		"## Integrity",
		"diff_size",
		"alice",
		"sha256",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown missing %q\n---\n%s", want, md)
		}
	}
}

func TestRenderMarkdown_EmptyDecisions(t *testing.T) {
	in := sampleInput()
	in.AuditRows = nil
	md := Build(in).RenderMarkdown()
	if !strings.Contains(md, "No governance decisions recorded.") {
		t.Fatalf("missing empty-state line:\n%s", md)
	}
}
