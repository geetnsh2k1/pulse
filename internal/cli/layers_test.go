package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/geetnsh2k1/pulse/internal/config"
	"github.com/geetnsh2k1/pulse/internal/importer"
)

// layerCfg is a project with one layered function; tests vary what's on disk.
func layerCfg(t *testing.T, arns ...string) *config.Config {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "functions/fn"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &config.Config{
		Root: root,
		Functions: map[string]*config.Function{
			"fn": {Name: "fn", Runtime: "python3.13", Handler: "h.h",
				CodeDir: "functions/fn", Layers: arns},
		},
	}
}

func layerDirOf(cfg *config.Config) string {
	return filepath.Join(cfg.Root, "functions/fn", importer.LayerDir)
}

// The whole point: the failure a fresh clone hits must name layers, name the
// ARNs, and give a command — not surface later as ModuleNotFoundError.
func TestMissingLayersFailBeforeAnythingRuns(t *testing.T) {
	cfg := layerCfg(t, "arn:aws:lambda:ap-south-1:1:layer:deps:9")

	err := requireLayers(cfg)
	if err == nil {
		t.Fatal("a declared layer with nothing on disk must stop the run")
	}
	msg := err.Error()
	for _, want := range []string{"fn", "layer:deps:9", "gitignored", "pulse aws layers"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message should mention %q, got:\n%s", want, msg)
		}
	}
}

func TestPresentLayersAreNotFlagged(t *testing.T) {
	cfg := layerCfg(t, "arn:aws:lambda:ap-south-1:1:layer:deps:9")
	if err := os.MkdirAll(filepath.Join(layerDirOf(cfg), "python", "pymongo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := requireLayers(cfg); err != nil {
		t.Errorf("layers are on disk, nothing should be reported: %v", err)
	}
}

// A directory left behind by an interrupted fetch is not a satisfied layer;
// treating it as one puts the user right back at ModuleNotFoundError.
func TestEmptyLayerDirectoryCountsAsMissing(t *testing.T) {
	cfg := layerCfg(t, "arn:aws:lambda:ap-south-1:1:layer:deps:9")
	if err := os.MkdirAll(layerDirOf(cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := requireLayers(cfg); err == nil {
		t.Error("an empty _layers directory should still be reported as missing")
	}
}

// The overwhelmingly common project has no layers at all; it must not pay for
// this check with a spurious error.
func TestProjectsWithoutLayersAreUnaffected(t *testing.T) {
	cfg := layerCfg(t)
	if err := requireLayers(cfg); err != nil {
		t.Errorf("a project with no layers should never be blocked: %v", err)
	}
	if got := findMissingLayers(cfg); len(got) != 0 {
		t.Errorf("nothing to report, got %+v", got)
	}
}
