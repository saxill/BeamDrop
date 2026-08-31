package mode

import (
	"fmt"
	"os/exec"
	"sync"
	"time"
)

// A relayed file arrives without anyone asking for it — you sent it from
// your phone an hour ago while the laptop was shut. The TUI logs it, but
// the TUI is not necessarily on screen, so the point of the transfer is
// lost if nothing says so.
//
// Entirely best-effort. No notification daemon, no notify-send, a headless
// Pi: all of those are normal, and none of them is a reason to fail or even
// to complain.

// notifyTimeout stops a wedged notification daemon from holding a transfer
// goroutine open indefinitely.
const notifyTimeout = 5 * time.Second

// notifyOnce avoids paying for the PATH lookup on every single file when
// the binary is not installed, which is the common case on a server.
var (
	notifyOnce sync.Once
	notifyPath string
)

func notifyArrival(name string, size int64) {
	notifyOnce.Do(func() {
		// Ignore the error: an empty path simply means notifications are
		// not available here.
		notifyPath, _ = exec.LookPath("notify-send")
	})
	if notifyPath == "" {
		return
	}
	body := fmt.Sprintf("%s (%s)", name, humanSize(size))
	go func() {
		cmd := exec.Command(notifyPath, "--app-name=beamdrop", "beamdrop", body)
		if err := cmd.Start(); err != nil {
			return
		}
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(notifyTimeout):
			_ = cmd.Process.Kill()
			<-done
		}
	}()
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}
