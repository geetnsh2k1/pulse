// Package engine is the long-running heart of pulse: it owns project state,
// runs the worker manager and hot-reload watcher, and exposes the local
// control API consumed by the CLI today and the desktop app later.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"pulse/internal/config"
	"pulse/internal/logs"
	"pulse/internal/store"
	"pulse/internal/version"
	"pulse/internal/watch"
	"pulse/internal/workers"
)

// Milestone is stamped into /health so clients can tell what this build
// of the engine is capable of.
const Milestone = "P1"

type Engine struct {
	cfg  *config.Config
	st   *store.Store
	sink *logs.Sink
	mgr  *workers.Manager
	wtch *watch.Watcher

	// OnEvent, when set (by `pulse start`), receives human-readable
	// happenings like hot reloads. Set it before calling Start.
	OnEvent func(msg string)

	ln        net.Listener
	srv       *http.Server
	startedAt time.Time
	readyIn   time.Duration

	shutdownOnce sync.Once
	shutdownCh   chan struct{}
	serveErrCh   chan error
}

func New(cfg *config.Config, st *store.Store) *Engine {
	return &Engine{
		cfg:        cfg,
		st:         st,
		shutdownCh: make(chan struct{}),
		serveErrCh: make(chan error, 1),
	}
}

// Start boots the worker manager, the file watcher, and the control API,
// then writes the runfile. t0 is when the caller began booting, so ready-in
// reflects the whole boot.
func (e *Engine) Start(t0 time.Time) error {
	e.sink = logs.NewSink(e.st)
	e.mgr = workers.NewManager(e.cfg, e.st, e.sink)
	if err := e.mgr.Start(); err != nil {
		return err
	}

	e.wtch = watch.New(e.cfg, e.onCodeChange)
	if err := e.wtch.Start(); err != nil {
		e.mgr.Shutdown()
		return fmt.Errorf("starting file watcher: %w", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		e.wtch.Stop()
		e.mgr.Shutdown()
		return fmt.Errorf("binding control API: %w", err)
	}
	e.ln = ln
	e.srv = &http.Server{Handler: e.routes()}
	go func() {
		if err := e.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			e.serveErrCh <- err
		}
	}()
	e.startedAt = time.Now()
	e.readyIn = time.Since(t0)

	if err := WriteRunInfo(e.cfg.Root, RunInfo{
		PID:       os.Getpid(),
		Addr:      e.ControlAddr(),
		Project:   e.cfg.Project,
		Root:      e.cfg.Root,
		Version:   version.Version,
		StartedAt: e.startedAt.UTC(),
	}); err != nil {
		e.srv.Close()
		e.wtch.Stop()
		e.mgr.Shutdown()
		return fmt.Errorf("writing runfile: %w", err)
	}
	return nil
}

// ControlAddr is the base URL of the control API, e.g. http://127.0.0.1:52341.
func (e *Engine) ControlAddr() string { return "http://" + e.ln.Addr().String() }

// ReadyIn reports how long boot took.
func (e *Engine) ReadyIn() time.Duration { return e.readyIn }

// Warnings surfaces runtime-resolution warnings for the start banner.
func (e *Engine) Warnings() []string { return e.mgr.Warnings() }

// ShutdownRequested fires once a client POSTs /api/shutdown.
func (e *Engine) ShutdownRequested() <-chan struct{} { return e.shutdownCh }

// ServeErr surfaces a fatal control-API serve error.
func (e *Engine) ServeErr() <-chan error { return e.serveErrCh }

// Shutdown stops the watcher, kills workers, closes the control API, and
// removes the runfile.
func (e *Engine) Shutdown(ctx context.Context) error {
	e.wtch.Stop()
	e.mgr.Shutdown()
	err := e.srv.Shutdown(ctx)
	RemoveRunInfo(e.cfg.Root)
	return err
}

func (e *Engine) event(msg string) {
	if e.OnEvent != nil {
		e.OnEvent(msg)
	}
}

func (e *Engine) onCodeChange(functions []string, reason string) {
	if functions == nil {
		e.sink.System("engine", "", "pulse.yaml changed — restart `pulse start` to apply", time.Now().UnixMilli())
		e.event("pulse.yaml changed — restart `pulse start` to apply it")
		return
	}
	e.mgr.Reload(functions)
	e.event(fmt.Sprintf("↻ hot reload: %s (%s)", strings.Join(functions, ", "), reason))
}

