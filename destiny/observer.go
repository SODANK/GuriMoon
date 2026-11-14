package destiny

import "fmt"

// First-sight ball policy: a ball an observer receives for the first time (AddBalls
// or SetState) is demoted to stop with its velocity zeroed when its mode cannot be
// reconstructed by that observer. The ShipSector rides on SubType, not Mode, so a
// mode change alone leaves the ball moving. See docs/destiny-arrival-lanes.md#first-sight-balls.

// BallDescForObserver returns bd sanitized for an observer seeing this ball for the
// first time (AddBalls or SetState). knows reports whether the observer holds a
// given ball and decides FOLLOW/ORBIT/MISSILE references; nil treats every
// reference as unknown. Not for a warp handover: see WarpMirrorBallDesc.
func BallDescForObserver(bd BallDesc, knows func(uint32) bool) BallDesc {
	switch bd.Mode {
	case ModeWarp:
		// The warping manager still sits at the departure point with the destination in
		// its tail. Publish the ball where it is going, at rest.
		bd.Pos = bd.TargetPos
		return stopBall(bd)
	case ModeFollow, ModeMissile:
		if !ballKnown(knows, bd.FollowID) {
			return stopBall(bd)
		}
	case ModeOrbit:
		if !ballKnown(knows, bd.OrbitID) {
			return stopBall(bd)
		}
	}
	return bd
}

// WarpHandoffBallDesc validates a mid-warp ball for delivery to a destination
// bubble. Nothing in the core calls it and new code must not: a ModeWarp ball in
// AddBalls does not arrive. It is kept to refuse that shape with an error.
// See docs/destiny-arrival-lanes.md#first-sight-balls.
func WarpHandoffBallDesc(bd BallDesc) (BallDesc, error) {
	if bd.Mode != ModeWarp {
		return bd, fmt.Errorf("destiny: warp handover for a ball in mode %d, not ModeWarp", bd.Mode)
	}
	if bd.TargetPos.IsZero() {
		return bd, fmt.Errorf("destiny: warp handover with no destination")
	}
	if bd.WarpOwnerID == 0 {
		return bd, fmt.Errorf("destiny: warp handover with no owner — the client has nothing to bind the effect to")
	}
	if bd.WarpEffectStamp == 0 {
		return bd, fmt.Errorf("destiny: warp handover with no effect stamp")
	}
	return bd, nil
}

// stopBall is the demotion. The velocity goes with the mode: the ShipSector rides
// on SubType, so a mode change alone leaves the ball moving.
func stopBall(bd BallDesc) BallDesc {
	bd.Mode = ModeStop
	bd.Vel = Vec3{}
	bd.SpeedFraction = 0
	bd.TargetPos = Vec3{}
	bd.WarpEffectStamp = 0
	bd.WarpOwnerID = 0
	bd.FollowID = 0
	bd.FollowRange = 0
	bd.OrbitID = 0
	bd.OrbitDist = 0
	return bd
}

func ballKnown(knows func(uint32) bool, id uint32) bool {
	if id == 0 {
		// Nothing to chase. The mode's own switch arm decides whether to demote; this
		// only says the reference is not known.
		return false
	}
	if knows == nil {
		return false
	}
	return knows(id)
}

// WarpMirrorBallDesc returns the ball to hand a destination-grid witness on the
// final approach of a warp, sent in the same action list as the WarpTo that flies
// it in. pos and vel come from WarpMirrorPoint; the velocity must stay, since the
// dll arms the WarpTo only on a moving ball. See docs/destiny-arrival-lanes.md#warp-mirror-the-destination-grid.
func WarpMirrorBallDesc(bd BallDesc, pos, vel Vec3) BallDesc {
	bd = stopBall(bd)
	bd.Pos = pos
	bd.Vel = vel
	bd.SpeedFraction = 1
	return bd
}
