// Package logs is the engine's log pipeline: every line a worker writes is
// persisted to the store, fanned out to live subscribers (SSE, the CLI's
// --follow), and — while an invocation is in flight — collected so `pulse
// invoke` can hand the caller exactly the lines their request produced.
package logs

import (
	"fmt"
	"os"
	"sync"

	"github.com/geetnsh2k1/pulse/internal/store"
)

// Line is one log line; the wire shape is shared with the store.
type Line = store.LogRow

type Sink struct {
	st *store.Store

	mu       sync.Mutex
	subs     map[int]*subscriber
	nextSub  int
	collects map[string][]Line // keyed by request id, while collecting
}

type subscriber struct {
	function string // "" = all functions
	ch       chan Line
}

func NewSink(st *store.Store) *Sink {
	return &Sink{
		st:       st,
		subs:     map[int]*subscriber{},
		collects: map[string][]Line{},
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
		if buf, ok := s.collects[l.RequestID]; ok {
			s.collects[l.RequestID] = append(buf, l)
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
	s.collects[requestID] = []Line{}
}

// EndCollect stops buffering and returns everything captured for the request.
func (s *Sink) EndCollect(requestID string) []Line {
	s.mu.Lock()
	defer s.mu.Unlock()
	buf := s.collects[requestID]
	delete(s.collects, requestID)
	return buf
}
