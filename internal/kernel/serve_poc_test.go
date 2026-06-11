package kernel

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/felipemarques-rec/sandcode/internal/brain"
	"github.com/felipemarques-rec/sandcode/internal/event"
	"github.com/felipemarques-rec/sandcode/internal/reactor"
)

// TestServe_ClassifyRoundTrip (SP3.3) proves the distribution seam with REAL
// production code: the classifier runs as a relocatable bus-subscriber handler
// (reactor.Serve) and a dispatcher reaches it via reactor.RequestReply. The same
// code path runs over NATS in M6 — only the bus implementation changes.
//
// Unlike the in-process reactive path (SP3.0/3.1) where commands carry no prompt
// (closure capture), a relocatable handler can't see a closure, so the command
// carries its input. (Over a distributed bus this payload would be redacted.)
func TestServe_ClassifyRoundTrip(t *testing.T) {
	const prompt = "design the distributed architecture migration"

	bus := event.NewLocalBus()
	t.Cleanup(func() { _ = bus.Close() })

	classifier := brain.NewClassifier()

	// The classify handler lives as a bus subscriber: it reads the prompt from
	// the command, runs the real classifier, and replies with run.classified.
	sub := reactor.Serve(bus, event.ClassifyRequested, func(_ context.Context, cmd event.Event) (event.Event, error) {
		var in struct {
			Prompt string `json:"prompt"`
		}
		_ = json.Unmarshal(cmd.Payload, &in)
		c := classifier.Classify(context.Background(), in.Prompt)
		payload, _ := json.Marshal(classificationPayload{
			Type:       string(c.Type),
			Complexity: string(c.Complexity),
			Reasoning:  c.Reasoning,
		})
		return event.New(event.RunClassified, cmd.RunID, payload), nil
	})
	defer sub.Cancel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cmdPayload, _ := json.Marshal(map[string]string{"prompt": prompt})
	cmd := event.New(event.ClassifyRequested, "run-serve", cmdPayload).WithCorrelation("run-serve")
	res, err := reactor.RequestReply(ctx, bus, cmd, event.RunClassified)
	if err != nil {
		t.Fatalf("RequestReply: %v", err)
	}

	if res.CausationID != cmd.ID {
		t.Fatalf("reply not correlated: CausationID=%q want %q", res.CausationID, cmd.ID)
	}

	// The served result matches a direct classification of the same prompt.
	want := classifier.Classify(context.Background(), prompt)
	var got struct {
		Type       string `json:"type"`
		Complexity string `json:"complexity"`
	}
	if err := json.Unmarshal(res.Payload, &got); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}
	if got.Type != string(want.Type) || got.Complexity != string(want.Complexity) {
		t.Fatalf("served classification %+v != direct %+v", got, want)
	}
	// sanity: the chosen prompt really exercises the divergent/high path.
	if want.Complexity != brain.ComplexityHigh {
		t.Fatalf("test prompt no longer high-complexity: %+v", want)
	}
}
