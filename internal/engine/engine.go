// Package engine drives the beamdrop wire protocol over a single
// bidirectional connection (TCP or WebSocket).
//
// The engine is symmetric: both initiator and responder run the same code,
// distinguished only by `IsInitiator` in the config. The engine owns
// pairing, then enters a loop accepting file offers or initiating them.
package engine

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"syscall"

	"github.com/saxill/beamdrop/internal/frame"
	"github.com/saxill/beamdrop/internal/pairing"
	"github.com/saxill/beamdrop/internal/peer"
	"github.com/saxill/beamdrop/internal/transfer"
)

type Confirmer func(initPub, respPub [32]byte, peerName, code string, known bool) bool

// FileAcceptor decides whether to accept an inbound file offer and, if so,
// which *directory* the file should land in.
//
// It deliberately returns a directory, not a full path: offer.Name is
// entirely peer-controlled, so letting the acceptor compose a destination
// path from it (e.g. filepath.Join(inbox, offer.Name)) hands any paired
// peer an arbitrary-file-write primitive — filepath.Join *cleans* "../"
// segments rather than rejecting them, so the joined path can legitimately
// resolve outside the intended inbox. Returning only a directory keeps the
// peer-controlled component confined to the filename, which
// transfer.NewReceiver sanitizes with filepath.Base before joining.
type FileAcceptor func(offer frame.FileOfferPayload) (destDir string, accept bool)

type Config struct {
	Name        string
	Conn        io.ReadWriteCloser
	IsInitiator bool
	Confirmer   Confirmer
	Known       *pairing.KnownPeers // optional; enables TOFU auto-accept + Remember on success
	// Identity is this machine's long-lived keypair. Leave it nil only
	// where the connection is genuinely one-off (tests): a fresh key per
	// connection means the peer sees a stranger every time and TOFU can
	// never take hold. See pairing.LoadOrCreateIdentity.
	Identity *pairing.KeyPair
	// OnReceiveError, if set, is called whenever an inbound transfer fails
	// (bad offer, a write error, or a SHA-256 mismatch on Verify). Serve
	// itself keeps running — one failed inbound transfer must not tear
	// down the connection or any other in-flight transfer on it — so this
	// is the only way a caller learns about a receive-side failure.
	OnReceiveError func(err error)
	// testKeyPair is used only in tests to inject a fixed keypair instead of generating one.
	// It is unexported and should never be set in production code.
	testKeyPair *pairing.KeyPair
}

// frameEvent is a frame routed from the engine's single background reader
// to whichever transfer (an in-progress receiveFile or SendFile) is
// waiting for it.
type frameEvent struct {
	t       frame.Type
	payload []byte
}

type Engine struct {
	cfg          Config
	conn         io.ReadWriteCloser
	keys         pairing.KeyPair
	peerKeys     pairing.KeyPair // remote side
	peerName     string
	sharedKey    [32]byte
	code         string
	mu           sync.Mutex
	onOffer      FileAcceptor                     // guarded by mu — set by OnFileOffer, read by readLoop
	onText       func(body string)                // guarded by mu — set by OnText, read by readLoop
	onFileDone   func(name, path string)          // guarded by mu — set by OnFileReceived, read by receiveFile
	onHistoryReq func() []frame.HistoryEntry      // guarded by mu — set by OnHistoryRequest, read by readLoop
	onHistory    func([]frame.HistoryEntry)       // guarded by mu — set by OnHistory, read by readLoop
	onPushKeyReq func() string                    // guarded by mu — set by OnPushKeyRequest, read by readLoop
	onPushSub    func(frame.PushSubscribePayload) // guarded by mu — set by OnPushSubscribe, read by readLoop
	onPushKey    func(string)                     // guarded by mu — set by OnPushKey, read by readLoop
	onFileReq    func(name string)                // guarded by mu — set by OnFileRequest, read by readLoop

	// writeMu serializes every frame write to conn: receiveFile's writes
	// (FILE_ACCEPT/FILE_DONE/ERROR) and a concurrent SendFile's writes
	// (FILE_OFFER/CHUNK) can both happen on the same connection now, and
	// neither net.Conn.Write nor websocket.Conn.WriteMessage is safe for
	// concurrent callers.
	writeMu sync.Mutex

	// The engine has exactly one background reader for its whole
	// lifetime, started lazily by whichever of Serve or SendFile runs
	// first (readOnce guarantees this). That reader is the only
	// goroutine that ever calls frame.ReadFrame on conn; it dispatches
	// each frame to chunkWait (an in-progress inbound receiveFile) or
	// sendWait (an in-progress outbound SendFile) rather than letting
	// receiveFile/SendFile read the connection directly. This is what
	// lets Serve (handling inbound offers) and SendFile (awaiting
	// outbound responses) run concurrently on one connection without
	// racing on ReadFrame.
	readOnce  sync.Once
	readDone  chan struct{}
	readErr   error
	chunkWait chan frameEvent // non-nil while a receiveFile is in progress
	sendWait  chan frameEvent // non-nil while a SendFile is in progress
	// chunkDone/sendDone are closed by receiveFile/SendFile's own defer
	// when they return, independent of chunkWait/sendWait being nilled
	// out. readLoop selects on these alongside the channel send so a
	// dispatch can never block forever on a consumer that has already
	// exited — a peer that streams a trailing/extra frame past what its
	// own consumer expected must not wedge the whole connection.
	chunkDone chan struct{}
	sendDone  chan struct{}

	// bestEffortOnce/bestEffortQueue back the engine's dedicated
	// best-effort writer goroutine — see startBestEffortWriter.
	bestEffortOnce  sync.Once
	bestEffortQueue chan bestEffortFrame
}

