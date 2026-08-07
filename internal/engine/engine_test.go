package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/geetnsh2k1/pulse/internal/config"
	"github.com/geetnsh2k1/pulse/internal/store"
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

func TestQueueSendEndpointFeedsQueues(t *testing.T) {
	cfg := testConfig(t)
	cfg.Resources.Queues = map[string]*config.Queue{
		"jobs": {Name: "jobs", VisibilityTimeout: 30},
	}
	st, err := store.Open(cfg.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	e := New(cfg, st)
	if err := e.Start(time.Now()); err != nil {
		t.Fatal(err)
	}
	defer e.Shutdown(context.Background())

	resp, err := http.Post(e.ControlAddr()+"/api/queues/send", "application/json",
		strings.NewReader(`{"queue":"jobs","body":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	var sent struct {
		MessageID string `json:"messageId"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&sent)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || sent.MessageID == "" {
		t.Fatalf("send: %d %+v", resp.StatusCode, sent)
	}

	resp, err = http.Get(e.ControlAddr() + "/api/queues")
	if err != nil {
		t.Fatal(err)
	}
	var stats []struct {
		Name    string `json:"name"`
		Visible int    `json:"visible"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&stats)
	resp.Body.Close()
	if len(stats) != 1 || stats[0].Name != "jobs" || stats[0].Visible != 1 {
		t.Errorf("queue stats = %+v", stats)
	}

	// Undeclared queues auto-create on send (write intent).
	resp, _ = http.Post(e.ControlAddr()+"/api/queues/send", "application/json",
		strings.NewReader(`{"queue":"brand-new","body":"x"}`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("auto-create send status = %d, want 200", resp.StatusCode)
	}
}

// TestConfigHotApply proves a pulse.yaml save reshapes the running engine —
// no restart. It also proves invalid saves are rejected harmlessly.
func TestConfigHotApply(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "fn"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "fn", "index.mjs"), []byte("export const handler = async () => ({});"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Grab a genuinely free port: `port: 0` would default to 3000, which a
	// real engine on this machine may already occupy.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	freePort := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	yamlV1 := fmt.Sprintf(`
project: hotapply
api: { port: %d }
functions:
  hello:
    runtime: nodejs20.x
    handler: index.handler
    codeDir: fn
triggers:
  - { type: http, method: GET, path: /one, function: hello }
`, freePort)
	cfgPath := filepath.Join(root, "pulse.yaml")
	if err := os.WriteFile(cfgPath, []byte(yamlV1), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err2 := config.Load(cfgPath)
	if err2 != nil {
		t.Fatal(err2)
	}
	st, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	e := New(cfg, st)
	if err := e.Start(time.Now()); err != nil {
		t.Fatal(err)
	}
	defer e.Shutdown(context.Background())

	routeCount := func() int {
		resp, err := http.Get(e.ControlAddr() + "/api/routes")
		if err != nil {
			return -1
		}
		defer resp.Body.Close()
		var routes []map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&routes)
		return len(routes)
	}
	if n := routeCount(); n != 1 {
		t.Fatalf("initial routes = %d, want 1", n)
	}

	// Save a config with a second route → it must appear without a restart.
	yamlV2 := yamlV1 + "  - { type: http, method: POST, path: /two, function: hello }\n"
	if err := os.WriteFile(cfgPath, []byte(yamlV2), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for routeCount() != 2 {
		if time.Now().After(deadline) {
			t.Fatalf("route never appeared after config save (routes=%d)", routeCount())
		}
		time.Sleep(150 * time.Millisecond)
	}

	// A broken save is rejected: routes stay, engine stays healthy.
	if err := os.WriteFile(cfgPath, []byte("project: hotapply\nfunctions: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond) // debounce + apply attempt window
	if n := routeCount(); n != 2 {
		t.Errorf("invalid config changed live routes: %d", n)
	}
	resp, err := http.Get(e.ControlAddr() + "/health")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("engine unhealthy after rejected config: %v", err)
	}
	resp.Body.Close()
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
