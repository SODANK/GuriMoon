// Drone wiring: connects the drone.Manager (registry + AI tick) to the wire.
//
// Client contract:
//   - ship.Drop(ids): launch drones from the bay, returns new IDs
//   - ship.ScoopDrone(objectIDs): scoop a drone ball back into the bay
//   - entity.Cmd*(droneIDs, ...): drone commands; returns {droneID: (msgKey, dict)}
//     for failures only (menusvc.HandleMultipleCallError)
//   - SetState droneState rowset + OnDroneStateChange destiny actions drive
//     the client's drone window (michelle.stateByDroneID).
package main

import (
	"context"
	"fmt"
	"database/sql"
	"log"
	"math"
	"math/rand"
	"strconv"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/dogma"
	"gurimoon/drone"
	"gurimoon/marshal"
	"gurimoon/npc"
	"gurimoon/rates"
	"gurimoon/safego"
	"gurimoon/server"
)

// globalDroneMgr is the process-wide drone manager, set once in main().
var globalDroneMgr *drone.Manager

// droneScoopRange is the maximum distance (m) between ship and drone for a
// scoop. The client menu gates at 1500m; the server allows slack for motion.
const droneScoopRange = 2500.0

// typeDronesSkill is the Drones skill (one drone in space per level).
const typeDronesSkill = 3436

// attrMaxActiveDrones is dogma attributeID 352 (maxActiveDrones), read by the
// client's drone window header from the character godma item.
const attrMaxActiveDrones = 352

// initDroneManager builds the drone manager with all wire callbacks.
func initDroneManager(sqldb *sql.DB, npcMgr *npc.NPCManager) *drone.Manager {
	cb := drone.Callbacks{
		Move: func(rec *drone.Record, act drone.Action) {
			ss := rec.System
			e := rec.Entity
			droneID := marshal.PyInt{Value: uint64(e.ID)}
			ssfAction := marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyString{Value: "SetSpeedFraction"},
				marshal.PyTuple{Items: []marshal.PyRep{droneID, marshal.PyReal{Value: 1.0}}},
			}}
			var moveAction marshal.PyRep
			switch act.Type {
			case drone.ActFollow:
				moveAction = marshal.PyTuple{Items: []marshal.PyRep{
					marshal.PyString{Value: "FollowBall"},
					marshal.PyTuple{Items: []marshal.PyRep{
						droneID,
						marshal.PyInt{Value: uint64(act.TargetID)},
						marshal.PyReal{Value: act.Dist},
					}},
				}}
			case drone.ActOrbit:
				moveAction = marshal.PyTuple{Items: []marshal.PyRep{
					marshal.PyString{Value: "Orbit"},
					marshal.PyTuple{Items: []marshal.PyRep{
						droneID,
						marshal.PyInt{Value: uint64(act.TargetID)},
						marshal.PyReal{Value: act.Dist},
					}},
				}}
			case drone.ActGoto:
				pos := e.Pos()
				dir := act.Pos.Sub(pos).Normalize()
				far := pos.Add(dir.Scale(1e15))
				moveAction = marshal.PyTuple{Items: []marshal.PyRep{
					marshal.PyString{Value: "GotoDirection"},
					marshal.PyTuple{Items: []marshal.PyRep{
						droneID,
						marshal.PyReal{Value: far.X},
						marshal.PyReal{Value: far.Y},
						marshal.PyReal{Value: far.Z},
					}},
				}}
			case drone.ActStop:
				ssfAction = marshal.PyTuple{Items: []marshal.PyRep{
					marshal.PyString{Value: "SetSpeedFraction"},
					marshal.PyTuple{Items: []marshal.PyRep{droneID, marshal.PyReal{Value: 0.0}}},
				}}
				moveAction = marshal.PyTuple{Items: []marshal.PyRep{
					marshal.PyString{Value: "Stop"},
					marshal.PyTuple{Items: []marshal.PyRep{droneID}},
				}}
			}
			// Follow/orbit targets must be in the recipient's ballpark or the
			// command red-boxes; asteroid targets are always in SetState.
			var moveTarget *destiny.Entity
			if act.Type == drone.ActFollow || act.Type == drone.ActOrbit {
				moveTarget = ss.GetEntity(act.TargetID)
			}
			// Queued, not stamped per recipient: a drone pack re-deciding in one AI tick
			// must not mint one stamp per drone per witness. Last-wins per key.
			droneKey := strconv.FormatUint(uint64(e.ID), 10)
			for _, p := range ss.GetBubbleConnectedPlayersExcept(e.ID) {
				if !ensureBallsKnown(p, e, moveTarget) {
					continue
				}
				p.QueueAction("ssf:"+droneKey, e.ID, ssfAction)
				p.QueueAction("move:"+droneKey, e.ID, moveAction)
			}
		},

		State: func(rec *drone.Record, removed bool) {
			updateDroneMiningFX(rec, removed)
			broadcastDroneState(rec, removed)
		},

		StopFire: func(rec *drone.Record, targetID uint32) {
			if rec.Attrs.EwarKind != "" {
				droneEwarClearTarget(rec, targetID)
				return
			}
			stopNPCFireFX(rec.Entity, rec.System, targetID)
		},

		Ewar: func(rec *drone.Record, target *destiny.Entity, dist float64) {
			droneEwarCycle(rec, target, dist, sqldb, npcMgr)
		},

		Fire: func(rec *drone.Record, target *destiny.Entity, applied float64, destroyed bool, hitQuality float64) {
			droneFireBroadcast(rec, target, applied, destroyed, hitQuality, sqldb, npcMgr)
		},

		Killed: func(rec *drone.Record, killerID uint32) {
			ss := rec.System
			entityID := rec.Entity.ID
			// A dead jammer must not leave its victim jammed or damped: the ball is
			// about to vanish and nothing else clears the key.
			if rec.Attrs.EwarKind != "" {
				droneEwarClear(rec, ss.GetEntity(rec.Target()))
			}
			remaining := ss.RemoveEntity(entityID)
			for _, p := range remaining {
				if p.Conn == nil || !p.KnowsBall(entityID) {
					continue
				}
				// Urgent: same death class as the NPC path. The explosion is the removal,
				// and a deferred one detonates somewhere else.
				p.SendStampedUrgent(func(stamp uint32) error {
					return destiny.SendTerminalRemove(p.Conn, globalNodeID, p.AccountID, stamp, entityID)
				})
				p.ForgetBalls(entityID)
			}
			if sqldb != nil {
				go func() {
					defer safego.Recover("initDroneManager")
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if err := evedb.DeleteDroneItem(ctx, sqldb, rec.ItemID); err != nil {
						log.Printf("[drone] DeleteDroneItem %d: %v", rec.ItemID, err)
					}
				}()
			}
		},

		MineCycle: func(rec *drone.Record, asteroidID uint32) bool {
			return droneMineCycle(rec, asteroidID, sqldb)
		},

		AutoScoop: func(rec *drone.Record) bool {
			return droneAutoScoop(rec, sqldb)
		},

		NotifyDamage: func(targetID, attackerID uint32, applied float64) {
			npcMgr.NotifyDamage(targetID, attackerID, applied)
			npcMgr.SyncHPToDB(targetID)
		},
	}
	return drone.NewManager(cb, rates.PlayerDamage)
}

