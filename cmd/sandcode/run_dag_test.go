package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateDAGFlags_DAGFromFileWithoutDAG_Errors(t *testing.T) {
	t.Parallel()
	f := runFlags{}
	f.dagFromFile = "/some/path.json"
	if err := validateDAGFlags(f); err == nil {
		t.Errorf("expected error: --dag-from-file without --dag")
	} else if !strings.Contains(err.Error(), "--dag") {
		t.Errorf("error should mention --dag, got: %v", err)
	}
}

func TestValidateDAGFlags_ParallelPlusMultiAgent_Errors(t *testing.T) {
	t.Parallel()
	f := runFlags{}
	f.dag = true
	f.parallel = 2
	f.agentName = "claude,codex"
	if err := validateDAGFlags(f); err == nil {
		t.Errorf("expected error: --parallel + multi-agent")
	}
}

func TestValidateDAGFlags_DagAlone_Valid(t *testing.T) {
	t.Parallel()
	f := runFlags{}
	f.dag = true
	if err := validateDAGFlags(f); err != nil {
		t.Errorf("--dag alone should validate: %v", err)
	}
}

func TestValidateDAGFlags_DagWithFile_Valid(t *testing.T) {
	t.Parallel()
	f := runFlags{}
	f.dag = true
	f.dagFromFile = "/tmp/plan.json"
	if err := validateDAGFlags(f); err != nil {
		t.Errorf("--dag --dag-from-file should validate: %v", err)
	}
}

func TestValidateDAGFlags_DagWithParallel_Valid(t *testing.T) {
	t.Parallel()
	f := runFlags{}
	f.dag = true
	f.parallel = 3
	if err := validateDAGFlags(f); err != nil {
		t.Errorf("--dag --parallel 3 should validate: %v", err)
	}
}

func TestValidateDAGFlags_DagWithMultiAgentNoParallel_Valid(t *testing.T) {
	t.Parallel()
	f := runFlags{}
	f.dag = true
	f.parallel = 1
	f.agentName = "claude,codex"
	if err := validateDAGFlags(f); err != nil {
		t.Errorf("--dag with multi-agent only (parallel=1) should validate: %v", err)
	}
}

func TestLoadDAGFromFile_LoadsAndValidates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.json")

	plan := map[string]any{
		"nodes": []map[string]any{
			{"id": "a", "prompt": "do thing"},
		},
	}
	data, _ := json.Marshal(plan)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadDAGFromFile(path)
	if err != nil {
		t.Fatalf("loadDAGFromFile: %v", err)
	}
	if len(loaded.Nodes) != 1 || loaded.Nodes[0].ID != "a" {
		t.Errorf("loaded plan unexpected: %+v", loaded)
	}
}

func TestLoadDAGFromFile_RejectsMissing(t *testing.T) {
	t.Parallel()
	if _, err := loadDAGFromFile("/nonexistent/path.json"); err == nil {
		t.Errorf("expected error on missing file")
	}
}

func TestLoadDAGFromFile_RejectsInvalidJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDAGFromFile(path); err == nil {
		t.Errorf("expected error on invalid JSON")
	}
}

func TestLoadDAGFromFile_RejectsCyclicPlan(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cycle.json")
	plan := map[string]any{
		"nodes": []map[string]any{
			{"id": "a", "prompt": "a", "depends_on": []string{"b"}},
			{"id": "b", "prompt": "b", "depends_on": []string{"a"}},
		},
	}
	data, _ := json.Marshal(plan)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDAGFromFile(path); err == nil {
		t.Errorf("expected validation error on cyclic plan")
	}
}
