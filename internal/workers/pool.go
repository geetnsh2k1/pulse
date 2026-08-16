package workers

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/geetnsh2k1/pulse/internal/config"
	"github.com/geetnsh2k1/pulse/internal/logs"
)

const (
	maxWorkersPerFunction = 4
	pendingQueueSize      = 512
	maxInitFailures       = 3
	// timeoutGrace gives the shim a beat to deliver its own error before the
	// engine declares a hard timeout and kills the process.
	timeoutGrace = 200 * time.Millisecond
)

// pool owns everything about one function: its runtime API listener, its
// worker processes, the pending-invocation queue, and in-flight bookkeeping.
//
// The engine never pushes work into workers. Workers long-poll
// GET /runtime/invocation/next exactly like real Lambda runtimes, which is
// what lets the shims stay tiny and future official runtime clients plug in.
type pool struct {
	fn          *config.Function
	dotEnv      map[string]string // project .env, the base layer for fn.Env
	region      string
	arn         string
	projectRoot string
	taskRoot    string
	shimDir     string
	sink        *logs.Sink
	awsEndpoint string

	rb      *runtimeBinary
	rbErr   error    // set when no usable interpreter exists
	rbNotes []string // venv/version notes for the start banner

	runtimeAddr string
	ln          net.Listener
	srv         *http.Server

	pending chan *Invocation

	mu           sync.Mutex
	workers      map[string]*worker
	inflight     map[string]*flight
	gen          int
	genCh        chan struct{}
	initFails    int
	lastInitErr  []byte
	nextWorkerID int
	closed       bool
}

// flight is one dispatched invocation: which worker has it and its deadline.
type flight struct {
	inv          *Invocation
	w            *worker
	timer        *time.Timer
	dispatchedAt time.Time
}

func newPool(fn *config.Function, cfg *config.Config, sink *logs.Sink, shimDir string) *pool {
	return &pool{
		fn:          fn,
		dotEnv:      cfg.DotEnv,
		region:      cfg.Region,
		arn:         fmt.Sprintf("arn:aws:lambda:%s:000000000000:function:%s", cfg.Region, fn.Name),
		projectRoot: cfg.Root,
		taskRoot:    filepath.Join(cfg.Root, fn.CodeDir),
		shimDir:     shimDir,
		sink:        sink,
		pending:     make(chan *Invocation, pendingQueueSize),
		workers:     map[string]*worker{},
		inflight:    map[string]*flight{},
		genCh:       make(chan struct{}),
	}
}

func (p *pool) start() error {
	p.rb, p.rbNotes, p.rbErr = resolveRuntime(p.fn.Runtime, p.projectRoot)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("binding runtime API for %s: %w", p.fn.Name, err)
	}
	p.ln = ln
	p.runtimeAddr = ln.Addr().String()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /2018-06-01/runtime/invocation/next", p.handleNext)
	mux.HandleFunc("POST /2018-06-01/runtime/invocation/{id}/response", p.handleResponse)
	mux.HandleFunc("POST /2018-06-01/runtime/invocation/{id}/error", p.handleError)
	mux.HandleFunc("POST /2018-06-01/runtime/init/error", p.handleInitError)
	p.srv = &http.Server{Handler: mux}
	go func() { _ = p.srv.Serve(ln) }()
	return nil
}

// invoke queues an invocation, makes sure capacity exists, and waits for the
// outcome. Timeouts are enforced internally; ctx is only a safety net.
func (p *pool) invoke(ctx context.Context, inv *Invocation) (*Result, error) {
	p.mu.Lock()
	switch {
	case p.closed:
		p.mu.Unlock()
		return nil, fmt.Errorf("engine is shutting down")
	case p.rb == nil:
		msg := "runtime unavailable"
		if p.rbErr != nil {
			msg = p.rbErr.Error()
		}
		p.mu.Unlock()
		inv.complete(&Result{Status: "error", Payload: errDoc(msg, "Runtime.NotFound")})
		return inv.result, nil
	case p.initFails >= maxInitFailures && p.lastInitErr != nil:
		doc := p.lastInitErr
		p.mu.Unlock()
		inv.complete(&Result{Status: "error", Payload: doc})
		return inv.result, nil
	}
	p.mu.Unlock()

	select {
	case p.pending <- inv:
	default:
		return nil, fmt.Errorf("function %q invocation queue is full (%d waiting)", p.fn.Name, pendingQueueSize)
	}
	p.ensureCapacity()

	select {
	case <-inv.done:
		return inv.result, nil
	case <-ctx.Done():
		inv.complete(&Result{Status: "error", Payload: errDoc("invocation abandoned: "+ctx.Err().Error(), "Pulse.Internal")})
		return inv.result, nil
	}
}