// broadcastDroneState sends OnDroneStateChange to the players in the drone's
// system who own or control it, or already hold a row for it. Not every player
// in the system (drone.Record.VisibleTo): the client raises OnDroneControlLost
// for any row that is not the session's own.
func broadcastDroneState(rec *drone.Record, removed bool) {
	controllerID, controllerOwnerID, activity, _, _ := rec.Snapshot()
	var act marshal.PyRep
	if removed {
		// ownerID=0 + controllerID=0 fails the client's ownership check: row deleted
		// + OnDroneControlLost.
		act = destiny.BuildDroneStateAction(rec.ItemID, 0, 0, marshal.PyNone{}, rec.TypeID, 0)
	} else {
		act = destiny.BuildDroneStateAction(rec.ItemID, rec.OwnerID, controllerID,
			marshal.PyInt{Value: uint64(activity)}, rec.TypeID, controllerOwnerID)
	}
	for _, p := range rec.System.GetAllPlayersInSystem() {
		if p.Conn == nil || !p.HasBallparkState() {
			continue
		}
		if !rec.VisibleTo(p.Slim.CharID, int64(p.ID)) {
			continue
		}
		if !removed {
			rec.MarkNotified(p.Slim.CharID)
		}
		p.SendFXStamped(func(stamp uint32) error {
			return destiny.SendActionList(p.Conn, globalNodeID, p.AccountID, stamp, act)
		})
	}
}

// droneFireBroadcast mirrors the NPC combat broadcast: repeating OnSpecialFX
// burst + damage state + combat messages + destroyed routing.
func droneFireBroadcast(rec *drone.Record, target *destiny.Entity, applied float64,
	destroyed bool, hitQuality float64, sqldb *sql.DB, npcMgr *npc.NPCManager) {

	ss := rec.System
	e := rec.Entity
	_, _, _, masterID, _ := rec.Snapshot()
	if target.IsDestroyed() && !destroyed {
		return
	}

	// CombatDrone.FitHardpoints stores its turret as modules[self.id], so the
	// FX moduleID must equal the drone's own ball ID.
	modID := int64(e.ID)
	durationMs := rec.Attrs.CycleMs
	if durationMs <= 0 {
		durationMs = 4000
	}

	needFX := true
	for _, ae := range e.GetActiveEffects() {
		if ae.ModuleID != modID {
			continue
		}
		if ae.TargetID == target.ID {
			needFX = false
		} else {
			stopNPCFireFX(e, ss, ae.TargetID)
		}
		break
	}

	var fxAction marshal.PyRep
	if needFX {
		ae := destiny.ActiveEffect{
			EntityID:    e.ID,
			ModuleID:    modID,
			ModuleType:  rec.TypeID,
			TargetID:    target.ID,
			OtherType:   0,
			GUID:        dogma.FXGUIDLaser,
			IsOffensive: true,
			DurationMs:  durationMs,
			StartTime:   server.Win32TimeNow(),
		}
		fxAction = destiny.BuildEffectAction(ae)
		e.SetActiveEffect(modID, ae)
	}

	players := ss.GetBubbleConnectedPlayersExcept(e.ID)

	// A player-owned structure: credit, aggression memory and CONCORD go to
	// the controlling pilot, exactly as for a player victim.
	if ss.IsStructureEntity(target.ID) {
		master := ss.GetEntity(masterID)
		if master == nil || !master.IsPlayer {
			master = nil
		}
		structureDamaged(ss, target, master, rec.TypeID, applied, destroyed, sqldb)
	}
	// Red-box the victim and deliver combat messages.
	if target.IsPlayer {
		target.AddAggressor(e.ID, time.Now().Unix())
		// Drone aggression is the controlling pilot's crime.
		if master := ss.GetEntity(masterID); master != nil && master.IsPlayer {
			recordPlayerAggression(master, target, ss)
			maybeTriggerConcordResponse(master, target, ss, sqldb, npcMgr)
		}
	}
	dmgID := hitQualityID(hitQuality)
	if target.IsPlayer && target.Conn != nil &&
		target.BubbleID >= 0 && target.BubbleID == e.BubbleID &&
		!target.IsInWarp() && !target.IsUntargetable() {
		sendCombatMessage(target.Conn, int64(target.AccountID), attackMsgTaken[dmgID], marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "source"}, Value: marshal.PyInt{Value: uint64(e.ID)}},
			{Key: marshal.PyString{Value: "weapon"}, Value: marshal.PyInt{Value: uint64(rec.TypeID)}},
			{Key: marshal.PyString{Value: "target"}, Value: marshal.PyInt{Value: uint64(target.ID)}},
			{Key: marshal.PyString{Value: "damage"}, Value: marshal.PyReal{Value: applied}},
		}})
	}
	// "Your drone hits..." feedback to the controlling pilot.
	if master := ss.GetEntity(masterID); master != nil && master.Conn != nil &&
		!master.IsInWarp() {
		sendCombatMessage(master.Conn, int64(master.AccountID), attackMsgGiven[dmgID], marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "weapon"}, Value: marshal.PyInt{Value: uint64(rec.TypeID)}},
			{Key: marshal.PyString{Value: "target"}, Value: marshal.PyInt{Value: uint64(target.ID)}},
			{Key: marshal.PyString{Value: "damage"}, Value: marshal.PyReal{Value: applied}},
		}})
	}

	if applied == 0 {
		if fxAction == nil {
			return
		}
		for _, p := range players {
			if p.IsInWarp() || p.IsUntargetable() {
				continue
			}
			if !ensureBallsKnown(p, e, target) {
				continue
			}
			p.SendFXStamped(func(stamp uint32) error {
				return destiny.SendActionList(p.Conn, globalNodeID, p.AccountID, stamp, fxAction)
			})
		}
		return
	}

	dmgState := target.DamageStateFrac()
	dmgState.Timestamp = server.Win32TimeNow()
	dmgAction := destiny.BuildSimpleAction("OnDamageStateChange",
		marshal.PyInt{Value: uint64(target.ID)},
		destiny.EncodeDamageState(dmgState),
	)
	for _, p := range players {
		if p.IsInWarp() || p.IsUntargetable() {
			continue
		}
		if !ensureBallsKnown(p, e, target) {
			continue
		}
		// Same split as the NPC fire path: per-burst FX stays direct, the damage
		// state is queued (absolute, last-wins per ball) and drains with the tick.
		if fxAction != nil {
			p.SendFXStamped(func(stamp uint32) error {
				return destiny.SendActionList(p.Conn, globalNodeID, p.AccountID, stamp, fxAction)
			})
		}
		p.QueueFXAction("dmg:"+strconv.FormatUint(uint64(target.ID), 10), target.ID, dmgAction)
	}
	sendPlayerHPUpdate(target)

	if destroyed {
		// Credit the kill to the controlling player where possible.
		killerID := masterID
		if killerID == 0 {
			killerID = e.ID
		}
		if target.IsPlayer && target.Conn != nil && target.PlayerState != nil {
			queuePlayerDeath(target.PlayerState.(*destinyConn), target.Slim.TypeID == 670)
		} else if globalDroneMgr != nil && globalDroneMgr.HandleDestroyed(target.ID, killerID) {
			// drone-on-drone kill handled
		} else if !npcMgr.Kill(target.ID, killerID) {
			npcDestroyEntity(ss, target, killerID)
		}
	}
}

