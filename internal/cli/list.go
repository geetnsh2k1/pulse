package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
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

	info, running := engine.Current(cfg.Root)
	printResources(cfg, fetchQueueStats(info, running), fetchTableStats(info, running))

	fmt.Println()
	if info, ok := info, running; ok {
		if info.APIAddr != "" {
			fmt.Printf("engine: running (pid %d, api %s, control %s)\n", info.PID, info.APIAddr, info.Addr)
		} else {
			fmt.Printf("engine: running (pid %d, control %s)\n", info.PID, info.Addr)
		}
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

// fetchQueueStats asks a running engine for live queue depths ("" values
// when the engine is stopped).
func fetchQueueStats(info *engine.RunInfo, running bool) map[string]string {
	stats := map[string]string{}
	if !running {
		return stats
	}
	resp, err := http.Get(info.Addr + "/api/queues")
	if err != nil {
		return stats
	}
	defer resp.Body.Close()
	var rows []struct {
		Name     string `json:"name"`
		Visible  int    `json:"visible"`
		InFlight int    `json:"inFlight"`
		Delayed  int    `json:"delayed"`
	}
	if json.NewDecoder(resp.Body).Decode(&rows) != nil {
		return stats
	}
	for _, r := range rows {
		stats[r.Name] = fmt.Sprintf("%d visible, %d in flight, %d delayed", r.Visible, r.InFlight, r.Delayed)
	}
	return stats
}

// fetchTableStats asks a running engine for live table item counts.
func fetchTableStats(info *engine.RunInfo, running bool) map[string]string {
	stats := map[string]string{}
	if !running {
		return stats
	}
	resp, err := http.Get(info.Addr + "/api/tables")
	if err != nil {
		return stats
	}
	defer resp.Body.Close()
	var rows []struct {
		Name  string `json:"name"`
		Items int    `json:"items"`
	}
	if json.NewDecoder(resp.Body).Decode(&rows) != nil {
		return stats
	}
	for _, r := range rows {
		stats[r.Name] = fmt.Sprintf("%d item(s)", r.Items)
	}
	return stats
}

func printResources(cfg *config.Config, queueStats, tableStats map[string]string) {
	r := cfg.Resources
	if len(r.Tables) == 0 && len(r.Buckets) == 0 && len(r.Queues) == 0 && len(r.Topics) == 0 {
		return
	}
	fmt.Println("\nRESOURCES")

	for _, name := range sortedKeys(r.Tables) {
		tb := r.Tables[name]
		key := fmt.Sprintf("pk %s %s", tb.PK.Name, tb.PK.Type)
		if tb.SK != nil {
			key += fmt.Sprintf(", sk %s %s", tb.SK.Name, tb.SK.Type)
		}
		line := fmt.Sprintf("  table   %s (%s)", name, key)
		if depth, ok := tableStats[name]; ok {
			line += " · " + depth
		}
		fmt.Println(line)
	}
	for _, b := range r.Buckets {
		fmt.Printf("  bucket  %s\n", b)
	}
	for _, name := range sortedKeys(r.Queues) {
		q := r.Queues[name]
		line := "  queue   " + name
		if q.DLQ != "" {
			line += fmt.Sprintf(" (dlq %s after %d receives)", q.DLQ, q.MaxReceiveCount)
		}
		if depth, ok := queueStats[name]; ok {
			line += " · " + depth
		}
		fmt.Println(line)
	}
	// Queues that exist at runtime but aren't declared (auto-created on
	// first send) still deserve visibility.
	for _, name := range sortedKeys(queueStats) {
		if _, declared := r.Queues[name]; !declared {
			fmt.Printf("  queue   %s (auto-created) · %s\n", name, queueStats[name])
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
