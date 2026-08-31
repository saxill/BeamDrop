package main

import (
	"github.com/spf13/cobra"

	"github.com/saxill/beamdrop/internal/config"
	"github.com/saxill/beamdrop/internal/mode"
)

var (
	portalPort      int
	portalInbox     string
	portalRelay     bool
	portalRelayTo   string
	portalConnectTo string
)

var portalCmd = &cobra.Command{
	Use:   "portal",
	Short: "Launch the TUI portal and the iPhone-facing webui together.",
	Long: "Launch the portal: a TUI on the laptop and, on the same port, the\n" +
		"page your phone opens over Tailscale. `beamdrop send` and\n" +
		"`beamdrop watch` connect to this too.\n\n" +
		"Type `:send <path>` to push a file to every connected peer, `:q` to quit.",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := config.Defaults()
		if portalPort != 0 {
			cfg.Port = portalPort
		}
		if portalInbox != "" {
			cfg.InboxDir = portalInbox
		}
		inbox, err := config.EnsureInbox(cfg)
		if err != nil {
			return err
		}
		return mode.Portal(mode.PortalOptions{
			Relay:         portalRelay,
			RelayTo:       portalRelayTo,
			ConnectTo:     portalConnectTo,
			InboxDir:      inbox,
			KnownPeersDir: cfg.KnownPeersDir,
			ConfigDir:     cfg.ConfigDir,
			Port:          cfg.Port,
		})
	},
}

func init() {
	portalCmd.Flags().IntVar(&portalPort, "port", 0, "port to listen on (default 4747)")
	portalCmd.Flags().StringVar(&portalInbox, "inbox", "", "directory for incoming files (default ~/Portal/inbox)")
	portalCmd.Flags().StringVar(&portalRelayTo, "relay-to", "", "forward uploads with no destination header to this peer (relay mode)")
	portalCmd.Flags().BoolVar(&portalRelay, "relay", false, "hold files for peers that are offline and deliver them later (for an always-on machine)")
	portalCmd.Flags().StringVar(&portalConnectTo, "connect-to", "", "dial this address and stay connected to it (e.g. a relay's tailnet IP:port)")
}
