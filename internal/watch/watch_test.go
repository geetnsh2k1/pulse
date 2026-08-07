package watch

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/geetnsh2k1/pulse/internal/config"
)

func TestWatcherDetectsCodeAndConfigChanges(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "fn"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(root, "pulse.yaml")
	if err := os.WriteFile(cfgPath, []byte("project: w\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Project: "w",
		Functions: map[string]*config.Function{
			"a": {Name: "a", Runtime: "nodejs20.x", Handler: "index.handler", CodeDir: "fn", Timeout: 3, Memory: 128},
		},
		Root: root,
		Path: cfgPath,
	}

	events := make(chan []string, 8)
	w := New(cfg, func(fns []string, _ string) { events <- fns })
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	time.Sleep(150 * time.Millisecond) // let kqueue/inotify watches settle

	if err := os.WriteFile(filepath.Join(root, "fn", "index.mjs"), []byte("export const x = 1;"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case fns := <-events:
		if len(fns) != 1 || fns[0] != "a" {
			t.Errorf("code change attributed to %v, want [a]", fns)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("code change never reported")
	}

	if err := os.WriteFile(cfgPath, []byte("project: w\nregion: us-east-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case fns := <-events:
		if fns != nil {
			t.Errorf("config change reported as %v, want nil", fns)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pulse.yaml change never reported")
	}

	// Ignored noise must not fire callbacks.
	_ = os.MkdirAll(filepath.Join(root, "fn", "node_modules"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "fn", "node_modules", "x.js"), []byte("x"), 0o644)
	select {
	case fns := <-events:
		t.Errorf("ignored path fired callback: %v", fns)
	case <-time.After(600 * time.Millisecond):
	}
}
