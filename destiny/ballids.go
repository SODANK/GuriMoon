package destiny

import (
	"math"
	"sync/atomic"
)

// Synthetic ball IDs: balls with no entity row (NPCs, missiles, probes, rigid
// dungeon objects, billboards, asteroids) take IDs from fixed bands clear of the
// entity table. See docs/destiny-dll-spec.md#ball-id-space.
const (
	// MaxBallID is the largest ball ID the client can hold.
	MaxBallID uint32 = math.MaxInt32

	// SyntheticBallFloor is the lowest counter-minted ball ID. Entity item
	// IDs must stay below it; the server warns at boot as they approach.
	SyntheticBallFloor uint32 = 1_500_000_000

	// LegacyItemGapTop is the lowest entity item ID above the table's original 140M
	// range. Bands under it and above 2e8 are in space no item occupies.
	LegacyItemGapTop uint32 = 937_000_000

	ballBandSpan = 100_000_000

	NPCIDBase      uint32 = 1_500_000_000
	DunRigidIDBase uint32 = 1_600_000_000
	MissileIDBase  uint32 = 1_700_000_000
	ProbeIDBase    uint32 = 1_800_000_000
	ProbeIDCeiling uint32 = ProbeIDBase + ballBandSpan

	// Billboards are numbered per system from the base (a handful per
	// system, one per stargate), so a small band is plenty.
	BillboardIDBase    uint32 = 1_900_000_000
	BillboardIDCeiling uint32 = 1_901_000_000

	// AsteroidIDCeiling bounds the per-system asteroid band: 9,033 belts of
	// AsteroidIDsPerBelt, against a real maximum of ~42.
	AsteroidIDCeiling uint32 = LegacyItemGapTop
)

// BallBand is one synthetic ball-ID range, [Base, Ceiling).
type BallBand struct {
	Name          string
	Base, Ceiling uint32
}

// Contains reports whether id falls in the band.
func (b BallBand) Contains(id uint32) bool { return id >= b.Base && id < b.Ceiling }

// BallBands lists every synthetic band this package mints from.
func BallBands() []BallBand {
	return []BallBand{
		{"asteroid", AsteroidIDBase, AsteroidIDCeiling},
		{"npc", NPCIDBase, NPCIDBase + ballBandSpan},
		{"dungeon-rigid", DunRigidIDBase, DunRigidIDBase + ballBandSpan},
		{"missile", MissileIDBase, MissileIDBase + ballBandSpan},
		{"probe", ProbeIDBase, ProbeIDCeiling},
		{"billboard", BillboardIDBase, BillboardIDCeiling},
	}
}

// ballCounter mints process-global IDs from one band, wrapping at the top. A wrap
// reuses IDs a full band of allocations old; everything minted here lives seconds
// to a spawn lifetime and is long dead by then.
type ballCounter struct {
	n    atomic.Uint64
	base uint32
}

func (c *ballCounter) next() uint32 {
	return c.base + uint32((c.n.Add(1)-1)%ballBandSpan)
}

// NPC and missile entity IDs must be unique across the whole process, not just
// within one solar system: npc.NPCManager keys its registry by entityID alone,
// so per-system counters would collide as soon as two systems are live.
var (
	npcIDs      = ballCounter{base: NPCIDBase}
	missileIDs  = ballCounter{base: MissileIDBase}
	dunRigidIDs = ballCounter{base: DunRigidIDBase}
	probeIDs    = ballCounter{base: ProbeIDBase}
)

// NextDunRigidID mints a rigid dungeon ball ID (structures, gates, clouds: client-only
// balls with no system entity). Process-wide, since one connection's rigid balls
// are shown to other players on the same grid.
func NextDunRigidID() uint32 { return dunRigidIDs.next() }

// NextProbeID allocates a scanner-probe ball ID.
func NextProbeID() uint32 { return probeIDs.next() }

// IsNPCBallID reports whether id is in the NPC band.
func IsNPCBallID(id uint32) bool { return id >= NPCIDBase && id < NPCIDBase+ballBandSpan }

// IsDunRigidBallID reports whether id is a rigid dungeon ball.
func IsDunRigidBallID(id uint32) bool {
	return id >= DunRigidIDBase && id < DunRigidIDBase+ballBandSpan
}
