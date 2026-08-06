package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"pulse/internal/config"
	"pulse/internal/engine"
)

// pulse add <thing> — scaffolding that edits pulse.yaml for you (comments
// survive; the result is validated before anything is written) and, for
// functions, creates a ready-to-run handler file. With the engine running,
// every addition applies live.

var addCmd = &cobra.Command{
	Use:   "add",
	Short: "Add functions, routes, queues, or tables to the project",
	Long: `Add pieces to pulse.yaml without editing it by hand. The file is edited
surgically (your comments survive), validated, and — if the engine is
running — applied live.`,
}

var (
	flagAddRuntime  string
	flagAddDir      string
	flagAddFn       string
	flagAddWorker   string
	flagAddPK       string
	flagAddSK       string
	flagAddDLQ      bool
	flagAddTableFns []string
)

var addFunctionCmd = &cobra.Command{
	Use:   "function <name>",
	Short: "Create a function: handler file + pulse.yaml entry",
	Args:  cobra.ExactArgs(1),
	RunE:  runAddFunction,
}

var addRouteCmd = &cobra.Command{
	Use:   "route <METHOD> <path>",
	Short: "Wire an HTTP route to a function",
	Args:  cobra.ExactArgs(2),
	RunE:  runAddRoute,
}

var addQueueCmd = &cobra.Command{
	Use:   "queue <name>",
	Short: "Declare a queue (optionally with a DLQ and a worker function)",
	Args:  cobra.ExactArgs(1),
	RunE:  runAddQueue,
}

var addTableCmd = &cobra.Command{
	Use:   "table <name>",
	Short: "Declare a DynamoDB table",
	Args:  cobra.ExactArgs(1),
	RunE:  runAddTable,
}

func init() {
	addFunctionCmd.Flags().StringVar(&flagAddRuntime, "runtime", "", "node | python | a full runtime id (default: match the project)")
	addFunctionCmd.Flags().StringVar(&flagAddDir, "dir", "", "code directory (default services/<name>)")
	addRouteCmd.Flags().StringVar(&flagAddFn, "function", "", "function to invoke (required)")
	_ = addRouteCmd.MarkFlagRequired("function")
	addQueueCmd.Flags().StringVar(&flagAddWorker, "worker", "", "also wire an sqs trigger to this function")
	addQueueCmd.Flags().BoolVar(&flagAddDLQ, "dlq", false, "also create <name>-dlq with maxReceiveCount 3")
	addTableCmd.Flags().StringVar(&flagAddPK, "pk", "id", "partition key, name[:TYPE] (types: S, N, B)")
	addTableCmd.Flags().StringVar(&flagAddSK, "sk", "", "optional sort key, name[:TYPE]")
	addTableCmd.Flags().StringArrayVar(&flagAddTableFns, "function", nil, "wire the table name into this function's env (repeatable)")
	addCmd.AddCommand(addFunctionCmd, addRouteCmd, addQueueCmd, addTableCmd)
}

const nodeStarter = `// %s — edit this file and save; pulse hot-reloads it.
// "event" is whatever triggered you: an HTTP request, a queue batch, or
// the JSON you pass to pulse invoke. Logs: ` + "`pulse logs %s`" + `
export const handler = async (event, context) => {
  console.log("received:", JSON.stringify(event));
  return { ok: true };
};
`

const pythonStarter = `"""%s — edit this file and save; pulse hot-reloads it.

"event" is whatever triggered you: an HTTP request, a queue batch, or the
JSON you pass to pulse invoke. Logs: ` + "`pulse logs %s`" + `
"""

import json


def handler(event, context):
    print("received:", json.dumps(event))
    return {"ok": True}
`

// scaffoldedFn describes a freshly created function's files.
type scaffoldedFn struct {
	runtime     string
	dir         string
	handlerFile string
}

