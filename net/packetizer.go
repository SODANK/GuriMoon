// Package net implements the EVEmu TCP network layer: stream framing,
// connection management, and the presentation handshake.
package net

import (
	"encoding/binary"
	"errors"
)

// MaxPacketSize is the maximum allowed payload size.
// The Blood-era client handles packets well beyond 1 MB; 10 MB covers large
// responses like region-wide assembly line listings.
const MaxPacketSize = 10_000_000

// ErrPacketTooLarge is returned when a framed length field exceeds MaxPacketSize.
var ErrPacketTooLarge = errors.New("gurimoon/net: packet exceeds maximum allowed size")

// StreamPacketizer reassembles length-prefixed packets (4-byte little-endian uint32 length, then
// that many payload bytes) from a raw byte stream. Packets may be split across Append calls.
type StreamPacketizer struct {
	buf     []byte // accumulated raw bytes not yet consumed
	packets [][]byte
}

// Append feeds raw bytes from the TCP stream into the packetizer.
// Complete packets are buffered internally; retrieve them with Pop.
// Returns ErrPacketTooLarge if a length header exceeds MaxPacketSize.
func (p *StreamPacketizer) Append(data []byte) error {
	p.buf = append(p.buf, data...)
	for {
		if len(p.buf) < 4 {
			break
		}
		payloadLen := binary.LittleEndian.Uint32(p.buf[:4])
		if payloadLen > MaxPacketSize {
			return ErrPacketTooLarge
		}
		total := 4 + int(payloadLen)
		if len(p.buf) < total {
			break
		}
		pkt := make([]byte, int(payloadLen))
		copy(pkt, p.buf[4:total])
		p.packets = append(p.packets, pkt)
		p.buf = p.buf[total:]
	}
	return nil
}

// Pop returns the next complete packet payload, or nil if none are ready.
func (p *StreamPacketizer) Pop() []byte {
	if len(p.packets) == 0 {
		return nil
	}
	pkt := p.packets[0]
	p.packets = p.packets[1:]
	return pkt
}

// Reset discards all buffered data and queued packets.
func (p *StreamPacketizer) Reset() {
	p.buf = p.buf[:0]
	p.packets = p.packets[:0]
}

// Frame encodes a payload into a length-prefixed wire frame.
func Frame(payload []byte) []byte {
	out := make([]byte, 4+len(payload))
	binary.LittleEndian.PutUint32(out[:4], uint32(len(payload)))
	copy(out[4:], payload)
	return out
}
