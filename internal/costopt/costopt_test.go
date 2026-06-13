package costopt

import (
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/brain"
)

func TestRuleRouter_Route(t *testing.T) {
	cases := []struct {
		name string
		c    brain.Classification
		want string
	}{
		{"low convergent → fast", brain.Classification{Type: brain.Convergent, Complexity: brain.ComplexityLow}, ModelFast},
		{"medium convergent → balanced", brain.Classification{Type: brain.Convergent, Complexity: brain.ComplexityMedium}, ModelBalanced},
		{"high convergent → strong", brain.Classification{Type: brain.Convergent, Complexity: brain.ComplexityHigh}, ModelStrong},
		{"low divergent → strong", brain.Classification{Type: brain.Divergent, Complexity: brain.ComplexityLow}, ModelStrong},
		{"medium divergent → strong", brain.Classification{Type: brain.Divergent, Complexity: brain.ComplexityMedium}, ModelStrong},
		{"high divergent → strong", brain.Classification{Type: brain.Divergent, Complexity: brain.ComplexityHigh}, ModelStrong},
	}
	r := New()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model, reason := r.Route(tc.c)
			if model != tc.want {
				t.Fatalf("model = %q, want %q", model, tc.want)
			}
			if reason == "" {
				t.Fatal("reason must be non-empty")
			}
		})
	}
}
