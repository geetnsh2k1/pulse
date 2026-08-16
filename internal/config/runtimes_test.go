package config

import "testing"

// A hardcoded runtime list makes pulse wrong every time AWS ships a runtime.
// It refused a real deployed python3.14 function on the day it was tested,
// while the README promised "Python 3.10+" — this is the code agreeing with
// the docs.
func TestSupportsRuntimeIsAFloorNotAList(t *testing.T) {
	supported := []string{
		"python3.10", "python3.11", "python3.12", "python3.13",
		"python3.14",              // the one that started this: newer than we knew about
		"python3.20", "python4.0", // whatever comes next
		"nodejs18.x", "nodejs20.x", "nodejs22.x", "nodejs24.x", "nodejs99.x",
	}
	for _, r := range supported {
		if !SupportsRuntime(r) {
			t.Errorf("SupportsRuntime(%q) = false, want true — it is at or above the floor", r)
		}
	}

	refused := []string{
		"python3.9", "python2.7", "nodejs16.x", "nodejs12.x", // below the floor
		"java17", "dotnet8", "ruby3.2", "go1.x", "provided.al2023", // other families
		"python", "nodejs", "python3.x", "nodejs.x", "", "banana", // malformed
	}
	for _, r := range refused {
		if SupportsRuntime(r) {
			t.Errorf("SupportsRuntime(%q) = true, want false", r)
		}
	}
}

// Accepted-but-untested is a real category and has to be reportable, or pulse
// would be quietly assuming.
func TestRuntimeNewerThanTested(t *testing.T) {
	if !RuntimeNewerThanTested("python3.14") {
		t.Error("python3.14 is above the floor and not in the CI matrix — it should be flagged")
	}
	for _, tested := range SupportedRuntimes {
		if RuntimeNewerThanTested(tested) {
			t.Errorf("%q is in the CI matrix, it must not be flagged as untested", tested)
		}
	}
	// Something refused outright isn't "newer", it's unsupported.
	for _, r := range []string{"python3.9", "java17", "garbage"} {
		if RuntimeNewerThanTested(r) {
			t.Errorf("%q is refused, not newer-than-tested", r)
		}
	}
}

// Every runtime CI claims to test must actually pass the gate, or the matrix
// and the validator disagree.
func TestCITestedRuntimesAllPassTheGate(t *testing.T) {
	for _, r := range SupportedRuntimes {
		if !SupportsRuntime(r) {
			t.Errorf("%q is in SupportedRuntimes but the gate refuses it", r)
		}
	}
}
