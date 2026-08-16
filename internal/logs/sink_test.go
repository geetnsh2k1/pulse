package logs

import (
	"testing"
	"time"

	"github.com/geetnsh2k1/pulse/internal/store"
)

// newTestSink is a sink backed by a throwaway store.
func newTestSink(t *testing.T) *Sink {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return NewSink(st)
}

func TestSinkPersistsFansOutAndCollects(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := NewSink(st)

	chA, cancelA := s.Subscribe("fnA")
	defer cancelA()
	chAll, cancelAll := s.Subscribe("")
	defer cancelAll()

	s.StartCollect("req-1")
	now := time.Now().UnixMilli()
	s.Write(Line{Function: "fnA", RequestID: "req-1", Stream: "stdout", TS: now, Text: "hello"})
	s.Write(Line{Function: "fnB", RequestID: "req-2", Stream: "stderr", TS: now, Text: "other"})

	got := s.EndCollect("req-1")
	if len(got) != 1 || got[0].Text != "hello" {
		t.Errorf("collected = %+v", got)
	}
	if extra := s.EndCollect("req-1"); len(extra) != 0 {
		t.Errorf("second EndCollect returned %+v", extra)
	}

	select {
	case l := <-chA:
		if l.Function != "fnA" {
			t.Errorf("fnA subscriber got %+v", l)
		}
	default:
		t.Error("fnA subscriber got nothing")
	}
	if len(chA) != 0 {
		t.Error("fnA subscriber received a foreign function's line")
	}
	if len(chAll) != 2 {
		t.Errorf("all-subscriber buffered %d lines, want 2", len(chAll))
	}

	rows, err := st.RecentLogs("", 10)
	if err != nil || len(rows) != 2 {
		t.Errorf("stored rows = %d (%v)", len(rows), err)
	}
}

// The manager used to sleep 15ms and hope the pipes had drained. Completion is
// now a signal: both streams mark the end of a request, and only then are the
// lines handed over.
func TestWaitCollectWaitsForBothStreams(t *testing.T) {
	s := newTestSink(t)
	s.StartCollect("req-1")

	s.Write(Line{RequestID: "req-1", Function: "f", Stream: "stdout", Text: "hello"})

	// One stream done is not enough — stderr may still be in flight.
	s.StreamComplete("req-1")
	done := make(chan []Line, 1)
	go func() { done <- s.WaitCollect("req-1", 2*time.Second) }()
	select {
	case <-done:
		t.Fatal("returned after only one stream signalled")
	case <-time.After(60 * time.Millisecond):
	}

	// A line that arrives late still lands, which is the whole point.
	s.Write(Line{RequestID: "req-1", Function: "f", Stream: "stderr", Text: "late line"})
	s.StreamComplete("req-1")

	select {
	case lines := <-done:
		if len(lines) != 2 {
			t.Fatalf("lines = %+v, want both", lines)
		}
		if lines[1].Text != "late line" {
			t.Errorf("the late line was lost: %+v", lines)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitCollect never returned after both streams signalled")
	}
}

// A handler killed mid-run never writes its markers; that must not hang the
// invocation.
func TestWaitCollectGivesUpOnADeadHandler(t *testing.T) {
	s := newTestSink(t)
	s.StartCollect("req-2")
	s.Write(Line{RequestID: "req-2", Function: "f", Stream: "stdout", Text: "partial"})

	start := time.Now()
	lines := s.WaitCollect("req-2", 80*time.Millisecond)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("waited %s — the timeout must bound it", elapsed)
	}
	if len(lines) != 1 || lines[0].Text != "partial" {
		t.Errorf("whatever was captured must still come back, got %+v", lines)
	}
}

// The marker is a signal, never output.
func TestEndOfRequestMarkerParsing(t *testing.T) {
	id, ok := IsEndOfRequest(EndOfRequestMarker + "abc-123")
	if !ok || id != "abc-123" {
		t.Errorf("IsEndOfRequest = %q, %v", id, ok)
	}
	for _, notMarker := range []string{"hello", "pulse:end-of-request:x", ""} {
		if _, ok := IsEndOfRequest(notMarker); ok {
			t.Errorf("%q must not be treated as a marker", notMarker)
		}
	}
}
