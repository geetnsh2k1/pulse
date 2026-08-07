package cli

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/geetnsh2k1/pulse/internal/ui"
)

// Generic interactive prompts — the init wizard's pattern, reusable by
// every command that can ask instead of erroring. All prompts read from an
// injected reader so tests can script answers.

// pickOption is one numbered choice.
type pickOption struct {
	label string // what the user picks by (also matched by name)
	desc  string // dim explanation, optional
}

// askPick shows numbered options and returns the chosen index. Enter picks
// def (1-based). Answers match by number or by label text.
func askPick(in *bufio.Reader, out io.Writer, question string, options []pickOption, def int) (int, error) {
	fmt.Fprintf(out, "\n%s %s\n", ui.Accent("?"), question)
	for i, o := range options {
		line := fmt.Sprintf("    %s %s", ui.Dim(fmt.Sprintf("%d.", i+1)), ui.Bold(o.label))
		if o.desc != "" {
			line += "  " + ui.Dim(o.desc)
		}
		fmt.Fprintln(out, line)
	}
	for {
		fmt.Fprintf(out, "  pick 1-%d %s › ", len(options), ui.Dim(fmt.Sprintf("(%d)", def)))
		line, err := readAnswer(in)
		if err != nil {
			return 0, err
		}
		if line == "" {
			return def - 1, nil
		}
		if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(options) {
			return n - 1, nil
		}
		for i, o := range options {
			if o.label == line {
				return i, nil
			}
		}
		fmt.Fprintf(out, "  %s answer with a number 1-%d\n", ui.Err("✗"), len(options))
	}
}

// askText prompts for one line; Enter returns def. validate may be nil.
func askText(in *bufio.Reader, out io.Writer, question, def string, validate func(string) error) (string, error) {
	hint := ""
	if def != "" {
		hint = " " + ui.Dim("("+def+")")
	}
	for {
		fmt.Fprintf(out, "\n%s %s%s › ", ui.Accent("?"), question, hint)
		line, err := readAnswer(in)
		if err != nil {
			return "", err
		}
		if line == "" {
			line = def
		}
		if validate != nil {
			if err := validate(line); err != nil {
				fmt.Fprintf(out, "  %s %s\n", ui.Err("✗"), err)
				continue
			}
		}
		return line, nil
	}
}

// askYesNo prompts y/N style; def is the Enter answer.
func askYesNo(in *bufio.Reader, out io.Writer, question string, def bool) (bool, error) {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	fmt.Fprintf(out, "\n%s %s %s › ", ui.Accent("?"), question, ui.Dim("("+hint+")"))
	line, err := readAnswer(in)
	if err != nil {
		return false, err
	}
	switch strings.ToLower(line) {
	case "":
		return def, nil
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

// pickFunction is the everywhere-needed "which function?" prompt.
func pickFunction(in *bufio.Reader, out io.Writer, cfg interface{ FunctionNames() []string }, question string) (string, error) {
	names := cfg.FunctionNames()
	opts := make([]pickOption, len(names))
	for i, n := range names {
		opts[i] = pickOption{label: n}
	}
	i, err := askPick(in, out, question, opts, 1)
	if err != nil {
		return "", err
	}
	return names[i], nil
}