// updateDroneMiningFX keeps the drone's effects.Mining beam in sync with its
// activity: started when the drone settles into StateMining, stopped on any
// transition away. The client's droneMining.py drives the beam off the drone
// ball (spaceObject.MiningDrone) toward the asteroid model.
func updateDroneMiningFX(rec *drone.Record, removed bool) {
	e := rec.Entity
	ss := rec.System
	if e == nil || ss == nil {
		return
	}
	_, _, activity, _, dead := rec.Snapshot()
	mining := !removed && !dead && activity == drone.StateMining
	modID := int64(e.ID)
	var current *destiny.ActiveEffect
	for _, ae := range e.GetActiveEffects() {
		if ae.ModuleID == modID && ae.GUID == dogma.FXGUIDMining {
			aeCopy := ae
			current = &aeCopy
			break
		}
	}
	switch {
	case mining && current == nil:
		targetID := rec.Target()
		if targetID == 0 {
			return
		}
		durationMs := rec.Attrs.CycleMs
		if durationMs <= 0 {
			durationMs = 60000
		}
		ae := destiny.ActiveEffect{
			EntityID:   e.ID,
			ModuleID:   modID,
			ModuleType: rec.TypeID,
			TargetID:   targetID,
			GUID:       dogma.FXGUIDMining,
			DurationMs: durationMs,
			StartTime:  server.Win32TimeNow(),
		}
		e.SetActiveEffect(modID, ae)
		fxAction := destiny.BuildEffectAction(ae)
		for _, p := range ss.GetBubbleConnectedPlayersExcept(e.ID) {
			if p.Conn == nil || !p.KnowsBall(e.ID) {
				continue
			}
			p.SendFXStamped(func(stamp uint32) error {
				return destiny.SendActionList(p.Conn, globalNodeID, p.AccountID, stamp, fxAction)
			})
		}
	case !mining && current != nil:
		stopNPCFireFX(e, ss, current.TargetID)
	}
}