// bestEffortFrame is a frame queued for the engine's best-effort writer.
type bestEffortFrame struct {
	t       frame.Type
	payload any
}

// New constructs an engine, generates a keypair, performs pairing, and returns
// a ready engine. If `IsInitiator` is true, New blocks until pairing completes.
func New(cfg Config) (*Engine, error) {
	if cfg.Confirmer == nil {
		return nil, fmt.Errorf("engine: Confirmer required")
	}
	var kp pairing.KeyPair
	switch {
	case cfg.testKeyPair != nil: // test-injected; never set in production
		kp = *cfg.testKeyPair
	case cfg.Identity != nil:
		kp = *cfg.Identity
	default:
		var err error
		kp, err = pairing.Generate()
		if err != nil {
			return nil, err
		}
	}
	e := &Engine{
		cfg:  cfg,
		conn: cfg.Conn,
		keys: kp,
	}
	if err := e.pair(); err != nil {
		return nil, fmt.Errorf("engine: pair: %w", err)
	}
	return e, nil
}

func decodeHello(b []byte) (frame.HelloPayload, error) {
	if len(b) < 4 {
		return frame.HelloPayload{}, fmt.Errorf("hello: too short")
	}
	p := frame.HelloPayload{Mode: b[0], Capabilities: binary.LittleEndian.Uint16(b[1:3])}
	nameLen := int(b[3])
	if 4+nameLen > len(b) {
		return p, fmt.Errorf("hello: bad name len")
	}
	p.Name = string(b[4 : 4+nameLen])
	return p, nil
}

