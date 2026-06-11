package orchestrator

import (
	"fmt"
	"strings"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/planner"
)

func TestBuildHandoffPrompt_FirstNodeHasNoHandoff(t *testing.T) {
	t.Parallel()
	node := planner.Node{ID: "a", Prompt: "create user model"}
	got := buildHandoffPrompt(node, nil)
	if got != "create user model" {
		t.Errorf("first node should be literal prompt, got: %q", got)
	}
}

func TestBuildHandoffPrompt_AppendsStructuredBlock(t *testing.T) {
	t.Parallel()
	prev := []NodeResult{{
		NodeID: "a",
		Prompt: "create user model",
		Result: AgentInvocationResult{Completion: "Done. Created models/user.go with id, email, password fields."},
		Diff:   "diff --git a/models/user.go b/models/user.go\nnew file mode 100644\n+++ b/models/user.go\n+package models\n",
	}}
	node := planner.Node{ID: "b", Prompt: "add validation to user model"}
	got := buildHandoffPrompt(node, prev)

	for _, want := range []string{
		"add validation to user model",
		"Previous step (node a)",
		"create user model",
		"Done. Created models/user.go",
		"models/user.go",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("handoff missing %q\nfull: %s", want, got)
		}
	}
}

func TestBuildHandoffPrompt_AppendsDoD(t *testing.T) {
	t.Parallel()
	// First node (no prev): DoD appended after the literal prompt.
	first := planner.Node{ID: "a", Prompt: "create user model", DoD: "go test ./... passes"}
	got := buildHandoffPrompt(first, nil)
	if !strings.Contains(got, "## Definition of done") || !strings.Contains(got, "go test ./... passes") {
		t.Errorf("first-node DoD missing\nfull: %s", got)
	}
	if !strings.HasPrefix(got, "create user model") {
		t.Errorf("DoD should follow the prompt, got: %q", got)
	}

	// Subsequent node (with prev): DoD appended after the handoff block.
	prev := []NodeResult{{NodeID: "a", Prompt: "create user model", Result: AgentInvocationResult{Completion: "done"}}}
	node := planner.Node{ID: "b", Prompt: "add validation", DoD: "validation rejects empty email"}
	got = buildHandoffPrompt(node, prev)
	if !strings.Contains(got, "Previous step (node a)") {
		t.Errorf("handoff block missing\nfull: %s", got)
	}
	if !strings.Contains(got, "## Definition of done") || !strings.Contains(got, "validation rejects empty email") {
		t.Errorf("subsequent-node DoD missing\nfull: %s", got)
	}
}

func TestBuildHandoffPrompt_NoDoD_ByteIdentical(t *testing.T) {
	t.Parallel()
	// Empty DoD must leave both paths byte-identical to the legacy output.
	first := planner.Node{ID: "a", Prompt: "create user model"}
	if got := buildHandoffPrompt(first, nil); got != "create user model" {
		t.Errorf("empty DoD changed first-node output: %q", got)
	}
	if strings.Contains(buildHandoffPrompt(first, nil), "Definition of done") {
		t.Error("empty DoD should not emit a Definition-of-done section")
	}
}

func TestBuildHandoffPrompt_TruncatesLongCompletion(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x ", 5000)
	prev := []NodeResult{{
		NodeID: "a",
		Prompt: "p",
		Result: AgentInvocationResult{Completion: long},
	}}
	node := planner.Node{ID: "b", Prompt: "next"}
	got := buildHandoffPrompt(node, prev)
	if len(got) > 4096 {
		t.Errorf("handoff exceeded soft cap: %d bytes", len(got))
	}
	if !strings.Contains(got, "…(truncated)") {
		t.Errorf("expected truncation marker in handoff, got: %s", got[:200])
	}
}

func TestBuildHandoffPrompt_EscapesTripleBackticks(t *testing.T) {
	t.Parallel()
	prev := []NodeResult{{
		NodeID: "a",
		Prompt: "p",
		Result: AgentInvocationResult{Completion: "use ```bash\nls\n``` to check"},
	}}
	node := planner.Node{ID: "b", Prompt: "next"}
	got := buildHandoffPrompt(node, prev)
	openFences := strings.Count(got, "```")
	if openFences%2 != 0 || openFences > 0 {
		t.Errorf("handoff has unbalanced ``` fences: %d", openFences)
	}
}

func TestBuildHandoffPrompt_DeterministicForSameInput(t *testing.T) {
	t.Parallel()
	prev := []NodeResult{{
		NodeID: "a",
		Prompt: "p",
		Result: AgentInvocationResult{Completion: "did stuff"},
		Diff:   "diff --git a/x.go b/x.go\n",
	}}
	node := planner.Node{ID: "b", Prompt: "next"}
	first := buildHandoffPrompt(node, prev)
	second := buildHandoffPrompt(node, prev)
	if first != second {
		t.Errorf("handoff non-deterministic for identical input")
	}
}

func TestBuildHandoffPrompt_FilesTruncatedWithCount(t *testing.T) {
	t.Parallel()
	var diff strings.Builder
	for i := 0; i < 60; i++ {
		diff.WriteString(fmt.Sprintf("diff --git a/file%02d.go b/file%02d.go\n", i, i))
	}
	prev := []NodeResult{{
		NodeID: "a", Prompt: "p",
		Result: AgentInvocationResult{Completion: "ok"},
		Diff:   diff.String(),
	}}
	got := buildHandoffPrompt(planner.Node{ID: "b", Prompt: "next"}, prev)
	if !strings.Contains(got, "(+10 more)") {
		t.Errorf("expected truncation marker for 60 files (cap 50), got: %s", got)
	}
}

func TestExtractChangedFiles(t *testing.T) {
	t.Parallel()
	diff := "diff --git a/foo/bar.go b/foo/bar.go\n+changes\ndiff --git a/baz.go b/baz.go\n+changes\n"
	got := extractChangedFiles(diff)
	want := []string{"foo/bar.go", "baz.go"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestExtractChangedFiles_Empty(t *testing.T) {
	t.Parallel()
	if got := extractChangedFiles(""); got != nil {
		t.Errorf("empty diff: got %v want nil", got)
	}
}
