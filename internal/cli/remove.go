package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/geetnsh2k1/pulse/internal/config"
	"github.com/geetnsh2k1/pulse/internal/ui"
)

// pulse remove — add's twin. Every removal is yaml surgery (comments
// survive, result validated-or-reverted, applied live). Code and data are
// never deleted: removing a function keeps its folder, removing a table
// keeps its rows in .pulse/.

var removeCmd = &cobra.Command{
	Use:     "remove",
	Aliases: []string{"rm"},
	Short:   "Remove functions, routes, queues, or tables from the project",
	Long: `The inverse of pulse add. Removals edit pulse.yaml surgically and apply
live; your code files and stored data are never deleted — only the wiring.

Bare ` + "`pulse remove`" + ` on a terminal asks what to remove.`,
	Args: cobra.NoArgs,
	RunE: runRemoveWizard,
}

var removeFunctionCmd = &cobra.Command{
	Use:   "function <name>",
	Short: "Remove a function and every trigger pointing at it (code stays)",
	Args:  cobra.ExactArgs(1),
	RunE:  runRemoveFunction,
}

var removeRouteCmd = &cobra.Command{
	Use:   "route <METHOD> <path>",
	Short: "Remove one HTTP route",
	Args:  cobra.ExactArgs(2),
	RunE:  runRemoveRoute,
}

var removeQueueCmd = &cobra.Command{
	Use:   "queue <name>",
	Short: "Remove a queue and its sqs trigger (queued data stays in .pulse/)",
	Args:  cobra.ExactArgs(1),
	RunE:  runRemoveQueue,
}

var removeTableCmd = &cobra.Command{
	Use:   "table <name>",
	Short: "Remove a table declaration (its rows stay in .pulse/)",
	Args:  cobra.ExactArgs(1),
	RunE:  runRemoveTable,
}

func init() {
	removeCmd.AddCommand(removeFunctionCmd, removeRouteCmd, removeQueueCmd, removeTableCmd)
	rootCmd.AddCommand(removeCmd)
	removeFunctionCmd.ValidArgsFunction = completeFunctionArg
}

func runRemoveWizard(cmd *cobra.Command, _ []string) error {
	if !stdinIsInteractive() {
		return cmd.Help()
	}
	cfg, err := loadProject()
	if err != nil {
		return err
	}
	in := promptIn(cmd)
	out := cmd.OutOrStdout()

	kind, err := askPick(in, out, "what do you want to remove?", []pickOption{
		{label: "function", desc: "also removes triggers pointing at it — code stays"},
		{label: "route", desc: "one URL mapping"},
		{label: "queue", desc: "and its sqs trigger — queued data stays"},
		{label: "table", desc: "declaration only — rows stay in .pulse/"},
	}, 1)
	if err != nil {
		return err
	}
	fmt.Fprintln(out)

	switch kind {
	case 0:
		fn, err := pickFunction(in, out, cfg, "which function?")
		if err != nil {
			return err
		}
		return runRemoveFunction(cmd, []string{fn})
	case 1:
		var routes []pickOption
		for _, t := range cfg.Triggers {
			if t.Type == "http" {
				routes = append(routes, pickOption{label: t.Method + " " + t.Path, desc: "→ " + t.Function})
			}
		}
		if len(routes) == 0 {
			return fmt.Errorf("no routes to remove")
		}
		i, err := askPick(in, out, "which route?", routes, 1)
		if err != nil {
			return err
		}
		parts := strings.SplitN(routes[i].label, " ", 2)
		return runRemoveRoute(cmd, parts)
	case 2:
		names := sortedKeys(cfg.Resources.Queues)
		if len(names) == 0 {
			return fmt.Errorf("no queues to remove")
		}
		opts := make([]pickOption, len(names))
		for i, n := range names {
			opts[i] = pickOption{label: n}
		}
		i, err := askPick(in, out, "which queue?", opts, 1)
		if err != nil {
			return err
		}
		return runRemoveQueue(cmd, []string{names[i]})
	default:
		names := sortedKeys(cfg.Resources.Tables)
		if len(names) == 0 {
			return fmt.Errorf("no tables to remove")
		}
		opts := make([]pickOption, len(names))
		for i, n := range names {
			opts[i] = pickOption{label: n}
		}
		i, err := askPick(in, out, "which table?", opts, 1)
		if err != nil {
			return err
		}
		return runRemoveTable(cmd, []string{names[i]})
	}
}