func (e *Engine) pair() error {
	// Step 1: HELLO + pubkey tail.
	// To avoid TCP byte-stream interleaving races between the two sides
	// writing concurrently, the initiator writes first; the responder
	// reads first. Then both exchange PAIR_OK.
	name := e.cfg.Name
	if len(name) > 32 {
		name = name[:32]
	}
	helloPayload := frame.HelloPayload{
		Name:         name,
		Mode:         0x05, // portal+send for now
		Capabilities: 0,
	}
	if e.cfg.IsInitiator {
		if err := frame.WriteFrame(e.conn, frame.HelloType, helloPayload); err != nil {
			return err
		}
	}
	// Each side now reads the other's HELLO frame.
	_, peerHelloPayload, err := frame.ReadFrame(e.conn)
	if err != nil {
		return err
	}
	peerHello, err := decodeHello(peerHelloPayload)
	if err != nil {
		return err
	}
	e.peerName = peerHello.Name
	if !e.cfg.IsInitiator {
		if err := frame.WriteFrame(e.conn, frame.HelloType, helloPayload); err != nil {
			return err
		}
	}
	// Exchange pubkeys as raw 32-byte tails, after both HELLOs are framed.
	// Initiator writes first, then reads.
	if e.cfg.IsInitiator {
		if _, err := e.conn.Write(e.keys.Pub[:]); err != nil {
			return err
		}
	}
	var peerPub [32]byte
	if _, err := io.ReadFull(e.conn, peerPub[:]); err != nil {
		return err
	}
	if !e.cfg.IsInitiator {
		if _, err := e.conn.Write(e.keys.Pub[:]); err != nil {
			return err
		}
	}
	e.peerKeys = pairing.KeyPair{Pub: peerPub}
	// Compute known (TOFU check)
	known := false
	if e.cfg.Known != nil {
		_, known = e.cfg.Known.IsKnown(peerPub)
	}
	// Derive code
	if e.cfg.IsInitiator {
		e.code = pairing.Code(e.keys.Pub, peerPub)
	} else {
		e.code = pairing.Code(peerPub, e.keys.Pub)
	}
	// Derive shared key (before the HMAC ceremony and confirmer call)
	e.sharedKey = pairing.SharedKey(e.keys.Priv, peerPub, e.code)
	// Step 2: PAIR_CHALLENGE/PAIR_RESPONSE — cryptographic proof both
	// sides derived the same shared key, before bothering the human with
	// a code-confirmation prompt. Same write/read-first discipline as
	// every other step: initiator writes the challenge; responder reads
	// it, then writes the response; initiator reads the response.
	var initNonce, respNonce [32]byte
	var gotHMAC [32]byte
	if e.cfg.IsInitiator {
		if _, err := rand.Read(initNonce[:]); err != nil {
			return err
		}
		if err := frame.WriteFrame(e.conn, frame.PairChallengeType, frame.PairChallengePayload{Nonce: initNonce}); err != nil {
			return err
		}
		t, payload, err := frame.ReadFrame(e.conn)
		if err != nil {
			return err
		}
		if t != frame.PairResponseType || len(payload) != 64 {
			return fmt.Errorf("engine: expected PAIR_RESPONSE, got 0x%02x", t)
		}
		copy(respNonce[:], payload[:32])
		copy(gotHMAC[:], payload[32:])
		if !pairing.Verify(e.sharedKey, initNonce, respNonce, gotHMAC) {
			return fmt.Errorf("engine: pairing HMAC verification failed")
		}
	} else {
		t, payload, err := frame.ReadFrame(e.conn)
		if err != nil {
			return err
		}
		if t != frame.PairChallengeType || len(payload) != 32 {
			return fmt.Errorf("engine: expected PAIR_CHALLENGE, got 0x%02x", t)
		}
		copy(initNonce[:], payload)
		if _, err := rand.Read(respNonce[:]); err != nil {
			return err
		}
		mac := pairing.ComputeHMAC(e.sharedKey, initNonce, respNonce)
		if err := frame.WriteFrame(e.conn, frame.PairResponseType, frame.PairResponsePayload{ResponderNonce: respNonce, HMAC: mac}); err != nil {
			return err
		}
	}
	// For the MVP, the test's Confirmer always returns true. In the real
	// product, this is where the TUI / web UI prompts the user.
	if !e.cfg.Confirmer(e.keys.Pub, peerPub, e.peerName, e.code, known) {
		return fmt.Errorf("engine: pairing rejected by user")
	}
	// Exchange PAIR_OK frames.
	// Initiator writes first; responder reads first.
	if e.cfg.IsInitiator {
		if err := frame.WriteFrame(e.conn, frame.PairOKType, frame.PairOKPayload{
			PeerName: e.cfg.Name,
			PubKey:   e.keys.Pub,
		}); err != nil {
			return err
		}
	}
	t, payload, err := frame.ReadFrame(e.conn)
	if err != nil {
		return err
	}
	if t != frame.PairOKType {
		return fmt.Errorf("engine: expected PAIR_OK, got 0x%02x", t)
	}
	if len(payload) < 33 {
		return fmt.Errorf("engine: PAIR_OK too short")
	}
	nameLen := int(payload[0])
	if 1+nameLen+32 != len(payload) {
		return fmt.Errorf("engine: PAIR_OK length mismatch")
	}
	var gotPub [32]byte
	copy(gotPub[:], payload[1+nameLen:])
	if gotPub != peerPub {
		return fmt.Errorf("engine: peer pubkey mismatch")
	}
	if !e.cfg.IsInitiator {
		if err := frame.WriteFrame(e.conn, frame.PairOKType, frame.PairOKPayload{
			PeerName: e.cfg.Name,
			PubKey:   e.keys.Pub,
		}); err != nil {
			return err
		}
	}
	// After the PAIR_OK exchange succeeds, remember the peer if a known-peers
	// store is configured. Ignore errors — a failed write to the known-peers
	// file shouldn't fail an otherwise-successful pairing.
	if e.cfg.Known != nil {
		// Record where this peer was reached. Broadcast discovery does not
		// cross a tailnet, so a remembered address is the only way a later
		// `beamdrop send` finds a laptop that is not on this WiFi.
		remembered := peer.Peer{Name: e.peerName, PubKey: peerPub}
		if c, ok := e.conn.(interface{ RemoteAddr() net.Addr }); ok {
			if host, _, err := net.SplitHostPort(c.RemoteAddr().String()); err == nil {
				remembered.LastIP = host
			}
		}
		_ = e.cfg.Known.Remember(remembered)
	}
	return nil
}

// startReader lazily starts the engine's single background reader
// goroutine. Both Serve and SendFile call this — whichever runs first
// actually starts it (sync.Once); later callers just share it. Guarantees
// exactly one goroutine ever calls frame.ReadFrame on e.conn for the
// engine's whole lifetime.
func (e *Engine) startReader() {
	e.readOnce.Do(func() {
		e.readDone = make(chan struct{})
		go e.readLoop()
	})
}

