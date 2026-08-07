package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/geetnsh2k1/pulse/internal/templates"
)

// Dynamic tab completion: names come from the current project's pulse.yaml,
// so `pulse invoke <TAB>` offers *your* functions. All helpers fail silent —
// outside a project there is simply nothing to complete.

func init() {
	invokeCmd.ValidArgsFunction = completeFunctionArg
	logsCmd.ValidArgsFunction = completeFunctionArg
	sendCmd.ValidArgsFunction = completeQueueArg
	eventsReplayCmd.ValidArgsFunction = completeEventArg
	_ = eventsCmd.RegisterFlagCompletionFunc("function", completeFunctionFlag)
	_ = eventsListCmd.RegisterFlagCompletionFunc("function", completeFunctionFlag)

	_ = addRouteCmd.RegisterFlagCompletionFunc("function", completeFunctionFlag)
	_ = addQueueCmd.RegisterFlagCompletionFunc("worker", completeFunctionFlag)
	_ = addTableCmd.RegisterFlagCompletionFunc("function", completeFunctionFlag)

	_ = initCmd.RegisterFlagCompletionFunc("template",
		func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			var out []string
			for _, t := range templates.List() {
				out = append(out, t.Name+"\t"+t.Description)
			}
			return out, cobra.ShellCompDirectiveNoFileComp
		})
	_ = initCmd.RegisterFlagCompletionFunc("lang",
		func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			if v := templates.Variants(flagTemplate); len(v) > 0 {
				return v, cobra.ShellCompDirectiveNoFileComp
			}
			return []string{"node", "python"}, cobra.ShellCompDirectiveNoFileComp
		})

	// Commands whose arguments are free text or nothing: don't fall back to
	// filename completion.
	for _, c := range []*cobra.Command{startCmd, stopCmd, listCmd, validateCmd, versionCmd, addFunctionCmd, initCmd} {
		c.ValidArgsFunction = cobra.NoFileCompletions
	}
}

func completeFunctionArg(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return projectFunctions(), cobra.ShellCompDirectiveNoFileComp
}

func completeFunctionFlag(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return projectFunctions(), cobra.ShellCompDirectiveNoFileComp
}

// completeEventArg offers recent event ids with a what/when description.
func completeEventArg(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	cfg, err := loadProject()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	rows, err := recentEvents(cfg, "", 15)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var out []string
	for _, ev := range rows {
		out = append(out, fmt.Sprintf("%s\t%s → %s · %s", shortEventID(ev.ID), ev.Type, ev.Function, ev.Status))
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

func completeQueueArg(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	cfg, err := loadProject()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var out []string
	for name := range cfg.Resources.Queues {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, cobra.ShellCompDirectiveNoFileComp
}

// projectFunctions lists the current project's functions as "name\truntime".
func projectFunctions() []string {
	cfg, err := loadProject()
	if err != nil {
		return nil
	}
	var out []string
	for name, fn := range cfg.Functions {
		desc := fn.Runtime
		if fn.CodeDir != "" {
			desc += " · " + fn.CodeDir
		}
		out = append(out, name+"\t"+desc)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.SplitN(out[i], "\t", 2)[0] < strings.SplitN(out[j], "\t", 2)[0]
	})
	return out
}
