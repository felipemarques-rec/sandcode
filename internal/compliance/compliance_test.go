package compliance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/governance"
)

func sampleInput() ReportInput {
	at := time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC)
	return ReportInput{
		Run: RunIdentity{
			ID:        "run-1",
			Agent:     "claude",
			Prompt:    "deploy with key AKIAIOSFODNN7EXAMPLE now",
			Status:    "success",
			StartedAt: at,
			ExitCode:  0,
		},
		TraceID: "abc123",
		AuditRows: []governance.AuditRow{
			{RunID: "run-1", ActionType: "agent.apply_patch", Result: governance.Allow, CreatedAt: at},
			{RunID: "run-1", ActionType: "agent.apply_patch", Result: governance.Review,
				PolicyName: "diff_size", Reasons: []string{"diff too large"}, CreatedAt: at.Add(time.Second)},
			{RunID: "run-1", ActionType: "agent.apply_patch", Result: governance.Approved,
				Approver: "alice", CreatedAt: at.Add(2 * time.Second)},
		},
		Now: at.Add(3 * time.Second),
	}
}

func TestBuild_BasicShapeAndSummary(t *testing.T) {
	rep := Build(sampleInput())

	if rep.SchemaVersion != SchemaVersion {
		t.Fatalf("schema = %q, want %q", rep.SchemaVersion, SchemaVersion)
	}
	if !rep.GeneratedAt.Equal(sampleInput().Now) {
		t.Fatalf("GeneratedAt = %v, want injected Now", rep.GeneratedAt)
	}
	if rep.TraceID != "abc123" {
		t.Fatalf("TraceID = %q", rep.TraceID)
	}
	if rep.Summary.Total != 3 {
		t.Fatalf("Summary.Total = %d, want 3", rep.Summary.Total)
	}
	if rep.Summary.ByResult["allow"] != 1 || rep.Summary.ByResult["review"] != 1 || rep.Summary.ByResult["approved"] != 1 {
		t.Fatalf("ByResult = %+v", rep.Summary.ByResult)
	}
	if rep.Summary.PoliciesFired != 1 {
		t.Fatalf("PoliciesFired = %d, want 1 (diff_size)", rep.Summary.PoliciesFired)
	}
}

func TestBuild_RedactsPrompt(t *testing.T) {
	rep := Build(sampleInput())
	if got := rep.Run.Prompt; got == "" || containsSecret(got) {
		t.Fatalf("prompt not redacted: %q", got)
	}
}

func containsSecret(s string) bool {
	return len(s) >= 20 && (indexOf(s, "AKIAIOSFODNN7EXAMPLE") >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

func TestBuild_IntegrityReverifies(t *testing.T) {
	rep := Build(sampleInput())
	if rep.Integrity.Algorithm != "sha256" {
		t.Fatalf("algorithm = %q", rep.Integrity.Algorithm)
	}
	b, err := json.Marshal(rep.Decisions)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	if want := hex.EncodeToString(sum[:]); rep.Integrity.Digest != want {
		t.Fatalf("digest = %q, want %q", rep.Integrity.Digest, want)
	}
}

func TestBuild_MutationChangesDigest(t *testing.T) {
	a := Build(sampleInput())
	in := sampleInput()
	in.AuditRows[1].Reasons = []string{"tampered"}
	b := Build(in)
	if a.Integrity.Digest == b.Integrity.Digest {
		t.Fatal("digest unchanged after mutating a decision")
	}
}

func TestBuild_EmptyAudit(t *testing.T) {
	in := sampleInput()
	in.AuditRows = nil
	rep := Build(in)
	if rep.Summary.Total != 0 {
		t.Fatalf("Total = %d, want 0", rep.Summary.Total)
	}
	if rep.Integrity.Digest == "" {
		t.Fatal("digest empty for empty audit; want hash of empty array")
	}
}