// droneMineCycle runs one mining-drone cycle: pulls ore from the asteroid into
// the controlling ship's cargo hold. Returns false when the rock is depleted
// or the hold is full (drone returns home).
func droneMineCycle(rec *drone.Record, asteroidID uint32, sqldb *sql.DB) bool {
	ss := rec.System
	controllerID, controllerOwnerID, _, masterID, _ := rec.Snapshot()
	if sqldb == nil || controllerID == 0 {
		return false
	}
	oreTypeSrc := ss.GetAsteroidTypeID(asteroidID)
	if oreTypeSrc == 0 {
		return false
	}
	// Mining drones cannot chew ice or Mercoxit: send them home.
	if astGroup := ss.GetAsteroidGroupID(asteroidID); astGroup == destiny.GroupIce || astGroup == destiny.GroupMercoxit {
		return false
	}
	oreType := destiny.MinedOreType(oreTypeSrc)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	oreVolume, err := evedb.GetTypeVolume(ctx, sqldb, oreType)
	if err != nil || oreVolume <= 0 {
		oreVolume = 0.1
	}
	master := ss.GetEntity(masterID)
	yieldM3 := rec.Attrs.MiningAmount * rates.MiningYield
	// Drone Coordination link in the controlling pilot's gang.
	if master != nil {
		yieldM3 *= master.GangBoost().Mult(dogma.GSMiningDroneYield)
	}
	yieldUnits := int64(yieldM3 / oreVolume)
	if yieldUnits <= 0 {
		yieldUnits = 1
	}

	// Cap the yield to free cargo space on the controlling ship.
	if master != nil && master.Slim.TypeID != 0 {
		if cargoCap, err := evedb.GetShipCargoCapacityFitted(ctx, sqldb, master.Slim.TypeID, controllerID); err == nil && cargoCap > 0 {
			if usedVol, err := evedb.GetCargoUsedVolume(ctx, sqldb, controllerOwnerID, controllerID); err == nil {
				freeM3 := cargoCap - usedVol
				if freeM3 <= 0 {
					return false
				}
				if maxUnits := int64(freeM3 / oreVolume); maxUnits > 0 && yieldUnits > maxUnits {
					yieldUnits = maxUnits
				}
			}
		}
	}

	actual := ss.DecrementAsteroidQty(asteroidID, yieldUnits)
	if newRadius, shrank := ss.UpdateAsteroidRadius(asteroidID); shrank {
		broadcastAsteroidRadius(ss, asteroidID, newRadius)
	}
	if actual <= 0 {
		ss.MarkAsteroidDepleted(asteroidID)
		removeAsteroidFromClients(ss, asteroidID)
		return false
	}
	depleted := ss.GetAsteroidQty(asteroidID) <= 0
	if depleted {
		ss.MarkAsteroidDepleted(asteroidID)
		defer removeAsteroidFromClients(ss, asteroidID)
	}

	// Stack onto an existing cargo item or create a new one.
	var itemID int64
	var changeDict marshal.PyDict
	existID, existQty, findErr := evedb.FindCargoItemByType(ctx, sqldb, oreType, controllerOwnerID, controllerID)
	if findErr == nil && existID != 0 {
		if err := evedb.UpdateCargoItemQty(ctx, sqldb, existID, existQty+actual); err != nil {
			log.Printf("[drone] mine UpdateCargoItemQty: %v", err)
			return !depleted
		}
		itemID = existID
		changeDict = marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(existQty)}},
		}}
	} else {
		newID, err := evedb.CreateCargoItem(ctx, sqldb, oreType, controllerOwnerID, controllerID, actual)
		if err != nil {
			log.Printf("[drone] mine CreateCargoItem: %v", err)
			return !depleted
		}
		itemID = newID
		changeDict = marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: 0}},
		}}
	}
	if master != nil && master.Conn != nil {
		if er, err := evedb.GetEntityRow(ctx, sqldb, itemID); err == nil && er != nil {
			_ = server.SendNotification(master.Conn, int64(master.AccountID), "OnItemChange", "charid",
				marshal.PyTuple{Items: []marshal.PyRep{evedb.BuildEntityRow(er), changeDict}})
		}
	}
	if evedb.WorldEventsWanted() {
		// The ore is in the controller's hold: tell modules (db/worldevents.go).
		ev := evedb.OreMinedEvent{At: time.Now(), CharID: controllerOwnerID, TypeID: oreType,
			Units: actual, Volume: float64(actual) * oreVolume, SolarSystemID: ss.ID, Drone: true}
		if master != nil {
			ev.CorpID = master.Slim.CorpID
		}
		if er, err := evedb.GetEntityRow(ctx, sqldb, itemID); err == nil && er != nil {
			ev.GroupID = er.GroupID
		}
		evedb.EmitWorldEvent(ev)
	}
	log.Printf("[drone] %s mined %d units of type %d into ship %d", rec.TypeName, actual, oreType, controllerID)
	return !depleted
}

// droneAutoScoop finishes a Return To Drone Bay flight: item back to the bay,
// ball removed, state row deleted. Returns false if the scoop failed.
func droneAutoScoop(rec *drone.Record, sqldb *sql.DB) bool {
	controllerID, _, _, masterID, _ := rec.Snapshot()
	if sqldb == nil || controllerID == 0 {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := evedb.ScoopDroneToBay(ctx, sqldb, rec.ItemID, controllerID); err != nil {
		log.Printf("[drone] auto-scoop %d: %v", rec.ItemID, err)
		return false
	}
	globalDroneMgr.Remove(rec.ItemID)
	removeDroneBall(rec)
	broadcastDroneState(rec, true)

	// Refresh the controller's inventory view.
	ss := rec.System
	if master := ss.GetEntity(masterID); master != nil && master.Conn != nil {
		if er, err := evedb.GetEntityRow(ctx, sqldb, rec.ItemID); err == nil && er != nil {
			changeDict := marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: uint64(rec.System.ID)}},
				{Key: marshal.PyInt{Value: 4}, Value: marshal.PyInt{Value: 0}},
			}}
			_ = server.SendNotification(master.Conn, int64(master.AccountID), "OnItemChange", "charid",
				marshal.PyTuple{Items: []marshal.PyRep{evedb.BuildEntityRow(er), changeDict}})
		}
	}
	log.Printf("[drone] %s (item=%d) scooped to bay of ship %d", rec.TypeName, rec.ItemID, controllerID)
	return true
}

// removeDroneBall removes the drone entity from the system and RemoveBalls it
// from every client holding the ball (no explosion: scoop, not death).
func removeDroneBall(rec *drone.Record) {
	ss := rec.System
	entityID := rec.Entity.ID
	// Scooping a running jammer/web must lift what it was holding: the ball is
	// leaving and no cycle will ever clear it.
	if rec.Attrs.EwarKind != "" {
		droneEwarClear(rec, ss.GetEntity(rec.Target()))
	}
	remaining := ss.RemoveEntity(entityID)
	// Queued, not stamped: the drone is another ball, and a wing scooped in one
	// second shares the next flush (docs/destiny-stamps.md#budget-and-triage).
	key := "rm:" + strconv.FormatUint(uint64(entityID), 10)
	for _, p := range remaining {
		if p.Conn == nil || !p.KnowsBall(entityID) {
			continue
		}
		p.QueueRemoveBall(key, entityID)
	}
}

