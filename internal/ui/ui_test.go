package ui

import (
	"strings"
	"testing"
)

// Tests run with stdout not a TTY, so styles must be off by default and
// every helper must return its input unchanged.
func TestPlainWhenNotATerminal(t *testing.T) {
	for name, fn := range map[string]func(string) string{
		"Accent": Accent, "AccentBold": AccentBold, "OK": OK, "Err": Err,
		"Warn": Warn, "Cyan": Cyan, "Bold": Bold, "Dim": Dim,
		"Hint": Hint, "Commands": Commands, "Fn": Fn, "Status": Status,
	} {
		if got := fn("hello `world`"); got != "hello `world`" {
			t.Errorf("%s should pass through when disabled, got %q", name, got)
		}
	}
}

func TestStyledWhenForced(t *testing.T) {
	prev := enabled
	enabled = true
	defer func() { enabled = prev }()

	if got := OK("done"); !strings.Contains(got, "\x1b[32m") || !strings.Contains(got, "done") {
		t.Errorf("OK = %q", got)
	}
	if Fn("worker") != Fn("worker") {
		t.Error("Fn must be deterministic")
	}
	if got := Hint("try `pulse start` now"); !strings.Contains(got, "pulse start") {
		t.Errorf("Hint lost content: %q", got)
	}
	if strings.Contains(Hint("try `pulse start` now"), "`") {
		t.Error("Hint should consume the backticks")
	}
	if got := Status("404"); !strings.Contains(got, "\x1b[33m") {
		t.Errorf("Status 404 should be yellow, got %q", got)
	}

	Disable()
	defer func() { disabled = false }()
	if OK("x") != "x" {
		t.Error("Disable() must force plain output")
	}
}
