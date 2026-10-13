package kube

import (
	"regexp"
	"strings"
	"testing"
)

// labelValue is what Kubernetes accepts as a label value and as an object name (DNS label).
var labelValue = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func TestDerivedName(t *testing.T) {
	for _, tc := range []struct{ name, stem string }{
		{"qwen3:8b", "qwen3-8b"},
		{"qwen3-8b", "qwen3-8b"},
		{"Vendor/Model_Large", "vendor-model-large"},
		{"--x--", "x"},
		{"🔥", "model"},
		{":::", "model"},
		{strings.Repeat("a", 80), strings.Repeat("a", 54)},
		{strings.Repeat("a", 53) + "-b", strings.Repeat("a", 53)},
	} {
		got := DerivedName(tc.name, "model")
		if !strings.HasPrefix(got, tc.stem+"-") || len(got) != len(tc.stem)+9 {
			t.Errorf("DerivedName(%q) = %q, want %s-<8 hex digits>", tc.name, got, tc.stem)
		}
		if len(got) > 63 || !labelValue.MatchString(got) {
			t.Errorf("DerivedName(%q) = %q, not a valid label value", tc.name, got)
		}
	}
	if DerivedName("qwen3:8b", "model") == DerivedName("qwen3-8b", "model") {
		t.Error("names that look alike got one derived name")
	}
	// The SHA-256 of "batch-jobs" begins 405cbf45, as `printf %s batch-jobs | sha256sum` shows.
	if got, want := DerivedName("batch-jobs", "consumer"), "batch-jobs-405cbf45"; got != want {
		t.Errorf("DerivedName(batch-jobs) = %q, want %q", got, want)
	}
}
