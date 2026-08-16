package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/geetnsh2k1/pulse/internal/config"
	"github.com/geetnsh2k1/pulse/internal/importer"
	"github.com/geetnsh2k1/pulse/internal/ui"
)

// missingLayers is a function whose pulse.yaml declares layers that aren't on
// disk. The bytes live in <codeDir>/_layers, which is gitignored — so this is
// the normal state of a fresh clone, not a rare corruption.
type missingLayers struct {
	Function string
	CodeDir  string
	ARNs     []string
}

// layerDirFor is the one place that knows where a function's layers live.
func layerDirFor(cfg *config.Config, fn *config.Function) string {
	return filepath.Join(cfg.Root, fn.CodeDir, importer.LayerDir)
}

// findMissingLayers reports functions that declare layers with nothing
// unpacked for them. An empty directory counts as missing: a half-finished
// fetch that left the directory behind should not read as satisfied.
func findMissingLayers(cfg *config.Config) []missingLayers {
	var out []missingLayers
	for _, name := range sortedFunctionNames(cfg) {
		fn := cfg.Functions[name]
		if len(fn.Layers) == 0 {
			continue
		}
		if entries, err := os.ReadDir(layerDirFor(cfg, fn)); err == nil && len(entries) > 0 {
			continue
		}
		out = append(out, missingLayers{Function: name, CodeDir: fn.CodeDir, ARNs: fn.Layers})
	}
	return out
}

// requireLayers stops a run that would fail on an import anyway. Without this
// the user gets a bare ModuleNotFoundError naming a package they never
// installed, with the actual cause — layers that were never fetched into this
// checkout — nowhere on screen.
func requireLayers(cfg *config.Config) error {
	missing := findMissingLayers(cfg)
	if len(missing) == 0 {
		return nil
	}

	var b strings.Builder
	for _, m := range missing {
		fmt.Fprintf(&b, "%s needs %s:\n", ui.Bold(m.Function), missingClause(len(m.ARNs)))
		for _, arn := range m.ARNs {
			fmt.Fprintf(&b, "  %s\n", ui.Dim(arn))
		}
	}
	fmt.Fprintf(&b, "\n%s\n", ui.Dim(
		"Layer contents are gitignored (they're vendored bytes, not your source),\n"+
			"so a fresh clone never has them. The ARNs above came across in pulse.yaml\n"+
			"precisely so they can be fetched again."))

	return &layerError{
		msg: strings.TrimRight(b.String(), "\n"),
		fix: "`pulse aws layers` — downloads them read-only from AWS (needs lambda:GetLayerVersion)",
	}
}

// layerError follows the shape the rest of pulse uses for a problem with a
// known remedy: what happened, then the command that fixes it.
type layerError struct {
	msg string
	fix string
}

func (e *layerError) Error() string { return e.msg + "\n\n  fix: " + e.fix }

func layerWord(n int) string {
	if n == 1 {
		return "1 layer"
	}
	return fmt.Sprintf("%d layers", n)
}

// missingClause keeps the noun and its verb agreeing; "a layer that aren't"
// is the kind of thing that makes an error message look machine-generated.
func missingClause(n int) string {
	if n == 1 {
		return "a layer that isn't in this checkout"
	}
	return fmt.Sprintf("%d layers that aren't in this checkout", n)
}

// sortedFunctionNames keeps output stable across runs; Go map order is not.
func sortedFunctionNames(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.Functions))
	for n := range cfg.Functions {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
