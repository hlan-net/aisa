package apikey

import (
	"bytes"
	"strings"
	"testing"
)

func TestNew(t *testing.T) {
	key, err := New(nil)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	if !strings.HasPrefix(key, "aisa-") {
		t.Errorf("expected key to start with 'aisa-', got %s", key)
	}
	if len(key) != 5+64 {
		t.Errorf("expected key length 69, got %d", len(key))
	}

	// Test with deterministic reader
	r := bytes.NewReader(bytes.Repeat([]byte{0xab}, 32))
	k2, err := New(r)
	if err != nil {
		t.Fatalf("New with reader failed: %v", err)
	}
	expected := "aisa-" + strings.Repeat("ab", 32)
	if k2 != expected {
		t.Errorf("expected %s, got %s", expected, k2)
	}
}

func TestHash(t *testing.T) {
	h := Hash("aisa-test")
	if len(h) != 64 {
		t.Errorf("expected 64 hex chars, got %d", len(h))
	}
	// Verify lower-case hex
	if h != strings.ToLower(h) {
		t.Errorf("expected lowercase hex, got %s", h)
	}
}

func TestParseHashes(t *testing.T) {
	h1 := strings.Repeat("a", 64)
	h2 := strings.Repeat("b", 64)
	input := h1 + ", " + h2 + " invalid-hash " + strings.ToUpper(h1)
	res, bad := ParseHashes(input)
	if len(res) != 2 {
		t.Fatalf("expected 2 valid unique hashes, got %d: %v", len(res), res)
	}
	if bad != 1 {
		t.Errorf("expected 1 bad hash, got %d", bad)
	}
	if res[0] != h1 || res[1] != h2 {
		t.Errorf("expected [%s %s], got %v", h1, h2, res)
	}
}

