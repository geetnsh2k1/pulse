package workers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// AWS mounts layers at /opt and puts /opt/python on PYTHONPATH and
// /opt/nodejs/node_modules on NODE_PATH. pulse unpacks them under the function
// instead, so it has to reproduce those search paths itself — otherwise an
// imported function fails on `import pymongo` with the layer sitting right
// there on disk.
func TestLayerPathsMirrorTheLambdaMounts(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{
		"python",
		"python/lib/python3.13/site-packages",
		"nodejs/node_modules",
		"nodejs/node18/node_modules",
		"bin", // present in real layers, not a module path
	} {
		if err := os.MkdirAll(filepath.Join(root, layerDir, filepath.FromSlash(dir)), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	got := strings.Join(layerPaths(root), "\n")
	for _, want := range []string{
		"PYTHONPATH=", filepath.Join(layerDir, "python"),
		"NODE_PATH=", filepath.Join(layerDir, "nodejs", "node_modules"),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, filepath.Join(layerDir, "bin")) {
		t.Errorf("only module directories belong on the search paths:\n%s", got)
	}
}

// The common case is a function with no layers at all: it must not get empty
// or bogus search-path entries, which shadow the real ones on some runtimes.
func TestLayerPathsAreAbsentWithoutLayers(t *testing.T) {
	if got := layerPaths(t.TempDir()); len(got) != 0 {
		t.Errorf("want no entries, got %v", got)
	}
}
