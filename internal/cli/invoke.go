package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"pulse/internal/awsfacade"
	"pulse/internal/config"
	"pulse/internal/engine"
	"pulse/internal/logs"
	dynamodb "pulse/internal/services/dynamodb"
	sqs "pulse/internal/services/sqs"
	"pulse/internal/store"
	"pulse/internal/workers"
)

var (
	flagEvent string
	flagData  string
)

var invokeCmd = &cobra.Command{
	Use:   "invoke <function>",
	Short: "Invoke a function with a JSON event",
	Long: `Invoke a function locally and print its result and logs.

<function> is the function's name from pulse.yaml — the same names that
` + "`pulse list`" + `, the start banner, and Tab completion show.

Uses the running engine when there is one; otherwise boots an ephemeral
worker just for this invocation — handy for scripts and CI.`,
	Args: oneFunctionArg("invoke"),
	RunE: runInvoke,
}

func init() {
	invokeCmd.Flags().StringVarP(&flagEvent, "event", "e", "", `path to a JSON event file ("-" reads stdin)`)
	invokeCmd.Flags().StringVarP(&flagData, "data", "d", "", "inline JSON event")
}

func runInvoke(_ *cobra.Command, args []string) error {
	function := args[0]

	cfg, err := loadProject()
	if err != nil {
		return err
	}
	if _, ok := cfg.Functions[function]; !ok {
		return fmt.Errorf("unknown function %q — this project has: %s",
			function, strings.Join(cfg.FunctionNames(), ", "))
	}

	payload, err := resolvePayload()
	if err != nil {
		return err
	}

	var res engine.InvokeResult
	if info, ok := engine.Current(cfg.Root); ok {
		res, err = invokeViaEngine(info, cfg, function, payload)
	} else {
		res, err = invokeEphemeral(cfg, function, payload)
	}
	if err != nil {
		return err
	}

	printInvokeResult(function, res)
	if res.Status != "success" {
		return fmt.Errorf("%s invocation ended with status %q", function, res.Status)
	}
	return nil
}

func resolvePayload() ([]byte, error) {
	var payload []byte
	switch {
	case flagData != "":
		payload = []byte(flagData)
	case flagEvent == "-":
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, err
		}
		payload = b
	case flagEvent != "":
		b, err := os.ReadFile(flagEvent)
		if err != nil {
			return nil, err
		}
		payload = b
	default:
		payload = []byte("{}")
	}
	if !json.Valid(payload) {
		return nil, fmt.Errorf("the event is not valid JSON")
	}
	return payload, nil
}

func invokeViaEngine(info *engine.RunInfo, cfg *config.Config, function string, payload []byte) (engine.InvokeResult, error) {
	var out engine.InvokeResult
	body, _ := json.Marshal(map[string]json.RawMessage{
		"function": json.RawMessage(fmt.Sprintf("%q", function)),
		"event":    payload,
	})
	client := &http.Client{Timeout: time.Duration(cfg.Functions[function].Timeout)*time.Second + 15*time.Second}
	resp, err := client.Post(info.Addr+"/api/invoke", "application/json", bytes.NewReader(body))
	if err != nil {
		return out, fmt.Errorf("calling the engine: %w", err)
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
		return out, fmt.Errorf("%s", e.Error)
	}
	err = json.NewDecoder(resp.Body).Decode(&out)
	return out, err
}

// invokeEphemeral boots the worker manager (plus the AWS façade, so SDK
// calls work identically) in-process — no engine, no runfile — runs the
// single invocation, and tears everything down. Messages the function
// enqueues stay in the store; a later `pulse start` delivers them.
func invokeEphemeral(cfg *config.Config, function string, payload []byte) (engine.InvokeResult, error) {
	var out engine.InvokeResult

	st, err := store.Open(cfg.Root)
	if err != nil {
		return out, err
	}
	defer st.Close()

	sink := logs.NewSink(st)

	facade := awsfacade.New()
	svc := sqs.New(cfg, st)
	facade.Register("AmazonSQS", "sqs", svc)
	ddb := dynamodb.New(cfg, st)
	if err := ddb.Init(cfg); err != nil {
		return out, err
	}
	facade.Register("DynamoDB_20120810", "dynamodb", ddb)
	if err := facade.Start(0); err != nil {
		return out, err
	}
	defer facade.Close()
	svc.SetBaseURL(facade.URL)

	mgr := workers.NewManager(cfg, st, sink)
	mgr.SetAWSEndpoint(facade.URL())
	if err := mgr.Start(); err != nil {
		return out, err
	}
	defer mgr.Shutdown()

	for _, warning := range mgr.Warnings() {
		fmt.Fprintf(os.Stderr, "note: %s\n", warning)
	}

	res, err := mgr.Invoke(context.Background(), function, "manual", payload)
	if err != nil {
		return out, err
	}

	out = engine.InvokeResult{
		RequestID:  res.RequestID,
		Status:     res.Status,
		DurationMs: res.DurationMs,
		Logs:       res.Logs,
	}
	if res.Status == "success" {
		out.Result = res.Payload
	} else {
		out.Error = res.Payload
	}
	return out, nil
}

func printInvokeResult(function string, res engine.InvokeResult) {
	glyph := "✓"
	if res.Status != "success" {
		glyph = "✗"
	}
	shortID := res.RequestID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	fmt.Printf("%s %s · %s · %dms · request %s\n", glyph, function, res.Status, res.DurationMs, shortID)

	if len(res.Logs) > 0 {
		fmt.Println()
		for _, l := range res.Logs {
			printLogLine(l)
		}
	}
	fmt.Println()

	if res.Status == "success" {
		fmt.Println(prettyJSON(res.Result))
		return
	}

	var doc struct {
		ErrorMessage string   `json:"errorMessage"`
		ErrorType    string   `json:"errorType"`
		StackTrace   []string `json:"stackTrace"`
	}
	if json.Unmarshal(res.Error, &doc) == nil && doc.ErrorMessage != "" {
		fmt.Printf("%s: %s\n", orDefault(doc.ErrorType, "Error"), doc.ErrorMessage)
		for i, frame := range doc.StackTrace {
			if i >= 15 {
				fmt.Printf("  … %d more frames\n", len(doc.StackTrace)-i)
				break
			}
			if strings.TrimSpace(frame) != "" {
				fmt.Printf("  %s\n", strings.TrimRight(frame, "\r\n"))
			}
		}
	} else {
		fmt.Println(prettyJSON(res.Error))
	}
}

func printLogLine(l logs.Line) {
	ts := time.UnixMilli(l.TS).Format("15:04:05.000")
	fmt.Printf("  %s  %-6s  %s\n", ts, l.Stream, l.Text)
}

func prettyJSON(raw []byte) string {
	if len(raw) == 0 {
		return "null"
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
