// pulse is the CLI for the pulse local serverless platform.
package main

import (
	"fmt"
	"os"

	"pulse/internal/cli"
	"pulse/internal/ui"
)

func main() {
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, ui.Errorf(err))
		os.Exit(1)
	}
}
