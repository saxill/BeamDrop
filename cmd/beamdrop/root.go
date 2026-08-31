package main

import (
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "beamdrop",
	Short: "Send files between your laptop and iPhone, your way.",
}

func init() {
	rootCmd.AddCommand(sendCmd, watchCmd, portalCmd, spoolCmd)
}
