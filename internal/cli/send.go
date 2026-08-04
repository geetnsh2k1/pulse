package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"pulse/internal/engine"
	sqs "pulse/internal/services/sqs"
	"pulse/internal/store"
)

var (
	flagSendEvent string
	flagSendDelay int
)

var sendCmd = &cobra.Command{
	Use:   "send <queue> [message-body]",
	Short: "Put a message on a local queue (triggers its sqs-wired function)",
	Long: `Put a message on a local queue by hand — the async front door.

With the engine running, delivery to the queue's function happens within a
second or two. With it stopped, the message waits in the queue and is
delivered on the next ` + "`pulse start`" + `.`,
	Args: cobra.RangeArgs(1, 2),
	RunE: runSend,
}

func init() {
	sendCmd.Flags().StringVarP(&flagSendEvent, "event", "e", "", `read the message body from a file ("-" for stdin)`)
	sendCmd.Flags().IntVar(&flagSendDelay, "delay", 0, "DelaySeconds before the message becomes visible")
}

func runSend(_ *cobra.Command, args []string) error {
	queue := args[0]

	body, err := resolveSendBody(args)
	if err != nil {
		return err
	}

	cfg, err := loadProject()
	if err != nil {
		return err
	}
	if _, ok := cfg.Resources.Queues[queue]; !ok {
		fmt.Printf("note: %q isn't declared in pulse.yaml — creating it with defaults\n", queue)
	}

	if info, ok := engine.Current(cfg.Root); ok {
		id, err := sendViaEngine(info, queue, body)
		if err != nil {
			return err
		}
		fmt.Printf("✓ queued message %s on %s — the engine will deliver it in a moment\n", shortID(id), queue)
		return nil
	}

	// Engine stopped: write straight into the on-disk queue.
	st, err := store.Open(cfg.Root)
	if err != nil {
		return err
	}
	defer st.Close()
	svc := sqs.New(cfg, st)
	id, apiErr := svc.Send(queue, body, flagSendDelay, nil)
	if apiErr != nil {
		return fmt.Errorf("%s", apiErr.Message)
	}
	fmt.Printf("✓ queued message %s on %s — engine is stopped, it will be delivered on the next `pulse start`\n", shortID(id), queue)
	return nil
}

func resolveSendBody(args []string) (string, error) {
	switch {
	case len(args) == 2 && flagSendEvent != "":
		return "", fmt.Errorf("give the body either as an argument or via --event, not both")
	case len(args) == 2:
		return args[1], nil
	case flagSendEvent == "-":
		b, err := io.ReadAll(os.Stdin)
		return string(b), err
	case flagSendEvent != "":
		b, err := os.ReadFile(flagSendEvent)
		return string(b), err
	}
	return "", fmt.Errorf("no message body — pass it as an argument or via --event file")
}

func sendViaEngine(info *engine.RunInfo, queue, body string) (string, error) {
	payload, _ := json.Marshal(map[string]any{
		"queue": queue, "body": body, "delaySeconds": flagSendDelay,
	})
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(info.Addr+"/api/queues/send", "application/json", bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("calling the engine: %w", err)
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
		return "", fmt.Errorf("%s", e.Error)
	}
	var out struct {
		MessageID string `json:"messageId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.MessageID, nil
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
