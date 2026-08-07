package gateway

import (
	"encoding/base64"
	"encoding/json"
	"net/http"

	"github.com/geetnsh2k1/pulse/internal/workers"
)

// lambdaResponse is the envelope functions return for structured responses.
type lambdaResponse struct {
	StatusCode        int                 `json:"statusCode"`
	Headers           map[string]string   `json:"headers"`
	MultiValueHeaders map[string][]string `json:"multiValueHeaders"` // v1
	Body              string              `json:"body"`
	IsBase64Encoded   bool                `json:"isBase64Encoded"`
	Cookies           []string            `json:"cookies"` // v2
}

// writeResponse maps a function outcome onto the HTTP response and returns
// the status code written (for the access log). Mirrors API Gateway:
//   - function error/timeout → 500 (v2) / 502 (v1) with a generic message
//   - v2 auto-wraps bare JSON values as 200 application/json
//   - v1 requires the statusCode envelope; anything else is 502
func writeResponse(w http.ResponseWriter, res *workers.Result, format string) int {
	if res.Status != "success" {
		return writeGatewayError(w, format)
	}
	payload := res.Payload

	var probe map[string]json.RawMessage
	structured := json.Unmarshal(payload, &probe) == nil && probe != nil && probe["statusCode"] != nil
	if !structured {
		if format == "1.0" {
			return writeGatewayError(w, format)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
		return http.StatusOK
	}

	var lr lambdaResponse
	if err := json.Unmarshal(payload, &lr); err != nil || lr.StatusCode < 100 || lr.StatusCode > 599 {
		return writeGatewayError(w, format)
	}

	for k, v := range lr.Headers {
		w.Header().Set(k, v)
	}
	for k, vs := range lr.MultiValueHeaders {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	for _, c := range lr.Cookies {
		w.Header().Add("Set-Cookie", c)
	}

	body := []byte(lr.Body)
	if lr.IsBase64Encoded {
		decoded, err := base64.StdEncoding.DecodeString(lr.Body)
		if err != nil {
			return writeGatewayError(w, format)
		}
		body = decoded
	}
	w.WriteHeader(lr.StatusCode)
	_, _ = w.Write(body)
	return lr.StatusCode
}

func writeGatewayError(w http.ResponseWriter, format string) int {
	if format == "1.0" {
		return writeMessage(w, http.StatusBadGateway, "Internal server error")
	}
	return writeMessage(w, http.StatusInternalServerError, "Internal Server Error")
}

func writeMessage(w http.ResponseWriter, status int, msg string) int {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
	return status
}
