package config

import (
	"strconv"
	"strings"
)

// Runtime support is a FLOOR, not a list of blessed versions.
//
// This was learned the hard way twice. `pulse doctor` used to compare the
// local interpreter against a hardcoded "certified" list and flagged Python
// 3.13 — which CI tests — as uncertified; that became a floor comparison.
// The same stale list survived here, in what pulse.yaml may declare and what
// `pulse import aws` will accept, and it refused a real deployed function
// running python3.14 the day AWS started offering it.
//
// A hardcoded list means every AWS runtime release makes pulse wrong until
// someone edits a slice and ships a binary. The README has promised
// "Python 3.10+ and Node 18+" from the start; this makes the code agree.
const (
	MinNodeMajor   = 18
	MinPythonMajor = 3
	MinPythonMinor = 10
)

// RuntimeFloor is the promise, in the words the docs use.
const RuntimeFloor = "Node 18+ and Python 3.10+"

// SupportedRuntimes are the runtimes CI actually exercises (scripts/e2e.sh's
// matrix). They are the suggestions offered on a typo and the examples in
// error messages — NOT the gate. Use SupportsRuntime for that.
var SupportedRuntimes = []string{
	"nodejs18.x", "nodejs20.x", "nodejs22.x",
	"python3.10", "python3.11", "python3.12", "python3.13",
}

// SupportsRuntime reports whether pulse will run a Lambda runtime identifier.
// A recognized family at or above the floor is supported — including versions
// newer than this build has heard of, because a runtime AWS ships tomorrow
// honors the same handler contract as the one it shipped last year.
func SupportsRuntime(runtime string) bool {
	family, maj, min, ok := parseRuntime(runtime)
	if !ok {
		return false
	}
	switch family {
	case "nodejs":
		return maj >= MinNodeMajor
	case "python":
		return maj > MinPythonMajor || (maj == MinPythonMajor && min >= MinPythonMinor)
	}
	return false
}

// RuntimeNewerThanTested reports a runtime pulse accepts but has never run in
// CI. It should work; saying so out loud is cheaper than being wrong quietly.
func RuntimeNewerThanTested(runtime string) bool {
	if !SupportsRuntime(runtime) {
		return false
	}
	for _, known := range SupportedRuntimes {
		if known == runtime {
			return false
		}
	}
	return true
}

// parseRuntime splits a Lambda runtime identifier into family and version:
// "python3.12" → python 3.12, "nodejs20.x" → nodejs 20.0. Everything else —
// java17, dotnet8, ruby3.2, go1.x, provided.al2023 — is a family pulse does
// not run, and reports ok=false rather than guessing.
func parseRuntime(runtime string) (family string, maj, min int, ok bool) {
	for _, f := range []string{"nodejs", "python"} {
		if !strings.HasPrefix(runtime, f) {
			continue
		}
		version := strings.TrimSuffix(strings.TrimPrefix(runtime, f), ".x")
		if version == "" {
			return "", 0, 0, false
		}
		parts := strings.SplitN(version, ".", 2)
		maj, err := strconv.Atoi(parts[0])
		if err != nil {
			return "", 0, 0, false
		}
		if len(parts) == 2 {
			// A trailing non-numeric segment (an alN suffix, say) makes the
			// identifier one we don't understand; better to refuse than to run
			// something unexpected.
			if min, err = strconv.Atoi(parts[1]); err != nil {
				return "", 0, 0, false
			}
		}
		return f, maj, min, true
	}
	return "", 0, 0, false
}
