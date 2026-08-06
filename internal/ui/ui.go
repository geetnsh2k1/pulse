// Package ui is pulse's terminal styling: a zero-dependency set of semantic
// text styles with amber as the brand accent.
//
// Styling turns itself off (returning input unchanged) when stdout is not a
// terminal, when NO_COLOR is set (https://no-color.org), when TERM=dumb, or
// when the user passes --no-color — so pipes, scripts, and CI always see
// today's plain text.
package ui

import (
	"fmt"
	"hash/fnv"
	"os"
	"strings"

	"golang.org/x/term"
)

var (
	enabled  = detect()
	use256   = detect256()
	disabled bool // --no-color
)

// Disable turns all styling off for this process (the --no-color flag).
func Disable() { disabled = true }

// Enabled reports whether styles are currently applied.
func Enabled() bool { return enabled && !disabled }

func detect() bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	// pulse tour pipes a child engine's output to a real terminal — the
	// parent sets this so the child's colors survive the pipe.
	if os.Getenv("PULSE_FORCE_COLOR") == "1" {
		return true
	}
	return term.IsTerminal(int(os.Stdout.Fd()))
}

func detect256() bool {
	return strings.Contains(os.Getenv("TERM"), "256color") || os.Getenv("COLORTERM") != ""
}

func wrap(code, s string) string {
	if !Enabled() || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// Accent is pulse's brand color: amber. True amber on 256-color terminals,
// yellow elsewhere.
func Accent(s string) string {
	if use256 {
		return wrap("38;5;214", s)
	}
	return wrap("33", s)
}

// AccentBold is the accent for headings and the wordmark.
func AccentBold(s string) string {
	if use256 {
		return wrap("1;38;5;214", s)
	}
	return wrap("1;33", s)
}

func OK(s string) string   { return wrap("32", s) } // green
func Err(s string) string  { return wrap("31", s) } // red
func Warn(s string) string { return wrap("33", s) } // yellow
func Cyan(s string) string { return wrap("36", s) } // queue machinery
func Bold(s string) string { return wrap("1", s) }  // names the user chose
func Dim(s string) string  { return wrap("2", s) }  // metadata, hints

// Hint styles a "what to do next" line: dim, with `backtick` spans in
// accent so the command to type stands out even inside quiet text.
func Hint(s string) string {
	if !Enabled() {
		return s
	}
	var b strings.Builder
	parts := strings.Split(s, "`")
	for i, p := range parts {
		if i%2 == 1 && i < len(parts) { // inside backticks
			b.WriteString(Accent(p))
		} else {
			b.WriteString(Dim(p))
		}
	}
	return b.String()
}

// Commands highlights `backtick` spans in accent, leaving the rest as-is —
// for error messages whose fix is a command.
func Commands(s string) string {
	if !Enabled() || !strings.Contains(s, "`") {
		return s
	}
	var b strings.Builder
	parts := strings.Split(s, "`")
	for i, p := range parts {
		if i%2 == 1 {
			b.WriteString(Accent(p))
		} else {
			b.WriteString(p)
		}
	}
	return b.String()
}

// fnPalette holds visually distinct, readable-on-dark-and-light colors for
// per-function log prefixes (docker-compose style).
var fnPalette = []string{"36", "35", "34", "32", "33", "96", "95", "94", "92", "93"}

// Fn colors a function name deterministically — the same function always
// gets the same color within and across runs.
func Fn(name string) string {
	if !Enabled() {
		return name
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return wrap(fnPalette[h.Sum32()%uint32(len(fnPalette))], name)
}

// Status colors an HTTP status code by class: 2xx green, 3xx cyan,
// 4xx yellow, 5xx red.
func Status(code string) string {
	switch {
	case strings.HasPrefix(code, "2"):
		return OK(code)
	case strings.HasPrefix(code, "3"):
		return Cyan(code)
	case strings.HasPrefix(code, "4"):
		return Warn(code)
	case strings.HasPrefix(code, "5"):
		return Err(code)
	}
	return code
}

// Errorf formats an error for the terminal: red ✗, message with any
// `commands` highlighted.
func Errorf(err error) string {
	return fmt.Sprintf("%s %s", Err("✗"), Commands(err.Error()))
}
