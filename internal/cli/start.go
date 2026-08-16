package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/geetnsh2k1/pulse/internal/config"
	"github.com/geetnsh2k1/pulse/internal/engine"
	"github.com/geetnsh2k1/pulse/internal/gateway"
	"github.com/geetnsh2k1/pulse/internal/store"
	"github.com/geetnsh2k1/pulse/internal/ui"
	"github.com/geetnsh2k1/pulse/internal/update"
	"github.com/geetnsh2k1/pulse/internal/version"
)

var flagPort int

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Boot the local environment (foreground; Ctrl+C to stop)",
	Args:  cobra.NoArgs,
	RunE:  runStart,
}

func init() {
	startCmd.Flags().IntVar(&flagPort, "port", 0, "override the api port from pulse.yaml")
}

func runStart(_ *cobra.Command, _ []string) error {
	t0 := time.Now()

	cfg, err := loadProject()
	if err != nil {
		return err
	}
	// Before booting anything: a function whose layers were never fetched into
	// this checkout will die on its first import with a message that names a
	// package, not the real cause.
	if err := requireLayers(cfg); err != nil {
		return err
	}
	if flagPort > 0 {
		cfg.API.Port = flagPort
	}

	if info, ok := engine.Current(cfg.Root); ok {
		return fmt.Errorf("an engine for this project is already running (pid %d, %s) — run `pulse stop` first",
			info.PID, info.Addr)
	}

	st, err := store.Open(cfg.Root)
	if err != nil {
		return err
	}
	defer st.Close()

	eng := engine.New(cfg, st)
	eng.PortOverride = flagPort // must survive pulse.yaml / .env reloads
	eng.OnEvent = func(msg string) { fmt.Println(styleEventLine(msg)) }
	if err := eng.Start(t0); err != nil {
		return err
	}

	for _, warning := range eng.Warnings() {
		fmt.Printf("%s %s\n", ui.Warn("✱"), ui.Dim(warning))
	}

	names := make([]string, 0, len(cfg.Functions))
	for _, n := range cfg.FunctionNames() {
		names = append(names, ui.Fn(n))
	}
	fmt.Printf("%s %s — %s %s\n", ui.AccentBold("⚡ pulse"), ui.Dim(version.Version),
		ui.Bold(cfg.Project), ui.Dim("("+cfg.Region+")"))
	fmt.Printf("  %s  %s\n", ui.Dim("functions"), strings.Join(names, ui.Dim(" · ")))
	if apiURL := eng.APIURL(); apiURL != "" {
		fmt.Printf("  %s        %s\n", ui.Dim("api"), ui.Bold(apiURL))
		label := "routes"
		for _, rt := range eng.Routes() {
			fmt.Printf("  %s  %s %s %s %s\n", ui.Dim(fmt.Sprintf("%-9s", label)),
				ui.Bold(rt.Method), rt.Path, ui.Dim("→"), ui.Fn(rt.Function))
			label = ""
		}
		label = "try"
		for _, tl := range tryLines(eng.Routes(), apiURL, sampleBodies(cfg.Root)) {
			fmt.Printf("  %s  %s\n", ui.Dim(fmt.Sprintf("%-9s", label)), ui.Accent(tl))
			label = ""
		}
	}
	fmt.Printf("  %s        %s %s\n", ui.Dim("aws"), eng.AWSURL(), ui.Dim("("+strings.Join(eng.AWSServices(), ", ")+")"))
	fmt.Printf("  %s    %s\n", ui.Dim("control"), ui.Dim(eng.ControlAddr()))
	fmt.Printf("%s %s\n", ui.OK("ready in "+eng.ReadyIn().Round(time.Millisecond).String()),
		ui.Dim("— code & pulse.yaml changes apply live · Ctrl+C to stop"))

	// A just-imported project boots fine and then fails on its first request,
	// because .env is still placeholders. Say it here, once, where it's cheap
	// to act on — not inside a stack trace.
	if left := cfg.PlaceholderKeys(); len(left) > 0 {
		fmt.Println(ui.Warn("✱ ") + ui.Hint(fmt.Sprintf(
			"%d value(s) in .env still say %s (%s) — fill them in or functions using them will fail",
			len(left), config.Placeholder, strings.Join(left, ", "))))
	}

	// Once-a-day update hint, printed into the console stream whenever the
	// answer arrives — never delays startup, never complains offline.
	go func() {
		if latest, ok := <-update.Check(version.Version); ok {
			fmt.Println(ui.Hint(fmt.Sprintf(
				"pulse %s is available (you have %s) — `brew upgrade pulse` · PULSE_NO_UPDATE_CHECK=1 to silence",
				latest, version.Version)))
		}
	}()

	// Stream every function's output into this console: the terminal
	// running `pulse start` tells the whole story.
	feed, cancelFeed := eng.LogFeed()
	defer cancelFeed()
	go func() {
		for line := range feed {
			if line.Stream == "system" {
				continue // delivery/reload lines already print via OnEvent
			}
			marker := ui.Dim("|")
			if line.Stream == "stderr" {
				marker = ui.Err("!")
			}
			fmt.Printf("  %s %s %s\n", ui.Fn(line.Function), marker, line.Text)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

	select {
	case s := <-sig:
		fmt.Println(ui.Dim(fmt.Sprintf("\nreceived %s, shutting down…", s)))
	case <-eng.ShutdownRequested():
		fmt.Println(ui.Dim("shutdown requested via API, shutting down…"))
	case err := <-eng.ServeErr():
		return fmt.Errorf("control API failed: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := eng.Shutdown(ctx); err != nil {
		return err
	}
	fmt.Printf("%s stopped — see you next time\n", ui.OK("✓"))
	return nil
}

// tryLines renders up to three copy-paste curl commands for the banner —
// writes first (they make something happen), with bodies taken from the
// project's events/*.json samples when one matches the route, so the
// suggested command actually succeeds. Path params get sample values.
func tryLines(routes []gateway.RouteInfo, apiURL string, samples map[string]string) []string {
	host := strings.TrimPrefix(apiURL, "http://")
	var gets, writes []string
	for _, rt := range routes {
		path := samplePath(rt.Path)
		switch rt.Method {
		case "GET", "ANY":
			gets = append(gets, fmt.Sprintf("curl %s%s", host, path))
		default:
			body := samples[rt.Method+" "+rt.Path]
			if body == "" {
				body = `{"key":"value"}`
			}
			writes = append(writes, fmt.Sprintf("curl -X %s %s%s -d '%s'", rt.Method, host, path, body))
		}
	}
	out := append(writes, gets...) // a write first: it makes something happen
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}

// sampleBodies maps "METHOD /path" → request body, read from the project's
// events/*.json sample files (they carry a routeKey).
func sampleBodies(root string) map[string]string {
	out := map[string]string{}
	files, _ := filepath.Glob(filepath.Join(root, "events", "*.json"))
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var ev struct{ RouteKey, Body string }
		if json.Unmarshal(raw, &ev) != nil || ev.RouteKey == "" || !json.Valid([]byte(ev.Body)) {
			continue
		}
		var buf bytes.Buffer
		if json.Compact(&buf, []byte(ev.Body)) == nil && !strings.Contains(buf.String(), "'") {
			out[ev.RouteKey] = buf.String()
		}
	}
	return out
}

// styleEventLine colorizes the engine's console lines by their leading
// glyph; HTTP access lines get their status code colored by class.
func styleEventLine(msg string) string {
	if !ui.Enabled() {
		return msg
	}
	for glyph, style := range map[string]func(string) string{
		"⚙": ui.Cyan, "↻": ui.Warn, "✓": ui.OK, "✱": ui.Warn,
	} {
		if strings.HasPrefix(msg, glyph) {
			return style(glyph) + msg[len(glyph):]
		}
	}
	switch {
	case strings.HasPrefix(msg, "✗"):
		return ui.Err("✗") + ui.Commands(msg[len("✗"):])
	case strings.HasPrefix(msg, "☠"):
		return ui.Err(msg)
	case strings.HasPrefix(msg, "🎉"):
		return ui.AccentBold(msg)
	}
	if m := accessStatus.FindStringSubmatchIndex(msg); m != nil {
		return msg[:m[2]] + ui.Status(msg[m[2]:m[3]]) + msg[m[3]:]
	}
	return msg
}

var accessStatus = regexp.MustCompile(` · (\d{3}) · `)

// samplePath makes a route path pasteable: {id} → 123, {proxy+} → hello.
func samplePath(path string) string {
	out := path
	for {
		i := strings.IndexByte(out, '{')
		j := strings.IndexByte(out, '}')
		if i < 0 || j < i {
			return out
		}
		sample := "123"
		if strings.HasSuffix(out[i:j], "+") {
			sample = "hello"
		}
		out = out[:i] + sample + out[j+1:]
	}
}
