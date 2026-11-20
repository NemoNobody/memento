package main

import (
	"os"

	"github.com/NemoNobody/velruna/cmd"
)

func main() {
	root := cmd.NewRootCmd()
	if err := root.Execute(); err != nil {
		// Cobra already printed the error to stderr; just signal failure.
		os.Exit(1)
	}
}
