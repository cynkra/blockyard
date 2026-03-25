package main

import (
	"os"

	"github.com/spf13/cobra"
)

func main() {
	root := &cobra.Command{Use: "by-builder"}
	store := &cobra.Command{Use: "store"}
	store.AddCommand(populateCmd(), ingestCmd())
	root.AddCommand(store)
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
