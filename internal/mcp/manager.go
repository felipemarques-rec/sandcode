// Package mcp manages Model Context Protocol server configurations
// that are injected into sandbox containers.
//
// MCP servers give agents access to external tools (documentation,
// memory, skills) at runtime without modifying the agent's code.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Config represents a single MCP server configuration.
type Config struct {
	Name    string            `json:"name"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env,omitempty"`
	Enabled bool              `json:"enabled"`
}

// Manager resolves and injects MCP configurations into sandbox work directories.
type Manager struct {
	configs []Config
}

// NewManager creates an MCP manager with the given server configs.
func NewManager(configs []Config) *Manager {
	return &Manager{configs: configs}
}

// DefaultConfigs returns the built-in MCP server configurations.
// All are disabled by default — enable via config or --mcp flags.
func DefaultConfigs() []Config {
	return []Config{
		{
			Name:    "context7",
			Command: "npx",
			Args:    []string{"-y", "@upstash/context7-mcp@latest"},
			Enabled: false,
		},
		{
			Name:    "claude-mem",
			Command: "npx",
			Args:    []string{"-y", "claude-mem-mcp@latest"},
			Enabled: false,
		},
	}
}

// ListEnabled returns only the enabled MCP server configs.
func (m *Manager) ListEnabled(_ context.Context) []Config {
	var out []Config
	for _, c := range m.configs {
		if c.Enabled {
			out = append(out, c)
		}
	}
	return out
}

// Enable activates a named MCP server.
func (m *Manager) Enable(name string) bool {
	for i := range m.configs {
		if m.configs[i].Name == name {
			m.configs[i].Enabled = true
			return true
		}
	}
	return false
}

// mcpJSON is the structure written to .mcp.json inside containers.
type mcpJSON struct {
	MCPServers map[string]mcpServerEntry `json:"mcpServers"`
}

type mcpServerEntry struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env,omitempty"`
}

// InjectIntoDir writes a .mcp.json file into the given directory.
// Agents that support MCP will auto-discover it.
func (m *Manager) InjectIntoDir(_ context.Context, workDir string) error {
	enabled := m.ListEnabled(context.Background())
	if len(enabled) == 0 {
		return nil // nothing to inject
	}

	mcpFile := mcpJSON{MCPServers: make(map[string]mcpServerEntry)}
	for _, c := range enabled {
		mcpFile.MCPServers[c.Name] = mcpServerEntry{
			Command: c.Command,
			Args:    c.Args,
			Env:     c.Env,
		}
	}

	data, err := json.MarshalIndent(mcpFile, "", "  ")
	if err != nil {
		return fmt.Errorf("mcp: marshal: %w", err)
	}

	path := filepath.Join(workDir, ".mcp.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("mcp: write %s: %w", path, err)
	}
	return nil
}

// RemoveFromDir deletes the .mcp.json previously written by InjectIntoDir.
// Callers invoke it before committing the worktree so the injected config —
// a runtime artifact, not the agent's work product — never lands in the
// run's diff/commit/merge. Best-effort: a missing file is not an error.
func (m *Manager) RemoveFromDir(workDir string) error {
	if err := os.Remove(filepath.Join(workDir, ".mcp.json")); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("mcp: remove %s: %w", filepath.Join(workDir, ".mcp.json"), err)
	}
	return nil
}
