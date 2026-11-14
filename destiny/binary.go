package destiny

import (
	"bytes"
	"encoding/binary"
	"math"
	"unicode/utf16"
)

// MiniBall describes a single miniball point for station/stargate entries.
type MiniBall struct {
	X, Y, Z, Radius float64
}

// BallDesc fully describes one entity ball for binary encoding.
// The Mode field controls which optional sectors are written.
type BallDesc struct {
	EntityID uint32
	Mode     BallMode

	// SubType bit 0x01 → ShipSector is present.
	SubType uint8

	// Position and basic geometry.
	Pos    Vec3
	Radius float64

	// MassSector (written when Mode != ModeRigid).
	Mass       float64
	CorpID     uint32
	AllianceID uint32
	// Harmonic is the ball's shield harmonic (destiny.dll getset table). Force fields
	// carry it; moonmining.py treats 0 and -1 as inactive. Zero here means unset and
	// goes on the wire as -1.
	Harmonic int64
	// Cloaked controls the cloak flag in MassSector. When true the client
	// renders the entity as invisible/untargetable (gate cloak, module cloak).
	Cloaked bool

	// ShipSector (written when SubType & 0x01 is set).
	MaxSpeed      float64
	Vel           Vec3
	Agility       float64
	SpeedFraction float64

	// MiniBalls are written for stations/stargates (SubType 66) after the
	// formationID and before the name.
	MiniBalls []MiniBall

	// Entity display name (encoded as UTF-16LE with a uint8 length prefix).
	Name string

	// Mode-specific fields.
	TargetPos       Vec3    // GOTO, WARP
	FollowID        uint32  // follow, warp, missile, formation modes
	FollowRange     float64 // follow, warp, missile, formation modes
	OrbitID         uint32  // ORBIT
	OrbitDist       float64 // ORBIT
	WarpEffectStamp uint32  // warp mode: animation sync stamp
	WarpOwnerID     uint32  // warp mode: owner entity (charID)
}

// EncodeSetState builds the binary destiny blob for SetState (CreateBallpark).
// The C++ CreateBallpark reads one header then all balls sequentially to EOF.
func EncodeSetState(seq uint32, balls []BallDesc) []byte {
	var buf bytes.Buffer
	le := binary.LittleEndian

	// Single AddBall_header: more(uint8=0) + sequence(uint32) = 5 bytes.
	buf.WriteByte(0)
	writeU32(&buf, le, seq)

	for i := range balls {
		writeBall(&buf, le, &balls[i])
	}
	return buf.Bytes()
}

// EncodeAddBalls builds the binary destiny blob for AddBalls (ProcessState).
// Same layout as EncodeSetState: single header then balls to EOF.
func EncodeAddBalls(seq uint32, balls []BallDesc) []byte {
	var buf bytes.Buffer
	le := binary.LittleEndian

	// AddBall_header: packet_type(uint8=0) + sequence(uint32) = 5 bytes.
	buf.WriteByte(0)
	writeU32(&buf, le, seq)

	for i := range balls {
		writeBall(&buf, le, &balls[i])
	}
	return buf.Bytes()
}

