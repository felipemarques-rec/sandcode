//go:build integration
// +build integration

package orchestrator

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/agent"
	"github.com/felipemarques-rec/sandcode/internal/auth"
	gitm "github.com/felipemarques-rec/sandcode/internal/git"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
	"github.com/felipemarques-rec/sandcode/internal/store"
)

// TestOrchestrator_Docker_Smoke runs a tiny shell script as the "agent"
// inside an alpine container. It validates the full Docker-based pipeline
// (worktree -> bind-mount -> exec -> commit -> merge) without depending on
// Claude Code being installed.
//
// Run with:
//
//	go test -tags=integration -run TestOrchestrator_Docker ./internal/orchestrator/
func TestOrchestrator_Docker_Smoke(t *testing.T) {
	if !dockerAvailable() {
		t.Skip("docker not available; skipping integration test")
	}
	repo := initRepo(t) // helper from run_test.go
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	db, err := store.Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// noopAuth from helpers — no credential mounts so the test doesn't
	// depend on the host's ~/.claude.
	events, await, err := Run(ctx,
		sandbox.NewDockerProvider(),
		&shellAgent{name: "shell", script: `echo "hi from container"; printf "world\n" > greet.txt`},
		&noopAuth{},
		RunOptions{
			Prompt:         "noop",
			CWD:            repo,
			SandboxImage:   "alpine:3.20",
			SandboxWorkDir: "/workspace",
			Strategy:       gitm.StrategyMergeToHead,
			Timeout:        60 * time.Second,
			Network:        "none",
			Store:          db,
		},
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var seen []string
	for ev := range events {
		seen = append(seen, ev.Text)
	}
	res := await()
	if res.Status != "success" {
		t.Fatalf("status=%s err=%v", res.Status, res.Err)
	}
	if !strings.Contains(strings.Join(seen, "\n"), "hi from container") {
		t.Fatalf("missing event text: %v", seen)
	}
	if !strings.Contains(res.Diff, "greet.txt") {
		t.Fatalf("diff should include greet.txt, got:\n%s", res.Diff)
	}
}

// shellAgent runs an inline /bin/sh script. Used for integration testing
// against alpine; alpine's busybox sh is enough.
type shellAgent struct {
	name   string
	script string
}

func (s *shellAgent) Name() string { return s.name }
func (s *shellAgent) BuildCommand(opts agent.RunOptions) agent.Command {
	return agent.Command{Argv: []string{"/bin/sh", "-c", s.script}}
}
func (*shellAgent) ParseLine(line string) (agent.StreamEvent, bool) {
	if line == "" {
		return agent.StreamEvent{}, false
	}
	return agent.StreamEvent{Kind: agent.EventText, Text: line, Timestamp: time.Now()}, true
}
func (*shellAgent) AuthHints() agent.AuthHints { return agent.AuthHints{} }

// Compile-time assertions.
var _ agent.Provider = (*shellAgent)(nil)
var _ auth.Provider = (*noopAuth)(nil)
