package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/rbac"
)

// writeRBACConfig writes a valid RBAC config JSON into a temp dir and returns
// its path. The "operator" role grants the bare MCP server name "github" (NOT
// the mcp__ prefix) so the permits closure can be exercised directly.
func writeRBACConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "rbac.json")
	cfg := `{
  "roles": {
    "operator": { "tools": ["github"], "capabilities": ["run:create"] }
  },
  "keys": [
    { "token": "tok-operator", "id": "alice", "roles": ["operator"] }
  ]
}`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write rbac config: %v", err)
	}
	return path
}

func TestRoleSetOfNil(t *testing.T) {
	if rs := roleSetOf(nil); rs != nil {
		t.Fatalf("roleSetOf(nil) = %v, want nil", rs)
	}
}

func TestRoleSetOfLoaded(t *testing.T) {
	kr, err := rbac.LoadConfig(writeRBACConfig(t))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	rs := roleSetOf(kr)
	if rs == nil {
		t.Fatal("roleSetOf(keyring) = nil, want the keyring's RoleSet")
	}
	// The returned RoleSet must be the keyring's own: operator must resolve to a
	// grant that allows the bare tool "github".
	grant := rs.Resolve(rbac.Principal{Roles: []string{"operator"}})
	if !grant.AllowsTool("github") {
		t.Fatalf("operator grant does not allow tool github; got %+v", grant)
	}
}

func TestEffectiveAuthToken(t *testing.T) {
	if got := effectiveAuthToken("t", nil); got != "t" {
		t.Fatalf("effectiveAuthToken(\"t\", nil) = %q, want \"t\"", got)
	}
	kr, err := rbac.LoadConfig(writeRBACConfig(t))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := effectiveAuthToken("t", kr); got != "" {
		t.Fatalf("effectiveAuthToken(\"t\", keyring) = %q, want \"\" (keyring supersedes)", got)
	}
}

func TestLoadConfigFromFlagPath(t *testing.T) {
	kr, err := rbac.LoadConfig(writeRBACConfig(t))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if kr == nil {
		t.Fatal("LoadConfig returned nil keyring")
	}
	p, ok := kr.Lookup("Bearer tok-operator")
	if !ok {
		t.Fatal("Lookup(Bearer tok-operator) failed")
	}
	if p.ID != "alice" {
		t.Fatalf("principal ID = %q, want alice", p.ID)
	}
}

// TestPermitsClosure exercises the exact closure shape runServe appends to the
// governance engine as builtin.ToolPermission{Permits: ...}.
func TestPermitsClosure(t *testing.T) {
	kr, err := rbac.LoadConfig(writeRBACConfig(t))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	permits := func(roles []string, tool string) bool {
		return kr.RoleSet().Resolve(rbac.Principal{Roles: roles}).AllowsTool(tool)
	}
	if !permits([]string{"operator"}, "github") {
		t.Fatal("operator should be permitted bare tool github")
	}
	if permits([]string{"operator"}, "filesystem") {
		t.Fatal("operator should NOT be permitted tool filesystem")
	}
	if permits([]string{"unknown"}, "github") {
		t.Fatal("unknown role should be permitted nothing")
	}
}