// ---- control API ----

func (e *Engine) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", e.handleHealth)
	mux.HandleFunc("GET /api/functions", e.handleFunctions)
	mux.HandleFunc("GET /api/triggers", e.handleTriggers)
	mux.HandleFunc("GET /api/resources", e.handleResources)
	mux.HandleFunc("POST /api/invoke", e.handleInvoke)
	mux.HandleFunc("GET /api/invocations", e.handleInvocations)
	mux.HandleFunc("GET /api/logs", e.handleLogs)
	mux.HandleFunc("GET /api/logs/stream", e.handleLogStream)
	mux.HandleFunc("POST /api/shutdown", e.handleShutdown)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (e *Engine) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":      "ok",
		"project":     e.cfg.Project,
		"root":        e.cfg.Root,
		"version":     version.Version,
		"milestone":   Milestone,
		"pid":         os.Getpid(),
		"uptime_ms":   time.Since(e.startedAt).Milliseconds(),
		"ready_in_ms": e.readyIn.Milliseconds(),
		"functions":   len(e.cfg.Functions),
	})
}

func (e *Engine) handleFunctions(w http.ResponseWriter, _ *http.Request) {
	out := make([]*config.Function, 0, len(e.cfg.Functions))
	for _, name := range e.cfg.FunctionNames() {
		out = append(out, e.cfg.Functions[name])
	}
	writeJSON(w, http.StatusOK, out)
}

func (e *Engine) handleTriggers(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, e.cfg.Triggers)
}

func (e *Engine) handleResources(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, e.cfg.Resources)
}

// InvokeResult is the wire shape of POST /api/invoke.
type InvokeResult struct {
	RequestID  string          `json:"requestId"`
	Status     string          `json:"status"`
	DurationMs int64           `json:"durationMs"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      json.RawMessage `json:"error,omitempty"`
	Logs       []logs.Line     `json:"logs"`
}

func (e *Engine) handleInvoke(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Function string          `json:"function"`
		Event    json.RawMessage `json:"event"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Function == "" {
		writeError(w, http.StatusBadRequest, `expected body {"function": "...", "event": {...}}`)
		return
	}

	res, err := e.mgr.Invoke(r.Context(), req.Function, "manual", req.Event)
	if err != nil {
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "unknown function") {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}

	out := InvokeResult{
		RequestID:  res.RequestID,
		Status:     res.Status,
		DurationMs: res.DurationMs,
		Logs:       res.Logs,
	}
	if res.Status == "success" {
		out.Result = res.Payload
	} else {
		out.Error = res.Payload
	}
	if out.Logs == nil {
		out.Logs = []logs.Line{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (e *Engine) handleInvocations(w http.ResponseWriter, r *http.Request) {
	rows, err := e.st.RecentInvocations(r.URL.Query().Get("function"), queryLimit(r, 20))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if rows == nil {
		rows = []store.InvocationRow{}
	}
	writeJSON(w, http.StatusOK, rows)
}

func (e *Engine) handleLogs(w http.ResponseWriter, r *http.Request) {
	rows, err := e.st.RecentLogs(r.URL.Query().Get("function"), queryLimit(r, 100))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if rows == nil {
		rows = []store.LogRow{}
	}
	writeJSON(w, http.StatusOK, rows)
}

// handleLogStream is a Server-Sent Events feed of live log lines; the CLI's
// `logs --follow` and the future UI both sit on this.
func (e *Engine) handleLogStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	ch, cancel := e.sink.Subscribe(r.URL.Query().Get("function"))
	defer cancel()

	for {
		select {
		case line := <-ch:
			b, _ := json.Marshal(line)
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		case <-r.Context().Done():
			return
		case <-e.shutdownCh:
			return
		}
	}
}

func (e *Engine) handleShutdown(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "shutting down"})
	e.shutdownOnce.Do(func() { close(e.shutdownCh) })
}

func queryLimit(r *http.Request, def int) int {
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 5000 {
			return n
		}
	}
	return def
}