func writeBall(buf *bytes.Buffer, le binary.ByteOrder, b *BallDesc) {
	// BallHeader: entityID(u32) | mode(u8) | radius(f64) | x(f64) | y(f64) | z(f64) | sub_type(u8)
	writeU32(buf, le, b.EntityID)
	buf.WriteByte(uint8(b.Mode))
	writeF64(buf, le, b.Radius)
	writeF64(buf, le, b.Pos.X)
	writeF64(buf, le, b.Pos.Y)
	writeF64(buf, le, b.Pos.Z)
	buf.WriteByte(b.SubType)

	// MassSector (all modes except RIGID)
	if b.Mode != ModeRigid {
		writeF64(buf, le, b.Mass)
		if b.Cloaked {
			buf.WriteByte(1)
		} else {
			buf.WriteByte(0)
		}
		// Shield harmonic (int64).  -1 = unset/inactive, which is what every
		// non-forcefield ball sends.
		harmonic := b.Harmonic
		if harmonic == 0 {
			harmonic = -1
		}
		var u64 [8]byte
		le.PutUint64(u64[:], uint64(harmonic))
		buf.Write(u64[:])
		writeU32(buf, le, b.CorpID)
		// allianceID is all-ones (-1) for every ball but a force field admitting its
		// alliance: moonmining.py ShowForce reads allianceID != -1 as that checkbox.
		if b.AllianceID != 0 {
			writeU32(buf, le, b.AllianceID)
		} else {
			writeU32(buf, le, 0xFFFFFFFF)
		}
	}

	// ShipSector (when SubType & 0x01)
	if b.SubType&0x01 != 0 {
		writeF64(buf, le, b.MaxSpeed)
		writeF64(buf, le, b.Vel.X)
		writeF64(buf, le, b.Vel.Y)
		writeF64(buf, le, b.Vel.Z)
		writeF64(buf, le, b.Agility)
		writeF64(buf, le, b.SpeedFraction)
	}

	// formationID, always 0xFF
	buf.WriteByte(0xFF)

	// Mode-specific tail
	switch b.Mode {
	case ModeGoto:
		writeF64(buf, le, b.TargetPos.X)
		writeF64(buf, le, b.TargetPos.Y)
		writeF64(buf, le, b.TargetPos.Z)
	case ModeFollow:
		writeU32(buf, le, b.FollowID)
		writeF64(buf, le, b.FollowRange)
	case ModeOrbit:
		writeU32(buf, le, b.OrbitID)
		writeF64(buf, le, b.OrbitDist)
	case ModeWarp:
		writeF64(buf, le, b.TargetPos.X)
		writeF64(buf, le, b.TargetPos.Y)
		writeF64(buf, le, b.TargetPos.Z)
		writeU32(buf, le, b.WarpEffectStamp)
		writeF64(buf, le, b.FollowRange)
		writeU32(buf, le, b.FollowID)
		writeU32(buf, le, b.WarpOwnerID)
	case ModeMissile:
		writeU32(buf, le, b.FollowID)
		writeF64(buf, le, b.FollowRange)
		// Other modes (stop, field, rigid, formation, mushroom, troll, miniball, boid):
		// nothing after formationID.
	}

	// SubType 66 (stations, stargates, starbases): the client always reads a uint16
	// miniball count, so it must be written even when zero. See docs/destiny-dll-spec.md#ball-subtypes-and-collision.
	if b.SubType == 66 {
		writeU16(buf, le, uint16(len(b.MiniBalls)))
		for _, mb := range b.MiniBalls {
			writeF64(buf, le, mb.X)
			writeF64(buf, le, mb.Y)
			writeF64(buf, le, mb.Z)
			writeF64(buf, le, mb.Radius)
		}
	}

	// Name: uint8 length (in 16-bit units) followed by UTF-16LE code units.
	nameU16 := utf16.Encode([]rune(b.Name))
	buf.WriteByte(uint8(len(nameU16)))
	for _, ch := range nameU16 {
		buf.WriteByte(byte(ch))
		buf.WriteByte(byte(ch >> 8))
	}
}

// ---- helpers ----------------------------------------------------------------

func writeU32(buf *bytes.Buffer, le binary.ByteOrder, v uint32) {
	var b [4]byte
	le.PutUint32(b[:], v)
	buf.Write(b[:])
}

func writeU16(buf *bytes.Buffer, le binary.ByteOrder, v uint16) {
	var b [2]byte
	le.PutUint16(b[:], v)
	buf.Write(b[:])
}

func writeF64(buf *bytes.Buffer, le binary.ByteOrder, v float64) {
	var b [8]byte
	le.PutUint64(b[:], math.Float64bits(v))
	buf.Write(b[:])
}
