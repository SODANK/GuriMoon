package main

import (
	"context"
	"database/sql"
	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/dogma"
	"gurimoon/drone"
	"gurimoon/marshal"
	"gurimoon/npc"
	"gurimoon/server"
	"log"
	"math"
	"math/rand"
	"time"
)

// Server half of electronic-warfare drones. The drone manager decides when a
// cycle comes due; this file decides what the cycle does. Electronic Warfare
// Drones (group 639), Cap Drain Drones (544) and Stasis Webifying Drones (641)
// carry the same dgmEffects row and attributes as the matching ship module, so
// the rolls and debuffs here are the module math with the drone as the source.

// droneEwarFXGUID maps each drone effect to the client visual for it.
var droneEwarFXGUID = map[string]string{
	dogma.EffNameECMJam:              dogma.FXGUIDECMJam,
	dogma.EffNameSensorDamp:          dogma.FXGUIDEwarTargetBeam,
	dogma.EffNameTrackingDisrupt:     dogma.FXGUIDEwarTargetBeam,
	dogma.EffNameTargetPaint:         dogma.FXGUIDTargetPaint,
	dogma.EffNameEnergyVampire:       dogma.FXGUIDEnergyVampire,     // Cap Drain Drone (group 544)
	dogma.EffNameDecreaseTargetSpeed: dogma.FXGUIDModifyTargetSpeed, // Stasis Webifying Drone (641)
}

// droneEwarKind returns the EWAR effect a drone type runs, or "" for an
// ordinary combat or mining drone.
func droneEwarKind(ctx context.Context, sqldb *sql.DB, typeID int64) string {
	if sqldb == nil {
		return ""
	}
	effects, err := evedb.GetTypeEffects(ctx, sqldb, typeID)
	if err != nil {
		log.Printf("[drone-ewar] GetTypeEffects(%d): %v", typeID, err)
		return ""
	}
	for _, e := range effects {
		if _, ok := droneEwarFXGUID[e.EffectName]; ok {
			return e.EffectName
		}
	}
	return ""
}

// droneEwarKey is the effect/debuff key for one drone: its bare ball ID, the
// key the drone's turret FX also uses. An EWAR drone never shoots, so no clash.
func droneEwarKey(rec *drone.Record) int64 {
	return int64(rec.Entity.ID)
}

