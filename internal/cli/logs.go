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
	flagLogsGrep  string
	flagLogsReq   string
)

var logsCmd = &cobra.Command{
	Use:   "logs <function>",
	Short: "Show a function's recent logs, or follow them live",
	Long: `Show what one of your functions printed, newest last.

<function> is the function's name from pulse.yaml — the same names that
` + "`pulse list`" + `, the start banner, and Tab completion show.

--grep finds text in a much larger window (case-insensitive). --request
tells one request's whole story: its event, logs, and result.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runLogs,
}

func init() {
	logsCmd.Flags().BoolVarP(&flagFollow, "follow", "f", false, "stream new log lines as they arrive (needs a running engine)")
	logsCmd.Flags().IntVarP(&flagLogsLimit, "limit", "n", 50, "how many recent lines to show")
	logsCmd.Flags().StringVar(&flagLogsGrep, "grep", "", "only lines containing this text (searches the last 1000)")
	logsCmd.Flags().StringVar(&flagLogsReq, "request", "", "show one request's whole story by id (prefix ok)")
}

func runLogs(cmd *cobra.Command, args []string) error {
	if flagLogsReq != "" {
		return showRequestStory(flagLogsReq)
	}
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
	// straight from the store on disk. --grep searches a much larger window.
	limit := flagLogsLimit
	if flagLogsGrep != "" && !cmd.Flags().Changed("limit") {
		limit = 1000
	}
	var recent []logs.Line
	if running {
		recent, err = fetchRecentLogs(info, function, limit)
	} else {
		var st *store.Store
		if st, err = store.Open(cfg.Root); err == nil {
			recent, err = st.RecentLogs(function, limit)
			st.Close()
		}
	}
	if err != nil {
		return err
	}
	recent = grepLines(recent, flagLogsGrep)
	for _, l := range recent {
		printLogLine(l)
	}

	if !flagFollow {
		if len(recent) == 0 && flagLogsGrep != "" {
			fmt.Println(ui.Hint(fmt.Sprintf("nothing matching %q in the last %d lines", flagLogsGrep, limit)))
		} else if len(recent) == 0 {
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
			if flagLogsGrep != "" && !strings.Contains(strings.ToLower(l.Text), strings.ToLower(flagLogsGrep)) {
				continue
			}
			printLogLine(l)
		}
	}
	fmt.Println(ui.Dim("── engine closed the stream ──"))
	return nil
}

// grepLines keeps lines containing needle, case-insensitively.
func grepLines(lines []logs.Line, needle string) []logs.Line {
	if needle == "" {
		return lines
	}
	n := strings.ToLower(needle)
	var out []logs.Line
	for _, l := range lines {
		if strings.Contains(strings.ToLower(l.Text), n) {
			out = append(out, l)
		}
	}
	return out
}

// showRequestStory prints one request end to end: what arrived, what the
// function said, and what came back.
func showRequestStory(id string) error {
	cfg, err := loadProject()
	if err != nil {
		return err
	}

	var story *store.RequestStory
	if info, ok := engine.Current(cfg.Root); ok {
		resp, herr := http.Get(info.Addr + "/api/request?id=" + url.QueryEscape(id))
		if herr != nil {
			return fmt.Errorf("calling the engine: %w", herr)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			var e struct {
				Error string `json:"error"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&e)
			return fmt.Errorf("%s", e.Error)
		}
		story = &store.RequestStory{}
		if err := json.NewDecoder(resp.Body).Decode(story); err != nil {
			return err
		}
	} else {
		st, serr := store.Open(cfg.Root)
		if serr != nil {
			return serr
		}
		story, err = st.RequestByPrefix(id)
		st.Close()
		if err != nil {
			return err
		}
	}

	inv := story.Invocation
	status := ui.OK(inv.Status)
	if inv.Status != "success" {
		status = ui.Err(inv.Status)
	}
	fmt.Printf("%s %s %s %s %s · %s %s\n", ui.AccentBold("⚡ request"), ui.Bold(shortEventID(inv.ID)),
		ui.Cyan(inv.Source), ui.Dim("→"), ui.Fn(inv.Function), status,
		ui.Dim(fmt.Sprintf("· %dms · %s", inv.DurationMs, fmtEventTime(inv.StartedAt))))

	fmt.Println("\n" + ui.AccentBold("event"))
	printClipped(prettyJSON(story.Event), 14)

	fmt.Println("\n" + ui.AccentBold("logs"))
	if len(story.Logs) == 0 {
		fmt.Println(ui.Dim("  (the function printed nothing)"))
	}
	for _, l := range story.Logs {
		printLogLine(l)
	}

	if inv.Status == "success" {
		fmt.Println("\n" + ui.AccentBold("result"))
		printClipped(prettyJSON(story.Result), 14)
	} else {
		fmt.Println("\n" + ui.AccentBold("error"))
		if inv.Error != "" {
			fmt.Println("  " + ui.Err(inv.Error))
		} else {
			printClipped(prettyJSON(story.Result), 14)
		}
	}

	fmt.Println("\n" + ui.Hint("re-run it against your current code: `pulse events replay "+shortEventID(inv.ID)+"`"))
	return nil
}

// printClipped indents text and cuts it after maxLines, saying what's left.
func printClipped(text string, maxLines int) {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		if i == maxLines {
			fmt.Println(ui.Dim(fmt.Sprintf("  … %d more line(s)", len(lines)-i)))
			return
		}
		fmt.Println("  " + l)
	}
}
