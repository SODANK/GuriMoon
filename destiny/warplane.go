package destiny

import (
	"sync"
	"time"

	"gurimoon/syncx"
)

// Arrival lanes: one point per ship arriving at a warp landing or gate jump-in,
// checked against in-flight reservations and the free balls standing there now.
// See docs/destiny-arrival-lanes.md#why-a-lane.
const (
	// arrivalLaneHold is how long a claimed point stays claimed. A claim is made at
	// warp engage and must outlast align + tunnel + exit buffer; once the ship is there
	// the occupancy scan sees it. See docs/destiny-arrival-lanes.md#reservations-and-occupancy.
	arrivalLaneHold = 90 * time.Second

	// ArrivalLaneMaxSlots caps how far down the lane a claim walks before
	// giving up on finding a clear one.
	ArrivalLaneMaxSlots = 24

	// ArrivalClearMargin is the gap between two hulls beyond the sum of their radii.
	// Kept small: a large margin rejects the lane's own 600 m lateral columns for
	// hulls bigger than a cruiser.
	ArrivalClearMargin = 100.0

	// arrivalScanRadius bounds the live occupancy scan around the base point.
	// The lane never runs further than a few km even after the overflow, so
	// nothing past this can collide with any slot.
	arrivalScanRadius = 50_000.0
)

// BallFootprint is one free ball's collision geometry. Rigid balls (structures,
// celestials, asteroids) are not footprints: the client interpenetrates those and
// overlap-resolves only free balls against each other.
type BallFootprint struct {
	Pos    Vec3
	Radius float64
}

type arrivalReservation struct {
	at    BallFootprint
	until time.Time
	// owner is the claiming ball's ID (0 for an anonymous claim). A ball holds at
	// most one landing: claiming again replaces its own reservation instead of
	// leaving a stale seat behind for the rest of arrivalLaneHold.
	owner uint32
}

type arrivalLane struct {
	mu sync.Mutex
	// dead marks a lane the sweep has retired from arrivalLanes. A claimer that loaded
	// the pointer before the delete sees the flag under the lock and retries, so no
	// reservation is written into a lane nobody else can load.
	dead bool
	res  []arrivalReservation
}

// arrivalLanes is keyed by solarSystemID and swept by SweepArrivalLanes so a long
// uptime does not keep one entry per system ever visited.
var arrivalLanes syncx.Map[int64, *arrivalLane]

// WarpLaneOffset turns a slot number into an offset from the computed warp
// landing point, for a ship arriving along the outward radial dir (landing
// minus celestial). Slot 0 is the landing itself.
func WarpLaneOffset(dir Vec3, slot int) Vec3 {
	return UndockLaneOffset(dir, slot)
}

// ClaimArrivalPoint returns a point in the arrival lane at base, running outward
// along dir, clear of arrivals still in flight and of the free balls standing in
// ss now (nil ss, as on a gate jump before the far system is loaded, consults
// reservations only). radius is the arriving hull's; exclude is its own ball ID.
func ClaimArrivalPoint(ss *SolarSystem, sysID int64, base, dir Vec3, radius float64, exclude uint32) Vec3 {
	p, _ := ClaimArrivalPointEx(ss, sysID, base, dir, radius, exclude)
	return p
}

// ArrivalClaim is what ClaimArrivalPointEx settled on, for the caller's log:
// how far down the queue the arrival went (Slot, Overflow) and why candidates
// were skipped (Rejected against Reservations and Occupied).
type ArrivalClaim struct {
	// Slot handed out. 0 is the unmodified landing.
	Slot int
	// Overflow is set when every tracked seat was taken and Slot is a point
	// past the band rather than a reservation-backed one.
	Overflow bool
	// Rejected is how many candidate slots were refused before this one.
	Rejected int
	// Reservations still live in this lane, and free balls found standing
	// within arrivalScanRadius of base, at the moment of the claim.
	Reservations int
	Occupied     int
}

// Moved reports whether the lane displaced the arrival; a claim that did not
// is not worth a log line.
func (c ArrivalClaim) Moved() bool { return c.Slot != 0 }

