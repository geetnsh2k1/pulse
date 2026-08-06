package cli

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"pulse/internal/config"
	"pulse/internal/engine"
	"pulse/internal/store"
	"pulse/internal/ui"
)

// pulse tour — a hands-on five-minute walkthrough. It builds a real project
// in ./pulse-tour and drives the real CLI as subprocesses, one step per
// Enter press, so everything the learner sees is exactly what pulse does.

var tourCmd = &cobra.Command{
	Use:   "tour",
	Short: "A hands-on 5-minute tour: build and run a tiny app, step by step",
	Long: `Learn pulse by doing. The tour creates a real project in ./pulse-tour and
walks the whole loop — create, start, call over HTTP, add a background
worker, send it a job, and replay history — one Enter press per step.

Nothing is simulated: every step runs the real command it shows you.`,
	Args: cobra.NoArgs,
	RunE: runTour,
}

func init() {
	tourCmd.ValidArgsFunction = cobra.NoFileCompletions
	rootCmd.AddCommand(tourCmd)
}

type tourStep struct {
	explain string // 1–3 lines of why, shown before the command
	display string // the command as the learner would type it
	run     func(*tour) error
}

type tour struct {
	self   string // path to this pulse binary
	dir    string // the sandbox project
	port   int
	in     *bufio.Reader
	engine *exec.Cmd // the running `pulse start`, once started
}

func runTour(cmd *cobra.Command, _ []string) error {
	if !stdinIsInteractive() {
		return fmt.Errorf("the tour is interactive — run it in a terminal")
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}

	dir := "pulse-tour"
	if flagChdir != "" {
		dir = filepath.Join(flagChdir, dir)
	}
	if _, err := os.Stat(filepath.Join(dir, config.FileName)); err == nil {
		return fmt.Errorf("./%s already exists — delete it (or run the tour elsewhere) and try again", dir)
	}

	port, err := freePort()
	if err != nil {
		return err
	}

	t := &tour{self: self, dir: dir, port: port, in: bufio.NewReader(cmd.InOrStdin())}
	defer t.stopEngine()

	wave := ui.Wave()
	fmt.Println(wave[0])
	fmt.Printf("%s   %s %s\n", wave[1], ui.Bold("welcome to pulse"), ui.Dim("— a 5-minute hands-on tour"))
	fmt.Println(ui.Dim("each step shows a command, you press Enter, it runs for real · q quits"))

	steps := t.steps()
	for i, s := range steps {
		fmt.Printf("\n%s %s\n", ui.AccentBold(fmt.Sprintf("[%d/%d]", i+1, len(steps))), s.explain)
		if s.display != "" {
			fmt.Printf("\n    %s\n\n", ui.Accent(s.display))
		}
		if !t.waitEnter() {
			fmt.Println(ui.Dim("\ntour ended — the pulse-tour folder is yours to keep or delete"))
			return nil
		}
		if err := s.run(t); err != nil {
			return fmt.Errorf("tour step failed: %w", err)
		}
	}

	fmt.Printf("\n%s\n", ui.AccentBold("🎓 that's the whole loop"))
	fmt.Println(ui.Hint("you just built an API with a background worker and replayed its history.\n" +
		"start your own project with `pulse init` · the full guide lives in docs/GUIDE.md\n" +
		"(the pulse-tour folder is yours to keep or delete)"))
	return nil
}

