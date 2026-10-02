// The UDP handshake datagram and the one-datagram answer.
//
// A client sends "OTJ1", the ticket length as a big-endian uint16, then the ticket
// bytes. The proxy answers with "OTOK" or "OTNO" plus one reason byte (doc 14 §8). The
// framing is narrow on purpose: a client binds one local port, sends the handshake from
// it, then rebinds the same port with ENet, so the ticket has to fit in one small
// datagram before the game protocol starts.

package proxy

import "encoding/binary"

const (
	// handshakeMagic is the four ASCII bytes that open every handshake. A datagram
	// that does not start with them is not a handshake and never gets an answer.
	handshakeMagic = "OTJ1"

	// handshakeHeaderLen is the magic plus the two length bytes.
	handshakeHeaderLen = 4 + 2

	// maxTicketLen is the largest ticket a uint16 length field can describe.
	maxTicketLen = 1<<16 - 1
)

// Refusal reasons carried by the byte after "OTNO", fixed by doc 14 §8.
const (
	reasonInvalid       byte = 1 // malformed ticket, bad signature, wrong iss/aud/alg
	reasonExpired       byte = 2 // correctly signed but past exp plus leeway
	reasonReused        byte = 3 // the jti was already accepted from another address
	reasonUnknownServer byte = 4 // srv is not in the server directory
)

// replyOK is the single accepted answer. It is a package variable rather than a fresh
// slice per handshake because it is never modified and is copied by WriteToUDP.
var replyOK = []byte("OTOK")

// replyRefused builds the small "OTNO"+reason datagram. A new slice each time is
// cheaper than the synchronisation a shared buffer would need, since a refusal is not
// on the hot forwarding path.
func replyRefused(reason byte) []byte {
	return []byte{'O', 'T', 'N', 'O', reason}
}

// DecodeHandshake splits a datagram into the ticket it carries. It reports false for
// anything that is not exactly one handshake: too short for the header, the wrong
// magic, or a length field that does not match the bytes actually present. Both a
// truncated datagram and one with trailing bytes are rejected, so a later protocol
// cannot be mistaken for a handshake that happens to share the magic.
func DecodeHandshake(datagram []byte) (string, bool) {
	if len(datagram) < handshakeHeaderLen {
		return "", false
	}
	if string(datagram[:4]) != handshakeMagic {
		return "", false
	}
	n := int(binary.BigEndian.Uint16(datagram[4:6]))
	if len(datagram) != handshakeHeaderLen+n {
		return "", false
	}
	return string(datagram[handshakeHeaderLen:]), true
}

// EncodeHandshake builds the datagram for ticket. It exists so the proxy's tests and a
// small fake client share one encoder with DecodeHandshake instead of each spelling out
// the framing. It reports false when the ticket is too long for the uint16 length
// field, which is a client bug rather than something to truncate.
func EncodeHandshake(ticket string) ([]byte, bool) {
	if len(ticket) > maxTicketLen {
		return nil, false
	}
	out := make([]byte, handshakeHeaderLen+len(ticket))
	copy(out, handshakeMagic)
	binary.BigEndian.PutUint16(out[4:6], uint16(len(ticket)))
	copy(out[handshakeHeaderLen:], ticket)
	return out, true
}
