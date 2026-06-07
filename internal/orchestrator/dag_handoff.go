package orchestrator

import (
	"fmt"
	"strings"

	"github.com/felipemarques-rec/sandcode/internal/planner"
)

// Handoff sizing knobs. Total per-prev-node overhead capped at ~2KB to
// avoid prompt bloat for long chains.
const (
	handoffMaxGoalChars      = 800  // ~200 tokens
	handoffMaxFinalNoteChars = 2000 // ~500 tokens
	handoffMaxFilesListed    = 50
)

// buildHandoffPrompt composes the prompt sent to the agent for one node
// in a chain. The first node of a chain (prev empty) receives only
// node.Prompt verbatim — no handoff overhead. Subsequent nodes receive
// node.Prompt followed by a structured block summarizing the prior step.
//
//	{node.Prompt}
//
//	---
//	Previous step (node {prev.ID}):
//	  Goal: {prev.Prompt, truncated}
//	  Files changed: {derived from prev.Diff name list, truncated}
//	  Commits: (not tracked)
//	  Final note: {tail of prev.Result.Completion, truncated}
//	---
//
// Backticks (triple) in prev content are neutralized to keep the
// surrounding markdown well-formed when the handoff bubbles up into
// the synthesizer / judge.
func buildHandoffPrompt(node planner.Node, prev []NodeResult) string {
	if len(prev) == 0 {
		return node.Prompt
	}

	var b strings.Builder
	b.WriteString(node.Prompt)
	b.WriteString("\n\n---\n")

	// Slice 4 only summarizes the immediately-preceding node. Future
	// (multi-prev fan-in) once diamonds are supported.
	last := prev[len(prev)-1]

	b.WriteString(fmt.Sprintf("Previous step (node %s):\n", last.NodeID))
	b.WriteString("  Goal: ")
	b.WriteString(truncate(last.Prompt, handoffMaxGoalChars))
	b.WriteString("\n")

	files := extractChangedFiles(last.Diff)
	extra := 0
	if len(files) > handoffMaxFilesListed {
		extra = len(files) - handoffMaxFilesListed
		files = files[:handoffMaxFilesListed]
	}
	b.WriteString("  Files changed: ")
	if len(files) == 0 {
		b.WriteString("(none recorded)")
	} else {
		b.WriteString(strings.Join(files, ", "))
		if extra > 0 {
			b.WriteString(fmt.Sprintf(" … (+%d more)", extra))
		}
	}
	b.WriteString("\n")

	// Commits not surfaced yet — chains run agent commits into the
	// worktree but we don't yet propagate the git log here. Reserved
	// field; emit a stable placeholder so the prompt structure holds.
	b.WriteString("  Commits: (not tracked)\n")

	b.WriteString("  Final note: ")
	b.WriteString(stripBackticks(truncate(last.Result.Completion, handoffMaxFinalNoteChars)))
	b.WriteString("\n---\n")

	return b.String()
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + " …(truncated)"
}

// stripBackticks neutralizes triple-backtick fences (which would
// unbalance any surrounding markdown structure) while preserving
// single backticks (inline code is fine).
func stripBackticks(s string) string {
	return strings.ReplaceAll(s, "```", "''' ")
}

// extractChangedFiles parses a unified diff for the file paths it
// touches. Returns the unique list in first-seen order. Tolerates
// empty/nil diff (returns empty slice). Best-effort — not a full diff
// parser; the agent gets file names good enough for context.
func extractChangedFiles(diff string) []string {
	if diff == "" {
		return nil
	}
	seen := map[string]struct{}{}
	var out []string
	for _, line := range strings.Split(diff, "\n") {
		if !strings.HasPrefix(line, "diff --git ") {
			continue
		}
		// Format: "diff --git a/path b/path"
		parts := strings.Fields(line)
		if len(parts) < 4 {
			continue
		}
		path := strings.TrimPrefix(parts[2], "a/")
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	return out
}
