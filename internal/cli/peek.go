package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/spf13/cobra"

	"github.com/geetnsh2k1/pulse/internal/config"
	"github.com/geetnsh2k1/pulse/internal/engine"
	sqssvc "github.com/geetnsh2k1/pulse/internal/services/sqs"
	"github.com/geetnsh2k1/pulse/internal/store"
	"github.com/geetnsh2k1/pulse/internal/ui"
)

// pulse peek — read a queue's messages without consuming them. Nothing
// about visibility or receive counts changes; the worker still gets them.

var flagPeekLimit int

var peekCmd = &cobra.Command{
	Use:   "peek [queue]",
	Short: "See a queue's waiting messages without consuming them",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runPeek,
}

func init() {
	peekCmd.Flags().IntVarP(&flagPeekLimit, "limit", "n", 10, "messages to show")
	peekCmd.ValidArgsFunction = completeQueueArg
	rootCmd.AddCommand(peekCmd)
}

func runPeek(cmd *cobra.Command, args []string) error {
	cfg, err := loadProject()
	if err != nil {
		return err
	}

	queue := ""
	if len(args) == 1 {
		queue = args[0]
	} else {
		queue, err = pickQueue(cmd, cfg)
		if err != nil {
			return err
		}
	}

	msgs, err := fetchPeek(cfg, queue, flagPeekLimit)
	if err != nil {
		return err
	}
	if len(msgs) == 0 {
		fmt.Println(ui.Hint(fmt.Sprintf("queue %s is empty — `pulse send %s '{...}'` puts something on it", queue, queue)))
		return nil
	}

	fmt.Printf("%s %s\n", ui.AccentBold(queue), ui.Dim(fmt.Sprintf("— %d message(s), oldest first (peeking doesn't consume)", len(msgs))))
	now := time.Now().UnixMilli()
	for _, m := range msgs {
		state := ui.OK("visible")
		switch {
		case m.VisibleAt > now:
			state = ui.Warn(fmt.Sprintf("hidden %ds", (m.VisibleAt-now)/1000))
		case m.ReceiveCount > 0:
			state = ui.Warn(fmt.Sprintf("retried ×%d", m.ReceiveCount))
		}
		body := m.Body
		if len(body) > 90 {
			body = body[:87] + "…"
		}
		fmt.Printf("  %s  %s  %s\n", ui.Bold(shortID(m.ID)), state, ui.Dim(body))
	}
	return nil
}

func pickQueue(cmd *cobra.Command, cfg *config.Config) (string, error) {
	names := sortedKeys(cfg.Resources.Queues)
	if len(names) == 0 {
		return "", fmt.Errorf("no queues declared — `pulse add queue <name> --worker <fn>` creates one")
	}
	if !stdinIsInteractive() {
		return "", fmt.Errorf("which queue? this project has: %s", joinNames(names))
	}
	opts := make([]pickOption, len(names))
	for i, n := range names {
		opts[i] = pickOption{label: n}
	}
	i, err := askPick(promptIn(cmd), cmd.OutOrStdout(), "which queue?", opts, 1)
	if err != nil {
		return "", err
	}
	return names[i], nil
}

func joinNames(names []string) string {
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}

func fetchPeek(cfg *config.Config, queue string, limit int) ([]sqssvc.PeekedMessage, error) {
	if info, ok := engine.Current(cfg.Root); ok {
		u := fmt.Sprintf("%s/api/queues/peek?name=%s&limit=%d", info.Addr, url.QueryEscape(queue), limit)
		resp, err := http.Get(u)
		if err != nil {
			return nil, fmt.Errorf("calling the engine: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			var e struct {
				Error string `json:"error"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&e)
			return nil, fmt.Errorf("%s", e.Error)
		}
		var msgs []sqssvc.PeekedMessage
		return msgs, json.NewDecoder(resp.Body).Decode(&msgs)
	}

	st, err := store.Open(cfg.Root)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	svc := sqssvc.New(cfg, st)
	msgs, apiErr := svc.Peek(queue, limit)
	if apiErr != nil {
		return nil, fmt.Errorf("%s", apiErr.Message)
	}
	return msgs, nil
}
