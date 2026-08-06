// Package engine is the long-running heart of pulse: it owns project state,
// runs the worker manager, gateway, queue pollers, and hot-reload watcher,
// and exposes the local control API consumed by the CLI and the future UI.
//
// pulse.yaml changes apply LIVE: the engine validates the new config, swaps
// its subsystems in place, and keeps serving — no restarts. Invalid configs
// are rejected with the full problem list while the old config keeps running.
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

	"pulse/internal/awsfacade"
	"pulse/internal/config"
	"pulse/internal/esm"
	"pulse/internal/gateway"
	"pulse/internal/logs"
	ddbsvc "pulse/internal/services/dynamodb"
	sqssvc "pulse/internal/services/sqs"
	"pulse/internal/store"
	"pulse/internal/version"
	"pulse/internal/watch"
	"pulse/internal/workers"
)

// Milestone is stamped into /health so clients can tell what this build
// of the engine is capable of.
const Milestone = "P4+dx"

type Engine struct {
	// immutable after New/Start
	st     *store.Store
	path   string // pulse.yaml location
	root   string
	sink   *logs.Sink
	facade *awsfacade.Facade

	// OnEvent, when set (by `pulse start`), receives human-readable
	// happenings like hot reloads and config applies. Set before Start.
	OnEvent func(msg string)

	// mu guards the swappable state below (config hot-apply).
	mu       sync.RWMutex
	cfg      *config.Config
	sqs      *sqssvc.Service
	ddb      *ddbsvc.Service
	mgr      *workers.Manager
	gw       *gateway.Server // nil when the config has no http triggers
	pollers  *esm.Poller     // nil when the config has no sqs triggers
	wtch     *watch.Watcher
	applying bool

	ln        net.Listener
	srv       *http.Server
	startedAt time.Time
	readyIn   time.Duration

	shutdownOnce sync.Once
	shutdownCh   chan struct{}
	serveErrCh   chan error

	// celebrateMu serializes the once-ever first-job console line.
	celebrateMu sync.Mutex
}

func New(cfg *config.Config, st *store.Store) *Engine {
	return &Engine{
		st:         st,
		cfg:        cfg,
		path:       cfg.Path,
		root:       cfg.Root,
		shutdownCh: make(chan struct{}),
		serveErrCh: make(chan error, 1),
	}
}

