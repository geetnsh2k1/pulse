package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"pulse/internal/config"
)

var validateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Strictly validate pulse.yaml and report every problem at once",
	Args:  cobra.NoArgs,
	RunE:  runValidate,
}

func runValidate(_ *cobra.Command, _ []string) error {
	cfg, err := loadProject()
	if err != nil {
		return err
	}
	resources := len(cfg.Resources.Tables) + len(cfg.Resources.Buckets) +
		len(cfg.Resources.Queues) + len(cfg.Resources.Topics)
	fmt.Printf("✓ %s valid — %d function(s), %d trigger(s), %d resource(s)\n",
		config.FileName, len(cfg.Functions), len(cfg.Triggers), resources)
	return nil
}
