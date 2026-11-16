package main

// Ship Maintenance Array: storing and boarding ships in space. The client's
// calls arrive on the ship moniker (menusvc StoreVessel / BoardStoredShip via
// PerformSessionChange); the reply is ignored and the client waits for the
// shipid session change. The pilot moves with swapPilotInSpace (eject.go):
// new ball delivered, then session.shipid, then the old hull leaves the
// pilot's grid (bystanders lose it at once). Launch Ship and
// Scoop to Ship Maintenance Bay are refused out loud. Unpiloted hulls exist
// now (eject.go) but neither path is wired to them yet.

import (
	"context"
	"database/sql"
	"log"

	evedb "gurimoon/db"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
)

const groupCapsule = 29

func handleSMAShipCall(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket,
	accountID, charID, stationID int64, sqldb *sql.DB, ds *destinyConn) error {

	refusal, after := smaShipCall(c, addr, accountID, charID, stationID, cs.Method, cs.ArgTuple.Items, sqldb, ds)
	if refusal != nil {
		return server.SendCallException(c, pkt, accountID, refusal)
	}
	if err := server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}); err != nil {
		return err
	}
	if after != nil {
		after()
	}
	return nil
}

// smaShipCall runs one Ship Maintenance Array ship call. Like shipSpaceCall
// (eject.go) it returns the UserError to raise, or a function to run once the
// reply, plain or the inline bind tuple, has gone out. The client builds a
// fresh ship moniker per call, so these usually arrive inline on the bind.
func smaShipCall(c *evenet.Conn, addr string, accountID, charID, stationID int64, method string,
	args []marshal.PyRep, sqldb *sql.DB, ds *destinyConn) (marshal.PyRep, func()) {

	switch method {
	case "LaunchFromContainer", "ScoopToSMA":
		return customError("Unpiloted ships cannot be left floating in space here. Board the ship instead."), nil
	}
	if sqldb == nil || ds == nil || ds.entity == nil || ds.solarSystem == nil || stationID != 0 {
		return customError("You must be in space to do that."), nil
	}
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
		return customError(why), nil
	}

	switch method {
	case "StoreVessel":
		if ds.shipGroupID == groupCapsule {
			return customError("A capsule cannot be stored."), nil
		}
		if why := smaHasRoom(ctx, sqldb, sma.EntityID, ds.shipID); why != "" {
			return customError(why), nil
		}
		oldShip := ds.shipID
		capsuleID, err := evedb.CreateCapsuleEntity(ctx, sqldb, charID, ds.sysID, ds.charName)
		if err != nil {
			log.Printf("[%s] StoreVessel capsule: %v", addr, err)
			return customError("Could not eject you into a capsule."), nil
		}
		pos := ds.entity.Pos()
		_ = evedb.UpdateEntityPosition(ctx, sqldb, capsuleID, pos.X, pos.Y, pos.Z)
		if _, err := sqldb.ExecContext(ctx,
			`UPDATE entity SET locationID = ?, flag = 0 WHERE itemID = ?`, sma.EntityID, oldShip); err != nil {
			log.Printf("[%s] StoreVessel move ship: %v", addr, err)
			_ = evedb.DeleteEntity(ctx, sqldb, capsuleID)
			return customError("Could not store the ship."), nil
		}
		return nil, func() {
			swapPilotInSpace(c, addr, accountID, charID, sqldb, ds, capsuleID, oldHull{})
			log.Printf("[%s] ship %d stored in SMA %d by char %d (now in capsule %d)", addr, oldShip, sma.EntityID, charID, capsuleID)
		}

	case "BoardStoredShip":
		shipID := arg(1)
		er, err := evedb.GetEntityRow(ctx, sqldb, shipID)
		if err != nil || er == nil || er.LocationID != sma.EntityID || er.CategoryID != 6 {
			return customError("That ship is not in this Ship Maintenance Array."), nil
		}
		if er.OwnerID != charID {
			return customError("That ship belongs to someone else."), nil
		}
		if uerr := boardSkillError(ctx, sqldb, charID, shipID); uerr != nil {
			return uerr, nil
		}
		// A pilot the police are engaging may not swap out of the fight.
		if concordIsFlagged(charID) {
			return server.BuildUserError("ShipInvolvedInPoliceCrime", nil), nil
		}
		oldShip := ds.shipID
		wasCapsule := ds.shipGroupID == groupCapsule
		if !wasCapsule {
			// Trade places: the current ship goes into the array.
			if why := smaHasRoom(ctx, sqldb, sma.EntityID, oldShip); why != "" {
				return customError(why), nil
			}
		}
		pos := ds.entity.Pos()
		if _, err := sqldb.ExecContext(ctx,
			`UPDATE entity SET locationID = ?, flag = 0, x = ?, y = ?, z = ? WHERE itemID = ?`,
			ds.sysID, pos.X, pos.Y, pos.Z, shipID); err != nil {
			log.Printf("[%s] BoardStoredShip launch: %v", addr, err)
			return customError("Could not launch the ship."), nil
		}
		if !wasCapsule {
			if _, err := sqldb.ExecContext(ctx,
				`UPDATE entity SET locationID = ?, flag = 0 WHERE itemID = ?`, sma.EntityID, oldShip); err != nil {
				log.Printf("[%s] BoardStoredShip store old ship: %v", addr, err)
			}
		}
		return nil, func() {
			swapPilotInSpace(c, addr, accountID, charID, sqldb, ds, shipID, oldHull{deleteRow: wasCapsule})
			log.Printf("[%s] char %d boarded ship %d from SMA %d (old ship %d, capsule=%v)",
				addr, charID, shipID, sma.EntityID, oldShip, wasCapsule)
		}
	}
	return customError("That is not possible here."), nil
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
