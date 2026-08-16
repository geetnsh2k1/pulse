package workers

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/geetnsh2k1/pulse/internal/config"
	"github.com/geetnsh2k1/pulse/internal/logs"
	"github.com/geetnsh2k1/pulse/internal/store"
)

func requireBinary(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s not on PATH; skipping", name)
	}
}

// newTestManager builds a one-function project on disk and starts a manager.
func newTestManager(t *testing.T, runtime, handlerSpec, fileName, code string, timeoutSec int) *Manager {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "fn"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeHandler(t, root, fileName, code)

	cfg := &config.Config{
		Project: "workers-test",
		Region:  "us-east-1",
		Functions: map[string]*config.Function{
			"echo": {Name: "echo", Runtime: runtime, Handler: handlerSpec, CodeDir: "fn",
				Timeout: timeoutSec, Memory: 128, Env: map[string]string{"GREETING": "yo"}},
		},
		Root: root,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	m := NewManager(cfg, st, logs.NewSink(st))
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Shutdown)
	return m
}

func writeHandler(t *testing.T, root, fileName, code string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "fn", fileName), []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}
}

func invokeJSON(t *testing.T, m *Manager, payload string) *Result {
	t.Helper()
	res, err := m.Invoke(context.Background(), "echo", "manual", []byte(payload))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	return res
}

const nodeEcho = `
export const handler = async (event, context) => {
  console.log("node says hi");
  return {
    echoed: event,
    name: context.functionName,
    hasReq: Boolean(context.awsRequestId),
    greeting: process.env.GREETING,
    worker: process.env.PULSE_WORKER_ID,
  };
};
`

func TestNodeEchoAndWarmReuse(t *testing.T) {
	requireBinary(t, "node")
	m := newTestManager(t, "nodejs20.x", "index.handler", "index.mjs", nodeEcho, 10)

	res := invokeJSON(t, m, `{"x":1}`)
	if res.Status != "success" {
		t.Fatalf("status = %s, payload = %s", res.Status, res.Payload)
	}
	var out struct {
		Echoed   map[string]any `json:"echoed"`
		Name     string         `json:"name"`
		HasReq   bool           `json:"hasReq"`
		Greeting string         `json:"greeting"`
	}
	if err := json.Unmarshal(res.Payload, &out); err != nil {
		t.Fatal(err)
	}
	if out.Echoed["x"] != float64(1) || out.Name != "echo" || !out.HasReq || out.Greeting != "yo" {
		t.Errorf("result = %+v", out)
	}

	foundLog := false
	for _, l := range res.Logs {
		if strings.Contains(l.Text, "node says hi") {
			foundLog = true
			if l.RequestID != res.RequestID {
				t.Errorf("log attributed to %q, want %q", l.RequestID, res.RequestID)
			}
		}
	}
	if !foundLog {
		t.Errorf("console output not captured; logs = %+v", res.Logs)
	}

	// Warm second invoke should reuse the worker and be quick.
	start := time.Now()
	res2 := invokeJSON(t, m, `{"x":2}`)
	if res2.Status != "success" {
		t.Fatalf("second invoke failed: %s", res2.Payload)
	}
	if wall := time.Since(start); wall > 2*time.Second {
		t.Errorf("warm invoke took %v — worker reuse is broken", wall)
	}
}

const pyEcho = `
def handler(event, context):
    print("py says hi")
    return {
        "echoed": event,
        "name": context.function_name,
        "has_req": bool(context.aws_request_id),
        "remaining_ok": context.get_remaining_time_in_millis() > 0,
    }
`

func TestPythonEcho(t *testing.T) {
	requireBinary(t, "python3")
	m := newTestManager(t, "python3.12", "handler.handler", "handler.py", pyEcho, 10)

	res := invokeJSON(t, m, `{"k":"v"}`)
	if res.Status != "success" {
		t.Fatalf("status = %s, payload = %s", res.Status, res.Payload)
	}
	var out struct {
		Echoed      map[string]any `json:"echoed"`
		Name        string         `json:"name"`
		HasReq      bool           `json:"has_req"`
		RemainingOK bool           `json:"remaining_ok"`
	}
	if err := json.Unmarshal(res.Payload, &out); err != nil {
		t.Fatal(err)
	}
	if out.Echoed["k"] != "v" || out.Name != "echo" || !out.HasReq || !out.RemainingOK {
		t.Errorf("result = %+v", out)
	}
	joined := ""
	for _, l := range res.Logs {
		joined += l.Text + "\n"
	}
	if !strings.Contains(joined, "py says hi") {
		t.Errorf("print output not captured; logs = %q", joined)
	}
}

