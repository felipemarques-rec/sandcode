package orchestrator

import "github.com/google/uuid"

// NewRunID returns a fresh, short run identifier suitable for callers
// that want to pre-allocate the ID (e.g. the HTTP server returning a
// Location header before Run() emits events).
//
// The shape — first 8 chars of a UUIDv4 — matches the internal default
// used by Run() so logs, store rows, and event correlation all line up.
// Collisions are vanishingly unlikely at sandcode's per-node throughput
// (~3.6 trillion before a 50% chance via the birthday bound).
func NewRunID() string {
	return uuid.New().String()[:8]
}
