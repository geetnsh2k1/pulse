package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"pulse/internal/config"
	"pulse/internal/templates"
)

var (
	flagTemplate     string
	flagLang         string
	flagListTemplate bool
	flagNoInstall    bool
)

var initCmd = &cobra.Command{
	Use:   "init [name]",
	Short: "Create a new pulse project from a starter template",
	Long: `Create a new pulse project.

  pulse init             no arguments: asks name, template, and language
  pulse init <name>      creates <name> with the default template (hello)
  pulse init --list      shows the available templates

<name> becomes both the folder and the project name; use "." for the
current (empty) directory.`,
	Args: func(_ *cobra.Command, args []string) error {
		if len(args) > 1 {
			return fmt.Errorf("init takes one name, got %d (%s) — flag values need their flag, e.g. --template hello",
				len(args), strings.Join(args, " "))
		}
		return nil
	},
	RunE: runInit,
}

func init() {
	initCmd.Flags().StringVarP(&flagTemplate, "template", "t", "hello", "starter template (see --list)")
	initCmd.Flags().StringVar(&flagLang, "lang", "node", "language: node or python")
	initCmd.Flags().BoolVar(&flagListTemplate, "list", false, "list available templates and exit")
	initCmd.Flags().BoolVar(&flagNoInstall, "no-install", false, "skip automatic dependency installation")
}

func runInit(cmd *cobra.Command, args []string) error {
	// `pulse init -t --list` parses "--list" as -t's value — catch the
	// mistake and teach, instead of failing on a template named "--list".
	if strings.HasPrefix(flagTemplate, "-") {
		return fmt.Errorf("--template needs a template name (you passed %q) — run `pulse init --list` to see them", flagTemplate)
	}
	if strings.HasPrefix(flagLang, "-") {
		return fmt.Errorf("--lang needs a language, node or python (you passed %q)", flagLang)
	}
	if flagListTemplate {
		fmt.Println("available templates:")
		for _, t := range templates.List() {
			name := t.Name
			if len(t.Variants) > 0 {
				name += " (--lang " + strings.Join(t.Variants, "|") + ")"
			}
			fmt.Printf("  %-36s %s\n", name, t.Description)
		}
		return nil
	}

	// --lang only means something for templates that have variants.
	if cmd.Flags().Changed("lang") && len(templates.Variants(flagTemplate)) == 0 {
		return fmt.Errorf("template %q has no language variants — drop the --lang flag", flagTemplate)
	}
	if len(args) == 0 {
		if !stdinIsInteractive() {
			return errors.New("usage: pulse init <name> (or pulse init --list to see templates)")
		}
		wizardName, err := initWizard(cmd)
		if err != nil {
			return err
		}
		args = []string{wizardName}
	}

	name := args[0]
	dst := name
	project := name
	if name == "." {
		abs, err := filepath.Abs(".")
		if err != nil {
			return err
		}
		project = strings.ToLower(filepath.Base(abs))
	}
	if flagChdir != "" {
		dst = filepath.Join(flagChdir, dst)
	}
	if !config.ValidProjectName(project) {
		return fmt.Errorf("%q is not a valid project name — use lowercase letters, digits, and hyphens", project)
	}

	// Refuse to clobber anything that looks like existing work.
	if _, err := os.Stat(filepath.Join(dst, config.FileName)); err == nil {
		return fmt.Errorf("%s already exists in %s", config.FileName, dst)
	}
	if entries, err := os.ReadDir(dst); err == nil {
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), ".") {
				return fmt.Errorf("directory %s is not empty — pulse init wants a fresh directory", dst)
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}

	written, err := templates.Render(flagTemplate, dst, templates.Data{Project: project, Lang: flagLang})
	if err != nil {
		return err
	}

	label := flagTemplate
	if len(templates.Variants(flagTemplate)) > 0 {
		label += " (" + flagLang + ")"
	}
	fmt.Printf("✓ created project %s from template %s (%d files)\n", project, label, len(written))

	if !flagNoInstall {
		installDeps(dst)
	}

	fmt.Println("\nnext steps:")
	if name != "." {
		fmt.Printf("  cd %s\n", name)
	}
	fmt.Println("  pulse start")
	if flagTemplate == "api-and-worker" {
		fmt.Println(`  curl -X POST localhost:3000/orders -H 'content-type: application/json' -d '{"sku":"A1","qty":2}'`)
		fmt.Println(`  curl localhost:3000/orders/<id-from-above>    # → status "processed", via queue + worker + table`)
	} else {
		fmt.Println(`  curl "localhost:3000/hello?name=you"`)
	}
	fmt.Println("\nedit code or pulse.yaml while it runs — changes apply live. `pulse add --help` scaffolds more.")
	return nil
}

// installDeps makes fresh projects runnable with zero manual setup: npm
// install when there's a package.json, a project venv + pip install when
// there's a requirements.txt. Failures degrade to a note, never an error —
// projects still work, the SDK-dependent parts just stay dormant.
func installDeps(dst string) {
	// Absolute everything: exec resolves relative binary paths against
	// cmd.Dir, which double-prefixes otherwise.
	if abs, err := filepath.Abs(dst); err == nil {
		dst = abs
	}
	if _, err := os.Stat(filepath.Join(dst, "package.json")); err == nil {
		if _, lookErr := exec.LookPath("npm"); lookErr != nil {
			fmt.Println("note: npm not found — run `npm install` inside the project to enable the AWS SDK")
		} else {
			fmt.Print("  installing npm dependencies… ")
			cmd := exec.Command("npm", "install", "--no-fund", "--no-audit")
			cmd.Dir = dst
			if out, err := cmd.CombinedOutput(); err != nil {
				fmt.Println("didn't finish")
				fmt.Printf("note: run `npm install` inside the project later (offline?): %s\n", lastLine(out))
			} else {
				fmt.Println("done")
			}
		}
	}

	if _, err := os.Stat(filepath.Join(dst, "requirements.txt")); err == nil {
		py := ""
		for _, c := range []string{"python3.12", "python3", "python"} {
			if p, lookErr := exec.LookPath(c); lookErr == nil {
				py = p
				break
			}
		}
		if py == "" {
			fmt.Println("note: no python found — install Python, then run: python3 -m venv .venv && .venv/bin/pip install -r requirements.txt")
			return
		}
		fmt.Print("  creating .venv and installing python dependencies… ")
		venv := exec.Command(py, "-m", "venv", ".venv")
		venv.Dir = dst
		if out, err := venv.CombinedOutput(); err != nil {
			fmt.Println("didn't finish")
			fmt.Printf("note: create it manually later: python3 -m venv .venv && .venv/bin/pip install -r requirements.txt (%s)\n", lastLine(out))
			return
		}
		pip := exec.Command(filepath.Join(dst, ".venv", "bin", "python"), "-m", "pip", "install", "-q", "-r", "requirements.txt")
		pip.Dir = dst
		if out, err := pip.CombinedOutput(); err != nil {
			fmt.Println("didn't finish")
			fmt.Printf("note: run `.venv/bin/pip install -r requirements.txt` later (offline?) — %s\n", lastLine(out))
			return
		}
		fmt.Println("done")
	}
}

func lastLine(out []byte) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}
