package cli

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/geetnsh2k1/pulse/internal/importer"
)

// scripted feeds prompt answers and captures what the user would see.
func scripted(answers string) (*bufio.Reader, *bytes.Buffer) {
	return bufio.NewReader(strings.NewReader(answers)), &bytes.Buffer{}
}

func fnList() []importer.FunctionSummary {
	return []importer.FunctionSummary{
		{Name: "createOrder", Runtime: "python3.12", CodeSize: 4 << 20, Importable: true},
		{Name: "worker", Runtime: "nodejs20.x", CodeSize: 900 << 10, Importable: true},
		{Name: "legacy", Runtime: "java17", CodeSize: 30 << 20, Importable: false,
			Why: "uses runtime java17, which pulse can't run"},
	}
}

func TestPickImportFunctionShowsWhatCannotRunAndWhy(t *testing.T) {
	t.Setenv("PULSE_ASSUME_TTY", "1")
	in, out := scripted("2\n")

	got, err := pickImportFunction(in, out, fnList(), "eu-west-1")
	if err != nil {
		t.Fatalf("pickImportFunction: %v", err)
	}
	if got != "worker" {
		t.Errorf("chose %q, want worker", got)
	}
	screen := out.String()
	// The unrunnable one is still listed — a user looking for it must not
	// conclude pulse failed to see it.
	if !strings.Contains(screen, "legacy") || !strings.Contains(screen, "java17") {
		t.Errorf("the refused function and its reason should be visible:\n%s", screen)
	}
	if !strings.Contains(screen, "3 in eu-west-1") {
		t.Errorf("the question should say where it looked:\n%s", screen)
	}
}

func TestPickImportFunctionRefusesAnUnrunnableChoice(t *testing.T) {
	t.Setenv("PULSE_ASSUME_TTY", "1")
	in, out := scripted("3\n")

	_, err := pickImportFunction(in, out, fnList(), "eu-west-1")
	if err == nil {
		t.Fatal("want a refusal when the picked function can't run locally")
	}
	if !strings.Contains(err.Error(), "java17") || !strings.Contains(err.Error(), "fix:") {
		t.Errorf("error = %v", err)
	}
}

func TestPickImportFunctionSkipsTheQuestionForOne(t *testing.T) {
	t.Setenv("PULSE_ASSUME_TTY", "1")
	in, out := scripted("") // nothing to read: nothing should be asked

	got, err := pickImportFunction(in, out, fnList()[:1], "eu-west-1")
	if err != nil {
		t.Fatalf("pickImportFunction: %v", err)
	}
	if got != "createOrder" {
		t.Errorf("chose %q", got)
	}
	if strings.Contains(out.String(), "which function?") {
		t.Error("one function means one answer — don't ask")
	}
}

func TestPickImportFunctionOnAnEmptyRegionSuggestsAnother(t *testing.T) {
	in, out := scripted("")
	_, err := pickImportFunction(in, out, nil, "ap-south-1")
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "ap-south-1") || !strings.Contains(err.Error(), "--region") {
		t.Errorf("error should name the region and the fix, got: %v", err)
	}
}

