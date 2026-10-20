// Package aisa carries the repository's Markdown documentation into the binary, so the admin
// interface can show the documents of the version that runs.
package aisa

import "embed"

// Docs holds README.md, ROADMAP.md, CHANGELOG.md and the documents under docs/, at their paths
// in the repository.
//
//go:embed README.md ROADMAP.md CHANGELOG.md docs
var Docs embed.FS
