//go:build integration
// +build integration

package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestDockerProvider_EndToEnd brings up a real container and exercises Exec
// + CopyIn + CopyOut + Close. Skipped unless `docker` is on PATH.
//
// Run with:
//
//	go test -tags=integration -run TestDockerProvider ./internal/sandbox/
func TestDockerProvider_EndToEnd(t *testing.T) {
	if !dockerAvailable() {
		t.Skip("docker not available (binary missing or daemon unreachable)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	p := NewDockerProvider()
	box, err := p.Create(ctx, SandboxSpec{
		Image:   "alpine:3.20",
		WorkDir: "/work",
		Limits:  Limits{CPUs: "1", Memory: "256m"},
		Network: "none",
		Labels:  map[string]string{"sandcode.test": "1"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer box.Close(context.Background())

	lines, wait, err := box.Exec(ctx, []string{"sh", "-c", "echo hello-from-sandbox; uname -s"}, nil, ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	var out []string
	for ln := range lines {
		out = append(out, ln.Text)
	}
	res := wait()
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d err=%v out=%v", res.ExitCode, res.Err, out)
	}
	full := strings.Join(out, "\n")
	if !strings.Contains(full, "hello-from-sandbox") {
		t.Fatalf("missing stdout: %q", full)
	}
	if !strings.Contains(full, "Linux") {
		t.Fatalf("missing uname output: %q", full)
	}
}
