package frame

import "testing"

func TestTypeConstants(t *testing.T) {
	cases := []struct {
		got  Type
		want uint8
		name string
	}{
		{HelloType, 0x01, "HELLO"},
		{PairChallengeType, 0x02, "PAIR_CHALLENGE"},
		{PairResponseType, 0x03, "PAIR_RESPONSE"},
		{PairOKType, 0x04, "PAIR_OK"},
		{FileOfferType, 0x10, "FILE_OFFER"},
		{FileAcceptType, 0x11, "FILE_ACCEPT"},
		{ChunkType, 0x12, "CHUNK"},
		{AckType, 0x13, "ACK"},
		{FileDoneType, 0x14, "FILE_DONE"},
		{TextType, 0x80, "TEXT"},
		{ErrorType, 0xF0, "ERROR"},
	}
	for _, c := range cases {
		if uint8(c.got) != c.want {
			t.Errorf("%s: got 0x%02x, want 0x%02x", c.name, c.got, c.want)
		}
	}
}
