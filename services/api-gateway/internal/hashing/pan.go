// Package hashing implements PAN tokenisation for the API Gateway.
//
// Raw PAN never travels beyond this layer.
//
// card_hash = HMAC-SHA256(SHA-256(pan), secret)   → 64-char lowercase hex
// masked_pan = first N digits + mask_char * middle + last M digits
//              e.g. 4111111111111111 → 411111XXXXXX1111  (N=6, M=4)
package hashing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Hasher holds the PAN hashing configuration.
type Hasher struct {
	secret         []byte
	prefixLen      int
	suffixLen      int
	maskChar       rune
}

// New creates a Hasher.
//   - secret      — HMAC secret (from PAN_HASH_SECRET env var)
//   - prefixLen   — digits kept at the front of the masked PAN (config: 6)
//   - suffixLen   — digits kept at the tail (config: 4)
//   - maskChar    — replacement character for middle digits (config: 'X')
func New(secret string, prefixLen, suffixLen int, maskChar string) (*Hasher, error) {
	if len(secret) == 0 {
		return nil, fmt.Errorf("PAN hash secret must not be empty")
	}
	if prefixLen < 1 || suffixLen < 1 {
		return nil, fmt.Errorf("prefix/suffix lengths must be ≥ 1")
	}
	mc, size := utf8.DecodeRuneInString(maskChar)
	if size == 0 || mc == utf8.RuneError {
		return nil, fmt.Errorf("mask_char must be a valid UTF-8 character")
	}
	return &Hasher{
		secret:    []byte(secret),
		prefixLen: prefixLen,
		suffixLen: suffixLen,
		maskChar:  mc,
	}, nil
}

// Hash computes card_hash = HMAC-SHA256(SHA-256(pan), secret).
// The input PAN must contain digits only (spaces/dashes are stripped first).
// Returns a 64-char lowercase hex string.
func (h *Hasher) Hash(pan string) string {
	pan = stripNonDigits(pan)
	inner := sha256.Sum256([]byte(pan))
	mac := hmac.New(sha256.New, h.secret)
	mac.Write(inner[:])
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify checks whether candidateHash matches Hash(pan).
// Uses hmac.Equal to prevent timing attacks.
func (h *Hasher) Verify(pan, candidateHash string) bool {
	expected := h.Hash(pan)
	expectedBytes, err := hex.DecodeString(expected)
	if err != nil {
		return false
	}
	candidate, err := hex.DecodeString(candidateHash)
	if err != nil {
		return false
	}
	return hmac.Equal(expectedBytes, candidate)
}

// MaskedPAN builds a display-safe PAN string.
// If pan is shorter than prefixLen+suffixLen digits, the entire string is masked.
func (h *Hasher) MaskedPAN(pan string) string {
	pan = stripNonDigits(pan)
	total := len(pan)
	min := h.prefixLen + h.suffixLen

	if total <= min {
		return strings.Repeat(string(h.maskChar), total)
	}

	prefix := pan[:h.prefixLen]
	suffix := pan[total-h.suffixLen:]
	middle := strings.Repeat(string(h.maskChar), total-h.prefixLen-h.suffixLen)
	return prefix + middle + suffix
}

// MaskedPANFromLast4 builds a masked PAN when only the last 4 digits are
// available (no full PAN). Produces XXXXXXXXXXXX{last4}.
func (h *Hasher) MaskedPANFromLast4(last4 string) string {
	last4 = stripNonDigits(last4)
	if len(last4) > h.suffixLen {
		last4 = last4[len(last4)-h.suffixLen:]
	}
	// Standard card length 16; middle = 16 - prefixLen - suffixLen
	middle := 16 - h.prefixLen - h.suffixLen
	if middle < 0 {
		middle = 0
	}
	return strings.Repeat(string(h.maskChar), h.prefixLen+middle) + last4
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func stripNonDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
