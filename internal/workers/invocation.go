package workers

import (
	"encoding/json"
	"sync"

	"pulse/internal/logs"
)

// Invocation is one request to run a function. It resolves exactly once.
type Invocation struct {
	ID       string
	Function string
	Source   string // manual | http | sqs | ... (trigger sources arrive in later phases)
	Payload  []byte

	once   sync.Once
	done   chan struct{}
	result *Result
}

// Result is the outcome handed back to whoever invoked.
type Result struct {
	RequestID  string      `json:"requestId"`
	Status     string      `json:"status"` // success | error | timeout
	Payload    []byte      `json:"-"`      // response JSON, or an AWS-shaped error document
	DurationMs int64       `json:"durationMs"`
	Logs       []logs.Line `json:"logs,omitempty"`
}

// ErrorMessage extracts errorMessage from an AWS error document, best effort.
func (r *Result) ErrorMessage() string {
	if r.Status == "success" {
		return ""
	}
	var doc struct {
		ErrorMessage string `json:"errorMessage"`
	}
	_ = json.Unmarshal(r.Payload, &doc)
	if doc.ErrorMessage == "" {
		return "invocation failed"
	}
	return doc.ErrorMessage
}

func newInvocation(id, function, source string, payload []byte) *Invocation {
	return &Invocation{ID: id, Function: function, Source: source, Payload: payload, done: make(chan struct{})}
}

// complete resolves the invocation; later calls are no-ops, so the timeout
// timer, a worker crash, and a real response can race safely.
func (inv *Invocation) complete(res *Result) {
	inv.once.Do(func() {
		res.RequestID = inv.ID
		inv.result = res
		close(inv.done)
	})
}

// errDoc builds an AWS-shaped error document.
func errDoc(message, errorType string) []byte {
	b, _ := json.Marshal(map[string]string{"errorMessage": message, "errorType": errorType})
	return b
}
