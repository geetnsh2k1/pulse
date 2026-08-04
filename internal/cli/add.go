package cli

import (
	"fmt"
	"os"
	"path/filepath"
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
	flagAddRuntime string
	flagAddDir     string
	flagAddFn      string
	flagAddWorker  string
	flagAddPK      string
	flagAddSK      string
	flagAddDLQ     bool
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
	addCmd.AddCommand(addFunctionCmd, addRouteCmd, addQueueCmd, addTableCmd)
}

const nodeStarter = `// %s — a pulse function. Edit freely and save: pulse hot-reloads it.
//
// The same function can serve several triggers; branch on the event shape.
// Everything you console.log shows in the engine console and in
// ` + "`pulse logs %s`" + `.
export const handle = async (event, context) => {
  // Queue batch (an sqs trigger delivered messages)
  if (event.Records) {
    for (const record of event.Records) {
      const job = JSON.parse(record.body || "{}");
      console.log("job received:", JSON.stringify(job));
      // do the work here; push {itemIdentifier: record.messageId} into
      // batchItemFailures below to retry just this message
    }
    return { batchItemFailures: [] };
  }

  // HTTP request (an http trigger routed it here)
  if (event.requestContext?.http) {
    console.log("http " + event.requestContext.http.method + " " + event.rawPath);
    return {
      statusCode: 200,
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ ok: true }),
    };
  }

  // Direct run: pulse invoke %s -d '{...}'
  console.log("invoked with:", JSON.stringify(event).slice(0, 200));
  return { ok: true };
};
`

const pythonStarter = `"""%s — a pulse function. Edit freely and save: pulse hot-reloads it.

The same function can serve several triggers; branch on the event shape.
Everything you print() shows in the engine console and ` + "`pulse logs %s`" + `.
"""

import json


def handle(event, context):
    # Queue batch (an sqs trigger delivered messages)
    if "Records" in event:
        for record in event["Records"]:
            job = json.loads(record.get("body") or "{}")
            print("job received:", json.dumps(job))
            # do the work here; append {"itemIdentifier": record["messageId"]}
            # to batchItemFailures below to retry just this message
        return {"batchItemFailures": []}

    # HTTP request (an http trigger routed it here)
    if "requestContext" in event:
        http = event["requestContext"].get("http", {})
        print("http", http.get("method"), event.get("rawPath"))
        return {
            "statusCode": 200,
            "headers": {"content-type": "application/json"},
            "body": json.dumps({"ok": True}),
        }

    # Direct run: pulse invoke %s -d '{...}'
    print("invoked with:", json.dumps(event)[:200])
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
	handlerFile, starter := "handler.mjs", fmt.Sprintf(nodeStarter, name, name, name)
	if family == "python" {
		handlerFile, starter = "handler.py", fmt.Sprintf(pythonStarter, name, name, name)
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
		"handler": "handler.handle",
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

func runAddTable(_ *cobra.Command, args []string) error {
	name := args[0]
	cfg, err := loadProject()
	if err != nil {
		return err
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

	err = config.EditYAML(cfg.Path, func(root *yaml.Node) error {
		resources := config.TopMap(root, "resources")
		tables := config.TopMap(resources, "tables")
		if config.HasMapKey(tables, name) {
			return fmt.Errorf("table %q already in pulse.yaml", name)
		}
		entry := map[string]any{"pk": pkNode}
		if skNode != nil {
			entry["sk"] = skNode
		}
		return config.SetMapEntry(tables, name, entry)
	})
	if err != nil {
		return err
	}
	fmt.Printf("✓ added table %s (pk %s%s)\n", name, flagAddPK, skSuffix())
	fmt.Println("  your code can use it right away — no schema for the other columns: just write items")
	printAppliesLive(cfg.Root)
	return nil
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
