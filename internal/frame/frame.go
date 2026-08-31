package frame

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const maxFrameLen = 1 << 24 // 16 MiB cap; CHUNK payload is ≤64KB so this is generous

// Encode serializes a payload into a full frame: [len:u32][type:u8][payload].
// `len` includes the type byte.
func Encode(t Type, payload any) ([]byte, error) {
	body, err := encodePayload(t, payload)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 4+1+len(body))
	binary.LittleEndian.PutUint32(out[:4], uint32(1+len(body)))
	out[4] = uint8(t)
	copy(out[5:], body)
	return out, nil
}

// Decode parses a full frame, returning the type and payload bytes.
func Decode(buf []byte) (Type, []byte, error) {
	if len(buf) < 5 {
		return 0, nil, io.ErrUnexpectedEOF
	}
	ln := binary.LittleEndian.Uint32(buf[:4])
	if ln < 1 || uint32(len(buf)-4) < ln {
		return 0, nil, io.ErrUnexpectedEOF
	}
	return Type(buf[4]), buf[5 : 4+ln], nil
}

// WriteFrame encodes and writes a single frame.
func WriteFrame(w io.Writer, t Type, payload any) error {
	buf, err := Encode(t, payload)
	if err != nil {
		return err
	}
	_, err = w.Write(buf)
	return err
}

// ReadFrame reads exactly one frame.
func ReadFrame(r io.Reader) (Type, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:4]); err != nil {
		return 0, nil, err
	}
	ln := binary.LittleEndian.Uint32(hdr[:4])
	if ln < 1 || ln > maxFrameLen {
		return 0, nil, fmt.Errorf("frame: bad length %d", ln)
	}
	if _, err := io.ReadFull(r, hdr[4:5]); err != nil {
		return 0, nil, err
	}
	payload := make([]byte, ln-1)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return Type(hdr[4]), payload, nil
}

// encodePayload is a small switch over known payload types.
// Using reflection would be more general; we keep it explicit and obvious.
func encodePayload(t Type, payload any) ([]byte, error) {
	switch t {
	case HelloType:
		p, ok := payload.(HelloPayload)
		if !ok {
			return nil, fmt.Errorf("frame: HELLO wants HelloPayload, got %T", payload)
		}
		return encodeHello(p)
	case PairChallengeType:
		p := payload.(PairChallengePayload)
		return p.Nonce[:], nil
	case PairResponseType:
		p := payload.(PairResponsePayload)
		out := make([]byte, 64)
		copy(out[:32], p.ResponderNonce[:])
		copy(out[32:], p.HMAC[:])
		return out, nil
	case PairOKType:
		p := payload.(PairOKPayload)
		name, err := encodeShortString(p.PeerName, 32)
		if err != nil {
			return nil, err
		}
		out := make([]byte, 1+len(name)+32)
		out[0] = uint8(len(name))
		copy(out[1:1+len(name)], name)
		copy(out[1+len(name):], p.PubKey[:])
		return out, nil
	case FileOfferType:
		p := payload.(FileOfferPayload)
		name, err := encodeShortString(p.Name, 255)
		if err != nil {
			return nil, err
		}
		mime, err := encodeShortString(p.MIME, 64)
		if err != nil {
			return nil, err
		}
		// [id:u64 LE][size:u64 LE][name_len:u8][name][mime_len:u8][mime][sha256]
		out := make([]byte, 0, 8+8+1+len(name)+1+len(mime)+32)
		out = binary.LittleEndian.AppendUint64(out, p.ID)
		out = binary.LittleEndian.AppendUint64(out, p.Size)
		out = append(out, uint8(len(name)))
		out = append(out, name...)
		out = append(out, uint8(len(mime)))
		out = append(out, mime...)
		out = append(out, p.SHA256[:]...)
		return out, nil
	case FileAcceptType:
		p := payload.(FileAcceptPayload)
		out := make([]byte, 16)
		binary.LittleEndian.PutUint64(out[:8], p.ID)
		binary.LittleEndian.PutUint64(out[8:], p.ResumeFrom)
		return out, nil
	case ChunkType:
		p := payload.(ChunkPayload)
		if len(p.Data) > 65536 {
			return nil, errors.New("frame: CHUNK data > 64KB")
		}
		out := make([]byte, 16+len(p.Data))
		binary.LittleEndian.PutUint64(out[:8], p.ID)
		binary.LittleEndian.PutUint64(out[8:16], p.Offset)
		copy(out[16:], p.Data)
		return out, nil
	case AckType:
		p := payload.(AckPayload)
		out := make([]byte, 16)
		binary.LittleEndian.PutUint64(out[:8], p.ID)
		binary.LittleEndian.PutUint64(out[8:], p.Offset)
		return out, nil
	case FileDoneType:
		p := payload.(FileDonePayload)
		out := make([]byte, 8+32)
		binary.LittleEndian.PutUint64(out[:8], p.ID)
		copy(out[8:], p.SHA256[:])
		return out, nil
	case TextType:
		p := payload.(TextPayload)
		if len(p.Body) > 4096 {
			return nil, errors.New("frame: TEXT body > 4096")
		}
		return []byte(p.Body), nil
	case HistoryRequestType:
		return nil, nil // the frame itself is the whole question
	case HistoryType:
		p := payload.(HistoryPayload)
		entries := p.Entries
		if entries == nil {
			// json.Marshal turns a nil slice into "null"; the browser wants
			// an array it can iterate without a special case.
			entries = []HistoryEntry{}
		}
		b, err := json.Marshal(entries)
		if err != nil {
			return nil, fmt.Errorf("frame: HISTORY: %w", err)
		}
		if len(b) > MaxHistoryBytes {
			return nil, fmt.Errorf("frame: HISTORY is %d bytes, limit is %d", len(b), MaxHistoryBytes)
		}
		return b, nil
	case PushKeyRequestType:
		return nil, nil
	case PushKeyType:
		p := payload.(PushKeyPayload)
		return []byte(p.Key), nil
	case PushSubscribeType:
		p := payload.(PushSubscribePayload)
		b, err := json.Marshal(p)
		if err != nil {
			return nil, fmt.Errorf("frame: PUSH_SUBSCRIBE: %w", err)
		}
		if len(b) > MaxPushBytes {
			return nil, fmt.Errorf("frame: PUSH_SUBSCRIBE is %d bytes, limit is %d", len(b), MaxPushBytes)
		}
		return b, nil
	case FileRequestType:
		p := payload.(FileRequestPayload)
		name, err := encodeShortString(p.Name, 255)
		if err != nil {
			return nil, err
		}
		return name, nil
	case ErrorType:
		p := payload.(ErrorPayload)
		msg, err := encodeShortString(p.Message, 255)
		if err != nil {
			return nil, err
		}
		out := make([]byte, 1+len(msg))
		out[0] = p.Code
		copy(out[1:], msg)
		return out, nil
	default:
		return nil, fmt.Errorf("frame: unknown type 0x%02x", t)
	}
}

