package cli

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func reader(s string) *bufio.Reader { return bufio.NewReader(strings.NewReader(s)) }

func TestAskPick(t *testing.T) {
	opts := []pickOption{{label: "alpha"}, {label: "beta"}}
	var out bytes.Buffer

	if i, _ := askPick(reader("2\n"), &out, "q?", opts, 1); i != 1 {
		t.Errorf("number pick = %d, want 1", i)
	}
	if i, _ := askPick(reader("\n"), &out, "q?", opts, 2); i != 1 {
		t.Errorf("default pick = %d, want 1", i)
	}
	if i, _ := askPick(reader("beta\n"), &out, "q?", opts, 1); i != 1 {
		t.Errorf("name pick = %d, want 1", i)
	}
	if i, _ := askPick(reader("9\nalpha\n"), &out, "q?", opts, 1); i != 0 {
		t.Errorf("re-ask after invalid = %d, want 0", i)
	}
	if _, err := askPick(reader(""), &out, "q?", opts, 1); err == nil {
		t.Error("EOF should cancel")
	}
}

func TestAskTextValidates(t *testing.T) {
	var out bytes.Buffer
	got, err := askText(reader("Bad!\nfine\n"), &out, "name", "", func(s string) error {
		if strings.Contains(s, "!") {
			return fmt.Errorf("no bangs")
		}
		return nil
	})
	if err != nil || got != "fine" {
		t.Fatalf("got %q, %v", got, err)
	}
	if got, _ := askText(reader("\n"), &out, "name", "fallback", nil); got != "fallback" {
		t.Errorf("default = %q", got)
	}
}

func TestAskYesNo(t *testing.T) {
	var out bytes.Buffer
	if v, _ := askYesNo(reader("y\n"), &out, "sure?", false); !v {
		t.Error("y should be true")
	}
	if v, _ := askYesNo(reader("\n"), &out, "sure?", true); !v {
		t.Error("Enter should take the default")
	}
	if v, _ := askYesNo(reader("nope\n"), &out, "sure?", true); v {
		t.Error("anything else is no")
	}
}