// ClaimArrivalPointEx is ClaimArrivalPoint with the claim reported back.
func ClaimArrivalPointEx(ss *SolarSystem, sysID int64, base, dir Vec3, radius float64, exclude uint32) (Vec3, ArrivalClaim) {
	if dir.IsZero() {
		dir = Vec3{X: 1}
	}
	// Snapshot live occupancy outside the lane lock: SolarSystem and Entity take
	// their own locks, and a claim must never hold two.
	var occupied []BallFootprint
	if ss != nil {
		occupied = ss.FreeBallFootprints(base, arrivalScanRadius, exclude)
	}

	var point Vec3
	var claim ArrivalClaim
	withLiveLane(sysID, true, func(lane *arrivalLane, now time.Time) {
		// The claimer's own earlier landing is replaced, not counted against it.
		if exclude != 0 {
			kept := lane.res[:0]
			for _, r := range lane.res {
				if r.owner != exclude {
					kept = append(kept, r)
				}
			}
			lane.res = kept
		}
		point, claim = claimInLaneLocked(lane, now, base, dir, radius, exclude, occupied)
	})
	return point, claim
}

// withLiveLane runs fn under sysID's lane lock with expired reservations
// already pruned. create=false skips a system with no lane (fn is not called).
// A lane the sweep retired between the load and the lock is retried.
func withLiveLane(sysID int64, create bool, fn func(lane *arrivalLane, now time.Time)) {
	for {
		var lane *arrivalLane
		if create {
			lane, _ = arrivalLanes.LoadOrStore(sysID, &arrivalLane{})
		} else {
			v, ok := arrivalLanes.Load(sysID)
			if !ok {
				return
			}
			lane = v
		}

		now := time.Now()
		lane.mu.Lock()
		if lane.dead {
			lane.mu.Unlock()
			if !create {
				return
			}
			continue // swept between the load and the lock; take a fresh lane
		}
		kept := lane.res[:0]
		for _, r := range lane.res {
			if now.Before(r.until) {
				kept = append(kept, r)
			}
		}
		lane.res = kept
		fn(lane, now)
		lane.mu.Unlock()
		return
	}
}

// claimInLaneLocked walks the lane for the first seat clear of reservations and
// occupied, records the reservation for owner and returns it. Caller holds lane.mu.
func claimInLaneLocked(lane *arrivalLane, now time.Time, base, dir Vec3, radius float64,
	owner uint32, occupied []BallFootprint) (Vec3, ArrivalClaim) {
	claim := ArrivalClaim{Reservations: len(lane.res), Occupied: len(occupied)}
	slot := -1
	var point Vec3
	for s := 0; s < ArrivalLaneMaxSlots; s++ {
		p := base.Add(UndockLaneOffset(dir, s))
		if arrivalPointClear(p, radius, lane.res, occupied) {
			slot, point = s, p
			break
		}
		claim.Rejected++
	}
	if slot < 0 {
		// Every tracked slot is taken: hand out a point past the band, never a wrapped
		// seat on a live ball. Spread so two overflows do not pick the same step.
		slot = ArrivalLaneMaxSlots + int(now.UnixNano()%ArrivalLaneMaxSlots)
		point = base.Add(UndockLaneOffset(dir, slot))
		claim.Overflow = true
	}
	claim.Slot = slot
	lane.res = append(lane.res, arrivalReservation{
		at:    BallFootprint{Pos: point, Radius: radius},
		until: now.Add(arrivalLaneHold),
		owner: owner,
	})
	return point, claim
}

// arrivalPointClear reports whether a hull of the given radius placed at p
// would overlap anything already spoken for or already there.
func arrivalPointClear(p Vec3, radius float64, res []arrivalReservation, occupied []BallFootprint) bool {
	for _, r := range res {
		if footprintsOverlap(p, radius, r.at) {
			return false
		}
	}
	for _, o := range occupied {
		if footprintsOverlap(p, radius, o) {
			return false
		}
	}
	return true
}

func footprintsOverlap(p Vec3, radius float64, other BallFootprint) bool {
	sep := radius + other.Radius + ArrivalClearMargin
	return p.Sub(other.Pos).LenSq() < sep*sep
}

// SweepArrivalLanes drops expired reservations and retires lanes nobody is
// standing in.
func SweepArrivalLanes() {
	now := time.Now()
	arrivalLanes.Range(func(k int64, v *arrivalLane) bool {
		lane := v
		lane.mu.Lock()
		kept := lane.res[:0]
		for _, r := range lane.res {
			if now.Before(r.until) {
				kept = append(kept, r)
			}
		}
		lane.res = kept
		if len(lane.res) == 0 {
			lane.dead = true
			arrivalLanes.Delete(k)
		}
		lane.mu.Unlock()
		return true
	})
}
