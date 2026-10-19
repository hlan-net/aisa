// Package apikey provides shared API key generation and hashing for consumers.
// Keys issued by aisa have the format "aisa-" followed by 64 hex characters (32 random bytes).
// Hashes stored in Vault are lowercase SHA-256 hex digests.
package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode"
)

// Prefix is the prefix of keys issued by aisa, distinguishing them from provider keys.
const Prefix = "aisa-"

// MinCustomKeyLength is the minimum allowed length for custom API keys.
const MinCustomKeyLength = 16

var (
	// ErrKeyTooShort is returned when a custom key is shorter than MinCustomKeyLength.
	ErrKeyTooShort = errors.New("key must be at least 16 characters")
)

// New returns a key of 32 random bytes, hex-encoded, with the "aisa-" prefix.
func New(r io.Reader) (string, error) {
	if r == nil {
		r = rand.Reader
	}
	b := make([]byte, 32)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", fmt.Errorf("generate a key: %w", err)
	}
	return Prefix + hex.EncodeToString(b), nil
}

// Hash returns the lowercase hex-encoded SHA-256 digest of key.
func Hash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// ParseHashes splits a key_sha256 value into lowercase hex-encoded SHA-256 digests.
// It accepts one hash, or several separated by commas or whitespace (including line ends).
// It returns the valid unique hashes and the count of values that were not valid 32-byte hex digests.
func ParseHashes(v string) (hashes []string, bad int) {
	for f := range strings.FieldsFuncSeq(v, func(r rune) bool { return r == ',' || unicode.IsSpace(r) }) {
		f = strings.ToLower(f)
		b, err := hex.DecodeString(f)
		if err != nil || len(b) != sha256.Size {
			bad++
			continue
		}
		if !slices.Contains(hashes, f) {
			hashes = append(hashes, f)
		}
	}
	return hashes, bad
}

// SplitHashes reads key_sha256 as comma- or whitespace-separated hex-encoded SHA-256 digests.
// Values that are not valid 32-byte hex digests are dropped.
func SplitHashes(v string) []string {
	hashes, _ := ParseHashes(v)
	return hashes
}
