package frame

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func TestEncodeHello(t *testing.T) {
	p := HelloPayload{Name: "laptop", Mode: 0x05, Capabilities: 0}
	buf, err := Encode(HelloType, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(buf) < 5 {
		t.Fatalf("frame too short: %d", len(buf))
	}
	gotLen := binary.LittleEndian.Uint32(buf[:4])
	if gotLen != uint32(len(buf)-4) {
		t.Errorf("len field: got %d, want %d", gotLen, len(buf)-4)
	}
	if Type(buf[4]) != HelloType {
		t.Errorf("type: got 0x%02x, want 0x02", buf[4])
	}
}

func TestRoundTripAllTypes(t *testing.T) {
	cases := []struct {
		name    string
		t       Type
		payload any
	}{
		{"hello", HelloType, HelloPayload{Name: "n", Mode: 1, Capabilities: 0}},
		{"pair_chal", PairChallengeType, PairChallengePayload{Nonce: [32]byte{1, 2, 3}}},
		{"pair_resp", PairResponseType, PairResponsePayload{ResponderNonce: [32]byte{4}, HMAC: [32]byte{5}}},
		{"pair_ok", PairOKType, PairOKPayload{PeerName: "p", PubKey: [32]byte{6}}},
		{"file_offer", FileOfferType, FileOfferPayload{ID: 7, Name: "f.jpg", Size: 100, MIME: "image/jpeg"}},
		{"file_accept", FileAcceptType, FileAcceptPayload{ID: 7, ResumeFrom: 50}},
		{"chunk", ChunkType, ChunkPayload{ID: 7, Offset: 0, Data: []byte("hello")}},
		{"ack", AckType, AckPayload{ID: 7, Offset: 5}},
		{"file_done", FileDoneType, FileDonePayload{ID: 7, SHA256: [32]byte{9}}},
		{"text", TextType, TextPayload{Body: "hi"}},
		{"err", ErrorType, ErrorPayload{Code: 1, Message: "x"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			buf, err := Encode(c.t, c.payload)
			if err != nil {
				t.Fatal(err)
			}
			got, payload, err := Decode(buf)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.t {
				t.Errorf("type: got 0x%02x, want 0x%02x", got, c.t)
			}
			if len(payload) == 0 {
				t.Errorf("payload empty")
			}
		})
	}
}

func TestReadFrameShort(t *testing.T) {
	r := bytes.NewReader([]byte{0x05, 0x00, 0x00}) // announces 5, gives 3
	_, _, err := ReadFrame(r)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("got %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestReadFrameMultiple(t *testing.T) {
	var buf bytes.Buffer
	_ = WriteFrame(&buf, HelloType, HelloPayload{Name: "a"})
	_ = WriteFrame(&buf, TextType, TextPayload{Body: "b"})

	t1, p1, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if t1 != HelloType || string(p1) == "" {
		t.Errorf("first frame wrong: type=0x%02x", t1)
	}
	t2, p2, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if t2 != TextType || string(p2) == "" {
		t.Errorf("second frame wrong: type=0x%02x", t2)
	}
}
