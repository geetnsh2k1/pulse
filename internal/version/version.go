// Package version holds build metadata injected at link time.
package version

// Set via -ldflags "-X github.com/geetnsh2k1/pulse/internal/version.Version=…"
// (see Makefile and .goreleaser.yaml).
var (
	Version = "0.1.0-dev"
	Commit  = "none"
)
