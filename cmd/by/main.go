package main

import (
	"os"

	"github.com/spf13/cobra"
)

var version = "dev"

func main() {
	root := &cobra.Command{
		Use:     "by",
		Short:   "Blockyard CLI",
		Long:    "Command-line client for the Blockyard deployment platform.",
		Version: version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().Bool("json", false, "Output machine-readable JSON")

	root.AddCommand(
		loginCmd(),
		initCmd(),
		deployCmd(),
		listCmd(),
		getCmd(),
		enableCmd(),
		disableCmd(),
		deleteCmd(),
		restoreCmd(),
		bundlesCmd(),
		rollbackCmd(),
		scaleCmd(),
		updateCmd(),
		accessCmd(),
		tagsCmd(),
		refreshCmd(),
		logsCmd(),
		usersCmd(),
		selfUpdateCmd(),
	)

	// Aliases: create full copies so flags work correctly.
	ls := listCmd()
	ls.Use = "ls"
	ls.Hidden = true
	ls.Aliases = nil
	root.AddCommand(ls)

	rm := deleteCmd()
	rm.Use = "rm"
	rm.Hidden = true
	rm.Aliases = nil
	root.AddCommand(rm)

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
