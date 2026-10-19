// Package ui holds the static files of the admin web dashboard.
package ui

import "embed"

// FS holds the embedded assets of the admin web UI.
//
//go:embed index.html assets/*
var FS embed.FS
