package engine

import (
	"bytes"
	"crypto/sha256"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/saxill/beamdrop/internal/frame"
	"github.com/saxill/beamdrop/internal/pairing"
	"github.com/saxill/beamdrop/internal/peer"
	"github.com/saxill/beamdrop/internal/transfer"
)

// frameSpy wraps a net.Conn and records the frame.Type byte of every Write call.
type frameSpy struct {
	net.Conn
	mu    sync.Mutex
	types []frame.Type
}

func (s *frameSpy) Write(p []byte) (int, error) {
	if len(p) >= 5 {
		s.mu.Lock()
		s.types = append(s.types, frame.Type(p[4]))
		s.mu.Unlock()
	}
	return s.Conn.Write(p)
}

func (s *frameSpy) seen(t frame.Type) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.types {
		if x == t {
			return true
		}
	}
	return false
}

// pipe is a pair of in-memory full-duplex connections, one for each side.
type pipe struct {
	a, b net.Conn
}

func newPipe(t *testing.T) *pipe {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	type dialResult struct {
		c   net.Conn
		err error
	}
	resCh := make(chan dialResult, 1)
	go func() {
		c, err := net.Dial("tcp", addr.String())
		resCh <- dialResult{c, err}
	}()
	server, err := ln.Accept()
	if err != nil {
		ln.Close()
		t.Fatal(err)
	}
	ln.Close()
	res := <-resCh
	if res.err != nil {
		t.Fatal(res.err)
	}
	return &pipe{a: res.c, b: server}
}

func (p *pipe) close() {
	p.a.Close()
	p.b.Close()
}

// codePrompt returns a Confirmer that auto-accepts the pairing.
func codePrompt(t *testing.T) Confirmer {
	return func(initPub, respPub [32]byte, peerName, code string, known bool) bool { return true }
}

func TestEngineConfirmerReceivesNameCodeAndKnownFlag(t *testing.T) {
	p := newPipe(t)
	defer p.close()
	var gotName, gotCode string
	var gotKnown bool
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := New(Config{Name: "A", Conn: p.a, IsInitiator: true, Confirmer: codePrompt(t)})
		if err != nil {
			t.Errorf("A: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		_, err := New(Config{Name: "B-Phone", Conn: p.b, IsInitiator: false, Confirmer: func(initPub, respPub [32]byte, peerName, code string, known bool) bool {
			gotName, gotCode, gotKnown = peerName, code, known
			return true
		}})
		if err != nil {
			t.Errorf("B: %v", err)
		}
	}()
	wg.Wait()
	if gotName != "A" {
		t.Errorf("peerName = %q, want %q", gotName, "A")
	}
	if len(gotCode) != 6 {
		t.Errorf("code = %q, want 6 digits", gotCode)
	}
	if gotKnown {
		t.Error("known = true on a fresh pairing, want false")
	}
}

