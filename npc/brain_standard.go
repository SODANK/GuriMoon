package npc

import (
	"gurimoon/destiny"
	"log"
	"math"
	"time"
)

// moveSwitchDwell is the minimum time an NPC stays in a movement mode
// (Orbit/Follow) before the hysteresis is allowed to switch it again.
const moveSwitchDwell = 8 * time.Second

// StandardBrain is the one NPC brain. What it does comes from its Behavior
// (role + stance, see behavior.go); range values (orbit distance, aggro range,
// leash range) are derived from the weapon profile.
type StandardBrain struct {
	Behavior *Behavior
}

// deriveRanges computes orbit distance, follow range, aggro range and leash
// range from the weapon profile fields in the WorldView.
func deriveRanges(view WorldView) (orbitDist, followRange, aggrRange, leashRange float64) {
	// Defaults for unarmed NPCs.
	orbitDist = 5000
	followRange = 5000
	aggrRange = 80000
	leashRange = 200000

	if view.Weapons == nil {
		// No guns (unarmed or electronic warfare only): orbit at the authored fly range. The
		// aggro range was read off the type at spawn and arrives as view.AggrRange.
		if view.UnarmedFlyRange > 0 {
			orbitDist = view.UnarmedFlyRange
			followRange = orbitDist
		}
	} else {
		// Orbit at FlyRange (preferred engagement range).
		if view.Weapons.FlyRange > 0 {
			orbitDist = view.Weapons.FlyRange
		} else if view.Weapons.OptimalRange > 0 {
			orbitDist = view.Weapons.OptimalRange * 0.8
		}

		// Turrets orbit inside optimal+falloff: FlyRange can exceed it and leave
		// the gun firing deep in falloff.
		if !view.Weapons.IsMissile && view.Weapons.OptimalRange > 0 {
			maxOrbit := view.Weapons.OptimalRange + view.Weapons.FalloffRange
			if orbitDist > maxOrbit {
				orbitDist = view.Weapons.OptimalRange * 0.8
			}
		}

		// Follow at same distance as orbit.
		followRange = orbitDist

		// Aggro range is the larger of EffectiveRange and ChaseMaxDist: ChaseMaxDist
		// alone is a pursuit give-up value and runs 5-20 km on small hulls.
		if r := math.Max(view.Weapons.EffectiveRange, view.Weapons.ChaseMaxDist); r > 0 {
			aggrRange = r
		}
	}

	// A WorldView AggrRange set by a spawner or dungeon wins in both directions;
	// see docs/npc-behaviour.md#hostility-and-threat.
	if view.AggrRange > 0 {
		aggrRange = view.AggrRange
	}

	// Leash at a multiple of aggro range (npc.leashMult).
	leashRange = aggrRange * npcLeashMult()

	return
}

