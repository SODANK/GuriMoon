package main

// "Trash It": inventorymgr.TrashItems([itemIDs]) from menusvc.py. The client offers it only on
// items in the pilot's own hangar at the docked station, never on the active ship.

import (
	"context"
	"database/sql"
	"log"
	"time"

	evedb "gurimoon/db"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
)

// trashEligible is the server's restatement of the client's menu gate:
// mine, in my hangar at this station, not the ship I am sitting in.
func trashEligible(er *evedb.EntityRow, charID, stationID, activeShipID int64) (bool, string) {
	switch {
	case er == nil:
		return false, "no such item"
	case er.OwnerID != charID:
		return false, "not owned by the caller"
	case stationID == 0 || er.LocationID != stationID || er.Flag != 4:
		return false, "not in the caller's hangar here"
	case er.ItemID == activeShipID:
		return false, "the active ship"
	}
	return true, ""
}

// pyIDs reads a list or tuple of ints; anything else is empty.
func pyIDs(rep marshal.PyRep) []int64 {
	var items []marshal.PyRep
	switch v := rep.(type) {
	case marshal.PyList:
		items = v.Items
	case marshal.PyTuple:
		items = v.Items
	default:
		return nil
	}
	out := make([]int64, 0, len(items))
	for _, it := range items {
		if n, ok := it.(marshal.PyInt); ok {
			out = append(out, int64(n.Value))
		}
	}
	return out
}

func handleTrashItems(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket,
	accountID, charID, stationID int64, sqldb *sql.DB) error {
	none := marshal.PyNone{}
	if sqldb == nil || charID == 0 || len(cs.ArgTuple.Items) == 0 {
		return server.SendCallRsp(c, pkt, accountID, none)
	}
	ids := pyIDs(cs.ArgTuple.Items[0])
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	activeShip, _ := evedb.GetCurrentShipID(ctx, sqldb, charID)

	// Validate everything first: one locked print refuses the whole batch before any row is touched.
	var rows []*evedb.EntityRow
	for _, id := range ids {
		er, err := evedb.GetEntityRow(ctx, sqldb, id)
		if err != nil || er == nil {
			continue
		}
		if ok, why := trashEligible(er, charID, stationID, activeShip); !ok {
			log.Printf("[%s] TrashItems %d skipped: %s", addr, id, why)
			continue
		}
		if evedb.IsItemLockedDown(ctx, sqldb, id) {
			return clientMsgErr(c, pkt, accountID, "CrpItemIsLocked", nil)
		}
		rows = append(rows, er)
	}

	push := func(er *evedb.EntityRow) {
		if err := server.SendNotification(c, accountID, "OnItemChange", "charid", itemDeletedNotifArgs(er)); err != nil {
			log.Printf("[%s] TrashItems OnItemChange: %v", addr, err)
		}
	}
	for _, er := range rows {
		// An assembled ship or container takes its contents with it; a row under a deleted parent can
		// never be listed again.
		children, err := evedb.GetChildEntityRows(ctx, sqldb, er.ItemID)
		if err != nil {
			log.Printf("[%s] TrashItems children of %d: %v", addr, er.ItemID, err)
			continue
		}
		for i := range children {
			if err := evedb.DeleteEntityDeep(ctx, sqldb, children[i].ItemID); err != nil {
				log.Printf("[%s] TrashItems child %d: %v", addr, children[i].ItemID, err)
				continue
			}
			push(&children[i])
		}
		if err := evedb.DeleteEntityDeep(ctx, sqldb, er.ItemID); err != nil {
			log.Printf("[%s] TrashItems %d: %v", addr, er.ItemID, err)
			continue
		}
		push(er)
		log.Printf("[%s] → TrashItems charID=%d itemID=%d typeID=%d qty=%d (+%d inside)",
			addr, charID, er.ItemID, er.TypeID, er.Quantity, len(children))
	}
	return server.SendCallRsp(c, pkt, accountID, none)
}
