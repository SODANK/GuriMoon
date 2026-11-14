package main

import (
	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/dogma"
	"gurimoon/marshal"
	"gurimoon/npc"
	"gurimoon/server"
	"log"
	"time"
)

// Server half of NPC electronic warfare. The npc package decides whether a
// module fires (chance roll, range, tactics) and calls in here to land it:
// the debuff on the victim, the OnSpecialFX beam, and a player's broken locks.

// npcEwarModuleTypeID is the module typeID quoted to the client per EWAR kind.
// OnSpecialFX needs a valid invTypes row to resolve its graphics, so each kind
// borrows the Tech 1 module.
var npcEwarModuleTypeID = map[string]int64{
	"scram":           447,   // Warp Scrambler I
	"web":             526,   // Stasis Webifier I
	"jam":             1957,  // ECM - Multispectral Jammer I
	"sensorDamp":      1968,  // Remote Sensor Dampener I
	"trackingDisrupt": 2108,  // Tracking Disruptor I
	"targetPaint":     12709, // Target Painter I
	"capDrain":        533,   // Small Energy Neutralizer I
}

// npcEwarFXGUID maps each kind to the client effect that draws it.
var npcEwarFXGUID = map[string]string{
	"scram":           dogma.FXGUIDWarpScramble,
	"web":             dogma.FXGUIDModifyTargetSpeed,
	"jam":             dogma.FXGUIDECMJam,
	"sensorDamp":      dogma.FXGUIDEwarTargetBeam,
	"trackingDisrupt": dogma.FXGUIDEwarTargetBeam,
	"targetPaint":     dogma.FXGUIDTargetPaint,
	"capDrain":        dogma.FXGUIDEnergyVampire,
}

// npcEwarKindSlot gives each kind its own slot, so two effects from one NPC on
// one victim get distinct keys and neither collides with the turret fire FX,
// which keys its ActiveEffect by the bare ball ID.
var npcEwarKindSlot = map[string]int64{
	"scram": 1, "jam": 2, "sensorDamp": 3,
	"trackingDisrupt": 4, "targetPaint": 5, "capDrain": 6, "web": 7,
}

// npcEwarKey builds the per-(NPC, kind) key used for the victim's debuff map
// and for the effect's moduleID. Multiplying by 100 keeps it clear of the
// bare-ball-ID space that turret FX occupies.
func npcEwarKey(npcID uint32, kind string) int64 {
	return int64(npcID)*100 + npcEwarKindSlot[kind]
}

// applyNPCEwar lands one electronic effect on a target. mgr is passed rather
// than reached for globally because the NPC manager is a local in main().
func applyNPCEwar(mgr *npc.NPCManager, npcEntity *destiny.Entity, ss *destiny.SolarSystem,
	target *destiny.Entity, mod evedb.NPCEwarModule) {

	if npcEntity == nil || ss == nil || target == nil {
		return
	}
	key := npcEwarKey(npcEntity.ID, mod.Kind)

	// Multipliers scale the victim's stat. A type declaring a chance but no
	// multiplier gets a mild effect rather than a 1.0 no-op.
	mult := mod.Multiplier
	if mult <= 0 || mult >= 1 {
		mult = 0.75
	}

	switch mod.Kind {
	case "scram":
		// warpScrambleStrength (attr 105) is 1-6 across the table and is what a
		// warp core stabilizer is measured against.
		strength := mod.Strength
		if strength <= 0 {
			strength = 1
		}
		target.AddWarpScramble(key, strength)
	case "web":
		// attr 20 speedFactor is authored two ways: a negative percent (-75 is x0.25)
		// or a fractional positive multiplier (0.75 is x0.75); unauthored webs at -50.
		factor := mod.Multiplier
		switch {
		case factor > 0 && factor <= 1:
			factor = (factor - 1) * 100
		case factor >= 0:
			factor = -50
		}
		// Never a full stop: a ball at zero maxVelocity is a client divide-by-zero on
		// lock. -95 is the hardest web the table asks for.
		if factor < -95 {
			factor = -95
		}
		newSpeed := target.SetSpeedModifier(key, 1.0+factor/100.0)
		broadcastSetMaxSpeedIn(ss, target, newSpeed)
	case "jam":
		if target.IsPlayer {
			jamPlayerTargets(target, mod.DurationMs)
		} else if mgr != nil {
			mgr.JamTargets(target.ID, time.Now().Add(time.Duration(mod.DurationMs)*time.Millisecond))
		}
	case "sensorDamp":
		target.SetEwarDebuff(key, destiny.EwarDebuff{
			MaxTargetRangeMult: mult,
			ScanResolutionMult: mult,
		})
	case "trackingDisrupt":
		target.SetEwarDebuff(key, destiny.EwarDebuff{
			TurretRangeMult:    mult,
			TurretTrackingMult: mult,
		})
	case "targetPaint":
		// A painter grows the victim's signature, so the multiplier runs the
		// other way from the debuffs above.
		paint := mod.Multiplier
		if paint <= 1 {
			paint = 1.3
		}
		target.SetEwarDebuff(key, destiny.EwarDebuff{SigRadiusMult: paint})
	case "capDrain":
		drainEntityCap(target, mod.Amount)
	default:
		return
	}

	// Damps and painters move numbers the victim's UI shows; a damp also
	// invalidates locks the player already holds.
	switch mod.Kind {
	case "sensorDamp", "trackingDisrupt", "targetPaint":
		pushEwarVictimStats(target, globalDB)
	}

	sendNPCEwarFX(npcEntity, ss, target, mod.Kind, key, mod.DurationMs, true)
}