func (b *StandardBrain) Think(view WorldView) CombatAction {
	p := b.Behavior
	ca := CombatAction{Movement: Action{Type: ActionIdle}}

	orbitDist, _, aggrRange, leashRange := deriveRanges(view)

	targetID := b.pickTarget(view, aggrRange, p.TargetPreference)
	targetState := findNearby(view, targetID)

	// A runner resolves before everything else: a fleeing NPC has no movement
	// pattern and no combat commands.
	if p.MovementMode == "flee" {
		if from := fleeFrom(view, targetID); from != 0 {
			return CombatAction{Movement: Action{Type: ActionFlee, TargetID: from}}
		}
		if view.LeashPos != nil && view.LeashOrbitDist > 0 {
			return CombatAction{Movement: leashIdleAction(view)}
		}
		return CombatAction{Movement: Action{Type: ActionIdle}}
	}

	// Leash check: if too far from spawn, return home.
	if leashRange > 0 && view.SpawnPos != (destiny.Vec3{}) {
		dx := view.Self.Pos.X - view.SpawnPos.X
		dy := view.Self.Pos.Y - view.SpawnPos.Y
		dz := view.Self.Pos.Z - view.SpawnPos.Z
		distFromSpawn := math.Sqrt(dx*dx + dy*dy + dz*dz)

		if targetID != 0 && distFromSpawn > leashRange {
			// Too far from home: disengage and return.
			ca.Movement = Action{Type: ActionGoto, Pos: view.SpawnPos}
			return ca
		}
		if targetID == 0 && view.LeashPos == nil && distFromSpawn > 1000 {
			// No target and drifted from spawn: return home.
			ca.Movement = Action{Type: ActionGoto, Pos: view.SpawnPos}
			return ca
		}
	}

	// Leash-idle: with no combat target, patrol a circle around the leash anchor.
	// Applies in every movement mode, so passive decoration ships fly their loop.
	if view.LeashPos != nil && view.LeashOrbitDist > 0 && targetID == 0 {
		ca.Movement = leashIdleAction(view)
		return ca
	}

	// Hold mode: no movement, only combat.
	if p.MovementMode == "hold" {
		return b.thinkCombatOnly(view, targetID)
	}

	if targetID == 0 {
		return ca
	}
	if targetState == nil {
		log.Printf("[npc-brain] entityID=%d: targetState nil for targetID=%d", view.Self.ID, targetID)
		return ca
	}

	// Orbit, closing with Follow when the target is far. Dead-band hysteresis plus
	// a dwell time stop the mode flip-flopping; see docs/npc-behaviour.md#movement-commands.
	switchToFollow := orbitDist * 2.5
	switchToOrbit := orbitDist * 1.8
	canSwitch := view.MoveSwitchAge >= moveSwitchDwell

	if canSwitch && view.LastMovementType == ActionOrbit && targetState.Distance > switchToFollow {
		ca.Movement = Action{Type: ActionFollow, TargetID: targetID, Distance: orbitDist}
	} else if canSwitch && view.LastMovementType == ActionFollow && targetState.Distance < switchToOrbit {
		ca.Movement = Action{Type: ActionOrbit, TargetID: targetID, Distance: orbitDist}
	} else if view.LastMovementType == ActionOrbit {
		ca.Movement = Action{Type: ActionOrbit, TargetID: targetID, Distance: orbitDist}
	} else if view.LastMovementType == ActionFollow {
		ca.Movement = Action{Type: ActionFollow, TargetID: targetID, Distance: orbitDist}
	} else {
		// Initial state: simple threshold.
		if targetState.Distance > orbitDist*2 {
			ca.Movement = Action{Type: ActionFollow, TargetID: targetID, Distance: orbitDist}
		} else {
			ca.Movement = Action{Type: ActionOrbit, TargetID: targetID, Distance: orbitDist}
		}
	}

	// Combat logic (only if autoAggro or has threat).
	if p.AutoAggro || len(view.ThreatTable) > 0 {
		ca.Combat = b.buildCombatCmds(view, targetID, p.MaxTargets)
	}

	return ca
}

// fleeFrom picks what a running NPC is running from, or 0 when it has no
// reason to run yet. Whoever is shooting wins over whoever is nearest, and the
// threat pick is not range-gated: the caller only needs a direction.
func fleeFrom(view WorldView, targetID uint32) uint32 {
	var bestID uint32
	var bestDmg float64
	for _, te := range view.ThreatTable {
		if te.TotalDamage > bestDmg {
			bestDmg = te.TotalDamage
			bestID = te.EntityID
		}
	}
	if bestID != 0 {
		return bestID
	}
	return targetID
}

// findNearby returns the view entry for an entity ID, or nil.
func findNearby(view WorldView, id uint32) *EntityState {
	if id == 0 {
		return nil
	}
	for i := range view.Nearby {
		if view.Nearby[i].ID == id {
			return &view.Nearby[i]
		}
	}
	return nil
}

// leashIdleAction steers the NPC around the leash anchor on the XZ circle of
// radius LeashOrbitDist. The target point is quantized to one of 12 sectors,
// two sectors ahead, so the waypoint is stable across ticks and applyAction's
// dedup drops it. See docs/npc-behaviour.md#movement-commands.
func leashIdleAction(view WorldView) Action {
	anchor := *view.LeashPos
	r := view.LeashOrbitDist
	dx := view.Self.Pos.X - anchor.X
	dz := view.Self.Pos.Z - anchor.Z
	ang := 0.0
	// Dead zone: inside 25% of the radius the angle stays 0, so convergence
	// costs one broadcast outward rather than a burst of sector crossings.
	deadZone := r * 0.25
	if dx*dx+dz*dz > deadZone*deadZone {
		ang = math.Atan2(dz, dx)
	}
	// Normalize to [0, 2pi) for consistent sector bucketing.
	if ang < 0 {
		ang += 2 * math.Pi
	}
	const sectors = 12
	const sectorSize = 2 * math.Pi / sectors
	sector := math.Floor(ang / sectorSize)
	// Aim two sectors ahead: always in front, gentle turns at boundaries.
	targetAng := (sector + 2) * sectorSize
	next := destiny.Vec3{
		X: anchor.X + r*math.Cos(targetAng),
		Y: anchor.Y,
		Z: anchor.Z + r*math.Sin(targetAng),
	}
	return Action{Type: ActionGoto, Pos: next, Idle: true}
}

