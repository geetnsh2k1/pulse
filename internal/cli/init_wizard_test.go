package cli

import (
	"bytes"
	"strings"
	"testing"
)

// driveWizard runs initWizard with scripted answers and returns the chosen
// name plus the flag globals it set.
func driveWizard(t *testing.T, answers string) (name, tpl, lang string) {
	t.Helper()
	prevTpl, prevLang, prevChdir := flagTemplate, flagLang, flagChdir
	flagTemplate, flagLang, flagChdir = "hello", "node", t.TempDir()
	initCmd.Flags().Lookup("template").Changed = false
	initCmd.Flags().Lookup("lang").Changed = false
	t.Cleanup(func() { flagTemplate, flagLang, flagChdir = prevTpl, prevLang, prevChdir })

	initCmd.SetIn(strings.NewReader(answers))
	initCmd.SetOut(&bytes.Buffer{})
	t.Cleanup(func() { initCmd.SetIn(nil); initCmd.SetOut(nil) })

	got, err := initWizard(initCmd)
	if err != nil {
		t.Fatalf("initWizard: %v", err)
	}
	return got, flagTemplate, flagLang
}

func TestWizardDefaults(t *testing.T) {
	name, tpl, lang := driveWizard(t, "\n\n\n")
	if name != "my-app" || tpl != "api-and-worker" || lang != "node" {
		t.Errorf("defaults = %q %q %q, want my-app api-and-worker node", name, tpl, lang)
	}
}

func TestWizardPicksByNumber(t *testing.T) {
	name, tpl, lang := driveWizard(t, "cafe\n2\n2\n") // 2 = hello, lang 2 = python
	if name != "cafe" || tpl != "hello" || lang != "python" {
		t.Errorf("got %q %q %q, want cafe hello python", name, tpl, lang)
	}
}

func TestWizardRejectsBadAnswersThenAccepts(t *testing.T) {
	// bad name → ok name; out-of-range template → valid; python by name.
	name, tpl, lang := driveWizard(t, "Bad Name!\ncafe\n9\n1\npython\n")
	if name != "cafe" || tpl != "api-and-worker" || lang != "python" {
		t.Errorf("got %q %q %q, want cafe api-and-worker python", name, tpl, lang)
	}
}

func TestWizardCancelledOnEOF(t *testing.T) {
	prevChdir := flagChdir
	flagChdir = t.TempDir()
	initCmd.Flags().Lookup("template").Changed = false
	t.Cleanup(func() { flagChdir = prevChdir })
	initCmd.SetIn(strings.NewReader("")) // immediate EOF
	initCmd.SetOut(&bytes.Buffer{})
	t.Cleanup(func() { initCmd.SetIn(nil); initCmd.SetOut(nil) })
	if _, err := initWizard(initCmd); err == nil {
		t.Fatal("want cancelled error on EOF")
	}
}
