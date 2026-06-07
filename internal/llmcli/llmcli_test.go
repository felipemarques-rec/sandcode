package llmcli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeClaude writes a stub `claude` executable that consumes stdin, prints out,
// and exits with code. It returns the script path for Client.Bin.
func fakeClaude(t *testing.T, out string, code int) string {
	t.Helper()
	dir := t.TempDir()
	outFile := filepath.Join(dir, "out.json")
	if err := os.WriteFile(outFile, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "claude")
	body := "#!/bin/sh\ncat >/dev/null\ncat " + outFile + "\nexit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// sleepyClaude writes a stub that sleeps before printing, to exercise timeouts.
func sleepyClaude(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "claude")
	body := "#!/bin/sh\ncat >/dev/null\nsleep 5\necho '{}'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// envelope wraps a result string the way `claude --output-format json` does.
func envelope(result string) string {
	b, _ := json.Marshal(struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		IsError bool   `json:"is_error"`
		Result  string `json:"result"`
	}{"result", "success", false, result})
	return string(b)
}

var schema = map[string]any{
	"type":       "object",
	"properties": map[string]any{"score": map[string]any{"type": "number"}},
	"required":   []string{"score"},
}

func TestStructured_CleanJSON(t *testing.T) {
	c := &Client{Bin: fakeClaude(t, envelope(`{"score":0.8}`), 0), Timeout: 5 * time.Second}
	raw, err := c.Structured(context.Background(), "sys", "user", "rank", schema)
	if err != nil {
		t.Fatalf("Structured: %v", err)
	}
	var got struct {
		Score float64 `json:"score"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Score != 0.8 {
		t.Fatalf("score = %v, want 0.8", got.Score)
	}
}

func TestStructured_FencedJSON(t *testing.T) {
	result := "```json\n{\"score\":0.5}\n```"
	c := &Client{Bin: fakeClaude(t, envelope(result), 0), Timeout: 5 * time.Second}
	raw, err := c.Structured(context.Background(), "", "user", "", schema)
	if err != nil {
		t.Fatalf("Structured: %v", err)
	}
	if !json.Valid(raw) || !strings.HasPrefix(strings.TrimSpace(string(raw)), "{") {
		t.Fatalf("expected clean JSON object, got %q", raw)
	}
}

func TestStructured_ProseEmbedded(t *testing.T) {
	result := `Here is my answer: {"score": 0.3} — hope that helps!`
	c := &Client{Bin: fakeClaude(t, envelope(result), 0), Timeout: 5 * time.Second}
	raw, err := c.Structured(context.Background(), "", "user", "", schema)
	if err != nil {
		t.Fatalf("Structured: %v", err)
	}
	var got struct {
		Score float64 `json:"score"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", raw, err)
	}
	if got.Score != 0.3 {
		t.Fatalf("score = %v, want 0.3", got.Score)
	}
}

func TestStructured_IsErrorEnvelope(t *testing.T) {
	out := `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"boom"}`
	c := &Client{Bin: fakeClaude(t, out, 0), Timeout: 5 * time.Second}
	if _, err := c.Structured(context.Background(), "", "u", "", schema); err == nil {
		t.Fatal("expected error for is_error envelope")
	}
}

func TestStructured_NonJSONResult(t *testing.T) {
	c := &Client{Bin: fakeClaude(t, envelope("I could not produce JSON."), 0), Timeout: 5 * time.Second}
	if _, err := c.Structured(context.Background(), "", "u", "", schema); err == nil {
		t.Fatal("expected error for non-JSON result")
	}
}

func TestStructured_NonZeroExit(t *testing.T) {
	c := &Client{Bin: fakeClaude(t, "", 1), Timeout: 5 * time.Second}
	if _, err := c.Structured(context.Background(), "", "u", "", schema); err == nil {
		t.Fatal("expected error for non-zero exit")
	}
}

func TestStructured_Timeout(t *testing.T) {
	c := &Client{Bin: sleepyClaude(t), Timeout: 100 * time.Millisecond}
	if _, err := c.Structured(context.Background(), "", "u", "", schema); err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestBuildPrompt_IncludesSchemaAndUser(t *testing.T) {
	p, err := buildPrompt("SYSTEM", "USERMSG", "rank", schema)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"SYSTEM", "USERMSG", "rank", `"score"`, "JSON object"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
}

func TestExtractJSONObject_BracesInStrings(t *testing.T) {
	// A string value containing braces must not confuse the balancer.
	in := `prefix {"msg":"a } b { c","n":1} suffix`
	raw, err := extractJSONObject(in)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	var got struct {
		Msg string `json:"msg"`
		N   int    `json:"n"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal %q: %v", raw, err)
	}
	if got.Msg != "a } b { c" || got.N != 1 {
		t.Fatalf("got %+v", got)
	}
}
