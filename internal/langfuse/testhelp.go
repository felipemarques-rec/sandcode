// Package langfuse provides Langfuse integration for LLM observability.
package langfuse

import (
	"net/http"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// NewProviderForTest builds an Enabled() Provider backed by the given
// TracerProvider. Test-only — not for production code. It lives in a
// non-_test.go file (rather than export_test.go) precisely so that
// cross-package tests (e.g. internal/orchestrator) can import it; Go
// hides _test.go symbols from other packages' test binaries.
//
// The http client is set to a default client so that SubmitScore (which
// fires a goroutine) does not panic with a nil pointer. In tests the
// HTTP call will simply fail (no live Langfuse backend), which is fine.
func NewProviderForTest(tp *sdktrace.TracerProvider) *Provider {
	return &Provider{
		tp:     tp,
		tracer: tp.Tracer("sandcode"),
		config: Config{Enabled: true},
		http:   &http.Client{Timeout: 5 * time.Second},
	}
}
