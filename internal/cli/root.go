// Package cli implements the pulse command tree.
package cli

import (
	"github.com/spf13/cobra"
)

var flagChdir string

var rootCmd = &cobra.Command{
	Use:   "pulse",
	Short: "pulse — local AWS serverless development",
	Long: `pulse runs Lambda-based apps entirely on your machine: local invokes,
service mocks, and instant hot reload. No Docker, no AWS account.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	CompletionOptions: cobra.CompletionOptions{
		HiddenDefaultCmd: true,
	},
}

// Execute runs the CLI; main prints the returned error.
func Execute() error {
	return rootCmd.Execute()
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&flagChdir, "chdir", "C", "",
		"run as if pulse was started in this directory")
	rootCmd.AddCommand(
		initCmd,
		startCmd,
		stopCmd,
		listCmd,
		validateCmd,
		invokeCmd,
		logsCmd,
		versionCmd,
	)
}