// readLoop is the engine's sole connection reader. It dispatches each
// frame to the transfer currently waiting for it: FILE_OFFER starts a new
// inbound receiveFile (in its own goroutine, since it must not block this
// loop from continuing to read); CHUNK routes to that receive's channel;
// FILE_ACCEPT/ACK/FILE_DONE/ERROR route to an in-progress outbound
// SendFile's channel. At most one inbound and one outbound transfer are
// supported at a time per engine — matching how every caller in this
// codebase actually uses one Engine (one peer connection) today, and
// required by the wire format itself: ERROR carries no transfer ID, so
// routing an error response to a specific in-flight transfer by ID isn't
// possible even if multiple were allowed.
func (e *Engine) readLoop() {
	defer close(e.readDone)
	defer func() {
		e.mu.Lock()
		if e.sendWait != nil {
			close(e.sendWait)
			e.sendWait = nil
		}
		if e.chunkWait != nil {
			close(e.chunkWait)
			e.chunkWait = nil
		}
		e.mu.Unlock()
	}()
	for {
		t, payload, err := frame.ReadFrame(e.conn)
		if err != nil {
			e.mu.Lock()
			e.readErr = err
			e.mu.Unlock()
			return
		}
		switch t {
		case frame.FileOfferType:
			offer, err := decodeFileOffer(payload)
			if err != nil {
				e.mu.Lock()
				e.readErr = err
				e.mu.Unlock()
				return
			}
			e.mu.Lock()
			onOffer := e.onOffer
			alreadyReceiving := e.chunkWait != nil
			e.mu.Unlock()
			if alreadyReceiving {
				// A second FILE_OFFER while one is still in flight is a
				// peer protocol error, not a reason to kill the whole
				// connection (which would also abort any unrelated
				// in-flight SendFile on this engine). Reject it before
				// even asking the acceptor — firing prompts/side effects
				// for an offer we're about to refuse would be surprising
				// — via the best-effort writer: this runs on readLoop's
				// own goroutine, so a blocking write here would stall
				// all frame processing on the connection.
				e.writeBestEffort(frame.ErrorType, frame.ErrorPayload{Code: 5, Message: "a receive is already in progress"})
				continue
			}
			if onOffer == nil {
				// No acceptor registered means this caller never asked to
				// receive anything — a send-only engine like watch mode,
				// whose SendFile still starts this read loop. Refuse rather
				// than inventing a destination: the previous default
				// ("." — the process working directory) let any paired peer
				// drop files wherever beamdrop happened to be launched
				// from.
				e.writeBestEffort(frame.ErrorType, frame.ErrorPayload{Code: 7, Message: "this peer does not accept incoming files"})
				continue
			}
			destDir, ok := onOffer(offer)
			if !ok {
				continue
			}
			ch := make(chan frameEvent, 64)
			done := make(chan struct{})
			e.mu.Lock()
			e.chunkWait = ch
			e.chunkDone = done
			e.mu.Unlock()
			go e.receiveFile(offer, destDir, ch, done)
		case frame.ChunkType:
			e.mu.Lock()
			ch, done := e.chunkWait, e.chunkDone
			e.mu.Unlock()
			if ch != nil {
				select {
				case ch <- frameEvent{t, payload}:
				case <-done:
					// receiveFile already returned (e.g. it hit an error
					// or the offer's declared Size was reached) but the
					// peer kept streaming past that point. Drop the
					// frame instead of blocking readLoop forever on a
					// consumer that will never read again.
				}
			}
		case frame.FileAcceptType, frame.FileDoneType, frame.ErrorType:
			e.mu.Lock()
			ch, done := e.sendWait, e.sendDone
			e.mu.Unlock()
			if ch != nil {
				select {
				case ch <- frameEvent{t, payload}:
				case <-done:
				}
			}
		case frame.AckType:
			// ACKs are purely advisory in this MVP — SendFile never
			// waits on them (it only needs to see a terminal
			// FILE_DONE/ERROR) and explicitly skips any it happens to
			// see in its wait loop below. Since nothing ever consumes
			// them, don't even attempt to route them into sendWait: on
			// a large transfer the receiver ACKs every chunk, and
			// SendFile doesn't drain sendWait at all during its
			// chunk-streaming loop, so enough queued ACKs would fill
			// sendWait entirely — which would then make the *next*
			// case's blocking dispatch (a genuine FILE_ACCEPT/
			// FILE_DONE/ERROR) wait on room that never frees up.
			continue
		case frame.TextType:
			e.mu.Lock()
			onText := e.onText
			e.mu.Unlock()
			if onText != nil {
				// On its own goroutine: a handler that blocks (a UI queue,
				// a disk write) must not stop this loop reading the
				// connection.
				go onText(string(payload))
			}
			continue
		case frame.HistoryRequestType:
			e.mu.Lock()
			fn := e.onHistoryReq
			e.mu.Unlock()
			// On its own goroutine for the same reason as TEXT: building
			// the answer reads a directory, and this loop must keep
			// draining the connection while that happens.
			go func() {
				var entries []frame.HistoryEntry
				if fn != nil {
					entries = fn()
				}
				// An unanswered request leaves the asker waiting forever,
				// so even "I have nothing" is sent.
				_ = e.writeFrame(frame.HistoryType, frame.HistoryPayload{Entries: entries})
			}()
			continue
		case frame.HistoryType:
			e.mu.Lock()
			fn := e.onHistory
			e.mu.Unlock()
			if fn != nil {
				entries, err := frame.DecodeHistory(payload)
				if err != nil {
					// Bad history is not worth dropping a working
					// connection over — the live conversation still works.
					if e.cfg.OnReceiveError != nil {
						e.cfg.OnReceiveError(fmt.Errorf("engine: history: %w", err))
					}
					continue
				}
				go fn(entries)
			}
			continue
		case frame.PushKeyRequestType:
			e.mu.Lock()
			fn := e.onPushKeyReq
			e.mu.Unlock()
			var key string
			if fn != nil {
				key = fn()
			}
			// Answered even when empty, so a page that asked is not left
			// waiting on a reply that never comes.
			e.writeBestEffort(frame.PushKeyType, frame.PushKeyPayload{Key: key})
			continue
		case frame.FileRequestType:
			e.mu.Lock()
			fn := e.onFileReq
			e.mu.Unlock()
			if fn != nil {
				// Detached: the handler sends a whole file, and this loop
				// has to keep reading the connection while that happens —
				// SendFile's own responses arrive here.
				go fn(string(payload))
			}
			continue
		case frame.PushKeyType:
			e.mu.Lock()
			fn := e.onPushKey
			e.mu.Unlock()
			if fn != nil {
				go fn(string(payload))
			}
			continue
		case frame.PushSubscribeType:
			e.mu.Lock()
			fn := e.onPushSub
			e.mu.Unlock()
			if fn == nil {
				continue
			}
			sub, err := frame.DecodePushSubscribe(payload)
			if err != nil {
				// A malformed registration is not worth dropping a working
				// connection over.
				if e.cfg.OnReceiveError != nil {
					e.cfg.OnReceiveError(fmt.Errorf("engine: push subscribe: %w", err))
				}
				continue
			}
			go fn(sub)
			continue
		default:
			e.mu.Lock()
			e.readErr = fmt.Errorf("engine: unsupported frame 0x%02x in serve loop", t)
			e.mu.Unlock()
			return
		}
	}
}

