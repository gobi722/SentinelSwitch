// Package auth provides API-key authentication for the API Gateway.
//
// Callers authenticate via a gRPC metadata header (default: "x-api-key").
// The key is looked up against the api_clients table (Postgres), cached in
// Redis to avoid a DB round-trip on every request. On any backing-store
// failure the cache fails CLOSED — unlike the idempotency store, which
// intentionally degrades open, an auth failure must never silently admit
// unauthenticated traffic.
package auth

import (
	"crypto/sha256"
	"encoding/hex"
)

// Hash computes the SHA-256 hex digest of an API key.
//
// API keys are high-entropy random tokens, not user passwords — a fast hash
// is the correct choice here (same primitive already used for PAN hashing in
// internal/hashing/pan.go). Bcrypt's deliberate slowness would only work
// against a store that must be checked on every request.
func Hash(apiKey string) string {
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:])
}
