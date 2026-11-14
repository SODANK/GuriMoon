package main

// Ship Maintenance Array: storing and boarding ships in space. The client's
// calls arrive on the ship moniker (menusvc StoreVessel / BoardStoredShip via
// PerformSessionChange); the reply is ignored and the client waits for the
// shipid session change. The server does what the death path does for a
// capsule: old hull off the grid, pilot into the new one with the old
// ballpark state, new ball delivered, then session.shipid. Launch Ship and
// Scoop to Ship Maintenance Bay are refused out loud: both need an unpiloted
// ship ball in space, which this server does not model.

import (
	"context"
	"database/sql"
	"log"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
)

const groupCapsule = 29

func handleSMAShipCall(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket,
	accountID, charID, stationID int64, sqldb *sql.DB, ds *destinyConn) error {

	switch cs.Method {
	case "LaunchFromContainer", "ScoopToSMA":
		return corpUserErr(c, pkt, accountID,
			"Unpiloted ships cannot be left floating in space here. Board the ship instead.")
	}
	if sqldb == nil || ds == nil || ds.entity == nil || ds.solarSystem == nil || stationID != 0 {
		return corpUserErr(c, pkt, accountID, "You must be in space to do that.")
	}
	args := cs.ArgTuple.Items
	arg := func(i int) int64 {
		if i < len(args) {
			if v, ok := args[i].(marshal.PyInt); ok {
				return int64(v.Value)
			}
		}
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), starbaseSweepTimeout)
	defer cancel()

	smaID := arg(0)
	sma, why := smaForPilot(ctx, sqldb, ds, charID, smaID)
	if why != "" {
		return corpUserErr(c, pkt, accountID, why)
	}

	switch cs.Method {
	case "StoreVessel":
		if ds.shipGroupID == groupCapsule {
			return corpUserErr(c, pkt, accountID, "A capsule cannot be stored.")
		}
		if why := smaHasRoom(ctx, sqldb, sma.EntityID, ds.shipID); why != "" {
			return corpUserErr(c, pkt, accountID, why)
		}
		oldShip := ds.shipID
		capsuleID, err := evedb.CreateCapsuleEntity(ctx, sqldb, charID, ds.sysID, ds.charName)
		if err != nil {
			log.Printf("[%s] StoreVessel capsule: %v", addr, err)
			return corpUserErr(c, pkt, accountID, "Could not eject you into a capsule.")
		}
		pos := ds.dm.Pos
		_ = evedb.UpdateEntityPosition(ctx, sqldb, capsuleID, pos.X, pos.Y, pos.Z)
		if _, err := sqldb.ExecContext(ctx,
			`UPDATE entity SET locationID = ?, flag = 0 WHERE itemID = ?`, sma.EntityID, oldShip); err != nil {
			log.Printf("[%s] StoreVessel move ship: %v", addr, err)
			_ = evedb.DeleteEntity(ctx, sqldb, capsuleID)
			return corpUserErr(c, pkt, accountID, "Could not store the ship.")
		}
		if err := server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}); err != nil {
			return err
		}
		swapPilotShipInSpace(c, addr, accountID, charID, sqldb, ds, capsuleID, false)
		log.Printf("[%s] ship %d stored in SMA %d by char %d (now in capsule %d)", addr, oldShip, sma.EntityID, charID, capsuleID)
		return nil

	case "BoardStoredShip":
		shipID := arg(1)
		er, err := evedb.GetEntityRow(ctx, sqldb, shipID)
		if err != nil || er == nil || er.LocationID != sma.EntityID || er.CategoryID != 6 {
			return corpUserErr(c, pkt, accountID, "That ship is not in this Ship Maintenance Array.")
		}
		if er.OwnerID != charID {
			return corpUserErr(c, pkt, accountID, "That ship belongs to someone else.")
		}
		if uerr := boardSkillError(ctx, sqldb, charID, shipID); uerr != nil {
			return server.SendCallException(c, pkt, accountID, uerr)
		}
		// A pilot the police are engaging may not swap out of the fight.
		if concordIsFlagged(charID) {
			return clientMsgErr(c, pkt, accountID, "ShipInvolvedInPoliceCrime", nil)
		}
		oldShip := ds.shipID
		wasCapsule := ds.shipGroupID == groupCapsule
		if !wasCapsule {
			// Trade places: the current ship goes into the array.
			if why := smaHasRoom(ctx, sqldb, sma.EntityID, oldShip); why != "" {
				return corpUserErr(c, pkt, accountID, why)
			}
		}
		pos := ds.dm.Pos
		if _, err := sqldb.ExecContext(ctx,
			`UPDATE entity SET locationID = ?, flag = 0, x = ?, y = ?, z = ? WHERE itemID = ?`,
			ds.sysID, pos.X, pos.Y, pos.Z, shipID); err != nil {
			log.Printf("[%s] BoardStoredShip launch: %v", addr, err)
			return corpUserErr(c, pkt, accountID, "Could not launch the ship.")
		}
		if !wasCapsule {
			if _, err := sqldb.ExecContext(ctx,
				`UPDATE entity SET locationID = ?, flag = 0 WHERE itemID = ?`, sma.EntityID, oldShip); err != nil {
				log.Printf("[%s] BoardStoredShip store old ship: %v", addr, err)
			}
		}
		if err := server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}); err != nil {
			return err
		}
		swapPilotShipInSpace(c, addr, accountID, charID, sqldb, ds, shipID, wasCapsule)
		log.Printf("[%s] char %d boarded ship %d from SMA %d (old ship %d, capsule=%v)",
			addr, charID, shipID, sma.EntityID, oldShip, wasCapsule)
		return nil
	}
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// smaForPilot resolves and validates the array a pilot is using: a Ship
// Maintenance Array on this grid, anchored and online, in range, owned by the
// pilot's corporation or alliance.
func smaForPilot(ctx context.Context, sqldb *sql.DB, ds *destinyConn, charID, smaID int64) (evedb.StarbaseRecord, string) {
	slim, ok := ds.solarSystem.GetStarbaseSlim(uint32(smaID))
	if !ok || slim.GroupID != evedb.GroupShipMaintenanceArray {
		return evedb.StarbaseRecord{}, "That is not a Ship Maintenance Array."
	}
	rec, found, err := evedb.GetStarbaseStructure(ctx, sqldb, smaID)
	if err != nil || !found || rec.State != evedb.StarbaseStateOnline {
		return rec, "The Ship Maintenance Array must be anchored and online."
	}
	_, corpID, allianceID, err := evedb.GetCharSovContext(ctx, sqldb, charID)
	if err != nil || !evedb.StarbaseManagePermitted(rec, corpID, allianceID) {
		return rec, "That Ship Maintenance Array belongs to someone else."
	}
	if bd, ok := ds.solarSystem.GetStarbaseBall(uint32(smaID)); !ok || !shipWithin(ds, bd.Pos, itemTransferRange+bd.Radius) {
		return rec, "You are too far away from the Ship Maintenance Array."
	}
	return rec, ""
}

