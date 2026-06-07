package agent

import "testing"

func TestClaudeCodeBuildCommand(t *testing.T) {
	c := NewClaudeCode()
	cmd := c.BuildCommand(RunOptions{
		Prompt:    "hello",
		Model:     "claude-haiku-4-5",
		SessionID: "abc",
		ExtraArgs: []string{"--debug"},
	})

	wantPrefix := []string{"claude", "--print", "--output-format=stream-json", "--verbose"}
	for i, w := range wantPrefix {
		if cmd.Argv[i] != w {
			t.Fatalf("argv[%d]=%q, want %q", i, cmd.Argv[i], w)
		}
	}
	hasModel := false
	hasSession := false
	hasDebug := false
	for i, a := range cmd.Argv {
		if a == "--model" && i+1 < len(cmd.Argv) && cmd.Argv[i+1] == "claude-haiku-4-5" {
			hasModel = true
		}
		if a == "--resume" && i+1 < len(cmd.Argv) && cmd.Argv[i+1] == "abc" {
			hasSession = true
		}
		if a == "--debug" {
			hasDebug = true
		}
	}
	if !hasModel || !hasSession || !hasDebug {
		t.Fatalf("missing flags: model=%v session=%v debug=%v argv=%v", hasModel, hasSession, hasDebug, cmd.Argv)
	}
}

func TestClaudeCodeParseLine(t *testing.T) {
	c := NewClaudeCode()

	cases := []struct {
		name string
		line string
		ok   bool
		kind EventKind
	}{
		{"empty", "", false, 0},
		{"non-json", "[INFO] starting", false, 0},
		{"system init session", `{"type":"system","subtype":"init","session_id":"sess-123"}`, true, EventSession},
		{"system non-init dropped", `{"type":"system","session_id":"sess-123"}`, false, 0},
		{"rate_limit_event dropped", `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}}`, false, 0},
		{"user tool_result dropped", `{"type":"user","message":{"role":"user","content":[{"type":"tool_result"}]}}`, false, 0},
		{"assistant text", `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`, true, EventText},
		{"assistant tool", `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"path":"foo"}}]}}`, true, EventToolCall},
		{"result with text", `{"type":"result","subtype":"success","result":"all done"}`, true, EventText},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, ok := c.ParseLine(tc.line)
			if ok != tc.ok {
				t.Fatalf("ok=%v, want %v (line=%q)", ok, tc.ok, tc.line)
			}
			if ok && ev.Kind != tc.kind {
				t.Fatalf("kind=%v, want %v", ev.Kind, tc.kind)
			}
		})
	}
}

func TestClaudeCodeAuthHints(t *testing.T) {
	c := NewClaudeCode()
	h := c.AuthHints()
	if !h.NeedsClaudeCredentials {
		t.Fatal("expected NeedsClaudeCredentials=true")
	}
	if len(h.AcceptedEnvVars) == 0 {
		t.Fatal("expected some accepted env vars")
	}
}
