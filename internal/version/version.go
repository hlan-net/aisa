// Package version holds the version of the build.
package version

// Version is set at link time:
//
//	go build -ldflags "-X github.com/hlan-net/aisa/internal/version.Version=v0.2.0" ./cmd/aisa
var Version = "dev"
