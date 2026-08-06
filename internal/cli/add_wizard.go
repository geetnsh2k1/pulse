package cli

import (
	"bufio"
	"fmt"

	"github.com/spf13/cobra"

	"pulse/internal/config"
)

// Bare `pulse add` on a terminal asks what to add and walks the questions —
// the flags stay for scripts, muscle memory, and CI.

func runAddWizard(cmd *cobra.Command, _ []string) error {
	if !stdinIsInteractive() {
		return cmd.Help()
	}
	cfg, err := loadProject()
	if err != nil {
		return err
	}
	in := bufio.NewReader(cmd.InOrStdin())
	out := cmd.OutOrStdout()

	kind, err := askPick(in, out, "what do you want to add?", []pickOption{
		{label: "function", desc: "code that runs — wire it to anything later"},
		{label: "route", desc: "a URL that calls a function"},
		{label: "queue", desc: "background jobs — with a worker function"},
		{label: "table", desc: "persistence, schema-free beyond its key"},
	}, 1)
	if err != nil {
		return err
	}
	fmt.Fprintln(out)

	switch kind {
	case 0:
		name, err := askText(in, out, "function name", "", validateNewFunction(cfg))
		if err != nil {
			return err
		}
		return runAddFunction(cmd, []string{name})

	case 1:
		methods := []pickOption{{label: "GET"}, {label: "POST"}, {label: "PUT"}, {label: "DELETE"}, {label: "PATCH"}, {label: "ANY"}}
		m, err := askPick(in, out, "method?", methods, 1)
		if err != nil {
			return err
		}
		path, err := askText(in, out, `path (like /things or "/things/{id}")`, "", validatePath)
		if err != nil {
			return err
		}
		fn, err := pickFunction(in, out, cfg, "which function should answer it?")
		if err != nil {
			return err
		}
		flagAddFn = fn
		return runAddRoute(cmd, []string{methods[m].label, path})

	case 2:
		name, err := askText(in, out, "queue name", "", nil)
		if err != nil {
			return err
		}
		worker, err := askText(in, out, "worker function (new or existing)", name+"-worker", nil)
		if err != nil {
			return err
		}
		dlq, err := askYesNo(in, out, "add a dead-letter queue (3 strikes and messages park there)?", false)
		if err != nil {
			return err
		}
		flagAddWorker, flagAddDLQ = worker, dlq
		fmt.Fprintln(out)
		return runAddQueue(cmd, []string{name})

	default:
		name, err := askText(in, out, "table name", "", nil)
		if err != nil {
			return err
		}
		pk, err := askText(in, out, "partition key", "id", nil)
		if err != nil {
			return err
		}
		flagAddPK = pk
		_ = cmd.Flags().Set("pk", pk) // mark Changed for the exists-check
		fmt.Fprintln(out)
		return runAddTable(addTableCmd, []string{name})
	}
}

func validateNewFunction(cfg *config.Config) func(string) error {
	return func(s string) error {
		if s == "" {
			return fmt.Errorf("a name is required")
		}
		if _, exists := cfg.Functions[s]; exists {
			return fmt.Errorf("function %q already exists", s)
		}
		return nil
	}
}

func validatePath(s string) error {
	if s == "" || s[0] != '/' {
		return fmt.Errorf("paths start with / (like /orders)")
	}
	return nil
}
