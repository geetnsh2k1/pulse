// Package awsfacade is the single local endpoint AWS SDKs talk to. Workers
// run with AWS_ENDPOINT_URL pointing here, so unmodified boto3 / AWS SDK v3
// code hits pulse's service mocks instead of real AWS — which also means
// local code can never accidentally touch a real account.
//
// The façade speaks the modern AWS JSON protocol (routing on the
// X-Amz-Target header). Legacy Query-protocol SDKs get a clear upgrade hint.
package awsfacade

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// APIError is an AWS-shaped service error.
type APIError struct {
	Type      string // smithy id, e.g. "com.amazonaws.sqs#QueueDoesNotExist"
	QueryCode string // legacy code, sent in x-amzn-query-error for SDK compat
	Message   string
	Status    int // defaults to 400
}

func (e *APIError) Error() string { return e.Type + ": " + e.Message }

// Service handles JSON-protocol actions for one AWS service.
type Service interface {
	Do(action string, body []byte) (any, *APIError)
}

type Facade struct {
	mu       sync.RWMutex
	byPrefix map[string]Service // X-Amz-Target prefix → service
	names    []string           // human-readable service names, e.g. "sqs"

	ln       net.Listener
	srv      *http.Server
	serveErr chan error
}

func New() *Facade {
	return &Facade{byPrefix: map[string]Service{}, serveErr: make(chan error, 1)}
}

// Register mounts (or replaces) a service under its X-Amz-Target prefix
// ("AmazonSQS"). Replacement keeps the façade address stable across config
// hot-applies.
func (f *Facade) Register(targetPrefix, name string, svc Service) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byPrefix[targetPrefix] = svc
	for _, n := range f.names {
		if n == name {
			return
		}
	}
	f.names = append(f.names, name)
}

func (f *Facade) service(prefix string) Service {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.byPrefix[prefix]
}

// Names lists registered services for banners and health output.
func (f *Facade) Names() []string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return append([]string(nil), f.names...)
}

// Start binds the façade to localhost (port 0 = random).
func (f *Facade) Start(port int) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("binding aws endpoint: %w", err)
	}
	f.ln = ln
	f.srv = &http.Server{Handler: f}
	go func() {
		if err := f.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			f.serveErr <- err
		}
	}()
	return nil
}

// URL is what goes into AWS_ENDPOINT_URL.
func (f *Facade) URL() string {
	return "http://" + f.ln.Addr().String()
}

func (f *Facade) ServeErr() <-chan error { return f.serveErr }

func (f *Facade) Close() error {
	if f.srv == nil {
		return nil
	}
	return f.srv.Close()
}

func (f *Facade) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("x-amzn-RequestId", uuid.NewString())

	target := r.Header.Get("X-Amz-Target")
	if target == "" {
		// Legacy Query-protocol SDKs post form-encoded Action=... bodies.
		if ct := r.Header.Get("Content-Type"); strings.Contains(ct, "x-www-form-urlencoded") {
			writeAPIError(w, &APIError{
				Type:      "com.amazonaws#InvalidAction",
				QueryCode: "InvalidAction",
				Message: "pulse speaks the AWS JSON protocol; this request used the legacy Query protocol. " +
					"Upgrade your SDK (boto3 ≥ 1.28, AWS SDK for JavaScript v3, AWS CLI v2.13+) and retry.",
			})
			return
		}
		writeAPIError(w, &APIError{
			Type:      "com.amazonaws#MissingAction",
			QueryCode: "MissingAction",
			Message:   "missing X-Amz-Target header — pulse routes AWS JSON-protocol requests by it",
		})
		return
	}

	prefix, action, ok := strings.Cut(target, ".")
	svc := f.service(prefix)
	if !ok || svc == nil {
		writeAPIError(w, &APIError{
			Type:      "com.amazonaws#UnknownService",
			QueryCode: "InvalidAction",
			Message: fmt.Sprintf("pulse does not emulate %q yet (available: %s) — this call would have gone to real AWS",
				prefix, strings.Join(f.names, ", ")),
		})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 5<<20))
	if err != nil {
		writeAPIError(w, &APIError{Type: "com.amazonaws#InvalidRequest", QueryCode: "InvalidRequest", Message: err.Error()})
		return
	}

	resp, apiErr := svc.Do(action, body)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	w.Header().Set("Content-Type", "application/x-amz-json-1.0")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func writeAPIError(w http.ResponseWriter, e *APIError) {
	status := e.Status
	if status == 0 {
		status = http.StatusBadRequest
	}
	if e.QueryCode != "" {
		w.Header().Set("x-amzn-query-error", e.QueryCode+";Sender")
	}
	w.Header().Set("Content-Type", "application/x-amz-json-1.0")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"__type": e.Type, "message": e.Message})
}
