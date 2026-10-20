// Package docs embeds aisa's Markdown documentation, so the admin interface can show it from
// the binary without fetching anything at runtime.
package docs

import "embed"

// FS holds the Markdown files of docs/, by their paths below it (concepts/ARCHITECTURE.md).
//
//go:embed concepts/*.md features/*.md process/*.md
var FS embed.FS
