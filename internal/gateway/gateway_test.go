package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/geetnsh2k1/pulse/internal/config"
	"github.com/geetnsh2k1/pulse/internal/logs"
	"github.com/geetnsh2k1/pulse/internal/store"
	"github.com/geetnsh2k1/pulse/internal/workers"
)

// ---- response-mapping tests with a canned invoker ----

type fakeInvoker struct {
	results map[string]*workers.Result // keyed by function name
}

func (f *fakeInvoker) InvokeAs(_ context.Context, id, fn, _ string, _ []byte) (*workers.Result, error) {
	r := *f.results[fn]
	r.RequestID = id
	return &r, nil
}

func startFakeServer(t *testing.T, cfg *config.Config, inv Invoker) *Server {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	s := New(cfg, inv, logs.NewSink(st))
	if err := s.Start(0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	return s
}

func TestResponseMapping(t *testing.T) {
	cfg := &config.Config{Triggers: []*config.Trigger{
		{Type: "http", Method: "GET", Path: "/structured", Function: "structured", PayloadFormat: "2.0"},
		{Type: "http", Method: "GET", Path: "/bare", Function: "bare", PayloadFormat: "2.0"},
		{Type: "http", Method: "GET", Path: "/binary", Function: "binary", PayloadFormat: "2.0"},
		{Type: "http", Method: "GET", Path: "/broken", Function: "broken", PayloadFormat: "2.0"},
		{Type: "http", Method: "GET", Path: "/v1ok", Function: "v1ok", PayloadFormat: "1.0"},
		{Type: "http", Method: "GET", Path: "/v1bare", Function: "v1bare", PayloadFormat: "1.0"},
	}}
	inv := &fakeInvoker{results: map[string]*workers.Result{
		"structured": {Status: "success", Payload: []byte(`{"statusCode":201,"headers":{"X-Fn":"yes"},"cookies":["s=1"],"body":"{\"ok\":true}"}`)},
		"bare":       {Status: "success", Payload: []byte(`{"hello":1}`)},
		"binary":     {Status: "success", Payload: []byte(`{"statusCode":200,"isBase64Encoded":true,"body":"/wCI"}`)},
		"broken":     {Status: "error", Payload: []byte(`{"errorMessage":"x"}`)},
		"v1ok":       {Status: "success", Payload: []byte(`{"statusCode":200,"body":"pong"}`)},
		"v1bare":     {Status: "success", Payload: []byte(`{"no":"envelope"}`)},
	}}
	s := startFakeServer(t, cfg, inv)

	get := func(path string) (*http.Response, []byte) {
		t.Helper()
		resp, err := http.Get(s.URL() + path)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, b
	}

	resp, body := get("/structured")
	if resp.StatusCode != 201 || resp.Header.Get("X-Fn") != "yes" || string(body) != `{"ok":true}` {
		t.Errorf("structured: %d %q %q", resp.StatusCode, resp.Header.Get("X-Fn"), body)
	}
	if c := resp.Header.Get("Set-Cookie"); c != "s=1" {
		t.Errorf("cookie header = %q", c)
	}

	resp, body = get("/bare")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/json" || string(body) != `{"hello":1}` {
		t.Errorf("bare: %d %q", resp.StatusCode, body)
	}

	resp, body = get("/binary")
	if resp.StatusCode != 200 || string(body) != "\xff\x00\x88" {
		t.Errorf("binary: %d %v", resp.StatusCode, []byte(string(body)))
	}

	resp, body = get("/broken")
	if resp.StatusCode != 500 || !strings.Contains(string(body), "Internal Server Error") {
		t.Errorf("broken: %d %q", resp.StatusCode, body)
	}

	resp, _ = get("/v1ok")
	if resp.StatusCode != 200 {
		t.Errorf("v1ok: %d", resp.StatusCode)
	}

	resp, _ = get("/v1bare")
	if resp.StatusCode != 502 {
		t.Errorf("v1 without envelope should 502, got %d", resp.StatusCode)
	}

	resp, _ = get("/nope")
	if resp.StatusCode != 404 {
		t.Errorf("no route should 404, got %d", resp.StatusCode)
	}
}

