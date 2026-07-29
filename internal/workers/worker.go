package workers

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"sync/atomic"
	"time"

	"pulse/internal/logs"
)

// worker wraps one long-lived runtime process (a bootstrap shim running the
// user's handler). Workers of the same function are fungible: any of them
// may take any pending invocation by long-polling the pool's runtime API.
type worker struct {
	id      string
	gen     int
	cmd     *exec.Cmd
	current atomic.Pointer[Invocation] // what it's executing right now, if anything

	// proc is published after cmd.Start() succeeds so that other goroutines
	// (timeout killer, pool shutdown) can signal it without racing Start.
	proc atomic.Pointer[os.Process]
}

// start launches the process and blocks until it exits; run in a goroutine.
func (w *worker) start(p *pool) {
	stdout, err := w.cmd.StdoutPipe()
	if err != nil {
		p.onWorkerExit(w, err)
		return
	}
	stderr, err := w.cmd.StderrPipe()
	if err != nil {
		p.onWorkerExit(w, err)
		return
	}
	if err := w.cmd.Start(); err != nil {
		p.onWorkerExit(w, err)
		return
	}
	w.proc.Store(w.cmd.Process)
	if p.isClosed() {
		w.kill() // pool shut down while we were starting
	}
	go w.scan(p, stdout, "stdout")
	go w.scan(p, stderr, "stderr")

	err = w.cmd.Wait()
	p.onWorkerExit(w, err)
}

// scan forwards one output stream line-by-line into the log sink, tagged
// with whichever invocation this worker is executing when the line arrives.
func (w *worker) scan(p *pool, r io.Reader, stream string) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		requestID := ""
		if inv := w.current.Load(); inv != nil {
			requestID = inv.ID
		}
		p.sink.Write(logs.Line{
			Function:  p.fn.Name,
			RequestID: requestID,
			Stream:    stream,
			TS:        time.Now().UnixMilli(),
			Text:      sc.Text(),
		})
	}
}

func (w *worker) kill() {
	if proc := w.proc.Load(); proc != nil {
		_ = proc.Kill()
	}
}