// ensureCapacity spawns workers until there are enough idle ones for the
// queued work, capped by maxWorkersPerFunction.
func (p *pool) ensureCapacity() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.rb == nil || p.initFails >= maxInitFailures {
		return
	}
	idle := len(p.workers) - len(p.inflight)
	for idle < len(p.pending) && len(p.workers) < maxWorkersPerFunction {
		p.spawnLocked()
		idle++
	}
}

func (p *pool) spawnLocked() {
	id := fmt.Sprintf("w%d", p.nextWorkerID)
	p.nextWorkerID++

	var cmd *exec.Cmd
	switch p.rb.Family {
	case "node":
		cmd = exec.Command(p.rb.Path, filepath.Join(p.shimDir, "bootstrap.mjs"))
	case "python":
		cmd = exec.Command(p.rb.Path, "-u", filepath.Join(p.shimDir, "bootstrap.py"))
	}
	cmd.Dir = p.taskRoot
	cmd.Env = p.workerEnv(id)

	w := &worker{id: id, gen: p.gen, cmd: cmd}
	p.workers[id] = w
	go w.start(p)
}

// layerDir is where `pulse import aws` unpacks a function's Lambda layers,
// relative to its code directory. Kept in step with importer.LayerDir — the
// convention is the wiring, so pulse.yaml needs no new field.
const layerDir = "_layers"

// layerPaths puts an imported function's layers on the runtime's module path,
// mirroring AWS: layers are mounted at /opt, with /opt/python (and its
// site-packages) on PYTHONPATH and /opt/nodejs/node_modules on NODE_PATH.
// Without this, a function whose dependencies live in a layer imports fine in
// production and fails locally on the first line.
func layerPaths(taskRoot string) []string {
	root := filepath.Join(taskRoot, layerDir)
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return nil
	}

	var pyPaths []string
	if py := filepath.Join(root, "python"); isDir(py) {
		pyPaths = append(pyPaths, py)
		// AWS also honours the versioned site-packages layout layers are often
		// built with; add whichever ones this layer actually contains.
		if matches, err := filepath.Glob(filepath.Join(py, "lib", "python*", "site-packages")); err == nil {
			for _, m := range matches {
				if isDir(m) {
					pyPaths = append(pyPaths, m)
				}
			}
		}
	}

	var out []string
	if len(pyPaths) > 0 {
		// Prepend, so a layer can shadow a system package the way it does on
		// Lambda, and keep any PYTHONPATH the user set.
		if existing := os.Getenv("PYTHONPATH"); existing != "" {
			pyPaths = append(pyPaths, existing)
		}
		out = append(out, "PYTHONPATH="+strings.Join(pyPaths, string(os.PathListSeparator)))
	}
	if node := filepath.Join(root, "nodejs", "node_modules"); isDir(node) {
		out = append(out, "NODE_PATH="+node)
	}
	return out
}

func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

