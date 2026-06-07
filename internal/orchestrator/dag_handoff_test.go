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
