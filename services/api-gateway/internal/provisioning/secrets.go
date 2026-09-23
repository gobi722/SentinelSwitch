// Package provisioning onboards a new external API client: an API key
// (Postgres), a Kafka SCRAM-SHA-256 credential, a dedicated results.<id>
// topic, and ACLs scoping that credential to only that topic and a
// <id>.-prefixed consumer-group namespace.
//
// This automates what scripts/provision-client.sh previously did by hand —
// see that script's header comment and docs/MULTI_TENANT_RESULT_DELIVERY.md
// for the original design this mirrors.
package provisioning

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"

	"golang.org/x/crypto/pbkdf2"
)

// ClientIDPattern must match result-notifier's router validation
// (services/result-notifier/internal/router/router.go) — client_id is
// interpolated directly into a Kafka topic name (results.<client_id>) and a
// consumer-group prefix, so it must be a legal component of both.
var ClientIDPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,200}$`)

// scramIterations is the SCRAM-SHA-256 iteration count used for both the
// salted-password derivation here and the value handed to Kafka's
// AlterUserScramCredentials — the two must agree, since the broker stores
// (salt, iterations, SaltedPassword) as an opaque triple and replays the
// same PBKDF2 computation during SASL handshake to verify a client's
// plaintext password.
const scramIterations = 4096

// randomHex returns n random bytes hex-encoded (2n characters). Mirrors
// scripts/provision-client.sh's `openssl rand -hex N`.
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("provisioning: generate random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// newAPIKey generates a 32-byte (64 hex char) high-entropy API key.
func newAPIKey() (string, error) {
	return randomHex(32)
}

// newSCRAMPassword generates a 24-byte (48 hex char) SCRAM password.
func newSCRAMPassword() (string, error) {
	return randomHex(24)
}

// newSalt generates a random SCRAM salt.
func newSalt() ([]byte, error) {
	salt := make([]byte, 24)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("provisioning: generate salt: %w", err)
	}
	return salt, nil
}

// saltedPassword derives SaltedPassword = Hi(password, salt, iterations) per
// RFC 5802 — this, not the plaintext password, is what Kafka's
// AlterUserScramCredentials API stores. The plaintext password is returned
// to the caller once (in the RPC response) and never persisted anywhere by
// this service.
func saltedPassword(password string, salt []byte) []byte {
	return pbkdf2.Key([]byte(password), salt, scramIterations, sha256.Size, sha256.New)
}
