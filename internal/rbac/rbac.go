// Package rbac provides role-based access control primitives: principals,
// roles, additive grants, and a constant-time bearer-token keyring. It is a
// leaf package with no HTTP, governance, orchestrator, or server dependency so
// it can be unit-tested in isolation and reused by any caller.
package rbac

import "crypto/subtle"

// Capability constants name the discrete actions an HTTP caller may perform.
// CapWildcard ("*") is the implicit grant-everything marker and is never a
// member of AllCapabilities.
const (
	CapRunCreate = "run:create"
	CapRunRead   = "run:read"
	CapRunCancel = "run:cancel"
	CapApprove   = "approve"
	CapAuditRead = "audit:read"
	CapWildcard  = "*"
)

// AllCapabilities returns the closed set of concrete capabilities (excluding
// the wildcard). Task 2's config loader uses this to reject unknown
// capabilities; the wildcard is always separately allowed.
func AllCapabilities() []string {
	return []string{
		CapRunCreate,
		CapRunRead,
		CapRunCancel,
		CapApprove,
		CapAuditRead,
	}
}

// Principal is an authenticated identity with zero or more roles. The
// unexported admin flag marks the implicit all-access identity returned by
// AdminPrincipal; it is only set through that constructor.
type Principal struct {
	ID    string
	Roles []string

	admin bool
}

// Grant is an additive permission set. A "*" element in any slice means "all"
// for that dimension (tools, capabilities, or endpoints).
//
// Endpoints is a forward-looking coarse route allowlist: it is parsed and
// resolved (unioned across roles) but NOT yet enforced — per-route access is
// gated by Capabilities today. It is reserved for a future endpoint-level gate.
type Grant struct {
	Tools        []string
	Capabilities []string
	Endpoints    []string
}

// RoleSet maps role names to their grants.
type RoleSet map[string]Grant

// KeyEntry binds a bearer token to the principal it authenticates. Task 2's
// loader builds these from config.
type KeyEntry struct {
	Token     string
	Principal Principal
}

// Keyring holds an ordered set of token->principal entries plus the RoleSet
// used to resolve a principal's effective grant. Construct with NewKeyring; it
// is safe for concurrent Lookup since it is immutable after construction.
type Keyring struct {
	roles   RoleSet
	entries []KeyEntry
}

// NewKeyring builds a Keyring from a RoleSet and ordered key entries. The
// entries slice is copied so the caller may reuse its backing array.
func NewKeyring(roles RoleSet, entries []KeyEntry) *Keyring {
	cp := make([]KeyEntry, len(entries))
	copy(cp, entries)
	return &Keyring{roles: roles, entries: cp}
}

// RoleSet returns the keyring's RoleSet for capability/tool checks.
func (k *Keyring) RoleSet() RoleSet {
	return k.roles
}

// Lookup matches a full Authorization header value ("Bearer <token>") against
// the keyring and returns the authenticated principal. It is CONSTANT-TIME with
// respect to which entry matches: it scans every entry on every call (no early
// exit) and selects the matched principal without a data-dependent branch,
// mirroring the withAuth discipline in internal/server. A miss returns
// (zero, false).
func (k *Keyring) Lookup(authzHeader string) (Principal, bool) {
	got := []byte(authzHeader)
	matched := 0
	var result Principal
	for i := range k.entries {
		expected := []byte("Bearer " + k.entries[i].Token)
		eq := subtle.ConstantTimeCompare(got, expected) // 1 on match, 0 otherwise
		// Branch-free select: copy the principal only when eq==1.
		if eq == 1 {
			result = k.entries[i].Principal
		}
		matched |= eq
	}
	return result, matched == 1
}

// AdminPrincipal returns the implicit all-access identity. Its resolved grant
// allows every tool and every capability even against an empty RoleSet, because
// RoleSet.Resolve short-circuits on the unexported admin flag.
func AdminPrincipal() Principal {
	return Principal{ID: "admin", admin: true}
}

// Resolve returns the additive union of grants for a principal's roles. An
// admin principal short-circuits to a synthetic grant allowing everything.
// Unknown roles contribute nothing. Resolve is a pure function.
func (rs RoleSet) Resolve(p Principal) Grant {
	if p.admin {
		return Grant{
			Tools:        []string{CapWildcard},
			Capabilities: []string{CapWildcard},
			Endpoints:    []string{CapWildcard},
		}
	}
	var out Grant
	for _, role := range p.Roles {
		g, ok := rs[role]
		if !ok {
			continue
		}
		out.Tools = append(out.Tools, g.Tools...)
		out.Capabilities = append(out.Capabilities, g.Capabilities...)
		out.Endpoints = append(out.Endpoints, g.Endpoints...)
	}
	return out
}

// AllowsTool reports whether the grant permits the named tool. A "*" member
// short-circuits true for any tool.
func (g Grant) AllowsTool(tool string) bool {
	return contains(g.Tools, tool)
}

// AllowsCapability reports whether the grant permits the named capability. A
// "*" member short-circuits true for any capability.
func (g Grant) AllowsCapability(cap string) bool {
	return contains(g.Capabilities, cap)
}

// PermittedTools returns the order-preserving intersection of cands with the
// grant: it keeps only candidates the grant allows, in input order.
func (g Grant) PermittedTools(cands []string) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		if g.AllowsTool(c) {
			out = append(out, c)
		}
	}
	return out
}

// contains reports membership, treating a "*" element as matching anything.
func contains(set []string, want string) bool {
	for _, s := range set {
		if s == CapWildcard || s == want {
			return true
		}
	}
	return false
}