// smaHasRoom checks the array's capacity against an assembled ship.
func smaHasRoom(ctx context.Context, sqldb *sql.DB, smaID, shipID int64) string {
	er, err := evedb.GetEntityRow(ctx, sqldb, shipID)
	if err != nil || er == nil {
		return "Could not read the ship."
	}
	capacity, used, err := evedb.StructureBayCapacity(ctx, sqldb, smaID, 0)
	if err != nil {
		return ""
	}
	if !evedb.BayHasRoom(capacity, used, evedb.ItemVolume(ctx, sqldb, er.ItemID, er.TypeID)) {
		return "There is not enough room in the Ship Maintenance Array."
	}
	return ""
}

// swapPilotShipInSpace moves the pilot's presence on the grid from the current ship to newShipID.
// The old hull is removed (stored, or a capsule deleted when deleteOld). Order follows the death
// path: old ball out, new entity registered with the old ballpark state, new ball in, then
// session.shipid, because the client sets bp.ego from the session only once the ball exists.
func swapPilotShipInSpace(c *evenet.Conn, addr string, accountID, charID int64, sqldb *sql.DB,
	ds *destinyConn, newShipID int64, deleteOld bool) {

	ctx, cancel := context.WithTimeout(context.Background(), starbaseSweepTimeout)
	defer cancel()

	oldEntity := ds.entity
	oldID := oldEntity.ID
	oldShipDBID := ds.shipID
	ss := ds.solarSystem

	clearPlayerCombatState(ds)
	if globalDroneMgr != nil && oldShipDBID != 0 {
		globalDroneMgr.AbandonControl(oldShipDBID)
	}

	if err := evedb.MoveCharacterIntoShip(ctx, sqldb, charID, newShipID); err != nil {
		log.Printf("[%s] ship swap: MoveCharacterIntoShip %d: %v", addr, newShipID, err)
		return
	}
	if deleteOld {
		_ = evedb.DeleteEntity(ctx, sqldb, oldShipDBID)
	}

	// The old hull leaves every grid it is on, a plain removal, not an explosion,
	// including the pilot's own.
	for _, p := range ss.RemoveEntity(oldID) {
		if p.Conn == nil || !p.KnowsBall(oldID) {
			continue
		}
		if err := p.SendStamped(func(stamp uint32) error {
			return destiny.SendRemoveBalls(p.Conn, globalNodeID, p.AccountID, stamp, []uint32{oldID})
		}); err == nil {
			p.ForgetBalls(oldID)
		}
	}
	if c != nil {
		oldEntity.SendStampedUrgent(func(stamp uint32) error {
			return destiny.SendRemoveBalls(c, globalNodeID, uint64(accountID), stamp, []uint32{oldID})
		})
	}

	// Rebuild the pilot's destiny state on the new hull where it floats.
	ds.pendingBallparkFrom = oldEntity
	ds.pendingBallparkOldID = oldID
	ds.dm = nil
	initDestiny(addr, charID, 0, sqldb, ds)
	if ds.dm == nil {
		ds.pendingBallparkFrom = nil
		log.Printf("[%s] ship swap: initDestiny failed for ship %d", addr, newShipID)
		return
	}
	registerUndock(c, addr, uint64(accountID), charID, sqldb, ds)
	ds.pendingBallparkFrom = nil
	if ds.entity == nil || c == nil {
		return
	}

	e := ds.entity
	bd := e.BallDesc()
	bd.CorpID = uint32(e.Slim.CorpID)
	bd.Name = e.BallName()
	dmg := e.DamageStateFrac()
	dmg.Timestamp = server.Win32TimeNow()
	if err := e.SendStampedUrgent(func(stamp uint32) error {
		return destiny.SendAddBallsFull(c, globalNodeID, uint64(accountID), stamp,
			[]destiny.BallDesc{bd}, []destiny.SlimItem{e.Slim},
			map[uint32]destiny.DamageState{e.ID: dmg})
	}); err != nil {
		log.Printf("[%s] ship swap: AddBalls %d: %v", addr, newShipID, err)
	}
	e.ClaimUnknownBalls(e.ID)
	time.Sleep(50 * time.Millisecond) // let the ball land before the session points at it
	if err := sendShipIDSessionChange(c, addr, oldShipDBID, newShipID); err != nil {
		log.Printf("[%s] ship swap: session change: %v", addr, err)
	}
}
