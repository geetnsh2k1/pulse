package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"pulse/internal/engine"
)

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the running local environment",
	Args:  cobra.NoArgs,
	RunE:  runStop,
}

func runStop(_ *cobra.Command, _ []string) error {
	root, err := findRoot()
	if err != nil {
		return err
	}
	info, ok := engine.Current(root)
	if !ok {
		fmt.Println("no engine running for this project")
		return nil
	}
	fmt.Printf("stopping engine (pid %d)…\n", info.PID)
	if err := engine.RequestShutdown(info, 5*time.Second); err != nil {
		return err
	}
	fmt.Println("✓ stopped")
	return nil
}
