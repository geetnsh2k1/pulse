package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"pulse/internal/version"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the pulse version",
	Args:  cobra.NoArgs,
	Run: func(_ *cobra.Command, _ []string) {
		fmt.Printf("pulse %s (%s, %s/%s)\n",
			version.Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	},
}
