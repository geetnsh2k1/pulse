package workers

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"pulse/internal/config"
)

// runtimeBinary is a resolved interpreter on this machine.
type runtimeBinary struct {
	Family  string // node | python
	Path    string
	Version string // e.g. "23.7.0" or "3.13.2"
}

var versionRe = regexp.MustCompile(`(\d+)\.(\d+)(?:\.(\d+))?`)

// resolveRuntime finds the local interpreter for a declared runtime like
// "nodejs20.x" or "python3.12". Policy: warn on version mismatch but run
// anyway; hard-fail only when no usable binary exists at all.
func resolveRuntime(runtime string) (bin *runtimeBinary, warning string, err error) {
	family := config.RuntimeFamily(runtime)
	switch family {
	case "node":
		want := strings.TrimSuffix(strings.TrimPrefix(runtime, "nodejs"), ".x")
		path, lookErr := exec.LookPath("node")
		if lookErr != nil {
			return nil, "", fmt.Errorf("no `node` binary on PATH (needed for %s) — install Node.js %s", runtime, want)
		}
		version := binaryVersion(path, "--version")
		if major := strings.Split(version, ".")[0]; version != "" && major != want {
			warning = fmt.Sprintf("using node v%s for %s functions (project declares %s) — behavior may differ",
				version, runtime, runtime)
		}
		return &runtimeBinary{Family: "node", Path: path, Version: version}, warning, nil

	case "python":
		want := strings.TrimPrefix(runtime, "python") // "3.12"
		// Prefer an exact interpreter (python3.12) when installed.
		var path string
		for _, candidate := range []string{"python" + want, "python3", "python"} {
			if p, lookErr := exec.LookPath(candidate); lookErr == nil {
				path = p
				break
			}
		}
		if path == "" {
			return nil, "", fmt.Errorf("no python binary on PATH (needed for %s) — install Python %s", runtime, want)
		}
		version := binaryVersion(path, "--version")
		if version != "" && !strings.HasPrefix(version, want) {
			warning = fmt.Sprintf("using python %s for %s functions (project declares %s) — behavior may differ",
				version, runtime, runtime)
		}
		return &runtimeBinary{Family: "python", Path: path, Version: version}, warning, nil
	}
	return nil, "", fmt.Errorf("unsupported runtime %q", runtime)
}

// binaryVersion runs `<path> <flag>` and extracts a dotted version, or "".
func binaryVersion(path, flag string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, flag).CombinedOutput()
	if err != nil {
		return ""
	}
	return versionRe.FindString(string(out))
}
