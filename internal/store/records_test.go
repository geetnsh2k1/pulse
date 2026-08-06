package store

import (
	"testing"
)

func TestInvocationLifecycleAndLogs(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.StartInvocation("inv-1", "api", "manual", []byte(`{"a":1}`), 1000); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteInvocation("inv-1", "success", []byte(`{"ok":true}`), "", 1042, 42); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordEvent("inv-1", "manual", "manual", "api", []byte(`{"a":1}`), 1000); err != nil {
		t.Fatal(err)
	}

	rows, err := s.RecentInvocations("api", 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("invocations = %+v (%v)", rows, err)
	}
	r := rows[0]
	if r.ID != "inv-1" || r.Status != "success" || r.DurationMs != 42 || r.Error != "" {
		t.Errorf("row = %+v", r)
	}

	for i, text := range []string{"one", "two", "three"} {
		if err := s.InsertLog(LogRow{Function: "api", Stream: "stdout", TS: int64(i), Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	logs, err := s.RecentLogs("api", 2)
	if err != nil {
		t.Fatal(err)
	}
	// Newest two, oldest-first: "two", "three".
	if len(logs) != 2 || logs[0].Text != "two" || logs[1].Text != "three" {
		t.Errorf("logs = %+v", logs)
	}
}

func TestRecentEventsAndPrefixLookup(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	seed := []struct {
		id, typ, fn string
		ts          int64
	}{
		{"aaaa1111-x", "http", "getOrder", 1000},
		{"aaaa2222-x", "sqs", "worker", 2000},
		{"bbbb3333-x", "http", "getOrder", 3000},
	}
	for _, e := range seed {
		if err := s.RecordEvent(e.id, e.typ, e.typ, e.fn, []byte(`{"n":1}`), e.ts); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.StartInvocation("aaaa2222-x", "worker", "sqs", []byte(`{}`), 2000)
	_ = s.CompleteInvocation("aaaa2222-x", "error", nil, "boom", 2100, 100)

	rows, err := s.RecentEvents("", 10)
	if err != nil || len(rows) != 3 {
		t.Fatalf("RecentEvents = %d rows (%v)", len(rows), err)
	}
	if rows[0].ID != "bbbb3333-x" {
		t.Errorf("want newest first, got %q", rows[0].ID)
	}
	if rows[1].Status != "error" || rows[1].DurationMs != 100 {
		t.Errorf("join missed invocation outcome: %+v", rows[1])
	}
	if len(rows[0].Payload) != 0 {
		t.Error("list view must omit payloads")
	}

	only, err := s.RecentEvents("worker", 10)
	if err != nil || len(only) != 1 || only[0].Function != "worker" {
		t.Fatalf("function filter = %+v (%v)", only, err)
	}

	ev, err := s.EventByPrefix("bbbb")
	if err != nil || ev.ID != "bbbb3333-x" || string(ev.Payload) != `{"n":1}` {
		t.Fatalf("EventByPrefix = %+v (%v)", ev, err)
	}
	if _, err := s.EventByPrefix("aaaa"); err == nil {
		t.Error("ambiguous prefix must error")
	}
	if _, err := s.EventByPrefix("zzzz"); err == nil {
		t.Error("missing prefix must error")
	}
}
