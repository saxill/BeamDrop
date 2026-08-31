package mode

import (
	"fmt"
	"sync"
	"time"
)

// A headless relay has no TUI to watch, so the log line that used to be the
// only record of "a file arrived" has nowhere to go. This keeps the last
// few hundred of them in memory for the dashboard to show.
//
// In memory on purpose: this is a convenience view, not an audit trail, and
// a relay should not slowly fill its card with its own logs.

const activityMax = 300

type activityEntry struct {
	At   time.Time
	Text string
}

type activityLog struct {
	mu      sync.Mutex
	entries []activityEntry
	// next receives every line as well, so the TUI keeps working when there
	// is one.
	next func(string, ...any)
}

func newActivityLog(next func(string, ...any)) *activityLog {
	return &activityLog{next: next}
}

// Logf records a line and passes it on.
func (a *activityLog) Logf(format string, args ...any) {
	text := fmt.Sprintf(format, args...)
	a.mu.Lock()
	a.entries = append(a.entries, activityEntry{At: time.Now(), Text: text})
	if len(a.entries) > activityMax {
		// Drop the oldest. Copying rather than reslicing keeps the backing
		// array from growing without bound.
		a.entries = append([]activityEntry(nil), a.entries[len(a.entries)-activityMax:]...)
	}
	a.mu.Unlock()
	if a.next != nil {
		a.next("%s", text)
	}
}

// Recent returns up to n entries, newest first.
func (a *activityLog) Recent(n int) []activityEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	if n > len(a.entries) {
		n = len(a.entries)
	}
	out := make([]activityEntry, 0, n)
	for i := len(a.entries) - 1; i >= len(a.entries)-n; i-- {
		out = append(out, a.entries[i])
	}
	return out
}
