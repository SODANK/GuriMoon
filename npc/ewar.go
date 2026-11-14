package npc

import (
	"context"
	"gurimoon/db"
	"gurimoon/destiny"
	"log"
	"math"
	"math/rand"
	"time"
)

// NPC electronic warfare. dgmTypeAttributes carries a per-type electronic
// loadout (warp scramble, ECM, tracking disruption, damping, painting, cap
// drain), each a per-cycle chance, a duration and a range, so the runtime is
// one loop over a table. Capability comes from the hull; an NPC uses whatever
// its type carries against its primary target.

// EwarApplyFunc is called when an NPC's electronic module lands on a target.
// The server layer owns the client-facing half: the debuff push, the
// OnSpecialFX beam, breaking a player's locks.
type EwarApplyFunc func(npcEntity *destiny.Entity, ss *destiny.SolarSystem,
	target *destiny.Entity, mod db.NPCEwarModule)

// EwarClearFunc is called when a landed effect expires, the target is lost, or
// the NPC dies. Must be idempotent: the same (npc, target, kind) may be
// cleared twice if a target is pruned in the same tick it expires.
type EwarClearFunc func(npcEntity *destiny.Entity, ss *destiny.SolarSystem,
	target *destiny.Entity, kind string)

// EwarRefreshFunc is called on every tick a lease-based effect (warp
// scrambling) is renewed rather than newly applied, so the client-facing
// countdown stays alive. Fires on the combat cadence; must throttle itself.
type EwarRefreshFunc func(npcEntity *destiny.Entity, ss *destiny.SolarSystem,
	target *destiny.Entity, kind string, holdMs int64)

// ewarActive tracks one landed effect so it can be lifted again.
type ewarActive struct {
	TargetID uint32
	Until    time.Time
}

// scramRetry is how long an NPC waits before rolling its warp scrambler again
// after a miss. entityWarpScrambleChance carries no cycle attribute, so the
// cadence is fixed here.
const scramRetry = 10 * time.Second

// ScramHold is how far ahead a successful scram's expiry is pushed each tick it
// stays valid. It is a lease: the effect lasts while the NPC holds the target
// locked and in range, and lapses shortly after it stops refreshing.
const ScramHold = 8 * time.Second

// scramFallbackRange stands in for warpScrambleRange (attr 103) on a scrambling
// type that ships without it, and caps the weapon-derived fallback.
const scramFallbackRange = 20000

// tickEwar runs the NPC's electronic modules for one tick. Called from
// tickNPC on the combat cadence, without rec.mu held.
func (m *NPCManager) tickEwar(rec *NPCRecord, now time.Time) {
	if m.ewarApply == nil {
		return
	}

	rec.mu.Lock()
	if rec.Dead || rec.EwarProfile == nil {
		rec.mu.Unlock()
		return
	}
	entity, ss := rec.Entity, rec.System
	if ss == nil {
		rec.mu.Unlock()
		return
	}

	// Expire anything whose lease has run out.
	type clearOp struct {
		targetID uint32
		kind     string
	}
	var clears []clearOp
	for kind, act := range rec.ewarActive {
		if now.After(act.Until) {
			clears = append(clears, clearOp{act.TargetID, kind})
			delete(rec.ewarActive, kind)
		}
	}

	// Primary target: EWAR only ever applies to something the NPC has locked,
	// so the player always gets the lock warning before the effect lands.
	var primary uint32
	if len(rec.Targets) > 0 {
		primary = rec.Targets[0]
	}

	type applyOp struct {
		targetID uint32
		mod      db.NPCEwarModule
	}
	var applies, renews []applyOp

	if primary != 0 && !now.Before(rec.JamUntil) {
		selfPos := entity.Pos()
		for _, mod := range rec.EwarProfile.Modules {
			maxRange := mod.MaxRange
			if mod.Kind == "scram" && maxRange <= 0 {
				// Attr 103 is the authored range. Only a type that ships without it falls
				// back to the weapon range, clamped to scramFallbackRange.
				if rec.WeaponProfile != nil {
					maxRange = rec.WeaponProfile.AttackRange
				}
				if maxRange <= 0 || maxRange > scramFallbackRange {
					maxRange = scramFallbackRange
				}
			}
			if maxRange <= 0 {
				continue
			}

			target := ss.GetEntity(primary)
			if target == nil {
				continue
			}
			dist := selfPos.Sub(target.Pos()).Len()
			chance, inRange := ewarChanceAt(mod.Chance, maxRange, mod.Falloff, dist)
			if !inRange {
				continue
			}

			if act, ok := rec.ewarActive[mod.Kind]; ok {
				// Already landed. Scram is a lease that renews while the
				// conditions hold; the timed kinds ride out their duration.
				if mod.Kind == "scram" && act.TargetID == primary {
					act.Until = now.Add(ScramHold)
					rec.ewarActive[mod.Kind] = act
					renews = append(renews, applyOp{primary, mod})
				}
				continue
			}
			if next, ok := rec.ewarNextTry[mod.Kind]; ok && now.Before(next) {
				continue
			}

			hold := time.Duration(mod.DurationMs) * time.Millisecond
			if mod.Kind == "scram" {
				hold = ScramHold
			}
			if hold <= 0 {
				hold = 20 * time.Second
			}

			if rand.Float64() >= chance {
				// Missed this cycle: try again after the cycle would have run.
				retry := hold
				if mod.Kind == "scram" {
					retry = scramRetry
				}
				rec.ewarNextTry[mod.Kind] = now.Add(retry)
				continue
			}

			rec.ewarActive[mod.Kind] = ewarActive{TargetID: primary, Until: now.Add(hold)}
			rec.ewarNextTry[mod.Kind] = now.Add(hold)
			applies = append(applies, applyOp{primary, mod})
		}
	}
	rec.mu.Unlock()

	for _, c := range clears {
		if t := ss.GetEntity(c.targetID); t != nil {
			m.ewarClear(entity, ss, t, c.kind)
		}
		log.Printf("[npc-ewar] entityID=%d %s OFF target=%d", entity.ID, c.kind, c.targetID)
	}
	for _, a := range applies {
		if t := ss.GetEntity(a.targetID); t != nil {
			m.ewarApply(entity, ss, t, a.mod)
			log.Printf("[npc-ewar] entityID=%d %s ON target=%d chance=%.2f range=%.0f dur=%dms",
				entity.ID, a.mod.Kind, a.targetID, a.mod.Chance, a.mod.MaxRange, a.mod.DurationMs)
		}
	}
	// A renewal is not logged: it happens every tick a scram holds.
	if m.ewarRefresh != nil {
		for _, r := range renews {
			if t := ss.GetEntity(r.targetID); t != nil {
				m.ewarRefresh(entity, ss, t, r.mod.Kind, ScramHold.Milliseconds())
			}
		}
	}
}

