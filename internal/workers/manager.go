// Package workers runs Lambda functions locally. The engine implements the
// real AWS Lambda runtime interface; tiny bootstrap shims (Node, Python)
// long-poll it exactly like AWS's own runtime clients, so fidelity comes
// from the contract itself rather than emulation glue.
package workers

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"pulse/internal/config"
	"pulse/internal/logs"
	"pulse/internal/store"
)

//go:embed shims/*
var shimsFS embed.FS

type Manager struct {
	cfg         *config.Config
	st          *store.Store
	sink        *logs.Sink
	pools       map[string]*pool
	warnings    []string
	awsEndpoint string
}

func NewManager(cfg *config.Config, st *store.Store, sink *logs.Sink) *Manager {
	return &Manager{cfg: cfg, st: st, sink: sink, pools: map[string]*pool{}}
}

// SetAWSEndpoint points workers' AWS SDKs at the local façade (must be
// called before Start). Empty means no injection.
func (m *Manager) SetAWSEndpoint(url string) { m.awsEndpoint = url }

// Start materializes the bootstrap shims into .pulse/runtime/shims and
// brings up one runtime-API listener per function. Worker processes spawn
// lazily on first invoke.
func (m *Manager) Start() error {
	shimDir := filepath.Join(m.cfg.Root, store.Dir, "runtime", "shims")
	if err := m.materializeShims(shimDir); err != nil {
		return fmt.Errorf("writing runtime shims: %w", err)
	}

	seenWarnings := map[string]bool{}
	for _, name := range m.cfg.FunctionNames() {
		p := newPool(m.cfg.Functions[name], m.cfg, m.sink, shimDir)
		p.awsEndpoint = m.awsEndpoint
		if err := p.start(); err != nil {
			m.Shutdown()
			return err
		}
		if p.rbErr != nil {
			m.warnings = append(m.warnings, fmt.Sprintf("%s: %v (invocations will fail until fixed)", name, p.rbErr))
		}
		for _, note := range p.rbNotes {
			if !seenWarnings[note] {
				seenWarnings[note] = true
				m.warnings = append(m.warnings, note)
			}
		}
		m.pools[name] = p
	}
	return nil
}

func (m *Manager) materializeShims(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	entries, err := fs.ReadDir(shimsFS, "shims")
	if err != nil {
		return err
	}
	for _, e := range entries {
		data, err := fs.ReadFile(shimsFS, "shims/"+e.Name())
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// Warnings reports runtime-resolution issues (missing binaries, version
// mismatches) collected at start.
func (m *Manager) Warnings() []string { return m.warnings }

// Invoke runs one function synchronously and returns its result, including
// exactly the log lines that request produced. Every invocation is recorded
// in the store (and its event payload kept for phase-5 replay).
func (m *Manager) Invoke(ctx context.Context, function, source string, payload []byte) (*Result, error) {
	return m.InvokeAs(ctx, uuid.NewString(), function, source, payload)
}

// InvokeAs is Invoke with a caller-chosen request id, so trigger frontends
// (the HTTP gateway, later the SQS poller) can stamp the same id into the
// event they build and into the invocation record.
func (m *Manager) InvokeAs(ctx context.Context, id, function, source string, payload []byte) (*Result, error) {
	p, ok := m.pools[function]
	if !ok {
		known := strings.Join(m.cfg.FunctionNames(), ", ")
		return nil, fmt.Errorf("unknown function %q (functions: %s)", function, known)
	}
	if len(payload) == 0 {
		payload = []byte("{}")
	}

	now := time.Now()
	_ = m.st.StartInvocation(id, function, source, payload, now.UnixMilli())
	_ = m.st.RecordEvent(id, source, source, function, payload, now.UnixMilli())
	m.sink.StartCollect(id)

	inv := newInvocation(id, function, source, payload)
	safety := time.Duration(p.fn.Timeout)*time.Second + 10*time.Second
	ictx, cancel := context.WithTimeout(ctx, safety)
	defer cancel()

	res, err := p.invoke(ictx, inv)
	if err != nil {
		m.sink.EndCollect(id)
		_ = m.st.CompleteInvocation(id, "error", nil, err.Error(), time.Now().UnixMilli(), 0)
		return nil, err
	}

	// Give trailing stdout a beat to flow through the pipes before snapshotting.
	time.Sleep(15 * time.Millisecond)
	res.Logs = m.sink.EndCollect(id)

	var resultPayload []byte
	errMsg := ""
	if res.Status == "success" {
		resultPayload = res.Payload
	} else {
		errMsg = res.ErrorMessage()
	}
	_ = m.st.CompleteInvocation(id, res.Status, resultPayload, errMsg, time.Now().UnixMilli(), res.DurationMs)
	return res, nil
}

// Reload retires the workers of the given functions so their next
// invocation runs freshly-loaded code.
func (m *Manager) Reload(functions []string) {
	sort.Strings(functions)
	for _, fn := range functions {
		if p, ok := m.pools[fn]; ok {
			p.reload()
			m.sink.System(fn, "", "hot reload: workers retired, next invoke runs fresh code", time.Now().UnixMilli())
		}
	}
}

// Shutdown stops listeners and kills every worker process.
func (m *Manager) Shutdown() {
	for _, p := range m.pools {
		p.close()
	}
}