func (p *pool) workerEnv(workerID string) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"TMPDIR=" + os.TempDir(),
		"LANG=en_US.UTF-8",
		// NOTE(windows): SystemRoot/TEMP will need forwarding for beta.
		"_HANDLER=" + p.fn.Handler,
		"LAMBDA_TASK_ROOT=" + p.taskRoot,
		"AWS_LAMBDA_RUNTIME_API=" + p.runtimeAddr,
		"AWS_LAMBDA_FUNCTION_NAME=" + p.fn.Name,
		"AWS_LAMBDA_FUNCTION_VERSION=$LATEST",
		fmt.Sprintf("AWS_LAMBDA_FUNCTION_MEMORY_SIZE=%d", p.fn.Memory),
		"AWS_REGION=" + p.region,
		"AWS_DEFAULT_REGION=" + p.region,
		"AWS_ACCESS_KEY_ID=PULSELOCALACCESSKEY",
		"AWS_SECRET_ACCESS_KEY=pulse-local-secret-key",
		"PULSE_WORKER_ID=" + workerID,
		"PYTHONUNBUFFERED=1",
	}
	env = append(env, layerPaths(p.taskRoot)...)
	if p.awsEndpoint != "" {
		// Point AWS SDKs (boto3 ≥1.28, JS v3, CLI v2.13+) at the local
		// façade. Everything a worker does stays on this machine.
		env = append(env,
			"AWS_ENDPOINT_URL="+p.awsEndpoint,
			"AWS_ENDPOINT_URL_SQS="+p.awsEndpoint,
			"AWS_ENDPOINT_URL_DYNAMODB="+p.awsEndpoint,
		)
	}
	// Precedence, lowest to highest: .env (shared, uncommitted) → the
	// function's own env: (explicit, per-function) → the pulse-controlled
	// AWS_* vars above, which must always win or the local cloud breaks.
	// The parent shell is deliberately NOT inherited: in AWS a function
	// sees only its configured variables, and pulse matches that.
	merged := make(map[string]string, len(p.dotEnv)+len(p.fn.Env))
	for k, v := range p.dotEnv {
		merged[k] = v
	}
	for k, v := range p.fn.Env {
		merged[k] = v
	}
	for k, v := range merged {
		if config.ReservedEnvKeys[k] {
			continue // validation already rejects these; never trust the merge
		}
		env = append(env, k+"="+v)
	}
	return env
}

// ---- runtime API handlers (called by the bootstrap shims) ----

func (p *pool) handleNext(w http.ResponseWriter, r *http.Request) {
	workerID := r.Header.Get("X-Pulse-Worker-Id")

	p.mu.Lock()
	wk := p.workers[workerID]
	genCh := p.genCh
	stale := wk == nil || wk.gen != p.gen
	if wk != nil && !stale && p.initFails != 0 {
		// A worker reached the poll loop, so the module imported fine again.
		p.initFails = 0
		p.lastInitErr = nil
	}
	p.mu.Unlock()

	if stale {
		w.WriteHeader(http.StatusGone)
		return
	}

	select {
	case inv := <-p.pending:
		p.dispatch(w, wk, inv)
	case <-genCh:
		w.WriteHeader(http.StatusGone) // retired by hot reload
	case <-r.Context().Done():
		// Worker died or engine is closing; nothing to say.
	}
}

func (p *pool) dispatch(w http.ResponseWriter, wk *worker, inv *Invocation) {
	timeout := time.Duration(p.fn.Timeout) * time.Second
	deadline := time.Now().Add(timeout)

	fl := &flight{inv: inv, w: wk, dispatchedAt: time.Now()}
	fl.timer = time.AfterFunc(timeout+timeoutGrace, func() { p.timeoutFlight(inv.ID) })

	p.mu.Lock()
	p.inflight[inv.ID] = fl
	p.mu.Unlock()
	wk.current.Store(inv)

	h := w.Header()
	h.Set("Lambda-Runtime-Aws-Request-Id", inv.ID)
	h.Set("Lambda-Runtime-Deadline-Ms", strconv.FormatInt(deadline.UnixMilli(), 10))
	h.Set("Lambda-Runtime-Invoked-Function-Arn", p.arn)
	h.Set("Content-Type", "application/json")
	_, _ = w.Write(inv.Payload)
}

func (p *pool) handleResponse(w http.ResponseWriter, r *http.Request) {
	p.finishFlight(w, r, "success")
}

func (p *pool) handleError(w http.ResponseWriter, r *http.Request) {
	p.finishFlight(w, r, "error")
}

