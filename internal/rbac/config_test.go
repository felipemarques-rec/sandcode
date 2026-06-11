package rbac

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeTemp writes content to a temp file in t.TempDir() and returns its path.
func writeTemp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "rbac.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return p
}

const validConfigJSON = `{
  "roles": {
    "admin":    { "tools": ["*"], "capabilities": ["*"], "endpoints": ["*"] },
    "operator": { "tools": ["github","fs"], "capabilities": ["run:create","run:read","run:cancel"] },
    "approver": { "capabilities": ["run:read","approve","audit:read"] },
    "viewer":   { "capabilities": ["run:read"] }
  },
  "keys": [ { "token": "tok_alice", "id": "alice", "roles": ["admin"] } ]
}`

func TestConfigValidParses(t *testing.T) {
	path := writeTemp(t, validConfigJSON)
	kr, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: unexpected error: %v", err)
	}
	if kr == nil {
		t.Fatal("LoadConfig returned nil keyring")
	}

	p, ok := kr.Lookup("Bearer tok_alice")
	if !ok {
		t.Fatal("Lookup(Bearer tok_alice) miss, want hit")
	}
	want := Principal{ID: "alice", Roles: []string{"admin"}}
	if p.ID != want.ID || !reflect.DeepEqual(p.Roles, want.Roles) {
		t.Fatalf("principal = %+v, want %+v", p, want)
	}

	rs := kr.RoleSet()
	for _, role := range []string{"admin", "operator", "approver", "viewer"} {
		if _, ok := rs[role]; !ok {
			t.Errorf("RoleSet missing role %q", role)
		}
	}
	op := rs["operator"]
	if !reflect.DeepEqual(op.Tools, []string{"github", "fs"}) {
		t.Errorf("operator.Tools = %v, want [github fs]", op.Tools)
	}
	if !reflect.DeepEqual(op.Capabilities, []string{"run:create", "run:read", "run:cancel"}) {
		t.Errorf("operator.Capabilities = %v", op.Capabilities)
	}
	admin := rs["admin"]
	if !reflect.DeepEqual(admin.Capabilities, []string{"*"}) {
		t.Errorf("admin.Capabilities = %v, want [*]", admin.Capabilities)
	}
}

func TestConfigUndefinedRole(t *testing.T) {
	cfg := `{
  "roles": { "admin": { "capabilities": ["*"] } },
  "keys": [ { "token": "tok_x", "id": "x", "roles": ["ghost"] } ]
}`
	_, err := LoadConfig(writeTemp(t, cfg))
	if err == nil {
		t.Fatal("want error for undefined role, got nil")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error %q should mention offending role %q", err, "ghost")
	}
}

func TestConfigDuplicateToken(t *testing.T) {
	cfg := `{
  "roles": { "admin": { "capabilities": ["*"] } },
  "keys": [
    { "token": "dupe", "id": "a", "roles": ["admin"] },
    { "token": "dupe", "id": "b", "roles": ["admin"] }
  ]
}`
	_, err := LoadConfig(writeTemp(t, cfg))
	if err == nil {
		t.Fatal("want error for duplicate token, got nil")
	}
	// The error must flag the reuse and locate the offending key (id "b"),
	// but must NOT echo the bearer token value itself.
	if !strings.Contains(err.Error(), "token") || !strings.Contains(err.Error(), "b") {
		t.Errorf("error %q should flag the token reuse and the offending key id", err)
	}
	if strings.Contains(err.Error(), "dupe") {
		t.Errorf("error %q must not leak the bearer token value", err)
	}
}

func TestConfigEmptyToken(t *testing.T) {
	cfg := `{
  "roles": { "admin": { "capabilities": ["*"] } },
  "keys": [ { "token": "", "id": "a", "roles": ["admin"] } ]
}`
	_, err := LoadConfig(writeTemp(t, cfg))
	if err == nil {
		t.Fatal("want error for empty token, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "token") {
		t.Errorf("error %q should mention token", err)
	}
}

func TestConfigEmptyID(t *testing.T) {
	cfg := `{
  "roles": { "admin": { "capabilities": ["*"] } },
  "keys": [ { "token": "tok", "id": "", "roles": ["admin"] } ]
}`
	_, err := LoadConfig(writeTemp(t, cfg))
	if err == nil {
		t.Fatal("want error for empty id, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "id") {
		t.Errorf("error %q should mention id", err)
	}
}

func TestConfigUnknownCapability(t *testing.T) {
	cfg := `{
  "roles": { "weird": { "capabilities": ["run:delete"] } },
  "keys": [ { "token": "tok", "id": "a", "roles": ["weird"] } ]
}`
	_, err := LoadConfig(writeTemp(t, cfg))
	if err == nil {
		t.Fatal("want error for unknown capability, got nil")
	}
	if !strings.Contains(err.Error(), "run:delete") {
		t.Errorf("error %q should mention bad capability %q", err, "run:delete")
	}
	if !strings.Contains(err.Error(), "weird") {
		t.Errorf("error %q should mention offending role %q", err, "weird")
	}
}

func TestConfigWildcardCapabilityAllowed(t *testing.T) {
	cfg := `{
  "roles": { "admin": { "capabilities": ["*"] } },
  "keys": [ { "token": "tok", "id": "a", "roles": ["admin"] } ]
}`
	if _, err := LoadConfig(writeTemp(t, cfg)); err != nil {
		t.Fatalf("wildcard capability should be allowed, got error: %v", err)
	}
}

func TestConfigFileNotFound(t *testing.T) {
	_, err := LoadConfig(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err == nil {
		t.Fatal("want error for missing file, got nil")
	}
}

func TestConfigInvalidJSON(t *testing.T) {
	_, err := LoadConfig(writeTemp(t, `{ this is not json `))
	if err == nil {
		t.Fatal("want error for invalid JSON, got nil")
	}
}
