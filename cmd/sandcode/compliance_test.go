package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/governance"
	"github.com/felipemarques-rec/sandcode/internal/store"
)

func seedRun(t *testing.T, dir string) {
	t.Helper()
	st, err := store.Open(dir + "/.sandcode/store.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	al, err := governance.OpenAuditLog(dir + "/.sandcode/audit.db")
	if err != nil {
		t.Fatal(err)
	}
	defer al.Close()
	ctx := context.Background()
	if err := st.CreateRun(ctx, store.Run{
		ID: "run-1", Agent: "claude", Prompt: "do work", Status: store.StatusSuccess, StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := al.Append(ctx, governance.AuditRow{
		RunID: "run-1", ActionType: "agent.apply_patch", Result: governance.Approved, Approver: "alice",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestComplianceCmd_JSON(t *testing.T) {
	dir := t.TempDir()
	seedRun(t, dir)
	cmd := newComplianceCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"run-1", "--cwd", dir, "--format", "json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out.String())
	}
	if m["schema_version"] != "1.0" {
		t.Fatalf("schema_version = %v", m["schema_version"])
	}
}

func TestComplianceCmd_Markdown(t *testing.T) {
	dir := t.TempDir()
	seedRun(t, dir)
	cmd := newComplianceCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"run-1", "--cwd", dir, "--format", "md"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "# Compliance Report — run run-1") {
		t.Fatalf("missing markdown header:\n%s", out.String())
	}
}

func TestComplianceCmd_BadFormat(t *testing.T) {
	cmd := newComplianceCmd()
	cmd.SetArgs([]string{"run-1", "--format", "pdf"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error for bad --format")
	}
}