// loadDroneAttributes builds combat/mining attributes for a drone type.
func loadDroneAttributes(ctx context.Context, sqldb *sql.DB, typeID int64) drone.Attributes {
	a := drone.Attributes{
		MaxVelocity: 500,
		OrbitRange:  500,
		CycleMs:     4000,
	}
	attrs, err := evedb.GetTypeAttributes(ctx, sqldb, typeID)
	if err != nil || attrs == nil {
		return a
	}
	if v := attrs[37]; v > 0 {
		a.MaxVelocity = v
	}
	a.Optimal = attrs[54]
	a.Falloff = attrs[158]
	a.TrackingSpeed = attrs[160]
	a.DmgEM = attrs[114]
	a.DmgExp = attrs[116]
	a.DmgKin = attrs[117]
	a.DmgTherm = attrs[118]
	a.DmgMult = attrs[64]
	if v := attrs[51]; v > 0 {
		a.CycleMs = int64(v)
	}
	a.MiningAmount = attrs[77]
	if v := attrs[416]; v > 0 { // entityFlyRange
		a.OrbitRange = v
	}
	// EWAR drones fight with an effect instead of a gun; the whole attribute map
	// rides along because the strengths it needs are the module attributes.
	if kind := droneEwarKind(ctx, sqldb, typeID); kind != "" {
		a.EwarKind = kind
		a.EwarAttrs = attrs
		if v := attrs[dogma.AttrDuration]; v > 0 {
			a.CycleMs = int64(v)
		}
	}
	return a
}

// droneMaxActive returns how many drones charID may control at once
// (Drones skill: one per level).
func droneMaxActive(ctx context.Context, sqldb *sql.DB, charID int64) int {
	lvl, err := evedb.GetCharSkillLevel(ctx, sqldb, charID, typeDronesSkill)
	if err != nil {
		log.Printf("[drone] GetCharSkillLevel: %v", err)
		return 0
	}
	return int(lvl)
}

// launchDronesForPlayer implements ship.Drop for drone-bay items. ids may
// repeat an itemID once per unit (the client expands stacks). Returns the
// itemIDs of the drones now in space.
func launchDronesForPlayer(ds *destinyConn, charID int64, ids []int64, sqldb *sql.DB) []int64 {
	var launched []int64
	if ds.solarSystem == nil || ds.entity == nil || sqldb == nil || globalDroneMgr == nil {
		return launched
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	maxActive := droneMaxActive(ctx, sqldb, charID)
	inSpace := globalDroneMgr.CountControlled(ds.shipID)
	for _, itemID := range ids {
		if inSpace >= maxActive {
			log.Printf("[drone] launch cap reached (%d/%d) for char %d", inSpace, maxActive, charID)
			// The launch answers with what did leave the bay; the rest just
			// stay put, so say why in the client's own words.
			item := "drone"
			if er, _ := evedb.GetEntityRow(ctx, sqldb, itemID); er != nil {
				item = typeNameOrID(ctx, sqldb, er.TypeID)
			}
			if maxActive <= 0 {
				pushRemoteMessage(ds.entity.Conn, int64(ds.entity.AccountID), "NoDroneManagementAbilities",
					map[string]marshal.PyRep{"item": marshal.PyString{Value: item}})
			} else {
				pushRemoteMessage(ds.entity.Conn, int64(ds.entity.AccountID), "NoDroneManagementAbilitiesLeft",
					map[string]marshal.PyRep{
						"item":  marshal.PyString{Value: item},
						"limit": marshal.PyInt{Value: uint64(maxActive)},
					})
			}
			break
		}
		bay, err := evedb.GetDroneBayItem(ctx, sqldb, itemID, ds.shipID)
		if err != nil || bay == nil {
			log.Printf("[drone] Drop: item %d not in drone bay of ship %d (%v)", itemID, ds.shipID, err)
			continue
		}
		shipPos := ds.entity.Pos()
		pos := destiny.Vec3{
			X: shipPos.X + 300 + rand.Float64()*400,
			Y: shipPos.Y + (rand.Float64()-0.5)*200,
			Z: shipPos.Z + 300 + rand.Float64()*400,
		}
		newID, err := evedb.LaunchDroneToSpace(ctx, sqldb, itemID, charID, ds.shipID, ds.sysID, pos.X, pos.Y, pos.Z)
		if err != nil {
			log.Printf("[drone] LaunchDroneToSpace item %d: %v", itemID, err)
			continue
		}
		rec := spawnDroneEntity(ds, charID, newID, bay, pos, sqldb)
		if rec == nil {
			continue
		}
		launched = append(launched, newID)
		inSpace++

		// Inventory updates for the launcher: launched item left the bay;
		// if the stack was split the remaining stack shrank by one.
		if er, err := evedb.GetEntityRow(ctx, sqldb, newID); err == nil && er != nil {
			changeDict := marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: uint64(ds.shipID)}},
				{Key: marshal.PyInt{Value: 4}, Value: marshal.PyInt{Value: 87}},
			}}
			_ = server.SendNotification(ds.entity.Conn, int64(ds.entity.AccountID), "OnItemChange", "charid",
				marshal.PyTuple{Items: []marshal.PyRep{evedb.BuildEntityRow(er), changeDict}})
		}
		if newID != itemID {
			if er, err := evedb.GetEntityRow(ctx, sqldb, itemID); err == nil && er != nil {
				changeDict := marshal.PyDict{Items: []marshal.DictEntry{
					{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(er.Quantity + 1)}},
				}}
				_ = server.SendNotification(ds.entity.Conn, int64(ds.entity.AccountID), "OnItemChange", "charid",
					marshal.PyTuple{Items: []marshal.PyRep{evedb.BuildEntityRow(er), changeDict}})
			}
		}
	}
	return launched
}

