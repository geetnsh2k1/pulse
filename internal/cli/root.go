// Package cli implements the pulse command tree.
package cli

import (
	"bufio"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/geetnsh2k1/pulse/internal/config"
	"github.com/geetnsh2k1/pulse/internal/ui"
)

var (
	flagChdir   string
	flagNoColor bool
)

var rootCmd = &cobra.Command{
	Use:   "pulse",
	Short: "pulse — local AWS serverless development",
	Long: `pulse runs Lambda-based apps entirely on your machine: local invokes,
service mocks, and instant hot reload. No Docker, no AWS account.

New here? Run: pulse tour`,
	SilenceUsage:  true,
	SilenceErrors: true,
	CompletionOptions: cobra.CompletionOptions{
		HiddenDefaultCmd: true,
	},
	// Bare `pulse` inside a project (or in a script) shows help; a human in
	// an empty folder gets pointed at the two front doors instead.
	RunE: func(cmd *cobra.Command, _ []string) error {
		if _, err := config.Find(workDir()); err == nil || !stdinIsInteractive() {
			return cmd.Help()
		}
		wave := ui.Wave()
		fmt.Println(wave[0])
		fmt.Printf("%s   %s %s\n", wave[1], ui.Bold("pulse"), ui.Dim("— no project in this folder yet"))
		in := bufio.NewReader(cmd.InOrStdin())
		i, err := askPick(in, cmd.OutOrStdout(), "what would you like?", []pickOption{
			{label: "tour", desc: "learn pulse hands-on, 5 minutes"},
			{label: "init", desc: "create a new project right here"},
			{label: "help", desc: "just show the commands"},
		}, 1)
		if err != nil {
			return err
		}
		fmt.Println()
		switch i {
		case 0:
			return runTour(tourCmd, nil)
		case 1:
			return runInit(initCmd, nil)
		default:
			return cmd.Help()
		}
	},
}

// Execute runs the CLI; main prints the returned error.
func Execute() error {
	return rootCmd.Execute()
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&flagChdir, "chdir", "C", "",
		"run as if pulse was started in this directory")
	rootCmd.PersistentFlags().BoolVar(&flagNoColor, "no-color", false,
		"disable colored output (also honors NO_COLOR)")
	rootCmd.PersistentPreRun = func(*cobra.Command, []string) {
		if flagNoColor {
			ui.Disable()
		}
	}
	rootCmd.AddCommand(
		initCmd,
		addCmd,
		startCmd,
		stopCmd,
		listCmd,
		validateCmd,
		invokeCmd,
		sendCmd,
		logsCmd,
		eventsCmd,
		versionCmd,
	)
}
