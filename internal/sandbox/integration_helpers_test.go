//go:build integration
// +build integration

package sandbox

import (
	"context"
	"os/exec"
	"time"
)

// dockerAvailable returns true when the docker binary is on PATH AND can
// reach a daemon. WSL distros without Docker Desktop integration ship a
// stub binary that errors out — checking only LookPath gives false positives.
func dockerAvailable() bool {
	if _, err := exec.LookPath("docker"); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	if len(out) == 0 {
		return false
	}
	return true
}
