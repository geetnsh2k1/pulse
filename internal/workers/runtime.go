package workers

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// resolveRuntime finds the interpreter for a declared runtime like
// "nodejs20.x" or "python3.12". A project-local virtualenv (.venv/) wins
// automatically, so Python users never have to activate anything. Policy:
// note/warn but run on version mismatch; hard-fail only when no usable
// binary exists at all.
func resolveRuntime(runtime, projectRoot string) (bin *runtimeBinary, notes []string, err error) {
	family := config.RuntimeFamily(runtime)
	switch family {
	case "node":
		want := strings.TrimSuffix(strings.TrimPrefix(runtime, "nodejs"), ".x")
		path, lookErr := exec.LookPath("node")
		if lookErr != nil {
			return nil, nil, fmt.Errorf("no `node` binary on PATH (needed for %s) — install Node.js %s", runtime, want)
		}
		version := binaryVersion(path, "--version")
		if major := strings.Split(version, ".")[0]; version != "" && major != want {
			notes = append(notes, fmt.Sprintf("using node v%s for %s functions (project declares %s) — behavior may differ",
				version, runtime, runtime))
		}
		return &runtimeBinary{Family: "node", Path: path, Version: version}, notes, nil

	case "python":
		want := strings.TrimPrefix(runtime, "python") // "3.12"

		// A project venv wins: whatever was pip-installed there is visible
		// to the workers with zero activation.
		if projectRoot != "" {
			for _, candidate := range []string{"python" + want, "python3", "python"} {
				p := filepath.Join(projectRoot, ".venv", "bin", candidate)
				if st, statErr := os.Stat(p); statErr == nil && !st.IsDir() {
					version := binaryVersion(p, "--version")
					notes = append(notes, fmt.Sprintf("python functions run inside the project venv (.venv, python %s)", version))
					if version != "" && !strings.HasPrefix(version, want) {
						notes = append(notes, fmt.Sprintf("venv python is %s but the project declares %s — behavior may differ", version, runtime))
					}
					return &runtimeBinary{Family: "python", Path: p, Version: version}, notes, nil
				}
			}
		}

		// Otherwise prefer an exact interpreter (python3.12) from PATH.
		var path string
		for _, candidate := range []string{"python" + want, "python3", "python"} {
			if p, lookErr := exec.LookPath(candidate); lookErr == nil {
				path = p
				break
			}
		}
		if path == "" {
			return nil, nil, fmt.Errorf("no python binary on PATH (needed for %s) — install Python %s", runtime, want)
		}
		version := binaryVersion(path, "--version")
		if version != "" && !strings.HasPrefix(version, want) {
			notes = append(notes, fmt.Sprintf("using python %s for %s functions (project declares %s) — behavior may differ",
				version, runtime, runtime))
		}
		return &runtimeBinary{Family: "python", Path: path, Version: version}, notes, nil
	}
	return nil, nil, fmt.Errorf("unsupported runtime %q", runtime)
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