func TestHandlerErrorIsAWSShaped(t *testing.T) {
	requireBinary(t, "node")
	m := newTestManager(t, "nodejs20.x", "index.handler", "index.mjs", `
export const handler = async () => { throw new Error("kaboom"); };
`, 10)

	res := invokeJSON(t, m, `{}`)
	if res.Status != "error" {
		t.Fatalf("status = %s", res.Status)
	}
	var doc struct {
		ErrorMessage string   `json:"errorMessage"`
		ErrorType    string   `json:"errorType"`
		StackTrace   []string `json:"stackTrace"`
	}
	if err := json.Unmarshal(res.Payload, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.ErrorMessage != "kaboom" || doc.ErrorType != "Error" || len(doc.StackTrace) == 0 {
		t.Errorf("error doc = %+v", doc)
	}
}

func TestTimeoutKillsAndRecovers(t *testing.T) {
	requireBinary(t, "python3")
	m := newTestManager(t, "python3.12", "handler.handler", "handler.py", `
import time


def handler(event, context):
    if event.get("sleep"):
        time.sleep(5)
    return {"ok": True}
`, 1)

	res := invokeJSON(t, m, `{"sleep": true}`)
	if res.Status != "timeout" {
		t.Fatalf("status = %s, payload = %s", res.Status, res.Payload)
	}
	if res.DurationMs < 900 || res.DurationMs > 4000 {
		t.Errorf("timeout fired at %dms, want ~1000ms", res.DurationMs)
	}

	// The wedged worker was killed; a fresh one must serve the next invoke.
	res2 := invokeJSON(t, m, `{}`)
	if res2.Status != "success" {
		t.Fatalf("post-timeout invoke: status = %s, payload = %s", res2.Status, res2.Payload)
	}
}

func TestHotReloadSwapsCode(t *testing.T) {
	requireBinary(t, "node")
	m := newTestManager(t, "nodejs20.x", "index.handler", "index.mjs", `
export const handler = async () => ({ v: 1 });
`, 10)

	res := invokeJSON(t, m, `{}`)
	if !strings.Contains(string(res.Payload), `"v":1`) {
		t.Fatalf("v1 payload = %s", res.Payload)
	}

	root := m.cfg.Root
	writeHandler(t, root, "index.mjs", `
export const handler = async () => ({ v: 2 });
`)
	m.Reload([]string{"echo"})

	res2 := invokeJSON(t, m, `{}`)
	if !strings.Contains(string(res2.Payload), `"v":2`) {
		t.Fatalf("after reload payload = %s (status %s)", res2.Payload, res2.Status)
	}
}

func TestInitErrorSurfacesAndRecovers(t *testing.T) {
	requireBinary(t, "node")
	m := newTestManager(t, "nodejs20.x", "index.handler", "index.mjs", `
throw new Error("bad module");
`, 10)

	res := invokeJSON(t, m, `{}`)
	if res.Status != "error" || !strings.Contains(string(res.Payload), "bad module") {
		t.Fatalf("status = %s, payload = %s", res.Status, res.Payload)
	}

	writeHandler(t, m.cfg.Root, "index.mjs", `
export const handler = async () => ({ fixed: true });
`)
	m.Reload([]string{"echo"})

	res2 := invokeJSON(t, m, `{}`)
	if res2.Status != "success" || !strings.Contains(string(res2.Payload), "fixed") {
		t.Fatalf("after fix: status = %s, payload = %s", res2.Status, res2.Payload)
	}
}

func TestConcurrentInvokesUseMultipleWorkers(t *testing.T) {
	requireBinary(t, "node")
	m := newTestManager(t, "nodejs20.x", "index.handler", "index.mjs", `
export const handler = async () => {
  await new Promise((r) => setTimeout(r, 300));
  return { worker: process.env.PULSE_WORKER_ID };
};
`, 10)

	const n = 4
	var wg sync.WaitGroup
	results := make([]*Result, n)
	start := time.Now()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := m.Invoke(context.Background(), "echo", "manual", []byte(`{}`))
			if err != nil {
				t.Errorf("invoke %d: %v", i, err)
				return
			}
			results[i] = res
		}(i)
	}
	wg.Wait()
	wall := time.Since(start)

	workersSeen := map[string]bool{}
	for i, res := range results {
		if res == nil || res.Status != "success" {
			t.Fatalf("invoke %d failed: %+v", i, res)
		}
		var out struct {
			Worker string `json:"worker"`
		}
		_ = json.Unmarshal(res.Payload, &out)
		workersSeen[out.Worker] = true
	}
	if len(workersSeen) < 2 {
		t.Errorf("expected multiple workers for concurrent load, saw %v", workersSeen)
	}
	if wall >= n*300*time.Millisecond {
		t.Errorf("wall clock %v suggests serialized execution", wall)
	}
}

// AWS's Python runtime puts a handler on the root logger; without one Python
// uses logging.lastResort, which drops anything below WARNING. The result was
// that `logger.setLevel(logging.INFO)` + `logger.info(...)` — the idiom in
// almost every Lambda ever written — printed nothing locally while working in
// CloudWatch, which makes an imported function look silent.
func TestPythonLoggingModuleReachesTheLogs(t *testing.T) {
	requireBinary(t, "python3")
	m := newTestManager(t, "python3.12", "handler.handler", "handler.py", `
import logging
logger = logging.getLogger()
logger.setLevel(logging.INFO)

def handler(event, context):
    logger.debug("debug-line")
    logger.info("info-line")
    logger.warning("warn-line")
    logger.error("error-line")
    return {"ok": True}
`, 10)

	res := invokeJSON(t, m, `{}`)
	if res.Status != "success" {
		t.Fatalf("status = %s, payload = %s", res.Status, res.Payload)
	}
	joined := ""
	for _, l := range res.Logs {
		joined += l.Text + "\n"
	}
	for _, want := range []string{"info-line", "warn-line", "error-line"} {
		if !strings.Contains(joined, want) {
			t.Errorf("%q missing from the logs:\n%s", want, joined)
		}
	}
	// INFO is the floor AWS uses too: debug stays off unless asked for.
	if strings.Contains(joined, "debug-line") {
		t.Errorf("debug should be below the default level:\n%s", joined)
	}
}
