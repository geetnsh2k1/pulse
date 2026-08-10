package cli

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/geetnsh2k1/pulse/internal/config"
	"github.com/geetnsh2k1/pulse/internal/engine"
	"github.com/geetnsh2k1/pulse/internal/store"
	"github.com/geetnsh2k1/pulse/internal/ui"
	"github.com/geetnsh2k1/pulse/internal/version"
)

// pulse doctor — the "why isn't this working?" command: every environment
// assumption checked, with the exact fix when one fails.

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check your setup and this project — with fixes for what's off",
	Args:  cobra.NoArgs,
	RunE:  runDoctor,
}

func init() {
	doctorCmd.ValidArgsFunction = cobra.NoFileCompletions
	rootCmd.AddCommand(doctorCmd)
}

type check struct {
	ok   bool
	warn bool
	line string
	fix  string
}

func runDoctor(_ *cobra.Command, _ []string) error {
	fmt.Printf("%s %s\n\n", ui.AccentBold("⚡ pulse doctor"), ui.Dim("— checking your setup"))
	var checks []check

	cfg, err := loadProject()
	if err != nil {
		// No project here is not a fault — it's the normal state right after
		// installing. Check the machine instead and point at the next step.
		return runEnvDoctor()
	}
	resources := len(cfg.Resources.Tables) + len(cfg.Resources.Queues)
	checks = append(checks, check{ok: true,
		line: fmt.Sprintf("pulse.yaml valid — %d function(s), %d trigger(s), %d resource(s)",
			len(cfg.Functions), len(cfg.Triggers), resources)})

	// .env is optional, so report it either way — a missing file is fine,
	// but "my variable isn't set" is much easier to diagnose when you know
	// whether pulse saw the file at all. Never print the values.
	if _, err := os.Stat(filepath.Join(cfg.Root, config.DotEnvFile)); err == nil {
		checks = append(checks, check{ok: true,
			line: fmt.Sprintf("%s loaded — %d variable(s) shared by every function", config.DotEnvFile, len(cfg.DotEnv))})
	} else {
		checks = append(checks, check{ok: true,
			line: fmt.Sprintf("no %s (optional — put local secrets there, not in pulse.yaml)", config.DotEnvFile)})
	}

	checks = append(checks, runtimeChecks(cfg)...)
	checks = append(checks, depChecks(cfg)...)

	// Engine + port.
	if info, running := engine.Current(cfg.Root); running {
		checks = append(checks, check{ok: true, line: fmt.Sprintf("engine running (pid %d, api %s)", info.PID, info.APIAddr)})
	} else {
		port := cfg.API.Port
		if port == 0 {
			port = 3000
		}
		if l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err != nil {
			checks = append(checks, check{ok: false, warn: true,
				line: fmt.Sprintf("port %d is taken by another process", port),
				fix:  fmt.Sprintf("`pulse start --port %d` or free the port", port+210)})
		} else {
			l.Close()
			checks = append(checks, check{ok: true, line: fmt.Sprintf("engine stopped · port %d is free", port)})
		}
	}

	// Store writable.
	if st, err := store.Open(cfg.Root); err != nil {
		checks = append(checks, check{ok: false, line: "project state (.pulse/state.db) not openable", fix: err.Error()})
	} else {
		st.Close()
		checks = append(checks, check{ok: true, line: "project state (.pulse/) healthy"})
	}

	printChecks(checks)

	bad, warns := 0, 0
	for _, c := range checks {
		if !c.ok && !c.warn {
			bad++
		} else if c.warn {
			warns++
		}
	}
	fmt.Println()
	switch {
	case bad > 0:
		return fmt.Errorf("%d problem(s) need fixing", bad)
	case warns > 0:
		fmt.Println(ui.Hint(fmt.Sprintf("%d warning(s), nothing blocking — `pulse start` away", warns)))
	default:
		fmt.Println(ui.OK("✓ everything looks good") + ui.Dim(" — `pulse start` away"))
	}
	return nil
}