func (p *pool) finishFlight(w http.ResponseWriter, r *http.Request, status string) {
	id := r.PathValue("id")
	body, _ := io.ReadAll(io.LimitReader(r.Body, 10<<20))

	fl := p.takeFlight(id)
	w.WriteHeader(http.StatusAccepted)
	if fl == nil {
		return // lost the race against the timeout killer; result discarded
	}
	fl.timer.Stop()
	fl.w.current.Store(nil)
	fl.inv.complete(&Result{
		Status:     status,
		Payload:    body,
		DurationMs: time.Since(fl.dispatchedAt).Milliseconds(),
	})
}

func (p *pool) handleInitError(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if len(body) == 0 {
		body = errDoc("function failed to initialize", "Runtime.InitError")
	}

	p.mu.Lock()
	p.initFails++
	p.lastInitErr = body
	p.mu.Unlock()

	// Everything queued would hit the same wall — fail it now, loudly.
	for {
		select {
		case inv := <-p.pending:
			inv.complete(&Result{Status: "error", Payload: body})
		default:
			w.WriteHeader(http.StatusAccepted)
			return
		}
	}
}

func (p *pool) timeoutFlight(id string) {
	fl := p.takeFlight(id)
	if fl == nil {
		return
	}
	fl.w.current.Store(nil)
	fl.w.kill() // the process is wedged past its deadline; a fresh worker will spawn on demand
	p.sink.System(p.fn.Name, id, fmt.Sprintf("task timed out after %d seconds", p.fn.Timeout), time.Now().UnixMilli())
	fl.inv.complete(&Result{
		Status:     "timeout",
		Payload:    errDoc(fmt.Sprintf("Task timed out after %d.00 seconds", p.fn.Timeout), "Sandbox.Timedout"),
		DurationMs: time.Since(fl.dispatchedAt).Milliseconds(),
	})
}

func (p *pool) takeFlight(id string) *flight {
	p.mu.Lock()
	defer p.mu.Unlock()
	fl := p.inflight[id]
	delete(p.inflight, id)
	return fl
}

func (p *pool) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// onWorkerExit removes a dead worker and fails whatever it was executing
// (unless the timeout killer already resolved it).
func (p *pool) onWorkerExit(w *worker, err error) {
	p.mu.Lock()
	delete(p.workers, w.id)
	closed := p.closed
	p.mu.Unlock()

	if inv := w.current.Load(); inv != nil {
		w.current.Store(nil)
		if fl := p.takeFlight(inv.ID); fl != nil {
			fl.timer.Stop()
			msg := "runtime exited unexpectedly while handling the request"
			if err != nil {
				msg = fmt.Sprintf("runtime exited unexpectedly: %v", err)
			}
			fl.inv.complete(&Result{
				Status:     "error",
				Payload:    errDoc(msg, "Runtime.ExitError"),
				DurationMs: time.Since(fl.dispatchedAt).Milliseconds(),
			})
		}
	}
	if !closed {
		p.ensureCapacity() // crash recovery: respawn if work is still queued
	}
}

// reload retires the current worker generation. Idle workers get 410 on
// their parked poll and exit; busy ones finish their invocation, poll, get
// 410, and exit. Fresh workers spawn lazily with the new code.
func (p *pool) reload() {
	p.mu.Lock()
	p.gen++
	close(p.genCh)
	p.genCh = make(chan struct{})
	p.initFails = 0
	p.lastInitErr = nil
	hasPending := len(p.pending) > 0
	p.mu.Unlock()
	if hasPending {
		p.ensureCapacity()
	}
}

func (p *pool) close() {
	p.mu.Lock()
	p.closed = true
	snapshot := make([]*worker, 0, len(p.workers))
	for _, w := range p.workers {
		snapshot = append(snapshot, w)
	}
	p.mu.Unlock()

	_ = p.srv.Close()
	for _, w := range snapshot {
		w.kill()
	}
	for {
		select {
		case inv := <-p.pending:
			inv.complete(&Result{Status: "error", Payload: errDoc("engine shut down before the invocation ran", "Pulse.Shutdown")})
		default:
			return
		}
	}
}
