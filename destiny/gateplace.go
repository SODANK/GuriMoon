package destiny

// Placement of an admin-built stargate (docs/stargates.md#placement). The gate sits beside an
// anchor planet or moon, on the side facing the system it leads to. That direction comes from the
// two systems' galactic positions, which share their axes with in-system coordinates.

const (
	// GateDefaultOffsetM is how far beyond the anchor's surface a new gate is placed when the
	// form leaves the distance blank: 0.1 AU.
	GateDefaultOffsetM = 0.1 * 149_597_870_700.0

	// GateMinClearanceM is the least surface-to-surface gap allowed between a new gate and
	// any other celestial. Four grid radii, so the gate never shares a grid with one.
	GateMinClearanceM = 4 * BubbleRadius
)

// PlaceGate returns the position of a gate beside the anchor at anchorPos, offsetM beyond its
// surface, on the bearing from srcSys to dstSys (galactic positions). Equal system positions
// fall back to +X.
func PlaceGate(anchorPos Vec3, anchorRadius float64, srcSys, dstSys Vec3, offsetM float64) Vec3 {
	dir := dstSys.Sub(srcSys)
	if dir.IsZero() {
		dir = Vec3{X: 1}
	}
	return anchorPos.Add(dir.Normalize().Scale(anchorRadius + offsetM))
}

// GateObstacle is a celestial a new gate must keep clear of.
type GateObstacle struct {
	ID     int64
	Name   string
	Pos    Vec3
	Radius float64
}

// GateClearance returns the obstacle nearest to a gate of gateRadius at pos whose surface gap is
// under minGapM, and whether there is one. The anchor itself counts, so an offset below
// minGapM is refused too.
func GateClearance(pos Vec3, gateRadius float64, obstacles []GateObstacle, minGapM float64) (GateObstacle, float64, bool) {
	var worst GateObstacle
	worstGap := 0.0
	found := false
	for _, o := range obstacles {
		gap := pos.Sub(o.Pos).Len() - o.Radius - gateRadius
		if gap >= minGapM {
			continue
		}
		if !found || gap < worstGap {
			worst, worstGap, found = o, gap, true
		}
	}
	return worst, worstGap, found
}