// runEnvDoctor answers "is this machine ready for pulse?" — everything that
// can be known without a project. Used when there's no pulse.yaml in sight,
// which is exactly where a freshly-installed user stands.
func runEnvDoctor() error {
	var checks []check

	checks = append(checks, check{ok: true, line: "pulse " + version.Version})

	node, nodeOK := runtimeCheck("node", "node", "--version")
	checks = append(checks, node)
	py, pyOK := runtimeCheck("python", "python3", "--version")
	checks = append(checks, py)

	// The update check caches here; if it isn't writable, say so quietly.
	if dir, err := os.UserConfigDir(); err != nil {
		checks = append(checks, check{ok: false, warn: true, line: "no user config dir", fix: err.Error()})
	} else if err := os.MkdirAll(filepath.Join(dir, "pulse"), 0o755); err != nil {
		checks = append(checks, check{ok: false, warn: true,
			line: "config dir not writable", fix: err.Error()})
	} else {
		checks = append(checks, check{ok: true, line: "config dir writable"})
	}

	if l, err := net.Listen("tcp", "127.0.0.1:3000"); err != nil {
		checks = append(checks, check{ok: false, warn: true,
			line: "port 3000 is taken by another process",
			fix:  "not a problem — new projects can use `pulse start --port 3210`"})
	} else {
		l.Close()
		checks = append(checks, check{ok: true, line: "port 3000 is free (the default api port)"})
	}

	printChecks(checks)
	fmt.Println()

	// A machine with neither runtime can't run functions at all; anything
	// else is ready to go.
	if !nodeOK && !pyOK {
		return fmt.Errorf("no supported runtime found — install Node 18+ or Python 3.10+, then run `pulse doctor` again")
	}
	fmt.Println(ui.OK("✓ your machine is ready") +
		ui.Dim(" — no pulse project in this directory yet"))
	fmt.Println(ui.Hint("start one: `pulse init <name>` · or take the tour: `pulse tour`"))
	return nil
}

// runtimeCheck reports whether a language toolchain is present and new
// enough. Missing is a warning here (you may only use the other one).
func runtimeCheck(family, bin, arg string) (check, bool) {
	path, err := exec.LookPath(bin)
	if err != nil {
		return check{ok: false, warn: true,
			line: family + " not found",
			fix:  "install it if you plan to write " + family + " functions",
		}, false
	}
	v, err := toolVersion(path, arg)
	if err != nil {
		return check{ok: false, warn: true, line: family + " found but not runnable", fix: err.Error()}, false
	}
	if !runtimeSupported(family, v) {
		return check{ok: false, warn: true,
			line: fmt.Sprintf("%s — below the supported floor (%s)", v, supportedFloor[family]),
			fix:  "upgrade to " + supportedFloor[family] + " for the tested behavior",
		}, false
	}
	return check{ok: true, line: labelled(family, v)}, true
}

// `node --version` answers "v23.7.0" with no clue what it is; `python3
// --version` says "Python 3.13.2". Name the tool either way.
func labelled(family, v string) string {
	if strings.HasPrefix(strings.ToLower(v), family) {
		return v
	}
	return family + " " + v
}

func printChecks(checks []check) {
	for _, c := range checks {
		glyph := ui.OK("✓")
		if !c.ok {
			glyph = ui.Err("✗")
			if c.warn {
				glyph = ui.Warn("✱")
			}
		}
		fmt.Printf("  %s %s\n", glyph, c.line)
		if c.fix != "" {
			fmt.Printf("    %s\n", ui.Hint("fix: "+c.fix))
		}
	}
}

// Supported floors, not a fixed list of blessed versions: a hardcoded list
// marks every future release "uncertified" until someone edits it (it was
// flagging Python 3.13, which CI tests). Matches README + the CI matrix.
var supportedFloor = map[string]string{
	"node":   "Node 18+",
	"python": "Python 3.10+",
}

// runtimeSupported parses the first x.y in a version string and compares it
// against the floor. Unparsable output is treated as supported — doctor
// should never cry wolf over an unusual `--version` format.
func runtimeSupported(family, version string) bool {
	maj, min, ok := parseMajorMinor(version)
	if !ok {
		return true
	}
	switch family {
	case "node":
		return maj >= 18
	case "python":
		return maj > 3 || (maj == 3 && min >= 10)
	}
	return true
}

