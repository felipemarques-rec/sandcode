package main

import (
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/scheduler"
)

func TestBuildSchedulerConfig(t *testing.T) {
	// Both > 0 => config built.
	if c := buildSchedulerConfig(4, 256); c == nil || c.PoolSize != 4 || c.QueueCap != 256 {
		t.Fatalf("both>0: got %+v want {4 256}", c)
	}
	// Either 0 => nil (scheduler disabled).
	if c := buildSchedulerConfig(0, 256); c != nil {
		t.Fatalf("poolSize 0 => want nil, got %+v", c)
	}
	if c := buildSchedulerConfig(4, 0); c != nil {
		t.Fatalf("queueCap 0 => want nil, got %+v", c)
	}
	var _ *scheduler.Config = buildSchedulerConfig(1, 1) // type assertion
}
