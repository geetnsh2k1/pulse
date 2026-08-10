package cli

import "testing"

func TestParseMajorMinor(t *testing.T) {
	cases := []struct {
		in       string
		maj, min int
		ok       bool
	}{
		{"v23.7.0", 23, 7, true},       // node
		{"v18.20.4", 18, 20, true},     // node LTS
		{"Python 3.13.2", 3, 13, true}, // python3 --version
		{"Python 3.9.21", 3, 9, true},  // below the floor
		{"3.10", 3, 10, true},          // bare x.y
		{"Python 3", 0, 0, false},      // no minor → unparsable
		{"unknown", 0, 0, false},       // no digits at all
		{"", 0, 0, false},              // empty
	}
	for _, c := range cases {
		maj, min, ok := parseMajorMinor(c.in)
		if maj != c.maj || min != c.min || ok != c.ok {
			t.Errorf("parseMajorMinor(%q) = (%d,%d,%v), want (%d,%d,%v)",
				c.in, maj, min, ok, c.maj, c.min, c.ok)
		}
	}
}

func TestRuntimeSupported(t *testing.T) {
	cases := []struct {
		family, version string
		want            bool
	}{
		{"node", "v18.20.4", true},
		{"node", "v20.11.0", true},
		{"node", "v23.7.0", true},   // newer than any hardcoded list — must pass
		{"node", "v16.20.2", false}, // below the floor
		{"python", "Python 3.10.15", true},
		{"python", "Python 3.13.2", true}, // CI tests this; must not warn
		{"python", "Python 3.9.21", false},
		{"python", "Python 4.0.0", true}, // future major
		{"node", "weird build", true},    // unparsable → never cry wolf
		{"python", "", true},
	}
	for _, c := range cases {
		if got := runtimeSupported(c.family, c.version); got != c.want {
			t.Errorf("runtimeSupported(%q, %q) = %v, want %v", c.family, c.version, got, c.want)
		}
	}
}

func TestLabelled(t *testing.T) {
	if got := labelled("node", "v23.7.0"); got != "node v23.7.0" {
		t.Errorf("labelled(node) = %q", got)
	}
	if got := labelled("python", "Python 3.13.2"); got != "Python 3.13.2" {
		t.Errorf("labelled(python) should not double-prefix, got %q", got)
	}
}