// thinkCombatOnly handles "hold" mode: no movement, fire at targets in range.
func (b *StandardBrain) thinkCombatOnly(view WorldView, targetID uint32) CombatAction {
	p := b.Behavior
	ca := CombatAction{Movement: Action{Type: ActionIdle}}

	if view.Weapons == nil || targetID == 0 {
		return ca
	}

	// A held NPC only engages inside its own weapon envelope, not out to the
	// wider aggro range the mobile path uses.
	maxRange := view.Weapons.EffectiveRange
	if st := findNearby(view, targetID); st == nil || st.Distance > maxRange {
		return ca
	}

	if p.AutoAggro || len(view.ThreatTable) > 0 {
		ca.Combat = b.buildCombatCmds(view, targetID, p.MaxTargets)
	}
	return ca
}

// buildCombatCmds generates lock/fire commands for the given primary target.
// maxTargets 0 is a behaviour that never locks, so it issues nothing.
func (b *StandardBrain) buildCombatCmds(view WorldView, targetID uint32, maxTargets int) []CombatCmd {
	if targetID == 0 || maxTargets <= 0 {
		return nil
	}
	var cmds []CombatCmd

	// Lock target if not locked.
	isLocked := false
	for _, t := range view.Targets {
		if t == targetID {
			isLocked = true
			break
		}
	}
	if !isLocked && view.Locking != targetID && len(view.Targets) < maxTargets {
		cmds = append(cmds, CombatCmd{Type: CmdLockTarget, TargetID: targetID})
	}

	// Fire at first locked target in range.
	if view.Weapons != nil && view.Weapons.IsReady {
		maxRange := view.Weapons.EffectiveRange
		for _, t := range view.Targets {
			inRange := false
			for i := range view.Nearby {
				if view.Nearby[i].ID == t && view.Nearby[i].Distance <= maxRange {
					inRange = true
					break
				}
			}
			if inRange {
				cmds = append(cmds, CombatCmd{Type: CmdFireWeapons, TargetID: t})
				break // one fire command per cycle
			}
		}
	}

	return cmds
}

// pickTarget selects the best target under the behaviour's TargetPreference.
func (b *StandardBrain) pickTarget(view WorldView, aggrRange float64, pref string) uint32 {
	p := b.Behavior

	if pref == "none" {
		return 0
	}

	// If not autoAggro, only engage threats.
	if !p.AutoAggro && len(view.ThreatTable) == 0 {
		return 0
	}

	// Threat first, pursued out to 1.5x aggro range: a hit taken or a mate's
	// aggro pulls this NPC even past its own cold-aggro range.
	if pref == "highestThreat" || len(view.ThreatTable) > 0 {
		chaseRange := aggrRange * 1.5
		var bestID uint32
		var bestDmg float64
		for _, te := range view.ThreatTable {
			if te.TotalDamage <= bestDmg {
				continue
			}
			for i := range view.Nearby {
				if view.Nearby[i].ID == te.EntityID && view.Nearby[i].Distance <= chaseRange {
					bestDmg = te.TotalDamage
					bestID = te.EntityID
					break
				}
			}
		}
		if bestID != 0 {
			return bestID
		}
	}

	// If not autoAggro and no threat found, don't aggro.
	if !p.AutoAggro {
		return 0
	}

	// Fallback: nearest hostile entity in aggr range.
	var closestID uint32
	closestDist := aggrRange + 1
	for i := range view.Nearby {
		e := &view.Nearby[i]
		if !e.IsHostile {
			continue
		}
		if e.Distance >= closestDist {
			continue
		}
		closestDist = e.Distance
		closestID = e.ID
	}

	// Sticky: keep current target if it's still hostile and within aggr range.
	if view.CurrentTargetID != 0 && view.CurrentTargetID != closestID {
		for i := range view.Nearby {
			e := &view.Nearby[i]
			if e.ID == view.CurrentTargetID && e.IsHostile && e.Distance <= aggrRange {
				return view.CurrentTargetID
			}
		}
	}
	return closestID
}