func runRemoveFunction(_ *cobra.Command, args []string) error {
	name := args[0]
	cfg, err := loadProject()
	if err != nil {
		return err
	}
	fn, ok := cfg.Functions[name]
	if !ok {
		return fmt.Errorf("unknown function %q — this project has: %s",
			name, strings.Join(cfg.FunctionNames(), ", "))
	}

	dropped := 0
	err = config.EditYAML(cfg.Path, func(root *yaml.Node) error {
		if !config.RemoveMapEntry(config.TopMap(root, "functions"), name) {
			return fmt.Errorf("function %q not in pulse.yaml", name)
		}
		dropped = config.FilterSeq(config.TopSeq(root, "triggers"), func(t *yaml.Node) bool {
			return config.MapScalar(t, "function") != name
		})
		return nil
	})
	if err != nil {
		return err
	}

	fmt.Printf("%s removed function %s\n", ui.OK("✓"), ui.Bold(name))
	if dropped > 0 {
		fmt.Printf("  also removed %d trigger(s) that pointed at it\n", dropped)
	}
	fmt.Println("  " + ui.Hint(fmt.Sprintf("code kept at `%s` — delete the folder yourself if it's unwanted", fn.CodeDir)))
	printAppliesLive(cfg.Root)
	return nil
}

func runRemoveRoute(_ *cobra.Command, args []string) error {
	method, path := strings.ToUpper(args[0]), args[1]
	cfg, err := loadProject()
	if err != nil {
		return err
	}

	removed := 0
	err = config.EditYAML(cfg.Path, func(root *yaml.Node) error {
		removed = config.FilterSeq(config.TopSeq(root, "triggers"), func(t *yaml.Node) bool {
			return !(config.MapScalar(t, "type") == "http" &&
				strings.EqualFold(config.MapScalar(t, "method"), method) &&
				config.MapScalar(t, "path") == path)
		})
		if removed == 0 {
			return fmt.Errorf("no route %s %s — this project has: %s", method, path, routeList(cfg))
		}
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Printf("%s removed route %s %s\n", ui.OK("✓"), ui.Bold(method), ui.Bold(path))
	printAppliesLive(cfg.Root)
	return nil
}

func runRemoveQueue(_ *cobra.Command, args []string) error {
	name := args[0]
	cfg, err := loadProject()
	if err != nil {
		return err
	}
	if _, ok := cfg.Resources.Queues[name]; !ok {
		return fmt.Errorf("unknown queue %q — this project has: %s",
			name, strings.Join(sortedKeys(cfg.Resources.Queues), ", "))
	}
	for qn, q := range cfg.Resources.Queues {
		if q.DLQ == name {
			return fmt.Errorf("queue %q is the dead-letter queue of %q — remove or rewire %q first", name, qn, qn)
		}
	}

	dropped := 0
	err = config.EditYAML(cfg.Path, func(root *yaml.Node) error {
		queues := config.TopMap(config.TopMap(root, "resources"), "queues")
		if !config.RemoveMapEntry(queues, name) {
			return fmt.Errorf("queue %q not in pulse.yaml", name)
		}
		dropped = config.FilterSeq(config.TopSeq(root, "triggers"), func(t *yaml.Node) bool {
			return !(config.MapScalar(t, "type") == "sqs" && config.MapScalar(t, "queue") == name)
		})
		return nil
	})
	if err != nil {
		return err
	}

	fmt.Printf("%s removed queue %s\n", ui.OK("✓"), ui.Bold(name))
	if dropped > 0 {
		fmt.Printf("  also removed its sqs trigger\n")
	}
	fmt.Println("  " + ui.Hint("messages already stored stay in `.pulse/` — they're ignored without the queue"))
	printAppliesLive(cfg.Root)
	return nil
}

func runRemoveTable(_ *cobra.Command, args []string) error {
	name := args[0]
	cfg, err := loadProject()
	if err != nil {
		return err
	}
	if _, ok := cfg.Resources.Tables[name]; !ok {
		return fmt.Errorf("unknown table %q — this project has: %s",
			name, strings.Join(sortedKeys(cfg.Resources.Tables), ", "))
	}

	var cleaned []string
	err = config.EditYAML(cfg.Path, func(root *yaml.Node) error {
		tables := config.TopMap(config.TopMap(root, "resources"), "tables")
		if !config.RemoveMapEntry(tables, name) {
			return fmt.Errorf("table %q not in pulse.yaml", name)
		}
		// Clean env vars that point at the removed table.
		functions := config.TopMap(root, "functions")
		for fnName, fn := range cfg.Functions {
			for envKey, envVal := range fn.Env {
				if envVal == name {
					env := config.TopMap(config.TopMap(functions, fnName), "env")
					if config.RemoveMapEntry(env, envKey) {
						cleaned = append(cleaned, fmt.Sprintf("%s (%s)", fnName, envKey))
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	fmt.Printf("%s removed table %s\n", ui.OK("✓"), ui.Bold(name))
	for _, c := range cleaned {
		fmt.Printf("  also removed the env wiring in %s\n", c)
	}
	fmt.Println("  " + ui.Hint("its rows stay in `.pulse/` — re-declare the table and they're back"))
	printAppliesLive(cfg.Root)
	return nil
}

func routeList(cfg *config.Config) string {
	var out []string
	for _, t := range cfg.Triggers {
		if t.Type == "http" {
			out = append(out, t.Method+" "+t.Path)
		}
	}
	if len(out) == 0 {
		return "none"
	}
	return strings.Join(out, ", ")
}