// droneEwarCycle rolls and applies one EWAR drone cycle.
func droneEwarCycle(rec *drone.Record, target *destiny.Entity, dist float64,
	sqldb *sql.DB, npcMgr *npc.NPCManager) {

	if rec == nil || target == nil || rec.System == nil || rec.Entity == nil {
		return
	}
	a := rec.Attrs
	attrs := a.EwarAttrs
	if attrs == nil {
		attrs = map[int64]float64{}
	}
	key := droneEwarKey(rec)
	durationMs := a.CycleMs
	if durationMs <= 0 {
		durationMs = 5000
	}

	// Range roll: inside optimal it always lands, past it the same falloff
	// curve the chance-based modules use.
	if a.Optimal > 0 && dist > a.Optimal && a.Falloff > 0 {
		chance := math.Pow(2, -((dist-a.Optimal)*(dist-a.Optimal))/(a.Falloff*a.Falloff))
		if rand.Float64() > chance {
			log.Printf("[drone-ewar] drone=%d %s out of range on %d (%.0fm)", rec.Entity.ID, a.EwarKind, target.ID, dist)
			droneEwarClear(rec, target)
			return
		}
	}

	switch a.EwarKind {
	case dogma.EffNameDecreaseTargetSpeed:
		// Webbing drone: speedFactor is a percentage change (-60 is x0.4) and holds
		// for as long as the drone stays on the target.
		mult := 1.0 + attrs[dogma.AttrSpeedFactor]/100.0
		if mult <= 0 || mult >= 1 {
			mult = 0.5
		}
		newSpeed := target.SetSpeedModifier(key, mult)
		broadcastSetMaxSpeedIn(rec.System, target, newSpeed)
		droneEwarAggression(rec, target, sqldb, npcMgr)
		log.Printf("[drone-ewar] drone=%d web ON target=%d ×%.2f → %.0f m/s", rec.Entity.ID, target.ID, mult, newSpeed)
		sendDroneEwarFX(rec, target, key, durationMs, true)
		return

	case dogma.EffNameEnergyVampire:
		// Cap drain drone: what it takes goes to the ship controlling it, under the
		// same pre-2007 nosferatu rule as the module (capWarfareBite).
		drained, gained := capWarfareBite(target, attrs[dogma.AttrPowerTransferAmount], true)
		if gained > 0 {
			_, _, _, masterID, _ := rec.Snapshot()
			if master := rec.System.GetEntity(masterID); master != nil && master.HasCap() {
				newVal := master.BoostCap(gained)
				if master.Conn != nil {
					if mds, isConn := master.PlayerState.(*destinyConn); isConn {
						sendModuleAttributeChange(master.Conn, int64(master.AccountID), mds.charID,
							mds.shipID, dogma.AttrCharge, newVal, newVal-gained)
					}
				}
			}
			droneEwarAggression(rec, target, sqldb, npcMgr)
			log.Printf("[drone-ewar] drone=%d drained %.1f GJ from %d, master gained %.1f", rec.Entity.ID, drained, target.ID, gained)
		}
		sendDroneEwarFX(rec, target, key, durationMs, true)
		return
	}

	if a.EwarKind == dogma.EffNameECMJam {
		// Jam strength against the victim's best sensor, as in chanceEffectLoop.
		tgtAttrs := map[int64]float64{}
		if sqldb != nil {
			taCtx, taCancel := context.WithTimeout(context.Background(), 3*time.Second)
			if ta, err := evedb.GetTypeAttributes(taCtx, sqldb, int64(target.Slim.TypeID)); err == nil {
				tgtAttrs = ta
			}
			taCancel()
		}
		var maxA, maxD float64
		havePair := false
		for _, pair := range electronicJamPairs {
			av := attrs[pair[0]]
			if av < 1.0 {
				continue
			}
			dv := tgtAttrs[pair[1]]
			switch {
			case !havePair || dv > maxD:
				maxA, maxD, havePair = av, dv, true
			case dv == maxD && av > maxA:
				maxA = av
			}
		}
		if havePair && maxD > 0 && rand.Float64() > maxA/maxD {
			log.Printf("[drone-ewar] drone=%d jam MISSED %d (%.1f vs %.1f)", rec.Entity.ID, target.ID, maxA, maxD)
			return
		}
		log.Printf("[drone-ewar] drone=%d JAMMED %d for %dms", rec.Entity.ID, target.ID, durationMs)
		if target.IsPlayer {
			jamPlayerTargets(target, durationMs)
		} else if npcMgr != nil {
			npcMgr.JamTargets(target.ID, time.Now().Add(time.Duration(durationMs)*time.Millisecond))
		}
		droneEwarAggression(rec, target, sqldb, npcMgr)
		sendDroneEwarFX(rec, target, key, durationMs, true)
		return
	}

	var debuff destiny.EwarDebuff
	switch a.EwarKind {
	case dogma.EffNameTrackingDisrupt:
		debuff = destiny.EwarDebuff{
			TurretRangeMult:    attrs[dogma.AttrMaxRangeMultiplier],
			TurretTrackingMult: attrs[dogma.AttrTrackingSpeedMultiplier],
		}
	case dogma.EffNameSensorDamp:
		debuff = destiny.EwarDebuff{
			MaxTargetRangeMult: attrs[dogma.AttrMaxTargetRangeMultiplier],
			ScanResolutionMult: attrs[dogma.AttrScanResolutionMultiplier],
		}
	case dogma.EffNameTargetPaint:
		debuff = destiny.EwarDebuff{SigRadiusMult: 1.0 + attrs[dogma.AttrSignatureRadiusBonus]/100.0}
	default:
		return
	}
	target.SetEwarDebuff(key, debuff)
	pushEwarVictimStats(target, sqldb)
	droneEwarAggression(rec, target, sqldb, npcMgr)
	log.Printf("[drone-ewar] drone=%d %s ON target=%d %+v", rec.Entity.ID, a.EwarKind, target.ID, debuff)
	sendDroneEwarFX(rec, target, key, durationMs, true)
}

