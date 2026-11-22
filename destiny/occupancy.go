package destiny

import "time"

// Occupancy: what a point in space is already spoken for by, for writers that place a
// ship somewhere other than through an arrival lane walk (a rolebot birth, a landing
// checked again just before it happens). The client overlap-resolves free balls and
// throws two overlapping hulls apart, so every writer of a ship's coordinate should
// ask first. See docs/destiny-arrival-lanes.md#why-a-lane.

// OccupancyFootprint is where this free ball physically counts for overlap, which is not
// always where its manager stands:
//   - in warp (ModeWarp): its warp target. A ball mid-tunnel or in its exit buffer is
//     published there (BallDescForObserver) and will be standing there shortly.
//   - aligning to warp (warpPending): where it is now; it has not left.
//   - otherwise: Footprint().
//
// Rigid balls report none, as with Footprint. Thread-safe.
func (e *Entity) OccupancyFootprint() (BallFootprint, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.Manager == nil || e.Manager.maxShipVelocity <= 0 {
		return BallFootprint{}, false
	}
	if e.Manager.CurrentBallMode() == ModeWarp {
		return BallFootprint{Pos: e.Manager.targetPos, Radius: e.Manager.radius}, true
	}
	return BallFootprint{Pos: e.Manager.Pos, Radius: e.Manager.radius}, true
}

// OccupancySnapshot returns, as one footprint list, the free balls within rng of center
// in ss (none for a nil ss) and the live arrival reservations in sysID's lane within rng
// that exclude does not own. exclude (0 for none) is the asking ball itself. It never
// creates a lane, and it takes the system's locks and the lane lock one after the other,
// never nested.
func OccupancySnapshot(ss *SolarSystem, sysID int64, center Vec3, rng float64, exclude uint32) []BallFootprint {
	var out []BallFootprint
	if ss != nil {
		out = ss.FreeBallFootprints(center, rng, exclude)
	}
	rngSq := rng * rng
	withLiveLane(sysID, false, func(lane *arrivalLane, _ time.Time) {
		for _, r := range lane.res {
			if exclude != 0 && r.owner == exclude {
				continue
			}
			if r.at.Pos.Sub(center).LenSq() > rngSq {
				continue
			}
			out = append(out, r.at)
		}
	})
	return out
}

// PointClear reports whether a hull of the given radius standing at p overlaps none of
// occ, by the same rule a lane seat is judged by (radii plus ArrivalClearMargin).
func PointClear(p Vec3, radius float64, occ []BallFootprint) bool {
	for _, o := range occ {
		if footprintsOverlap(p, radius, o) {
			return false
		}
	}
	return true
}

// RecheckArrivalPoint looks at a landing claimed earlier (ClaimArrivalPointEx) again,
// just before the ship arrives there. If point is still clear of the free balls in ss
// and of other owners' reservations, owner's reservation is refreshed and point comes
// back with moved=false. Otherwise owner's old reservation is dropped and a fresh seat
// is walked from base along dir, as the original claim did (moved=true).
func RecheckArrivalPoint(ss *SolarSystem, sysID int64, point, base, dir Vec3, radius float64,
	owner uint32) (Vec3, ArrivalClaim, bool) {

	if dir.IsZero() {
		dir = Vec3{X: 1}
	}
	var occupied []BallFootprint
	if ss != nil {
		occupied = ss.FreeBallFootprints(base, arrivalScanRadius, owner)
	}

	var out Vec3
	var claim ArrivalClaim
	moved := false
	withLiveLane(sysID, true, func(lane *arrivalLane, now time.Time) {
		others := lane.res[:0]
		for _, r := range lane.res {
			if owner == 0 || r.owner != owner {
				others = append(others, r)
			}
		}
		lane.res = others
		if arrivalPointClear(point, radius, lane.res, occupied) {
			lane.res = append(lane.res, arrivalReservation{
				at:    BallFootprint{Pos: point, Radius: radius},
				until: now.Add(arrivalLaneHold),
				owner: owner,
			})
			out = point
			claim = ArrivalClaim{Reservations: len(lane.res) - 1, Occupied: len(occupied)}
			return
		}
		out, claim = claimInLaneLocked(lane, now, base, dir, radius, owner, occupied)
		moved = true
	})
	return out, claim, moved
}