func TestPickImportFunctionWhenNothingCanRun(t *testing.T) {
	t.Setenv("PULSE_ASSUME_TTY", "1")
	in, out := scripted("1\n")
	list := []importer.FunctionSummary{
		{Name: "legacy", Runtime: "java17", Why: "uses runtime java17, which pulse can't run"},
		{Name: "imaged", PackageType: "Image", Why: "is a container-image function"},
	}
	_, err := pickImportFunction(in, out, list, "us-east-1")
	if err == nil {
		t.Fatal("want an error rather than a picker with no valid answer")
	}
	for _, want := range []string{"legacy", "java17", "Node 18+"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

// Non-interactive callers must never be left at a prompt.
func TestPickImportFunctionNonInteractiveNamesTheFlag(t *testing.T) {
	t.Setenv("PULSE_ASSUME_TTY", "0")
	in, out := scripted("")
	_, err := pickImportFunction(in, out, fnList(), "eu-west-1")
	if err == nil {
		t.Fatal("want an error, not a hang")
	}
	if !strings.Contains(err.Error(), "--function") || !strings.Contains(err.Error(), "createOrder") {
		t.Errorf("error should name the flag and the choices, got: %v", err)
	}
}

func guesses() []importer.Guess {
	return []importer.Guess{
		{Name: "orders", Kind: "table", Strong: true, Signals: []string{"env ORDERS_TABLE", "iam policy"}},
		{Name: "audit-log", Kind: "table", Strong: false, Signals: []string{"mentioned in code"}},
		{Name: "emails", Kind: "queue", Strong: false, Signals: []string{"mentioned in code"}},
	}
}

func TestConfirmGuessesPreChecksTheStrongOnes(t *testing.T) {
	t.Setenv("PULSE_ASSUME_TTY", "1")
	in, out := scripted("\n") // Enter accepts what's shown

	got, err := confirmGuesses(in, out, guesses(), false)
	if err != nil {
		t.Fatalf("confirmGuesses: %v", err)
	}
	if len(got) != 1 || got[0].Name != "orders" {
		t.Errorf("accepted %v, want just the strong guess", guessNames(got))
	}
	screen := out.String()
	if !strings.Contains(screen, "[x] 1. orders") {
		t.Errorf("strong guesses must arrive checked:\n%s", screen)
	}
	if !strings.Contains(screen, "[ ] 2. audit-log") {
		t.Errorf("weak guesses must arrive unchecked:\n%s", screen)
	}
	// The evidence is the whole reason to trust or reject a guess.
	if !strings.Contains(screen, "env ORDERS_TABLE") || !strings.Contains(screen, "mentioned in code") {
		t.Errorf("evidence should be on screen:\n%s", screen)
	}
}

func TestConfirmGuessesToggles(t *testing.T) {
	t.Setenv("PULSE_ASSUME_TTY", "1")
	// Toggle 1 off and 2 on by number, then accept.
	in, out := scripted("1 2\n\n")

	got, err := confirmGuesses(in, out, guesses(), false)
	if err != nil {
		t.Fatalf("confirmGuesses: %v", err)
	}
	if names := strings.Join(guessNames(got), ","); names != "audit-log" {
		t.Errorf("selected %q, want audit-log", names)
	}
	_ = out
}

func TestConfirmGuessesAcceptsNamesAllAndNone(t *testing.T) {
	t.Setenv("PULSE_ASSUME_TTY", "1")

	in, _ := scripted("all\n\n")
	got, err := confirmGuesses(in, &bytes.Buffer{}, guesses(), false)
	if err != nil || len(got) != 3 {
		t.Errorf(`"all" selected %d, want 3 (err %v)`, len(got), err)
	}

	in, _ = scripted("none\n\n")
	got, err = confirmGuesses(in, &bytes.Buffer{}, guesses(), false)
	if err != nil || len(got) != 0 {
		t.Errorf(`"none" selected %d, want 0 (err %v)`, len(got), err)
	}

	in, _ = scripted("emails\n\n") // by name, not by number
	got, err = confirmGuesses(in, &bytes.Buffer{}, guesses(), false)
	if err != nil {
		t.Fatal(err)
	}
	if names := strings.Join(guessNames(got), ","); names != "orders,emails" {
		t.Errorf("selected %q, want orders,emails", names)
	}
}

func TestConfirmGuessesRejectsNonsenseWithoutLosingState(t *testing.T) {
	t.Setenv("PULSE_ASSUME_TTY", "1")
	in, out := scripted("nope\n\n")

	got, err := confirmGuesses(in, out, guesses(), false)
	if err != nil {
		t.Fatalf("confirmGuesses: %v", err)
	}
	if len(got) != 1 || got[0].Name != "orders" {
		t.Errorf("a bad answer must not change the selection, got %v", guessNames(got))
	}
	if !strings.Contains(out.String(), "don't know nope") {
		t.Errorf("the user should be told what wasn't understood:\n%s", out.String())
	}
}

// --yes and pipes take the pre-checked defaults, so CI is predictable.
func TestConfirmGuessesWithYesTakesStrongOnly(t *testing.T) {
	t.Setenv("PULSE_ASSUME_TTY", "1")
	in, out := scripted("")

	got, err := confirmGuesses(in, out, guesses(), true)
	if err != nil {
		t.Fatalf("confirmGuesses: %v", err)
	}
	if len(got) != 1 || got[0].Name != "orders" {
		t.Errorf("--yes selected %v, want the strong guess only", guessNames(got))
	}
	screen := out.String()
	if !strings.Contains(screen, "orders") || !strings.Contains(screen, "skipping 2") {
		t.Errorf("--yes should still report what it took and skipped:\n%s", screen)
	}
	if strings.Contains(screen, "toggle") {
		t.Error("--yes must not prompt")
	}
}

func TestPreviewPlanIsHonestAboutEverything(t *testing.T) {
	fn := importer.Function{
		Name: "createOrder", Runtime: "python3.12", Handler: "handler.handler",
		TimeoutSec: 10, MemoryMB: 512, PackageType: "Zip", CodeSize: 4 << 20,
		Env:    map[string]string{"API_KEY": "secret", "TABLE_NAME": "orders"},
		Layers: []string{"arn:aws:lambda:eu-west-1:1:layer:deps:3"},
	}
	d := importer.Discovery{
		Region:   "eu-west-1",
		Function: fn,
		Routes:   []importer.HTTPRoute{{Method: "POST", Path: "/orders"}},
		EventSources: []importer.EventSource{
			{Kind: "sqs", ARN: "arn:aws:sqs:eu-west-1:1:order-events", BatchSize: 10, Enabled: true},
			{Kind: "kinesis", ARN: "arn:aws:kinesis:eu-west-1:1:stream/clicks"},
		},
	}
	plan, err := importer.BuildPlan(d, "shop")
	if err != nil {
		t.Fatal(err)
	}
	plan.AddTable(importer.Table{Name: "orders", PK: importer.Key{Name: "id", Type: "S"}},
		importer.Guessed, []string{"env TABLE_NAME", "iam policy"})

	out := &bytes.Buffer{}
	previewPlan(out, plan, "./shop")
	screen := out.String()

	for _, want := range []string{
		"./shop", // where it lands
		"createOrder", "python3.12", "handler.handler",
		"POST", "/orders", // the confirmed route
		"order-events",    // the confirmed queue trigger
		"orders", "pk id", // the table with its real key
		"inferred from env TABLE_NAME + iam policy", // and why it's there
		"2 environment variable(s) → .env",
		"caveats", "layer", // imported with a catch
		"not imported", "kinesis", // and what didn't come at all
		"IMPORT-NOTES.md",
	} {
		if !strings.Contains(screen, want) {
			t.Errorf("preview is missing %q:\n%s", want, screen)
		}
	}
	// A preview that leaks the value defeats the point of placeholdering it.
	if strings.Contains(screen, "secret") {
		t.Error("the preview printed an environment value")
	}
}

func TestImportDestStaysRelativeWhenItCan(t *testing.T) {
	old := flagChdir
	defer func() { flagChdir = old }()
	flagChdir = ""

	if got := importDest("shop"); got != "./shop" {
		t.Errorf("importDest = %q, want ./shop", got)
	}
}

func TestNotInsideAProjectRefusesWithAFix(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pulse.yaml"), []byte("project: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	old := flagChdir
	defer func() { flagChdir = old }()
	flagChdir = dir

	err := notInsideAProject()
	if err == nil {
		t.Fatal("import must refuse to run inside an existing project")
	}
	if !strings.Contains(err.Error(), "already a pulse project") || !strings.Contains(err.Error(), "fix:") {
		t.Errorf("error = %v", err)
	}
}

func TestHumanSize(t *testing.T) {
	cases := map[int64]string{
		0: "size unknown", 4 << 10: "4 KB", 900 << 10: "900 KB", 4 << 20: "4.0 MB",
	}
	for in, want := range cases {
		if got := humanSize(in); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", in, got, want)
		}
	}
}
