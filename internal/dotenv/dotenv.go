// Package dotenv reads .env files: the place local secrets live so they
// never end up in pulse.yaml (which is committed). Deliberately small and
// strict — a malformed line is an error naming the line number, not a
// silently skipped variable that leaves you debugging a missing value.
//
// Supported, matching what people already expect from .env files:
//
//	KEY=value            # inline comments after unquoted values
//	KEY="value"          # double quotes: \n \r \t \\ \" are unescaped
//	KEY='value'          # single quotes: taken literally
//	export KEY=value     # the export prefix is tolerated
//	# whole-line comments and blank lines are ignored
//
// Not supported (on purpose, so behavior is never surprising): variable
// expansion (${OTHER}), multi-line values, and YAML-style `KEY: value`.
package dotenv

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// Parse reads KEY=VALUE pairs. Later duplicates win, mirroring shells.
func Parse(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) // tolerate long values
	line := 0
	for sc.Scan() {
		line++
		raw := strings.TrimPrefix(sc.Text(), "\ufeff") // strip a UTF-8 BOM
		s := strings.TrimSpace(raw)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		s = strings.TrimPrefix(s, "export ")

		key, val, ok := strings.Cut(s, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected KEY=value, got %q", line, trunc(s))
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("line %d: missing variable name before '='", line)
		}
		if err := validKey(key, line); err != nil {
			return nil, err
		}

		v, err := parseValue(strings.TrimSpace(val), line)
		if err != nil {
			return nil, err
		}
		out[key] = v
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// Load reads a .env file. A missing file is not an error — .env is
// optional by design — so callers get an empty map and carry on.
func Load(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	defer f.Close()
	vars, err := Parse(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return vars, nil
}

func parseValue(v string, line int) (string, error) {
	if v == "" {
		return "", nil
	}
	switch v[0] {
	case '"':
		body, ok := closeQuote(v, '"')
		if !ok {
			return "", fmt.Errorf("line %d: unterminated double quote", line)
		}
		return unescape(body), nil
	case '\'':
		body, ok := closeQuote(v, '\'')
		if !ok {
			return "", fmt.Errorf("line %d: unterminated single quote", line)
		}
		return body, nil // single quotes are literal, like in a shell
	}
	// Unquoted: an unescaped # starts a comment.
	if i := strings.Index(v, " #"); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v), nil
}

// closeQuote returns the text between the leading quote and its match.
func closeQuote(v string, q byte) (string, bool) {
	for i := 1; i < len(v); i++ {
		if v[i] == '\\' { // skip the escaped char
			i++
			continue
		}
		if v[i] == q {
			return v[1:i], true
		}
	}
	return "", false
}

func unescape(s string) string {
	r := strings.NewReplacer(`\n`, "\n", `\r`, "\r", `\t`, "\t", `\"`, `"`, `\\`, `\`)
	return r.Replace(s)
}

func validKey(k string, line int) error {
	for i, c := range k {
		ok := c == '_' ||
			(c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9' && i > 0)
		if !ok {
			return fmt.Errorf("line %d: %q isn't a valid variable name (letters, digits, underscore; not starting with a digit)", line, k)
		}
	}
	return nil
}

func trunc(s string) string {
	if len(s) > 40 {
		return s[:40] + "…"
	}
	return s
}
