package sandbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
)

// NoSandboxProvider runs commands directly on the host. Use only for unit
// tests and dry-runs — there is NO isolation. Mounts/Env are translated into
// the host process; limits are ignored.
type NoSandboxProvider struct{}

func NewNoSandboxProvider() *NoSandboxProvider { return &NoSandboxProvider{} }

func (*NoSandboxProvider) Name() string { return "nosandbox" }

func (*NoSandboxProvider) Create(_ context.Context, spec SandboxSpec) (Sandbox, error) {
	return &noSandbox{spec: spec}, nil
}

type noSandbox struct{ spec SandboxSpec }

func (s *noSandbox) Exec(ctx context.Context, argv []string, stdin io.Reader, opts ExecOptions) (<-chan ExecLine, Wait, error) {
	if len(argv) == 0 {
		return nil, nil, errors.New("nosandbox: empty argv")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// Translate container WorkDir back to its host source via the spec's mounts.
	// This keeps nosandbox honest about the bind-mount abstraction so tests can
	// exercise the orchestrator without spinning up a real container.
	if dir := resolveHostDir(s.spec); dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = os.Environ()
	for k, v := range s.spec.Env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}
	for k, v := range opts.Env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}
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
		return nil, nil, err
	}
	lines := make(chan ExecLine, 64)
	done := make(chan ExecResult, 1)
	var pipeWG sync.WaitGroup
	pipeWG.Add(2)
	go pipeLinesWG(stdout, StreamStdout, lines, &pipeWG)
	go pipeLinesWG(stderr, StreamStderr, lines, &pipeWG)
	go func() {
		// Pipe scanners MUST finish before cmd.Wait(). The os/exec docs
		// for StdoutPipe spell this out: Wait closes the parent's read
		// fds the moment the child exits, and any unread bytes still
		// buffered in the kernel pipe are lost. With a fast child like
		// `sh -c 'echo foo'` this race is reproducible (~0.3-3% under
		// load): the child writes its line and exits before the pipe
		// goroutine schedules its first Scanner.Scan(), Wait fires,
		// closes the read fd, and Scan returns EOF with nothing read.
		//
		// pipeWG.Wait() returns once Scanner.Scan() has seen EOF on
		// both streams — which happens naturally when the child closes
		// its stdout/stderr fds at exit. After that it is safe to call
		// cmd.Wait() to reap the process and close the pipes.
		pipeWG.Wait()
		err := cmd.Wait()
		close(lines)
		ec := 0
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				ec = ee.ExitCode()
			} else {
				ec = -1
			}
		}
		done <- ExecResult{ExitCode: ec, Err: err}
	}()
	return lines, Wait(func() ExecResult { return <-done }), nil
}

func pipeLinesWG(r io.Reader, stream Stream, out chan<- ExecLine, wg *sync.WaitGroup) {
	defer wg.Done()
	pipeLines(r, stream, out)
}

func pipeLines(r io.Reader, stream Stream, out chan<- ExecLine) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		out <- ExecLine{Stream: stream, Text: sc.Text()}
	}
}

func (*noSandbox) CopyIn(context.Context, string, string) error  { return nil }
func (*noSandbox) CopyOut(context.Context, string, string) error { return nil }
func (*noSandbox) Close(context.Context) error                   { return nil }

// resolveHostDir maps spec.WorkDir (a container path) to the host path that
// the bind-mount would expose. Falls back to spec.WorkDir itself when no
// mount matches, which is fine for tests that pass an existing host path.
func resolveHostDir(spec SandboxSpec) string {
	if spec.WorkDir == "" {
		return ""
	}
	for _, m := range spec.Mounts {
		if m.Target == spec.WorkDir {
			return m.Source
		}
	}
	if _, err := os.Stat(spec.WorkDir); err == nil {
		return spec.WorkDir
	}
	return ""
}