// IsDisconnect reports whether err is an ordinary hang-up rather than a
// failure worth showing anyone.
//
// A clean close surfaces as io.EOF, but a peer that closes while the kernel
// still holds bytes it never read gets RST instead of FIN, and the other
// side sees ECONNRESET. That happens routinely here: ACKs are advisory and
// written asynchronously, so a sender can finish on FILE_DONE and close
// with a trailing ACK still unread. Both endings mean the same thing to a
// caller — this connection is over — and neither is worth logging as a
// problem.
func IsDisconnect(err error) bool {
	if err == nil {
		return true
	}
	return errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE)
}

// Serve runs the receive loop. Returns io.EOF or a frame error on disconnect.
func (e *Engine) Serve() error {
	e.startReader()
	<-e.readDone
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.readErr
}

func (e *Engine) OnFileOffer(fn FileAcceptor) {
	e.mu.Lock()
	e.onOffer = fn
	e.mu.Unlock()
}

// OnText registers a handler for TEXT frames from this peer.
func (e *Engine) OnText(fn func(body string)) {
	e.mu.Lock()
	e.onText = fn
	e.mu.Unlock()
}

// OnHistoryRequest supplies what to answer a peer's HISTORY_REQUEST with.
// Leaving it unset means the request is answered with an empty list rather
// than ignored: a page waiting for a reply that never comes would sit on a
// spinner forever.
func (e *Engine) OnHistoryRequest(fn func() []frame.HistoryEntry) {
	e.mu.Lock()
	e.onHistoryReq = fn
	e.mu.Unlock()
}

// OnHistory receives the answer to RequestHistory.
func (e *Engine) OnHistory(fn func(entries []frame.HistoryEntry)) {
	e.mu.Lock()
	e.onHistory = fn
	e.mu.Unlock()
}

// RequestHistory asks the peer what has already passed between the two of
// us. Only a peer new enough to understand the frame should be asked — see
// HistoryRequestType.
func (e *Engine) RequestHistory() error {
	return e.writeFrame(frame.HistoryRequestType, nil)
}

