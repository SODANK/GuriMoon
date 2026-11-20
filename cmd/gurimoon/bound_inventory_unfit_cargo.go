package main

import (
	"context"
	"database/sql"
	"log"

	evedb "gurimoon/db"
	"gurimoon/dogma"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
)

// isFittedModule reports whether er sits in a high/med/low slot as a module
// (not a charge loaded in that slot).
func isFittedModule(er *evedb.EntityRow) bool {
	return er != nil && er.Flag >= 11 && er.Flag <= 34 && er.CategoryID != 8
}

// loadedChargeVolume is the cargo volume the charge loaded in a fitted module
// takes once the module is unfitted to cargo; 0 for anything that is not a
// fitted module or has no charge.
func loadedChargeVolume(ctx context.Context, sqldb *sql.DB, er *evedb.EntityRow) float64 {
	if !isFittedModule(er) {
		return 0
	}
	lc, _ := evedb.GetLoadedCharge(ctx, sqldb, er.LocationID, er.Flag)
	if lc == nil {
		return 0
	}
	v, _ := evedb.GetTypeVolume(ctx, sqldb, lc.TypeID)
	return v * float64(lc.Quantity)
}

// prepFittedModuleForCargo readies a fitted module for a move into the cargo
// hold of shipID: its loaded charge moves to cargo, its online effect stops and
// its isOnline row is dropped.
//
// A cargo unfit keeps the module's locationID (only the flag changes), so unlike
// a hangar unfit the client's godma does not drop the module from the ship on
// OnItemChange alone; without the online stop it keeps a ghost fit in the slot.
// Call before the module's flag changes: GetOnlineModuleIDs reads slot flags.
func prepFittedModuleForCargo(ctx context.Context, c *evenet.Conn, addr string, accountID, charID, shipID int64,
	modER *evedb.EntityRow, sqldb *sql.DB) {
	if !isFittedModule(modER) {
		return
	}
	modLoc, modFlag := modER.LocationID, modER.Flag
	if loaded, _ := evedb.GetLoadedCharge(ctx, sqldb, modLoc, modFlag); loaded != nil {
		if chargeER, _ := evedb.GetEntityRow(ctx, sqldb, loaded.ItemID); chargeER != nil {
			chOldLoc, chOldFlag := chargeER.LocationID, chargeER.Flag
			_ = evedb.UnloadChargeToCargoOrHangar(ctx, sqldb, loaded.ItemID, shipID, 5)
			if newChER, _ := evedb.GetEntityRow(ctx, sqldb, loaded.ItemID); newChER != nil {
				chgChange := marshal.PyDict{Items: []marshal.DictEntry{
					{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: uint64(chOldLoc)}},
					{Key: marshal.PyInt{Value: 4}, Value: marshal.PyInt{Value: uint64(chOldFlag)}},
				}}
				_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
					evedb.BuildEntityRow(newChER), chgChange,
				}})
			}
			log.Printf("[%s] auto-unloaded charge %d from slot %d before cargo move", addr, loaded.ItemID, modFlag)
		}
	}
	onIDs, _ := evedb.GetOnlineModuleIDs(ctx, sqldb, modLoc)
	if onIDs[modER.ItemID] {
		sendGodmaShipEffect(c, accountID, modER.ItemID, modER.TypeID, modLoc, charID, 0, "online", false, 0, 0)
		sendModuleAttributeChange(c, accountID, charID, modER.ItemID, dogma.AttrIsOnline, 0, 1)
	}
	_ = evedb.ClearModuleOnlineState(ctx, sqldb, modER.ItemID)
}
