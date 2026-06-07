// Verifier role slot (SP2a). The Verifier runs post-implementation checks
// inside the live sandbox and reports pass/fail plus an output tail used as
// refine feedback. The DEFAULT implementation (cmdVerifier) reproduces the
// existing RunOptions.Refine.VerifyCmd behavior verbatim — deterministic, no
// LLM, no API key, no new deps. A custom Verifier set on RunOptions.Verifier
// overrides VerifyCmd entirely (the role slot wins).
//
// Event ownership stays in Run(): verify.started / verify.passed /
// verify.failed / refine.triggered are emitted by the attempt loop, NOT by
// the Verifier. The Verifier only decides pass/fail and produces the tail.
package orchestrator

import (
	"context"
	"strings"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/redact"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
)

// Verifier runs post-implementation checks on the agent's work inside the
// sandbox and reports pass/fail plus output for refine feedback.
type Verifier interface {
	Verify(ctx context.Context, in VerifyInput) (VerifyOutput, error)
}

// VerifyInput carries everything a Verifier needs for one verification pass.
type VerifyInput struct {
	Box       sandbox.Sandbox // the live (opened) sandbox the implementer ran in
	Attempt   int             // 1-based attempt number
	Timeout   time.Duration   // per-verify wall-clock cap; zero = inherit ctx
	TailBytes int             // cap on captured output (feeds buildRefinePrompt)
}

// VerifyOutput is a Verifier's verdict.
type VerifyOutput struct {
	Passed     bool // true if verification succeeded
	ExitCode   int
	StdoutTail string // truncated combined output
}

// cmdVerifier is the default Verifier: it runs a fixed command in the sandbox.
// This is the exact behavior of the legacy RunOptions.Refine.VerifyCmd path.
type cmdVerifier struct {
	cmd []string
}

func (c cmdVerifier) Verify(ctx context.Context, in VerifyInput) (VerifyOutput, error) {
	verifyCtx := ctx
	var cancel context.CancelFunc
	if in.Timeout > 0 {
		verifyCtx, cancel = context.WithTimeout(ctx, in.Timeout)
		defer cancel()
	}

	lines, wait, err := in.Box.Exec(verifyCtx, c.cmd, nil, sandbox.ExecOptions{})
	if err != nil {
		return VerifyOutput{}, err
	}
	var out strings.Builder
	for ln := range lines {
		out.WriteString(ln.Text)
		out.WriteByte('\n')
	}
	res := wait()
	return VerifyOutput{
		Passed:   res.ExitCode == 0,
		ExitCode: res.ExitCode,
		// Redact before the tail is logged, persisted in events, written to
		// REPORT.md, or fed to the refine prompt / LLM reviewer.
		StdoutTail: redact.Redact(tail(out.String(), in.TailBytes)),
	}, nil
}