// OnPushKeyRequest supplies the VAPID public key a page needs before it can
// subscribe. Returning "" means this portal cannot do push, which the page
// treats as "do not offer notifications" rather than as a failure.
func (e *Engine) OnPushKeyRequest(fn func() string) {
	e.mu.Lock()
	e.onPushKeyReq = fn
	e.mu.Unlock()
}

// OnPushSubscribe receives a registration from a paired peer.
func (e *Engine) OnPushSubscribe(fn func(frame.PushSubscribePayload)) {
	e.mu.Lock()
	e.onPushSub = fn
	e.mu.Unlock()
}

// OnFileRequest is called when the peer asks for a file by name. The
// handler is expected to send it, which is why it gets the engine.
func (e *Engine) OnFileRequest(fn func(name string)) {
	e.mu.Lock()
	e.onFileReq = fn
	e.mu.Unlock()
}

// RequestFile asks the peer to send a file it already has.
func (e *Engine) RequestFile(name string) error {
	return e.writeFrame(frame.FileRequestType, frame.FileRequestPayload{Name: name})
}

// OnPushKey receives the answer to RequestPushKey. An empty key means the
// peer cannot send notifications at all.
func (e *Engine) OnPushKey(fn func(key string)) {
	e.mu.Lock()
	e.onPushKey = fn
	e.mu.Unlock()
}

// RequestPushKey asks for the VAPID public key needed to subscribe.
func (e *Engine) RequestPushKey() error {
	return e.writeFrame(frame.PushKeyRequestType, nil)
}

// SendPushSubscription registers this peer for notifications.
func (e *Engine) SendPushSubscription(p frame.PushSubscribePayload) error {
	return e.writeFrame(frame.PushSubscribeType, p)
}

// OnFileReceived registers a handler called once an inbound file is written
// and its hash verified — never for one that failed, so a caller can treat
// it as "this file is now really here".
//
// OnFileOffer cannot serve this purpose: it fires before a single byte has
// arrived, so anything acting on the file would be racing the transfer.
func (e *Engine) OnFileReceived(fn func(name, path string)) {
	e.mu.Lock()
	e.onFileDone = fn
	e.mu.Unlock()
}

// SendText sends a short message. Unlike a file it is fire-and-forget:
// TEXT carries no id, so there is no acknowledgement to wait for.
func (e *Engine) SendText(body string) error {
	if len(body) > frame.MaxTextBytes {
		return fmt.Errorf("engine: message is %d bytes, limit is %d", len(body), frame.MaxTextBytes)
	}
	return e.writeFrame(frame.TextType, frame.TextPayload{Body: body})
}

func (e *Engine) OwnPubKey() [32]byte  { return e.keys.Pub }
func (e *Engine) PeerPubKey() [32]byte { return e.peerKeys.Pub }
func (e *Engine) PeerName() string     { return e.peerName }

// writeFrame serializes every frame write against concurrent writers —
// receiveFile and a concurrent SendFile both write to the same
// connection, and neither net.Conn.Write nor websocket.Conn.WriteMessage
// is safe for concurrent callers.
func (e *Engine) writeFrame(t frame.Type, payload any) error {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	return frame.WriteFrame(e.conn, t, payload)
}

// startBestEffortWriter lazily starts the engine's dedicated goroutine for
// best-effort frame writes: ACKs (from receiveFile) and readLoop's own
// outbound rejections (e.g. a duplicate-FILE_OFFER error). These writes
// must never block whichever goroutine wants to send one — receiveFile's
// drain loop and readLoop itself both depend on always being able to
// proceed regardless of the connection's write-side backpressure.
//
// A non-blocking mutex (TryLock on writeMu) is not enough on its own: it
// only avoids blocking on *acquiring* the lock, not on the conn.Write call
// made while holding it, which can itself block indefinitely on a full
// socket send buffer. If receiveFile's ACK write blocks there, it stops
// draining chunkWait, which stalls readLoop's dispatch, which stops
// readLoop from reading the connection at all — mirrored on the peer
// engine under a large enough bidirectional transfer, that's a permanent
// circular deadlock. Routing these writes through a bounded, drop-on-full
// queue and a goroutine that's allowed to block means the caller never
// does.
func (e *Engine) startBestEffortWriter() {
	e.bestEffortOnce.Do(func() {
		e.bestEffortQueue = make(chan bestEffortFrame, 64)
		go func() {
			for f := range e.bestEffortQueue {
				if err := e.writeFrame(f.t, f.payload); err != nil {
					// Connection is dead; further writes will fail too.
					// Stop draining — anything still queued is simply
					// dropped, which is fine since every frame sent
					// through this path is best-effort by definition.
					return
				}
			}
		}()
	})
}