// Start boots the AWS façade, all config-driven subsystems, and the control
// API, then writes the runfile. t0 is when the caller began booting.
func (e *Engine) Start(t0 time.Time) error {
	e.sink = logs.NewSink(e.st)

	// The façade outlives config applies so worker env stays valid.
	e.facade = awsfacade.New()
	if err := e.facade.Start(0); err != nil {
		return err
	}
	go func() {
		select {
		case err := <-e.facade.ServeErr():
			if err != nil {
				e.serveErrCh <- fmt.Errorf("aws endpoint: %w", err)
			}
		case <-e.shutdownCh:
		}
	}()

	if err := e.startSubsystems(e.cfg); err != nil {
		_ = e.facade.Close()
		return err
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		e.stopSubsystems()
		_ = e.facade.Close()
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

	if err := e.writeRunfile(); err != nil {
		e.srv.Close()
		e.stopSubsystems()
		_ = e.facade.Close()
		return fmt.Errorf("writing runfile: %w", err)
	}
	return nil
}

// startSubsystems builds everything derived from cfg and publishes it.
func (e *Engine) startSubsystems(cfg *config.Config) error {
	sqs := sqssvc.New(cfg, e.st)
	sqs.SetBaseURL(e.facade.URL)
	sqs.SetOnEvent(e.event)

	ddb := ddbsvc.New(cfg, e.st)
	if err := ddb.Init(cfg); err != nil {
		return fmt.Errorf("initializing local dynamodb: %w", err)
	}
	e.facade.Register("AmazonSQS", "sqs", sqs)
	e.facade.Register("DynamoDB_20120810", "dynamodb", ddb)

	mgr := workers.NewManager(cfg, e.st, e.sink)
	mgr.SetAWSEndpoint(e.facade.URL())
	if err := mgr.Start(); err != nil {
		return err
	}

	var gw *gateway.Server
	if hasTrigger(cfg, "http") {
		gw = gateway.New(cfg, mgr, e.sink)
		gw.OnRequest = e.event
		if err := gw.Start(cfg.API.Port); err != nil {
			mgr.Shutdown()
			return err
		}
		go func(gw *gateway.Server) {
			select {
			case err := <-gw.ServeErr():
				if err != nil {
					e.serveErrCh <- fmt.Errorf("api server: %w", err)
				}
			case <-e.shutdownCh:
			}
		}(gw)
	}

	var pollers *esm.Poller
	if hasTrigger(cfg, "sqs") {
		pollers = esm.New(cfg, sqs, mgr, e.sink, e.event)
		pollers.CelebrateOK = e.celebrateFirstJob
		pollers.Start()
	}

	wtch := watch.New(cfg, e.onCodeChange)
	if err := wtch.Start(); err != nil {
		if pollers != nil {
			pollers.Stop()
		}
		if gw != nil {
			shutdownGateway(gw)
		}
		mgr.Shutdown()
		return fmt.Errorf("starting file watcher: %w", err)
	}

	e.mu.Lock()
	e.cfg, e.sqs, e.ddb, e.mgr, e.gw, e.pollers, e.wtch = cfg, sqs, ddb, mgr, gw, pollers, wtch
	e.mu.Unlock()
	return nil
}

// stopSubsystems tears down everything startSubsystems built.
func (e *Engine) stopSubsystems() {
	e.mu.Lock()
	pollers, gw, wtch, mgr := e.pollers, e.gw, e.wtch, e.mgr
	e.pollers, e.gw, e.wtch, e.mgr = nil, nil, nil, nil
	e.mu.Unlock()

	if pollers != nil {
		pollers.Stop()
	}
	if gw != nil {
		shutdownGateway(gw)
	}
	if wtch != nil {
		wtch.Stop()
	}
	if mgr != nil {
		mgr.Shutdown()
	}
}

func shutdownGateway(gw *gateway.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = gw.Shutdown(ctx)
}

// applyConfig hot-swaps the running config after a pulse.yaml save.
func (e *Engine) applyConfig() {
	e.mu.Lock()
	if e.applying {
		e.mu.Unlock()
		return
	}
	e.applying = true
	old := e.cfg
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.applying = false
		e.mu.Unlock()
	}()

	newCfg, err := config.Load(e.path)
	if err != nil {
		e.event("✗ pulse.yaml changed but has problems — keeping the current config:\n" + err.Error())
		e.sink.System("engine", "", "rejected pulse.yaml change: "+err.Error(), time.Now().UnixMilli())
		return
	}

	e.event("pulse.yaml changed — applying live…")
	e.stopSubsystems()
	if err := e.startSubsystems(newCfg); err != nil {
		e.event(fmt.Sprintf("✗ couldn't apply the new config (%v) — rolling back", err))
		if err2 := e.startSubsystems(old); err2 != nil {
			e.serveErrCh <- fmt.Errorf("config rollback failed: %w", err2)
		}
		return
	}
	_ = e.writeRunfile()

	e.event(fmt.Sprintf("✓ config applied — %d function(s), %d trigger(s)%s",
		len(newCfg.Functions), len(newCfg.Triggers), apiNote(e.APIURL())))
	e.mu.RLock()
	mgr := e.mgr
	e.mu.RUnlock()
	if mgr != nil {
		for _, note := range mgr.Warnings() {
			e.event("note: " + note)
		}
	}
	e.sink.System("engine", "", "config applied live", time.Now().UnixMilli())
}

func apiNote(apiURL string) string {
	if apiURL == "" {
		return ""
	}
	return ", api " + apiURL
}

func hasTrigger(cfg *config.Config, kind string) bool {
	for _, t := range cfg.Triggers {
		if t.Type == kind {
			return true
		}
	}
	return false
}

