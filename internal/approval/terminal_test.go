package approval

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

func TestTerminalApprover_Yes(t *testing.T) {
	a := &TerminalApprover{In: strings.NewReader("y\n"), Out: io.Discard}
	d, err := a.RequestApproval(context.Background(), Request{RunID: "r", Action: "execute", Reasons: []string{"cost high"}})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !d.Approved {
		t.Fatal("y should approve")
	}
}

func TestTerminalApprover_NoAndEOF(t *testing.T) {
	for _, in := range []string{"n\n", ""} {
		a := &TerminalApprover{In: strings.NewReader(in), Out: io.Discard}
		d, err := a.RequestApproval(context.Background(), Request{RunID: "r"})
		if err != nil {
			t.Fatalf("in=%q err = %v", in, err)
		}
		if d.Approved {
			t.Fatalf("in=%q should reject", in)
		}
	}
}

func TestTerminalApprover_ContextCancel(t *testing.T) {
	pr, _ := io.Pipe() // never written → ReadString blocks
	a := &TerminalApprover{In: pr, Out: io.Discard}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := a.RequestApproval(ctx, Request{RunID: "r"})
	if err == nil {
		t.Fatal("expected ctx error when no input arrives")
	}
}