// writeBestEffort enqueues a frame for the best-effort writer, dropping it
// immediately if the queue is full rather than blocking the caller.
func (e *Engine) writeBestEffort(t frame.Type, payload any) {
	e.startBestEffortWriter()
	select {
	case e.bestEffortQueue <- bestEffortFrame{t, payload}:
	default:
	}
}

// receiveFile accepts the file offer, streams chunks to disk, and sends the
// final FILE_DONE or ERROR frame. It runs in its own goroutine, started by
// readLoop when a FILE_OFFER arrives — it never reads e.conn directly; ch
// delivers CHUNK frames routed by readLoop, so it can run concurrently with
// an outbound SendFile without racing readLoop for the connection.
//
// Any failure here is reported via Config.OnReceiveError (if set) rather
// than returned: this runs detached from Serve's caller, and one failed
// inbound transfer — a bad offer, a write error, a checksum mismatch —
// must not tear down the connection or any other transfer on it.
func (e *Engine) receiveFile(offer frame.FileOfferPayload, destDir string, ch <-chan frameEvent, done chan struct{}) {
	defer close(done)
	defer func() {
		e.mu.Lock()
		e.chunkWait = nil
		e.chunkDone = nil
		e.mu.Unlock()
	}()
	reportErr := func(err error) {
		if e.cfg.OnReceiveError != nil {
			e.cfg.OnReceiveError(err)
		}
	}
	rcv, err := transfer.NewReceiver(offer, destDir)
	if err != nil {
		_ = e.writeFrame(frame.ErrorType, frame.ErrorPayload{Code: 3, Message: err.Error()})
		reportErr(fmt.Errorf("engine: receive %s: %w", offer.Name, err))
		return
	}
	if err := e.writeFrame(frame.FileAcceptType, frame.FileAcceptPayload{
		ID:         offer.ID,
		ResumeFrom: 0,
	}); err != nil {
		reportErr(fmt.Errorf("engine: receive %s: send FILE_ACCEPT: %w", offer.Name, err))
		return
	}
	for ev := range ch {
		c := decodeChunk(ev.payload)
		if _, err := rcv.WriteChunk(c); err != nil {
			_ = e.writeFrame(frame.ErrorType, frame.ErrorPayload{Code: 6, Message: err.Error()})
			reportErr(fmt.Errorf("engine: receive %s: write chunk: %w", offer.Name, err))
			return
		}
		// ACKs are advisory (see readLoop's AckType comment) — a failed
		// or dropped one is not fatal to the transfer, so this uses the
		// best-effort writer rather than e.writeFrame.
		e.writeBestEffort(frame.AckType, frame.AckPayload{ID: c.ID, Offset: c.Offset + uint64(len(c.Data))})
		if rcv.Written() >= rcv.Size {
			if err := rcv.Verify(); err != nil {
				_ = e.writeFrame(frame.ErrorType, frame.ErrorPayload{Code: 4, Message: err.Error()})
				reportErr(fmt.Errorf("engine: receive %s: %w", offer.Name, err))
				return
			}
			if err := e.writeFrame(frame.FileDoneType, frame.FileDonePayload{ID: rcv.ID, SHA256: rcv.SHA256}); err != nil {
				reportErr(fmt.Errorf("engine: receive %s: send FILE_DONE: %w", offer.Name, err))
			}
			// After Verify, so this only ever fires for a file that is
			// really on disk and really intact. A failed FILE_DONE does not
			// suppress it: the bytes arrived either way, and the sender
			// merely did not hear us say so.
			e.mu.Lock()
			onDone := e.onFileDone
			e.mu.Unlock()
			if onDone != nil {
				onDone(rcv.Name, rcv.Dest)
			}
			return
		}
	}
	// ch was closed (readLoop exiting because the connection died) before
	// the transfer completed.
	reportErr(fmt.Errorf("engine: receive %s: connection closed before transfer completed", offer.Name))
}

