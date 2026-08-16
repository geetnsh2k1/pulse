// Package logs is the engine's log pipeline: every line a worker writes is
// persisted to the store, fanned out to live subscribers (SSE, the CLI's
// --follow), and — while an invocation is in flight — collected so `pulse
// invoke` can hand the caller exactly the lines their request produced.
package logs

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/geetnsh2k1/pulse/internal/store"
)

// Line is one log line; the wire shape is shared with the store.
type Line = store.LogRow

type Sink struct {
	st *store.Store

	mu       sync.Mutex
	subs     map[int]*subscriber
	nextSub  int
	collects map[string]*collection // keyed by request id, while collecting
}

// collection buffers one invocation's output and knows when that output is
// complete. Completeness is not a guess: each runtime shim writes an
// end-of-request marker to stdout and stderr after the handler returns, so
// seeing both markers proves every earlier line has already been read.
//
// Before this existed the manager slept 15ms and hoped, which cost every
// invocation 15ms (against a warm-invoke budget of ~17ms) and still lost the
// race on a loaded CI runner.
type collection struct {
	lines   []Line
	pending int // markers still expected: stdout and stderr
	done    chan struct{}
}

type subscriber struct {
	function string // "" = all functions
	ch       chan Line
}

func NewSink(st *store.Store) *Sink {
	return &Sink{
		st:       st,
		subs:     map[int]*subscriber{},
		collects: map[string]*collection{},
	}
}

// Write persists and fans out one line. Slow subscribers drop lines rather
// than block the worker pipeline.
func (s *Sink) Write(l Line) {
	if err := s.st.InsertLog(l); err != nil {
		fmt.Fprintf(os.Stderr, "pulse: writing log line: %v\n", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if l.RequestID != "" {
		if c, ok := s.collects[l.RequestID]; ok {
			c.lines = append(c.lines, l)
		}
	}
	for _, sub := range s.subs {
		if sub.function != "" && sub.function != l.Function {
			continue
		}
		select {
		case sub.ch <- l:
		default:
		}
	}
}

// System records an engine-generated line attached to a function.
func (s *Sink) System(function, requestID, text string, ts int64) {
	s.Write(Line{Function: function, RequestID: requestID, Stream: "system", TS: ts, Text: text})
}

// Subscribe returns a live feed (function "" = everything) and a cancel func.
func (s *Sink) Subscribe(function string) (<-chan Line, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextSub
	s.nextSub++
	sub := &subscriber{function: function, ch: make(chan Line, 256)}
	s.subs[id] = sub
	return sub.ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.subs, id)
	}
}

// StartCollect begins buffering lines for a request id.
func (s *Sink) StartCollect(requestID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.collects[requestID] = &collection{pending: 2, done: make(chan struct{})}
}

// StreamComplete records that one of a worker's two streams has emitted
// everything this request produced. Called by the scanner when it reads the
// shim's marker — which it never forwards as a log line.
func (s *Sink) StreamComplete(requestID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.collects[requestID]
	if !ok {
		return
	}
	if c.pending--; c.pending <= 0 {
		select {
		case <-c.done:
		default:
			close(c.done)
		}
	}
}

// WaitCollect returns the request's lines once both streams have signalled
// they are done, or when the deadline passes — a handler killed mid-run never
// writes its markers, and a missing log line must not hang an invocation.
func (s *Sink) WaitCollect(requestID string, timeout time.Duration) []Line {
	s.mu.Lock()
	c, ok := s.collects[requestID]
	s.mu.Unlock()
	if !ok {
		return nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-c.done:
	case <-timer.C:
	}
	return s.EndCollect(requestID)
}

// EndCollect stops buffering and returns everything captured for the request.
func (s *Sink) EndCollect(requestID string) []Line {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.collects[requestID]
	if !ok {
		return nil
	}
	delete(s.collects, requestID)
	return c.lines
}

// EndOfRequestMarker is written to stdout and stderr by every runtime shim
// once a handler returns, immediately before the response is posted. The
// leading control byte keeps it out of any plausible handler output, and the
// scanner swallows it rather than showing it to the user.
const EndOfRequestMarker = "\x01pulse:end-of-request:"

// IsEndOfRequest reports whether a scanned line is a shim marker, and for
// which request.
func IsEndOfRequest(line string) (requestID string, ok bool) {
	if !strings.HasPrefix(line, EndOfRequestMarker) {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(line, EndOfRequestMarker)), true
}
