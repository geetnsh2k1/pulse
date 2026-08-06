package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"pulse/internal/config"
	"pulse/internal/engine"
	"pulse/internal/ui"
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

	info, running := engine.Current(cfg.Root)
	status := ui.Dim("○ stopped — `pulse start` turns it on")
	if running {
		detail := fmt.Sprintf("(pid %d", info.PID)
		if info.APIAddr != "" {
			detail += ", api " + info.APIAddr
		}
		detail += ")"
		status = ui.OK("● running ") + ui.Dim(detail)
	}
	fmt.Printf("%s %s %s · %s\n", ui.AccentBold("⚡"), ui.Bold(cfg.Project), ui.Dim("("+cfg.Region+")"), status)

	fmt.Println("\n" + ui.AccentBold("functions"))
	nameW := 0
	for _, n := range cfg.FunctionNames() {
		nameW = max(nameW, len(n))
	}
	for _, name := range cfg.FunctionNames() {
		fn := cfg.Functions[name]
		fmt.Printf("  %s%s  %s\n", ui.Fn(name), pad(name, nameW),
			ui.Dim(fmt.Sprintf("%s · %s · %ds · %dMB", fn.Runtime, fn.CodeDir, fn.Timeout, fn.Memory)))
	}

	if len(cfg.Triggers) > 0 {
		fmt.Println("\n" + ui.AccentBold("triggers"))
		detailW := 0
		for _, t := range cfg.Triggers {
			detailW = max(detailW, len(plainTrigger(t)))
		}
		for _, t := range cfg.Triggers {
			fmt.Printf("  %s%s %s %s\n", styledTrigger(t), pad(plainTrigger(t), detailW),
				ui.Dim("→"), ui.Fn(t.Function))
		}
	}

	printResources(cfg, fetchQueueStats(info, running), fetchTableStats(info, running))
	return nil
}

// plainTrigger and styledTrigger render the same visible text — the plain
// form measures column width, the styled one prints (ANSI adds no width).
func plainTrigger(t *config.Trigger) string {
	if t.Type == "http" {
		return t.Method + " " + t.Path
	}
	return t.Type + " " + triggerDetails(t)
}

func styledTrigger(t *config.Trigger) string {
	if t.Type == "http" {
		return ui.Bold(t.Method) + " " + t.Path
	}
	return ui.Cyan(t.Type) + " " + triggerDetails(t)
}

func pad(s string, w int) string {
	if len(s) >= w {
		return ""
	}
	return strings.Repeat(" ", w-len(s))
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
	if len(r.Tables) == 0 && len(r.Buckets) == 0 && len(r.Queues) == 0 &&
		len(r.Topics) == 0 && len(queueStats) == 0 {
		return
	}
	fmt.Println("\n" + ui.AccentBold("resources"))

	for _, name := range sortedKeys(r.Tables) {
		tb := r.Tables[name]
		key := fmt.Sprintf("pk %s %s", tb.PK.Name, tb.PK.Type)
		if tb.SK != nil {
			key += fmt.Sprintf(", sk %s %s", tb.SK.Name, tb.SK.Type)
		}
		line := fmt.Sprintf("  %s   %s %s", ui.Dim("table"), ui.Bold(name), ui.Dim("("+key+")"))
		if depth, ok := tableStats[name]; ok {
			line += ui.Dim(" · ") + depth
		}
		fmt.Println(line)
	}
	for _, b := range r.Buckets {
		fmt.Printf("  %s  %s\n", ui.Dim("bucket"), ui.Bold(b))
	}
	for _, name := range sortedKeys(r.Queues) {
		q := r.Queues[name]
		line := fmt.Sprintf("  %s   %s", ui.Dim("queue"), ui.Bold(name))
		if q.DLQ != "" {
			line += ui.Dim(fmt.Sprintf(" (dlq %s after %d receives)", q.DLQ, q.MaxReceiveCount))
		}
		if depth, ok := queueStats[name]; ok {
			line += ui.Dim(" · ") + styleDepth(name, depth)
		}
		fmt.Println(line)
	}
	// Queues that exist at runtime but aren't declared (auto-created on
	// first send) still deserve visibility.
	for _, name := range sortedKeys(queueStats) {
		if _, declared := r.Queues[name]; !declared {
			fmt.Printf("  %s   %s %s %s\n", ui.Dim("queue"), ui.Bold(name),
				ui.Warn("(auto-created)"), ui.Dim("· ")+styleDepth(name, queueStats[name]))
		}
	}
	for _, name := range sortedKeys(r.Topics) {
		tp := r.Topics[name]
		fmt.Printf("  %s   %s %s %s\n", ui.Dim("topic"), ui.Bold(name), ui.Dim("→"), strings.Join(tp.Subscribers, ", "))
	}
}

// styleDepth quietly dims empty queues and shouts about messages stuck in a
// dead-letter queue.
func styleDepth(name, depth string) string {
	if strings.HasSuffix(name, "-dlq") && !strings.HasPrefix(depth, "0 visible") {
		return ui.Err(depth + " ← needs attention")
	}
	if strings.HasPrefix(depth, "0 visible, 0 in flight") {
		return ui.Dim(depth)
	}
	return depth
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
