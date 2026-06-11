package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/governance"
)

func TestToolPermission(t *testing.T) {
	t.Parallel()
	permitAll := func(_ []string, _ string) bool { return true }
	permitNone := func(_ []string, _ string) bool { return false }

	cases := []struct {
		name    string
		permits func([]string, string) bool
		action  governance.Action
		want    governance.Result
	}{
		{"empty tool allows", permitNone, governance.Action{Roles: []string{"admin"}}, governance.Allow},
		{"empty roles allows", permitNone, governance.Action{Tool: "mcp__ctx7"}, governance.Allow},
		{"both empty allows", permitNone, governance.Action{}, governance.Allow},
		{"permitted allows", permitAll, governance.Action{Tool: "mcp__ctx7", Roles: []string{"dev"}}, governance.Allow},
		{"not permitted denies", permitNone, governance.Action{Tool: "mcp__ctx7", Roles: []string{"dev"}}, governance.Deny},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := ToolPermission{Permits: tc.permits}
			got, _, err := p.Evaluate(context.Background(), tc.action)
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got=%s want=%s", got, tc.want)
			}
		})
	}
}

func TestToolPermission_DenyReasonMentionsTool(t *testing.T) {
	t.Parallel()
	p := ToolPermission{Permits: func(_ []string, _ string) bool { return false }}
	a := governance.Action{Tool: "mcp__context7", Roles: []string{"viewer"}}
	got, reason, err := p.Evaluate(context.Background(), a)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if got != governance.Deny {
		t.Fatalf("got=%s want=%s", got, governance.Deny)
	}
	if !strings.Contains(reason, "mcp__context7") {
		t.Fatalf("reason %q does not contain tool name", reason)
	}
}

func TestToolPermission_Deterministic(t *testing.T) {
	t.Parallel()
	p := ToolPermission{Permits: func(_ []string, _ string) bool { return false }}
	a := governance.Action{Tool: "mcp__context7", Roles: []string{"viewer"}}
	r1, reason1, err1 := p.Evaluate(context.Background(), a)
	r2, reason2, err2 := p.Evaluate(context.Background(), a)
	if err1 != nil || err2 != nil {
		t.Fatalf("Evaluate errors: %v %v", err1, err2)
	}
	if r1 != r2 || reason1 != reason2 {
		t.Fatalf("non-deterministic: (%s,%q) != (%s,%q)", r1, reason1, r2, reason2)
	}
}