// spawnDroneEntity creates the destiny entity + ball for a launched drone and
// registers it with the drone manager.
func spawnDroneEntity(ds *destinyConn, charID, itemID int64, bay *evedb.DroneBayItem,
	pos destiny.Vec3, sqldb *sql.DB) *drone.Record {

	ss := ds.solarSystem
	entityID := uint32(itemID)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	attrs := loadDroneAttributes(ctx, sqldb, bay.TypeID)
	ehp, _ := evedb.GetEntityHPByType(ctx, sqldb, bay.TypeID)
	typeAttrs, _ := evedb.GetTypeAttributes(ctx, sqldb, bay.TypeID)
	cancel()

	mass := bay.Mass
	if mass <= 0 {
		mass = 2500
	}
	radius := bay.Radius
	if radius <= 0 {
		radius = 25
	}
	agility := 1.0
	if typeAttrs != nil {
		if v := typeAttrs[70]; v > 0 {
			agility = v
		}
	}

	dm := destiny.New(entityID, mass, agility, attrs.MaxVelocity, radius)
	dm.Pos = pos
	dm.Stamp = 1
	dm.TargetLookup = func(targetID uint32) (destiny.Vec3, bool) {
		if t := ss.GetEntity(targetID); t != nil {
			return t.Pos(), true
		}
		if p, ok := ss.GetAsteroidPosition(targetID); ok {
			return p, true
		}
		return destiny.Vec3{}, false
	}

	entity := &destiny.Entity{
		ID:       entityID,
		Manager:  dm,
		CharName: bay.TypeName,
		BubbleID: -1,
		IsPlayer: false,
		Slim: destiny.SlimItem{
			BallID:     itemID,
			ItemID:     itemID,
			TypeID:     bay.TypeID,
			GroupID:    bay.GroupID,
			CategoryID: 18, // categoryDrone
			OwnerID:    charID,
			CorpID:     ds.corpID,
		},
		Damage: destiny.DamageState{Shield: 1.0, Tau: 1e9, Armor: 1.0, Structure: 1.0},
	}
	if ehp != nil {
		entity.HP = destiny.HPState{
			MaxShield: ehp.ShieldHP, CurShield: ehp.ShieldHP,
			MaxArmor: ehp.ArmorHP, CurArmor: ehp.ArmorHP,
			MaxStructure: ehp.StructureHP, CurStructure: ehp.StructureHP,
			ShieldTau: ehp.ShieldTau,
		}
		entity.Resists = [3][4]float64{ehp.ShieldRes, ehp.ArmorRes, ehp.HullRes}
	} else {
		entity.HP = destiny.HPState{MaxStructure: 50, CurStructure: 50}
		entity.Resists = [3][4]float64{{1, 1, 1, 1}, {1, 1, 1, 1}, {1, 1, 1, 1}}
	}
	dm.SetEntity(entity)
	ss.AddEntity(entity)

	// Deliver the new ball to everyone on grid (including the launcher).
	bd := entity.BallDesc()
	bd.CorpID = uint32(ds.corpID)
	bd.Name = bay.TypeName
	dmg := map[uint32]destiny.DamageState{entityID: entity.DamageStateFrac()}
	broadcastAddBalls(ds, []destiny.BallDesc{bd}, []destiny.SlimItem{entity.Slim}, dmg)

	rec := &drone.Record{
		Entity:            entity,
		System:            ss,
		ItemID:            itemID,
		TypeID:            bay.TypeID,
		GroupID:           bay.GroupID,
		TypeName:          bay.TypeName,
		OwnerID:           charID,
		ControllerID:      ds.shipID,
		ControllerOwnerID: charID,
		MasterID:          ds.entity.ID,
		Activity:          drone.StateIdle,
		Attrs:             attrs,
	}
	globalDroneMgr.Register(rec)
	return rec
}

// scoopDronesForPlayer implements ship.ScoopDrone: pull owned (or abandoned)
// drones within scoop range back into the drone bay.
func scoopDronesForPlayer(ds *destinyConn, charID int64, ids []int64, sqldb *sql.DB) {
	if ds.solarSystem == nil || ds.entity == nil || sqldb == nil || globalDroneMgr == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	shipPos := ds.entity.Pos()
	for _, itemID := range ids {
		rec := globalDroneMgr.Get(itemID)
		if rec == nil {
			continue
		}
		_, _, _, masterID, _ := rec.Snapshot()
		// Owners can always scoop their own drones; anyone can scoop an
		// abandoned (uncontrolled) drone.
		if rec.OwnerID != charID && masterID != 0 {
			log.Printf("[drone] scoop denied: item %d owned by %d, requested by %d", itemID, rec.OwnerID, charID)
			continue
		}
		dist := rec.Entity.Pos().Sub(shipPos).Len()
		if dist > droneScoopRange {
			log.Printf("[drone] scoop out of range: item %d at %.0fm", itemID, dist)
			continue
		}
		if err := evedb.ScoopDroneToBay(ctx, sqldb, itemID, ds.shipID); err != nil {
			log.Printf("[drone] ScoopDroneToBay %d: %v", itemID, err)
			continue
		}
		globalDroneMgr.Remove(itemID)
		removeDroneBall(rec)
		broadcastDroneState(rec, true)
		if er, err := evedb.GetEntityRow(ctx, sqldb, itemID); err == nil && er != nil {
			changeDict := marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: uint64(ds.sysID)}},
				{Key: marshal.PyInt{Value: 4}, Value: marshal.PyInt{Value: 0}},
			}}
			_ = server.SendNotification(ds.entity.Conn, int64(ds.entity.AccountID), "OnItemChange", "charid",
				marshal.PyTuple{Items: []marshal.PyRep{evedb.BuildEntityRow(er), changeDict}})
		}
		log.Printf("[drone] item %d scooped by char %d", itemID, charID)
	}
}

// ---- entity service (drone command) dispatch ----

// parseDroneIDs extracts a list of drone itemIDs from a call argument that
// may be a PyList/PyTuple of ints or a single int.
func parseDroneIDs(rep marshal.PyRep) []int64 {
	var out []int64
	switch v := rep.(type) {
	case marshal.PyList:
		for _, it := range v.Items {
			if iv, ok := it.(marshal.PyInt); ok {
				out = append(out, int64(iv.Value))
			}
		}
	case marshal.PyTuple:
		for _, it := range v.Items {
			if iv, ok := it.(marshal.PyInt); ok {
				out = append(out, int64(iv.Value))
			}
		}
	case marshal.PyInt:
		out = append(out, int64(v.Value))
	}
	return out
}