func TestEngineRoundTrip(t *testing.T) {
	p := newPipe(t)
	defer p.close()

	dir := t.TempDir()

	// Side A: sender
	dataPath := filepath.Join(dir, "src.bin")
	payload := bytes.Repeat([]byte("XY"), 50_000) // 100KB
	if err := os.WriteFile(dataPath, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	receiveDir := filepath.Join(dir, "recv")
	os.MkdirAll(receiveDir, 0o755)

	sent := make(chan uint64, 1)
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)

	// Side A runs the initiator engine.
	go func() {
		defer wg.Done()
		e, err := New(Config{
			Name:        "A",
			Conn:        p.a,
			IsInitiator: true,
			Confirmer:   codePrompt(t),
		})
		if err != nil {
			t.Errorf("A: new engine: %v", err)
			return
		}
		s, err := transfer.NewSender(dataPath)
		if err != nil {
			t.Errorf("A: sender: %v", err)
			return
		}
		if err := e.SendFile(s, func(transfer.Receiver) bool { return true }); err != nil {
			t.Errorf("A: send: %v", err)
			return
		}
		// Signal end-of-session by closing our end of the connection.
		// The responder's Serve loop is blocking on ReadFrame, so it needs
		// EOF (or any error) to exit. The brief's test as-written does
		// not close the connection and would deadlock here.
		p.a.Close()
		sent <- s.Size
	}()

	// Side B runs the responder engine and accepts everything.
	go func() {
		defer wg.Done()
		e, err := New(Config{
			Name:        "B",
			Conn:        p.b,
			IsInitiator: false,
			Confirmer:   codePrompt(t),
		})
		if err != nil {
			t.Errorf("B: new engine: %v", err)
			return
		}
		e.OnFileOffer(func(frame.FileOfferPayload) (string, bool) {
			return receiveDir, true
		})
		// Side A closes the connection to end the session, and it may do so
		// with a trailing advisory ACK still unread — which the kernel
		// answers with RST rather than FIN. Any ordinary hang-up is fine
		// here; only a protocol error should fail the test.
		if err := e.Serve(); !IsDisconnect(err) {
			t.Errorf("B: serve: %v", err)
		}
		close(done)
	}()

	select {
	case <-sent:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for send to complete")
	}
	<-done
	wg.Wait()

	// Verify received file matches source.
	got, err := os.ReadFile(filepath.Join(receiveDir, "src.bin"))
	if err != nil {
		t.Fatalf("read received file: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("received file differs (got %d bytes, want %d bytes)", len(got), len(payload))
	}
}

func TestEnginePairingChallengeResponseExchanged(t *testing.T) {
	p := newPipe(t)
	defer p.close()

	spyA := &frameSpy{Conn: p.a}
	spyB := &frameSpy{Conn: p.b}

	var wg sync.WaitGroup
	wg.Add(2)

	// Side A runs the initiator engine.
	go func() {
		defer wg.Done()
		_, err := New(Config{
			Name:        "A",
			Conn:        spyA,
			IsInitiator: true,
			Confirmer:   codePrompt(t),
		})
		if err != nil {
			t.Errorf("A: new engine: %v", err)
		}
	}()

	// Side B runs the responder engine.
	go func() {
		defer wg.Done()
		_, err := New(Config{
			Name:        "B",
			Conn:        spyB,
			IsInitiator: false,
			Confirmer:   codePrompt(t),
		})
		if err != nil {
			t.Errorf("B: new engine: %v", err)
		}
	}()

	wg.Wait()

	// Assert that both challenge and response frames were exchanged.
	if !spyA.seen(frame.PairChallengeType) {
		t.Error("initiator did not send PAIR_CHALLENGE")
	}
	if !spyB.seen(frame.PairResponseType) {
		t.Error("responder did not send PAIR_RESPONSE")
	}
}

func TestTOFUKnownPeerSkipsToAutoKnown(t *testing.T) {
	// Test that the known==true branch in pair() is exercised when a peer is
	// pre-seeded in the KnownPeers store. We inject a fixed keypair for the
	// responder so we can pre-seed it into the store before pairing.

	// Step 1: Generate a fixed keypair for the responder
	responderKP, err := pairing.Generate()
	if err != nil {
		t.Fatalf("generate responder keypair: %v", err)
	}

	// Step 2: Create KnownPeers store and pre-seed with responder's pubkey
	dir := t.TempDir()
	store := peer.NewMemStore()
	store.Add(peer.Peer{
		Name:   "PreSeededResponder",
		PubKey: responderKP.Pub,
	})
	knownPeers, err := pairing.NewKnownPeers(dir, store)
	if err != nil {
		t.Fatalf("create known peers store: %v", err)
	}

	// Step 3: Run pairing with responder using the pre-seeded keypair
	p := newPipe(t)
	defer p.close()

	var initiatorSawKnown, responderSawKnown bool
	var wg sync.WaitGroup
	wg.Add(2)

	// Initiator (side a) with Config.Known set
	go func() {
		defer wg.Done()
		_, err := New(Config{
			Name:        "Initiator",
			Conn:        p.a,
			IsInitiator: true,
			Confirmer: func(initPub, respPub [32]byte, peerName, code string, known bool) bool {
				// Initiator will see responder as known because responder's pubkey
				// is pre-seeded in the store
				initiatorSawKnown = known
				return true
			},
			Known: knownPeers,
		})
		if err != nil {
			// Errorf, not Fatalf: Fatalf calls runtime.Goexit, which only
			// unwinds the goroutine it runs on, so from a non-test
			// goroutine it fails to stop the test (go vet flags this).
			t.Errorf("initiator: %v", err)
			return
		}
	}()

	// Responder (side b) using the pre-seeded keypair
	go func() {
		defer wg.Done()
		_, err := New(Config{
			Name:        "PreSeededResponder",
			Conn:        p.b,
			IsInitiator: false,
			Confirmer: func(initPub, respPub [32]byte, peerName, code string, known bool) bool {
				responderSawKnown = known
				return true
			},
			Known:       knownPeers,
			testKeyPair: &responderKP,
		})
		if err != nil {
			t.Errorf("responder: %v", err)
			return
		}
	}()

	wg.Wait()

	// Initiator should see known==true because responder's pubkey was pre-seeded
	if !initiatorSawKnown {
		t.Error("initiator: expected known==true for pre-seeded responder, got false")
	}

	// Responder will see known==false because initiator's pubkey is fresh
	if responderSawKnown {
		t.Error("responder: expected known==false for fresh initiator, got true")
	}
}

func TestTOFUSuccessfulPairingPersistsPeer(t *testing.T) {
	dir := t.TempDir()
	store := peer.NewMemStore()
	knownPeers, err := pairing.NewKnownPeers(dir, store)
	if err != nil {
		t.Fatalf("create known peers store: %v", err)
	}

	p := newPipe(t)
	defer p.close()

	var initiatorPubKey, responderPubKey [32]byte
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		e, err := New(Config{
			Name:        "Initiator",
			Conn:        p.a,
			IsInitiator: true,
			Confirmer:   codePrompt(t),
			Known:       knownPeers,
		})
		if err != nil {
			t.Errorf("initiator: %v", err)
			return
		}
		initiatorPubKey = e.PeerPubKey()
	}()

	go func() {
		defer wg.Done()
		e, err := New(Config{
			Name:        "Responder",
			Conn:        p.b,
			IsInitiator: false,
			Confirmer:   codePrompt(t),
			Known:       knownPeers,
		})
		if err != nil {
			t.Errorf("responder: %v", err)
			return
		}
		responderPubKey = e.PeerPubKey()
	}()

	wg.Wait()

	// After successful pairing, both peers should be in the known-peers store.
	// The initiator knows the responder's pubkey, and the responder knows
	// the initiator's pubkey.
	_, knownInit := knownPeers.IsKnown(responderPubKey)
	if !knownInit {
		t.Error("after pairing, initiator's peer (responder) should be known in store, but IsKnown returned false")
	}

	_, knownResp := knownPeers.IsKnown(initiatorPubKey)
	if !knownResp {
		t.Error("after pairing, responder's peer (initiator) should be known in store, but IsKnown returned false")
	}
}