func (e *Engine) writeRunfile() error {
	return WriteRunInfo(e.root, RunInfo{
		PID:       os.Getpid(),
		Addr:      e.ControlAddr(),
		APIAddr:   e.APIURL(),
		AWSAddr:   e.facade.URL(),
		Project:   e.currentCfg().Project,
		Root:      e.root,
		Version:   version.Version,
		StartedAt: e.startedAt.UTC(),
	})
}

func (e *Engine) currentCfg() *config.Config {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.cfg
}

// state snapshots the swappable pieces for request handlers.
func (e *Engine) state() (*config.Config, *workers.Manager, *sqssvc.Service, *ddbsvc.Service, *gateway.Server) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.cfg, e.mgr, e.sqs, e.ddb, e.gw
}

// ControlAddr is the base URL of the control API, e.g. http://127.0.0.1:52341.
func (e *Engine) ControlAddr() string { return "http://" + e.ln.Addr().String() }

// ReadyIn reports how long boot took.
func (e *Engine) ReadyIn() time.Duration { return e.readyIn }

// AWSURL is the local AWS endpoint (the façade) workers talk to.
func (e *Engine) AWSURL() string {
	if e.facade == nil {
		return ""
	}
	return e.facade.URL()
}

// AWSServices names the emulated services, for banners.
func (e *Engine) AWSServices() []string {
	if e.facade == nil {
		return nil
	}
	return e.facade.Names()
}

// APIURL returns the local API base URL, or "" when no http triggers exist.
func (e *Engine) APIURL() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.gw == nil {
		return ""
	}
	return e.gw.URL()
}

// Routes lists the API routes served by the gateway.
func (e *Engine) Routes() []gateway.RouteInfo {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.gw == nil {
		return nil
	}
	return e.gw.Routes()
}

// Warnings surfaces runtime-resolution notes for the start banner.
func (e *Engine) Warnings() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.mgr == nil {
		return nil
	}
	return e.mgr.Warnings()
}

// LogFeed streams every captured log line from every function — the start
// console prints these so the terminal tells the whole story.
func (e *Engine) LogFeed() (<-chan logs.Line, func()) {
	return e.sink.Subscribe("")
}

// ShutdownRequested fires once a client POSTs /api/shutdown.
func (e *Engine) ShutdownRequested() <-chan struct{} { return e.shutdownCh }

// ServeErr surfaces a fatal serve error.
func (e *Engine) ServeErr() <-chan error { return e.serveErrCh }

// Shutdown stops everything and removes the runfile.
func (e *Engine) Shutdown(ctx context.Context) error {
	e.shutdownOnce.Do(func() { close(e.shutdownCh) })
	e.stopSubsystems()
	_ = e.facade.Close()
	err := e.srv.Shutdown(ctx)
	RemoveRunInfo(e.root)
	return err
}

func (e *Engine) event(msg string) {
	if e.OnEvent != nil {
		e.OnEvent(msg)
	}
}

// celebrateFirstJob prints a one-time line the first time this project ever
// processes a background job successfully — the aha moment of the async
// loop. The KV flag makes it once per project, forever.
func (e *Engine) celebrateFirstJob() {
	e.celebrateMu.Lock()
	defer e.celebrateMu.Unlock()
	if _, ok, err := e.st.GetKV("celebrated_first_job"); ok || err != nil {
		return
	}
	if err := e.st.SetKV("celebrated_first_job", "1"); err != nil {
		return
	}
	e.event("🎉 first background job processed — your async loop works end to end")
}

func (e *Engine) onCodeChange(functions []string, reason string) {
	if functions == nil {
		e.applyConfig()
		return
	}
	e.mu.RLock()
	mgr := e.mgr
	e.mu.RUnlock()
	if mgr != nil {
		mgr.Reload(functions)
	}
	e.event(fmt.Sprintf("↻ hot reload: %s (%s)", strings.Join(functions, ", "), reason))
}

// ---- control API ----

