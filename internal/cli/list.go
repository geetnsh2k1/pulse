package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"pulse/internal/config"
	"pulse/internal/engine"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "Show the project's functions, triggers, and resources",
	Args:  cobra.NoArgs,
	RunE:  runList,
}

func runList(_ *cobra.Command, _ []string) error {
	cfg, err := loadProject()
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "FUNCTION\tRUNTIME\tHANDLER\tCODE\tTIMEOUT\tMEMORY")
	for _, name := range cfg.FunctionNames() {
		fn := cfg.Functions[name]
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%ds\t%dMB\n",
			name, fn.Runtime, fn.Handler, fn.CodeDir, fn.Timeout, fn.Memory)
	}
	w.Flush()

	if len(cfg.Triggers) > 0 {
		fmt.Println()
		w = tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(w, "TRIGGER\tDETAILS\tFUNCTION")
		for _, t := range cfg.Triggers {
			fmt.Fprintf(w, "%s\t%s\t→ %s\n", t.Type, triggerDetails(t), t.Function)
		}
		w.Flush()
	}

	printResources(cfg)

	fmt.Println()
	if info, ok := engine.Current(cfg.Root); ok {
		fmt.Printf("engine: running (pid %d, %s)\n", info.PID, info.Addr)
	} else {
		fmt.Println("engine: stopped")
	}
	return nil
}

func triggerDetails(t *config.Trigger) string {
	switch t.Type {
	case "http":
		return t.Method + " " + t.Path
	case "sqs":
		return fmt.Sprintf("%s (batch %d)", t.Queue, t.BatchSize)
	case "sns":
		return t.Topic
	case "s3":
		return fmt.Sprintf("%s [%s]", t.Bucket, strings.Join(t.Events, ","))
	case "dynamodb-stream":
		return t.Table
	}
	return ""
}

func printResources(cfg *config.Config) {
	r := cfg.Resources
	if len(r.Tables) == 0 && len(r.Buckets) == 0 && len(r.Queues) == 0 && len(r.Topics) == 0 {
		return
	}
	fmt.Println("\nRESOURCES")

	for _, name := range sortedKeys(r.Tables) {
		tb := r.Tables[name]
		streams := "streams off"
		if tb.Streams {
			streams = "streams on"
		}
		key := fmt.Sprintf("pk %s %s", tb.PK.Name, tb.PK.Type)
		if tb.SK != nil {
			key += fmt.Sprintf(", sk %s %s", tb.SK.Name, tb.SK.Type)
		}
		fmt.Printf("  table   %s (%s, %s)\n", name, key, streams)
	}
	for _, b := range r.Buckets {
		fmt.Printf("  bucket  %s\n", b)
	}
	for _, name := range sortedKeys(r.Queues) {
		q := r.Queues[name]
		if q.DLQ != "" {
			fmt.Printf("  queue   %s (dlq %s after %d receives)\n", name, q.DLQ, q.MaxReceiveCount)
		} else {
			fmt.Printf("  queue   %s\n", name)
		}
	}
	for _, name := range sortedKeys(r.Topics) {
		tp := r.Topics[name]
		fmt.Printf("  topic   %s → %s\n", name, strings.Join(tp.Subscribers, ", "))
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
