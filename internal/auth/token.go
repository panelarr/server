// SPDX-License-Identifier: GPL-3.0-or-later

package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

// APIKeyPrefix starts every personal API key, so keys are easy to spot
// (also for secret scanners).
const APIKeyPrefix = "pnl_"

// apiKeyShownLen is how much of a key is kept in clear to tell keys apart.
const apiKeyShownLen = len(APIKeyPrefix) + 8

// newToken returns a random 256-bit token, URL-safe, with prefix.
func newToken(prefix string) string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // never fails on supported platforms
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

// hashToken hashes a high-entropy token for storage and lookup. A plain
// SHA-256 is enough for random 256-bit tokens; slow hashes are for
// passwords.
func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// NewInviteToken returns a fresh invite token and its stored hash.
func NewInviteToken() (token string, hash []byte) {
	t := newToken("")
	return t, hashToken(t)
}

// HashInviteToken hashes an invite token to look it up.
func HashInviteToken(token string) []byte { return hashToken(token) }