func parseMajorMinor(s string) (maj, min int, ok bool) {
	digits := func(r rune) bool { return r >= '0' && r <= '9' }
	i := strings.IndexFunc(s, digits)
	if i < 0 {
		return 0, 0, false
	}
	rest := s[i:]
	end := strings.IndexFunc(rest, func(r rune) bool { return !digits(r) && r != '.' })
	if end > 0 {
		rest = rest[:end]
	}
	parts := strings.Split(rest, ".")
	if len(parts) < 2 {
		return 0, 0, false
	}
	maj, err1 := strconv.Atoi(parts[0])
	min, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return maj, min, true
}

func runtimeChecks(cfg *config.Config) []check {
	var out []check
	families := map[string]bool{}
	for _, fn := range cfg.Functions {
		if strings.HasPrefix(fn.Runtime, "node") {
			families["node"] = true
		} else if strings.HasPrefix(fn.Runtime, "python") {
			families["python"] = true
		}
	}

	if families["node"] {
		if v, err := toolVersion("node", "--version"); err != nil {
			out = append(out, check{ok: false, line: "node not found (project uses a Node runtime)",
				fix: "install Node 18+ — https://nodejs.org"})
		} else if !runtimeSupported("node", v) {
			out = append(out, check{ok: false, warn: true,
				line: fmt.Sprintf("node %s — below the supported floor (%s)", v, supportedFloor["node"]),
				fix:  "upgrade for the behavior pulse tests in CI"})
		} else {
			out = append(out, check{ok: true, line: "node " + v})
		}
	}
	if families["python"] {
		py, v := findPython(cfg.Root)
		switch {
		case py == "":
			out = append(out, check{ok: false, line: "python not found (project uses a Python runtime)",
				fix: "install Python 3.10+"})
		case !runtimeSupported("python", v):
			out = append(out, check{ok: false, warn: true,
				line: fmt.Sprintf("%s (%s) — below the supported floor (%s)", v, py, supportedFloor["python"]),
				fix:  "upgrade for the behavior pulse tests in CI"})
		default:
			out = append(out, check{ok: true, line: fmt.Sprintf("%s (%s)", v, py)})
		}
	}
	return out
}

func depChecks(cfg *config.Config) []check {
	var out []check
	if _, err := os.Stat(filepath.Join(cfg.Root, "package.json")); err == nil {
		if _, err := os.Stat(filepath.Join(cfg.Root, "node_modules")); err == nil {
			out = append(out, check{ok: true, line: "node_modules installed"})
		} else {
			out = append(out, check{ok: false, warn: true, line: "package.json without node_modules",
				fix: "`npm install` (functions needing the AWS SDK will fail until then)"})
		}
	}
	if _, err := os.Stat(filepath.Join(cfg.Root, "requirements.txt")); err == nil {
		if _, err := os.Stat(filepath.Join(cfg.Root, ".venv", "bin", "python")); err == nil {
			out = append(out, check{ok: true, line: ".venv present (pulse finds it automatically)"})
		} else {
			out = append(out, check{ok: false, warn: true, line: "requirements.txt without a .venv",
				fix: "`python3 -m venv .venv && .venv/bin/pip install -r requirements.txt`"})
		}
	}
	return out
}

// findPython prefers the project venv, mirroring the worker runtime rules.
func findPython(root string) (path, version string) {
	venv := filepath.Join(root, ".venv", "bin", "python")
	if _, err := os.Stat(venv); err == nil {
		if v, err := toolVersion(venv, "--version"); err == nil {
			return ".venv/bin/python", v
		}
	}
	for _, c := range []string{"python3.12", "python3"} {
		if p, err := exec.LookPath(c); err == nil {
			if v, err := toolVersion(p, "--version"); err == nil {
				return c, v
			}
		}
	}
	return "", ""
}

func toolVersion(bin string, arg string) (string, error) {
	out, err := exec.Command(bin, arg).CombinedOutput()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
