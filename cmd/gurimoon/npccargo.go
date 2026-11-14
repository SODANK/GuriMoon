package main

// Open Cargo on a lootable dungeon structure (npc.IsCargoStructure, the groups menusvc draws Open
// Cargo on). The first open rolls the structure's loot
// (rollNPCLoot, as the death drop does) into item rows whose locationID is the NPC entity ID, in the
// destiny.NPCIDBase band that is swept at boot. The structure then lists and moves like a loot can.
// A destroyed opened structure moves its contents into the death can (npcCargoToDeathCan).

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/esp"
	"gurimoon/npc"
	"gurimoon/safego"
	"gurimoon/tick"
)

var (
	npcCargoMu     sync.Mutex
	npcCargoRolled = map[uint32]bool{}
)

func init() {
	moduleBootHooks = append(moduleBootHooks, func(sqldb *sql.DB, _ *esp.SessionRegistry, _ *tick.Manager) {
		if sqldb == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		res, err := sqldb.ExecContext(ctx,
			`DELETE FROM entity WHERE locationID >= ? AND locationID < ?`,
			int64(destiny.NPCIDBase), int64(destiny.NPCIDBase)+100_000_000)
		if err != nil {
			log.Printf("[npc-cargo] boot sweep: %v", err)
			return
		}
		if n, _ := res.RowsAffected(); n > 0 {
			log.Printf("[npc-cargo] boot sweep: removed %d item(s) left in last run's opened structures", n)
		}
	})
}

// npcCargoEntity returns the live lootable structure itemID names in the
// caller's system, or nil.
func npcCargoEntity(ds *destinyConn, itemID int64) *destiny.Entity {
	if ds == nil || ds.solarSystem == nil || itemID < int64(destiny.NPCIDBase) || itemID >= int64(destiny.NPCIDBase)+100_000_000 {
		return nil
	}
	e := ds.solarSystem.GetEntity(uint32(itemID))
	if e == nil || e.IsDestroyed() || !npc.IsCargoStructure(e.Slim.GroupID) {
		return nil
	}
	return e
}

// isNPCCargo reports whether itemID is an opened lootable structure. List, StackAll and the
// item-source gate use it.
func isNPCCargo(ds *destinyConn, itemID int64) bool {
	if npcCargoEntity(ds, itemID) == nil {
		return false
	}
	npcCargoMu.Lock()
	defer npcCargoMu.Unlock()
	return npcCargoRolled[uint32(itemID)]
}

// npcCargoOpened reports whether the structure was opened (and so its loot
// already rolled), for the death drop.
func npcCargoOpened(entityID uint32) bool {
	npcCargoMu.Lock()
	defer npcCargoMu.Unlock()
	return npcCargoRolled[entityID]
}

// bindNPCCargo is the GetInventoryFromId arm: true when itemID is a lootable
// structure the caller is close enough to open, rolling its loot on the first
// open. The roll runs at most once per structure.
func bindNPCCargo(ds *destinyConn, sqldb *sql.DB, itemID int64) bool {
	e := npcCargoEntity(ds, itemID)
	if e == nil || sqldb == nil || !shipWithin(ds, e.Pos(), itemTransferRange+e.Manager.Radius()) {
		return false
	}
	npcCargoMu.Lock()
	if npcCargoRolled[e.ID] {
		npcCargoMu.Unlock()
		return true
	}
	npcCargoRolled[e.ID] = true
	npcCargoMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var dunObjectID int64
	if globalNPCMgr != nil {
		dunObjectID = globalNPCMgr.GetDunObjectID(e.ID)
	}
	rolled := rollNPCLoot(ctx, sqldb, e.Slim.TypeID, e.Slim.GroupID, "", dunObjectID)
	for _, d := range rolled {
		if err := evedb.CreateNPCLootItem(ctx, sqldb, itemID, e.Slim.OwnerID, d.typeID, d.qty); err != nil {
			log.Printf("[npc-cargo] structure %d: CreateNPCLootItem type=%d: %v", itemID, d.typeID, err)
		}
	}
	log.Printf("[npc-cargo] structure %d (type %d, dunObject %d) opened: %d drop(s)", itemID, e.Slim.TypeID, dunObjectID, len(rolled))
	return true
}

// npcCargoToDeathCan is the death drop for an opened structure: what is left
// inside goes into a loot can at the death position, and nothing is rolled
// again. Runs on its own goroutine like spawnNPCLoot.
func npcCargoToDeathCan(sqldb *sql.DB, ss *destiny.SolarSystem, entityID uint32, ownerID int64, deathPos destiny.Vec3) {
	defer safego.Recover("npcCargoToDeathCan")
	npcCargoMu.Lock()
	delete(npcCargoRolled, entityID)
	npcCargoMu.Unlock()
	if sqldb == nil || ss == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var n int
	if err := sqldb.QueryRowContext(ctx, `SELECT COUNT(*) FROM entity WHERE locationID = ?`, int64(entityID)).Scan(&n); err != nil || n == 0 {
		return
	}
	containerID, err := evedb.CreateLootContainer(ctx, sqldb, ownerID, ss.ID, deathPos.X, deathPos.Y, deathPos.Z)
	if err != nil {
		log.Printf("[npc-cargo] structure %d: CreateLootContainer: %v", entityID, err)
		return
	}
	if _, err := sqldb.ExecContext(ctx,
		`UPDATE entity SET locationID = ?, ownerID = ? WHERE locationID = ?`,
		containerID, ownerID, int64(entityID)); err != nil {
		log.Printf("[npc-cargo] structure %d: move contents: %v", entityID, err)
		return
	}
	log.Printf("[npc-cargo] structure %d destroyed: %d item(s) moved to can %d", entityID, n, containerID)
	spawnLootContainerBall(ss, sqldb, containerID, deathPos, ownerID)
}
