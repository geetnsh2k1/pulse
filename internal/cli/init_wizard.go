package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/geetnsh2k1/pulse/internal/config"
	"github.com/geetnsh2k1/pulse/internal/templates"
	"github.com/geetnsh2k1/pulse/internal/ui"
)

// Bare `pulse init` on a terminal runs a three-question wizard instead of
// erroring — nobody should have to learn flags on day one. Everything is
// plain stdin prompts (numbered choices, Enter = default), so it works over
// ssh and in any terminal; CI and scripts never see it (stdin must be a
// TTY), and every question has a flag equivalent.

// stdinIsInteractive reports whether we may ask questions. A real ioctl
// check, not ModeCharDevice — /dev/null is a char device and must not
// count. PULSE_ASSUME_TTY=1 forces it for tests.
func stdinIsInteractive() bool {
	if os.Getenv("PULSE_ASSUME_TTY") == "1" {
		return true
	}
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// stdoutIsTerminal answers a different question from stdinIsInteractive: not
// "can I ask?" but "is a human reading this?". Output that will be redirected
// into a file should be the data alone, with none of the framing.
func stdoutIsTerminal() bool {
	if os.Getenv("PULSE_ASSUME_TTY") == "1" {
		return true
	}
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// initWizard collects name/template/lang, mutating the same flag globals the
// flag path uses, and returns the project name. Reads from cmd.InOrStdin()
// so tests can feed answers.
func initWizard(cmd *cobra.Command) (string, error) {
	in := promptIn(cmd)
	out := cmd.OutOrStdout()

	fmt.Fprintf(out, "%s %s\n", ui.AccentBold("⚡ pulse"), ui.Dim("— three quick questions, Enter picks the default"))

	name, err := askProjectName(in, out)
	if err != nil {
		return "", err
	}

	tpl, err := askTemplate(in, out, cmd)
	if err != nil {
		return "", err
	}
	flagTemplate = tpl.Name

	if len(tpl.Variants) > 0 && !cmd.Flags().Changed("lang") {
		lang, err := askLanguage(in, out, tpl.Variants)
		if err != nil {
			return "", err
		}
		flagLang = lang
	}

	fmt.Fprintln(out)
	return name, nil
}

func askProjectName(in *bufio.Reader, out io.Writer) (string, error) {
	for {
		fmt.Fprintf(out, "\n%s project name %s › ", ui.Accent("?"), ui.Dim("(my-app)"))
		line, err := readAnswer(in)
		if err != nil {
			return "", err
		}
		if line == "" {
			line = "my-app"
		}
		if line != "." && !config.ValidProjectName(line) {
			fmt.Fprintf(out, "  %s lowercase letters, digits, and hyphens only\n", ui.Err("✗"))
			continue
		}
		dst := line
		if flagChdir != "" {
			dst = filepath.Join(flagChdir, dst)
		}
		if _, err := os.Stat(filepath.Join(dst, config.FileName)); err == nil {
			fmt.Fprintf(out, "  %s %s already has a pulse.yaml — pick another name\n", ui.Err("✗"), line)
			continue
		}
		return line, nil
	}
}

func askTemplate(in *bufio.Reader, out io.Writer, cmd *cobra.Command) (templates.Info, error) {
	all := wizardTemplateOrder(templates.List())
	if len(all) == 0 {
		return templates.Info{}, fmt.Errorf("no templates embedded — this build is broken")
	}
	if cmd.Flags().Changed("template") {
		for _, t := range all {
			if t.Name == flagTemplate {
				return t, nil
			}
		}
		return templates.Info{}, fmt.Errorf("unknown template %q — run `pulse init --list` to see what's available", flagTemplate)
	}

	fmt.Fprintf(out, "\n%s template — what should it start with?\n", ui.Accent("?"))
	for i, t := range all {
		marker := " "
		if i == 0 {
			marker = ui.Accent("★") // the recommended full demo
		}
		fmt.Fprintf(out, "    %s %s %s %s\n", ui.Dim(fmt.Sprintf("%d.", i+1)), ui.Bold(fmt.Sprintf("%-16s", t.Name)), marker, ui.Dim(t.Description))
	}
	for {
		fmt.Fprintf(out, "  pick 1-%d %s › ", len(all), ui.Dim("(1)"))
		line, err := readAnswer(in)
		if err != nil {
			return templates.Info{}, err
		}
		if line == "" {
			return all[0], nil
		}
		if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(all) {
			return all[n-1], nil
		}
		for _, t := range all {
			if t.Name == line {
				return t, nil
			}
		}
		fmt.Fprintf(out, "  %s answer with a number 1-%d or a template name\n", ui.Err("✗"), len(all))
	}
}

func askLanguage(in *bufio.Reader, out io.Writer, variants []string) (string, error) {
	fmt.Fprintf(out, "\n%s language  ", ui.Accent("?"))
	def := 1
	for i, v := range variants {
		fmt.Fprintf(out, "%d. %s  ", i+1, v)
		if v == "node" {
			def = i + 1 // keep the flag default
		}
	}
	for {
		fmt.Fprintf(out, "%s › ", ui.Dim(fmt.Sprintf("(%d)", def)))
		line, err := readAnswer(in)
		if err != nil {
			return "", err
		}
		if line == "" {
			return variants[def-1], nil
		}
		if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(variants) {
			return variants[n-1], nil
		}
		for _, v := range variants {
			if v == line {
				return v, nil
			}
		}
		fmt.Fprintf(out, "  %s answer with a number 1-%d or a language name\n  ", ui.Err("✗"), len(variants))
	}
}

// wizardTemplateOrder puts the full demo first — it's the recommended
// answer, and the wizard's default.
func wizardTemplateOrder(all []templates.Info) []templates.Info {
	out := make([]templates.Info, 0, len(all))
	for _, t := range all {
		if t.Name == "api-and-worker" {
			out = append(out, t)
		}
	}
	for _, t := range all {
		if t.Name != "api-and-worker" {
			out = append(out, t)
		}
	}
	return out
}

func readAnswer(in *bufio.Reader) (string, error) {
	line, err := in.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("cancelled")
	}
	return strings.TrimSpace(line), nil
}
