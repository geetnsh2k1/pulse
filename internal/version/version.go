// Package version holds build metadata injected at link time.
package version

// Set via -ldflags "-X pulse/internal/version.Version=..." (see Makefile).
var (
	Version = "0.1.0-dev"
	Commit  = "none"
)