func (e *Engine) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", e.handleHealth)
	mux.HandleFunc("GET /api/functions", e.handleFunctions)
	mux.HandleFunc("GET /api/triggers", e.handleTriggers)
	mux.HandleFunc("GET /api/resources", e.handleResources)
	mux.HandleFunc("GET /api/routes", e.handleRoutes)
	mux.HandleFunc("GET /api/queues", e.handleQueues)
	mux.HandleFunc("POST /api/queues/send", e.handleQueueSend)
	mux.HandleFunc("GET /api/tables", e.handleTables)
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
	cfg, _, _, _, gw := e.state()
	var api any
	if gw != nil {
		api = map[string]any{"url": gw.URL(), "routes": len(gw.Routes())}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"aws":         map[string]any{"url": e.facade.URL(), "services": e.facade.Names()},
		"status":      "ok",
		"project":     cfg.Project,
		"root":        e.root,
		"version":     version.Version,
		"milestone":   Milestone,
		"pid":         os.Getpid(),
		"uptime_ms":   time.Since(e.startedAt).Milliseconds(),
		"ready_in_ms": e.readyIn.Milliseconds(),
		"functions":   len(cfg.Functions),
		"api":         api,
	})
}

func (e *Engine) handleFunctions(w http.ResponseWriter, _ *http.Request) {
	cfg, _, _, _, _ := e.state()
	out := make([]*config.Function, 0, len(cfg.Functions))
	for _, name := range cfg.FunctionNames() {
		out = append(out, cfg.Functions[name])
	}
	writeJSON(w, http.StatusOK, out)
}

func (e *Engine) handleTriggers(w http.ResponseWriter, _ *http.Request) {
	cfg, _, _, _, _ := e.state()
	writeJSON(w, http.StatusOK, cfg.Triggers)
}

func (e *Engine) handleResources(w http.ResponseWriter, _ *http.Request) {
	cfg, _, _, _, _ := e.state()
	writeJSON(w, http.StatusOK, cfg.Resources)
}

func (e *Engine) handleRoutes(w http.ResponseWriter, _ *http.Request) {
	routes := e.Routes()
	if routes == nil {
		routes = []gateway.RouteInfo{}
	}
	writeJSON(w, http.StatusOK, routes)
}

func (e *Engine) handleQueues(w http.ResponseWriter, _ *http.Request) {
	_, _, sqs, _, _ := e.state()
	if sqs == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	writeJSON(w, http.StatusOK, sqs.AllStats())
}

func (e *Engine) handleTables(w http.ResponseWriter, _ *http.Request) {
	_, _, _, ddb, _ := e.state()
	if ddb == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	writeJSON(w, http.StatusOK, ddb.AllStats())
}

// handleQueueSend lets `pulse send` (and the future UI) drop a message on a
// queue without going through an SDK.
func (e *Engine) handleQueueSend(w http.ResponseWriter, r *http.Request) {
	_, _, sqs, _, _ := e.state()
	if sqs == nil {
		writeError(w, http.StatusServiceUnavailable, "config is being applied — retry in a moment")
		return
	}
	var req struct {
		Queue        string `json:"queue"`
		Body         string `json:"body"`
		DelaySeconds int    `json:"delaySeconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Queue == "" {
		writeError(w, http.StatusBadRequest, `expected body {"queue": "...", "body": "..."}`)
		return
	}
	id, apiErr := sqs.Send(req.Queue, req.Body, req.DelaySeconds, nil)
	if apiErr != nil {
		status := http.StatusBadRequest
		if strings.Contains(apiErr.Type, "QueueDoesNotExist") {
			status = http.StatusNotFound
		}
		writeError(w, status, apiErr.Message)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"messageId": id})
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
	_, mgr, _, _, _ := e.state()
	if mgr == nil {
		writeError(w, http.StatusServiceUnavailable, "config is being applied — retry in a moment")
		return
	}
	var req struct {
		Function string          `json:"function"`
		Event    json.RawMessage `json:"event"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Function == "" {
		writeError(w, http.StatusBadRequest, `expected body {"function": "...", "event": {...}}`)
		return
	}

	res, err := mgr.Invoke(r.Context(), req.Function, "manual", req.Event)
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