// DecodeHistory parses a HISTORY payload. An empty payload is a valid
// "nothing yet" rather than an error, so a fresh portal answering its first
// request does not look like a protocol fault.
func DecodeHistory(payload []byte) ([]HistoryEntry, error) {
	if len(payload) == 0 {
		return nil, nil
	}
	if len(payload) > MaxHistoryBytes {
		return nil, fmt.Errorf("frame: HISTORY is %d bytes, limit is %d", len(payload), MaxHistoryBytes)
	}
	var entries []HistoryEntry
	if err := json.Unmarshal(payload, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// DecodePushSubscribe parses a registration the browser produced. The
// fields are checked here because they come straight off the wire and are
// about to be used as a URL to POST to.
func DecodePushSubscribe(payload []byte) (PushSubscribePayload, error) {
	var p PushSubscribePayload
	if len(payload) > MaxPushBytes {
		return p, fmt.Errorf("frame: PUSH_SUBSCRIBE is %d bytes, limit is %d", len(payload), MaxPushBytes)
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return p, err
	}
	if p.Endpoint == "" || p.P256dh == "" || p.Auth == "" {
		return p, errors.New("frame: PUSH_SUBSCRIBE is missing endpoint or keys")
	}
	// A push endpoint is an https URL at the browser vendor's service.
	// Anything else is either a mistake or an attempt to aim our outbound
	// requests somewhere of the sender's choosing.
	if !strings.HasPrefix(p.Endpoint, "https://") {
		return p, fmt.Errorf("frame: PUSH_SUBSCRIBE endpoint is not https")
	}
	return p, nil
}

func encodeHello(p HelloPayload) ([]byte, error) {
	name, err := encodeShortString(p.Name, 32)
	if err != nil {
		return nil, err
	}
	// [mode:u8][capabilities:u16 LE][name_len:u8][name]
	out := make([]byte, 1+2+1+len(name))
	out[0] = p.Mode
	binary.LittleEndian.PutUint16(out[1:3], p.Capabilities)
	out[3] = uint8(len(name))
	copy(out[4:], name)
	return out, nil
}

func encodeShortString(s string, max int) ([]byte, error) {
	if len(s) > max {
		return nil, fmt.Errorf("frame: string too long (%d > %d)", len(s), max)
	}
	return []byte(s), nil
}
