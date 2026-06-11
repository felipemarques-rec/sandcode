package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfigsAllDisabled(t *testing.T) {
	for _, c := range DefaultConfigs() {
		if c.Enabled {
			t.Errorf("DefaultConfigs: %q should be disabled by default", c.Name)
		}
	}
}

func TestEnable(t *testing.T) {
	m := NewManager(DefaultConfigs())
	if !m.Enable("context7") {
		t.Fatal("Enable(context7) = false, want true")
	}
	if m.Enable("does-not-exist") {
		t.Fatal("Enable(does-not-exist) = true, want false")
	}
	enabled := m.ListEnabled(context.Background())
	if len(enabled) != 1 || enabled[0].Name != "context7" {
		t.Fatalf("ListEnabled = %v, want [context7]", enabled)
	}
}

func TestInjectIntoDir(t *testing.T) {
	m := NewManager(DefaultConfigs())
	m.Enable("context7")

	dir := t.TempDir()
	if err := m.InjectIntoDir(context.Background(), dir); err != nil {
		t.Fatalf("InjectIntoDir: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	if err != nil {
		t.Fatalf("read .mcp.json: %v", err)
	}
	var got struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	srv, ok := got.MCPServers["context7"]
	if !ok {
		t.Fatalf("mcpServers missing context7: %s", data)
	}
	if srv.Command != "npx" {
		t.Errorf("command = %q, want npx", srv.Command)
	}
	if len(srv.Args) == 0 {
		t.Errorf("args empty, want context7 npx args")
	}
}

func TestRemoveFromDir(t *testing.T) {
	m := NewManager(DefaultConfigs())
	m.Enable("context7")
	dir := t.TempDir()
	if err := m.InjectIntoDir(context.Background(), dir); err != nil {
		t.Fatalf("InjectIntoDir: %v", err)
	}
	if err := m.RemoveFromDir(dir); err != nil {
		t.Fatalf("RemoveFromDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".mcp.json")); !os.IsNotExist(err) {
		t.Fatalf(".mcp.json should be gone, stat err = %v", err)
	}
	// Idempotent: removing a missing file is not an error.
	if err := m.RemoveFromDir(dir); err != nil {
		t.Fatalf("RemoveFromDir (missing) = %v, want nil", err)
	}
}

func TestInjectIntoDirNoEnabledIsNoop(t *testing.T) {
	m := NewManager(DefaultConfigs()) // nothing enabled
	dir := t.TempDir()
	if err := m.InjectIntoDir(context.Background(), dir); err != nil {
		t.Fatalf("InjectIntoDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".mcp.json")); !os.IsNotExist(err) {
		t.Fatalf(".mcp.json should not exist when no servers enabled, stat err = %v", err)
	}
}
