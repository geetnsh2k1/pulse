package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"pulse/internal/config"
	"pulse/internal/templates"
)

var (
	flagTemplate     string
	flagListTemplate bool
)

var initCmd = &cobra.Command{
	Use:   "init <name>",
	Short: "Create a new pulse project from a starter template",
	Long: `Create a new pulse project. <name> becomes the directory and project name;
use "." to initialize the current (empty) directory.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runInit,
}

func init() {
	initCmd.Flags().StringVarP(&flagTemplate, "template", "t", "node-api", "starter template (see --list)")
	initCmd.Flags().BoolVar(&flagListTemplate, "list", false, "list available templates and exit")
}

func runInit(_ *cobra.Command, args []string) error {
	if flagListTemplate {
		fmt.Println("available templates:")
		for _, t := range templates.List() {
			fmt.Printf("  %-16s %s\n", t.Name, t.Description)
		}
		return nil
	}
	if len(args) == 0 {
		return errors.New("usage: pulse init <name> (or pulse init --list to see templates)")
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

	written, err := templates.Render(flagTemplate, dst, templates.Data{Project: project})
	if err != nil {
		return err
	}

	fmt.Printf("✓ created project %s from template %s (%d files)\n\n", project, flagTemplate, len(written))
	fmt.Println("next steps:")
	if name != "." {
		fmt.Printf("  cd %s\n", name)
	}
	fmt.Println("  pulse validate   # check the config")
	fmt.Println("  pulse list       # see functions & triggers")
	fmt.Println("  pulse start      # boot the local environment")
	return nil
}