// scaffoldFunctionFiles creates the code directory and a commented starter
// handler (skipping the file if one already exists).
func scaffoldFunctionFiles(cfg *config.Config, name, runtimeFlag, dirFlag string) (*scaffoldedFn, error) {
	runtime, family, err := pickRuntime(cfg, runtimeFlag)
	if err != nil {
		return nil, err
	}
	dir := dirFlag
	if dir == "" {
		dir = filepath.Join("services", name)
	}
	absDir := filepath.Join(cfg.Root, dir)
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		return nil, err
	}
	handlerFile, starter := "handler.mjs", fmt.Sprintf(nodeStarter, name, name)
	if family == "python" {
		handlerFile, starter = "handler.py", fmt.Sprintf(pythonStarter, name, name)
	}
	handlerPath := filepath.Join(absDir, handlerFile)
	if _, err := os.Stat(handlerPath); os.IsNotExist(err) {
		if err := os.WriteFile(handlerPath, []byte(starter), 0o644); err != nil {
			return nil, err
		}
	}
	return &scaffoldedFn{runtime: runtime, dir: dir, handlerFile: handlerFile}, nil
}

func (s *scaffoldedFn) yamlEntry() map[string]any {
	return map[string]any{
		"runtime": s.runtime,
		"handler": "handler.handler",
		"codeDir": s.dir,
	}
}

func runAddFunction(_ *cobra.Command, args []string) error {
	name := args[0]
	cfg, err := loadProject()
	if err != nil {
		return err
	}
	if _, exists := cfg.Functions[name]; exists {
		return fmt.Errorf("function %q already exists", name)
	}

	fn, err := scaffoldFunctionFiles(cfg, name, flagAddRuntime, flagAddDir)
	if err != nil {
		return err
	}
	runtime, dir, handlerFile := fn.runtime, fn.dir, fn.handlerFile

	err = config.EditYAML(cfg.Path, func(root *yaml.Node) error {
		functions := config.TopMap(root, "functions")
		if config.HasMapKey(functions, name) {
			return fmt.Errorf("function %q already in pulse.yaml", name)
		}
		return config.SetMapEntry(functions, name, fn.yamlEntry())
	})
	if err != nil {
		return err
	}

	fmt.Printf("✓ added function %s (%s)\n", name, runtime)
	fmt.Printf("  code   %s\n", filepath.Join(dir, handlerFile))
	fmt.Printf("  try    pulse invoke %s -d '{\"hello\":1}'\n", name)
	fmt.Printf("  wire   pulse add route GET /%s --function %s   ·   pulse add queue %s-jobs --worker %s\n", name, name, name, name)
	printAppliesLive(cfg.Root)
	return nil
}

func runAddRoute(_ *cobra.Command, args []string) error {
	method := strings.ToUpper(args[0])
	path := args[1]
	cfg, err := loadProject()
	if err != nil {
		return err
	}
	if _, ok := cfg.Functions[flagAddFn]; !ok {
		return fmt.Errorf("unknown function %q — this project has: %s", flagAddFn, strings.Join(cfg.FunctionNames(), ", "))
	}

	err = config.EditYAML(cfg.Path, func(root *yaml.Node) error {
		return config.AppendSeq(config.TopSeq(root, "triggers"), map[string]any{
			"type": "http", "method": method, "path": path, "function": flagAddFn,
		})
	})
	if err != nil {
		return err
	}
	fmt.Printf("✓ added route %s %s → %s\n", method, path, flagAddFn)
	hint := strings.ReplaceAll(strings.ReplaceAll(path, "{", "<"), "}", ">")
	fmt.Printf("  try    curl -X %s localhost:3000%s\n", method, hint)
	printAppliesLive(cfg.Root)
	return nil
}

