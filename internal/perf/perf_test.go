// Package perf enforces PLAN §1's performance bars in CI:
//
//	engine start → ready   < 1s
//	warm invocation total  < 50ms (trivial handler, so ≈ engine+IPC overhead)
//	memory (engine + one warm runtime) < 200MB
//
// Run without the race detector — instrumentation would measure the tool,
// not the product.
package perf

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/geetnsh2k1/pulse/internal/config"
	"github.com/geetnsh2k1/pulse/internal/engine"
	"github.com/geetnsh2k1/pulse/internal/store"
)

const projectYAML = `project: perf
functions:
  echo:
    runtime: python3.12
    handler: handler.handler
    codeDir: fn
`

const echoHandler = `def handler(event, context):
    return event
`

func TestPerformanceBars(t *testing.T) {
	if raceEnabled {
		t.Skip("perf bars are measured without the race detector")
	}
	if testing.Short() {
		t.Skip("-short skips perf bars")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte(projectYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "fn"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fn", "handler.py"), []byte(echoHandler), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(filepath.Join(dir, config.FileName))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Bar 1: start → ready.
	eng := engine.New(cfg, st)
	if err := eng.Start(time.Now()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = eng.Shutdown(ctx)
	}()
	ready := eng.ReadyIn()
	t.Logf("engine ready in %s (bar: <1s)", ready)
	if ready >= time.Second {
		t.Errorf("engine ready in %s, bar is <1s", ready)
	}

	// First invoke pays the worker cold start; it is not the bar.
	cold := timeInvoke(t, eng)
	t.Logf("cold invoke %s (informational)", cold)

	// Bar 2: warm invocation — median of 10.
	durations := make([]time.Duration, 0, 10)
	for i := 0; i < 10; i++ {
		durations = append(durations, timeInvoke(t, eng))
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	median := durations[len(durations)/2]
	t.Logf("warm invoke median %s, fastest %s, slowest %s (bar: <50ms)", median, durations[0], durations[len(durations)-1])
	if median >= 50*time.Millisecond {
		t.Errorf("warm invoke median %s, bar is <50ms", median)
	}

	// Bar 3: memory — this test process (engine runs in-process) plus its
	// descendants (the warm python worker), via ps.
	rssKB, err := rssTreeKB(os.Getpid())
	if err != nil {
		t.Logf("memory bar skipped: %v", err)
		return
	}
	t.Logf("engine + warm runtime RSS %.1f MB (bar: <200MB)", float64(rssKB)/1024)
	if rssKB >= 200*1024 {
		t.Errorf("RSS %.1f MB, bar is <200MB", float64(rssKB)/1024)
	}
}

func timeInvoke(t *testing.T, eng *engine.Engine) time.Duration {
	t.Helper()
	body := []byte(`{"function":"echo","event":{"n":1}}`)
	start := time.Now()
	resp, err := http.Post(eng.ControlAddr()+"/api/invoke", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	defer resp.Body.Close()
	var out struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Status != "success" {
		t.Fatalf("invoke status %q (err %v)", out.Status, err)
	}
	return elapsed
}

// rssTreeKB sums the resident set of pid and all its descendants using ps
// (portable across the darwin/linux CI matrix).
func rssTreeKB(root int) (int64, error) {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,rss=").Output()
	if err != nil {
		return 0, fmt.Errorf("ps: %w", err)
	}
	children := map[int][]int{}
	rss := map[int]int64{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		ppid, _ := strconv.Atoi(f[1])
		kb, _ := strconv.ParseInt(f[2], 10, 64)
		children[ppid] = append(children[ppid], pid)
		rss[pid] = kb
	}
	var sum int64
	var walk func(int)
	walk = func(pid int) {
		sum += rss[pid]
		for _, c := range children[pid] {
			walk(c)
		}
	}
	walk(root)
	return sum, nil
}
