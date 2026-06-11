package main

import (
	"io"
	"strings"
	"testing"
)

// TestKernelFlags_Registered verifies the E1.7 flags exist on `run` and
// default to false (off ⇒ byte-identical legacy kernel path).
func TestKernelFlags_Registered(t *testing.T) {
	cmd := newRunCmd()
	for _, name := range []string{"plan", "strategy-select", "reactive"} {
		fl := cmd.Flags().Lookup(name)
		if fl == nil {
			t.Fatalf("flag --%s not registered", name)
		}
		if fl.DefValue != "false" {
			t.Errorf("flag --%s default = %q, want false", name, fl.DefValue)
		}
	}
}

// TestKernelFlags_RequireLearn verifies --plan and --strategy-select are
// gated on --learn (they wire kernel stages, which only exist on the
// --learn path). The gate fires before any sandbox/worktree work.
func TestKernelFlags_RequireLearn(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"plan without learn", []string{"--plan", "do a thing"}, "--plan requires --learn"},
		{"strategy-select without learn", []string{"--strategy-select", "do a thing"}, "--strategy-select requires --learn"},
		{"reactive without learn", []string{"--reactive", "do a thing"}, "--reactive requires --learn"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newRunCmd()
			cmd.SetArgs(tc.args)
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			err := cmd.Execute()
			if err == nil {
				t.Fatal("expected gating error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}
