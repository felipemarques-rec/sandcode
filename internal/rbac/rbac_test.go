package rbac

import (
	"fmt"
	"reflect"
	"sort"
	"sync"
	"testing"
)

func TestKeyringLookupHit(t *testing.T) {
	p := Principal{ID: "alice", Roles: []string{"dev"}}
	k := NewKeyring(RoleSet{}, []KeyEntry{
		{Token: "tok-alice", Principal: p},
	})
	got, ok := k.Lookup("Bearer tok-alice")
	if !ok {
		t.Fatalf("expected hit, got miss")
	}
	if !reflect.DeepEqual(got, p) {
		t.Fatalf("got principal %+v, want %+v", got, p)
	}
}

func TestKeyringLookupMiss(t *testing.T) {
	k := NewKeyring(RoleSet{}, []KeyEntry{
		{Token: "tok-alice", Principal: Principal{ID: "alice"}},
	})
	cases := []string{
		"Bearer wrong",
		"tok-alice",         // missing "Bearer " prefix
		"",                  // empty
		"Bearer ",           // empty token
		"Bearer tok-alic",   // prefix of real token
		"Bearer tok-alice2", // superstring of real token
	}
	for _, h := range cases {
		if got, ok := k.Lookup(h); ok {
			t.Errorf("header %q: expected miss, got principal %+v", h, got)
		}
	}
}

func TestKeyringLookupEmptyKeyring(t *testing.T) {
	k := NewKeyring(RoleSet{}, nil)
	if _, ok := k.Lookup("Bearer anything"); ok {
		t.Fatalf("empty keyring should never match")
	}
}

func TestRoleSetResolveAdditiveUnion(t *testing.T) {
	rs := RoleSet{
		"a": {Tools: []string{"github"}, Capabilities: []string{CapRunCreate}},
		"b": {Tools: []string{"fs"}, Capabilities: []string{CapRunRead}},
	}
	p := Principal{ID: "bob", Roles: []string{"a", "b"}}
	g := rs.Resolve(p)
	if !g.AllowsTool("github") {
		t.Errorf("expected tool github allowed via role a")
	}
	if !g.AllowsTool("fs") {
		t.Errorf("expected tool fs allowed via role b")
	}
	if !g.AllowsCapability(CapRunCreate) {
		t.Errorf("expected cap run:create allowed via role a")
	}
	if !g.AllowsCapability(CapRunRead) {
		t.Errorf("expected cap run:read allowed via role b")
	}
	if g.AllowsTool("other") {
		t.Errorf("did not expect tool other")
	}
}

func TestRoleSetResolveUnknownRoleContributesNothing(t *testing.T) {
	rs := RoleSet{
		"a": {Tools: []string{"github"}},
	}
	p := Principal{ID: "bob", Roles: []string{"a", "ghost"}}
	g := rs.Resolve(p)
	if !g.AllowsTool("github") {
		t.Errorf("expected tool github from known role a")
	}
	if g.AllowsTool("anything") {
		t.Errorf("unknown role ghost should contribute nothing")
	}
}

func TestGrantAllowsToolWildcard(t *testing.T) {
	g := Grant{Tools: []string{"*"}}
	for _, tool := range []string{"github", "fs", "anything-at-all", ""} {
		if !g.AllowsTool(tool) {
			t.Errorf("wildcard grant should allow tool %q", tool)
		}
	}
}

func TestGrantAllowsToolExact(t *testing.T) {
	g := Grant{Tools: []string{"github", "fs"}}
	if !g.AllowsTool("github") || !g.AllowsTool("fs") {
		t.Errorf("expected exact membership to allow listed tools")
	}
	if g.AllowsTool("other") {
		t.Errorf("did not expect non-listed tool")
	}
}

func TestGrantAllowsCapabilityWildcardAndExact(t *testing.T) {
	wild := Grant{Capabilities: []string{"*"}}
	if !wild.AllowsCapability(CapApprove) || !wild.AllowsCapability("unknown") {
		t.Errorf("wildcard should allow any capability")
	}
	exact := Grant{Capabilities: []string{CapApprove, CapAuditRead}}
	if !exact.AllowsCapability(CapApprove) || !exact.AllowsCapability(CapAuditRead) {
		t.Errorf("expected listed capabilities allowed")
	}
	if exact.AllowsCapability(CapRunCancel) {
		t.Errorf("did not expect non-listed capability")
	}
}

