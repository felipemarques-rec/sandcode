package planner

import (
	"errors"
	"reflect"
	"testing"
)

func TestTaskDAG_Validate_HappyPath(t *testing.T) {
	d := TaskDAG{Nodes: []Node{
		{ID: "a", Prompt: "first"},
		{ID: "b", Prompt: "second", DependsOn: []string{"a"}},
		{ID: "c", Prompt: "third", DependsOn: []string{"a", "b"}},
	}}
	if err := d.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestTaskDAG_Validate_RejectsBadShapes(t *testing.T) {
	tests := []struct {
		name string
		dag  TaskDAG
		want error
	}{
		{"empty dag", TaskDAG{}, ErrEmptyDAG},
		{"empty id", TaskDAG{Nodes: []Node{{ID: "", Prompt: "x"}}}, ErrEmptyID},
		{"empty prompt", TaskDAG{Nodes: []Node{{ID: "a", Prompt: ""}}}, ErrEmptyPrompt},
		{"duplicate id", TaskDAG{Nodes: []Node{
			{ID: "a", Prompt: "x"},
			{ID: "a", Prompt: "y"},
		}}, ErrDuplicateID},
		{"unknown dep", TaskDAG{Nodes: []Node{
			{ID: "a", Prompt: "x", DependsOn: []string{"ghost"}},
		}}, ErrUnknownDep},
		{"self dep", TaskDAG{Nodes: []Node{
			{ID: "a", Prompt: "x", DependsOn: []string{"a"}},
		}}, ErrSelfDependency},
		{"cycle 2-node", TaskDAG{Nodes: []Node{
			{ID: "a", Prompt: "x", DependsOn: []string{"b"}},
			{ID: "b", Prompt: "y", DependsOn: []string{"a"}},
		}}, ErrCycle},
		{"cycle 3-node", TaskDAG{Nodes: []Node{
			{ID: "a", Prompt: "x", DependsOn: []string{"c"}},
			{ID: "b", Prompt: "y", DependsOn: []string{"a"}},
			{ID: "c", Prompt: "z", DependsOn: []string{"b"}},
		}}, ErrCycle},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.dag.Validate()
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestTaskDAG_Roots(t *testing.T) {
	d := TaskDAG{Nodes: []Node{
		{ID: "a", Prompt: "x"},
		{ID: "b", Prompt: "x", DependsOn: []string{"a"}},
		{ID: "c", Prompt: "x"}, // also a root
		{ID: "d", Prompt: "x", DependsOn: []string{"b", "c"}},
	}}
	got := d.Roots()
	want := []string{"a", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Roots() = %v, want %v", got, want)
	}
}

func TestTaskDAG_TopoSort_LinearAndDiamond(t *testing.T) {
	tests := []struct {
		name string
		dag  TaskDAG
		// We assert ID ordering invariants rather than a single
		// expected slice — multiple valid topo orders exist, but
		// ours is stable on input order within each level.
		mustBefore [][2]string
	}{
		{
			"linear a→b→c",
			TaskDAG{Nodes: []Node{
				{ID: "a", Prompt: "x"},
				{ID: "b", Prompt: "x", DependsOn: []string{"a"}},
				{ID: "c", Prompt: "x", DependsOn: []string{"b"}},
			}},
			[][2]string{{"a", "b"}, {"b", "c"}},
		},
		{
			"diamond a→{b,c}→d",
			TaskDAG{Nodes: []Node{
				{ID: "a", Prompt: "x"},
				{ID: "b", Prompt: "x", DependsOn: []string{"a"}},
				{ID: "c", Prompt: "x", DependsOn: []string{"a"}},
				{ID: "d", Prompt: "x", DependsOn: []string{"b", "c"}},
			}},
			[][2]string{{"a", "b"}, {"a", "c"}, {"b", "d"}, {"c", "d"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.dag.TopoSort()
			if err != nil {
				t.Fatalf("TopoSort: %v", err)
			}
			if len(got) != len(tc.dag.Nodes) {
				t.Fatalf("len(got)=%d, want %d", len(got), len(tc.dag.Nodes))
			}
			pos := make(map[string]int, len(got))
			for i, id := range got {
				pos[id] = i
			}
			for _, pair := range tc.mustBefore {
				if pos[pair[0]] >= pos[pair[1]] {
					t.Errorf("%s should come before %s; got order=%v", pair[0], pair[1], got)
				}
			}
		})
	}
}

func TestTaskDAG_TopoSort_RejectsCycle(t *testing.T) {
	d := TaskDAG{Nodes: []Node{
		{ID: "a", Prompt: "x", DependsOn: []string{"b"}},
		{ID: "b", Prompt: "y", DependsOn: []string{"a"}},
	}}
	if _, err := d.TopoSort(); !errors.Is(err, ErrCycle) {
		t.Errorf("err = %v, want ErrCycle", err)
	}
}
