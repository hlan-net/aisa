// Package docs embeds aisa's Markdown documentation, so the admin interface can show it from the
// binary without reaching the repository.
package docs

import "embed"

// FS holds the Markdown files under docs/, at their paths relative to this directory
// (for example "concepts/ARCHITECTURE.md").
//
//go:embed concepts/*.md features/*.md process/*.md
var FS embed.FS
