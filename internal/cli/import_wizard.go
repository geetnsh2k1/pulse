package cli

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/geetnsh2k1/pulse/internal/importer"
	"github.com/geetnsh2k1/pulse/internal/ui"
)

// The interactive half of `pulse import aws`. Kept apart from the command so
// every prompt can be tested with scripted input and no AWS at all.

// pickImportFunction chooses which Lambda to import. The list leads with the
// ones pulse can actually run and shows the reason beside the ones it can't —
// discovering a refusal after picking would waste the user's time.
func pickImportFunction(in *bufio.Reader, out io.Writer, list []importer.FunctionSummary, region string) (string, error) {
	if len(list) == 0 {
		return "", fmt.Errorf("no Lambda functions in %s\n    fix: wrong region? try --region <other>, or check `pulse aws whoami`", region)
	}

	runnable := 0
	for _, f := range list {
		if f.Importable {
			runnable++
		}
	}
	if runnable == 0 {
		var why []string
		for _, f := range list[:min(3, len(list))] {
			why = append(why, "  "+ui.Dim("· "+f.Name+" — "+f.Why))
		}
		return "", fmt.Errorf("none of the %d functions in %s can run locally:\n%s\n    fix: pulse imports zip-packaged Node 18+ / Python 3.10+ functions",
			len(list), region, strings.Join(why, "\n"))
	}
	if len(list) == 1 {
		fmt.Fprintf(out, "%s\n", ui.Dim("one function in "+region+": "+list[0].Name))
		return list[0].Name, nil
	}
	if !stdinIsInteractive() {
		names := make([]string, 0, runnable)
		for _, f := range list {
			if f.Importable {
				names = append(names, f.Name)
			}
		}
		return "", fmt.Errorf("no function chosen and there are %d to choose from\n    fix: pass --function with one of: %s",
			len(list), strings.Join(names, ", "))
	}

	opts := make([]pickOption, len(list))
	for i, f := range list {
		label, desc := f.Name, f.Runtime+" · "+humanSize(f.CodeSize)
		if !f.Importable {
			// Still listed: a user looking for a function should see it here
			// rather than wonder whether pulse missed it.
			label = ui.Dim(f.Name)
			desc = ui.Warn("can't run locally") + ui.Dim(" — "+f.Why)
		}
		opts[i] = pickOption{label: label, desc: desc}
	}
	idx, err := askPick(in, out, fmt.Sprintf("which function? (%d in %s)", len(list), region), opts, 1)
	if err != nil {
		return "", err
	}
	if !list[idx].Importable {
		return "", &importer.Refusal{
			Function: list[idx].Name,
			Reason:   list[idx].Name + " " + list[idx].Why,
			Fix:      "pick a zip-packaged Node 18+ or Python 3.10+ function",
		}
	}
	return list[idx].Name, nil
}

// confirmGuesses asks about the resources pulse inferred. AWS records what
// *triggers* a function but nothing about what its code calls at runtime, so
// these are proposals with their evidence attached — strong ones pre-checked,
// weak ones shown unchecked, and the whole list one Enter away from done.
//
// assumeYes and non-interactive callers take the pre-checked defaults, so
// `--yes` in CI is predictable (PLAN §12.9 rule 5).
func confirmGuesses(in *bufio.Reader, out io.Writer, guesses []importer.Guess, assumeYes bool) ([]importer.Guess, error) {
	if len(guesses) == 0 {
		return nil, nil
	}
	if assumeYes || !stdinIsInteractive() {
		var picked []importer.Guess
		for _, g := range guesses {
			if g.Strong {
				picked = append(picked, g)
			}
		}
		if len(picked) > 0 {
			fmt.Fprintf(out, "%s\n", ui.Dim(fmt.Sprintf("taking %d inferred resource(s) with strong evidence: %s",
				len(picked), strings.Join(guessNames(picked), ", "))))
		}
		if weak := len(guesses) - len(picked); weak > 0 {
			fmt.Fprintf(out, "%s\n", ui.Dim(fmt.Sprintf("skipping %d weaker guess(es) — add them later with `pulse add`", weak)))
		}
		return picked, nil
	}

	opts := make([]multiOption, len(guesses))
	for i, g := range guesses {
		opts[i] = multiOption{
			label:   g.Name,
			desc:    g.Kind + " · " + evidence(g),
			checked: g.Strong,
		}
	}
	fmt.Fprintf(out, "\n%s %s\n", ui.AccentBold("⚡ resources your code may use"),
		ui.Dim("— AWS doesn't record these, so pulse inferred them"))
	checked, err := askMultiPick(in, out, "include these? (checked ones have strong evidence)", opts)
	if err != nil {
		return nil, err
	}
	var picked []importer.Guess
	for i, ok := range checked {
		if ok {
			picked = append(picked, guesses[i])
		}
	}
	return picked, nil
}

// evidence renders why pulse thinks a resource is used, in the user's terms.
// Capped at two reasons: a line that wraps twice stops being read, and two
// signals is already the threshold for a strong guess.
func evidence(g importer.Guess) string {
	if len(g.Signals) == 0 {
		return "inferred"
	}
	s := append([]string(nil), g.Signals...)
	sort.Strings(s)
	if len(s) > 2 {
		return fmt.Sprintf("%s + %d more", strings.Join(s[:2], " + "), len(s)-2)
	}
	return strings.Join(s, " + ")
}

