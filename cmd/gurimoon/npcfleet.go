package main

// Dispatched NPC fleets (CONCORD, faction police, faction navy): one wave of a
// type spawned around a point, and a silent removal of a whole fleet.

import (
	"context"
	"database/sql"
	"log"
	"math"
	"math/rand"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/marshal"
	"gurimoon/npc"
	"gurimoon/safego"
	"gurimoon/server"
)

// spawnNPCWave spawns count NPCs of typeID at random points rMin..rMax metres
// from center, defensive (blind until threat is seeded), owned by corpID and
// factionID. Balls are delivered by the caller with ensureBallsKnown, so a
// warp-in can be set first. Returns the new entity IDs.
func spawnNPCWave(ctx context.Context, ss *destiny.SolarSystem, sqldb *sql.DB, npcMgr *npc.NPCManager,
	typeID int64, count int, center destiny.Vec3, rMin, rMax float64, corpID, factionID int64) []uint32 {

	if count <= 0 || typeID <= 0 {
		return nil
	}
	stats, err := evedb.GetNPCStats(ctx, sqldb, typeID)
	if err != nil {
		log.Printf("[npcfleet] GetNPCStats typeID=%d: %v", typeID, err)
		return nil
	}
	var npcHP *npc.NPCHPInit
	if ehp, err := evedb.GetEntityHPByType(ctx, sqldb, typeID); err == nil {
		npcHP = &npc.NPCHPInit{
			ShieldHP: ehp.ShieldHP, ArmorHP: ehp.ArmorHP,
			StructureHP: ehp.StructureHP, ShieldTau: ehp.ShieldTau,
			ShieldRes: ehp.ShieldRes, ArmorRes: ehp.ArmorRes, HullRes: ehp.HullRes,
		}
	}
	weaponProfile, err := evedb.GetNPCWeaponProfile(ctx, sqldb, typeID)
	if err != nil {
		log.Printf("[npcfleet] GetNPCWeaponProfile typeID=%d: %v", typeID, err)
		weaponProfile = nil // spawn unarmed rather than not at all
	}

	var ids []uint32
	for i := 0; i < count; i++ {
		theta := rand.Float64() * 2 * math.Pi
		phi := rand.Float64() * math.Pi
		r := rMin + rand.Float64()*(rMax-rMin)
		pos := destiny.Vec3{
			X: center.X + r*math.Sin(phi)*math.Cos(theta),
			Y: center.Y + r*math.Sin(phi)*math.Sin(theta),
			Z: center.Z + r*math.Cos(phi),
		}
		entity := npcMgr.SpawnNPC(ss, typeID, stats.TypeName,
			pos, stats.Mass, stats.Agility, stats.MaxVelocity, stats.Radius,
			npc.StanceDefensive, npcHP, weaponProfile, corpID, false, 0, factionID, false)
		if entity == nil {
			continue
		}
		npc.SetGroupCategory(entity, stats.GroupID, stats.CategoryID)
		ids = append(ids, entity.ID)
	}
	return ids
}

// deliverNPCFleet sends the fleet's balls to every pilot on each ship's grid.
func deliverNPCFleet(ss *destiny.SolarSystem, ids []uint32) {
	for _, id := range ids {
		if e := ss.GetEntity(id); e != nil {
			for _, p := range ss.GetBubbleConnectedPlayersExcept(id) {
				ensureBallsKnown(p, e)
			}
		}
	}
}

// despawnNPCFleet removes a fleet silently (no explosion: they "warp off"):
// NPC manager, live-NPC rows, one RemoveBalls per witness, red boxes and any
// pilot's lock on them. Ships already dead or in a torn-down system are skipped.
func despawnNPCFleet(ss *destiny.SolarSystem, sqldb *sql.DB, npcMgr *npc.NPCManager, ids []uint32) {
	live := make([]uint32, 0, len(ids))
	for _, entityID := range ids {
		if ss.GetEntity(entityID) == nil {
			continue
		}
		npcMgr.Remove(entityID)
		live = append(live, entityID)

		if sqldb != nil {
			id := entityID
			go func() {
				defer safego.Recover("despawnNPCFleet")
				dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer dcancel()
				_ = evedb.DeleteLiveNPC(dctx, sqldb, id)
			}()
		}
	}

	// One RemoveBalls per witness; a packet per ship would fast-forward each
	// witness's sim clock a second per ship (removeBallsBatched).
	for _, p := range removeBallsBatched(ss, live) {
		for _, entityID := range live {
			// Red-box cleanup for anyone the fleet was shooting.
			p.ClearAggressor(entityID)

			// Free server-side target locks on the removed ship (the client
			// clears its own UI on ball removal but never sends RemoveTarget).
			pds, ok := p.PlayerState.(*destinyConn)
			if !ok || pds == nil {
				continue
			}
			pds.targetMu.Lock()
			if cancelFn, ok := pds.lockingTargets[entityID]; ok {
				cancelFn()
				delete(pds.lockingTargets, entityID)
			}
			wasLocked := false
			filtered := pds.targets[:0]
			for _, t := range pds.targets {
				if t == entityID {
					wasLocked = true
				} else {
					filtered = append(filtered, t)
				}
			}
			pds.targets = filtered
			pds.targetMu.Unlock()
			if wasLocked && p.Conn != nil {
				lostArgs := marshal.PyTuple{Items: []marshal.PyRep{
					marshal.PyString{Value: "lost"},
					marshal.PyInt{Value: uint64(entityID)},
					marshal.PyNone{},
				}}
				_ = server.SendNotification(p.Conn, int64(p.AccountID), "OnTarget", "charid", lostArgs)
			}
		}
	}
}

// breakCloakForPursuit drops a pursued pilot's cloak when the police land
// (CloakingDisengagedPolicePursuit): the gate cloak, or a cloaking device, which is switched
// off. A GM cloak is left alone.
func breakCloakForPursuit(target *destiny.Entity) {
	if target == nil || !target.IsCloaked() || target.IsGMCloak() {
		return
	}
	tds, ok := target.PlayerState.(*destinyConn)
	if !ok || tds == nil {
		return
	}
	if modID := target.ModuleCloak(); modID != 0 {
		forceModuleCloakOff(tds, target, modID)
	}
	breakGateCloak(tds)
	if target.Conn != nil {
		pushRemoteMessage(target.Conn, int64(target.AccountID), "CloakingDisengagedPolicePursuit", nil)
	}
}