func runAddQueue(_ *cobra.Command, args []string) error {
	name := args[0]
	cfg, err := loadProject()
	if err != nil {
		return err
	}

	// --worker names the function that processes this queue. If it doesn't
	// exist yet, scaffold it too — one command, whole pipeline.
	var newWorker *scaffoldedFn
	workerDir := ""
	if flagAddWorker != "" {
		if existing, ok := cfg.Functions[flagAddWorker]; ok {
			workerDir = existing.CodeDir
		} else {
			newWorker, err = scaffoldFunctionFiles(cfg, flagAddWorker, "", "")
			if err != nil {
				return err
			}
			workerDir = newWorker.dir
		}
	}

	err = config.EditYAML(cfg.Path, func(root *yaml.Node) error {
		if newWorker != nil {
			functions := config.TopMap(root, "functions")
			if !config.HasMapKey(functions, flagAddWorker) {
				if err := config.SetMapEntry(functions, flagAddWorker, newWorker.yamlEntry()); err != nil {
					return err
				}
			}
		}
		resources := config.TopMap(root, "resources")
		queues := config.TopMap(resources, "queues")
		if config.HasMapKey(queues, name) {
			return fmt.Errorf("queue %q already in pulse.yaml", name)
		}
		if flagAddDLQ {
			if err := config.SetMapEntry(queues, name, map[string]any{
				"dlq": name + "-dlq", "maxReceiveCount": 3,
			}); err != nil {
				return err
			}
			if !config.HasMapKey(queues, name+"-dlq") {
				if err := config.SetMapEntry(queues, name+"-dlq", map[string]any{}); err != nil {
					return err
				}
			}
		} else if err := config.SetMapEntry(queues, name, map[string]any{}); err != nil {
			return err
		}
		if flagAddWorker != "" {
			return config.AppendSeq(config.TopSeq(root, "triggers"), map[string]any{
				"type": "sqs", "queue": name, "function": flagAddWorker,
			})
		}
		return nil
	})
	if err != nil {
		return err
	}

	fmt.Printf("✓ added queue %s", name)
	if flagAddDLQ {
		fmt.Printf(" (dlq %s-dlq)", name)
	}
	if flagAddWorker != "" {
		fmt.Printf(" → %s", flagAddWorker)
	}
	fmt.Println()
	if newWorker != nil {
		fmt.Printf("  also created function %s — its handler is %s\n",
			flagAddWorker, filepath.Join(newWorker.dir, newWorker.handlerFile))
	} else if workerDir != "" {
		fmt.Printf("  messages are handled by the existing function %s (%s)\n", flagAddWorker, workerDir)
	}
	fmt.Printf("  try    pulse send %s '{\"hello\":1}'   (needs `pulse start` running to deliver)\n", name)
	fmt.Printf("  watch  pulse logs %s -f\n", flagAddWorker)
	printAppliesLive(cfg.Root)
	return nil
}

func runAddTable(cmd *cobra.Command, args []string) error {
	name := args[0]
	cfg, err := loadProject()
	if err != nil {
		return err
	}

	// --function wires the table's name into each function's env — the one
	// piece of glue code needs. Validate the names before touching yaml.
	for _, fn := range flagAddTableFns {
		if _, ok := cfg.Functions[fn]; !ok {
			return fmt.Errorf("unknown function %q — functions in this project: %s",
				fn, strings.Join(functionNames(cfg), ", "))
		}
	}

	_, tableExists := cfg.Resources.Tables[name]
	if tableExists {
		if cmd.Flags().Changed("pk") || flagAddSK != "" {
			return fmt.Errorf("table %q already in pulse.yaml — its key can't be changed here, edit the file directly", name)
		}
		if len(flagAddTableFns) == 0 {
			return fmt.Errorf("table %q already in pulse.yaml", name)
		}
	}

	pkNode, err := keyNode(flagAddPK)
	if err != nil {
		return fmt.Errorf("--pk: %w", err)
	}
	var skNode any
	if flagAddSK != "" {
		skNode, err = keyNode(flagAddSK)
		if err != nil {
			return fmt.Errorf("--sk: %w", err)
		}
	}

	envName := envVarForTable(name)
	var wired, skipped []string
	err = config.EditYAML(cfg.Path, func(root *yaml.Node) error {
		if !tableExists {
			resources := config.TopMap(root, "resources")
			tables := config.TopMap(resources, "tables")
			entry := map[string]any{"pk": pkNode}
			if skNode != nil {
				entry["sk"] = skNode
			}
			if err := config.SetMapEntry(tables, name, entry); err != nil {
				return err
			}
		}
		functions := config.TopMap(root, "functions")
		for _, fn := range flagAddTableFns {
			// Already pointing at this table under any env name? Done.
			if existing := envVarPointingAt(cfg.Functions[fn].Env, name); existing != "" {
				skipped = append(skipped, fmt.Sprintf("%s already has %s=%s", fn, existing, name))
				continue
			}
			env := config.TopMap(config.TopMap(functions, fn), "env")
			if err := config.SetMapEntry(env, envName, name); err != nil {
				return err
			}
			wired = append(wired, fn)
		}
		return nil
	})
	if err != nil {
		return err
	}

	if tableExists {
		fmt.Printf("✱ table %s already declared — wiring env only\n", name)
	} else {
		fmt.Printf("✓ added table %s (pk %s%s)\n", name, flagAddPK, skSuffix())
		fmt.Println("  your code can use it right away — no schema for the other columns: just write items")
	}
	for _, fn := range wired {
		fmt.Printf("  wired  %s env %s=%s\n", fn, envName, name)
	}
	for _, s := range skipped {
		fmt.Printf("  ✱ %s — skipped\n", s)
	}
	if len(wired) > 0 {
		fmt.Printf("  code   %s\n", tableCodeHint(cfg, wired[0], envName))
	}
	printAppliesLive(cfg.Root)
	return nil
}

