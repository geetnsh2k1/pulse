package gateway

import (
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

const requestTimeLayout = "02/Jan/2006:15:04:05 -0700"

// buildEvent constructs the proxy event for the matched route's format.
func buildEvent(r *http.Request, body []byte, m *Match, requestID string, now time.Time) ([]byte, error) {
	if m.Format == "1.0" {
		return buildV1(r, body, m, requestID, now)
	}
	return buildV2(r, body, m, requestID, now)
}

// buildV2 emits the API Gateway HTTP API "payload format 2.0" event.
func buildV2(r *http.Request, body []byte, m *Match, requestID string, now time.Time) ([]byte, error) {
	headers := map[string]string{}
	for k, vs := range r.Header {
		lk := strings.ToLower(k)
		if lk == "cookie" {
			continue // v2 moves cookies out of headers
		}
		headers[lk] = strings.Join(vs, ",")
	}
	var cookies []string
	for _, c := range r.Cookies() {
		cookies = append(cookies, c.Name+"="+c.Value)
	}

	var qsp map[string]string
	if q := r.URL.Query(); len(q) > 0 {
		qsp = map[string]string{}
		for k, vs := range q {
			qsp[k] = strings.Join(vs, ",")
		}
	}

	bodyStr, isB64 := encodeBody(body)
	event := map[string]any{
		"version":         "2.0",
		"routeKey":        m.RouteKey,
		"rawPath":         r.URL.Path,
		"rawQueryString":  r.URL.RawQuery,
		"headers":         headers,
		"isBase64Encoded": isB64,
		"requestContext": map[string]any{
			"accountId":    "000000000000",
			"apiId":        "pulse-local",
			"domainName":   r.Host,
			"domainPrefix": "pulse-local",
			"http": map[string]any{
				"method":    r.Method,
				"path":      r.URL.Path,
				"protocol":  r.Proto,
				"sourceIp":  remoteIP(r),
				"userAgent": r.UserAgent(),
			},
			"requestId": requestID,
			"routeKey":  m.RouteKey,
			"stage":     "$default",
			"time":      now.Format(requestTimeLayout),
			"timeEpoch": now.UnixMilli(),
		},
	}
	if len(cookies) > 0 {
		event["cookies"] = cookies
	}
	if qsp != nil {
		event["queryStringParameters"] = qsp
	}
	if len(m.Params) > 0 {
		event["pathParameters"] = m.Params
	}
	if bodyStr != "" {
		event["body"] = bodyStr
	}
	return json.Marshal(event)
}

// buildV1 emits the REST API "payload format 1.0" event.
func buildV1(r *http.Request, body []byte, m *Match, requestID string, now time.Time) ([]byte, error) {
	headers := map[string]string{}
	multiHeaders := map[string][]string{}
	for k, vs := range r.Header {
		headers[k] = vs[len(vs)-1]
		multiHeaders[k] = vs
	}

	var qsp map[string]string
	var mqsp map[string][]string
	if q := r.URL.Query(); len(q) > 0 {
		qsp = map[string]string{}
		mqsp = map[string][]string{}
		for k, vs := range q {
			qsp[k] = vs[len(vs)-1]
			mqsp[k] = vs
		}
	}

	bodyStr, isB64 := encodeBody(body)
	var bodyVal any
	if bodyStr != "" {
		bodyVal = bodyStr
	}
	var params any
	if len(m.Params) > 0 {
		params = m.Params
	}

	event := map[string]any{
		"resource":                        m.Resource,
		"path":                            r.URL.Path,
		"httpMethod":                      r.Method,
		"headers":                         headers,
		"multiValueHeaders":               multiHeaders,
		"queryStringParameters":           orNil(qsp),
		"multiValueQueryStringParameters": orNil(mqsp),
		"pathParameters":                  params,
		"stageVariables":                  nil,
		"body":                            bodyVal,
		"isBase64Encoded":                 isB64,
		"requestContext": map[string]any{
			"accountId":        "000000000000",
			"apiId":            "pulse-local",
			"domainName":       r.Host,
			"httpMethod":       r.Method,
			"path":             r.URL.Path,
			"protocol":         r.Proto,
			"requestId":        requestID,
			"requestTime":      now.Format(requestTimeLayout),
			"requestTimeEpoch": now.UnixMilli(),
			"resourcePath":     m.Resource,
			"stage":            "local",
			"identity": map[string]any{
				"sourceIp":  remoteIP(r),
				"userAgent": r.UserAgent(),
			},
		},
	}
	return json.Marshal(event)
}

// encodeBody returns the event body string and whether it was base64-encoded
// (binary payloads travel base64, exactly like API Gateway).
func encodeBody(body []byte) (string, bool) {
	if len(body) == 0 {
		return "", false
	}
	if utf8.Valid(body) {
		return string(body), false
	}
	return base64.StdEncoding.EncodeToString(body), true
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func orNil[M ~map[string]V, V any](m M) any {
	if len(m) == 0 {
		return nil
	}
	return m
}
