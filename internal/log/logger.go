// Package log provides structured logging with correlation IDs.
//
// All sandcode components should use log.Logger(ctx) instead of fmt.Printf.
// The correlation ID propagates through the entire run lifecycle, linking
// logs across orchestrator, sandbox, brain, and store operations.
package log

import (
	"context"
	"log/slog"
	"os"
	"sync"
)

type contextKey string

const correlationKey contextKey = "correlation_id"

var (
	defaultLogger *slog.Logger
	once          sync.Once
)

// Init sets up the global logger. format is "json" or "text".
// Safe to call multiple times; only the first call takes effect.
func Init(format string, level slog.Level) {
	once.Do(func() {
		opts := &slog.HandlerOptions{Level: level}
		var handler slog.Handler
		switch format {
		case "json":
			handler = slog.NewJSONHandler(os.Stderr, opts)
		default:
			handler = slog.NewTextHandler(os.Stderr, opts)
		}
		defaultLogger = slog.New(handler)
		slog.SetDefault(defaultLogger)
	})
}

// WithCorrelation injects a correlation ID into the context. Every
// subsequent log.Logger(ctx) call includes this ID automatically.
func WithCorrelation(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationKey, id)
}

// CorrelationID extracts the correlation ID from the context, or "" if absent.
func CorrelationID(ctx context.Context) string {
	if id, ok := ctx.Value(correlationKey).(string); ok {
		return id
	}
	return ""
}

// Logger returns a slog.Logger enriched with the correlation ID from ctx.
// If no correlation ID is present, returns the default logger unchanged.
func Logger(ctx context.Context) *slog.Logger {
	l := slog.Default()
	if id := CorrelationID(ctx); id != "" {
		l = l.With("correlation_id", id)
	}
	return l
}
