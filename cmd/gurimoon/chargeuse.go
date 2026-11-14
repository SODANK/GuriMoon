package main

// Charge consumption for modules that use one unit of their loaded charge per cycle:
// turrets and launchers (weaponCycleLoop) and capacitor boosters (capBoosterLoop).

import (
	"context"
	"database/sql"
	"log"
	"time"

	evedb "gurimoon/db"
	"gurimoon/dogma"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/safego"
	"gurimoon/server"
)

// groupCapacitorBooster is invGroups 76: the modules that take Cap Booster charges (group 87).
const groupCapacitorBooster int64 = 76

// consumeOneCharge uses up one unit of the charge loaded in the module slot slotFlag and tells
// the client: the last unit deletes the stack, any other lowers its quantity. loaded is false
// when the slot holds no charge; depleted is true when this unit was the last.
func consumeOneCharge(c *evenet.Conn, addr string, accountID, charID int64, ds *destinyConn, slotFlag int64, sqldb *sql.DB) (loaded, depleted bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lc, err := evedb.GetLoadedCharge(ctx, sqldb, ds.shipID, slotFlag)
	if err != nil {
		log.Printf("[%s] charge use: lookup ship=%d flag=%d: %v", addr, ds.shipID, slotFlag, err)
	}
	if lc == nil {
		return false, false
	}
	oldQty := lc.Quantity
	if oldQty <= 1 {
		_ = evedb.DeleteEntity(ctx, sqldb, lc.ItemID)
		delER := &evedb.EntityRow{ItemID: lc.ItemID, TypeID: lc.TypeID, OwnerID: charID,
			LocationID: ds.shipID, Flag: slotFlag, Quantity: oldQty}
		_ = server.SendNotification(c, accountID, "OnItemChange", "charid", itemDeletedNotifArgs(delER))
		log.Printf("[%s] charge use: last unit of charge %d (flag %d) used", addr, lc.ItemID, slotFlag)
		return true, true
	}
	_ = evedb.UpdateCargoItemQty(ctx, sqldb, lc.ItemID, oldQty-1)
	if updER, _ := evedb.GetEntityRow(ctx, sqldb, lc.ItemID); updER != nil {
		// Key 7 is quantity: the dict carries the old value.
		qtyChange := marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(oldQty)}},
		}}
		_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
			evedb.BuildEntityRow(updER),
			qtyChange,
		}})
	}
	return true, false
}

// capBoosterLoop runs a capacitor booster: each cycle uses one loaded charge and injects its
// capacitorBonus at the start, pushing the new charge to the client (attr 18). It stops, with
// the stop event, when the slot is empty, at the end of the cycle that used the last charge,
// or when cancelled. The client reloads by itself on a stop with no charge left.
func capBoosterLoop(ctx context.Context, c *evenet.Conn, addr string, accountID, charID int64, ds *destinyConn, modItemID, modTypeID, modSlotFlag int64, effectName string, durationMs int64, boostAmount float64, sqldb *sql.DB) {
	defer safego.Recover("capBoosterLoop")
	if durationMs <= 0 {
		durationMs = 15000
	}
	cycleDuration := time.Duration(durationMs) * time.Millisecond
	log.Printf("[%s] cap booster start: mod=%d +%.0f GJ per %v", addr, modItemID, boostAmount, cycleDuration)
	stop := func() {
		stopWeaponFX(c, addr, accountID, charID, ds, modItemID, modTypeID, 0, effectName, nil)
	}
	for {
		if ds.entity == nil {
			return
		}
		loaded, depleted := consumeOneCharge(c, addr, accountID, charID, ds, modSlotFlag, sqldb)
		if !loaded {
			log.Printf("[%s] cap booster: mod=%d has no charge left, stopping", addr, modItemID)
			stop()
			return
		}
		newVal := ds.entity.BoostCap(boostAmount)
		sendModuleAttributeChange(c, accountID, charID, ds.shipID, dogma.AttrCharge, newVal, 0)
		select {
		case <-ctx.Done():
			stop()
			return
		case <-time.After(cycleDuration):
		}
		if depleted {
			stop()
			return
		}
	}
}
