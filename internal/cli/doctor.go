package cli

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/geetnsh2k1/pulse/internal/config"
	"github.com/geetnsh2k1/pulse/internal/engine"
	"github.com/geetnsh2k1/pulse/internal/store"
	"github.com/geetnsh2k1/pulse/internal/ui"
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
		checks = append(checks, check{ok: false, line: "pulse.yaml", fix: err.Error()})
		printChecks(checks)
		fmt.Println()
		return fmt.Errorf("1 problem needs fixing — the other checks need a valid pulse.yaml")
	}
	resources := len(cfg.Resources.Tables) + len(cfg.Resources.Queues)
	checks = append(checks, check{ok: true,
		line: fmt.Sprintf("pulse.yaml valid — %d function(s), %d trigger(s), %d resource(s)",
			len(cfg.Functions), len(cfg.Triggers), resources)})

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

// certified runtime ranges (matching README/PLAN).
var certified = map[string][]string{
	"node":   {"v18.", "v20.", "v22."},
	"python": {"3.9.", "3.10.", "3.11.", "3.12."},
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
				fix: "install Node 18/20/22 — https://nodejs.org"})
		} else if !versionCertified("node", v) {
			out = append(out, check{ok: false, warn: true, line: fmt.Sprintf("node %s found — outside the certified 18/20/22 range", v),
				fix: "works, but behavior may differ from real Lambda"})
		} else {
			out = append(out, check{ok: true, line: "node " + v})
		}
	}
	if families["python"] {
		py, v := findPython(cfg.Root)
		switch {
		case py == "":
			out = append(out, check{ok: false, line: "python not found (project uses a Python runtime)",
				fix: "install Python 3.9–3.12"})
		case !versionCertified("python", v):
			out = append(out, check{ok: false, warn: true, line: fmt.Sprintf("%s (%s) — outside the certified 3.9–3.12 range", v, py),
				fix: "works, but behavior may differ from real Lambda"})
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

func versionCertified(family, version string) bool {
	for _, prefix := range certified[family] {
		if strings.Contains(version, prefix) {
			return true
		}
	}
	return false
}
