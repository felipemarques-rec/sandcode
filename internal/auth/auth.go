// Package auth wires authentication credentials into a SandboxSpec so the
// agent inside the container can reach Anthropic / its provider.
package auth

import (
	"github.com/felipemarques-rec/sandcode/internal/agent"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
)

// Provider applies auth materials to a SandboxSpec.
type Provider interface {
	Name() string
	Apply(spec *sandbox.SandboxSpec, hints agent.AuthHints) error
}
