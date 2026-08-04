package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/google/uuid"

	"pulse/internal/config"
	"pulse/internal/logs"
	"pulse/internal/workers"
)

// maxBodyBytes mirrors API Gateway's 10MB payload cap.
const maxBodyBytes = 10 << 20

// Invoker is what the gateway needs from the worker manager.
type Invoker interface {
	InvokeAs(ctx context.Context, requestID, function, source string, payload []byte) (*workers.Result, error)
}

type Server struct {
	router *Router
	inv    Invoker
	sink   *logs.Sink

	// OnRequest receives one human-readable access-log line per request.
	OnRequest func(line string)

	ln       net.Listener
	srv      *http.Server
	serveErr chan error
}

func New(cfg *config.Config, inv Invoker, sink *logs.Sink) *Server {
	return &Server{router: NewRouter(cfg), inv: inv, sink: sink, serveErr: make(chan error, 1)}
}

func (s *Server) Routes() []RouteInfo { return s.router.Routes() }

// Start binds the API server to localhost:port (0 = random free port).
func (s *Server) Start(port int) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("api port %d is unavailable (%w) — set api.port in pulse.yaml or pass --port", port, err)
	}
	s.ln = ln
	s.srv = &http.Server{Handler: s}
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.serveErr <- err
		}
	}()
	return nil
}

// URL is the human-facing base URL, e.g. http://localhost:3000.
func (s *Server) URL() string {
	port := s.ln.Addr().(*net.TCPAddr).Port
	return fmt.Sprintf("http://localhost:%d", port)
}

func (s *Server) ServeErr() <-chan error { return s.serveErr }

func (s *Server) Shutdown(ctx context.Context) error { return s.srv.Shutdown(ctx) }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	m, ok := s.router.Match(r.Method, r.URL.Path)
	if !ok {
		writeMessage(w, http.StatusNotFound, "Not Found")
		s.access(fmt.Sprintf("%s %s → 404 · no route", r.Method, r.URL.Path))
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		writeMessage(w, http.StatusBadRequest, "Bad Request")
		return
	}
	if len(body) > maxBodyBytes {
		writeMessage(w, http.StatusRequestEntityTooLarge, "Request Entity Too Large")
		return
	}

	requestID := uuid.NewString()
	event, err := buildEvent(r, body, m, requestID, start)
	if err != nil {
		writeMessage(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}

	// Background context: like AWS, a client hanging up doesn't stop the
	// function mid-flight.
	res, err := s.inv.InvokeAs(context.Background(), requestID, m.Function, "http", event)
	if err != nil {
		writeMessage(w, http.StatusInternalServerError, "Internal Server Error")
		s.access(fmt.Sprintf("%s %s → %s · 500 · %s", r.Method, r.URL.Path, m.Function, err))
		return
	}

	status := writeResponse(w, res, m.Format)

	line := fmt.Sprintf("%s %s → %s · %d · %dms", r.Method, r.URL.Path, m.Function, status, time.Since(start).Milliseconds())
	s.access(line)
	s.sink.System(m.Function, requestID, line, time.Now().UnixMilli())
}

func (s *Server) access(line string) {
	if s.OnRequest != nil {
		s.OnRequest(line)
	}
}
