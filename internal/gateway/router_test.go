package gateway

import (
	"testing"

	"pulse/internal/config"
)

func testRouter() *Router {
	cfg := &config.Config{Triggers: []*config.Trigger{
		{Type: "http", Method: "GET", Path: "/orders", Function: "listOrders", PayloadFormat: "2.0"},
		{Type: "http", Method: "POST", Path: "/orders", Function: "createOrder", PayloadFormat: "2.0"},
		{Type: "http", Method: "GET", Path: "/orders/{id}", Function: "getOrder", PayloadFormat: "2.0"},
		{Type: "http", Method: "GET", Path: "/orders/latest", Function: "latestOrder", PayloadFormat: "2.0"},
		{Type: "http", Method: "ANY", Path: "/files/{proxy+}", Function: "files", PayloadFormat: "2.0"},
		{Type: "http", Method: "ANY", Path: "/orders/{id}", Function: "anyOrder", PayloadFormat: "2.0"},
		{Type: "http", Method: "GET", Path: "/", Function: "root", PayloadFormat: "2.0"},
		{Type: "sqs", Queue: "q", Function: "ignored"},
	}}
	return NewRouter(cfg)
}

func TestRouterMatching(t *testing.T) {
	rt := testRouter()
	cases := []struct {
		method, path string
		wantFn       string
		wantParams   map[string]string
		wantMiss     bool
	}{
		{method: "GET", path: "/orders", wantFn: "listOrders"},
		{method: "POST", path: "/orders", wantFn: "createOrder"},
		// Literal beats parameter.
		{method: "GET", path: "/orders/latest", wantFn: "latestOrder"},
		// Exact method beats ANY.
		{method: "GET", path: "/orders/42", wantFn: "getOrder", wantParams: map[string]string{"id": "42"}},
		// ANY catches other methods.
		{method: "DELETE", path: "/orders/42", wantFn: "anyOrder", wantParams: map[string]string{"id": "42"}},
		// Greedy captures the joined remainder.
		{method: "PUT", path: "/files/a/b/c.txt", wantFn: "files", wantParams: map[string]string{"proxy": "a/b/c.txt"}},
		// Greedy needs at least one segment.
		{method: "GET", path: "/files", wantMiss: true},
		{method: "GET", path: "/", wantFn: "root"},
		{method: "GET", path: "/nope", wantMiss: true},
		// Trailing slashes are normalized away, like API Gateway HTTP APIs.
		{method: "GET", path: "/orders/", wantFn: "listOrders"},
	}
	for _, tc := range cases {
		m, ok := rt.Match(tc.method, tc.path)
		if tc.wantMiss {
			if ok {
				t.Errorf("%s %s: matched %s, want miss", tc.method, tc.path, m.Function)
			}
			continue
		}
		if !ok {
			t.Errorf("%s %s: no match, want %s", tc.method, tc.path, tc.wantFn)
			continue
		}
		if m.Function != tc.wantFn {
			t.Errorf("%s %s: matched %s, want %s", tc.method, tc.path, m.Function, tc.wantFn)
		}
		for k, v := range tc.wantParams {
			if m.Params[k] != v {
				t.Errorf("%s %s: param %s = %q, want %q", tc.method, tc.path, k, m.Params[k], v)
			}
		}
	}
}