// droneFailuresToPy converts a command failure map into the client's
// {droneID: (msgKey, msgDict)} response shape.
func droneFailuresToPy(fails map[int64]drone.CmdFailure) marshal.PyRep {
	if len(fails) == 0 {
		return marshal.PyDict{}
	}
	entries := make([]marshal.DictEntry, 0, len(fails))
	for id, f := range fails {
		entries = append(entries, marshal.DictEntry{
			Key: marshal.PyInt{Value: uint64(id)},
			Value: marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyString{Value: f.MsgKey},
				marshal.PyDict{Items: []marshal.DictEntry{
					{Key: marshal.PyString{Value: "info"}, Value: marshal.PyString{Value: f.Info}},
				}},
			}},
		})
	}
	return marshal.PyDict{Items: entries}
}

// isDroneCommandMethod reports whether an entity-service method is a drone
// command this server implements.
func isDroneCommandMethod(method string) bool {
	switch method {
	case "CmdEngage", "CmdMine", "CmdMineRepeatedly", "CmdReturnHome",
		"CmdReturnBay", "CmdDelegateControl", "CmdRelinquishControl", "CmdUnanchor":
		return true
	}
	return false
}

// handleDroneCommand executes one entity.Cmd* call and returns the failure
// dict result rep.
func handleDroneCommand(method string, args marshal.PyTuple, ds *destinyConn, charID int64) marshal.PyRep {
	if globalDroneMgr == nil || ds.solarSystem == nil || ds.entity == nil {
		return marshal.PyDict{}
	}
	var droneIDs []int64
	if len(args.Items) >= 1 {
		droneIDs = parseDroneIDs(args.Items[0])
	}
	var targetID uint32
	if len(args.Items) >= 2 {
		if iv, ok := args.Items[1].(marshal.PyInt); ok {
			targetID = uint32(iv.Value)
		}
	}
	log.Printf("[drone] %s drones=%v target=%d char=%d", method, droneIDs, targetID, charID)

	var fails map[int64]drone.CmdFailure
	switch method {
	case "CmdEngage":
		fails = globalDroneMgr.Engage(droneIDs, ds.shipID, targetID)
	case "CmdMine":
		fails = globalDroneMgr.Mine(droneIDs, ds.shipID, targetID, false)
	case "CmdMineRepeatedly":
		fails = globalDroneMgr.Mine(droneIDs, ds.shipID, targetID, true)
	case "CmdReturnHome":
		fails = globalDroneMgr.ReturnHome(droneIDs, ds.shipID)
	case "CmdReturnBay":
		fails = globalDroneMgr.ReturnBay(droneIDs, ds.shipID)
	case "CmdRelinquishControl":
		fails = globalDroneMgr.RelinquishControl(droneIDs, charID, ds.shipID, ds.entity.ID)
	case "CmdDelegateControl":
		// args: (droneIDs, controllerCharID)
		var newCtrlCharID int64
		if len(args.Items) >= 2 {
			if iv, ok := args.Items[1].(marshal.PyInt); ok {
				newCtrlCharID = int64(iv.Value)
			}
		}
		target := ds.solarSystem.GetPlayerByCharID(newCtrlCharID)
		if target == nil {
			fails = make(map[int64]drone.CmdFailure)
			for _, id := range droneIDs {
				fails[id] = drone.CmdFailure{MsgKey: "CustomInfo", Info: "That pilot is not on grid."}
			}
			break
		}
		fails = globalDroneMgr.DelegateControl(droneIDs, ds.shipID,
			target.Slim.ItemID, newCtrlCharID, target.ID)
	case "CmdUnanchor":
		fails = make(map[int64]drone.CmdFailure)
		for _, id := range droneIDs {
			fails[id] = drone.CmdFailure{MsgKey: "CustomInfo", Info: "Unanchoring drones are not supported yet."}
		}
	}
	return droneFailuresToPy(fails)
}

// fighterWarpFollow launches warp-follow for every fighter drone the warping
// ship controls. Called from the ship warp goroutine right after the ship's
// own dm.WarpTo (tunnel start): RMR carrier fighters follow their controller
// through warp; every other drone group stays behind on the departure grid.
func fighterWarpFollow(ds *destinyConn, celestialPos, landing, warpDir destiny.Vec3, exitDist, tunnelSecs float64) {
	if globalDroneMgr == nil || ds.solarSystem == nil || ds.entity == nil {
		return
	}
	fighters := globalDroneMgr.FightersControlledBy(ds.shipID)
	if len(fighters) == 0 {
		return
	}
	controller := ds.entity
	for i, rec := range fighters {
		if !globalDroneMgr.BeginWarp(rec) {
			continue
		}
		// Scatter the wing around the ship's landing point so the fighters
		// don't all decelerate onto the same spot.
		ang := float64(i) * 2.4
		jitter := destiny.Vec3{
			X: math.Cos(ang) * 2500,
			Y: float64(i%3-1) * 800,
			Z: math.Sin(ang) * 2500,
		}
		go fighterWarpRun(rec, controller, celestialPos.Add(jitter), landing.Add(jitter),
			warpDir, exitDist, tunnelSecs)
	}
}

