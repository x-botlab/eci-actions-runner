package main

import (
	"fmt"
	"os"

	"github.com/x-botlab/eci-actions-runner/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "eci-runner:", err)
		os.Exit(1)
	}
}
