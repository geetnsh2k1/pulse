package dotenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	in := strings.Join([]string{
		"# a comment",
		"",
		"PLAIN=value",
		"  SPACED  =  trimmed  ",
		`QUOTED="hello world"`,
		`ESCAPED="line1\nline2\ttabbed"`,
		`SINGLE='literal \n stays'`,
		"export EXPORTED=yes",
		"WITH_COMMENT=value # trailing",
		"HASH_IN_QUOTES=\"kept # here\"",
		"EMPTY=",
		"URL=postgres://user:pa55@host:5432/db?sslmode=require",
		"DUP=first",
		"DUP=second",
	}, "\n")

	got, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := map[string]string{
		"PLAIN":          "value",
		"SPACED":         "trimmed",
		"QUOTED":         "hello world",
		"ESCAPED":        "line1\nline2\ttabbed",
		"SINGLE":         `literal \n stays`,
		"EXPORTED":       "yes",
		"WITH_COMMENT":   "value",
		"HASH_IN_QUOTES": "kept # here",
		"EMPTY":          "",
		"URL":            "postgres://user:pa55@host:5432/db?sslmode=require",
		"DUP":            "second", // last wins, like a shell
	}
	if len(got) != len(want) {
		t.Fatalf("got %d vars, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name, in, wantIn string
	}{
		{"no equals", "JUST_A_NAME", "expected KEY=value"},
		{"missing name", "=orphan", "missing variable name"},
		{"bad char", "MY-KEY=1", "isn't a valid variable name"},
		{"leading digit", "1KEY=1", "isn't a valid variable name"},
		{"unterminated double", `K="open`, "unterminated double quote"},
		{"unterminated single", `K='open`, "unterminated single quote"},
		{"yaml style", "KEY: value", "expected KEY=value"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(c.in))
			if err == nil {
				t.Fatalf("expected an error for %q", c.in)
			}
			if !strings.Contains(err.Error(), c.wantIn) {
				t.Errorf("error %q should mention %q", err, c.wantIn)
			}
			if !strings.Contains(err.Error(), "line 1") {
				t.Errorf("error %q should name the line", err)
			}
		})
	}
}

func TestLoadMissingFileIsFine(t *testing.T) {
	got, err := Load(filepath.Join(t.TempDir(), "nope.env"))
	if err != nil {
		t.Fatalf("missing .env must not be an error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty map, got %v", got)
	}
}

func TestLoadNamesTheFileOnError(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".env")
	if err := os.WriteFile(p, []byte("GOOD=1\nBROKEN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(p)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), ".env") || !strings.Contains(err.Error(), "line 2") {
		t.Errorf("error should name file and line, got: %v", err)
	}
}