// SendFile drives the sender half: offer, accept, stream chunks, await
// FILE_DONE. Starts the engine's shared reader if it isn't already
// running (e.g. a one-shot send with no Serve call), so this works
// standalone as well as concurrently with an active Serve loop.
func (e *Engine) SendFile(s *transfer.Sender, accept func(transfer.Receiver) bool) error {
	e.startReader()

	e.mu.Lock()
	if e.sendWait != nil {
		e.mu.Unlock()
		return fmt.Errorf("engine: a send is already in progress on this connection")
	}
	ch := make(chan frameEvent, 64)
	done := make(chan struct{})
	e.sendWait = ch
	e.sendDone = done
	readDone := e.readDone // safe to read: startReader() already ran, so this is set
	e.mu.Unlock()
	defer func() {
		close(done)
		e.mu.Lock()
		e.sendWait = nil
		e.sendDone = nil
		e.mu.Unlock()
	}()

	offer, err := s.Offer()
	if err != nil {
		return err
	}
	if err := e.writeFrame(frame.FileOfferType, offer); err != nil {
		return err
	}
	// Wait for FILE_ACCEPT, routed by readLoop. Also select on readDone:
	// if the reader has already exited (connection dead, or this engine
	// is stale in a registry) before or while we're waiting, don't hang
	// forever — nothing will ever feed ch again.
	ev, err := recvFrame(ch, readDone)
	if err != nil {
		return e.recvErr(err)
	}
	if ev.t != frame.FileAcceptType {
		return fmt.Errorf("engine: expected FILE_ACCEPT, got 0x%02x", ev.t)
	}
	acceptP := decodeFileAccept(ev.payload)
	if acceptP.ID != offer.ID {
		return fmt.Errorf("engine: accept id mismatch")
	}
	// Resume support: would set s.offset = acceptP.ResumeFrom here; for the
	// MVP we ignore ResumeFrom and stream from the beginning. The sender's
	// offset field is unexported; resume is left for a future iteration.
	_ = acceptP.ResumeFrom
	// Stream chunks; send an ACK back-pressure if needed (not implemented in MVP)
	for {
		c, more, err := s.NextChunk(64 * 1024)
		if err != nil {
			return err
		}
		if err := e.writeFrame(frame.ChunkType, c); err != nil {
			return err
		}
		if !more {
			break
		}
	}
	// Wait for FILE_DONE or ERROR, routed by readLoop. The receiver may
	// interleave ACK frames after each chunk, so skip those and keep
	// waiting until we see a terminal frame.
	for {
		ev, err := recvFrame(ch, readDone)
		if err != nil {
			return e.recvErr(err)
		}
		if ev.t == frame.ErrorType {
			return fmt.Errorf("engine: receiver reported error")
		}
		if ev.t == frame.FileDoneType {
			return nil
		}
		// otherwise (ACK, etc.) keep waiting
	}
}

// recvFrame waits for the next frameEvent on ch, or reports that the
// connection's reader has exited (readDone closed) or ch itself was
// closed (readLoop's own shutdown, racing the readDone check) first.
func recvFrame(ch <-chan frameEvent, readDone <-chan struct{}) (frameEvent, error) {
	select {
	case ev, ok := <-ch:
		if !ok {
			return frameEvent{}, io.ErrClosedPipe
		}
		return ev, nil
	case <-readDone:
		return frameEvent{}, io.ErrClosedPipe
	}
}

// recvErr turns a recvFrame failure into a caller-facing error, preferring
// the engine's actual read error (a real cause: EOF, a reset connection,
// a bad frame) over the generic sentinel recvFrame returns when it can't
// tell which of ch/readDone fired first.
func (e *Engine) recvErr(_ error) error {
	e.mu.Lock()
	readErr := e.readErr
	e.mu.Unlock()
	if readErr != nil {
		return fmt.Errorf("engine: connection closed: %w", readErr)
	}
	return fmt.Errorf("engine: connection closed while awaiting a response")
}

// --- payload decoders (mirror frame.encodePayload) ---

func decodeFileOffer(b []byte) (frame.FileOfferPayload, error) {
	if len(b) < 17 {
		return frame.FileOfferPayload{}, fmt.Errorf("file offer: too short")
	}
	p := frame.FileOfferPayload{
		ID:   binary.LittleEndian.Uint64(b[:8]),
		Size: binary.LittleEndian.Uint64(b[8:16]),
	}
	off := 16
	nameLen := int(b[off])
	off++
	if off+nameLen > len(b) {
		return p, fmt.Errorf("file offer: bad name len")
	}
	p.Name = string(b[off : off+nameLen])
	off += nameLen
	if off >= len(b) {
		return p, fmt.Errorf("file offer: no mime len")
	}
	mimeLen := int(b[off])
	off++
	if off+mimeLen+32 > len(b) {
		return p, fmt.Errorf("file offer: bad mime len")
	}
	p.MIME = string(b[off : off+mimeLen])
	off += mimeLen
	copy(p.SHA256[:], b[off:off+32])
	return p, nil
}

func decodeChunk(b []byte) frame.ChunkPayload {
	c := frame.ChunkPayload{
		ID:     binary.LittleEndian.Uint64(b[:8]),
		Offset: binary.LittleEndian.Uint64(b[8:16]),
	}
	if len(b) > 16 {
		c.Data = make([]byte, len(b)-16)
		copy(c.Data, b[16:])
	}
	return c
}

func decodeFileAccept(b []byte) frame.FileAcceptPayload {
	return frame.FileAcceptPayload{
		ID:         binary.LittleEndian.Uint64(b[:8]),
		ResumeFrom: binary.LittleEndian.Uint64(b[8:16]),
	}
}
