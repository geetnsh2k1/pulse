package logs

import (
	"testing"
	"time"

	"github.com/geetnsh2k1/pulse/internal/store"
)

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
