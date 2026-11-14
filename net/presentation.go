package net

import (
	"errors"
	"fmt"
	"io"

	"gurimoon/marshal"
)

// VersionExchange is the first packet sent/received on a connection.
// It corresponds to the C++ VersionExchange inline tuple.
type VersionExchange struct {
	Birthday       uint64
	MachoVersion   uint64
	UserCount      uint64
	VersionNumber  float64
	BuildVersion   uint64
	ProjectVersion string
}

// DefaultBirthday is the EVE client birthday constant (0x29A08 = 170472).
const DefaultBirthday = 170472

// Decode parses a VersionExchange from a 6-element PyTuple.
func (v *VersionExchange) Decode(rep marshal.PyRep) error {
	t, ok := rep.(marshal.PyTuple)
	if !ok {
		return fmt.Errorf("version exchange: expected PyTuple, got %T", rep)
	}
	if len(t.Items) != 6 {
		return fmt.Errorf("version exchange: expected 6 items, got %d", len(t.Items))
	}
	if err := decodeUint64(t.Items[0], &v.Birthday); err != nil {
		return fmt.Errorf("version exchange birthday: %w", err)
	}
	if err := decodeUint64(t.Items[1], &v.MachoVersion); err != nil {
		return fmt.Errorf("version exchange macho_version: %w", err)
	}
	if err := decodeUint64(t.Items[2], &v.UserCount); err != nil {
		return fmt.Errorf("version exchange user_count: %w", err)
	}
	if r, ok := t.Items[3].(marshal.PyReal); ok {
		v.VersionNumber = r.Value
	} else {
		return fmt.Errorf("version exchange version_number: expected PyReal, got %T", t.Items[3])
	}
	if err := decodeUint64(t.Items[4], &v.BuildVersion); err != nil {
		return fmt.Errorf("version exchange build_version: %w", err)
	}
	if s, ok := t.Items[5].(marshal.PyString); ok {
		v.ProjectVersion = s.Value
	} else {
		return fmt.Errorf("version exchange project_version: expected PyString, got %T", t.Items[5])
	}
	return nil
}

// Encode serialises a VersionExchange as a 6-element PyTuple.
func (v *VersionExchange) Encode() marshal.PyRep {
	return marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: v.Birthday},
		marshal.PyInt{Value: v.MachoVersion},
		marshal.PyInt{Value: v.UserCount},
		marshal.PyReal{Value: v.VersionNumber},
		marshal.PyInt{Value: v.BuildVersion},
		marshal.PyString{Value: v.ProjectVersion},
	}}
}

// connState tracks which part of the EVE handshake a connection is in.
type connState int

const (
	stateWaitVersion connState = iota // waiting for client VersionExchange
	stateActive                       // version received; normal PyPacket traffic
)

// Presentation wraps a Conn and handles the EVE presentation-layer handshake (non-DRAGON
// mode). The first inbound packet is a VersionExchange tuple and every later one is a
// PyPacket. Sends are marshalled and length-framed over the Conn.
type Presentation struct {
	conn  *Conn
	state connState

	// Local version advertised to clients (set by SendHandshake).
	ourVersion VersionExchange
	// Version received from the remote peer.
	PeerVersion *VersionExchange
}

// NewPresentation wraps an existing Conn.
func NewPresentation(c *Conn) *Presentation {
	return &Presentation{
		conn:  c,
		state: stateWaitVersion,
		ourVersion: VersionExchange{
			Birthday: DefaultBirthday,
		},
	}
}

// SendHandshake marshals our VersionExchange and sends it to the peer.
// This must be called by the server before reading any packets.
func (p *Presentation) SendHandshake(ve VersionExchange) error {
	p.ourVersion = ve
	return p.sendRep(ve.Encode())
}

// ReadPacket reads the next PyPacket. The first call consumes the peer's VersionExchange
// (stored in PeerVersion) and returns a nil packet, so the caller reads again.
func (p *Presentation) ReadPacket() (*PyPacket, error) {
	raw, err := p.conn.ReadPacket()
	if err != nil {
		return nil, err
	}
	rep, err := marshal.Unmarshal(raw)
	if err != nil {
		return nil, fmt.Errorf("presentation: unmarshal: %w", err)
	}

	switch p.state {
	case stateWaitVersion:
		ve := &VersionExchange{}
		if err := ve.Decode(rep); err != nil {
			return nil, fmt.Errorf("presentation: %w", err)
		}
		p.PeerVersion = ve
		p.state = stateActive
		// The initial version exchange yields no packet; the caller reads again.
		return nil, nil

	case stateActive:
		pkt := &PyPacket{}
		if err := pkt.Decode(rep); err != nil {
			return nil, fmt.Errorf("presentation: %w", err)
		}
		return pkt, nil

	default:
		return nil, errors.New("presentation: unknown connection state")
	}
}

// SendPacket encodes a PyPacket and writes it to the connection.
func (p *Presentation) SendPacket(pkt *PyPacket) error {
	if pkt == nil {
		return nil
	}
	return p.sendRep(pkt.Encode())
}

// SendRaw marshals and sends an arbitrary PyRep that is not a full PyPacket (for
// example a handshake).
func (p *Presentation) SendRaw(rep marshal.PyRep) error {
	return p.sendRep(rep)
}

func (p *Presentation) sendRep(rep marshal.PyRep) error {
	payload, err := marshal.Marshal(rep)
	if err != nil {
		return fmt.Errorf("presentation: marshal: %w", err)
	}
	if len(payload) > MaxPacketSize {
		return ErrPacketTooLarge
	}
	return p.conn.SendPacket(payload)
}

// Close closes the underlying connection.
func (p *Presentation) Close() error {
	return p.conn.Close()
}

// RemoteAddr returns the remote peer address string.
func (p *Presentation) RemoteAddr() string {
	if p.conn == nil {
		return "<nil>"
	}
	addr := p.conn.RemoteAddr()
	if addr == nil {
		return "<nil>"
	}
	return addr.String()
}

// ErrEOF is a convenience alias so callers can compare without importing "io".
var ErrEOF = io.EOF
