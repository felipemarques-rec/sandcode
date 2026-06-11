// Package planner provides LLM-based decomposition of a user prompt
// into a TaskDAG of subtasks. The DAG is the canonical contract between
// the planner and any downstream executor (kernel today, multi-step
// orchestrator in Stage 4).
//
// Slice 1 ships the type system + a no-op fallback + the Anthropic LLM
// implementation. Kernel integration, strategy selection, and DAG
// execution are out of scope — see the master plan §9.1 and §6 for the
// staging.
package planner

import (
	"errors"
	"fmt"
)

// Node is a single subtask in a TaskDAG. IDs are caller-assigned and
// must be unique within a DAG; DependsOn references other Node IDs in
// the same DAG.
//
// Role is a free-form label naming which agent role (Implementer,
// Reviewer, etc.) should execute the node — empty means "use the
// default agent". Slice 1 does not interpret this field; Slice 3+
// (multi-role coordination) will.
//
// DoD (Definition of Done) is an optional, concrete, checkable acceptance
// criterion for the node — the "Definition of Done" box from the diagram's
// Setup Phase, made first-class per subtask. When set, the DAG executor
// surfaces it to the agent as an explicit acceptance criterion (see
// buildHandoffPrompt). Empty means "the prompt is the implicit DoD" — the
// legacy behavior (byte-identical).
type Node struct {
	ID        string   `json:"id"`
	Prompt    string   `json:"prompt"`
	Role      string   `json:"role,omitempty"`
	DoD       string   `json:"dod,omitempty"`
	DependsOn []string `json:"depends_on,omitempty"`
}

// TaskDAG is a directed acyclic graph of subtasks. The zero value is
// invalid; callers must populate Nodes and call Validate before use.
type TaskDAG struct {
	Nodes []Node `json:"nodes"`
}

// Validation errors. Sentinels so callers can branch on cause.
var (
	ErrEmptyDAG       = errors.New("planner: dag has no nodes")
	ErrEmptyID        = errors.New("planner: node has empty id")
	ErrDuplicateID    = errors.New("planner: duplicate node id")
	ErrUnknownDep     = errors.New("planner: dependency references unknown node id")
	ErrSelfDependency = errors.New("planner: node depends on itself")
	ErrCycle          = errors.New("planner: dependency cycle detected")
	ErrEmptyPrompt    = errors.New("planner: node has empty prompt")
)

// Validate reports structural errors in d. A DAG is valid when:
//
//   - Nodes is non-empty.
//   - Every node has a non-empty ID and Prompt.
//   - IDs are unique across nodes.
//   - Every DependsOn entry references an existing node ID.
//   - No node depends on itself.
//   - The DependsOn relation is acyclic.
//
// Validation is strict: callers receive the first error encountered
// rather than a list, because the planner LLM's job is to emit a clean
// DAG. Partial recovery would mask bugs in the schema.
func (d TaskDAG) Validate() error {
	if len(d.Nodes) == 0 {
		return ErrEmptyDAG
	}
	ids := make(map[string]struct{}, len(d.Nodes))
	for _, n := range d.Nodes {
		if n.ID == "" {
			return ErrEmptyID
		}
		if n.Prompt == "" {
			return fmt.Errorf("%w: id=%s", ErrEmptyPrompt, n.ID)
		}
		if _, dup := ids[n.ID]; dup {
			return fmt.Errorf("%w: %s", ErrDuplicateID, n.ID)
		}
		ids[n.ID] = struct{}{}
	}
	for _, n := range d.Nodes {
		for _, dep := range n.DependsOn {
			if dep == n.ID {
				return fmt.Errorf("%w: %s", ErrSelfDependency, n.ID)
			}
			if _, ok := ids[dep]; !ok {
				return fmt.Errorf("%w: %s -> %s", ErrUnknownDep, n.ID, dep)
			}
		}
	}
	if cycle := findCycle(d.Nodes); cycle != "" {
		return fmt.Errorf("%w: %s", ErrCycle, cycle)
	}
	return nil
}

// Roots returns the IDs of nodes with no DependsOn — the entry points
// of the DAG. Order matches the input Nodes slice (stable).
func (d TaskDAG) Roots() []string {
	out := make([]string, 0)
	for _, n := range d.Nodes {
		if len(n.DependsOn) == 0 {
			out = append(out, n.ID)
		}
	}
	return out
}

// TopoSort returns the node IDs in dependency order: every node appears
// after all of its DependsOn. Returns an error if the DAG is cyclic —
// callers should usually Validate() first, which already catches this.
//
// Algorithm: Kahn's. O(V+E). Stable on input order within each level
// so callers get deterministic output.
func (d TaskDAG) TopoSort() ([]string, error) {
	indeg := make(map[string]int, len(d.Nodes))
	depsOf := make(map[string][]string, len(d.Nodes))
	for _, n := range d.Nodes {
		indeg[n.ID] = len(n.DependsOn)
		depsOf[n.ID] = n.DependsOn
	}
	// reverse adjacency: for each "u depends on v", v -> [u, ...]
	dependents := make(map[string][]string, len(d.Nodes))
	for _, n := range d.Nodes {
		for _, dep := range n.DependsOn {
			dependents[dep] = append(dependents[dep], n.ID)
		}
	}

	// Seed the ready queue with the roots, preserving input order.
	ready := make([]string, 0)
	for _, n := range d.Nodes {
		if indeg[n.ID] == 0 {
			ready = append(ready, n.ID)
		}
	}

	out := make([]string, 0, len(d.Nodes))
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		out = append(out, id)
		for _, child := range dependents[id] {
			indeg[child]--
			if indeg[child] == 0 {
				ready = append(ready, child)
			}
		}
	}
	if len(out) != len(d.Nodes) {
		return nil, ErrCycle
	}
	return out, nil
}

// findCycle returns the offending node ID when nodes contain a cycle,
// empty when acyclic. DFS with three-color marking.
func findCycle(nodes []Node) string {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int, len(nodes))
	deps := make(map[string][]string, len(nodes))
	for _, n := range nodes {
		deps[n.ID] = n.DependsOn
	}
	var visit func(id string) string
	visit = func(id string) string {
		switch color[id] {
		case gray:
			return id // back edge
		case black:
			return ""
		}
		color[id] = gray
		for _, dep := range deps[id] {
			if hit := visit(dep); hit != "" {
				return hit
			}
		}
		color[id] = black
		return ""
	}
	for _, n := range nodes {
		if hit := visit(n.ID); hit != "" {
			return hit
		}
	}
	return ""
}
