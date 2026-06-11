package main

import (
	"strings"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/agent"
	"github.com/felipemarques-rec/sandcode/internal/mcp"
)

// twoServerManager enables two known servers (context7, claude-mem) so the
// allow-list has a deterministic, multi-entry order to filter against.
func twoServerManager(t *testing.T) *mcp.Manager {
	t.Helper()
	m, err := buildMCPManager([]string{"context7", "claude-mem"})
	if err != nil {
		t.Fatalf("buildMCPManager err = %v", err)
	}
	enabled := m.ListEnabled(t.Context())
	if len(enabled) != 2 || enabled[0].Name != "context7" || enabled[1].Name != "claude-mem" {
		t.Fatalf("enabled = %v, want [context7 claude-mem] in order", enabled)
	}
	return m
}

func TestMCPExtraArgs_NilFilterByteIdentical(t *testing.T) {
	m := twoServerManager(t)
	got := mcpExtraArgs(m, agent.NewClaudeCode(), nil)
	want := []string{
		"--strict-mcp-config",
		"--mcp-config", ".mcp.json",
		"--allowedTools", "mcp__context7 mcp__claude-mem",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v (len %d), want %v (len %d)", got, len(got), want, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("arg[%d] = %q, want %q (full got %v)", i, got[i], want[i], got)
		}
	}
}

func TestMCPExtraArgs_FilterSelectsSubset(t *testing.T) {
	m := twoServerManager(t)
	permitted := func(tool string) bool { return tool == "context7" }
	got := mcpExtraArgs(m, agent.NewClaudeCode(), permitted)
	want := []string{
		"--strict-mcp-config",
		"--mcp-config", ".mcp.json",
		"--allowedTools", "mcp__context7",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestMCPExtraArgs_FilterRejectsAllIsNil(t *testing.T) {
	m := twoServerManager(t)
	rejectAll := func(tool string) bool { return false }
	if got := mcpExtraArgs(m, agent.NewClaudeCode(), rejectAll); got != nil {
		t.Fatalf("got %v, want nil when filter rejects everything", got)
	}
}
