package destiny

import "math"

// Undock lane geometry: where successive ships leaving one station door are
// placed. Slots step forward along the undock line, three abreast, so no two
// ships share a coordinate. See docs/undock-geometry.md#lane-not-disc.
const (
	// Distance between successive rows along the undock line.
	UndockLaneStep = 750.0

	// Lateral columns and their spacing. Three keeps the lane readable as a lane.
	UndockLaneColumns = 3
	UndockLaneWidth   = 600.0

	// Slots per corridor before the lane wraps into a laterally offset layer.
	UndockLaneSlotsPerLayer = 36

	// Laterally offset layers before the pattern repeats; 9 x 36 = UndockLaneMaxSlots.
	// Layers sit half a column width apart on the second perpendicular axis.
	UndockLaneLayerSpan = 9

	// Overflow slots: axial rows past the band with no layer offset, so an overflow
	// point never coincides with an in-band seat or the bay. They are not
	// reservations and two of them can coincide. See docs/undock-geometry.md#overflow.
	UndockLaneOverflowSlots = 72
)

// UndockLaneMaxAxial and UndockLaneMaxLateral bound the box every slot lands
// in. Widening the lattice must move these too; a test asserts the geometry
// honours them.
const (
	// Band rows run 0..UndockLaneRowsPerLayer-1; overflow rows continue past them.
	UndockLaneRowsPerLayer = UndockLaneSlotsPerLayer / UndockLaneColumns
	UndockLaneOverflowRows = UndockLaneOverflowSlots / UndockLaneColumns

	UndockLaneMaxAxial   = float64(UndockLaneRowsPerLayer+UndockLaneOverflowRows-1) * UndockLaneStep
	UndockLaneMaxLateral = float64(UndockLaneColumns/2)*UndockLaneWidth +
		float64(UndockLaneLayerSpan/2)*(UndockLaneWidth/2)
)

// UndockClearMargin is how far past the ship's own hull the exit point sits
// beyond the authored dock entry.
const UndockClearMargin = 2_000.0

// UndockExitOffset returns the offset from the station centre a ship undocks
// at: the authored dock entry nudged forward along dir by the ship's radius
// plus UndockClearMargin. Do not walk out to the station's mapDenormalize
// radius; see docs/undock-geometry.md#exit-point. A zero dir returns dockEntry.
func UndockExitOffset(dockEntry, dir Vec3, shipRadius float64) Vec3 {
	n := dir.Normalize()
	if n.IsZero() {
		return dockEntry
	}
	return dockEntry.Add(n.Scale(shipRadius + UndockClearMargin))
}

// UndockLaneOffset maps a slot number to an offset from the docking bay for a
// ship undocking along dir (any length; zero gives zero). Slot 0 is the bay;
// slots fill the columns, then step a row forward, wrap into offset layers past
// UndockLaneSlotsPerLayer, and use the overflow corridor past UndockLaneMaxSlots.
func UndockLaneOffset(dir Vec3, slot int) Vec3 {
	if slot <= 0 || dir.IsZero() {
		return Vec3{}
	}
	n := dir.Normalize()
	u, v := PerpendicularBasis(n)

	var row int
	var lateralU, lateralV float64

	if slot >= UndockLaneMaxSlots {
		// Overflow corridor: rows past the band, no layer offset.
		seat := (slot - UndockLaneMaxSlots) % UndockLaneOverflowSlots
		row = UndockLaneRowsPerLayer + seat/UndockLaneColumns
		lateralU = centredStep(seat%UndockLaneColumns, UndockLaneWidth)
	} else {
		// Which corridor this slot belongs to, and where in it.
		layer := slot / UndockLaneSlotsPerLayer
		seat := slot % UndockLaneSlotsPerLayer

		row = seat / UndockLaneColumns

		// Columns are centred on the lane (0, +w, -w, ...). Columns move along u and
		// layers along v: on separate axes every (column, layer) pair is a distinct point.
		lateralU = centredStep(seat%UndockLaneColumns, UndockLaneWidth)
		lateralV = centredStep(layer%UndockLaneLayerSpan, UndockLaneWidth/2)
	}

	return n.Scale(float64(row) * UndockLaneStep).
		Add(u.Scale(lateralU)).
		Add(v.Scale(lateralV))
}

