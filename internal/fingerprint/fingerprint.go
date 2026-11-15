// Package fingerprint turns a secret into a safe ID (a SHA-256 hash)
// and a redacted form that is safe to print.
package fingerprint

import (
	"crypto/sha256"
	"encoding/hex"
)

// ShortLen is how many hex characters we show on screen.
const ShortLen = 12

// minRedactLen: secrets shorter than this are fully hidden,
// because "first 4 + last 4" would show most of the secret.
const minRedactLen = 12

func Full(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// Short returns the first ShortLen characters of a full fingerprint.
// Use this when printing.
func Short(full string) string {
	if len(full) <= ShortLen {
		return full
	}
	return full[:ShortLen]
}

// Redact keeps the first 4 and last 4 characters: "abcd...wxyz".
// Short secrets become "****" so we never leak most of a secret.
func Redact(secret string) string {
	if len(secret) < minRedactLen {
		return "****"
	}
	return secret[:4] + "..." + secret[len(secret)-4:]
}
