package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"pulse/internal/engine"
	"pulse/internal/ui"
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
		fmt.Println(ui.Dim("○ no engine running for this project"))
		return nil
	}
	fmt.Println(ui.Dim(fmt.Sprintf("stopping engine (pid %d)…", info.PID)))
	if err := engine.RequestShutdown(info, 5*time.Second); err != nil {
		return err
	}
	fmt.Printf("%s stopped — data, queues, and history are safe in .pulse/\n", ui.OK("✓"))
	return nil
}
