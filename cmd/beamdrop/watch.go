package main

import (
	"github.com/spf13/cobra"

	"github.com/saxill/beamdrop/internal/mode"
)

var watchOpts = mode.WatchOptions{Verbose: true}

var watchCmd = &cobra.Command{
	Use:   "watch <dir>",
	Short: "Watch a directory and beam new files to the paired peer.",
	Long: "Watch a directory and beam every new or changed file to a running\n" +
		"portal. Pairs once at startup and reuses that connection, so a hot\n" +
		"folder does not re-pair per file.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return mode.Watch(args[0], watchOpts)
	},
}

func init() {
	watchCmd.Flags().StringVar(&watchOpts.Peer, "peer", "", "host or IP running the beamdrop portal (default 127.0.0.1)")
	watchCmd.Flags().IntVar(&watchOpts.Port, "port", 0, "port the portal listens on (default 4747)")
}