// droneEwarClear lifts an EWAR drone's debuff from one target and stops the
// beam. Idempotent: the drone tick calls it whenever the drone loses, changes
// or drifts away from its target, and the drone's death path calls it too.
func droneEwarClear(rec *drone.Record, target *destiny.Entity) {
	if rec == nil || rec.Entity == nil {
		return
	}
	key := droneEwarKey(rec)
	if target != nil {
		if target.RemoveEwarDebuff(key) {
			log.Printf("[drone-ewar] drone=%d %s OFF target=%d", rec.Entity.ID, rec.Attrs.EwarKind, target.ID)
			pushEwarVictimStats(target, globalDB)
		}
		if newSpeed, removed := target.RemoveSpeedModifier(key); removed {
			broadcastSetMaxSpeedIn(rec.System, target, newSpeed)
			log.Printf("[drone-ewar] drone=%d web OFF target=%d → %.0f m/s", rec.Entity.ID, target.ID, newSpeed)
		}
	}
	sendDroneEwarFX(rec, target, key, 0, false)
}

// droneEwarClearTarget is the StopFire hook: it resolves the ball ID the
// manager reports and lifts the debuff from it.
func droneEwarClearTarget(rec *drone.Record, targetID uint32) {
	if rec == nil || rec.System == nil || rec.Attrs.EwarKind == "" {
		return
	}
	droneEwarClear(rec, rec.System.GetEntity(targetID))
}

// droneEwarAggression makes EWAR a crime the same way damage is: the victim
// red-boxes the drone, and the offence belongs to the controlling pilot.
func droneEwarAggression(rec *drone.Record, target *destiny.Entity, sqldb *sql.DB, npcMgr *npc.NPCManager) {
	if !target.IsPlayer {
		return
	}
	ss := rec.System
	target.AddAggressor(rec.Entity.ID, time.Now().Unix())
	_, _, _, masterID, _ := rec.Snapshot()
	if master := ss.GetEntity(masterID); master != nil && master.IsPlayer {
		recordPlayerAggression(master, target, ss)
		maybeTriggerConcordResponse(master, target, ss, sqldb, npcMgr)
	}
}

// sendDroneEwarFX starts or stops the client-side beam from the drone ball,
// mirroring sendNPCEwarFX; the victim only red-boxes what it can see.
func sendDroneEwarFX(rec *drone.Record, target *destiny.Entity, key int64, durationMs int64, start bool) {
	guid, ok := droneEwarFXGUID[rec.Attrs.EwarKind]
	if !ok {
		return
	}
	e := rec.Entity
	ss := rec.System
	var targetID uint32
	if target != nil {
		targetID = target.ID
	}

	var action marshal.PyRep
	if start {
		if durationMs <= 0 {
			durationMs = 5000
		}
		ae := destiny.ActiveEffect{
			EntityID:    e.ID,
			ModuleID:    key,
			ModuleType:  rec.TypeID,
			TargetID:    targetID,
			GUID:        guid,
			IsOffensive: true,
			DurationMs:  durationMs,
			StartTime:   server.Win32TimeNow(),
		}
		// Already running against this target: the client repeats it itself.
		for _, existing := range e.GetActiveEffects() {
			if existing.ModuleID == key && existing.TargetID == targetID {
				return
			}
		}
		action = destiny.BuildEffectAction(ae)
		e.SetActiveEffect(key, ae)
	} else {
		e.ClearActiveEffect(key)
		action = destiny.BuildSimpleAction("OnSpecialFX",
			marshal.PyInt{Value: uint64(e.ID)},
			marshal.PyInt{Value: uint64(key)},
			marshal.PyInt{Value: uint64(rec.TypeID)},
			marshal.PyInt{Value: uint64(targetID)},
			marshal.PyNone{},
			marshal.PyList{},
			marshal.PyString{Value: guid},
			marshal.PyInt{Value: 1},
			marshal.PyInt{Value: 0},
			marshal.PyInt{Value: 0},
		)
	}

	for _, p := range ss.GetBubbleConnectedPlayersExcept(e.ID) {
		if p.Conn == nil {
			continue
		}
		if !ensureBallsKnown(p, e, target) {
			continue
		}
		if err := p.SendFXStamped(func(stamp uint32) error {
			return destiny.SendActionList(p.Conn, globalNodeID, p.AccountID, stamp, action)
		}); err != nil {
			log.Printf("[drone-ewar] FX to %d: %v", p.ID, err)
		}
	}
}