// envVarForTable derives the conventional env var name: customers →
// CUSTOMERS_TABLE, order-events → ORDER_EVENTS_TABLE.
func envVarForTable(table string) string {
	up := strings.ToUpper(table)
	up = strings.Map(func(r rune) rune {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, up)
	return up + "_TABLE"
}

// envVarPointingAt returns the name of an env var whose value is already the
// table name, or "".
func envVarPointingAt(env map[string]string, table string) string {
	for k, v := range env {
		if v == table {
			return k
		}
	}
	return ""
}

func functionNames(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.Functions))
	for n := range cfg.Functions {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// tableCodeHint shows the one line of code that reads the wired env var, in
// the language of the function it was wired into.
func tableCodeHint(cfg *config.Config, fn, envName string) string {
	if f, ok := cfg.Functions[fn]; ok && strings.HasPrefix(f.Runtime, "python") {
		return fmt.Sprintf(`table = boto3.resource("dynamodb").Table(os.environ[%q])`, envName)
	}
	return fmt.Sprintf(`const table = process.env.%s`, envName)
}

func skSuffix() string {
	if flagAddSK == "" {
		return ""
	}
	return ", sk " + flagAddSK
}

// keyNode turns "id" / "seq:N" into the shorthand string or the full map.
func keyNode(spec string) (any, error) {
	name, typ, hasType := strings.Cut(spec, ":")
	if name == "" {
		return nil, fmt.Errorf("empty key name")
	}
	if !hasType || typ == "S" {
		return name, nil // shorthand form, type defaults to S
	}
	if typ != "N" && typ != "B" {
		return nil, fmt.Errorf("type %q is not valid (S, N, or B)", typ)
	}
	return map[string]any{"name": name, "type": typ}, nil
}

// pickRuntime resolves --runtime shorthands, defaulting to the project's
// dominant family.
func pickRuntime(cfg *config.Config, flag string) (runtime, family string, err error) {
	switch flag {
	case "node", "nodejs":
		return "nodejs20.x", "node", nil
	case "python", "py":
		return "python3.12", "python", nil
	case "":
		// Match what the project already uses.
		for _, name := range cfg.FunctionNames() {
			fam := config.RuntimeFamily(cfg.Functions[name].Runtime)
			return cfg.Functions[name].Runtime, fam, nil
		}
		return "nodejs20.x", "node", nil
	}
	fam := config.RuntimeFamily(flag)
	if fam == "" {
		return "", "", fmt.Errorf("unknown runtime %q — use node, python, or a full id like nodejs20.x / python3.12", flag)
	}
	return flag, fam, nil
}

func printAppliesLive(root string) {
	if _, running := engine.Current(root); running {
		fmt.Println("  the running engine is applying this now")
	} else {
		fmt.Println("  applies when you `pulse start`")
	}
}
