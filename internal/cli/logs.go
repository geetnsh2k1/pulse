package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"pulse/internal/engine"
	"pulse/internal/logs"
	"pulse/internal/store"
	"pulse/internal/ui"
)

var (
	flagFollow    bool
	flagLogsLimit int
)

var logsCmd = &cobra.Command{
	Use:   "logs <function>",
	Short: "Show a function's recent logs, or follow them live",
	Long: `Show what one of your functions printed, newest last.

<function> is the function's name from pulse.yaml — the same names that
` + "`pulse list`" + `, the start banner, and Tab completion show.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runLogs,
}

func init() {
	logsCmd.Flags().BoolVarP(&flagFollow, "follow", "f", false, "stream new log lines as they arrive (needs a running engine)")
	logsCmd.Flags().IntVarP(&flagLogsLimit, "limit", "n", 50, "how many recent lines to show")
}

func runLogs(cmd *cobra.Command, args []string) error {
	function, err := resolveFunctionArg(cmd, args, "logs", "which function's logs?")
	if err != nil {
		return err
	}

	cfg, err := loadProject()
	if err != nil {
		return err
	}
	if _, ok := cfg.Functions[function]; !ok {
		return fmt.Errorf("unknown function %q — this project has: %s",
			function, strings.Join(cfg.FunctionNames(), ", "))
	}

	info, running := engine.Current(cfg.Root)

	// Recent history: from the engine when it's up (single writer), else
	// straight from the store on disk.
	var recent []logs.Line
	if running {
		recent, err = fetchRecentLogs(info, function, flagLogsLimit)
	} else {
		var st *store.Store
		if st, err = store.Open(cfg.Root); err == nil {
			recent, err = st.RecentLogs(function, flagLogsLimit)
			st.Close()
		}
	}
	if err != nil {
		return err
	}
	for _, l := range recent {
		printLogLine(l)
	}

	if !flagFollow {
		if len(recent) == 0 {
			fmt.Println(ui.Hint(fmt.Sprintf("no logs for %s yet — `pulse invoke %s` and check back", function, function)))
		}
		return nil
	}
	if !running {
		return fmt.Errorf("--follow needs a running engine — start one with `pulse start`")
	}
	fmt.Printf("%s %s %s\n", ui.Dim("── following"), ui.Fn(function), ui.Dim("(Ctrl+C to stop) ──"))
	return followLogs(info, function)
}

func fetchRecentLogs(info *engine.RunInfo, function string, limit int) ([]logs.Line, error) {
	resp, err := http.Get(fmt.Sprintf("%s/api/logs?function=%s&limit=%d",
		info.Addr, url.QueryEscape(function), limit))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var lines []logs.Line
	return lines, json.NewDecoder(resp.Body).Decode(&lines)
}

// followLogs consumes the engine's Server-Sent Events stream until the user
// interrupts or the engine goes away.
func followLogs(info *engine.RunInfo, function string) error {
	resp, err := http.Get(fmt.Sprintf("%s/api/logs/stream?function=%s",
		info.Addr, url.QueryEscape(function)))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var l logs.Line
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &l); err == nil {
			printLogLine(l)
		}
	}
	fmt.Println(ui.Dim("── engine closed the stream ──"))
	return nil
}
