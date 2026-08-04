package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBuildV2Event(t *testing.T) {
	r := httptest.NewRequest("POST", "http://localhost:3000/orders/42?x=1&x=2&y=z", strings.NewReader(""))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Cookie", "session=abc; theme=dark")
	r.Header.Add("X-Multi", "a")
	r.Header.Add("X-Multi", "b")

	m := &Match{Function: "api", RouteKey: "POST /orders/{id}", Resource: "/orders/{id}",
		Format: "2.0", Params: map[string]string{"id": "42"}}
	raw, err := buildV2(r, []byte(`{"sku":"A"}`), m, "req-1", time.UnixMilli(1785331221929))
	if err != nil {
		t.Fatal(err)
	}

	var ev struct {
		Version        string            `json:"version"`
		RouteKey       string            `json:"routeKey"`
		RawPath        string            `json:"rawPath"`
		RawQueryString string            `json:"rawQueryString"`
		Cookies        []string          `json:"cookies"`
		Headers        map[string]string `json:"headers"`
		Query          map[string]string `json:"queryStringParameters"`
		PathParameters map[string]string `json:"pathParameters"`
		Body           string            `json:"body"`
		IsBase64       bool              `json:"isBase64Encoded"`
		RequestContext struct {
			RequestID string `json:"requestId"`
			HTTP      struct {
				Method string `json:"method"`
			} `json:"http"`
		} `json:"requestContext"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatal(err)
	}

	if ev.Version != "2.0" || ev.RouteKey != "POST /orders/{id}" || ev.RawPath != "/orders/42" {
		t.Errorf("envelope = %+v", ev)
	}
	if ev.Headers["content-type"] != "application/json" {
		t.Errorf("headers not lowercased: %v", ev.Headers)
	}
	if _, hasCookieHeader := ev.Headers["cookie"]; hasCookieHeader {
		t.Error("cookie header should move to the cookies array in v2")
	}
	if len(ev.Cookies) != 2 || ev.Cookies[0] != "session=abc" {
		t.Errorf("cookies = %v", ev.Cookies)
	}
	if ev.Headers["x-multi"] != "a,b" {
		t.Errorf("multi header join = %q", ev.Headers["x-multi"])
	}
	if ev.Query["x"] != "1,2" || ev.Query["y"] != "z" {
		t.Errorf("query = %v", ev.Query)
	}
	if ev.PathParameters["id"] != "42" || ev.Body != `{"sku":"A"}` || ev.IsBase64 {
		t.Errorf("params/body = %v %q %v", ev.PathParameters, ev.Body, ev.IsBase64)
	}
	if ev.RequestContext.RequestID != "req-1" || ev.RequestContext.HTTP.Method != "POST" {
		t.Errorf("requestContext = %+v", ev.RequestContext)
	}
}

func TestBuildV2BinaryBody(t *testing.T) {
	r := httptest.NewRequest("POST", "http://localhost/upload", nil)
	m := &Match{RouteKey: "POST /upload", Resource: "/upload", Format: "2.0"}
	raw, err := buildV2(r, []byte{0xff, 0x00, 0x88}, m, "req", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var ev struct {
		Body     string `json:"body"`
		IsBase64 bool   `json:"isBase64Encoded"`
	}
	_ = json.Unmarshal(raw, &ev)
	if !ev.IsBase64 || ev.Body != "/wCI" {
		t.Errorf("binary body = %q (b64=%v)", ev.Body, ev.IsBase64)
	}
}

func TestBuildV1Event(t *testing.T) {
	r := httptest.NewRequest("GET", "http://localhost/things/7?a=1&a=2", nil)
	r.Header.Set("X-Test", "v")

	m := &Match{Function: "fn", RouteKey: "GET /things/{id}", Resource: "/things/{id}",
		Format: "1.0", Params: map[string]string{"id": "7"}}
	raw, err := buildV1(r, nil, m, "req-9", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	var ev struct {
		Resource   string              `json:"resource"`
		Path       string              `json:"path"`
		HTTPMethod string              `json:"httpMethod"`
		Headers    map[string]string   `json:"headers"`
		MVQ        map[string][]string `json:"multiValueQueryStringParameters"`
		Params     map[string]string   `json:"pathParameters"`
		Body       *string             `json:"body"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Resource != "/things/{id}" || ev.Path != "/things/7" || ev.HTTPMethod != "GET" {
		t.Errorf("v1 envelope = %+v", ev)
	}
	if ev.Headers["X-Test"] != "v" {
		t.Errorf("v1 headers = %v", ev.Headers)
	}
	if len(ev.MVQ["a"]) != 2 {
		t.Errorf("v1 multi query = %v", ev.MVQ)
	}
	if ev.Params["id"] != "7" || ev.Body != nil {
		t.Errorf("v1 params/body = %v %v", ev.Params, ev.Body)
	}
}
