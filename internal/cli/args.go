package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

// Placeholder args like <function> confuse newcomers ("which function?").
// These Args validators answer the question in the error itself, with the
// caller's own names from pulse.yaml.

// resolveFunctionArg turns an optional positional into a function name: the
// argument when given, an interactive picker on a terminal, and the
// teaching error ("this project has: …") everywhere else.
func resolveFunctionArg(cmd *cobra.Command, args []string, verb, question string) (string, error) {
	if len(args) == 1 {
		return args[0], nil
	}
	cfg, err := loadProject()
	if err != nil {
		return "", err
	}
	if stdinIsInteractive() && len(cfg.Functions) > 0 {
		return pickFunction(promptIn(cmd), cmd.OutOrStdout(), cfg, question)
	}
	names := "run `pulse list` to see them"
	if len(cfg.Functions) > 0 {
		names = strings.Join(functionNames(cfg), ", ")
	}
	return "", fmt.Errorf("which function? %s needs one — this project has: %s", verb, names)
}

// queueFirstArg requires a queue name (plus optionally more args, checked by
// max) and lists the project's queues when it's missing.
func queueFirstArg(verb string, max int) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) >= 1 && len(args) <= max {
			return nil
		}
		names := "run `pulse list` to see them"
		if cfg, err := loadProject(); err == nil && len(cfg.Resources.Queues) > 0 {
			qs := make([]string, 0, len(cfg.Resources.Queues))
			for q := range cfg.Resources.Queues {
				qs = append(qs, q)
			}
			sort.Strings(qs)
			names = strings.Join(qs, ", ")
		}
		if len(args) == 0 {
			return fmt.Errorf("which queue? this project has: %s", names)
		}
		return fmt.Errorf("%s takes a queue and an optional message body, got %d args (%s)",
			verb, len(args), strings.Join(args, " "))
	}
}
