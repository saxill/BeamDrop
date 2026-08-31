package mode

import (
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// messageLog is the conversation history a front end draws.
//
// Received files are not kept here. They are read back out of the inbox
// when the feed is asked for, so the history is right on the first run
// after a restart instead of starting empty every time. Text has nowhere
// else to live and is in memory only, which means it does not survive one —
// a real limitation, not an oversight.
type messageLog struct {
	mu   sync.Mutex
	msgs []Message
}

const messageLogMax = 500

func newMessageLog() *messageLog { return &messageLog{} }

func (l *messageLog) record(m Message) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.msgs = append(l.msgs, m)
	if len(l.msgs) > messageLogMax {
		// Copy rather than reslice, or the backing array grows unbounded.
		l.msgs = append([]Message(nil), l.msgs[len(l.msgs)-messageLogMax:]...)
	}
}

// feed merges remembered messages with the files actually in the inbox,
// oldest first, capped to the most recent `limit`.
func (l *messageLog) feed(inboxDir string, limit int) []Message {
	l.mu.Lock()
	out := append([]Message(nil), l.msgs...)
	l.mu.Unlock()

	if entries, err := os.ReadDir(inboxDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			out = append(out, Message{
				At:       info.ModTime(),
				Kind:     MessageFile,
				FileName: e.Name(),
				Size:     info.Size(),
				Path:     filepath.Join(inboxDir, e.Name()),
			})
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

var _ = time.Now
