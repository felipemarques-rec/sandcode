package reactor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/event"
)

const (
	cmdReq event.Type = "test.request"
	resRep event.Type = "test.reply"
)

// TestServeRequestReply_RoundTrip — a Serve'd handler replies over the bus and
// RequestReply receives the correlated result.
func TestServeRequestReply_RoundTrip(t *testing.T) {
	bus := event.NewLocalBus()
	t.Cleanup(func() { _ = bus.Close() })

	sub := Serve(bus, cmdReq, func(_ context.Context, cmd event.Event) (event.Event, error) {
		return event.New(resRep, cmd.RunID, []byte(`{"ok":true}`)), nil
	})
	defer sub.Cancel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := event.New(cmdReq, "run1", nil)
	res, err := RequestReply(ctx, bus, cmd, resRep)
	if err != nil {
		t.Fatalf("RequestReply: %v", err)
	}
	if res.Type != resRep {
		t.Fatalf("result type = %s, want %s", res.Type, resRep)
	}
	if res.CausationID != cmd.ID {
		t.Fatalf("result not correlated: CausationID=%q, want cmd.ID=%q", res.CausationID, cmd.ID)
	}
	if res.RunID != "run1" {
		t.Fatalf("result RunID defaulted wrong: %q", res.RunID)
	}
}

// TestRequestReply_TimesOutWithoutHandler — no Serve'd handler ⇒ the dispatcher
// surfaces a context timeout rather than hanging.
func TestRequestReply_TimesOutWithoutHandler(t *testing.T) {
	bus := event.NewLocalBus()
	t.Cleanup(func() { _ = bus.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := RequestReply(ctx, bus, event.New(cmdReq, "run1", nil), resRep)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

// TestRequestReply_CorrelationIsolation — two in-flight requests each get THEIR
// OWN reply (no cross-wiring), proving CausationID correlation.
func TestRequestReply_CorrelationIsolation(t *testing.T) {
	bus := event.NewLocalBus()
	t.Cleanup(func() { _ = bus.Close() })

	// Handler echoes the command's RunID into the reply payload so we can tell
	// the two replies apart.
	sub := Serve(bus, cmdReq, func(_ context.Context, cmd event.Event) (event.Event, error) {
		return event.New(resRep, cmd.RunID, []byte(`"`+cmd.RunID+`"`)), nil
	})
	defer sub.Cancel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cmdA := event.New(cmdReq, "A", nil)
	cmdB := event.New(cmdReq, "B", nil)
	resA, errA := RequestReply(ctx, bus, cmdA, resRep)
	resB, errB := RequestReply(ctx, bus, cmdB, resRep)
	if errA != nil || errB != nil {
		t.Fatalf("errA=%v errB=%v", errA, errB)
	}
	if resA.CausationID != cmdA.ID || string(resA.Payload) != `"A"` {
		t.Errorf("reply A mismatched: causation=%q payload=%s", resA.CausationID, resA.Payload)
	}
	if resB.CausationID != cmdB.ID || string(resB.Payload) != `"B"` {
		t.Errorf("reply B mismatched: causation=%q payload=%s", resB.CausationID, resB.Payload)
	}
}

// TestServe_HandlerErrorProducesNoReply — a handler error yields no reply, so
// the dispatcher times out (errors are not silently treated as success).
func TestServe_HandlerErrorProducesNoReply(t *testing.T) {
	bus := event.NewLocalBus()
	t.Cleanup(func() { _ = bus.Close() })

	sub := Serve(bus, cmdReq, func(_ context.Context, _ event.Event) (event.Event, error) {
		return event.Event{}, errors.New("handler failed")
	})
	defer sub.Cancel()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := RequestReply(ctx, bus, event.New(cmdReq, "run1", nil), resRep); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want timeout (no reply on handler error)", err)
	}
}
