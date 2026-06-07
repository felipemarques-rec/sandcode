package sandbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// DockerProvider implements Provider on top of the `docker` CLI. It is the
// most portable option because we don't link against a specific docker SDK
// version and Podman is wire-compatible.
type DockerProvider struct {
	// Binary is the docker CLI to invoke. Defaults to "docker".
	Binary string
}

// NewDockerProvider returns a provider using the `docker` binary.
func NewDockerProvider() *DockerProvider { return &DockerProvider{Binary: "docker"} }

// NewPodmanProvider returns a provider using the `podman` binary. Podman's CLI
// is wire-compatible with docker for the subset of commands we use.
func NewPodmanProvider() *DockerProvider { return &DockerProvider{Binary: "podman"} }

func (p *DockerProvider) Name() string { return p.Binary }

func (p *DockerProvider) bin() string {
	if p.Binary == "" {
		return "docker"
	}
	return p.Binary
}

// Create launches a long-lived container in detached mode that we then exec
// into. We run `sleep infinity` as PID 1 and tear it down on Close. This
// matches sandcastle's bind-mount Docker lifecycle.
func (p *DockerProvider) Create(ctx context.Context, spec SandboxSpec) (Sandbox, error) {
	if spec.Image == "" {
		return nil, errors.New("sandbox: spec.Image is required")
	}
	name := "sandcode-" + uuid.New().String()[:8]

	args := []string{"run", "--detach", "--rm", "--name", name}
	if spec.WorkDir != "" {
		args = append(args, "--workdir", spec.WorkDir)
	}
	if spec.User != "" {
		args = append(args, "--user", spec.User)
	}
	if spec.Network != "" {
		args = append(args, "--network", spec.Network)
	}
	if spec.Limits.CPUs != "" {
		args = append(args, "--cpus", spec.Limits.CPUs)
	}
	if spec.Limits.Memory != "" {
		args = append(args, "--memory", spec.Limits.Memory)
	}
	if spec.Limits.PidsLimit > 0 {
		args = append(args, "--pids-limit", strconv.Itoa(spec.Limits.PidsLimit))
	}
	for _, m := range spec.Mounts {
		mode := "rw"
		if m.ReadOnly {
			mode = "ro"
		}
		args = append(args, "--volume", fmt.Sprintf("%s:%s:%s", m.Source, m.Target, mode))
	}
	for k, v := range spec.Env {
		args = append(args, "--env", fmt.Sprintf("%s=%s", k, v))
	}
	for k, v := range spec.Labels {
		args = append(args, "--label", fmt.Sprintf("%s=%s", k, v))
	}
	args = append(args, spec.Image, "sleep", "infinity")

	cmd := exec.CommandContext(ctx, p.bin(), args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("docker run failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return &dockerSandbox{
		bin:           p.bin(),
		containerName: name,
		spec:          spec,
	}, nil
}

type dockerSandbox struct {
	bin           string
	containerName string
	spec          SandboxSpec
}

func (s *dockerSandbox) Exec(ctx context.Context, cmdArgs []string, stdin io.Reader, opts ExecOptions) (<-chan ExecLine, Wait, error) {
	args := []string{"exec", "--interactive"}
	if opts.Tty {
		args = append(args, "--tty")
	}
	if s.spec.WorkDir != "" {
		args = append(args, "--workdir", s.spec.WorkDir)
	}
	for k, v := range opts.Env {
		args = append(args, "--env", fmt.Sprintf("%s=%s", k, v))
	}
	args = append(args, s.containerName)
	args = append(args, cmdArgs...)

	cmd := exec.CommandContext(ctx, s.bin, args...)
	if stdin != nil {
		cmd.Stdin = stdin
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("docker exec start: %w", err)
	}

	lines := make(chan ExecLine, 64)
	done := make(chan ExecResult, 1)
	var pipeWG sync.WaitGroup
	pipeWG.Add(2)
	go scanIntoWG(stdout, StreamStdout, lines, &pipeWG)
	go scanIntoWG(stderr, StreamStderr, lines, &pipeWG)

	go func() {
		// Pipe scanners MUST finish before cmd.Wait(). See the matching
		// comment in nosandbox.go: os/exec.Wait closes the parent's
		// read fds the moment the child exits, truncating any unread
		// bytes still buffered in the pipe. Draining first guarantees
		// we surface every line the agent wrote — even when the
		// command is short-lived (`echo foo`-style scripts).
		pipeWG.Wait()
		err := cmd.Wait()
		close(lines)
		exitCode := 0
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				exitCode = ee.ExitCode()
			} else {
				exitCode = -1
			}
		}
		done <- ExecResult{ExitCode: exitCode, Err: err}
	}()

	wait := Wait(func() ExecResult { return <-done })
	return lines, wait, nil
}

func scanInto(r io.Reader, stream Stream, out chan<- ExecLine) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		out <- ExecLine{Stream: stream, Text: sc.Text()}
	}
}

func scanIntoWG(r io.Reader, stream Stream, out chan<- ExecLine, wg *sync.WaitGroup) {
	defer wg.Done()
	scanInto(r, stream, out)
}

func (s *dockerSandbox) CopyIn(ctx context.Context, srcHost, dstContainer string) error {
	cmd := exec.CommandContext(ctx, s.bin, "cp", srcHost, fmt.Sprintf("%s:%s", s.containerName, dstContainer))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker cp in: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (s *dockerSandbox) CopyOut(ctx context.Context, srcContainer, dstHost string) error {
	cmd := exec.CommandContext(ctx, s.bin, "cp", fmt.Sprintf("%s:%s", s.containerName, srcContainer), dstHost)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker cp out: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (s *dockerSandbox) Close(ctx context.Context) error {
	// `docker rm -f` because the container was started with --rm; this stops
	// it and triggers cleanup.
	cmd := exec.CommandContext(ctx, s.bin, "rm", "-f", s.containerName)
	out, err := cmd.CombinedOutput()
	if err != nil && !strings.Contains(string(out), "No such container") {
		return fmt.Errorf("docker rm: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
