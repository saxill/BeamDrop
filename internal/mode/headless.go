package mode

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// A relay's whole purpose is to run on a machine nobody is sitting at, and
// the TUI needs a terminal to exist. Under systemd there is none, so the
// portal failed to start on precisely the machine it was written for:
//
//	could not open a new TTY: open /dev/tty: no such device or address
//
// So when there is no terminal, don't try to draw one. Print the same
// information as plain lines and let the journal keep them.

// hasTerminal reports whether stdout is attached to something that can
// render a TUI. A pipe, a file, or systemd's journal is not.
func hasTerminal() bool {
	info, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// runHeadless serves until interrupted, printing what the TUI would have
// shown. Pairing prompts are the one thing it cannot reproduce — see
// headlessConfirmer.
func runHeadless(ctx context.Context, opts PortalOptions, logCh <-chan string, banner []string) error {
	for _, line := range banner {
		fmt.Println(line)
	}
	fmt.Printf("beamdrop: running headless (no terminal attached); Ctrl-C or systemctl stop to quit\n")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)

	for {
		select {
		case <-ctx.Done():
			return nil
		case s := <-sig:
			fmt.Printf("beamdrop: %s, shutting down\n", s)
			return nil
		case line, ok := <-logCh:
			if !ok {
				return nil
			}
			// Timestamped because these end up interleaved in a journal
			// with everything else on the machine.
			fmt.Printf("%s  %s\n", time.Now().Format("15:04:05"), line)
		}
	}
}

// headlessConfirmer decides pairings when there is nobody to ask.
//
// This is a genuine weakening and worth being clear about. Interactively, a
// human compares a 6-digit code on two screens before a new peer is
// trusted. A headless relay has no screen, so it either accepts new peers
// on its own or can never be paired with at all — and a relay nobody can
// pair with is not a relay.
//
// What carries the weight instead is reachability: the relay is only
// reachable from your tailnet, the same boundary the upload token sits
// behind. Pairing still requires being a node on it. Every new pairing is
// logged with the peer's name and the code it derived, so the dashboard and
// the journal both show what was accepted and when.
func headlessConfirmer(logf func(string, ...any)) func(_, _ [32]byte, peerName, code string, isKnown bool) bool {
	return func(_, _ [32]byte, peerName, code string, isKnown bool) bool {
		if isKnown {
			return true
		}
		logf("paired with a new peer %q (code %s) — nobody was here to confirm it", peerName, code)
		return true
	}
}
