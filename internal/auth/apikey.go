package auth

import (
	"fmt"
	"os"

	"github.com/felipemarques-rec/sandcode/internal/agent"
	"github.com/felipemarques-rec/sandcode/internal/sandbox"
)

// APIKey injects an API key from the host environment into the sandbox.
// Useful when running >2 parallel agents (where bind-mount of a single
// OAuth subscription becomes unreliable) or when no Claude Code login exists.
type APIKey struct{}

func NewAPIKey() *APIKey { return &APIKey{} }

func (*APIKey) Name() string { return "apikey" }

func (*APIKey) Apply(spec *sandbox.SandboxSpec, hints agent.AuthHints) error {
	if len(hints.AcceptedEnvVars) == 0 {
		return nil
	}
	if spec.Env == nil {
		spec.Env = map[string]string{}
	}
	matched := false
	for _, name := range hints.AcceptedEnvVars {
		if v := os.Getenv(name); v != "" {
			spec.Env[name] = v
			matched = true
		}
	}
	if !matched {
		return fmt.Errorf("auth(apikey): none of %v set in host environment", hints.AcceptedEnvVars)
	}
	return nil
}
