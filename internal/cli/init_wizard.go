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

	"pulse/internal/config"
	"pulse/internal/templates"
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

// initWizard collects name/template/lang, mutating the same flag globals the
// flag path uses, and returns the project name. Reads from cmd.InOrStdin()
// so tests can feed answers.
func initWizard(cmd *cobra.Command) (string, error) {
	in := bufio.NewReader(cmd.InOrStdin())
	out := cmd.OutOrStdout()

	fmt.Fprintln(out, "let's create a project — three quick questions, Enter picks the default")

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
		fmt.Fprint(out, "\n? project name (my-app) › ")
		line, err := readAnswer(in)
		if err != nil {
			return "", err
		}
		if line == "" {
			line = "my-app"
		}
		if line != "." && !config.ValidProjectName(line) {
			fmt.Fprintln(out, "  ✗ lowercase letters, digits, and hyphens only")
			continue
		}
		dst := line
		if flagChdir != "" {
			dst = filepath.Join(flagChdir, dst)
		}
		if _, err := os.Stat(filepath.Join(dst, config.FileName)); err == nil {
			fmt.Fprintf(out, "  ✗ %s already has a pulse.yaml — pick another name\n", line)
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

	fmt.Fprintln(out, "\n? template — what should it start with?")
	for i, t := range all {
		marker := " "
		if i == 0 {
			marker = "★" // the recommended full demo
		}
		fmt.Fprintf(out, "    %d. %-16s %s %s\n", i+1, t.Name, marker, t.Description)
	}
	for {
		fmt.Fprintf(out, "  pick 1-%d (1) › ", len(all))
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
		fmt.Fprintf(out, "  ✗ answer with a number 1-%d or a template name\n", len(all))
	}
}

func askLanguage(in *bufio.Reader, out io.Writer, variants []string) (string, error) {
	fmt.Fprint(out, "\n? language  ")
	def := 1
	for i, v := range variants {
		fmt.Fprintf(out, "%d. %s  ", i+1, v)
		if v == "node" {
			def = i + 1 // keep the flag default
		}
	}
	for {
		fmt.Fprintf(out, "(%d) › ", def)
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
		fmt.Fprintf(out, "  ✗ answer with a number 1-%d or a language name\n  ", len(variants))
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
