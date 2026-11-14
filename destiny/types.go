// Package destiny implements the EVEmu Destiny physics and space-state subsystem.
// It encodes the binary ball-state blobs, manages per-entity physics, and builds
// the DoDestinyUpdates Notification packets that the EVE client consumes.
package destiny

import "math"

// Physics constants: client-side values, must not be changed.
const (
	SpaceFriction   = 1.0e6 // SPACE_FRICTION from DestinyManager.cpp
	TicDuration     = 1.0   // TIC_DURATION_IN_SECONDS
	FollowBandWidth = 100.0 // FOLLOW_BAND_WIDTH
)

// BallMode is the per-entity movement mode in the binary destiny blob. Values
// match DSTBALL_* as registered by the client's destiny.dll (build 3913), FIELD 9
// and MINIBALL 10 included. See docs/destiny-dll-spec.md#ball-modes-verified.
type BallMode uint8

const (
	ModeGoto      BallMode = 0
	ModeFollow    BallMode = 1
	ModeStop      BallMode = 2
	ModeWarp      BallMode = 3
	ModeOrbit     BallMode = 4
	ModeMissile   BallMode = 5
	ModeMushroom  BallMode = 6
	ModeBoid      BallMode = 7
	ModeTroll     BallMode = 8
	ModeField     BallMode = 9
	ModeMiniball  BallMode = 10
	ModeRigid     BallMode = 11
	ModeFormation BallMode = 12
)

// SubType values for a rigid ball: a flag byte read by destiny.dll, 0x02 IsGlobal,
// 0x04 IsMassive (a client-side collision sphere of the ball's radius, applied to
// ships in warp too). See docs/destiny-dll-spec.md#ball-subtypes-and-collision.
const (
	SubTypeRigid      uint8 = 2 // Global; flown through
	SubTypeRigidSolid uint8 = 6 // Global|Massive; collidable sphere
)

// RigidSubType is the SubType for a rigid ball that is or is not collidable.
func RigidSubType(solid bool) uint8 {
	if solid {
		return SubTypeRigidSolid
	}
	return SubTypeRigid
}

// LightYear is one light year in metres. mapSolarSystems x/y/z are metres and
// jumpDriveRange (867) is light years, so every capital jump range check divides
// by this. It lives here so code outside cmd/gurimoon can reach it.
const LightYear = 9.4607304725808e15

// Vec3 is a 3-component vector of float64, used for positions and velocities.
type Vec3 struct{ X, Y, Z float64 }

// LightYearsTo returns the distance from v to o in light years.
func (v Vec3) LightYearsTo(o Vec3) float64 { return o.Sub(v).Len() / LightYear }

func (v Vec3) Add(o Vec3) Vec3      { return Vec3{v.X + o.X, v.Y + o.Y, v.Z + o.Z} }
func (v Vec3) Sub(o Vec3) Vec3      { return Vec3{v.X - o.X, v.Y - o.Y, v.Z - o.Z} }
func (v Vec3) Scale(s float64) Vec3 { return Vec3{v.X * s, v.Y * s, v.Z * s} }
func (v Vec3) LenSq() float64       { return v.X*v.X + v.Y*v.Y + v.Z*v.Z }
func (v Vec3) Len() float64         { return math.Sqrt(v.LenSq()) }
func (v Vec3) IsZero() bool         { return v.X == 0 && v.Y == 0 && v.Z == 0 }
func (v Vec3) Dot(o Vec3) float64   { return v.X*o.X + v.Y*o.Y + v.Z*o.Z }

// Cross returns the vector cross product v × o.
func (v Vec3) Cross(o Vec3) Vec3 {
	return Vec3{
		X: v.Y*o.Z - v.Z*o.Y,
		Y: v.Z*o.X - v.X*o.Z,
		Z: v.X*o.Y - v.Y*o.X,
	}
}

// Lerp returns the linear interpolation between v and other at parameter t (0→v, 1→other).
func (v Vec3) Lerp(other Vec3, t float64) Vec3 {
	return Vec3{
		X: v.X + (other.X-v.X)*t,
		Y: v.Y + (other.Y-v.Y)*t,
		Z: v.Z + (other.Z-v.Z)*t,
	}
}

// Normalize returns a unit vector, or zero if the vector has zero length.
func (v Vec3) Normalize() Vec3 {
	l := v.Len()
	if l == 0 {
		return Vec3{}
	}
	return v.Scale(1 / l)
}
