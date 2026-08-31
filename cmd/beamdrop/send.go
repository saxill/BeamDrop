package main

import (
	"github.com/spf13/cobra"

	"github.com/saxill/beamdrop/internal/mode"
)

var sendOpts mode.SendOptions

var sendCmd = &cobra.Command{
	Use:   "send <file>",
	Short: "Send a single file to a paired peer and exit.",
	Long: "Send a single file to a running portal and exit.\n\n" +
		"With no --peer this targets a portal on this same machine, which is\n" +
		"what `beamdrop portal` in another terminal gives you. Point --peer at\n" +
		"the other machine's tailnet address to send across the network.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return mode.Send(args[0], sendOpts)
	},
}

func init() {
	sendCmd.Flags().StringVar(&sendOpts.Peer, "peer", "", "host or IP running the beamdrop portal (default 127.0.0.1)")
	sendCmd.Flags().IntVar(&sendOpts.Port, "port", 0, "port the portal listens on (default 4747)")
}
