package awsfacade

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type stubService struct{}

func (stubService) Do(action string, body []byte) (any, *APIError) {
	switch action {
	case "Ping":
		var req map[string]string
		_ = json.Unmarshal(body, &req)
		return map[string]string{"Pong": req["Value"]}, nil
	case "Explode":
		return nil, &APIError{Type: "com.test#Boom", QueryCode: "Boom", Message: "it went boom"}
	}
	return nil, &APIError{Type: "com.test#Unknown", QueryCode: "Unknown", Message: "unknown action"}
}

func startFacade(t *testing.T) *Facade {
	t.Helper()
	f := New()
	f.Register("TestSvc", "test", stubService{})
	if err := f.Start(0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func post(t *testing.T, url, target, contentType, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	if target != "" {
		req.Header.Set("X-Amz-Target", target)
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func TestRoutingAndResponses(t *testing.T) {
	f := startFacade(t)

	resp, body := post(t, f.URL(), "TestSvc.Ping", "application/x-amz-json-1.0", `{"Value":"hey"}`)
	if resp.StatusCode != 200 || !strings.Contains(body, `"Pong":"hey"`) {
		t.Errorf("ping: %d %s", resp.StatusCode, body)
	}
	if resp.Header.Get("x-amzn-RequestId") == "" {
		t.Error("missing request id header")
	}

	resp, body = post(t, f.URL(), "TestSvc.Explode", "application/x-amz-json-1.0", `{}`)
	if resp.StatusCode != 400 || !strings.Contains(body, `"__type":"com.test#Boom"`) {
		t.Errorf("error shape: %d %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("x-amzn-query-error"); got != "Boom;Sender" {
		t.Errorf("query-error header = %q", got)
	}
}

func TestUnknownServiceAndLegacyProtocol(t *testing.T) {
	f := startFacade(t)

	resp, body := post(t, f.URL(), "DynamoDB_20120810.PutItem", "application/x-amz-json-1.0", `{}`)
	if resp.StatusCode != 400 || !strings.Contains(body, "does not emulate") {
		t.Errorf("unknown service: %d %s", resp.StatusCode, body)
	}

	resp, body = post(t, f.URL(), "", "application/x-www-form-urlencoded", "Action=SendMessage&Version=2012-11-05")
	if resp.StatusCode != 400 || !strings.Contains(body, "Query protocol") {
		t.Errorf("legacy hint: %d %s", resp.StatusCode, body)
	}
}
