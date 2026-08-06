package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"pulse/internal/ui"
	"pulse/internal/version"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the pulse version",
	Args:  cobra.NoArgs,
	Run: func(_ *cobra.Command, _ []string) {
		wave := ui.Wave()
		fmt.Println(wave[0])
		fmt.Printf("%s   %s %s\n", wave[1], ui.Bold("pulse"), version.Version)
		fmt.Printf("            %s\n", ui.Dim(fmt.Sprintf("%s · %s/%s · your local serverless cloud", runtime.Version(), runtime.GOOS, runtime.GOARCH)))
	},
}
