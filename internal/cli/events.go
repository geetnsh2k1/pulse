package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/geetnsh2k1/pulse/internal/config"
	"github.com/geetnsh2k1/pulse/internal/engine"
	"github.com/geetnsh2k1/pulse/internal/store"
	"github.com/geetnsh2k1/pulse/internal/ui"
)

// pulse events — every trigger that ever hit a function is recorded (the
// exact payload plus the outcome). `pulse events replay <id>` fires a
// recorded event again, byte for byte, through the function's current code:
// the bug's exact input becomes something you own and can re-run forever.

var (
	flagEventsFn    string
	flagEventsLimit int
)

var eventsCmd = &cobra.Command{
	Use:     "events",
	Aliases: []string{"history"},
	Short:   "List recorded trigger events — the project's history",
	Long: `Every trigger (HTTP request, queue delivery, invoke) is recorded with its
exact event payload and outcome. This lists them, newest first; any of them
can be re-run with ` + "`pulse events replay <id>`" + `.`,
	Args: cobra.NoArgs,
	RunE: runEventsList,
}

var eventsListCmd = &cobra.Command{
	Use:   "list",
	Short: "Same as bare `pulse events`",
	Args:  cobra.NoArgs,
	RunE:  runEventsList,
}

var eventsReplayCmd = &cobra.Command{
	Use:   "replay <event-id>",
	Short: "Re-run a recorded event through the current code",
	Long: `Fire a recorded event again, byte for byte, against the code you have NOW.
Fix a handler, save (hot reload), replay — same exact input, new code.

The id comes from ` + "`pulse events`" + ` (a unique prefix is enough). The replay is
recorded too, so history stays truthful.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runEventsReplay,
}

func init() {
	eventsCmd.Flags().StringVar(&flagEventsFn, "function", "", "only events for this function")
	eventsCmd.Flags().IntVarP(&flagEventsLimit, "limit", "n", 20, "how many to show")
	eventsListCmd.Flags().StringVar(&flagEventsFn, "function", "", "only events for this function")
	eventsListCmd.Flags().IntVarP(&flagEventsLimit, "limit", "n", 20, "how many to show")
	eventsCmd.AddCommand(eventsListCmd, eventsReplayCmd)
}

func runEventsList(_ *cobra.Command, _ []string) error {
	cfg, err := loadProject()
	if err != nil {
		return err
	}

	rows, err := recentEvents(cfg, flagEventsFn, flagEventsLimit)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Println("no events recorded yet — curl a route, `pulse send` a message, or `pulse invoke` something first")
		return nil
	}

	for _, ev := range rows {
		outcome := ev.Status
		if outcome == "" {
			outcome = "?"
		}
		status := ui.OK(outcome)
		if outcome != "success" {
			status = ui.Err(outcome)
		}
		fmt.Printf("  %s  %s  %s %s %s · %s %s\n",
			ui.Bold(fmt.Sprintf("%-8s", shortEventID(ev.ID))), ui.Dim(fmt.Sprintf("%-12s", fmtEventTime(ev.CreatedAt))),
			ui.Cyan(fmt.Sprintf("%-6s", ev.Type)), ui.Dim("→"), ui.Fn(fmt.Sprintf("%-14s", ev.Function)),
			status, ui.Dim(fmt.Sprintf("· %dms", ev.DurationMs)))
	}
	fmt.Println("\n" + ui.Hint("replay any: `pulse events replay <id>` · narrow: `--function <fn>` · more: `-n 50`"))
	return nil
}

func runEventsReplay(cmd *cobra.Command, args []string) error {
	cfg, err := loadProject()
	if err != nil {
		return err
	}

	id := ""
	if len(args) == 1 {
		id = args[0]
	} else {
		id, err = pickEvent(cmd, cfg)
		if err != nil {
			return err
		}
	}

	if info, ok := engine.Current(cfg.Root); ok {
		return replayViaEngine(info, id)
	}
	return replayEphemeral(cfg, id)
}

// pickEvent lets a terminal user choose from recent events; scripts still
// get the teaching error.
func pickEvent(cmd *cobra.Command, cfg *config.Config) (string, error) {
	if !stdinIsInteractive() {
		return "", fmt.Errorf("which event? run `pulse events` and pass an id (a unique prefix is enough)")
	}
	rows, err := recentEvents(cfg, "", 10)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", fmt.Errorf("no events recorded yet — curl a route or `pulse send` something first")
	}
	opts := make([]pickOption, len(rows))
	for i, ev := range rows {
		outcome := ev.Status
		if outcome == "" {
			outcome = "?"
		}
		opts[i] = pickOption{label: shortEventID(ev.ID),
			desc: fmt.Sprintf("%s  %s → %s · %s", fmtEventTime(ev.CreatedAt), ev.Type, ev.Function, outcome)}
	}
	in := promptIn(cmd)
	i, err := askPick(in, cmd.OutOrStdout(), "which event should I replay?", opts, 1)
	if err != nil {
		return "", err
	}
	return rows[i].ID, nil
}

func replayViaEngine(info *engine.RunInfo, id string) error {
	body, _ := json.Marshal(map[string]string{"id": id})
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Post(info.Addr+"/api/replay", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("calling the engine: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = fmt.Sprintf("engine returned HTTP %d", resp.StatusCode)
		}
		return fmt.Errorf("%s", e.Error)
	}

	var out engine.ReplayResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	printReplayHeader(out.Event)
	printInvokeResult(out.Event.Function, out.InvokeResult)
	if out.Status != "success" {
		return fmt.Errorf("replay ended with status %q", out.Status)
	}
	return nil
}

func replayEphemeral(cfg *config.Config, id string) error {
	st, err := store.Open(cfg.Root)
	if err != nil {
		return err
	}
	ev, err := st.EventByPrefix(id)
	st.Close() // release before the ephemeral invoke opens its own handle
	if err != nil {
		return err
	}
	if _, ok := cfg.Functions[ev.Function]; !ok {
		return fmt.Errorf("event %s targets function %q, which is no longer in pulse.yaml",
			shortEventID(ev.ID), ev.Function)
	}

	printReplayHeader(*ev)
	res, err := invokeEphemeral(cfg, ev.Function, "replay", ev.Payload)
	if err != nil {
		return err
	}
	printInvokeResult(ev.Function, res)
	if res.Status != "success" {
		return fmt.Errorf("replay ended with status %q", res.Status)
	}
	return nil
}

// recentEvents reads history from the engine when it's up (single writer),
// else straight from the store on disk — same split as pulse logs.
func recentEvents(cfg *config.Config, function string, limit int) ([]store.EventRow, error) {
	if info, ok := engine.Current(cfg.Root); ok {
		url := fmt.Sprintf("%s/api/events?function=%s&limit=%d", info.Addr, function, limit)
		resp, err := http.Get(url)
		if err != nil {
			return nil, fmt.Errorf("calling the engine: %w", err)
		}
		defer resp.Body.Close()
		var rows []store.EventRow
		return rows, json.NewDecoder(resp.Body).Decode(&rows)
	}

	st, err := store.Open(cfg.Root)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	return st.RecentEvents(function, limit)
}

func printReplayHeader(ev store.EventRow) {
	fmt.Printf("%s replaying %s %s\n\n", ui.Warn("↻"), ui.Bold(shortEventID(ev.ID)),
		ui.Dim(fmt.Sprintf("— %s → %s, originally %s", ev.Type, ev.Function, fmtEventTime(ev.CreatedAt))))
}

func shortEventID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// fmtEventTime keeps today's events short and older ones unambiguous.
func fmtEventTime(unixMs int64) string {
	t := time.UnixMilli(unixMs)
	now := time.Now()
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return t.Format("15:04:05")
	}
	return t.Format("Jan _2 15:04")
}
