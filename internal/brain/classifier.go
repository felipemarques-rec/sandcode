package brain

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ProblemType classifies a prompt for strategy selection.
type ProblemType string

const (
	// Convergent problems have a single clear solution (bug fix, simple feature).
	Convergent ProblemType = "convergent"

	// Divergent problems are exploratory and benefit from multiple approaches.
	Divergent ProblemType = "divergent"
)

// Classification is the result of analyzing a prompt.
type Classification struct {
	Type       ProblemType
	Complexity Complexity
	Reasoning  string
}

// Complexity indicates how hard a task is likely to be.
type Complexity string

const (
	ComplexityLow    Complexity = "low"
	ComplexityMedium Complexity = "medium"
	ComplexityHigh   Complexity = "high"
)

// Classifier determines the problem type from a prompt.
// Phase 1 is rule-based (deterministic, no LLM call).
type Classifier struct{}

// NewClassifier creates a rule-based classifier.
func NewClassifier() *Classifier {
	return &Classifier{}
}

// Classify analyzes a prompt and returns its classification.
func (c *Classifier) Classify(_ context.Context, prompt string) Classification {
	lower := strings.ToLower(prompt)

	pType := c.classifyType(lower)
	complexity := c.classifyComplexity(lower)

	return Classification{
		Type:       pType,
		Complexity: complexity,
		Reasoning:  fmt.Sprintf("type=%s (rule-based), complexity=%s", pType, complexity),
	}
}

func (c *Classifier) classifyType(prompt string) ProblemType {
	// Divergent indicators: exploration, design, multiple solutions
	divergentSignals := []string{
		"design", "architect", "explore", "brainstorm", "propose",
		"compare", "evaluate", "alternative", "approach", "strategy",
		"refactor", "redesign", "improve architecture", "best way",
		"what if", "how should", "multiple", "options",
	}
	divergentCount := 0
	for _, signal := range divergentSignals {
		if strings.Contains(prompt, signal) {
			divergentCount++
		}
	}

	// Convergent indicators: specific action, bug fix, well-defined task
	convergentSignals := []string{
		"fix", "bug", "error", "add", "implement", "create", "update",
		"delete", "remove", "rename", "move", "change", "set",
		"install", "configure", "test", "write test",
	}
	convergentCount := 0
	for _, signal := range convergentSignals {
		if strings.Contains(prompt, signal) {
			convergentCount++
		}
	}

	if divergentCount > convergentCount {
		return Divergent
	}
	return Convergent
}

func (c *Classifier) classifyComplexity(prompt string) Complexity {
	wordCount := len(strings.Fields(prompt))

	highSignals := []string{
		"migration", "distributed", "architecture", "refactor entire",
		"redesign", "multi-service", "breaking change", "cross-cutting",
		"performance optimization", "security audit",
	}
	for _, signal := range highSignals {
		if strings.Contains(prompt, signal) {
			return ComplexityHigh
		}
	}

	if wordCount > 100 {
		return ComplexityHigh
	}
	if wordCount > 40 {
		return ComplexityMedium
	}
	return ComplexityLow
}

// ScanProjectDocs reads project documentation files for Grill with Docs.
// It looks for CONTEXT.md, README.md, and docs/adr/*.md in the project root.
func ScanProjectDocs(cwd string) string {
	var parts []string

	// CONTEXT.md — primary domain context
	if content, err := os.ReadFile(filepath.Join(cwd, "CONTEXT.md")); err == nil {
		parts = append(parts, "### CONTEXT.md\n"+truncateDoc(string(content), 2000))
	}

	// README.md — project overview
	if content, err := os.ReadFile(filepath.Join(cwd, "README.md")); err == nil {
		parts = append(parts, "### README.md\n"+truncateDoc(string(content), 1500))
	}

	// docs/adr/*.md — Architecture Decision Records
	adrDir := filepath.Join(cwd, "docs", "adr")
	if entries, err := os.ReadDir(adrDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			if content, err := os.ReadFile(filepath.Join(adrDir, e.Name())); err == nil {
				parts = append(parts, "### ADR: "+e.Name()+"\n"+truncateDoc(string(content), 500))
			}
			if len(parts) > 10 { // cap ADR count
				break
			}
		}
	}

	if len(parts) == 0 {
		return "(no project documentation found — consider creating CONTEXT.md)"
	}
	return strings.Join(parts, "\n\n")
}

func truncateDoc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n...(truncated)"
}
