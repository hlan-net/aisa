// Package kube writes the Kubernetes objects that aisa makes from Consul and Vault for the
// applications of its cluster: each consumer's Secret with a key that aisa issues, and a Model
// object per model on offer (docs/concepts/KUBERNETES_OBJECTS.md). The objects are aisa's
// output, never its input: each pass compares them with Consul and Vault and repairs the
// difference.
package kube

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Labels and annotations that aisa sets on what it writes.
const (
	// LabelManagedBy marks the objects of aisa, with the value ManagedBy.
	LabelManagedBy = "app.kubernetes.io/managed-by"
	ManagedBy      = "aisa"
	// LabelInstall names the aisa install that owns an object, by the namespace it runs in;
	// another install leaves the object alone.
	LabelInstall = "aisa.hlan.net/install"
	// LabelConsumer is a consumer's name by DerivedName; AnnotationConsumer the exact name.
	LabelConsumer      = "aisa.hlan.net/consumer"
	AnnotationConsumer = "aisa.hlan.net/consumer-name"
	// LabelModel is a model's name by DerivedName; AnnotationModel the exact name.
	LabelModel      = "aisa.hlan.net/model"
	AnnotationModel = "aisa.hlan.net/model-name"
)

// maxStem leaves room for "-" and 8 hex digits in a label value of at most 63 characters.
const maxStem = 54

// DerivedName turns a name into an object name and label value
// (KUBERNETES_OBJECTS.md, Model): lowercase, every run of characters outside [a-z0-9] as one
// "-", trimmed, cut to 54 characters and trimmed again, fallback when nothing is left, then
// "-" and the first 8 hex digits of the SHA-256 of the exact name. Different names that look
// alike, such as "qwen3:8b" and "qwen3-8b", get different results.
func DerivedName(name, fallback string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	stem := strings.Trim(b.String(), "-")
	if len(stem) > maxStem {
		stem = strings.TrimRight(stem[:maxStem], "-")
	}
	if stem == "" {
		stem = fallback
	}
	sum := sha256.Sum256([]byte(name))
	return stem + "-" + hex.EncodeToString(sum[:])[:8]
}
