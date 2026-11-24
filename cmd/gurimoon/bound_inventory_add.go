package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	evedb "gurimoon/db"
	"gurimoon/dogma"
	"gurimoon/marshal"
	_ "gurimoon/modules"
	"gurimoon/server"
)

// boundMultiAdd handles the bound call "MultiAdd".
func boundMultiAdd(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, stationID, sqldb, ds, sessReg := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.stationID, cc.sqldb, cc.ds, cc.sessReg
	// InventoryBound.MultiAdd(itemIDs, quantity, flag): the fitting window's
	// drag from hangar to ship.
	if len(cs.ArgTuple.Items) < 1 {
		log.Printf("[%s] bound.MultiAdd: missing args", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	var maItemIDs []int64
	switch v := cs.ArgTuple.Items[0].(type) {
	case marshal.PyList:
		for _, it := range v.Items {
			if pi, ok := it.(marshal.PyInt); ok {
				maItemIDs = append(maItemIDs, int64(pi.Value))
			}
		}
	case marshal.PyTuple:
		for _, it := range v.Items {
			if pi, ok := it.(marshal.PyInt); ok {
				maItemIDs = append(maItemIDs, int64(pi.Value))
			}
		}
	}
	// qty 0 = all; the client sends None for whole stacks. A drag from another
	// container's window sends (ids, sourceID, flag=...) (invAddArgs).
	maQty, maFlag := invAddArgs(cs)
	maIsHangar := cs.RemoteObject == hangarBindID ||
		strings.HasSuffix(cs.RemoteObjectStr, fmt.Sprintf(":%d", hangarBindID))
	log.Printf("[%s] bound.MultiAdd itemIDs=%v qty=%d flag=%d isHangar=%v charID=%d bind=%d args=%v kwargs=%v",
		addr, maItemIDs, maQty, maFlag, maIsHangar, charID, bindNumFromCallStream(cs), cs.ArgTuple, cs.ArgDict)
	if courierSealBlocks(sqldb, cs, maItemIDs) {
		log.Printf("[%s] bound.MultiAdd: refused — courier package is sealed", addr)
		return sendNotInMissionPackage(c, pkt, accountID)
	}
	// Member hangar binds are view only (see bound.Add).
	if isMemberHangarBind(bindNumFromCallStream(cs)) {
		log.Printf("[%s] bound.MultiAdd: refused — member hangar is view only", addr)
		return corpUserErr(c, pkt, accountID, "That hangar is view only.")
	}
	// Jetcan, kill can or launched container in space: a multi-item drop (jetcan.go).
	if contBind := bindNumFromCallStream(cs); !maIsHangar && isLootContainer(ds, contBind) {
		for _, maItemID := range maItemIDs {
			if refused, err := handleLootCanAdd(c, addr, pkt, accountID, charID, contBind, maItemID, 0, sqldb, ds); refused {
				return err
			}
		}
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	// The pilot's own assembled container (alsc.go): multi-item drop.
	if !maIsHangar && sqldb != nil && charID != 0 {
		if contBind := bindNumFromCallStream(cs); contBind >= 100000000 && isOwnedContainerBind(sqldb, charID, contBind) {
			for _, maItemID := range maItemIDs {
				// Stop at the first refusal: it has answered the call.
				if refused, err := handleContainerAddRefused(c, addr, pkt, accountID, charID, derefStation(stationID),
					contBind, maItemID, 0, false, sqldb, ds); refused || err != nil {
					return err
				}
			}
			log.Printf("[%s] → bound.MultiAdd(container %d): %d item(s)", addr, contBind, len(maItemIDs))
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
	}
	// Structure bay: container.py sends MultiAdd for a multi-item drag into a
	// tower, sentry or silo window.
	if bayID := bindNumFromCallStream(cs); isStructureBay(ds, bayID) {
		for _, maItemID := range maItemIDs {
			_ = handleStarbaseBayAdd(c, addr, pkt, accountID, charID, bayID, maItemID, 0, maFlag, false, sqldb, ds)
		}
		log.Printf("[%s] → bound.MultiAdd(structure %d): %d item(s) flag %d", addr, bayID, len(maItemIDs), maFlag)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	// Opened dungeon container: a multi-item drop (dungeon_cargodrop.go).
	if contBind := bindNumFromCallStream(cs); isNPCCargo(ds, contBind) {
		for _, maItemID := range maItemIDs {
			if refused, err := handleDunCargoAdd(c, addr, pkt, accountID, charID, contBind, maItemID, 0, sqldb, ds); refused {
				return err
			}
		}
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	// Rented office: multi-item deposit into a corp hangar division.
	if !maIsHangar && sqldb != nil && charID != 0 && *stationID != 0 {
		if officeBind := bindNumFromCallStream(cs); officeBind >= 100000000 {
			octx, ocancel := context.WithTimeout(context.Background(), 5*time.Second)
			isOffice := evedb.IsOfficeItemID(octx, sqldb, officeBind)
			ocancel()
			if isOffice {
				for _, maItemID := range maItemIDs {
					_, _ = handleOfficeAdd(c, addr, pkt, accountID, charID, *stationID,
						officeBind, maItemID, 0, maFlag, false, sqldb, sessReg)
				}
				log.Printf("[%s] → bound.MultiAdd(office %d): %d item(s) flag %d", addr, officeBind, len(maItemIDs), maFlag)
				return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
			}
		}
	}
	if maIsHangar {
		// Strip Fitting and batch moves to the station hangar arrive as
		// hangar.MultiAdd with no flag and move items like RemoveItems; they never fit.
		if sqldb == nil || charID == 0 || *stationID == 0 {
			log.Printf("[%s] bound.MultiAdd(hangar): no station context", addr)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		maShipID, _ := evedb.GetCurrentShipID(ctx, sqldb, charID)
		for _, maItemID := range maItemIDs {
			oldER, err := evedb.GetEntityRow(ctx, sqldb, maItemID)
			if err != nil || oldER == nil {
				log.Printf("[%s] bound.MultiAdd(hangar): GetEntityRow(%d): %v", addr, maItemID, err)
				continue
			}
			oldLocID := oldER.LocationID
			oldFlag := oldER.Flag
			// A corp-office item moved to the personal hangar changes owner under the
			// division take role.
			if oldER.OwnerID != charID && *stationID != 0 {
				if handled, _ := handleOfficeTakeToHangar(c, addr, pkt, accountID, charID, *stationID,
					oldER, 0, false, sqldb, sessReg); handled {
					continue
				}
			}
			if _, why, ok := authorizeItemMove(ctx, sqldb, ds, charID, maShipID, *stationID,
				oldER, destHangar); !ok {
				log.Printf("[%s] bound.MultiAdd(hangar): refused item %d (loc %d owner %d): %s",
					addr, maItemID, oldLocID, oldER.OwnerID, why)
				continue
			}
			// Unload a charge still in this module's slot first; the strip list
			// normally carries charges ahead of modules.
			if oldFlag >= 11 && oldFlag <= 34 && oldER.CategoryID != 8 {
				if loaded, _ := evedb.GetLoadedCharge(ctx, sqldb, oldLocID, oldFlag); loaded != nil {
					chargeER, _ := evedb.GetEntityRow(ctx, sqldb, loaded.ItemID)
					if chargeER != nil {
						chOldLoc := chargeER.LocationID
						chOldFlag := chargeER.Flag
						_ = evedb.UnfitModuleToHangar(ctx, sqldb, loaded.ItemID, *stationID)
						if newChER, _ := evedb.GetEntityRow(ctx, sqldb, loaded.ItemID); newChER != nil {
							chgChange := itemMoveChange(newChER, newChER.OwnerID, chOldLoc, chOldFlag)
							_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
								evedb.BuildEntityRow(newChER), chgChange,
							}})
						}
						log.Printf("[%s] bound.MultiAdd(hangar): auto-unloaded charge %d from slot %d", addr, loaded.ItemID, oldFlag)
					}
				}
			}
			if err := evedb.UnfitModuleToHangar(ctx, sqldb, maItemID, *stationID); err != nil {
				log.Printf("[%s] bound.MultiAdd(hangar): UnfitModuleToHangar(%d): %v", addr, maItemID, err)
				continue
			}
			if newER, _ := evedb.GetEntityRow(ctx, sqldb, maItemID); newER != nil {
				changeDict := marshal.PyDict{Items: []marshal.DictEntry{
					{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: uint64(oldLocID)}},
					{Key: marshal.PyInt{Value: 4}, Value: marshal.PyInt{Value: uint64(oldFlag)}},
				}}
				_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
					evedb.BuildEntityRow(newER), changeDict,
				}})
			}
			log.Printf("[%s] bound.MultiAdd(hangar): moved item %d → hangar station %d", addr, maItemID, *stationID)
		}
		if maShipID != 0 {
			sendShipFittingStats(c, accountID, charID, maShipID, sqldb)
			pushWeaponAttrs(c, accountID, charID, maShipID, sqldb, ds.activeEffectIDs())
			broadcastSlimItemChange(c, accountID, ds, maShipID, sqldb)
		}
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	if sqldb != nil && charID != 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		shipID, _ := evedb.GetCurrentShipID(ctx, sqldb, charID)
		if shipID == 0 {
			log.Printf("[%s] bound.MultiAdd: no current ship", addr)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		// Source locations of moved items; emptied loot containers despawn after
		// the loop.
		maSrcLocs := map[int64]bool{}
		maTold := map[string]bool{} // fit refusals already shown this call
		maRefusal := ""             // first refusal, shown when nothing moved
		maMoved := false
		if maFlag == 0 && lootTakeDefaultsToCargo(ctx, sqldb, ds, maItemIDs) {
			maFlag = dogma.FlagCargo
		}
		shipER, _ := evedb.GetEntityRow(ctx, sqldb, shipID)
		var hiSlots, medSlots, loSlots int64
		var cpuOutput, pgOutput float64
		var turretMax, launcherMax float64
		if shipER != nil {
			hiSlots, medSlots, loSlots, _ = evedb.GetShipHullSlots(ctx, sqldb, shipER.TypeID)
			cpuOutput, pgOutput, _ = evedb.GetShipCPUPGCapacityFitted(ctx, sqldb, shipER.TypeID, shipID)
			hullAttrs, _ := evedb.GetTypeAttributes(ctx, sqldb, shipER.TypeID)
			if hullAttrs != nil {
				turretMax = hullAttrs[dogma.AttrTurretSlotsLeft]
				launcherMax = hullAttrs[dogma.AttrLauncherSlotsLeft]
			}
		}
		for _, maItemID := range maItemIDs {
			oldER, err := evedb.GetEntityRow(ctx, sqldb, maItemID)
			if err != nil || oldER == nil {
				log.Printf("[%s] bound.MultiAdd: GetEntityRow(%d): %v", addr, maItemID, err)
				continue
			}
			// Corp office stock moved into the cargo hold changes owner under the
			// division take role.
			if maFlag == 5 && oldER.OwnerID != charID && *stationID != 0 {
				if handled, _ := handleOfficeTake(c, addr, pkt, accountID, charID, *stationID,
					oldER, 0, false, false, sqldb, sessReg); handled {
					continue
				}
			}
			maSrc, maWhy, maOK := authorizeItemMove(ctx, sqldb, ds, charID, shipID, *stationID,
				oldER, itemDestFor(false, maFlag))
			if !maOK {
				log.Printf("[%s] bound.MultiAdd: refused item %d (loc %d owner %d): %s",
					addr, maItemID, oldER.LocationID, oldER.OwnerID, maWhy)
				if maRefusal == "" {
					maRefusal = maWhy
				}
				continue
			}
			if maSrc == srcStructureBay && !structureTakeAllowed(ctx, addr, sqldb, ds, oldER.LocationID, charID, oldER.Flag) {
				continue
			}
			// Drone-bay drops (menusvc FitDrone, MultiAdd(ids, None, 87)) move the
			// whole stack and skip the module logic.
			if maFlag == 87 {
				if oldER.CategoryID != 18 {
					log.Printf("[%s] bound.MultiAdd: item %d (category %d) is not a drone — skipping drone-bay drop", addr, maItemID, oldER.CategoryID)
					continue
				}
				if shipER != nil {
					maHullAttrs, _ := evedb.GetTypeAttributes(ctx, sqldb, shipER.TypeID)
					droneCap := maHullAttrs[dogma.AttrDroneCapacity]
					usedVol, _ := evedb.GetDroneBayUsedVolume(ctx, sqldb, shipID)
					itemVol, _ := evedb.GetTypeVolume(ctx, sqldb, oldER.TypeID)
					if itemVol*float64(oldER.Quantity) > droneCap-usedVol {
						log.Printf("[%s] bound.MultiAdd: drone bay full for item %d (used %.1f + needed %.1f > cap %.1f)",
							addr, maItemID, usedVol, itemVol*float64(oldER.Quantity), droneCap)
						continue
					}
				}
				if err := evedb.MoveStackToDroneBay(ctx, sqldb, maItemID, shipID); err != nil {
					log.Printf("[%s] bound.MultiAdd(droneBay): %v", addr, err)
					continue
				}
				if newOwner := takeOwnerFor(maSrc, charID, oldER.OwnerID); newOwner != oldER.OwnerID {
					_ = evedb.SetEntityOwner(ctx, sqldb, maItemID, newOwner)
					noteLootTake(ctx, sqldb, ds, maSrc, charID, oldER.OwnerID, oldER.LocationID)
				}
				if newER, _ := evedb.GetEntityRow(ctx, sqldb, maItemID); newER != nil {
					changeDict := marshal.PyDict{Items: []marshal.DictEntry{
						{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: uint64(oldER.LocationID)}},
						{Key: marshal.PyInt{Value: 4}, Value: marshal.PyInt{Value: uint64(oldER.Flag)}},
					}}
					_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
						evedb.BuildEntityRow(newER), changeDict,
					}})
				}
				log.Printf("[%s] bound.MultiAdd: moved item %d (qty %d) → drone bay of ship %d", addr, maItemID, oldER.Quantity, shipID)
				maSrcLocs[oldER.LocationID] = true
				maMoved = true
				continue
			}
			// Cargo drops (MultiAdd(ids, None, 5)) move the whole stack; the module
			// path would force singleton=1 and quantity=1.
			if maFlag == 5 {
				moveQty := oldER.Quantity
				if shipER != nil {
					cargoCap, _ := evedb.GetShipCargoCapacityFitted(ctx, sqldb, shipER.TypeID, shipID)
					usedVol, _ := evedb.GetCargoUsedVolume(ctx, sqldb, charID, shipID)
					itemVol := evedb.ItemVolume(ctx, sqldb, oldER.ItemID, oldER.TypeID)
					if itemVol*float64(moveQty)+loadedChargeVolume(ctx, sqldb, oldER) > cargoCap-usedVol {
						log.Printf("[%s] bound.MultiAdd(cargo): cargo full for item %d", addr, maItemID)
						continue
					}
				}
				// Fitting window drag to cargo: unload the charge and stop the online
				// effect (as bound.Add does), or the client keeps a ghost fit and the
				// charge is stranded in the emptied slot.
				prepFittedModuleForCargo(ctx, c, addr, accountID, charID, shipID, oldER, sqldb)
				oldOwnerID := oldER.OwnerID
				newOwnerID := takeOwnerFor(maSrc, charID, oldOwnerID)
				if _, err := sqldb.ExecContext(ctx,
					`UPDATE entity SET locationID=?, flag=5, ownerID=? WHERE itemID=?`,
					shipID, newOwnerID, maItemID); err != nil {
					log.Printf("[%s] bound.MultiAdd(cargo): move: %v", addr, err)
					continue
				}
				if newOwnerID != oldOwnerID {
					noteLootTake(ctx, sqldb, ds, maSrc, charID, oldOwnerID, oldER.LocationID)
				}
				if newER, _ := evedb.GetEntityRow(ctx, sqldb, maItemID); newER != nil {
					changeDict := itemMoveChange(newER, oldOwnerID, oldER.LocationID, oldER.Flag)
					_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
						evedb.BuildEntityRow(newER), changeDict,
					}})
				}
				log.Printf("[%s] bound.MultiAdd(cargo): moved item %d → cargo (ship %d)", addr, maItemID, shipID)
				maSrcLocs[oldER.LocationID] = true
				maMoved = true
				continue
			}
			// Charge loads arrive through bound.Add, never here; a charge must not
			// reach the module path.
			if oldER.CategoryID == 8 {
				log.Printf("[%s] bound.MultiAdd: charge %d with flag=%d — ignoring", addr, maItemID, maFlag)
				continue
			}
			if shipER != nil && shipER.GroupID == 29 {
				return server.SendCallException(c, pkt, accountID, server.BuildUserError(
					"CantFitToCapsule", nil))
			}
			// Skills do not gate the fit, and slot and hardpoint checks run before the
			// stack splits. See docs/main-s6-notes.md#fitting-order.
			targetFlag := maFlag
			if targetFlag != 0 {
				// Explicit slot: validate rack, range and occupancy; FitModuleToShip is a
				// bare UPDATE and would double-book a flag.
				slotType, stErr := evedb.GetModuleSlotType(ctx, sqldb, oldER.TypeID)
				if stErr != nil {
					log.Printf("[%s] bound.MultiAdd: type %d is not fittable (%v)", addr, oldER.TypeID, stErr)
					continue
				}
				wantRack := dogma.SlotTypeForFlag(targetFlag)
				if wantRack == "" || wantRack != slotType {
					log.Printf("[%s] bound.MultiAdd: module %d (%s-power) can't go in flag %d (%s rack)", addr, maItemID, slotType, targetFlag, wantRack)
					continue
				}
				var rackMax int64
				switch slotType {
				case "hi":
					rackMax = hiSlots
				case "med":
					rackMax = medSlots
				case "lo":
					rackMax = loSlots
				}
				if dogma.SlotIndexForFlag(targetFlag) >= rackMax {
					log.Printf("[%s] bound.MultiAdd: flag %d out of range — hull has %d %s slots", addr, targetFlag, rackMax, slotType)
					continue
				}
				if occ, _ := evedb.IsModuleSlotOccupied(ctx, sqldb, shipID, targetFlag); occ {
					log.Printf("[%s] bound.MultiAdd: slot flag %d already occupied on ship %d", addr, targetFlag, shipID)
					continue
				}
			}
			if targetFlag == 0 {
				// Auto-fit: the slot type comes from dgmTypeEffects.
				slotType, stErr := evedb.GetModuleSlotType(ctx, sqldb, oldER.TypeID)
				if stErr != nil {
					log.Printf("[%s] bound.MultiAdd: GetModuleSlotType(%d): %v", addr, oldER.TypeID, stErr)
					continue
				}
				var maxSlots int64
				switch slotType {
				case "hi":
					maxSlots = hiSlots
				case "med":
					maxSlots = medSlots
				case "lo":
					maxSlots = loSlots
				}
				freeSlot, fsErr := evedb.FindFreeSlot(ctx, sqldb, shipID, slotType, maxSlots)
				if fsErr != nil || freeSlot == 0 {
					log.Printf("[%s] bound.MultiAdd: no free %s slot on ship %d (max %d)", addr, slotType, shipID, maxSlots)
					if len(maItemIDs) == 1 {
						return server.SendCallException(c, pkt, accountID, server.BuildUserError(
							"NoFittingLocations", nil))
					}
					continue
				}
				targetFlag = freeSlot
			}
			isTurret, isLauncher, _ := evedb.IsModuleTurretOrLauncher(ctx, sqldb, oldER.TypeID)
			if isTurret || isLauncher {
				fittedTurrets, fittedLaunchers, _ := evedb.CountFittedTurretsLaunchers(ctx, sqldb, shipID)
				if isTurret && fittedTurrets >= int64(turretMax) {
					log.Printf("[%s] bound.MultiAdd: no turret hardpoints left on ship %d (%d/%d)", addr, shipID, fittedTurrets, int64(turretMax))
					if len(maItemIDs) == 1 {
						return server.SendCallException(c, pkt, accountID, server.BuildUserError(
							"NotEnoughTurretSlots", map[string]marshal.PyRep{
								"moduleName": marshal.PyString{Value: typeNameOrID(ctx, sqldb, oldER.TypeID)},
							}))
					}
					continue
				}
				if isLauncher && fittedLaunchers >= int64(launcherMax) {
					log.Printf("[%s] bound.MultiAdd: no launcher hardpoints left on ship %d (%d/%d)", addr, shipID, fittedLaunchers, int64(launcherMax))
					if len(maItemIDs) == 1 {
						return server.SendCallException(c, pkt, accountID, server.BuildUserError(
							"NotEnoughLauncherSlots", map[string]marshal.PyRep{
								"moduleName": marshal.PyString{Value: typeNameOrID(ctx, sqldb, oldER.TypeID)},
							}))
					}
					continue
				}
			}
			fitItemID := maItemID
			oldLocID := oldER.LocationID
			oldFlag := oldER.Flag
			if oldER.Quantity > 1 {
				newID, splitErr := evedb.SplitStack(ctx, sqldb, maItemID)
				if splitErr != nil {
					log.Printf("[%s] bound.MultiAdd: SplitStack(%d): %v", addr, maItemID, splitErr)
					continue
				}
				fitItemID = newID
				stackER, _ := evedb.GetEntityRow(ctx, sqldb, maItemID)
				if stackER != nil {
					stackChange := marshal.PyDict{Items: []marshal.DictEntry{
						{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(oldER.Quantity)}}, // old quantity
					}}
					stackNotif := marshal.PyTuple{Items: []marshal.PyRep{
						evedb.BuildEntityRow(stackER),
						stackChange,
					}}
					_ = server.SendNotification(c, accountID, "OnItemChange", "charid", stackNotif)
				}
			}
			if err := evedb.FitModuleToShip(ctx, sqldb, fitItemID, shipID, targetFlag); err != nil {
				log.Printf("[%s] bound.MultiAdd: FitModuleToShip(%d): %v", addr, fitItemID, err)
				continue
			}
			newER, _ := evedb.GetEntityRow(ctx, sqldb, fitItemID)
			if newER != nil {
				changeDict := marshal.PyDict{Items: []marshal.DictEntry{
					{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: uint64(oldLocID)}},
					{Key: marshal.PyInt{Value: 4}, Value: marshal.PyInt{Value: uint64(oldFlag)}},
				}}
				notifArgs := marshal.PyTuple{Items: []marshal.PyRep{
					evedb.BuildEntityRow(newER),
					changeDict,
				}}
				if nErr := server.SendNotification(c, accountID, "OnItemChange", "charid", notifArgs); nErr != nil {
					log.Printf("[%s] bound.MultiAdd OnItemChange: %v", addr, nErr)
				}
			}
			log.Printf("[%s] bound.MultiAdd: fitted item %d → ship %d flag %d", addr, fitItemID, shipID, targetFlag)
			maSrcLocs[oldLocID] = true
			maMoved = true
			// Auto-online runs fitOnlineRefusal; a refused module stays offline and
			// the pilot is told once per refusal kind. See docs/cmd-misc-notes.md#fit-online-refusal.
			if refKey, refDict := fitOnlineRefusal(ctx, sqldb, charID, shipID, fitItemID,
				oldER.TypeID, oldER.GroupID, cpuOutput, pgOutput); refKey != "" {
				log.Printf("[%s] bound.MultiAdd: skipping auto-online for item %d — %s %v", addr, fitItemID, refKey, refDict)
				_ = evedb.SetModuleOnlineState(ctx, sqldb, fitItemID, false)
				if !maTold[refKey] {
					maTold[refKey] = true
					pushRemoteMessage(c, accountID, refKey, refDict)
				}
			} else {
				if err := evedb.SetModuleOnlineState(ctx, sqldb, fitItemID, true); err != nil {
					log.Printf("[%s] bound.MultiAdd SetModuleOnlineState(%d): %v", addr, fitItemID, err)
				}
				sendGodmaShipEffect(c, accountID, fitItemID, oldER.TypeID, shipID, charID, 0, "online", true, 0, 0)
				sendModuleAttributeChange(c, accountID, charID, fitItemID, dogma.AttrIsOnline, 1, 0)
			}
		}
		for locID := range maSrcLocs {
			despawnLootContainerIfEmpty(ds, sqldb, locID)
		}
		sendShipFittingStats(c, accountID, charID, shipID, sqldb)
		pushWeaponAttrs(c, accountID, charID, shipID, sqldb, ds.activeEffectIDs())
		broadcastSlimItemChange(c, accountID, ds, shipID, sqldb)
		if !maMoved && maRefusal != "" {
			return corpUserErr(c, pkt, accountID, "You cannot move that item: "+maRefusal+".")
		}
	}
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// boundAdd handles the bound call "Add".
func boundAdd(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, stationID, sqldb, ds, sessReg := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.stationID, cc.sqldb, cc.ds, cc.sessReg
	// InventoryBound.Add(itemID[, quantity, flag]). On the hangar bind
	// (remoteObjectStr ends :3) it unfits; on the ship bind it fits.
	if len(cs.ArgTuple.Items) < 1 {
		log.Printf("[%s] bound.Add: missing args", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	addItemIDRep, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		log.Printf("[%s] bound.Add: arg[0] not int: %T", addr, cs.ArgTuple.Items[0])
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	addItemID := int64(addItemIDRep.Value)
	addQty, addFlag := invAddArgs(cs) // qty 0 means "all"
	addIsHangar := cs.RemoteObject == hangarBindID ||
		strings.HasSuffix(cs.RemoteObjectStr, fmt.Sprintf(":%d", hangarBindID))
	log.Printf("[%s] bound.Add itemID=%d qty=%d flag=%d isHangar=%v charID=%d bind=%d args=%v kwargs=%v",
		addr, addItemID, addQty, addFlag, addIsHangar, charID, bindNumFromCallStream(cs), cs.ArgTuple, cs.ArgDict)
	// Courier packages are sealed both ways: nothing comes out before delivery
	// and nothing goes in.
	if courierSealBlocks(sqldb, cs, []int64{addItemID}) {
		log.Printf("[%s] bound.Add: refused — courier package is sealed", addr)
		return sendNotInMissionPackage(c, pkt, accountID)
	}
	// POS structure bay or outpost platform hold: the bound object is a
	// structure in space, so the item is deposited, not fitted. Precedes the module paths.
	if bayID := bindNumFromCallStream(cs); isStructureBay(ds, bayID) {
		return handleStarbaseBayAdd(c, addr, pkt, accountID, charID, bayID, addItemID, addQty, addFlag, true, sqldb, ds)
	}
	// Opened dungeon container: a drop into it (dungeon_cargodrop.go).
	if contBind := bindNumFromCallStream(cs); isNPCCargo(ds, contBind) {
		refused, err := handleDunCargoAdd(c, addr, pkt, accountID, charID, contBind, addItemID, addQty, sqldb, ds)
		return sendNoneAfterDunCargoAdd(c, pkt, accountID, refused, err)
	}
	// Jetcan, kill can or launched container in space: a drop into it (jetcan.go).
	if contBind := bindNumFromCallStream(cs); !addIsHangar && isLootContainer(ds, contBind) {
		refused, err := handleLootCanAdd(c, addr, pkt, accountID, charID, contBind, addItemID, addQty, sqldb, ds)
		if refused {
			return err
		}
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	// Member hangar binds are view only (both client windows are oneWay).
	// See docs/corp-notes.md#member-hangars.
	if isMemberHangarBind(bindNumFromCallStream(cs)) {
		log.Printf("[%s] bound.Add: refused — member hangar is view only", addr)
		return corpUserErr(c, pkt, accountID, "That hangar is view only.")
	}
	// Rented office: deposit into a corp hangar division (bound by the office
	// itemID, see GetInventoryFromId).
	if !addIsHangar && sqldb != nil && charID != 0 && *stationID != 0 {
		if officeBind := bindNumFromCallStream(cs); officeBind >= 100000000 {
			if handled, err := handleOfficeAdd(c, addr, pkt, accountID, charID, *stationID,
				officeBind, addItemID, addQty, addFlag, true, sqldb, sessReg); handled {
				return err
			}
		}
	}
	// The pilot's own assembled container (alsc.go): a drop into it.
	if !addIsHangar && sqldb != nil && charID != 0 {
		if contBind := bindNumFromCallStream(cs); contBind >= 100000000 {
			if handled, err := handleContainerAdd(c, addr, pkt, accountID, charID, derefStation(stationID),
				contBind, addItemID, addQty, true, sqldb, ds); handled {
				return err
			}
		}
	}
	if sqldb != nil && charID != 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		oldER, err := evedb.GetEntityRow(ctx, sqldb, addItemID)
		if err != nil || oldER == nil {
			log.Printf("[%s] bound.Add: GetEntityRow(%d): %v", addr, addItemID, err)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		oldLocID := oldER.LocationID
		oldFlag := oldER.Flag
		// Taking out of a corp office to the personal hangar or the cargo hold
		// transfers ownership under the division take role. See docs/corp-notes.md#offices.
		if oldER.OwnerID != charID && *stationID != 0 && (addIsHangar || addFlag == 5) {
			if handled, err := handleOfficeTake(c, addr, pkt, accountID, charID, *stationID,
				oldER, addQty, addIsHangar, true, sqldb, sessReg); handled {
				return err
			}
		}
		// Refuse anything the caller could not have dragged from an open window.
		// See docs/cmd-misc-notes.md#item-source.
		if addFlag == 0 && !addIsHangar && lootTakeDefaultsToCargo(ctx, sqldb, ds, []int64{addItemID}) {
			addFlag = dogma.FlagCargo
		}
		addShipID, _ := evedb.GetCurrentShipID(ctx, sqldb, charID)
		addSrc, addWhy, addOK := authorizeItemMove(ctx, sqldb, ds, charID, addShipID, *stationID,
			oldER, itemDestFor(addIsHangar, addFlag))
		if !addOK {
			log.Printf("[%s] bound.Add: refused item %d (loc %d owner %d) for char %d: %s",
				addr, addItemID, oldLocID, oldER.OwnerID, charID, addWhy)
			return corpUserErr(c, pkt, accountID, "You cannot move that item: "+addWhy+".")
		}
		if addSrc == srcStructureBay && !structureTakeAllowed(ctx, addr, sqldb, ds, oldLocID, charID, oldFlag) {
			return corpUserErr(c, pkt, accountID, "You are not allowed to take items from that structure.")
		}
		if addIsHangar {
			if *stationID == 0 {
				log.Printf("[%s] bound.Add: no stationID for unfit", addr)
				return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
			}
			// Unfitting a module from a slot (flags 11-34): unload its charge first so
			// it is not orphaned.
			if oldFlag >= 11 && oldFlag <= 34 && oldER.CategoryID != 8 {
				if loaded, _ := evedb.GetLoadedCharge(ctx, sqldb, oldLocID, oldFlag); loaded != nil {
					chargeER, _ := evedb.GetEntityRow(ctx, sqldb, loaded.ItemID)
					if chargeER != nil {
						chOldLoc := chargeER.LocationID
						chOldFlag := chargeER.Flag
						shipID, _ := evedb.GetCurrentShipID(ctx, sqldb, charID)
						destLoc, destFlag := chargeUnloadDest(ctx, sqldb, chargeER, shipID, *stationID, charID)
						_ = evedb.UnloadChargeToCargoOrHangar(ctx, sqldb, loaded.ItemID, destLoc, destFlag)
						newChER, _ := evedb.GetEntityRow(ctx, sqldb, loaded.ItemID)
						if newChER != nil {
							chgChange := itemMoveChange(newChER, newChER.OwnerID, chOldLoc, chOldFlag)
							_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
								evedb.BuildEntityRow(newChER), chgChange,
							}})
						}
						log.Printf("[%s] bound.Add: auto-unloaded charge %d from slot %d before unfit", addr, loaded.ItemID, oldFlag)
					}
				}
			}
			moveQty := addQty
			if moveQty <= 0 || moveQty >= oldER.Quantity {
				if err := evedb.UnfitModuleToHangar(ctx, sqldb, addItemID, *stationID); err != nil {
					log.Printf("[%s] bound.Add: UnfitModuleToHangar(%d): %v", addr, addItemID, err)
					return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
				}
				unfitER, _ := evedb.GetEntityRow(ctx, sqldb, addItemID)
				if unfitER != nil {
					changeDict := marshal.PyDict{Items: []marshal.DictEntry{
						{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: uint64(oldLocID)}},
						{Key: marshal.PyInt{Value: 4}, Value: marshal.PyInt{Value: uint64(oldFlag)}},
					}}
					_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
						evedb.BuildEntityRow(unfitER),
						changeDict,
					}})
				}
				log.Printf("[%s] bound.Add: moved item %d → hangar station %d", addr, addItemID, *stationID)
				mergeLandedStack(ctx, c, addr, accountID, addItemID, sqldb)
			} else {
				oldQty := oldER.Quantity
				_ = evedb.UpdateCargoItemQty(ctx, sqldb, addItemID, oldER.Quantity-moveQty)
				srcER, _ := evedb.GetEntityRow(ctx, sqldb, addItemID)
				if srcER != nil {
					srcChange := marshal.PyDict{Items: []marshal.DictEntry{
						{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(oldQty)}},
					}}
					_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
						evedb.BuildEntityRow(srcER), srcChange,
					}})
				}
				newItemID, crErr := evedb.CreateCargoItem(ctx, sqldb, oldER.TypeID, charID, *stationID, moveQty)
				if crErr != nil {
					log.Printf("[%s] bound.Add: CreateCargoItem hangar split: %v", addr, crErr)
					return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
				}
				_, _ = sqldb.ExecContext(ctx,
					`UPDATE entity SET flag=4, singleton=0 WHERE itemID=?`, newItemID)
				newER, _ := evedb.GetEntityRow(ctx, sqldb, newItemID)
				if newER != nil {
					_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
						evedb.BuildEntityRow(newER), newItemChange(),
					}})
				}
				log.Printf("[%s] bound.Add: split %d of %d → hangar station %d", addr, moveQty, addItemID, *stationID)
				mergeSplitPiece(ctx, c, addr, accountID, newItemID, oldLocID, oldFlag, sqldb)
			}
		} else {
			// Destination is the ship: fit a module or load a charge.
			shipID, _ := evedb.GetCurrentShipID(ctx, sqldb, charID)
			if shipID == 0 {
				log.Printf("[%s] bound.Add: no current ship", addr)
				return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
			}

			// ----- Charge loading path -----
			if oldER.CategoryID == 8 && addFlag >= 11 && addFlag <= 34 {
				// A loaded charge changes the gun's range: re-send the folded weapon
				// attributes once the call has answered.
				defer pushWeaponAttrs(c, accountID, charID, shipID, sqldb, ds.activeEffectIDs())
				var modER *evedb.EntityRow
				if addFlag >= 11 && addFlag <= 34 {
					modER, _ = evedb.FindModuleInSlot(ctx, sqldb, shipID, addFlag)
				}
				if modER == nil {
					log.Printf("[%s] bound.Add: no module in slot flag %d for charge", addr, addFlag)
					return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
				}
				if err := evedb.ValidateChargeForModule(ctx, sqldb, oldER.TypeID, modER.TypeID); err != nil {
					log.Printf("[%s] bound.Add: charge validation failed: %v", addr, err)
					return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
				}
				if ds.reloadWait(modER.ItemID) > 0 {
					return clientMsgErr(c, pkt, accountID, "AlreadyBeingReloaded", map[string]marshal.PyRep{
						"item": marshal.PyString{Value: typeNameOrID(ctx, sqldb, modER.TypeID)}})
				}
				stopWeaponForReload(c, addr, accountID, charID, ds, modER.ItemID, sqldb)
				addReload := time.Duration(0)
				if g, _, gErr := evedb.GetTypeGroupCategory(ctx, sqldb, modER.TypeID); gErr == nil {
					addReload = reloadDelayFor(g)
				}
				modCap, _ := evedb.GetModuleCapacity(ctx, sqldb, modER.TypeID)
				chargeVol, _ := evedb.GetTypeVolume(ctx, sqldb, oldER.TypeID)
				maxQty := int64(1)
				if chargeVol > 0 {
					maxQty = int64(modCap / chargeVol)
				}
				if maxQty < 1 {
					maxQty = 1
				}
				// 0 or None requests the whole stack.
				reqQty := addQty
				if reqQty <= 0 || reqQty > oldER.Quantity {
					reqQty = oldER.Quantity
				}
				existingCharge, _ := evedb.GetLoadedCharge(ctx, sqldb, shipID, addFlag)
				if existingCharge != nil && existingCharge.TypeID != oldER.TypeID {
					// A different charge is loaded: the old one moves to cargo and the new
					// one loads in its place.
					exER, _ := evedb.GetEntityRow(ctx, sqldb, existingCharge.ItemID)
					_ = evedb.UnloadChargeToCargoOrHangar(ctx, sqldb, existingCharge.ItemID, shipID, 5)
					if exER != nil {
						if newExER, _ := evedb.GetEntityRow(ctx, sqldb, existingCharge.ItemID); newExER != nil {
							swapChange := itemMoveChange(newExER, newExER.OwnerID, exER.LocationID, exER.Flag)
							_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
								evedb.BuildEntityRow(newExER), swapChange,
							}})
						}
					}
					log.Printf("[%s] bound.Add: swapped out charge %d (type %d) → cargo before loading type %d",
						addr, existingCharge.ItemID, existingCharge.TypeID, oldER.TypeID)
					existingCharge = nil
				}
				room := maxQty
				if existingCharge != nil {
					room = maxQty - existingCharge.Quantity
				}
				if room < 0 {
					room = 0
				}
				if reqQty > room {
					// NotEnoughChargeSpace lets the client offer its partial-load dialog
					// (AskPartialCargoLoad) instead of a silent truncation.
					log.Printf("[%s] bound.Add: charge space short (room %d < req %d) in slot %d", addr, room, reqQty, addFlag)
					return server.SendCallException(c, pkt, accountID, server.BuildUserError(
						"NotEnoughChargeSpace", map[string]marshal.PyRep{
							"capacity": marshal.PyReal{Value: float64(room) * chargeVol},
							"volume":   marshal.PyReal{Value: float64(reqQty) * chargeVol},
						}))
				}
				if existingCharge != nil {
					loadQty := reqQty
					newChargeQty := existingCharge.Quantity + loadQty
					_ = evedb.UpdateCargoItemQty(ctx, sqldb, existingCharge.ItemID, newChargeQty)
					remaining := oldER.Quantity - loadQty
					if remaining <= 0 {
						_ = evedb.DeleteEntity(ctx, sqldb, addItemID)
						// A None change dict raises in the client's invCache before the
						// window hears of it; send a proper deletion (#20).
						delNotif := itemDeletedNotifArgs(oldER)
						_ = server.SendNotification(c, accountID, "OnItemChange", "charid", delNotif)
					} else {
						_ = evedb.UpdateCargoItemQty(ctx, sqldb, addItemID, remaining)
						srcER, _ := evedb.GetEntityRow(ctx, sqldb, addItemID)
						if srcER != nil {
							srcChange := marshal.PyDict{Items: []marshal.DictEntry{
								{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(oldER.Quantity)}},
							}}
							_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
								evedb.BuildEntityRow(srcER), srcChange,
							}})
						}
					}
					chargeER, _ := evedb.GetEntityRow(ctx, sqldb, existingCharge.ItemID)
					if chargeER != nil {
						chgChange := marshal.PyDict{Items: []marshal.DictEntry{
							{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(existingCharge.Quantity)}},
						}}
						_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
							evedb.BuildEntityRow(chargeER), chgChange,
						}})
					}
					log.Printf("[%s] bound.Add: added %d charges to existing stack in module %d", addr, loadQty, modER.ItemID)
					return answerChargeLoad(c, pkt, accountID, ds, *stationID, []int64{modER.ItemID}, addReload)
				}
				loadQty := reqQty
				remaining := oldER.Quantity - loadQty
				if remaining > 0 {
					_ = evedb.UpdateCargoItemQty(ctx, sqldb, addItemID, remaining)
					srcER, _ := evedb.GetEntityRow(ctx, sqldb, addItemID)
					if srcER != nil {
						srcChange := marshal.PyDict{Items: []marshal.DictEntry{
							{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(oldER.Quantity)}},
						}}
						_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
							evedb.BuildEntityRow(srcER), srcChange,
						}})
					}
					chargeItemID, crErr := evedb.CreateCargoItem(ctx, sqldb, oldER.TypeID, charID, modER.ItemID, loadQty)
					if crErr != nil {
						log.Printf("[%s] bound.Add: CreateCargoItem charge: %v", addr, crErr)
						return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
					}
					_ = evedb.LoadChargeIntoModule(ctx, sqldb, chargeItemID, shipID, addFlag, loadQty)
					chargeER, _ := evedb.GetEntityRow(ctx, sqldb, chargeItemID)
					if chargeER != nil {
						_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
							evedb.BuildEntityRow(chargeER),
							marshal.PyDict{},
						}})
					}
				} else {
					oldLocID := oldER.LocationID
					oldFlag := oldER.Flag
					_ = evedb.LoadChargeIntoModule(ctx, sqldb, addItemID, shipID, addFlag, loadQty)
					chargeER, _ := evedb.GetEntityRow(ctx, sqldb, addItemID)
					if chargeER != nil {
						chgChange := marshal.PyDict{Items: []marshal.DictEntry{
							{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: uint64(oldLocID)}},
							{Key: marshal.PyInt{Value: 4}, Value: marshal.PyInt{Value: uint64(oldFlag)}},
							{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(oldER.Quantity)}},
						}}
						_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
							evedb.BuildEntityRow(chargeER), chgChange,
						}})
					}
				}
				log.Printf("[%s] bound.Add: loaded %d charges of type %d into module %d (slot %d)", addr, loadQty, oldER.TypeID, modER.ItemID, addFlag)
				return answerChargeLoad(c, pkt, accountID, ds, *stationID, []int64{modER.ItemID}, addReload)
			}
			// ----- End charge loading path -----

			// ----- Cargo move path -----
			if addFlag == 5 {
				if oldFlag >= 11 && oldFlag <= 34 && oldER.CategoryID == 8 {
					if gunER, _ := evedb.FindModuleInSlot(ctx, sqldb, oldLocID, oldFlag); gunER != nil {
						if refused, err := refuseBusyCharge(ctx, c, pkt, accountID, ds, gunER, sqldb); refused {
							return err
						}
					}
					// Unloading a gun returns its range to the empty value.
					defer pushWeaponAttrs(c, accountID, charID, oldLocID, sqldb, ds.activeEffectIDs())
				}
				moveQty := addQty
				if moveQty <= 0 || moveQty > oldER.Quantity {
					moveQty = oldER.Quantity
				}
				srcIsFittedModule := isFittedModule(oldER)
				// Space check runs before anything changes, so a refused unfit leaves the
				// module online with its charge loaded. A fitted module's charge lands in
				// cargo too.
				shipER, _ := evedb.GetEntityRow(ctx, sqldb, shipID)
				if shipER != nil {
					cargoCap, _ := evedb.GetShipCargoCapacityFitted(ctx, sqldb, shipER.TypeID, shipID)
					usedVol, _ := evedb.GetCargoUsedVolume(ctx, sqldb, charID, shipID)
					itemVol := evedb.ItemVolume(ctx, sqldb, oldER.ItemID, oldER.TypeID)
					totalNeeded := itemVol*float64(moveQty) + loadedChargeVolume(ctx, sqldb, oldER)
					freeSpace := cargoCap - usedVol
					if totalNeeded > freeSpace {
						log.Printf("[%s] bound.Add: cargo full (used %.1f + needed %.1f > cap %.1f)", addr, usedVol, totalNeeded, cargoCap)
						return server.SendCallException(c, pkt, accountID, server.BuildUserError(
							"NotEnoughCargoSpace", map[string]marshal.PyRep{
								"volume":    marshal.PyReal{Value: totalNeeded},
								"available": marshal.PyReal{Value: freeSpace},
							}))
					}
				}
				// Unfitting a module into cargo: unload its charge and stop its online
				// effect, or the client keeps a ghost fit in the slot.
				if srcIsFittedModule {
					prepFittedModuleForCargo(ctx, c, addr, accountID, charID, shipID, oldER, sqldb)
				}
				oldLocID := oldER.LocationID
				oldFlag := oldER.Flag
				oldQty := oldER.Quantity
				if moveQty < oldER.Quantity {
					_ = evedb.UpdateCargoItemQty(ctx, sqldb, addItemID, oldER.Quantity-moveQty)
					srcER, _ := evedb.GetEntityRow(ctx, sqldb, addItemID)
					if srcER != nil {
						srcChange := marshal.PyDict{Items: []marshal.DictEntry{
							{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(oldQty)}},
						}}
						_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
							evedb.BuildEntityRow(srcER), srcChange,
						}})
					}
					newItemID, crErr := evedb.CreateCargoItem(ctx, sqldb, oldER.TypeID, charID, shipID, moveQty)
					if crErr != nil {
						log.Printf("[%s] bound.Add: CreateCargoItem split: %v", addr, crErr)
						return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
					}
					newER, _ := evedb.GetEntityRow(ctx, sqldb, newItemID)
					if newER != nil {
						_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
							evedb.BuildEntityRow(newER), newItemChange(),
						}})
					}
					if oldER.OwnerID != charID {
						noteLootTake(ctx, sqldb, ds, addSrc, charID, oldER.OwnerID, oldLocID)
					}
					log.Printf("[%s] bound.Add: split %d of %d → cargo (ship %d)", addr, moveQty, addItemID, shipID)
					mergeSplitPiece(ctx, c, addr, accountID, newItemID, oldLocID, oldFlag, sqldb)
				} else {
					// Full move. Loot and structure-bay rows carry another owner; take
					// ownership or the cargo List (filtered by ownerID) hides the row.
					oldOwnerID := oldER.OwnerID
					newOwnerID := takeOwnerFor(addSrc, charID, oldOwnerID)
					_, err := sqldb.ExecContext(ctx,
						`UPDATE entity SET locationID=?, flag=5, ownerID=? WHERE itemID=?`,
						shipID, newOwnerID, addItemID)
					if err != nil {
						log.Printf("[%s] bound.Add: move to cargo: %v", addr, err)
						return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
					}
					if newOwnerID != oldOwnerID {
						noteLootTake(ctx, sqldb, ds, addSrc, charID, oldOwnerID, oldLocID)
					}
					newER, _ := evedb.GetEntityRow(ctx, sqldb, addItemID)
					if newER != nil {
						changeDict := itemMoveChange(newER, oldOwnerID, oldLocID, oldFlag)
						_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
							evedb.BuildEntityRow(newER), changeDict,
						}})
					}
					log.Printf("[%s] bound.Add: moved item %d → cargo (ship %d)", addr, addItemID, shipID)
					mergeLandedStack(ctx, c, addr, accountID, addItemID, sqldb)
					despawnLootContainerIfEmpty(ds, sqldb, oldLocID)
				}
				if srcIsFittedModule {
					// The slot emptied and the CPU/PG load changed: push the fitting stats
					// and refresh the slim item.
					sendShipFittingStats(c, accountID, charID, shipID, sqldb)
					pushWeaponAttrs(c, accountID, charID, shipID, sqldb, ds.activeEffectIDs())
					broadcastSlimItemChange(c, accountID, ds, shipID, sqldb)
				}
				return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
			}
			// ----- End cargo move path -----

			// ----- Drone bay path -----
			// Flag 87 moves drones (whole stack or addQty) into the drone bay; the
			// module path would force singleton=1 and quantity=1.
			if addFlag == 87 {
				if oldER.CategoryID != 18 {
					log.Printf("[%s] bound.Add: item %d (category %d) is not a drone — ignoring drone-bay drop", addr, addItemID, oldER.CategoryID)
					return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
				}
				moveQty := addQty
				if moveQty <= 0 || moveQty > oldER.Quantity {
					moveQty = oldER.Quantity
				}
				// Volume check against the hull's droneCapacity (attr 283).
				shipER, _ := evedb.GetEntityRow(ctx, sqldb, shipID)
				if shipER != nil {
					hullAttrs, _ := evedb.GetTypeAttributes(ctx, sqldb, shipER.TypeID)
					droneCap := hullAttrs[dogma.AttrDroneCapacity]
					usedVol, _ := evedb.GetDroneBayUsedVolume(ctx, sqldb, shipID)
					itemVol, _ := evedb.GetTypeVolume(ctx, sqldb, oldER.TypeID)
					if itemVol*float64(moveQty) > droneCap-usedVol {
						log.Printf("[%s] bound.Add: drone bay full (used %.1f + needed %.1f > cap %.1f)", addr, usedVol, itemVol*float64(moveQty), droneCap)
						return server.SendCallException(c, pkt, accountID, server.BuildUserError(
							"NotEnoughDroneBaySpace", map[string]marshal.PyRep{
								"volume":    marshal.PyReal{Value: itemVol * float64(moveQty)},
								"available": marshal.PyReal{Value: droneCap - usedVol},
							}))
					}
				}
				oldQty := oldER.Quantity
				if moveQty < oldER.Quantity {
					_ = evedb.UpdateCargoItemQty(ctx, sqldb, addItemID, oldER.Quantity-moveQty)
					srcER, _ := evedb.GetEntityRow(ctx, sqldb, addItemID)
					if srcER != nil {
						srcChange := marshal.PyDict{Items: []marshal.DictEntry{
							{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(oldQty)}},
						}}
						_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
							evedb.BuildEntityRow(srcER), srcChange,
						}})
					}
					newItemID, crErr := evedb.CreateCargoItem(ctx, sqldb, oldER.TypeID, charID, shipID, moveQty)
					if crErr != nil {
						log.Printf("[%s] bound.Add: CreateCargoItem drone split: %v", addr, crErr)
						return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
					}
					_, _ = sqldb.ExecContext(ctx, `UPDATE entity SET flag=87 WHERE itemID=?`, newItemID)
					if newER, _ := evedb.GetEntityRow(ctx, sqldb, newItemID); newER != nil {
						_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
							evedb.BuildEntityRow(newER), newItemChange(),
						}})
					}
					log.Printf("[%s] bound.Add: split %d of %d → drone bay (ship %d)", addr, moveQty, addItemID, shipID)
					mergeSplitPiece(ctx, c, addr, accountID, newItemID, oldLocID, oldFlag, sqldb)
				} else {
					if err := evedb.MoveStackToDroneBay(ctx, sqldb, addItemID, shipID); err != nil {
						log.Printf("[%s] bound.Add: MoveStackToDroneBay(%d): %v", addr, addItemID, err)
						return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
					}
					if newOwner := takeOwnerFor(addSrc, charID, oldER.OwnerID); newOwner != oldER.OwnerID {
						_ = evedb.SetEntityOwner(ctx, sqldb, addItemID, newOwner)
						noteLootTake(ctx, sqldb, ds, addSrc, charID, oldER.OwnerID, oldLocID)
					}
					if newER, _ := evedb.GetEntityRow(ctx, sqldb, addItemID); newER != nil {
						changeDict := marshal.PyDict{Items: []marshal.DictEntry{
							{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: uint64(oldLocID)}},
							{Key: marshal.PyInt{Value: 4}, Value: marshal.PyInt{Value: uint64(oldFlag)}},
						}}
						_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
							evedb.BuildEntityRow(newER), changeDict,
						}})
					}
					log.Printf("[%s] bound.Add: moved item %d (qty %d) → drone bay (ship %d)", addr, addItemID, oldQty, shipID)
					mergeLandedStack(ctx, c, addr, accountID, addItemID, sqldb)
					despawnLootContainerIfEmpty(ds, sqldb, oldLocID)
				}
				return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
			}
			// ----- End drone bay path -----

			// A charge with flagAutoFit (0) loads into every compatible fitted module
			// until the stack runs out.
			if oldER.CategoryID == 8 && addFlag == 0 {
				mods, _ := evedb.GetFittedModules(ctx, sqldb, shipID)
				chargeVol, _ := evedb.GetTypeVolume(ctx, sqldb, oldER.TypeID)
				srcQty := oldER.Quantity
				loadedAny := false
				srcMoved := false
				// Modules that took a charge, and the longest reload among them.
				var fitReloadMods []int64
				fitReload := time.Duration(0)
				for _, mod := range mods {
					if srcQty <= 0 {
						break
					}
					if evedb.ValidateChargeForModule(ctx, sqldb, oldER.TypeID, mod.TypeID) != nil {
						continue
					}
					if ds.reloadWait(mod.ItemID) > 0 || ds.weaponCycling(mod.ItemID) {
						continue // auto-fit never interrupts a reload or a firing weapon
					}
					noteFitReload := func() {
						fitReloadMods = append(fitReloadMods, mod.ItemID)
						if g, _, gErr := evedb.GetTypeGroupCategory(ctx, sqldb, mod.TypeID); gErr == nil {
							if d := reloadDelayFor(g); d > fitReload {
								fitReload = d
							}
						}
					}
					modCap, _ := evedb.GetModuleCapacity(ctx, sqldb, mod.TypeID)
					if modCap <= 0 {
						continue // no charge bay
					}
					maxQty := int64(1)
					if chargeVol > 0 {
						maxQty = int64(modCap / chargeVol)
					}
					if maxQty < 1 {
						maxQty = 1
					}
					existing, _ := evedb.GetLoadedCharge(ctx, sqldb, shipID, mod.Flag)
					room := maxQty
					if existing != nil {
						if existing.TypeID != oldER.TypeID {
							continue // a different ammo is loaded; auto-fit leaves it
						}
						room = maxQty - existing.Quantity
					}
					if room <= 0 {
						continue
					}
					q := srcQty
					if q > room {
						q = room
					}
					if existing != nil {
						_ = evedb.UpdateCargoItemQty(ctx, sqldb, existing.ItemID, existing.Quantity+q)
						if chER, _ := evedb.GetEntityRow(ctx, sqldb, existing.ItemID); chER != nil {
							chgChange := marshal.PyDict{Items: []marshal.DictEntry{
								{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(existing.Quantity)}},
							}}
							_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
								evedb.BuildEntityRow(chER), chgChange,
							}})
						}
					} else if q < srcQty {
						newChargeID, crErr := evedb.CreateCargoItem(ctx, sqldb, oldER.TypeID, charID, mod.ItemID, q)
						if crErr != nil {
							log.Printf("[%s] bound.Add(autofit charge): CreateCargoItem: %v", addr, crErr)
							break
						}
						_ = evedb.LoadChargeIntoModule(ctx, sqldb, newChargeID, shipID, mod.Flag, q)
						if chER, _ := evedb.GetEntityRow(ctx, sqldb, newChargeID); chER != nil {
							_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
								evedb.BuildEntityRow(chER), marshal.PyDict{},
							}})
						}
					} else {
						_ = evedb.LoadChargeIntoModule(ctx, sqldb, addItemID, shipID, mod.Flag, q)
						if chER, _ := evedb.GetEntityRow(ctx, sqldb, addItemID); chER != nil {
							chgChange := marshal.PyDict{Items: []marshal.DictEntry{
								{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: uint64(oldLocID)}},
								{Key: marshal.PyInt{Value: 4}, Value: marshal.PyInt{Value: uint64(oldFlag)}},
							}}
							_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
								evedb.BuildEntityRow(chER), chgChange,
							}})
						}
						srcQty = 0
						srcMoved = true
						loadedAny = true
						noteFitReload()
						log.Printf("[%s] bound.Add(autofit charge): loaded %d of type %d into module %d (slot %d)", addr, q, oldER.TypeID, mod.ItemID, mod.Flag)
						break
					}
					srcQty -= q
					loadedAny = true
					noteFitReload()
					log.Printf("[%s] bound.Add(autofit charge): loaded %d of type %d into module %d (slot %d)", addr, q, oldER.TypeID, mod.ItemID, mod.Flag)
				}
				if loadedAny && !srcMoved && srcQty != oldER.Quantity {
					if srcQty > 0 {
						_ = evedb.UpdateCargoItemQty(ctx, sqldb, addItemID, srcQty)
						if srcER, _ := evedb.GetEntityRow(ctx, sqldb, addItemID); srcER != nil {
							srcChange := marshal.PyDict{Items: []marshal.DictEntry{
								{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(oldER.Quantity)}},
							}}
							_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
								evedb.BuildEntityRow(srcER), srcChange,
							}})
						}
					} else {
						_ = evedb.DeleteEntity(ctx, sqldb, addItemID)
						_ = server.SendNotification(c, accountID, "OnItemChange", "charid", itemDeletedNotifArgs(oldER))
					}
				}
				if loadedAny {
					defer pushWeaponAttrs(c, accountID, charID, shipID, sqldb, ds.activeEffectIDs())
				}
				if !loadedAny {
					log.Printf("[%s] bound.Add: no compatible module with room for charge %d (type %d)", addr, addItemID, oldER.TypeID)
				}
				return answerChargeLoad(c, pkt, accountID, ds, *stationID, fitReloadMods, fitReload)
			}
			if oldER.CategoryID == 8 {
				log.Printf("[%s] bound.Add: charge item %d with flag=%d — not a module slot, ignoring", addr, addItemID, addFlag)
				return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
			}

			// Reject unfittable types before splitting the stack, or a failed fit
			// strands a single-unit row in cargo.
			if addFlag == 0 {
				if _, stErr := evedb.GetModuleSlotType(ctx, sqldb, oldER.TypeID); stErr != nil {
					log.Printf("[%s] bound.Add: type %d is not fittable (%v) — ignoring", addr, oldER.TypeID, stErr)
					return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
				}
			}

			if capsER, _ := evedb.GetEntityRow(ctx, sqldb, shipID); capsER != nil && capsER.GroupID == 29 {
				return server.SendCallException(c, pkt, accountID, server.BuildUserError(
					"CantFitToCapsule", nil))
			}
			// Skills do not gate the fit (see bound.MultiAdd).

			if oldER.Quantity > 1 {
				newID, splitErr := evedb.SplitStack(ctx, sqldb, addItemID)
				if splitErr != nil {
					log.Printf("[%s] bound.Add: SplitStack(%d): %v", addr, addItemID, splitErr)
					return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
				}
				stackER, _ := evedb.GetEntityRow(ctx, sqldb, addItemID)
				if stackER != nil {
					stackChange := marshal.PyDict{Items: []marshal.DictEntry{
						{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(oldER.Quantity)}}, // old quantity
					}}
					stackNotif := marshal.PyTuple{Items: []marshal.PyRep{
						evedb.BuildEntityRow(stackER),
						stackChange,
					}}
					_ = server.SendNotification(c, accountID, "OnItemChange", "charid", stackNotif)
				}
				addItemID = newID
				oldER, err = evedb.GetEntityRow(ctx, sqldb, addItemID)
				if err != nil || oldER == nil {
					log.Printf("[%s] bound.Add: GetEntityRow(split %d): %v", addr, addItemID, err)
					return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
				}
				oldLocID = oldER.LocationID
				oldFlag = oldER.Flag
			}
			// addAutoOnlined records a successful auto-online so the effect
			// notification follows OnItemChange.
			var addAutoOnlined bool
			addShipER, _ := evedb.GetEntityRow(ctx, sqldb, shipID)
			var addHiSlots, addMedSlots, addLoSlots int64
			var addCpuOutput, addPgOutput float64
			var addTurretMax, addLauncherMax float64
			if addShipER != nil {
				addHiSlots, addMedSlots, addLoSlots, _ = evedb.GetShipHullSlots(ctx, sqldb, addShipER.TypeID)
				addCpuOutput, addPgOutput, _ = evedb.GetShipCPUPGCapacityFitted(ctx, sqldb, addShipER.TypeID, shipID)
				addHullAttrs, _ := evedb.GetTypeAttributes(ctx, sqldb, addShipER.TypeID)
				if addHullAttrs != nil {
					addTurretMax = addHullAttrs[dogma.AttrTurretSlotsLeft]
					addLauncherMax = addHullAttrs[dogma.AttrLauncherSlotsLeft]
				}
			}
			targetFlag := addFlag
			if targetFlag != 0 {
				// Explicit slot: validate rack, range and occupancy before FitModuleToShip,
				// a bare UPDATE, double-books the flag.
				slotType, stErr := evedb.GetModuleSlotType(ctx, sqldb, oldER.TypeID)
				if stErr != nil {
					log.Printf("[%s] bound.Add: type %d is not fittable (%v)", addr, oldER.TypeID, stErr)
					return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
				}
				wantRack := dogma.SlotTypeForFlag(targetFlag)
				if wantRack == "" || wantRack != slotType {
					log.Printf("[%s] bound.Add: module %d (%s-power) can't go in flag %d (%s rack)", addr, addItemID, slotType, targetFlag, wantRack)
					return server.SendCallException(c, pkt, accountID, server.BuildUserError(
						"ItemDoesntFitPower", map[string]marshal.PyRep{
							"item":      marshal.PyString{Value: typeNameOrID(ctx, sqldb, oldER.TypeID)},
							"slotpower": marshal.PyString{Value: wantRack + "Power"},
							"itempower": marshal.PyString{Value: slotType + "Power"},
						}))
				}
				var rackMax int64
				switch slotType {
				case "hi":
					rackMax = addHiSlots
				case "med":
					rackMax = addMedSlots
				case "lo":
					rackMax = addLoSlots
				}
				if dogma.SlotIndexForFlag(targetFlag) >= rackMax {
					log.Printf("[%s] bound.Add: flag %d out of range — hull has %d %s slots", addr, targetFlag, rackMax, slotType)
					return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
				}
				if occ, _ := evedb.IsModuleSlotOccupied(ctx, sqldb, shipID, targetFlag); occ {
					log.Printf("[%s] bound.Add: slot flag %d already occupied on ship %d", addr, targetFlag, shipID)
					return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
				}
			}
			if targetFlag == 0 {
				slotType, stErr := evedb.GetModuleSlotType(ctx, sqldb, oldER.TypeID)
				if stErr != nil {
					log.Printf("[%s] bound.Add: GetModuleSlotType(%d): %v", addr, oldER.TypeID, stErr)
					return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
				}
				var addMaxSlots int64
				switch slotType {
				case "hi":
					addMaxSlots = addHiSlots
				case "med":
					addMaxSlots = addMedSlots
				case "lo":
					addMaxSlots = addLoSlots
				}
				freeSlot, fsErr := evedb.FindFreeSlot(ctx, sqldb, shipID, slotType, addMaxSlots)
				if fsErr != nil || freeSlot == 0 {
					log.Printf("[%s] bound.Add: no free %s slot on ship %d (max %d)", addr, slotType, shipID, addMaxSlots)
					return server.SendCallException(c, pkt, accountID, server.BuildUserError(
						"NoFittingLocations", nil))
				}
				targetFlag = freeSlot
			}
			addIsTurret, addIsLauncher, _ := evedb.IsModuleTurretOrLauncher(ctx, sqldb, oldER.TypeID)
			if addIsTurret || addIsLauncher {
				addFittedTurrets, addFittedLaunchers, _ := evedb.CountFittedTurretsLaunchers(ctx, sqldb, shipID)
				if addIsTurret && addFittedTurrets >= int64(addTurretMax) {
					log.Printf("[%s] bound.Add: no turret hardpoints left on ship %d (%d/%d)", addr, shipID, addFittedTurrets, int64(addTurretMax))
					return server.SendCallException(c, pkt, accountID, server.BuildUserError(
						"NotEnoughTurretSlots", map[string]marshal.PyRep{
							"moduleName": marshal.PyString{Value: typeNameOrID(ctx, sqldb, oldER.TypeID)},
						}))
				}
				if addIsLauncher && addFittedLaunchers >= int64(addLauncherMax) {
					log.Printf("[%s] bound.Add: no launcher hardpoints left on ship %d (%d/%d)", addr, shipID, addFittedLaunchers, int64(addLauncherMax))
					return server.SendCallException(c, pkt, accountID, server.BuildUserError(
						"NotEnoughLauncherSlots", map[string]marshal.PyRep{
							"moduleName": marshal.PyString{Value: typeNameOrID(ctx, sqldb, oldER.TypeID)},
						}))
				}
			}
			if err := evedb.FitModuleToShip(ctx, sqldb, addItemID, shipID, targetFlag); err != nil {
				log.Printf("[%s] bound.Add: FitModuleToShip(%d): %v", addr, addItemID, err)
				return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
			}
			log.Printf("[%s] bound.Add: fitted item %d → ship %d flag %d", addr, addItemID, shipID, targetFlag)
			// Auto-online: same gate and notice as bound.MultiAdd.
			if refKey, refDict := fitOnlineRefusal(ctx, sqldb, charID, shipID, addItemID,
				oldER.TypeID, oldER.GroupID, addCpuOutput, addPgOutput); refKey != "" {
				log.Printf("[%s] bound.Add: skipping auto-online for item %d — %s %v", addr, addItemID, refKey, refDict)
				_ = evedb.SetModuleOnlineState(ctx, sqldb, addItemID, false)
				pushRemoteMessage(c, accountID, refKey, refDict)
			} else {
				if err := evedb.SetModuleOnlineState(ctx, sqldb, addItemID, true); err != nil {
					log.Printf("[%s] bound.Add SetModuleOnlineState(%d): %v", addr, addItemID, err)
				}
				addAutoOnlined = true
			}
			// OnItemChange goes before OnGodmaShipEffect so the client knows the item
			// before its online effect arrives.
			newER, _ := evedb.GetEntityRow(ctx, sqldb, addItemID)
			if newER != nil {
				changeDict := marshal.PyDict{Items: []marshal.DictEntry{
					{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: uint64(oldLocID)}},
					{Key: marshal.PyInt{Value: 4}, Value: marshal.PyInt{Value: uint64(oldFlag)}},
				}}
				notifArgs := marshal.PyTuple{Items: []marshal.PyRep{
					evedb.BuildEntityRow(newER),
					changeDict,
				}}
				if nErr := server.SendNotification(c, accountID, "OnItemChange", "charid", notifArgs); nErr != nil {
					log.Printf("[%s] bound.Add OnItemChange: %v", addr, nErr)
				}
			}
			if addAutoOnlined {
				sendGodmaShipEffect(c, accountID, addItemID, oldER.TypeID, shipID, charID, 0, "online", true, 0, 0)
				sendModuleAttributeChange(c, accountID, charID, addItemID, dogma.AttrIsOnline, 1, 0)
			}
		}
		despawnLootContainerIfEmpty(ds, sqldb, oldLocID)
		addShipID, _ = evedb.GetCurrentShipID(ctx, sqldb, charID)
		sendShipFittingStats(c, accountID, charID, addShipID, sqldb)
		pushWeaponAttrs(c, accountID, charID, addShipID, sqldb, ds.activeEffectIDs())
		broadcastSlimItemChange(c, accountID, ds, ds.shipID, sqldb)
	}
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}
