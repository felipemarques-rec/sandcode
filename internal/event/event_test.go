package event

import "testing"

func TestDAGEventConstants(t *testing.T) {
	cases := []struct {
		got  Type
		want string
	}{
		{DAGStarted, "dag.started"},
		{DAGChainStarted, "dag.chain_started"},
		{DAGNodeStarted, "dag.node_started"},
		{DAGNodeCompleted, "dag.node_completed"},
		{DAGChainCompleted, "dag.chain_completed"},
		{DAGSynthesisStarted, "dag.synthesis_started"},
		{DAGSynthesisCompleted, "dag.synthesis_completed"},
		{DAGCompleted, "dag.completed"},
	}
	for _, c := range cases {
		if string(c.got) != c.want {
			t.Errorf("constant string mismatch: got %q want %q", c.got, c.want)
		}
	}
}