func TestGrantPermittedToolsOrderPreservingIntersection(t *testing.T) {
	g := Grant{Tools: []string{"fs", "github"}}
	cands := []string{"github", "shell", "fs", "github"}
	got := g.PermittedTools(cands)
	want := []string{"github", "fs", "github"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestGrantPermittedToolsWildcard(t *testing.T) {
	g := Grant{Tools: []string{"*"}}
	cands := []string{"a", "b", "c"}
	got := g.PermittedTools(cands)
	if !reflect.DeepEqual(got, cands) {
		t.Fatalf("wildcard should permit all candidates in order: got %v want %v", got, cands)
	}
}

func TestAdminPrincipalAllowsEverythingViaEmptyRoleSet(t *testing.T) {
	rs := RoleSet{} // empty on purpose
	g := rs.Resolve(AdminPrincipal())
	for _, c := range AllCapabilities() {
		if !g.AllowsCapability(c) {
			t.Errorf("admin should allow capability %q", c)
		}
	}
	if !g.AllowsCapability("any-unknown-cap") {
		t.Errorf("admin should allow even unknown capabilities")
	}
	for _, tool := range []string{"github", "fs", "shell", "anything"} {
		if !g.AllowsTool(tool) {
			t.Errorf("admin should allow tool %q", tool)
		}
	}
}

func TestAllCapabilitiesIsClosedConcreteSet(t *testing.T) {
	got := AllCapabilities()
	want := []string{
		CapRunCreate, CapRunRead, CapRunCancel,
		CapApprove, CapAuditRead,
	}
	gs := append([]string(nil), got...)
	ws := append([]string(nil), want...)
	sort.Strings(gs)
	sort.Strings(ws)
	if !reflect.DeepEqual(gs, ws) {
		t.Fatalf("AllCapabilities = %v, want %v", got, want)
	}
	for _, c := range got {
		if c == CapWildcard {
			t.Errorf("AllCapabilities must not include the wildcard")
		}
	}
}

func TestKeyringLookupTableFullScan(t *testing.T) {
	const n = 50
	entries := make([]KeyEntry, 0, n)
	for i := 0; i < n; i++ {
		entries = append(entries, KeyEntry{
			Token:     fmt.Sprintf("tok-%d", i),
			Principal: Principal{ID: fmt.Sprintf("user-%d", i), Roles: []string{"r"}},
		})
	}
	k := NewKeyring(RoleSet{"r": {Tools: []string{"fs"}}}, entries)
	for i := 0; i < n; i++ {
		got, ok := k.Lookup(fmt.Sprintf("Bearer tok-%d", i))
		if !ok {
			t.Fatalf("token %d: expected hit", i)
		}
		if got.ID != fmt.Sprintf("user-%d", i) {
			t.Errorf("token %d: got id %q", i, got.ID)
		}
	}
	if _, ok := k.Lookup("Bearer tok-999"); ok {
		t.Errorf("absent token should miss")
	}
}

func TestKeyringLookupConcurrent(t *testing.T) {
	const n = 32
	entries := make([]KeyEntry, 0, n)
	for i := 0; i < n; i++ {
		entries = append(entries, KeyEntry{
			Token:     fmt.Sprintf("tok-%d", i),
			Principal: Principal{ID: fmt.Sprintf("user-%d", i)},
		})
	}
	k := NewKeyring(RoleSet{}, entries)

	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < n; i++ {
				got, ok := k.Lookup(fmt.Sprintf("Bearer tok-%d", i))
				if !ok || got.ID != fmt.Sprintf("user-%d", i) {
					t.Errorf("concurrent lookup token %d failed: %+v ok=%v", i, got, ok)
				}
			}
			_, _ = k.Lookup("Bearer nope")
		}()
	}
	wg.Wait()
}

func TestKeyringRoleSetAccessor(t *testing.T) {
	rs := RoleSet{"r": {Tools: []string{"fs"}}}
	k := NewKeyring(rs, nil)
	got := k.RoleSet()
	if !got["r"].AllowsTool("fs") {
		t.Errorf("RoleSet accessor should return the configured RoleSet")
	}
}