// ewarChanceAt applies the falloff curve to a module's per-cycle chance and
// reports whether the target is inside the module's reach: full chance inside
// optimal, halving once per falloff squared, cut off at two falloffs. A module
// with no falloff (warp scrambling) keeps a hard edge at optimal.
func ewarChanceAt(chance, maxRange, falloff, dist float64) (float64, bool) {
	if dist <= maxRange {
		return chance, true
	}
	if falloff <= 0 || dist > maxRange+2*falloff {
		return 0, false
	}
	x := (dist - maxRange) / falloff
	return chance * math.Pow(0.5, x*x), true
}

// clearAllEwar lifts every effect this NPC has applied. Called on death,
// removal and system teardown so a gone NPC leaves nobody scrambled.
func (m *NPCManager) clearAllEwar(rec *NPCRecord) {
	if m.ewarClear == nil {
		return
	}
	rec.mu.Lock()
	if len(rec.ewarActive) == 0 {
		rec.mu.Unlock()
		return
	}
	entity, ss := rec.Entity, rec.System
	pending := make(map[string]uint32, len(rec.ewarActive))
	for kind, act := range rec.ewarActive {
		pending[kind] = act.TargetID
	}
	rec.ewarActive = make(map[string]ewarActive)
	rec.mu.Unlock()

	if ss == nil {
		return
	}
	for kind, targetID := range pending {
		if t := ss.GetEntity(targetID); t != nil {
			m.ewarClear(entity, ss, t, kind)
		}
		log.Printf("[npc-ewar] entityID=%d %s OFF target=%d (npc gone)", entity.ID, kind, targetID)
	}
}

// clearEwarOnTarget lifts every effect this NPC has applied to one specific
// entity. Called when a target is unlocked or pruned from the bubble.
func (m *NPCManager) clearEwarOnTarget(rec *NPCRecord, targetID uint32) {
	if m.ewarClear == nil {
		return
	}
	rec.mu.Lock()
	var kinds []string
	for kind, act := range rec.ewarActive {
		if act.TargetID == targetID {
			kinds = append(kinds, kind)
			delete(rec.ewarActive, kind)
		}
	}
	entity, ss := rec.Entity, rec.System
	rec.mu.Unlock()

	if ss == nil || len(kinds) == 0 {
		return
	}
	target := ss.GetEntity(targetID)
	for _, kind := range kinds {
		if target != nil {
			m.ewarClear(entity, ss, target, kind)
		}
		log.Printf("[npc-ewar] entityID=%d %s OFF target=%d (target lost)", entity.ID, kind, targetID)
	}
}

// InvalidateTypeCaches drops the manager's per-type caches (signature radius, EWAR loadout) for
// the given types, so an edit to the type's attributes reaches the next spawn and the next hit
// roll. NPCs already in space keep the profile they spawned with.
func (m *NPCManager) InvalidateTypeCaches(typeIDs ...int64) {
	for _, id := range typeIDs {
		m.sigCache.Delete(id)
		m.ewarCache.Delete(id)
	}
}

// ewarProfileFor returns an NPC type's electronic loadout, caching the result
// (including "this type has none", the common case) so respawns don't re-read
// dgmTypeAttributes.
func (m *NPCManager) ewarProfileFor(typeID int64) *db.NPCEwarProfile {
	if m.sqldb == nil {
		return nil
	}
	if p, ok := m.ewarCache.Load(typeID); ok {
		return p
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	p, err := db.GetNPCEwarProfile(ctx, m.sqldb, typeID)
	if err != nil {
		log.Printf("[npc-ewar] GetNPCEwarProfile typeID=%d: %v", typeID, err)
		return nil
	}
	m.ewarCache.Store(typeID, p)
	if p != nil {
		kinds := make([]string, 0, len(p.Modules))
		for _, mod := range p.Modules {
			kinds = append(kinds, mod.Kind)
		}
		log.Printf("[npc-ewar] typeID=%d carries %v", typeID, kinds)
	}
	return p
}
