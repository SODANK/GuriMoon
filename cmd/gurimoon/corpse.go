package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/marshal"
	"gurimoon/safego"
	"gurimoon/server"
)

// ---- podded pilots' corpses ----
//
// A destroyed capsule leaves "<pilot>'s Frozen Corpse" (typeID 25, Biomass)
// where it died. The client draws it (spaceObject.Corpse) and offers Scoop To
// Cargo Hold on it (menusvc checkScoopable); the name comes from ball.name.
// The corpse rides the loot slices for their lifecycle, but
// GetLootContainerPosition refuses it, so no inventory gate opens or fills it.

// corpseBallSlim builds a corpse's ball and slim. Shared by the spawn and the
// system reload so the two cannot drift.
func corpseBallSlim(corpseID int64, pos destiny.Vec3, ownerID int64, name string) (destiny.BallDesc, destiny.SlimItem) {
	return destiny.BallDesc{
			EntityID: uint32(corpseID), Mode: destiny.ModeRigid, SubType: 2,
			Pos: pos, Radius: 1, Name: name,
		}, destiny.SlimItem{
			BallID: corpseID, ItemID: corpseID,
			TypeID: evedb.TypeCorpse, GroupID: evedb.GroupBiomass, CategoryID: 2,
			OwnerID: ownerID,
		}
}

// spawnPodCorpse creates the corpse row and puts its ball on the grid. Runs in
// its own goroutine off the death path, like the loot roll: a failure costs the
// body, never the respawn.
func spawnPodCorpse(sqldb *sql.DB, ss *destiny.SolarSystem, charID int64, charName string, pos destiny.Vec3) {
	defer safego.Recover("spawnPodCorpse")
	if sqldb == nil || ss == nil || charID == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	name := evedb.CorpseName(charName)
	corpseID, err := evedb.CreateCorpse(ctx, sqldb, charID, ss.ID, name, pos.X, pos.Y, pos.Z)
	if err != nil {
		log.Printf("[death] CreateCorpse charID=%d: %v", charID, err)
		return
	}
	bd, slim := corpseBallSlim(corpseID, pos, charID, name)
	spawnLootBall(ss, sqldb, bd, slim)
	log.Printf("[death] corpse %d (%q) left in system %d", corpseID, name, ss.ID)
}

// scoopCorpseFromCall answers ship.Scoop when the object is a corpse.
// handled=false means it is not one and the caller should try its other
// scoopables.
func scoopCorpseFromCall(ds *destinyConn, addr string, charID, itemID int64, sqldb *sql.DB) (handled bool, err error) {
	if ds.solarSystem == nil {
		return false, nil
	}
	ss := ds.solarSystem
	bd, slim, ok := ss.GetLootBall(uint32(itemID))
	if !ok || slim.TypeID != evedb.TypeCorpse {
		return false, nil
	}
	if ds.entity == nil || ds.shipID == 0 || sqldb == nil {
		return true, fmt.Errorf("you must be in space to scoop that")
	}
	if !shipWithin(ds, bd.Pos, itemTransferRange+bd.Radius) {
		return true, fmt.Errorf("you are too far away to scoop that")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if shipER, _ := evedb.GetEntityRow(ctx, sqldb, ds.shipID); shipER != nil {
		cargoCap, _ := evedb.GetShipCargoCapacityFitted(ctx, sqldb, shipER.TypeID, ds.shipID)
		usedVol, _ := evedb.GetCargoUsedVolume(ctx, sqldb, charID, ds.shipID)
		need := evedb.ItemVolume(ctx, sqldb, itemID, evedb.TypeCorpse)
		if free := cargoCap - usedVol; need > free {
			return true, fmt.Errorf("not enough cargo space: the corpse needs %.0f m3 and your hold has %.0f m3 free",
				need, max(free, 0))
		}
	}

	// Taking the ball out of the system is the claim: of two pilots scooping
	// one body, only one gets past this.
	if !ss.RemoveLootContainer(uint32(itemID)) {
		return true, fmt.Errorf("there is nothing here to scoop")
	}
	if err := evedb.ScoopCorpseToCargo(ctx, sqldb, itemID, ds.sysID, ds.shipID, charID); err != nil {
		if !errors.Is(err, evedb.ErrCorpseGone) {
			ss.AddLootContainer(bd, slim) // still floating; put it back
			return true, err
		}
		removeLootBallFromGrid(ss, itemID)
		return true, err
	}
	removeLootBallFromGrid(ss, itemID)

	if newER, err := evedb.GetEntityRow(ctx, sqldb, itemID); err == nil && newER != nil {
		changeDict := marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyInt{Value: 2}, Value: marshal.PyInt{Value: uint64(slim.OwnerID)}},
			{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: uint64(ds.sysID)}},
			{Key: marshal.PyInt{Value: 4}, Value: marshal.PyInt{Value: 0}},
		}}
		_ = server.SendNotification(ds.entity.Conn, int64(ds.entity.AccountID), "OnItemChange", "charid",
			marshal.PyTuple{Items: []marshal.PyRep{evedb.BuildEntityRow(newER), changeDict}})
	}
	log.Printf("[%s] corpse %d scooped by char %d into ship %d", addr, itemID, charID, ds.shipID)
	return true, nil
}

// removeLootBallFromGrid takes a loot-slice ball (already removed from the
// system's slices) off every client that holds it.
func removeLootBallFromGrid(ss *destiny.SolarSystem, id int64) {
	for _, p := range ss.GetAllPlayersInSystem() {
		if p.Conn == nil || !p.KnowsBall(uint32(id)) {
			continue
		}
		p.SendStamped(func(stamp uint32) error {
			return destiny.SendRemoveBalls(p.Conn, globalNodeID, p.AccountID, stamp, []uint32{uint32(id)})
		})
		p.ForgetBalls(uint32(id))
	}
}
