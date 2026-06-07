package main

import (
	"context"
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/langfuse"
)

// Default env (no LANGFUSE_ENABLED) → disabled provider; the launcher
// retains it and ExecuteOptions plumbing compiles. Guards G2 wiring
// without needing a live Langfuse backend.
func TestServe_LangfuseDisabledIsNoOp(t *testing.T) {
	p, err := langfuse.Init(context.Background(), langfuse.ConfigFromEnv())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if p.Enabled() {
		t.Fatal("langfuse must be disabled by default in tests (no LANGFUSE_ENABLED)")
	}
	l := &orchestratorLauncher{langfuse: p}
	if l.langfuse == nil {
		t.Fatal("launcher did not retain provider")
	}
}
