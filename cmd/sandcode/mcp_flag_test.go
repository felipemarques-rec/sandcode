package main

import (
	"strings"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/agent"
)

func TestBuildMCPManager_EmptyIsNil(t *testing.T) {
	m, err := buildMCPManager(nil)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if m != nil {
		t.Fatalf("manager = %v, want nil for empty slice", m)
	}
}

func TestBuildMCPManager_KnownServer(t *testing.T) {
	m, err := buildMCPManager([]string{"context7"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if m == nil {
		t.Fatal("manager = nil, want non-nil")
	}
	enabled := m.ListEnabled(t.Context())
	if len(enabled) != 1 || enabled[0].Name != "context7" {
		t.Fatalf("enabled = %v, want [context7]", enabled)
	}
}

func TestBuildMCPManager_UnknownServerErrors(t *testing.T) {
	_, err := buildMCPManager([]string{"bogus"})
	if err == nil {
		t.Fatal("err = nil, want unknown-server error")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("err = %q, want mention of %q", err, "bogus")
	}
}

func TestMCPExtraArgs_ClaudeBuildsAllowList(t *testing.T) {
	m, _ := buildMCPManager([]string{"context7"})
	args := mcpExtraArgs(m, agent.NewClaudeCode(), nil)
	got := strings.Join(args, " ")
	for _, want := range []string{"--strict-mcp-config", "--mcp-config", ".mcp.json", "--allowedTools", "mcp__context7"} {
		if !strings.Contains(got, want) {
			t.Errorf("args %q missing %q", got, want)
		}
	}
}

func TestMCPExtraArgs_NonClaudeIsNil(t *testing.T) {
	m, _ := buildMCPManager([]string{"context7"})
	if args := mcpExtraArgs(m, agent.NewCodex(), nil); args != nil {
		t.Fatalf("args = %v, want nil for non-claude agent", args)
	}
}

func TestMCPExtraArgs_NilManagerIsNil(t *testing.T) {
	if args := mcpExtraArgs(nil, agent.NewClaudeCode(), nil); args != nil {
		t.Fatalf("args = %v, want nil for nil manager", args)
	}
}
