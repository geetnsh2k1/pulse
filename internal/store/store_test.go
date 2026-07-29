package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenMigratesAndIsIdempotent(t *testing.T) {
	root := t.TempDir()

	s, err := Open(root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	for _, table := range []string{"invocations", "events", "logs", "kv", "schema_migrations"} {
		var name string
		err := s.DB().QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
		if err != nil {
			t.Errorf("table %s missing: %v", table, err)
		}
	}

	if err := s.SetKV("hello", "world"); err != nil {
		t.Fatalf("SetKV: %v", err)
	}
	if err := s.SetKV("hello", "again"); err != nil {
		t.Fatalf("SetKV upsert: %v", err)
	}
	v, ok, err := s.GetKV("hello")
	if err != nil || !ok || v != "again" {
		t.Errorf("GetKV = %q,%v,%v; want again,true,nil", v, ok, err)
	}
	if _, ok, _ := s.GetKV("absent"); ok {
		t.Error("GetKV(absent) reported ok")
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopening must not re-run migrations or lose data.
	s2, err := Open(root)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	v, ok, err = s2.GetKV("hello")
	if err != nil || !ok || v != "again" {
		t.Errorf("after reopen GetKV = %q,%v,%v", v, ok, err)
	}

	var count int
	if err := s2.DB().QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	names, err := migrationNames()
	if err != nil {
		t.Fatal(err)
	}
	if count != len(names) {
		t.Errorf("schema_migrations rows = %d, want %d", count, len(names))
	}

	if _, err := os.Stat(filepath.Join(root, Dir, "state.db")); err != nil {
		t.Errorf("state.db not where expected: %v", err)
	}
}