func guessNames(gs []importer.Guess) []string {
	out := make([]string, len(gs))
	for i, g := range gs {
		out[i] = g.Name
	}
	return out
}

// previewPlan prints exactly what would be written, including everything
// that wouldn't. Nothing about an import should be a surprise afterwards.
func previewPlan(out io.Writer, p *importer.Plan, dest string) {
	fmt.Fprintf(out, "\n%s %s\n", ui.AccentBold("⚡ import plan"), ui.Dim("→ "+dest))

	fmt.Fprintf(out, "\n  %s\n", ui.Bold("functions"))
	for _, f := range p.Functions {
		fmt.Fprintf(out, "    %-22s %s\n", ui.Fn(f.Name),
			ui.Dim(fmt.Sprintf("%s · %s · %ds · %d MB", f.Runtime, f.Handler, f.TimeoutSec, f.MemoryMB)))
		if n := len(f.EnvNames); n > 0 {
			fmt.Fprintf(out, "    %s\n", ui.Dim(fmt.Sprintf("  %d environment variable(s) → .env", n)))
		}
	}

	var routes, queues []importer.PlannedTrigger
	for _, t := range p.Triggers {
		if t.Kind == "http" {
			routes = append(routes, t)
		} else {
			queues = append(queues, t)
		}
	}
	if len(routes) > 0 {
		fmt.Fprintf(out, "\n  %s %s\n", ui.Bold("routes"), ui.Dim("· confirmed by AWS"))
		for _, t := range routes {
			fmt.Fprintf(out, "    %-6s %-24s %s %s\n", t.Method, t.Path, ui.Dim("→"), ui.Fn(t.Function))
		}
	}
	if len(queues) > 0 {
		fmt.Fprintf(out, "\n  %s %s\n", ui.Bold("queue triggers"), ui.Dim("· confirmed by AWS"))
		for _, t := range queues {
			fmt.Fprintf(out, "    %-24s %s %s %s\n", ui.Cyan(t.Queue), ui.Dim("→"), ui.Fn(t.Function),
				ui.Dim(fmt.Sprintf("· batch of %d", t.BatchSize)))
		}
	}
	if len(p.Tables) > 0 {
		fmt.Fprintf(out, "\n  %s\n", ui.Bold("tables"))
		for _, t := range p.Tables {
			key := "pk " + t.PK.Name
			if t.SK != nil {
				key += ", sk " + t.SK.Name
			}
			fmt.Fprintf(out, "    %-24s %s\n", ui.Bold(t.Name), ui.Dim(key+" · "+provenance(t.Provenance, t.Signals)))
		}
	}
	if len(p.Queues) > 0 {
		fmt.Fprintf(out, "\n  %s\n", ui.Bold("queues"))
		for _, q := range p.Queues {
			note := provenance(q.Provenance, q.Signals)
			if q.DLQ != "" {
				note = "dlq " + q.DLQ + " after " + fmt.Sprint(q.MaxReceiveCount) + " · " + note
			}
			fmt.Fprintf(out, "    %-24s %s\n", ui.Cyan(q.Name), ui.Dim(note))
		}
	}

	// The honesty section. Printed last because it's what someone should
	// still have on screen when they answer the confirmation.
	if len(p.Warnings) > 0 {
		fmt.Fprintf(out, "\n  %s %s\n", ui.Warn("caveats"), ui.Dim("· imported, but not identical to AWS"))
		for _, n := range p.Warnings {
			fmt.Fprintf(out, "    %s %s %s\n", ui.Warn("✱"), n.Subject, ui.Dim("— "+n.Detail))
		}
	}
	if len(p.Unsupported) > 0 {
		fmt.Fprintf(out, "\n  %s %s\n", ui.Err("not imported"), ui.Dim("· pulse can't represent these yet"))
		for _, n := range p.Unsupported {
			fmt.Fprintf(out, "    %s %s %s\n", ui.Err("✗"), n.Subject, ui.Dim("— "+n.Detail))
		}
	}
	fmt.Fprintf(out, "\n  %s\n", ui.Dim("all of the above is also written to IMPORT-NOTES.md"))
}

func provenance(pv importer.Provenance, signals []string) string {
	switch pv {
	case importer.Confirmed:
		return "confirmed by AWS"
	case importer.Picked:
		return "you confirmed it"
	default:
		if len(signals) == 0 {
			return "inferred"
		}
		s := append([]string(nil), signals...)
		sort.Strings(s)
		return "inferred from " + strings.Join(s, " + ")
	}
}

func humanSize(b int64) string {
	switch {
	case b <= 0:
		return "size unknown"
	case b < 1<<10:
		// A small handler is genuinely a few hundred bytes; rounding it to
		// "0 KB" reads like pulse failed to measure it.
		return fmt.Sprintf("%d bytes", b)
	case b < 1<<20:
		return fmt.Sprintf("%d KB", b>>10)
	default:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	}
}
