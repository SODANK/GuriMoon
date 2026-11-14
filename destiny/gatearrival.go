package destiny

import (
	"math"
	"math/rand"
)

// Gate arrival scatter: a jump-in lands GateArrivalRangeM from the gate on a
// bearing drawn uniformly by solid angle from a cone around the jump line, so a
// crowd spreads over a cap instead of a row. See docs/destiny-arrival-lanes.md#gate-scatter.
const (
	// GateArrivalRangeM is how far from the destination gate's centre a jump-in lands.
	GateArrivalRangeM = 15000.0

	// GateArrivalConeDeg is the half-angle of the scatter cone around the jump line.
	// 60 degrees keeps every landing on the near side of the gate while spreading a
	// crowd over a cap of about 7.5 km radius.
	GateArrivalConeDeg = 60.0
)

// GateArrivalDir returns a unit vector within GateArrivalConeDeg of axis,
// uniformly distributed over that cone's solid angle. rnd supplies uniforms in
// [0,1) (rand.Float64 in production; a fixed source in tests). A zero axis is
// answered with +X.
func GateArrivalDir(axis Vec3, rnd func() float64) Vec3 {
	if axis.IsZero() {
		axis = Vec3{X: 1}
	}
	if rnd == nil {
		rnd = rand.Float64
	}
	n := axis.Normalize()
	cosCone := math.Cos(GateArrivalConeDeg * math.Pi / 180)
	// Polar angle: cos(theta) uniform on [cosCone, 1].
	cosT := cosCone + (1-cosCone)*rnd()
	sinT := math.Sqrt(math.Max(0, 1-cosT*cosT))
	// Azimuth: uniform round the axis.
	phi := 2 * math.Pi * rnd()
	u, v := PerpendicularBasis(n)
	return n.Scale(cosT).
		Add(u.Scale(sinT * math.Cos(phi))).
		Add(v.Scale(sinT * math.Sin(phi)))
}

// GateArrivalPoint is where a jump-in lands: GateArrivalRangeM from gate along
// a scattered radial. It returns the landing and the radial it lies on, which
// is the direction the arrival lane then runs outward in.
func GateArrivalPoint(gate, jumpDir Vec3, rnd func() float64) (landing, dir Vec3) {
	dir = GateArrivalDir(jumpDir, rnd)
	return gate.Add(dir.Scale(GateArrivalRangeM)), dir
}