// centredStep maps 0,1,2,3,4 onto 0,+w,-w,+2w,-2w so a series fans out around
// the centre line. Callers bound i.
func centredStep(i int, w float64) float64 {
	switch {
	case i <= 0:
		return 0
	case i%2 == 1:
		return float64((i+1)/2) * w
	default:
		return -float64(i/2) * w
	}
}

// PerpendicularBasis returns two unit vectors spanning the plane normal to
// dir. The seed axis is chosen against dir's smallest component so the cross
// product is never degenerate.
func PerpendicularBasis(dir Vec3) (Vec3, Vec3) {
	n := dir.Normalize()
	if n.IsZero() {
		n = Vec3{Z: 1}
	}
	seed := Vec3{X: 1}
	if math.Abs(n.X) >= math.Abs(n.Y) && math.Abs(n.X) >= math.Abs(n.Z) {
		seed = Vec3{Y: 1}
	}
	u := n.Cross(seed).Normalize()
	if u.IsZero() {
		u = Vec3{Y: 1}
	}
	v := n.Cross(u).Normalize()
	return u, v
}

// UndockRunM is how far the undock burn carries a ship clear of the door
// before anything else may steer it. A bounded waypoint, so client and server
// coast to the same point and stop together.
const UndockRunM = 15_000.0

// UndockPlacement is the single undock arithmetic shared by the server and the
// rolebots module: where a ship leaving stationPos is placed, its velocity and
// its goto target. ok is false when the station type has no authored dock entry
// (the exit would be the station centre). slot comes from ClaimUndockSlot.
func UndockPlacement(stationPos, dockEntry, dockOrientation Vec3,
	shipRadius, maxVelocity float64, slot int) (pos, vel, gotoTarget Vec3, ok bool) {

	if dockEntry.IsZero() {
		return Vec3{}, Vec3{}, Vec3{}, false
	}
	dir := dockOrientation.Normalize()
	if dir.IsZero() {
		// Every shipped station type has an orientation; +Z is the fallback for a
		// hand-edited row.
		dir = Vec3{Z: 1}
	}
	exit := UndockExitOffset(dockEntry, dir, shipRadius)
	pos = stationPos.
		Add(exit).
		Add(UndockLaneOffset(dir, slot))
	if pos.IsZero() {
		// A zero position is the rolebots module's "could not resolve" sentinel and
		// must not be returned as success.
		return Vec3{}, Vec3{}, Vec3{}, false
	}
	return pos, dir.Scale(maxVelocity), pos.Add(dir.Scale(UndockRunM)), true
}

// StationStandoffPoint is where a ship waiting at a station (not undocking)
// sits: on the undock axis at stationRadius + buffer, the distance a warp
// landing uses, plus the lane offset for slot. ok is false for a zero station
// position. See docs/undock-geometry.md#standoff-point.
func StationStandoffPoint(stationPos, dockOrientation Vec3,
	stationRadius, buffer float64, slot int) (pos Vec3, ok bool) {

	if stationPos.IsZero() {
		return Vec3{}, false
	}
	dir := dockOrientation.Normalize()
	if dir.IsZero() {
		// Same fallback as UndockPlacement, for the same hand-edited row.
		dir = Vec3{Z: 1}
	}
	if stationRadius < 0 {
		stationRadius = 0
	}
	if buffer < 0 {
		buffer = 0
	}
	pos = stationPos.
		Add(dir.Scale(stationRadius + buffer)).
		Add(UndockLaneOffset(dir, slot))
	if pos.IsZero() {
		return Vec3{}, false
	}
	return pos, true
}