// fighterWarpRun executes one fighter's warp: warp-out broadcast, departure
// bubble removal, server warp sim, arrival bubble insertion in WARP mode, and
// the final park. Mirrors the ship warp sequence in startPlayerWarp, minus
// the client-egoball phases a drone doesn't have.
func fighterWarpRun(rec *drone.Record, controller *destiny.Entity, dest, landing, warpDir destiny.Vec3, exitDist, tunnelSecs float64) {
	defer safego.Recover("fighterWarpRun")
	defer globalDroneMgr.EndWarp(rec)
	e := rec.Entity
	dm := e.Manager
	ss := rec.System
	if dm == nil || ss == nil {
		return
	}
	effectStamp := e.NextStamp()

	// Warp-out: the controller plus the players still on the departure grid see
	// the fighter enter warp. exitRange 0 lands the ball on the sent XYZ.
	// Queued per observer under one key each (BuildWarpToActions), so the
	// fighter's warp costs no stamp of its own (docs/destiny-stamps.md#budget-and-triage).
	mates := ss.RemoveFromBubble(e.ID)
	notify := append([]*destiny.Entity{controller}, mates...)
	idKey := strconv.FormatUint(uint64(e.ID), 10)
	warpActions := destiny.BuildWarpToActions(e.ID, landing.X, landing.Y, landing.Z,
		0, destiny.WarpSpeedAUperSec)
	warpKeys := []string{"ssf:" + idKey, "warp:" + idKey, "warpfx:" + idKey}
	for _, p := range notify {
		if p == nil || p.Conn == nil || !p.KnowsBall(e.ID) {
			continue
		}
		for i, a := range warpActions {
			p.QueueAction(warpKeys[i], e.ID, a)
		}
	}
	dm.WarpTo(dest, effectStamp, e.ID, exitDist)
	log.Printf("[drone] fighter %s (item=%d) warp-following ship %d", rec.TypeName, rec.ItemID, rec.ControllerID)

	time.Sleep(time.Duration(tunnelSecs * float64(time.Second)))

	// Departure cleanup: players left behind drop the ball after the warp-out
	// animation. The controller keeps it; their landing batch reconciles it.
	for _, mate := range mates {
		if mate.Conn == nil || !mate.KnowsBall(e.ID) {
			continue
		}
		mate.QueueRemoveBall("rm:"+idKey, e.ID)
	}

	// Arrival: join the destination bubble while the ball is still in WARP
	// mode so observers see the fighter decelerate in, not pop in parked.
	destMates, _ := ss.AddToBubble(e.ID, landing)
	// The warp handover (destiny.WarpHandoffBallDesc). A tail this path cannot
	// fill is a bug in the WarpTo above; fall back to publishing the fighter at rest.
	bd, warpErr := destiny.WarpHandoffBallDesc(dm.BallDesc())
	if warpErr != nil {
		log.Printf("fighter %d warp arrive: %v — publishing at rest instead", e.ID, warpErr)
		bd = destiny.BallDescForObserver(dm.BallDesc(), nil)
	}
	var dmgState destiny.DamageState
	if e.HasHP() {
		dmgState = e.DamageStateFrac()
		dmgState.Timestamp = server.Win32TimeNow()
	} else {
		dmgState = e.Damage
	}
	for _, mate := range destMates {
		if mate.Conn == nil || !mate.HasBallparkState() {
			continue
		}
		if len(mate.ClaimUnknownBalls(e.ID)) == 0 {
			continue
		}
		mate.QueueAddBalls("add:"+idKey, bd, e.Slim, dmgState)
	}

	// Exit buffer: let the client decel curves finish before parking the ball;
	// EndWarp (deferred) then hands the fighter back to the AI tick.
	time.Sleep(5 * time.Second)
	dm.SetPosition(landing)
	dm.Stop()
}

// pyIntList wraps a slice of int64 as a PyList of PyInts.
func pyIntList(ids []int64) marshal.PyRep {
	items := make([]marshal.PyRep, len(ids))
	for i, id := range ids {
		items[i] = marshal.PyInt{Value: uint64(id)}
	}
	return marshal.PyList{Items: items}
}

// droneRowsForSetState returns the droneState rows one pilot's SetState should
// carry (the drones in the system they own or control). Nil-safe.
func droneRowsForSetState(sysID, charID, shipID int64) []destiny.DroneStateRow {
	if globalDroneMgr == nil || sysID == 0 {
		return nil
	}
	return globalDroneMgr.RowsFor(sysID, charID, shipID)
}

// scoopDroneToCargo is ship.Scoop on a drone ball (menusvc draws Scoop To Cargo
// Hold for any drone). Same rules as the drone-bay scoop, plus the hold's free
// volume. handled=false means itemID is not a drone in space.
func scoopDroneToCargo(ds *destinyConn, charID, itemID int64, sqldb *sql.DB) (bool, error) {
	if globalDroneMgr == nil || ds == nil || ds.solarSystem == nil || ds.entity == nil || sqldb == nil {
		return false, nil
	}
	rec := globalDroneMgr.Get(itemID)
	if rec == nil {
		return false, nil
	}
	_, _, _, masterID, _ := rec.Snapshot()
	if rec.OwnerID != charID && masterID != 0 {
		return true, fmt.Errorf("that drone is controlled by someone else")
	}
	if d := rec.Entity.Pos().Sub(ds.entity.Pos()).Len(); d > droneScoopRange {
		return true, fmt.Errorf("you are too far away to scoop that drone")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	er, err := evedb.GetEntityRow(ctx, sqldb, itemID)
	if err != nil || er == nil {
		return true, fmt.Errorf("there is nothing here to scoop")
	}
	if shipER, _ := evedb.GetEntityRow(ctx, sqldb, ds.shipID); shipER != nil {
		cargoCap, _ := evedb.GetShipCargoCapacityFitted(ctx, sqldb, shipER.TypeID, ds.shipID)
		usedVol, _ := evedb.GetCargoUsedVolume(ctx, sqldb, charID, ds.shipID)
		need := evedb.ItemVolume(ctx, sqldb, itemID, er.TypeID) * float64(max(er.Quantity, 1))
		if free := cargoCap - usedVol; need > free {
			return true, fmt.Errorf("not enough cargo space: the drone needs %.0f m3 and your hold has %.0f m3 free", need, max(free, 0))
		}
	}
	if err := evedb.ScoopDroneToHold(ctx, sqldb, itemID, ds.shipID, charID); err != nil {
		return true, err
	}
	globalDroneMgr.Remove(itemID)
	removeDroneBall(rec)
	broadcastDroneState(rec, true)
	if newER, err := evedb.GetEntityRow(ctx, sqldb, itemID); err == nil && newER != nil {
		changeDict := marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyInt{Value: 2}, Value: marshal.PyInt{Value: uint64(er.OwnerID)}},
			{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: uint64(ds.sysID)}},
			{Key: marshal.PyInt{Value: 4}, Value: marshal.PyInt{Value: 0}},
		}}
		_ = server.SendNotification(ds.entity.Conn, int64(ds.entity.AccountID), "OnItemChange", "charid",
			marshal.PyTuple{Items: []marshal.PyRep{evedb.BuildEntityRow(newER), changeDict}})
	}
	log.Printf("[drone] item %d scooped to cargo by char %d", itemID, charID)
	return true, nil
}