// clearNPCEwar lifts one electronic effect. Idempotent: the npc package may
// clear the same effect twice if a target is pruned in the tick it expires.
func clearNPCEwar(npcEntity *destiny.Entity, ss *destiny.SolarSystem,
	target *destiny.Entity, kind string) {

	if npcEntity == nil || ss == nil {
		return
	}
	key := npcEwarKey(npcEntity.ID, kind)
	if target != nil {
		switch kind {
		case "scram":
			target.RemoveWarpScramble(key)
		case "web":
			if newSpeed, removed := target.RemoveSpeedModifier(key); removed {
				broadcastSetMaxSpeedIn(ss, target, newSpeed)
			}
		case "sensorDamp", "trackingDisrupt", "targetPaint":
			if target.RemoveEwarDebuff(key) {
				pushEwarVictimStats(target, globalDB)
			}
		}
		// Jams and cap drains are one-shot: the victim's own jam timer runs
		// them out, and drained capacitor recharges normally. Nothing to lift.
	}
	sendNPCEwarFX(npcEntity, ss, target, kind, key, 0, false)
}

// sendNPCEwarFX starts or stops the client-side effect for an NPC EWAR module,
// in the shape of the NPC turret fire FX: tactical.py red-boxes an attacker on
// the effect GUID alone, so a scrambling NPC must announce itself like one
// that shoots.
func sendNPCEwarFX(npcEntity *destiny.Entity, ss *destiny.SolarSystem,
	target *destiny.Entity, kind string, key int64, durationMs int64, start bool) {

	guid, ok := npcEwarFXGUID[kind]
	if !ok {
		return
	}
	modTypeID := npcEwarModuleTypeID[kind]
	var targetID uint32
	if target != nil {
		targetID = target.ID
	}

	var action marshal.PyRep
	var jamEvents []marshal.PyRep
	if start {
		if durationMs <= 0 {
			durationMs = 20000
		}
		ae := destiny.ActiveEffect{
			EntityID:    npcEntity.ID,
			ModuleID:    key,
			ModuleType:  modTypeID,
			TargetID:    targetID,
			GUID:        guid,
			IsOffensive: true,
			DurationMs:  durationMs,
			StartTime:   server.Win32TimeNow(),
		}
		action = destiny.BuildEffectAction(ae)
		// Track it so a player arriving mid-cycle gets the running effect
		// replayed through SetState rather than seeing an unexplained debuff.
		npcEntity.SetActiveEffect(key, ae)
		if jt, ok := destiny.JamBucketForFXGUID(guid); ok && targetID != 0 {
			// The bar counts the lease: a scram is renewed every npc.ScramHold, and the
			// client's ShowTimer keeps whichever timer ends later, so a 20 s bar would win.
			jamMs := durationMs
			if kind == "scram" {
				jamMs = npc.ScramHold.Milliseconds()
			}
			jamEvents = append(jamEvents, destiny.BuildJamEvent(true,
				npcEntity.ID, key, targetID, jt, ae.StartTime, jamMs))
			noteJamEmitted(key)
		}
	} else {
		npcEntity.ClearActiveEffect(key)
		action = destiny.BuildSimpleAction("OnSpecialFX",
			marshal.PyInt{Value: uint64(npcEntity.ID)}, // entityID
			marshal.PyInt{Value: uint64(key)},          // moduleID
			marshal.PyInt{Value: uint64(modTypeID)},    // moduleTypeID
			marshal.PyInt{Value: uint64(targetID)},     // targetID
			marshal.PyNone{},                           // otherTypeID
			marshal.PyList{},                           // area
			marshal.PyString{Value: guid},              // effect_type
			marshal.PyInt{Value: 1},                    // isOffensive
			marshal.PyInt{Value: 0},                    // start
			marshal.PyInt{Value: 0},                    // active
		)
		if jt, ok := destiny.JamBucketForFXGUID(guid); ok && targetID != 0 {
			jamEvents = append(jamEvents, destiny.BuildJamEvent(false,
				npcEntity.ID, key, targetID, jt, 0, 0))
		}
		forgetJamEmitted(key)
	}

	for _, p := range ewarFXWitnesses(ss, npcEntity) {
		if p.Conn == nil || !p.KnowsBall(npcEntity.ID) {
			continue
		}
		if err := p.SendFXStamped(func(stamp uint32) error {
			return destiny.SendActionListEvents(p.Conn, globalNodeID, p.AccountID, stamp,
				jamEvents, action)
		}); err != nil {
			log.Printf("[npc-ewar] FX to %d: %v", p.ID, err)
		}
	}
}

// refreshNPCEwarJamBar re-sends the jam start event for a still-held effect.
// See jambar.go for why a lease needs this and the rest of the machinery.
func refreshNPCEwarJamBar(npcEntity *destiny.Entity, ss *destiny.SolarSystem,
	target *destiny.Entity, kind string, holdMs int64) {

	if npcEntity == nil || ss == nil || target == nil || holdMs <= 0 {
		return
	}
	jt, ok := destiny.JamBucketForFXGUID(npcEwarFXGUID[kind])
	if !ok {
		return
	}
	key := npcEwarKey(npcEntity.ID, kind)
	if !claimJamRefresh(key, holdMs) {
		return
	}
	broadcastJamEvent(ss, npcEntity.ID, key, target.ID, jt, holdMs, true)
}

// ewarFXWitnesses is who sees an EWAR effect: the source's bubble mates, or
// for a POS battery (outside every bubble) every pilot in the system; the
// KnowsBall test that follows narrows it to those holding the ball.
func ewarFXWitnesses(ss *destiny.SolarSystem, src *destiny.Entity) []*destiny.Entity {
	if ss.IsStructureEntity(src.ID) {
		return ss.GetAllPlayersInSystem()
	}
	return ss.GetBubbleConnectedPlayersExcept(src.ID)
}
