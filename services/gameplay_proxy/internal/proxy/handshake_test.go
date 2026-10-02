package proxy

import (
	"bytes"
	"testing"
)

// TestDecodeHandshake pins the framing rule: exactly one handshake, no more and no
// less. The length-mismatch cases cover both a truncated ticket and trailing bytes,
// because a later protocol sharing the magic must not be mistaken for a handshake.
func TestDecodeHandshake(t *testing.T) {
	good, ok := EncodeHandshake("a.b.c")
	if !ok {
		t.Fatal("EncodeHandshake refused a short ticket")
	}

	short := []byte("OTJ")
	wrongMagic := append([]byte("XXXX"), good[4:]...)
	lengthTooLong := append([]byte(nil), good...)
	lengthTooLong[5] = byte(len("a.b.c") + 1)
	trailing := append(append([]byte(nil), good...), 'x')

	cases := []struct {
		name     string
		datagram []byte
		want     string
		wantOK   bool
	}{
		{"good", good, "a.b.c", true},
		{"empty ticket", mustEncode(t, ""), "", true},
		{"short", short, "", false},
		{"wrong magic", wrongMagic, "", false},
		{"declared longer than present", lengthTooLong, "", false},
		{"trailing bytes", trailing, "", false},
		{"nil", nil, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := DecodeHandshake(tc.datagram)
			if ok != tc.wantOK {
				t.Fatalf("DecodeHandshake ok = %v, want %v", ok, tc.wantOK)
			}
			if got != tc.want {
				t.Errorf("DecodeHandshake ticket = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestEncodeDecodeRoundTrip guards the encoder against drift from the decoder.
func TestEncodeDecodeRoundTrip(t *testing.T) {
	const ticket = "eyJhbGciOiJFZERTQSJ9.payload.signature"
	raw, ok := EncodeHandshake(ticket)
	if !ok {
		t.Fatal("EncodeHandshake refused a normal ticket")
	}
	got, ok := DecodeHandshake(raw)
	if !ok || got != ticket {
		t.Fatalf("round trip = %q, %v; want %q, true", got, ok, ticket)
	}
}

// TestReplyBytes fixes the wire answer, which clients switch on by exact bytes.
func TestReplyBytes(t *testing.T) {
	if !bytes.Equal(replyOK, []byte("OTOK")) {
		t.Errorf("replyOK = %q, want OTOK", replyOK)
	}
	for reason, want := range map[byte]string{
		reasonInvalid:       "OTNO\x01",
		reasonExpired:       "OTNO\x02",
		reasonReused:        "OTNO\x03",
		reasonUnknownServer: "OTNO\x04",
	} {
		if got := string(replyRefused(reason)); got != want {
			t.Errorf("replyRefused(%d) = %q, want %q", reason, got, want)
		}
	}
}

func mustEncode(t *testing.T, ticket string) []byte {
	t.Helper()
	raw, ok := EncodeHandshake(ticket)
	if !ok {
		t.Fatalf("EncodeHandshake(%q) refused", ticket)
	}
	return raw
}
