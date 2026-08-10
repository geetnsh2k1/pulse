package cli

import (
	"fmt"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/geetnsh2k1/pulse/internal/ui"
	"github.com/geetnsh2k1/pulse/internal/update"
	"github.com/geetnsh2k1/pulse/internal/version"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the pulse version",
	Args:  cobra.NoArgs,
	Run: func(_ *cobra.Command, _ []string) {
		upd := update.Check(version.Version)
		wave := ui.Wave()
		fmt.Println(wave[0])
		fmt.Printf("%s   %s %s\n", wave[1], ui.Bold("pulse"), version.Version)
		fmt.Printf("            %s\n", ui.Dim(fmt.Sprintf("%s · %s/%s · your local serverless cloud", runtime.Version(), runtime.GOOS, runtime.GOARCH)))
		// A cached answer arrives instantly; a live check gets 400ms, then
		// we stop caring — version must never feel slow.
		select {
		case latest, ok := <-upd:
			if ok {
				fmt.Printf("            %s\n", ui.Hint(fmt.Sprintf("%s available — `brew upgrade pulse`", latest)))
			}
		case <-time.After(400 * time.Millisecond):
		}
	},
}
