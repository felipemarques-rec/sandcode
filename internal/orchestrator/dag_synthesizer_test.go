package orchestrator

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/event"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
)

// synthesizerHarness mirrors chainHarness but for synthesizer testing.
// The "winner worktree" is a real git repo (initRepo) so the
// synthesizer can mount + write into it like any agent run.
type synthesizerHarness struct {
	repo string
	args synthesizerArgs
	rec  *recorder
}

func newSynthesizerHarness(t *testing.T, refine RefineOptions) *synthesizerHarness {
	t.Helper()
	repo := initRepo(t)
	lb := event.NewLocalBus()
	t.Cleanup(func() { _ = lb.Close() })
	rec := &recorder{}
	lb.Subscribe("*", rec.handle)

	winner := ChainResult{
		ChainID: "chain-0", Success: true,
		Nodes: []NodeResult{{
			NodeID: "a",
			Result: AgentInvocationResult{Completion: "winner did the thing"},
			Diff:   "diff --git a/winner.go b/winner.go\n",
		}},
	}
	other := ChainResult{
		ChainID: "chain-1", Success: true,
		Nodes: []NodeResult{{
			NodeID: "a",
			Result: AgentInvocationResult{Completion: "alt did it differently"},
			Diff:   "diff --git a/alt.go b/alt.go\n",
		}},
	}

	return &synthesizerHarness{
		repo: repo,
		rec:  rec,
		args: synthesizerArgs{
			WinnerWorktree: repo,
			Winner:         winner,
			AllChains:      []ChainResult{winner, other},
			JudgeRationale: "winner had cleaner separation",
			OriginalPrompt: "build the thing",
			SandboxImage:   "ignored-by-nosandbox",
			SandboxWorkDir: filepath.Join(repo, ".sandcode", "work", t.Name(), "syn"),
			Bus:            lb,
			RunID:          "syn-" + t.Name(),
			Refine:         refine,
		},
	}
}

// Prompt content assertion is covered by direct buildSynthesizerPrompt
// unit tests below — no sandbox needed for that. The runSynthesizer
// tests exercise the full execution loop with a real worktree mount
// (via nosandbox), focusing on success/refine/failure semantics rather
// than prompt shape.

func TestRunSynthesizer_HappyPath(t *testing.T) {
	h := newSynthesizerHarness(t, RefineOptions{})
	ctx := context.Background()

	res, err := runSynthesizer(ctx,
		sandbox.NewNoSandboxProvider(),
		&fakeAgent{script: `echo synthesizer ran`},
		&noopAuth{},
		h.args,
	)
	if err != nil {
		t.Fatalf("runSynthesizer: %v", err)
	}
	if res.ExitCode != 0 || res.Err != nil {
		t.Errorf("synthesizer should succeed: exit=%d err=%v", res.ExitCode, res.Err)
	}
	if h.rec.count(event.DAGSynthesisStarted) != 1 {
		t.Errorf("DAGSynthesisStarted: got %d want 1", h.rec.count(event.DAGSynthesisStarted))
	}
	if h.rec.count(event.DAGSynthesisCompleted) != 1 {
		t.Errorf("DAGSynthesisCompleted: got %d want 1", h.rec.count(event.DAGSynthesisCompleted))
	}
}

const synthRefineProgressive = `
counter=".syn_counter"
[ -f "$counter" ] && c=$(cat "$counter") || c=0
c=$((c+1))
echo "$c" > "$counter"
echo "syn attempt $c"
if [ "$c" -ge 2 ]; then
  echo fixed > .syn_fixed
fi
`

func TestRunSynthesizer_RespectsRefine(t *testing.T) {
	h := newSynthesizerHarness(t, RefineOptions{
		Enabled:     true,
		VerifyCmd:   []string{"sh", "-c", "test -f .syn_fixed"},
		MaxAttempts: 3,
	})
	ctx := context.Background()

	res, err := runSynthesizer(ctx,
		sandbox.NewNoSandboxProvider(),
		&fakeAgent{script: synthRefineProgressive},
		&noopAuth{},
		h.args,
	)
	if err != nil {
		t.Fatalf("runSynthesizer: %v", err)
	}
	if res.ExitCode != 0 || res.Err != nil {
		t.Errorf("synthesizer with refine should eventually succeed: exit=%d err=%v", res.ExitCode, res.Err)
	}
}

func TestRunSynthesizer_FailureReturnsNonZeroExit(t *testing.T) {
	h := newSynthesizerHarness(t, RefineOptions{})
	ctx := context.Background()

	res, err := runSynthesizer(ctx,
		sandbox.NewNoSandboxProvider(),
		&fakeAgent{script: `echo failing; exit 1`},
		&noopAuth{},
		h.args,
	)
	if err != nil {
		t.Fatalf("runSynthesizer (infra) error: %v", err)
	}
	if res.ExitCode == 0 {
		t.Errorf("expected non-zero ExitCode for failed synthesizer, got 0")
	}
	if h.rec.count(event.DAGSynthesisCompleted) != 1 {
		t.Errorf("DAGSynthesisCompleted: got %d want 1", h.rec.count(event.DAGSynthesisCompleted))
	}
}

func TestBuildSynthesizerPrompt_IncludesAllChains(t *testing.T) {
	t.Parallel()
	winner := ChainResult{
		ChainID: "chain-0", Success: true,
		Nodes: []NodeResult{{NodeID: "a", Result: AgentInvocationResult{Completion: "winner work"}, Diff: "diff --git a/x.go b/x.go\n"}},
	}
	other := ChainResult{
		ChainID: "chain-1", Success: true,
		Nodes: []NodeResult{{NodeID: "a", Result: AgentInvocationResult{Completion: "alt work"}, Diff: "diff --git a/y.go b/y.go\n"}},
	}
	prompt := buildSynthesizerPrompt(synthesizerArgs{
		Winner:         winner,
		AllChains:      []ChainResult{winner, other},
		JudgeRationale: "winner had cleaner separation",
		OriginalPrompt: "build the thing",
	})
	for _, want := range []string{
		"chain-0", "chain-1",
		"winner had cleaner separation",
		"build the thing",
		"x.go", "y.go",
		"Winner: chain-0",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("synthesizer prompt missing %q\nfull:\n%s", want, prompt)
		}
	}
}
