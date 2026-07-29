// pulse is the CLI for the pulse local serverless platform.
package main

import (
	"fmt"
	"os"

	"pulse/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "pulse: %v\n", err)
		os.Exit(1)
	}
}
