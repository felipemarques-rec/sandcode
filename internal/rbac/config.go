package rbac

import (
	"encoding/json"
	"fmt"
	"os"
)

// fileConfig is the on-disk JSON shape of an RBAC config file. It is decoded
// into the exported RoleSet/[]KeyEntry surface by LoadConfig after validation.
type fileConfig struct {
	Roles map[string]roleGrant `json:"roles"`
	Keys  []keyEntry           `json:"keys"`
}

type roleGrant struct {
	Tools        []string `json:"tools"`
	Capabilities []string `json:"capabilities"`
	Endpoints    []string `json:"endpoints"`
}

type keyEntry struct {
	Token string   `json:"token"`
	ID    string   `json:"id"`
	Roles []string `json:"roles"`
}

// LoadConfig reads, parses, and validates an RBAC config JSON file and returns
// the resulting Keyring. It never panics; all failure modes (missing file,
// invalid JSON, unknown capability, undefined role, empty/duplicate key fields)
// return a non-nil, descriptive error.
//
// Validation rules:
//   - every capability in every role must be a member of AllCapabilities or the
//     wildcard "*";
//   - every key must have a non-empty token and a non-empty id;
//   - every role named by a key must be defined in roles;
//   - no two keys may share the same token.
func LoadConfig(path string) (*Keyring, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("rbac config: read %s: %w", path, err)
	}

	var fc fileConfig
	if err := json.Unmarshal(data, &fc); err != nil {
		return nil, fmt.Errorf("rbac config: parse %s: %w", path, err)
	}

	// Build the allowed-capability set once for O(1) membership checks.
	allowedCaps := make(map[string]struct{}, len(AllCapabilities())+1)
	for _, c := range AllCapabilities() {
		allowedCaps[c] = struct{}{}
	}
	allowedCaps[CapWildcard] = struct{}{}

	// Validate roles: every declared capability must be known or the wildcard.
	roleSet := make(RoleSet, len(fc.Roles))
	for name, rg := range fc.Roles {
		for _, c := range rg.Capabilities {
			if _, ok := allowedCaps[c]; !ok {
				return nil, fmt.Errorf("rbac config: role %q grants unknown capability %q", name, c)
			}
		}
		roleSet[name] = Grant{
			Tools:        rg.Tools,
			Capabilities: rg.Capabilities,
			Endpoints:    rg.Endpoints,
		}
	}

	// Validate keys and build entries.
	entries := make([]KeyEntry, 0, len(fc.Keys))
	seen := make(map[string]struct{}, len(fc.Keys))
	for i, k := range fc.Keys {
		if k.Token == "" {
			return nil, fmt.Errorf("rbac config: key %d (id %q) has an empty token", i, k.ID)
		}
		if k.ID == "" {
			return nil, fmt.Errorf("rbac config: key %d has an empty id", i)
		}
		// Identify the offending key by index/id rather than echoing the bearer
		// token verbatim — tokens are secrets and must not leak into error text.
		if _, dup := seen[k.Token]; dup {
			return nil, fmt.Errorf("rbac config: key %d (id %q) reuses a token already assigned to another key", i, k.ID)
		}
		seen[k.Token] = struct{}{}
		for _, role := range k.Roles {
			if _, ok := fc.Roles[role]; !ok {
				return nil, fmt.Errorf("rbac config: key %q references undefined role %q", k.ID, role)
			}
		}
		entries = append(entries, KeyEntry{
			Token:     k.Token,
			Principal: Principal{ID: k.ID, Roles: k.Roles},
		})
	}

	return NewKeyring(roleSet, entries), nil
}