// ---- end-to-end with a real Node worker ----

const gatewayHandler = `
export const handler = async (event) => {
  if (event.version === "2.0") {
    const method = event.requestContext.http.method;
    const id = event.pathParameters?.id;
    if (id === "boom") throw new Error("exploded");
    if (event.rawPath.startsWith("/files/")) return { proxy: event.pathParameters.proxy };
    return {
      statusCode: method === "POST" ? 201 : 200,
      headers: { "x-fn": "api" },
      body: JSON.stringify({
        method,
        id: id ?? null,
        body: event.body ?? null,
        q: event.queryStringParameters ?? null,
      }),
    };
  }
  return { statusCode: 200, body: JSON.stringify({ v1: event.httpMethod, res: event.resource }) };
};
`

func TestGatewayEndToEndWithNode(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH; skipping")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "fn"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "fn", "index.mjs"), []byte(gatewayHandler), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Project: "gw-test",
		Region:  "us-east-1",
		Functions: map[string]*config.Function{
			"api": {Name: "api", Runtime: "nodejs20.x", Handler: "index.handler", CodeDir: "fn", Timeout: 10, Memory: 128},
		},
		Triggers: []*config.Trigger{
			{Type: "http", Method: "POST", Path: "/orders", Function: "api", PayloadFormat: "2.0"},
			{Type: "http", Method: "GET", Path: "/orders/{id}", Function: "api", PayloadFormat: "2.0"},
			{Type: "http", Method: "ANY", Path: "/files/{proxy+}", Function: "api", PayloadFormat: "2.0"},
			{Type: "http", Method: "GET", Path: "/v1ping", Function: "api", PayloadFormat: "1.0"},
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
	sink := logs.NewSink(st)
	mgr := workers.NewManager(cfg, st, sink)
	if err := mgr.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Shutdown)

	s := New(cfg, mgr, sink)
	if err := s.Start(0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })

	// POST with a JSON body.
	resp, err := http.Post(s.URL()+"/orders", "application/json", strings.NewReader(`{"sku":"A"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 201 || resp.Header.Get("x-fn") != "api" {
		t.Fatalf("POST /orders: %d %q", resp.StatusCode, b)
	}
	var out struct {
		Method string            `json:"method"`
		ID     *string           `json:"id"`
		Body   string            `json:"body"`
		Q      map[string]string `json:"q"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Method != "POST" || out.Body != `{"sku":"A"}` || out.ID != nil {
		t.Errorf("POST payload seen by fn = %+v", out)
	}

	// GET with path + multi-value query params.
	resp, _ = http.Get(s.URL() + "/orders/42?x=1&x=2")
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	_ = json.Unmarshal(b, &out)
	if resp.StatusCode != 200 || out.ID == nil || *out.ID != "42" || out.Q["x"] != "1,2" {
		t.Errorf("GET /orders/42: %d %+v", resp.StatusCode, out)
	}

	// Greedy route returning a bare value → v2 auto-wrap.
	resp, _ = http.Get(s.URL() + "/files/a/b/c.txt")
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"proxy":"a/b/c.txt"`) {
		t.Errorf("greedy auto-wrap: %d %q", resp.StatusCode, b)
	}

	// Handler exception → 500 with the API Gateway message shape.
	resp, _ = http.Get(s.URL() + "/orders/boom")
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 500 || !strings.Contains(string(b), "Internal Server Error") {
		t.Errorf("error mapping: %d %q", resp.StatusCode, b)
	}

	// v1 payload format.
	resp, _ = http.Get(s.URL() + "/v1ping")
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"v1":"GET"`) || !strings.Contains(string(b), `"res":"/v1ping"`) {
		t.Errorf("v1 route: %d %q", resp.StatusCode, b)
	}

	// Requests were recorded as replayable http events.
	invs, err := st.RecentInvocations("api", 50)
	if err != nil || len(invs) < 5 {
		t.Fatalf("invocations recorded = %d (%v)", len(invs), err)
	}
	for _, inv := range invs {
		if inv.Source != "http" {
			t.Errorf("invocation source = %q, want http", inv.Source)
		}
	}
}
