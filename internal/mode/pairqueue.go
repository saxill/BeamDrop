package mode

import (
	"fmt"
	"sync"
)

// A pairing prompt has to be answerable from wherever the user actually is.
// The TUI could ask because it owned the process; a GUI in another language
// talking over HTTP cannot, so the question is parked here with an id and
// whoever is looking answers it by that id.
//
// The engine goroutine stays blocked on the answer throughout, which is
// what makes the prompt a decision rather than a notification.
type pairQueue struct {
	mu      sync.Mutex
	seq     uint64
	pending map[string]*pendingPair
}

type pendingPair struct {
	ID      string `json:"id"`
	Peer    string `json:"peer"`
	Code    string `json:"code"`
	respond chan bool
}

func newPairQueue() *pairQueue {
	return &pairQueue{pending: map[string]*pendingPair{}}
}

// Ask parks a request and blocks until something answers it. Returning
// false on an abandoned queue is deliberate: a pairing nobody confirmed is
// a pairing that did not happen.
func (q *pairQueue) Ask(peer, code string) bool {
	q.mu.Lock()
	q.seq++
	p := &pendingPair{
		ID:      fmt.Sprintf("p%d", q.seq),
		Peer:    peer,
		Code:    code,
		respond: make(chan bool, 1),
	}
	q.pending[p.ID] = p
	q.mu.Unlock()

	ok := <-p.respond

	q.mu.Lock()
	delete(q.pending, p.ID)
	q.mu.Unlock()
	return ok
}

// List returns what is waiting to be answered.
func (q *pairQueue) List() []pendingPair {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]pendingPair, 0, len(q.pending))
	for _, p := range q.pending {
		out = append(out, pendingPair{ID: p.ID, Peer: p.Peer, Code: p.Code})
	}
	return out
}

// Respond answers one request. Reports whether the id was actually waiting,
// so a stale click from a window that has not refreshed does not silently
// look like it worked.
func (q *pairQueue) Respond(id string, accept bool) bool {
	q.mu.Lock()
	p, ok := q.pending[id]
	q.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case p.respond <- accept:
		return true
	default:
		// Already answered — the channel is buffered for exactly one reply,
		// so a double click cannot block here.
		return false
	}
}

// RejectAll answers everything outstanding, so shutting down does not leave
// engine goroutines parked forever.
func (q *pairQueue) RejectAll() {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, p := range q.pending {
		select {
		case p.respond <- false:
		default:
		}
	}
}
