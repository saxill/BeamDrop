package main

import (
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/saxill/beamdrop/internal/config"
	"github.com/saxill/beamdrop/internal/spool"
)

var spoolClear bool

var spoolCmd = &cobra.Command{
	Use:   "spool",
	Short: "Show files this relay is holding for peers that were offline.",
	Long: "Show files this relay is holding for peers that were offline.\n\n" +
		"Only relevant on a node running `beamdrop portal --relay`. A file that\n" +
		"cannot be delivered is retried until it is collected or ages out, so\n" +
		"this is where you look when something you sent has not arrived.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := config.Defaults()
		dir := filepath.Join(cfg.ConfigDir, "spool")
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			fmt.Println("nothing spooled (this node is not running as a relay)")
			return nil
		}
		s, err := spool.Open(dir)
		if err != nil {
			return err
		}

		if spoolClear {
			n, err := s.Clear()
			if err != nil {
				return err
			}
			fmt.Printf("dropped %d spooled file(s)\n", n)
			return nil
		}

		items, err := s.Pending()
		if err != nil {
			return err
		}
		if len(items) == 0 {
			fmt.Println("nothing waiting — everything has been delivered")
			return nil
		}

		total, _ := s.Bytes()
		fmt.Printf("%d file(s) waiting, %s total, in %s\n\n", len(items), humanBytes(total), dir)

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "FOR\tFILE\tSIZE\tWAITING\tTRIES\tLAST ERROR")
		for _, i := range items {
			last := i.LastError
			if last == "" {
				last = "-"
			}
			if len(last) > 48 {
				last = last[:45] + "..."
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n",
				i.To, i.Name, humanBytes(i.Size),
				time.Since(i.ReceivedAt).Round(time.Minute), i.Attempts, last)
		}
		return w.Flush()
	},
}

func init() {
	spoolCmd.Flags().BoolVar(&spoolClear, "clear", false, "drop everything waiting (the files are lost, not delivered)")
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fM", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0fK", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}