// TestEngineConcurrentServeAndSendFile exercises the exact topology the
// multi-peer registry relies on: a single engine that is simultaneously
// Serve()-ing (receiving an inbound file) and the target of a concurrent
// SendFile() call (an outbound send), on both sides at once. Before the
// single-reader/dispatch redesign, Serve's inline receiveFile and a
// concurrent SendFile both called frame.ReadFrame directly, racing for
// the same connection.
//
// Payloads are multi-MB (well past a single TCP socket buffer) so this
// exercises real bidirectional traffic on the shared connection, giving
// far more coverage than the original 80KB (2-chunk) version. Note this
// does NOT reach the specific backpressure regime the writeMu/ACK
// deadlock needed to reproduce — that took shrinking OS socket buffers to
// a few KB (see TestWriteBestEffortDoesNotBlockWhenWriterIsStuckWriting's
// doc comment for why that approach was abandoned in favor of a
// deterministic unit test instead).
func TestEngineConcurrentServeAndSendFile(t *testing.T) {
	p := newPipe(t)
	defer p.close()

	dir := t.TempDir()
	aToB := filepath.Join(dir, "a-to-b.bin")
	bToA := filepath.Join(dir, "b-to-a.bin")
	if err := os.WriteFile(aToB, bytes.Repeat([]byte("AB"), 4_000_000), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bToA, bytes.Repeat([]byte("BA"), 4_000_000), 0o644); err != nil {
		t.Fatal(err)
	}
	recvA := filepath.Join(dir, "recvA")
	recvB := filepath.Join(dir, "recvB")
	os.MkdirAll(recvA, 0o755)
	os.MkdirAll(recvB, 0o755)

	// pair() requires both sides actively participating concurrently
	// (initiator writes while responder reads, in lockstep) — New() must
	// run in goroutines for both sides, not sequentially.
	var eA, eB *Engine
	var pairWG sync.WaitGroup
	pairWG.Add(2)
	var errA, errB error
	go func() {
		defer pairWG.Done()
		eA, errA = New(Config{Name: "A", Conn: p.a, IsInitiator: true, Confirmer: codePrompt(t)})
	}()
	go func() {
		defer pairWG.Done()
		eB, errB = New(Config{Name: "B", Conn: p.b, IsInitiator: false, Confirmer: codePrompt(t)})
	}()
	pairWG.Wait()
	if errA != nil {
		t.Fatalf("A: new: %v", errA)
	}
	if errB != nil {
		t.Fatalf("B: new: %v", errB)
	}
	eA.OnFileOffer(func(frame.FileOfferPayload) (string, bool) {
		return recvA, true
	})
	eB.OnFileOffer(func(frame.FileOfferPayload) (string, bool) {
		return recvB, true
	})

	// Serve blocks until the connection closes, so it must not be part of
	// the group we wait on before closing the connection (that's the
	// ordering bug TestEngineRoundTrip's own comment warns about). Only
	// the two SendFile calls are waited on directly; once both finish we
	// close the pipe to unblock both Serve loops, then wait for those.
	var sendWG sync.WaitGroup
	sendWG.Add(2)
	errs := make(chan error, 4)

	// Serve's return error on shutdown isn't asserted here: this test
	// closes both ends of the pipe itself while each side's readLoop may
	// still have a read in flight, which — unlike one side closing only
	// its own end while the peer reads (TestEngineRoundTrip's pattern) —
	// surfaces as "use of closed network connection" on that side rather
	// than io.EOF. The actual thing under test is the transferred file
	// content, asserted below; Serve's shutdown error flavor here is not
	// a signal of a real failure either way.
	serveDone := make(chan struct{})
	var serveWG sync.WaitGroup
	serveWG.Add(2)
	go func() { defer serveWG.Done(); _ = eA.Serve() }()
	go func() { defer serveWG.Done(); _ = eB.Serve() }()
	go func() { serveWG.Wait(); close(serveDone) }()

	go func() {
		defer sendWG.Done()
		s, err := transfer.NewSender(aToB)
		if err != nil {
			errs <- err
			return
		}
		defer s.Close()
		if err := eA.SendFile(s, nil); err != nil {
			errs <- err
		}
	}()
	go func() {
		defer sendWG.Done()
		s, err := transfer.NewSender(bToA)
		if err != nil {
			errs <- err
			return
		}
		defer s.Close()
		if err := eB.SendFile(s, nil); err != nil {
			errs <- err
		}
	}()

	done := make(chan struct{})
	go func() {
		sendWG.Wait()
		p.a.Close()
		p.b.Close()
		<-serveDone
		close(done)
	}()

	select {
	case <-done:
	case err := <-errs:
		t.Fatalf("concurrent transfer error: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("timeout")
	}
	select {
	case err := <-errs:
		t.Fatalf("concurrent transfer error: %v", err)
	default:
	}

	gotAB, err := os.ReadFile(filepath.Join(recvB, "a-to-b.bin"))
	if err != nil {
		t.Fatalf("recvB: %v", err)
	}
	wantAB, _ := os.ReadFile(aToB)
	if !bytes.Equal(gotAB, wantAB) {
		t.Errorf("A->B: got %d bytes, want %d matching", len(gotAB), len(wantAB))
	}
	gotBA, err := os.ReadFile(filepath.Join(recvA, "b-to-a.bin"))
	if err != nil {
		t.Fatalf("recvA: %v", err)
	}
	wantBA, _ := os.ReadFile(bToA)
	if !bytes.Equal(gotBA, wantBA) {
		t.Errorf("B->A: got %d bytes, want %d matching", len(gotBA), len(wantBA))
	}
}

// TestEngineDropsOrphanedChunkAfterReceiveCompletes exercises the C2
// scenario from the engine.go concurrency review: a peer streams a frame
// (here, a trailing CHUNK) for a transfer whose receiveFile goroutine has
// already returned. Before the chunkDone-guarded select in readLoop's
// dispatch, this was an unconditional blocking send into chunkWait with no
// consumer left to read it — permanently wedging readLoop, and with it the
// whole connection, remotely triggerable by any peer that overshoots its
// own declared transfer size. This test proves the connection survives:
// after the orphan frame, a second, unrelated transfer in the opposite
// direction must still complete within a short deadline.
func TestEngineDropsOrphanedChunkAfterReceiveCompletes(t *testing.T) {
	p := newPipe(t)
	defer p.close()

	dir := t.TempDir()
	first := filepath.Join(dir, "first.bin")
	if err := os.WriteFile(first, bytes.Repeat([]byte("X"), 1000), 0o644); err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(dir, "second.bin")
	if err := os.WriteFile(second, bytes.Repeat([]byte("Y"), 1000), 0o644); err != nil {
		t.Fatal(err)
	}
	recvB := t.TempDir()
	recvA := t.TempDir()

	var eA, eB *Engine
	var pairWG sync.WaitGroup
	pairWG.Add(2)
	var errA, errB error
	go func() {
		defer pairWG.Done()
		eA, errA = New(Config{Name: "A", Conn: p.a, IsInitiator: true, Confirmer: codePrompt(t)})
	}()
	go func() {
		defer pairWG.Done()
		eB, errB = New(Config{Name: "B", Conn: p.b, IsInitiator: false, Confirmer: codePrompt(t)})
	}()
	pairWG.Wait()
	if errA != nil {
		t.Fatalf("A: new: %v", errA)
	}
	if errB != nil {
		t.Fatalf("B: new: %v", errB)
	}

	eB.OnFileOffer(func(frame.FileOfferPayload) (string, bool) {
		return recvB, true
	})
	eA.OnFileOffer(func(frame.FileOfferPayload) (string, bool) {
		return recvA, true
	})
	// B needs its readLoop running to receive; A's is already started as a
	// side effect of the SendFile call below (startReader is shared).
	go func() { _ = eB.Serve() }()

	s, err := transfer.NewSender(first)
	if err != nil {
		t.Fatal(err)
	}
	if err := eA.SendFile(s, nil); err != nil {
		s.Close()
		t.Fatalf("first SendFile (A->B): %v", err)
	}
	s.Close()
	if got, err := os.ReadFile(filepath.Join(recvB, "first.bin")); err != nil || !bytes.Equal(got, bytes.Repeat([]byte("X"), 1000)) {
		t.Fatalf("first transfer did not arrive intact: %v", err)
	}

	// B's receiveFile for "first.bin" has returned (chunkDone closed).
	// Simulate a buggy/duplicate peer still streaming a CHUNK for that
	// same, already-finished transfer.
	orphanChunk := frame.ChunkPayload{ID: 999, Offset: 0, Data: []byte("orphan")}
	if err := frame.WriteFrame(p.a, frame.ChunkType, orphanChunk); err != nil {
		t.Fatalf("write orphan chunk: %v", err)
	}

	// Prove B's readLoop survived: round-trip a second, unrelated file the
	// other direction. This requires B's readLoop to still be dispatching
	// FILE_ACCEPT/FILE_DONE back to a new SendFile call — if the orphan
	// chunk had wedged it, this would hang forever instead of erroring, so
	// it's guarded with a deadline rather than relying on an error return.
	sendDone := make(chan error, 1)
	go func() {
		s2, err := transfer.NewSender(second)
		if err != nil {
			sendDone <- err
			return
		}
		defer s2.Close()
		sendDone <- eB.SendFile(s2, nil)
	}()
	select {
	case err := <-sendDone:
		if err != nil {
			t.Fatalf("second SendFile (B->A) after orphan chunk: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout: B's readLoop appears wedged by the orphaned chunk")
	}
	if got, err := os.ReadFile(filepath.Join(recvA, "second.bin")); err != nil || !bytes.Equal(got, bytes.Repeat([]byte("Y"), 1000)) {
		t.Fatalf("second transfer did not arrive intact: %v", err)
	}
}

// blockingConn wraps a net.Conn and lets a test make Write block on demand
// (until the returned channel is closed), standing in for a socket send
// buffer that's genuinely full. Reads and everything else pass through
// unchanged, so it's safe to use for the connection a New() pairing
// handshake runs over — only writes made after setBlocked is called are
// affected.
type blockingConn struct {
	net.Conn
	mu    sync.Mutex
	block chan struct{}
}

func (b *blockingConn) setBlocked(ch chan struct{}) {
	b.mu.Lock()
	b.block = ch
	b.mu.Unlock()
}

func (b *blockingConn) Write(p []byte) (int, error) {
	b.mu.Lock()
	ch := b.block
	b.mu.Unlock()
	if ch != nil {
		<-ch
	}
	return b.Conn.Write(p)
}

// TestWriteBestEffortDoesNotBlockWhenWriterIsStuckWriting directly
// exercises the invariant behind the writeMu/ACK deadlock found across two
// rounds of independent review: a SendFile chunk write (or any other
// writer sharing this engine's connection) can block on conn.Write for an
// arbitrarily long time on a full socket send buffer. A non-blocking mutex
// (TryLock) alone doesn't help — it only avoids blocking on *acquiring*
// writeMu, not on the conn.Write call made while holding it. If
// receiveFile's ACK write blocks there, it stops draining chunkWait, which
// stalls readLoop's dispatch, which stops readLoop reading the connection
// at all; mirrored on the peer engine under a large enough bidirectional
// transfer, that's a permanent circular deadlock.
//
// The fix routes ACKs (and readLoop's own outbound rejections) through a
// bounded queue drained by a dedicated writer goroutine, so the caller
// (writeBestEffort) never itself calls conn.Write. This test proves that:
// it makes the connection's Write block indefinitely, then confirms many
// back-to-back writeBestEffort calls — more than the queue's buffer size,
// so some are guaranteed to be dropped rather than merely buffered — all
// return promptly regardless.
//
// Reproducing the underlying deadlock end-to-end via real OS-level
// backpressure (shrinking socket buffers) was tried and abandoned: this
// sandbox's loopback TCP stack stalls completely (not just slowly) below
// roughly a 32KB SO_SNDBUF/SO_RCVBUF, independent of any engine code — an
// environment artifact, not a beamdrop bug. This test simulates the same
// condition directly via blockingConn instead.
func TestWriteBestEffortDoesNotBlockWhenWriterIsStuckWriting(t *testing.T) {
	p := newPipe(t)
	defer p.close()

	bc := &blockingConn{Conn: p.a}
	var eA *Engine
	var pairWG sync.WaitGroup
	pairWG.Add(2)
	var errA, errB error
	go func() {
		defer pairWG.Done()
		eA, errA = New(Config{Name: "A", Conn: bc, IsInitiator: true, Confirmer: codePrompt(t)})
	}()
	go func() {
		defer pairWG.Done()
		_, errB = New(Config{Name: "B", Conn: p.b, IsInitiator: false, Confirmer: codePrompt(t)})
	}()
	pairWG.Wait()
	if errA != nil {
		t.Fatalf("A: new: %v", errA)
	}
	if errB != nil {
		t.Fatalf("B: new: %v", errB)
	}

	// Block all further writes on A's connection — standing in for a full
	// socket send buffer that never drains. Released at test end so
	// cleanup doesn't hang.
	block := make(chan struct{})
	bc.setBlocked(block)
	defer close(block)

	done := make(chan struct{})
	go func() {
		defer close(done)
		// More than bestEffortQueue's buffer (64), so this only
		// completes if writeBestEffort drops rather than blocks once
		// the queue is full.
		for i := 0; i < 200; i++ {
			eA.writeBestEffort(frame.AckType, frame.AckPayload{ID: uint64(i)})
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("writeBestEffort blocked while its writer goroutine was stuck in conn.Write")
	}
}

// TestWriteBestEffortWritesWhenUncontended confirms the best-effort path
// isn't a no-op: with the connection free, a queued frame is actually
// written by the dedicated writer goroutine. Since that goroutine runs
// asynchronously, this polls briefly rather than checking immediately.
func TestWriteBestEffortWritesWhenUncontended(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().(*net.TCPAddr)

	type dialResult struct {
		c   net.Conn
		err error
	}
	resCh := make(chan dialResult, 1)
	go func() {
		c, err := net.Dial("tcp", addr.String())
		resCh <- dialResult{c, err}
	}()
	server, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	res := <-resCh
	if res.err != nil {
		t.Fatal(res.err)
	}
	spy := &frameSpy{Conn: res.c}
	defer spy.Close()
	defer server.Close()

	var eA *Engine
	var pairWG sync.WaitGroup
	pairWG.Add(2)
	var errA, errB error
	go func() {
		defer pairWG.Done()
		eA, errA = New(Config{Name: "A", Conn: spy, IsInitiator: true, Confirmer: codePrompt(t)})
	}()
	go func() {
		defer pairWG.Done()
		_, errB = New(Config{Name: "B", Conn: server, IsInitiator: false, Confirmer: codePrompt(t)})
	}()
	pairWG.Wait()
	if errA != nil {
		t.Fatalf("A: new: %v", errA)
	}
	if errB != nil {
		t.Fatalf("B: new: %v", errB)
	}

	eA.writeBestEffort(frame.AckType, frame.AckPayload{ID: 1, Offset: 100})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if spy.seen(frame.AckType) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("writeBestEffort did not write an ACK frame when the connection was uncontended")
}

// TestEngineReceiveConfinesOfferNameToAcceptorDir proves the engine treats
// FILE_OFFER.Name as untrusted input even from a peer that has completed
// pairing. Pairing establishes *who* the peer is, not that it is well
// behaved — and the whole point of a hot inbox is that it is writable by
// whoever is on the other end, so a name that climbs out of it is an
// arbitrary-file-write primitive.
//
// The offer is forged by writing the frame straight to the socket rather
// than going through transfer.Sender: Sender.Offer() calls filepath.Base,
// so an honest beamdrop peer physically cannot emit this name. Only a
// modified client can, which is exactly the case that matters.
func TestEngineReceiveConfinesOfferNameToAcceptorDir(t *testing.T) {
	p := newPipe(t)
	defer p.close()

	// inbox is nested inside root so an escape has somewhere to land that
	// the test can positively identify.
	root := t.TempDir()
	inbox := filepath.Join(root, "inbox")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	newErr := make(chan error, 1)
	go func() {
		defer wg.Done()
		e, err := New(Config{Name: "B", Conn: p.b, IsInitiator: false, Confirmer: codePrompt(t)})
		if err != nil {
			newErr <- err
			return
		}
		newErr <- nil
		e.OnFileOffer(func(frame.FileOfferPayload) (string, bool) { return inbox, true })
		_ = e.Serve()
	}()

	if _, err := New(Config{Name: "A", Conn: p.a, IsInitiator: true, Confirmer: codePrompt(t)}); err != nil {
		t.Fatalf("A: new: %v", err)
	}
	if err := <-newErr; err != nil {
		t.Fatalf("B: new: %v", err)
	}

	// Side A's engine never started its reader (that happens lazily on the
	// first Serve/SendFile), so the raw connection is free to write on.
	payload := []byte("beamed out of bounds")
	offer := frame.FileOfferPayload{
		ID:     42,
		Name:   "../escaped.bin",
		Size:   uint64(len(payload)),
		SHA256: sha256.Sum256(payload),
		MIME:   "application/octet-stream",
	}
	if err := frame.WriteFrame(p.a, frame.FileOfferType, offer); err != nil {
		t.Fatal(err)
	}
	if err := frame.WriteFrame(p.a, frame.ChunkType, frame.ChunkPayload{ID: 42, Data: payload}); err != nil {
		t.Fatal(err)
	}

	escaped := filepath.Join(root, "escaped.bin")
	confined := filepath.Join(inbox, "escaped.bin")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(escaped); err == nil {
			t.Fatalf("peer-controlled name escaped the acceptor's directory: wrote %s", escaped)
		}
		if _, err := os.Stat(confined); err == nil {
			p.a.Close()
			wg.Wait()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	p.a.Close()
	wg.Wait()
	t.Fatalf("file never arrived at %s", confined)
}

// TestEngineRefusesOffersWithNoAcceptorRegistered pins the send-only case:
// SendFile starts the same read loop that handles inbound offers, so an
// engine built purely to push files (watch mode) is still listening. With
// no OnFileOffer registered it must refuse — the earlier default of "."
// meant a paired peer could write into whatever directory beamdrop was
// launched from.
func TestEngineRefusesOffersWithNoAcceptorRegistered(t *testing.T) {
	p := newPipe(t)
	defer p.close()

	// Run from a scratch directory so the old "." default would leave an
	// observable artifact somewhere harmless rather than in the package dir.
	// (t.Chdir would be tidier but needs go1.24; this module is go1.22.)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(cwd) }()

	var wg sync.WaitGroup
	wg.Add(1)
	newErr := make(chan error, 1)
	go func() {
		defer wg.Done()
		e, err := New(Config{Name: "B", Conn: p.b, IsInitiator: false, Confirmer: codePrompt(t)})
		newErr <- err
		if err != nil {
			return
		}
		_ = e.Serve() // no OnFileOffer call
	}()

	if _, err := New(Config{Name: "A", Conn: p.a, IsInitiator: true, Confirmer: codePrompt(t)}); err != nil {
		t.Fatalf("A: new: %v", err)
	}
	if err := <-newErr; err != nil {
		t.Fatalf("B: new: %v", err)
	}

	payload := []byte("unwanted")
	offer := frame.FileOfferPayload{
		ID:     7,
		Name:   "unwanted.bin",
		Size:   uint64(len(payload)),
		SHA256: sha256.Sum256(payload),
	}
	if err := frame.WriteFrame(p.a, frame.FileOfferType, offer); err != nil {
		t.Fatal(err)
	}
	if err := frame.WriteFrame(p.a, frame.ChunkType, frame.ChunkPayload{ID: 7, Data: payload}); err != nil {
		t.Fatal(err)
	}

	// The refusal comes back as an ERROR frame; read until we see one.
	if err := p.a.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var sawError bool
	for !sawError {
		ft, _, err := frame.ReadFrame(p.a)
		if err != nil {
			t.Fatalf("waiting for refusal: %v", err)
		}
		if ft == frame.FileAcceptType {
			t.Fatal("engine accepted a file offer with no acceptor registered")
		}
		sawError = ft == frame.ErrorType
	}

	if _, err := os.Stat("unwanted.bin"); err == nil {
		t.Error("engine wrote the refused file into the working directory")
	}
	p.a.Close()
	wg.Wait()
}

// TestEngineTextRoundTrip: TEXT existed in the protocol and was decoded
// into a no-op, so "send yourself a link" was expressible on the wire and
// impossible in practice.
func TestEngineTextRoundTrip(t *testing.T) {
	p := newPipe(t)
	defer p.close()

	got := make(chan string, 4)
	var wg sync.WaitGroup
	wg.Add(2)

	var eA *Engine
	go func() {
		defer wg.Done()
		var err error
		eA, err = New(Config{Name: "A", Conn: p.a, IsInitiator: true, Confirmer: codePrompt(t)})
		if err != nil {
			t.Errorf("A: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		eB, err := New(Config{Name: "B", Conn: p.b, IsInitiator: false, Confirmer: codePrompt(t)})
		if err != nil {
			t.Errorf("B: %v", err)
			return
		}
		eB.OnText(func(body string) { got <- body })
		go eB.Serve()
	}()
	wg.Wait()
	if eA == nil {
		t.Fatal("initiator engine is nil")
	}

	want := "https://example.com/a-link-i-want-on-my-phone"
	if err := eA.SendText(want); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	select {
	case body := <-got:
		if body != want {
			t.Errorf("received %q, want %q", body, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("TEXT never arrived")
	}
}

func TestEngineSendTextRejectsOversized(t *testing.T) {
	p := newPipe(t)
	defer p.close()
	var wg sync.WaitGroup
	wg.Add(2)
	var eA *Engine
	go func() {
		defer wg.Done()
		var err error
		eA, err = New(Config{Name: "A", Conn: p.a, IsInitiator: true, Confirmer: codePrompt(t)})
		if err != nil {
			t.Errorf("A: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := New(Config{Name: "B", Conn: p.b, IsInitiator: false, Confirmer: codePrompt(t)}); err != nil {
			t.Errorf("B: %v", err)
		}
	}()
	wg.Wait()
	if eA == nil {
		t.Fatal("initiator engine is nil")
	}
	// The frame encoder would reject this anyway; catching it here gives a
	// message that says what to do about it.
	if err := eA.SendText(strings.Repeat("x", frame.MaxTextBytes+1)); err == nil {
		t.Error("oversized text was accepted")
	}
}
