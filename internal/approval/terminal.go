package approval

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
)

// TerminalApprover resolves an approval by prompting on a terminal (CLI mode).
// It reads a single y/N line from In and writes the prompt to Out.
type TerminalApprover struct {
	In  io.Reader
	Out io.Writer
}

// RequestApproval prints the request and reads one y/N line. Anything other
// than y/yes (including EOF) rejects. Honors ctx: if it fires before a line
// is read, returns ctx.Err() (the pending read goroutine is abandoned — the
// CLI process is about to fail-closed anyway).
func (a *TerminalApprover) RequestApproval(ctx context.Context, req Request) (Decision, error) {
	fmt.Fprintf(a.Out, "\n[sandcode] run %s requires approval (action=%s, attempt=%d)\n",
		req.RunID, req.Action, req.Attempt)
	for _, reason := range req.Reasons {
		fmt.Fprintf(a.Out, "  - %s\n", reason)
	}
	fmt.Fprint(a.Out, "Approve? [y/N]: ")

	type lineResult struct {
		line string
		err  error
	}
	resCh := make(chan lineResult, 1)
	go func() {
		line, err := bufio.NewReader(a.In).ReadString('\n')
		resCh <- lineResult{line, err}
	}()

	select {
	case <-ctx.Done():
		return Decision{}, ctx.Err()
	case res := <-resCh:
		ans := strings.ToLower(strings.TrimSpace(res.line))
		if ans == "y" || ans == "yes" {
			return Decision{Approved: true, Approver: "terminal"}, nil
		}
		return Decision{Approved: false, Reason: "rejected at terminal"}, nil
	}
}
