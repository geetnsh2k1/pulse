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

// oneFunctionArg requires exactly one function name and, when it's missing,
// lists the project's functions instead of cobra's bare arity error.
func oneFunctionArg(verb string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) == 1 {
			return nil
		}
		names := "run `pulse list` to see them"
		if cfg, err := loadProject(); err == nil && len(cfg.Functions) > 0 {
			names = strings.Join(functionNames(cfg), ", ")
		}
		if len(args) == 0 {
			return fmt.Errorf("which function? this project has: %s", names)
		}
		return fmt.Errorf("%s takes one function name, got %d (%s) — this project has: %s",
			verb, len(args), strings.Join(args, " "), names)
	}
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
