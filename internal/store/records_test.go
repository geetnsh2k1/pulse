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
