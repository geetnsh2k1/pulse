package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"pulse/internal/config"
	"pulse/internal/store"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "fn"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Project: "test",
		Region:  "us-east-1",
		Functions: map[string]*config.Function{
			"hello": {Name: "hello", Runtime: "nodejs20.x", Handler: "index.handler", CodeDir: "fn", Timeout: 3, Memory: 128},
		},
		Root: root,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestEngineLifecycle(t *testing.T) {
	cfg := testConfig(t)
	st, err := store.Open(cfg.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	e := New(cfg, st)
	if err := e.Start(time.Now()); err != nil {
		t.Fatal(err)
	}

	// Health reports the project.
	resp, err := http.Get(e.ControlAddr() + "/health")
	if err != nil {
		t.Fatal(err)
	}
	var health struct {
		Status    string `json:"status"`
		Project   string `json:"project"`
		Functions int    `json:"functions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if health.Status != "ok" || health.Project != "test" || health.Functions != 1 {
		t.Errorf("health = %+v", health)
	}

	// Runfile is discoverable and probes as live.
	info, ok := Current(cfg.Root)
	if !ok {
		t.Fatal("Current: engine not found while running")
	}
	if info.PID != os.Getpid() || info.Addr != e.ControlAddr() {
		t.Errorf("runfile = %+v", info)
	}

	// Config views are served.
	resp, err = http.Get(e.ControlAddr() + "/api/functions")
	if err != nil {
		t.Fatal(err)
	}
	var fns []config.Function
	if err := json.NewDecoder(resp.Body).Decode(&fns); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(fns) != 1 || fns[0].Name != "hello" || fns[0].Runtime != "nodejs20.x" {
		t.Errorf("functions view = %+v", fns)
	}

	// Shutdown endpoint fires the signal exactly once.
	for i := 0; i < 2; i++ {
		resp, err = http.Post(e.ControlAddr()+"/api/shutdown", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Errorf("shutdown status = %d", resp.StatusCode)
		}
	}
	select {
	case <-e.ShutdownRequested():
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown signal never fired")
	}

	if err := e.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := Current(cfg.Root); ok {
		t.Error("engine still reported as running after Shutdown")
	}
	if _, err := os.Stat(filepath.Join(cfg.Root, store.Dir, "runtime", "engine.json")); !os.IsNotExist(err) {
		t.Errorf("runfile not removed: %v", err)
	}
}

func TestStaleRunfileIsCleaned(t *testing.T) {
	root := t.TempDir()
	err := WriteRunInfo(root, RunInfo{
		PID:     999999,
		Addr:    "http://127.0.0.1:1", // nothing listens here
		Project: "ghost",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := Current(root); ok {
		t.Fatal("stale engine reported as live")
	}
	if _, err := os.Stat(filepath.Join(root, store.Dir, "runtime", "engine.json")); !os.IsNotExist(err) {
		t.Error("stale runfile was not cleaned up")
	}
}
