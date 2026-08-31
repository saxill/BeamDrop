// Package frame defines the wire format for beamdrop.
//
// All multi-byte fields are little-endian. A frame is:
//
//	+--------+------+----------+
//	| len:u32| type | payload  |  payload is (len-1) bytes
//	+--------+------+----------+
//
// `len` includes the `type` byte.
package frame

type Type uint8

const (
	HelloType         Type = 0x01
	PairChallengeType Type = 0x02
	PairResponseType  Type = 0x03
	PairOKType        Type = 0x04
	FileOfferType     Type = 0x10
	FileAcceptType    Type = 0x11
	ChunkType         Type = 0x12
	AckType           Type = 0x13
	FileDoneType      Type = 0x14
	TextType          Type = 0x80
	// HistoryRequestType asks the other side what has already passed
	// between the two of us. It carries no payload.
	//
	// A request rather than something the portal volunteers on connect,
	// because an unknown frame type is fatal to the connection here — see
	// the readLoop's default case. Only a peer that knows to ask can
	// receive the answer, so `beamdrop send` and any older page keep
	// working untouched.
	HistoryRequestType Type = 0x81
	HistoryType        Type = 0x82
	// Push registration rides this connection rather than an HTTP endpoint
	// so it inherits the pairing that already happened here. An unprotected
	// subscribe endpoint on the tailnet would let any node on it register
	// to receive your filenames.
	PushKeyRequestType Type = 0x83
	PushKeyType        Type = 0x84
	PushSubscribeType  Type = 0x85
	// FileRequestType asks for a file the peer already has, by name. The
	// answer is an ordinary FILE_OFFER, so nothing new is needed to receive
	// it. This is what makes a file listed in history openable: the page
	// holds no bytes for anything it did not receive this session.
	FileRequestType Type = 0x86
	ErrorType       Type = 0xF0
)

// HelloPayload is sent on connect.
type HelloPayload struct {
	Name         string // ≤32 bytes UTF-8
	Mode         uint8  // bitmask: 0x01 portal, 0x02 watch, 0x04 send, 0x08 accepts_incoming
	Capabilities uint16 // reserved for future use
}

type PairChallengePayload struct {
	Nonce [32]byte
}

type PairResponsePayload struct {
	ResponderNonce [32]byte
	HMAC           [32]byte
}

type PairOKPayload struct {
	PeerName string // ≤32 bytes
	PubKey   [32]byte
}

type FileOfferPayload struct {
	ID     uint64
	Name   string // ≤255 bytes
	Size   uint64
	SHA256 [32]byte
	MIME   string // ≤64 bytes
}

type FileAcceptPayload struct {
	ID         uint64
	ResumeFrom uint64
}

type ChunkPayload struct {
	ID     uint64
	Offset uint64
	Data   []byte // ≤65536 bytes
}

type AckPayload struct {
	ID     uint64
	Offset uint64
}

type FileDonePayload struct {
	ID     uint64
	SHA256 [32]byte
}

// MaxTextBytes is the TEXT payload ceiling from the wire spec.
const MaxTextBytes = 4096

type TextPayload struct {
	Body string // ≤4096 bytes UTF-8
}

// MaxHistoryBytes caps the encoded history. A phone on cellular should not
// be made to pull a megabyte of filenames before it can show anything, and
// the payload has to fit in one frame.
const MaxHistoryBytes = 64 * 1024

// HistoryEntry is one thing that already happened, as the other side
// remembers it. It carries no file contents — the bytes are long gone from
// the browser by the time it asks — so a file entry is a record that the
// transfer happened, not something that can be re-saved from the page.
type HistoryEntry struct {
	At   int64  `json:"at"`   // unix seconds
	Kind string `json:"kind"` // "text" or "file"
	// Outbound is from the *asking* peer's point of view: true means the
	// phone sent it. Without this the page cannot tell which side of the
	// conversation a line belongs on.
	Outbound bool   `json:"outbound"`
	Peer     string `json:"peer,omitempty"`
	Text     string `json:"text,omitempty"`
	Name     string `json:"name,omitempty"`
	Size     int64  `json:"size,omitempty"`
}

// FileRequestPayload names a file the sender wants. Only a name — never a
// path. What it resolves against is the receiver's business, and letting a
// peer supply a path is how an inbox turns into a filesystem browser.
type FileRequestPayload struct {
	Name string // ≤255 bytes
}

// MaxPushBytes caps a push registration. A subscription is a URL and two
// short keys; anything near this is not one.
const MaxPushBytes = 4096

// PushKeyPayload carries the server's VAPID public key, which the page
// needs before it can subscribe.
type PushKeyPayload struct {
	Key string
}

// PushSubscribePayload is the registration a browser produced, passed
// through unchanged. The fields are the browser's, not ours.
type PushSubscribePayload struct {
	Endpoint string `json:"endpoint"`
	P256dh   string `json:"p256dh"`
	Auth     string `json:"auth"`
}

// HistoryPayload is JSON rather than the packed encoding every other frame
// uses. The list is variable-length and its records are heterogeneous, so
// hand-rolling a binary format for it would buy nothing but a second
// decoder to keep in step with the browser's.
type HistoryPayload struct {
	Entries []HistoryEntry
}

type ErrorPayload struct {
	Code    uint8
	Message string // ≤255 bytes
}
