package strategy

import (
	"testing"

	"github.com/felipemarques-rec/sandcode/internal/brain"
	"github.com/felipemarques-rec/sandcode/internal/planner"
)

func TestRuleSelector_Select(t *testing.T) {
	multiRoot := planner.TaskDAG{Nodes: []planner.Node{
		{ID: "a", Prompt: "x"},
		{ID: "b", Prompt: "y"},
	}}
	linearChain := planner.TaskDAG{Nodes: []planner.Node{
		{ID: "a", Prompt: "x"},
		{ID: "b", Prompt: "y", DependsOn: []string{"a"}},
	}}
	singleRoot := planner.TaskDAG{Nodes: []planner.Node{
		{ID: "root", Prompt: "x"},
	}}
	empty := planner.TaskDAG{}

	tests := []struct {
		name      string
		cls       brain.Classification
		plan      planner.TaskDAG
		wantStrat Strategy
		// reason contains: substring assertion; empty = don't check.
		reasonHas string
	}{
		{
			"multi-root wins over high complexity",
			brain.Classification{Complexity: brain.ComplexityHigh},
			multiRoot,
			StrategyParallel,
			"multiple roots",
		},
		{
			"single-root chain + high complexity → refine",
			brain.Classification{Complexity: brain.ComplexityHigh},
			linearChain,
			StrategyRefine,
			"high complexity",
		},
		{
			"single-root + high complexity → refine",
			brain.Classification{Complexity: brain.ComplexityHigh},
			singleRoot,
			StrategyRefine,
			"high complexity",
		},
		{
			"empty plan + high complexity → refine",
			brain.Classification{Complexity: brain.ComplexityHigh},
			empty,
			StrategyRefine,
			"high complexity",
		},
		{
			"empty plan + low complexity → single",
			brain.Classification{Complexity: brain.ComplexityLow},
			empty,
			StrategySingle,
			"default",
		},
		{
			"single-root + medium → single (no rule fires)",
			brain.Classification{Complexity: brain.ComplexityMedium},
			singleRoot,
			StrategySingle,
			"default",
		},
		{
			"multi-root + low → parallel (DAG shape outranks complexity)",
			brain.Classification{Complexity: brain.ComplexityLow},
			multiRoot,
			StrategyParallel,
			"multiple roots",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := New().Select(tc.cls, tc.plan)
			if got != tc.wantStrat {
				t.Errorf("Select = %s, want %s", got, tc.wantStrat)
			}
			if tc.reasonHas != "" && !containsLower(reason, tc.reasonHas) {
				t.Errorf("reason = %q, want substring %q", reason, tc.reasonHas)
			}
		})
	}
}

func TestRuleSelector_DeterministicForSameInputs(t *testing.T) {
	// "same input → same output" is part of the documented contract.
	// Run Select many times and confirm no jitter.
	cls := brain.Classification{Complexity: brain.ComplexityHigh}
	plan := planner.TaskDAG{Nodes: []planner.Node{{ID: "root", Prompt: "x"}}}

	firstStrat, firstReason := New().Select(cls, plan)
	for i := 0; i < 100; i++ {
		s, r := New().Select(cls, plan)
		if s != firstStrat || r != firstReason {
			t.Fatalf("non-deterministic at iter %d: (%s,%q) vs (%s,%q)",
				i, s, r, firstStrat, firstReason)
		}
	}
}

func containsLower(s, sub string) bool {
	// Tiny helper that avoids importing strings just for ToLower.
	if len(sub) > len(s) {
		return false
	}
	// Case-insensitive substring search.
	for i := 0; i+len(sub) <= len(s); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			a, b := s[i+j], sub[j]
			if 'A' <= a && a <= 'Z' {
				a += 32
			}
			if 'A' <= b && b <= 'Z' {
				b += 32
			}
			if a != b {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
