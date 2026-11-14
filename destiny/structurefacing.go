package destiny

import (
	"math"

	"gurimoon/marshal"
)

// Turning an anchored POS structure to face its moon. The wire has no orientation;
// the heading is the ball's velocity direction plus the dll's blend toward its
// (zero) GOTO slot, set once by SetBallVelocity. See docs/destiny-dll-spec.md#rigid-ball-facing.

// structureFacingSpeed is the magnitude handed to SetBallVelocity. Only the
// direction matters (a rigid ball never integrates it); it is small so that
// anything displaying the ball's speed reads 0.
const structureFacingSpeed = 0.001

// facingBlend is the dll's 0.3 (constant at 0x10058618).
const facingBlend = 0.3

// groupMoonHarvester is invGroups 416 (Moon Mining): the harvesting arrays, whose
// business end belongs pointed at a moon.
const groupMoonHarvester = 416

// celGroupMoon is invGroups 8.
const celGroupMoon = 8

// RigidFacingVelocity returns the velocity that leaves a rigid ball at pos heading
// along dir once the dll adds its blend term 0.3*s (s = unit vector from the ball
// to the origin): u = lambda*d - 0.3*s with lambda = 0.3(d.s) + sqrt(0.09(d.s)^2 + 0.91)
// keeps |u| = 1 and the heading exactly lambda*d. A zero dir returns zero.
func RigidFacingVelocity(pos, dir Vec3) Vec3 {
	if dir.IsZero() {
		return Vec3{}
	}
	d := dir.Normalize()
	if pos.IsZero() {
		return d.Scale(structureFacingSpeed)
	}
	s := pos.Scale(-1).Normalize()
	ds := d.X*s.X + d.Y*s.Y + d.Z*s.Z
	lambda := facingBlend*ds + math.Sqrt(facingBlend*facingBlend*ds*ds+1-facingBlend*facingBlend)
	u := d.Scale(lambda).Sub(s.Scale(facingBlend))
	return u.Scale(structureFacingSpeed)
}

// structureFacesMoon reports whether a starbase-slice ball should be turned
// to face its moon: an anchored (rigid) moon harvesting array.
func structureFacesMoon(bd BallDesc, slim SlimItem) bool {
	return bd.Mode == ModeRigid && slim.GroupID == groupMoonHarvester
}

// nearestMoonLocked is the moon whose surface is closest to pos: the moon an
// anchored structure's tower stands at (towers sit tens to hundreds of km off
// their moon; moons are tens of thousands of km apart). Caller holds ss.mu.
func (ss *SolarSystem) nearestMoonLocked(pos Vec3) (Vec3, bool) {
	best := math.Inf(1)
	var at Vec3
	found := false
	for i, b := range ss.CelBalls {
		if i >= len(ss.CelSlims) || ss.CelSlims[i].GroupID != celGroupMoon {
			continue
		}
		if d := pos.Sub(b.Pos).Len() - b.Radius; d < best {
			best, at, found = d, b.Pos, true
		}
	}
	return at, found
}

// StructureFacingActions builds the SetBallVelocity actions that turn every
// anchored moon harvesting array among balls to face its moon. Send them stamped
// right after the SetState or AddBalls that presented the balls; a rebuilt
// ballpark has no heading. Safe for concurrent use.
func (ss *SolarSystem) StructureFacingActions(balls []BallDesc, slims []SlimItem) []marshal.PyRep {
	if ss == nil {
		return nil
	}
	var out []marshal.PyRep
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	for i, bd := range balls {
		if i >= len(slims) || !structureFacesMoon(bd, slims[i]) {
			continue
		}
		moon, ok := ss.nearestMoonLocked(bd.Pos)
		if !ok {
			continue
		}
		v := RigidFacingVelocity(bd.Pos, moon.Sub(bd.Pos))
		if v.IsZero() {
			continue
		}
		out = append(out, BuildSimpleAction("SetBallVelocity",
			marshal.PyInt{Value: uint64(bd.EntityID)},
			marshal.PyReal{Value: v.X}, marshal.PyReal{Value: v.Y}, marshal.PyReal{Value: v.Z}))
	}
	return out
}