func (t *tour) steps() []tourStep {
	return []tourStep{
		{
			explain: "a pulse project is a folder with pulse.yaml (the blueprint) and your\nfunctions. Let's create the smallest one:",
			display: "pulse init pulse-tour --template hello --lang python --no-install",
			run: func(t *tour) error {
				args := []string{"init", "pulse-tour", "--template", "hello", "--lang", "python", "--no-install"}
				if flagChdir != "" {
					args = append(args, "-C", flagChdir)
				}
				return t.pulse(args...)
			},
		},
		{
			explain: "pulse start boots your local cloud: routes answer, queues deliver.\nWatch the banner — it tells you everything that exists:",
			display: "pulse start",
			run:     (*tour).startEngine,
		},
		{
			explain: "the banner listed a route: GET /hello → hello. Call it like any API —\nthe engine console above shows the request the moment it lands:",
			display: fmt.Sprintf("curl \"localhost:%d/hello?name=you\"", t.port),
			run:     (*tour).callHello,
		},
		{
			explain: "now the async half. One command creates a queue, a worker function,\nand the wiring between them:",
			display: "pulse add queue jobs --worker crunch",
			run: func(t *tour) error {
				if err := t.pulse("add", "queue", "jobs", "--worker", "crunch", "-C", t.dir); err != nil {
					return err
				}
				time.Sleep(1500 * time.Millisecond) // let the live apply print before the next step
				return nil
			},
		},
		{
			explain: "drop a message on the queue and watch the console: pulse delivers it\nto the worker within a second — that's a background job:",
			display: `pulse send jobs '{"n":41}'`,
			run: func(t *tour) error {
				if err := t.pulse("send", "jobs", `{"n":41}`, "-C", t.dir); err != nil {
					return err
				}
				time.Sleep(2500 * time.Millisecond) // let the delivery print
				return nil
			},
		},
		{
			explain: "everything that just happened was recorded — payloads included.\nHere's your history, and a replay of the latest event:",
			display: "pulse events   →   pulse events replay <latest>",
			run:     (*tour).showAndReplay,
		},
		{
			explain: "that's it — stop the engine. Data and history survive for next time:",
			display: "pulse stop",
			run: func(t *tour) error {
				err := t.pulse("stop", "-C", t.dir)
				t.engine = nil
				return err
			},
		},
	}
}

// pulse runs this same binary as a subprocess, streaming its output through.
func (t *tour) pulse(args ...string) error {
	cmd := exec.Command(t.self, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = t.childEnv()
	return cmd.Run()
}

func (t *tour) childEnv() []string {
	env := os.Environ()
	if ui.Enabled() {
		env = append(env, "PULSE_FORCE_COLOR=1")
	}
	return env
}

func (t *tour) startEngine() error {
	cmd := exec.Command(t.self, "start", "--port", fmt.Sprint(t.port), "-C", t.dir)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = t.childEnv()
	if err := cmd.Start(); err != nil {
		return err
	}
	t.engine = cmd

	// Wait for the runfile so the next step can talk to the API.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := engine.Current(t.dir); ok {
			time.Sleep(150 * time.Millisecond) // let the banner finish printing
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("the engine didn't come up — is port %d free?", t.port)
}

func (t *tour) stopEngine() {
	if t.engine == nil || t.engine.Process == nil {
		return
	}
	_ = exec.Command(t.self, "stop", "-C", t.dir).Run()
	_ = t.engine.Wait()
	t.engine = nil
}

func (t *tour) callHello() error {
	resp, err := http.Get(fmt.Sprintf("http://localhost:%d/hello?name=you", t.port))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body := make([]byte, 512)
	n, _ := resp.Body.Read(body)
	time.Sleep(200 * time.Millisecond) // let the engine's access line print first
	fmt.Printf("\n  %s %s\n", ui.Status(fmt.Sprint(resp.StatusCode)), string(body[:n]))
	return nil
}

func (t *tour) showAndReplay() error {
	if err := t.pulse("events", "-C", t.dir, "-n", "5"); err != nil {
		return err
	}
	st, err := store.Open(t.dir)
	if err != nil {
		return err
	}
	rows, err := st.RecentEvents("", 1)
	st.Close()
	if err != nil || len(rows) == 0 {
		return fmt.Errorf("no events to replay: %v", err)
	}
	fmt.Println()
	return t.pulse("events", "replay", shortEventID(rows[0].ID), "-C", t.dir)
}

// waitEnter returns false when the learner quits.
func (t *tour) waitEnter() bool {
	fmt.Print(ui.Dim("  press Enter to run it · q to quit › "))
	line, err := t.in.ReadString('\n')
	if err != nil {
		return false
	}
	return strings.TrimSpace(strings.ToLower(line)) != "q"
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
