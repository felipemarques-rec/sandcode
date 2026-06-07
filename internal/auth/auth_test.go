package auth

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/agent"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
)

func TestBindMountApply(t *testing.T) {
	// Synthesize a fake ~/.claude
	tmp := t.TempDir()
	claudeDir := filepath.Join(tmp, ".claude")
	if err := os.MkdirAll(claudeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	bm := &BindMount{HostClaudeDir: claudeDir, ContainerHome: "/root"}
	spec := sandbox.SandboxSpec{}
	if err := bm.Apply(&spec, agent.AuthHints{NeedsClaudeCredentials: true}); err != nil {
		t.Fatal(err)
	}
	if len(spec.Mounts) != 1 {
		t.Fatalf("expected 1 mount, got %d", len(spec.Mounts))
	}
	m := spec.Mounts[0]
	if m.Source != claudeDir || m.Target != "/root/.claude" || !m.ReadOnly {
		t.Fatalf("unexpected mount: %+v", m)
	}
}

func TestBindMountSkipsWhenNotNeeded(t *testing.T) {
	bm := NewBindMount()
	spec := sandbox.SandboxSpec{}
	if err := bm.Apply(&spec, agent.AuthHints{}); err != nil {
		t.Fatal(err)
	}
	if len(spec.Mounts) != 0 {
		t.Fatalf("expected no mount when hints empty, got %d", len(spec.Mounts))
	}
}

func TestBindMountMultipleCredentialDirs(t *testing.T) {
	tmp := t.TempDir()
	for _, name := range []string{".claude", ".cursor", ".codex"} {
		if err := os.MkdirAll(filepath.Join(tmp, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	bm := &BindMount{HostHome: tmp, ContainerHome: "/root"}
	spec := sandbox.SandboxSpec{}
	if err := bm.Apply(&spec, agent.AuthHints{
		CredentialDirs: []string{".claude", ".cursor", ".codex"},
	}); err != nil {
		t.Fatal(err)
	}
	if len(spec.Mounts) != 3 {
		t.Fatalf("expected 3 mounts, got %d (%+v)", len(spec.Mounts), spec.Mounts)
	}
	wantTargets := map[string]bool{
		"/root/.claude": false,
		"/root/.cursor": false,
		"/root/.codex":  false,
	}
	for _, m := range spec.Mounts {
		if !m.ReadOnly {
			t.Fatalf("mount %s should be RO", m.Target)
		}
		if _, ok := wantTargets[m.Target]; ok {
			wantTargets[m.Target] = true
		}
	}
	for k, v := range wantTargets {
		if !v {
			t.Fatalf("missing mount target %s", k)
		}
	}
}

func TestBindMountMissingDir(t *testing.T) {
	bm := &BindMount{HostClaudeDir: "/nonexistent-sandcode-test", ContainerHome: "/root"}
	spec := sandbox.SandboxSpec{}
	err := bm.Apply(&spec, agent.AuthHints{NeedsClaudeCredentials: true})
	if err == nil {
		t.Fatal("expected error for missing dir")
	}
}

func TestAPIKeyApply(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	api := NewAPIKey()
	spec := sandbox.SandboxSpec{}
	if err := api.Apply(&spec, agent.AuthHints{AcceptedEnvVars: []string{"ANTHROPIC_API_KEY"}}); err != nil {
		t.Fatal(err)
	}
	if spec.Env["ANTHROPIC_API_KEY"] != "sk-test" {
		t.Fatalf("env not set: %+v", spec.Env)
	}
}

func TestAPIKeyMissing(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	api := NewAPIKey()
	spec := sandbox.SandboxSpec{}
	err := api.Apply(&spec, agent.AuthHints{AcceptedEnvVars: []string{"ANTHROPIC_API_KEY"}})
	if err == nil {
		t.Fatal("expected error when env not set")
	}
}
