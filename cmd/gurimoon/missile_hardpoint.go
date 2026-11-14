// Per-launcher missile spawn geometry and the shared launch queueing path. The
// server has no ship orientation, so a "hardpoint" is a deterministic offset in
// the plane perpendicular to the shot line, indexed by slot: two launchers on
// one ship never spawn on the same point. The offset decides only where the
// visible flight starts; the per-slot stagger (missileSlotStagger) is the main
// de-stacking lever at range, the offset carries close-range shots.
package main

import (
	"math"
	"strconv"
	"time"

	"gurimoon/destiny"
	"gurimoon/dogma"
	"gurimoon/marshal"
	"gurimoon/safego"
)

// missileSlotIndex maps a launcher's fitting flag to a stable 0-7 slot index.
// High slots are flags 27-34; anything else (NPC synthetic slots, a corrupt
// row) falls back to the module item ID so the index is still deterministic.
func missileSlotIndex(modSlotFlag, modItemID int64) int {
	if idx := dogma.SlotIndexForFlag(modSlotFlag); idx >= 0 {
		return int(idx) % 8
	}
	idx := int(modItemID % 8)
	if idx < 0 {
		idx += 8
	}
	return idx
}

const (
	// missileSlotStaggerStep de-phases launchers activated in the same click:
	// slot N's first cycle fires N steps late, and every later cycle inherits
	// the phase. 120ms is enough that two flights read as two missiles.
	missileSlotStaggerStep = 120 * time.Millisecond
	// missileSlotStaggerCap bounds the total added delay so an 8-launcher
	// ship's last tube does not feel unresponsive.
	missileSlotStaggerCap = 500 * time.Millisecond
)

// missileSlotStagger is the pre-first-cycle delay for a launcher in the given
// slot. Slot 0 fires immediately.
func missileSlotStagger(slotIndex int) time.Duration {
	d := time.Duration(slotIndex) * missileSlotStaggerStep
	if d > missileSlotStaggerCap {
		d = missileSlotStaggerCap
	}
	return d
}

// hardpointPattern spreads 8 slots over the plane perpendicular to the shot
// line: the four cardinals first, then the diagonals (unit length, so every
// slot sits at the same distance from the ship's center line).
var hardpointPattern = [8][2]float64{
	{1, 0}, {-1, 0}, {0, 1}, {0, -1},
	{math.Sqrt2 / 2, math.Sqrt2 / 2}, {-math.Sqrt2 / 2, math.Sqrt2 / 2},
	{math.Sqrt2 / 2, -math.Sqrt2 / 2}, {-math.Sqrt2 / 2, -math.Sqrt2 / 2},
}

// missileHardpointOffset is the world-space spawn offset for a launcher.
// Deterministic per (shot line, slot); perpendicular to the shot line so the
// offset never shortens or lengthens the shot; scaled to the hull so a
// frigate's tubes sit meters apart and a battleship's tens of meters.
func missileHardpointOffset(shipPos, targetPos destiny.Vec3, shipRadius float64, slotIndex int) destiny.Vec3 {
	dir := targetPos.Sub(shipPos)
	if dir.IsZero() {
		// Target inside the launcher: no shot line to build a basis from.
		return destiny.Vec3{}
	}
	dir = dir.Normalize()
	up := destiny.Vec3{Y: 1}
	if math.Abs(dir.Y) > 0.99 {
		// Shot line (anti)parallel to world up: pick another basis seed.
		up = destiny.Vec3{X: 1}
	}
	right := dir.Cross(up).Normalize()
	up2 := right.Cross(dir)

	scale := shipRadius * 0.75
	if scale < 15 {
		scale = 15
	}
	if scale > 150 {
		scale = 150
	}
	p := hardpointPattern[((slotIndex%8)+8)%8]
	return right.Scale(p[0] * scale).Add(up2.Scale(p[1] * scale))
}

// queueMissileLaunch queues one missile ball's AddBalls, LaunchMissile and
// (when missileHardpointOffsets is on) a position rewrite that must follow the
// launch, to each recipient. It never flushes; the system tick delivers.
// See docs/combat-notes.md#missile-launch-queueing.
func queueMissileLaunch(players []*destiny.Entity, owner, targetEntity *destiny.Entity, targetID uint32,
	ball destiny.BallDesc, slim destiny.SlimItem, dmg map[uint32]destiny.DamageState,
	perRecipient func(p *destiny.Entity)) {

	launchAction := destiny.BuildLaunchMissileAction(ball.EntityID, targetID, owner.ID)
	key := strconv.FormatUint(uint64(ball.EntityID), 10)
	balls := []destiny.BallDesc{ball}
	slims := []destiny.SlimItem{slim}
	for _, p := range players {
		// Owner and target must be in the recipient's ballpark first: a missing owner
		// makes LaunchMissile a no-op, and a FollowID on an unknown target red-boxes.
		if !ensureBallsKnown(p, owner, targetEntity) {
			continue
		}
		// QueueAddActionFunc, not QueueActionFunc: scheduleMissileRemove tests whether
		// this entry has delivered the ball before it cancels the pair.
		p.QueueAddActionFunc("mslAdd:"+key, ball.EntityID, func(stamp uint32) marshal.PyRep {
			return destiny.BuildAddBallsAction(stamp, balls, slims, dmg)
		})
		p.QueueAction("mslGo:"+key, ball.EntityID, launchAction)
		if missileHardpointOffsets {
			p.QueueActionFunc("mslPos:"+key, ball.EntityID, func(stamp uint32) marshal.PyRep {
				return destiny.BuildBallStateAction(stamp, balls)
			})
		}
		p.MarkBallsKnown(ball.EntityID)
		if perRecipient != nil {
			perRecipient(p)
		}
	}
}

// ---- Removal scheduling ----
// A removal is the missile's whole visible flight, so it waits for its add and
// is never dropped. See docs/combat-notes.md#missile-removal-waits-for-its-add.
const (
	missileRemoveRetry = 200 * time.Millisecond
	missileRemoveGrace = 3 * time.Second
)

// scheduleMissileRemove sleeps out the missile's flight and then retires the
// ball on every recipient that got it, retrying while a recipient's AddBalls is
// still queued. Own goroutine. players is the set captured at launch: a missile
// ball (950M band) is skipped by the reconciliation prune, so nothing else removes it.
func scheduleMissileRemove(players []*destiny.Entity, missileID uint32, explode bool, after time.Duration) {
	if len(players) == 0 {
		return
	}
	go func() {
		defer safego.Recover("scheduleMissileRemove")
		time.Sleep(after)
		deadline := time.Now().Add(missileRemoveGrace)
		for {
			waiting := queueMissileRemove(players, missileID, explode)
			if len(waiting) == 0 {
				return
			}
			if time.Now().After(deadline) {
				// The add never made it out. Cancel the pair: a RemoveBalls for an unknown
				// ball makes the client log "RemoveBall trying to remove non-existing ball".
				for _, p := range waiting {
					p.DropPending(missileID)
					p.ForgetBalls(missileID)
				}
				return
			}
			players = waiting
			time.Sleep(missileRemoveRetry)
		}
	}()
}
