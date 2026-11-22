package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/dogma"
	"gurimoon/marshal"
	_ "gurimoon/modules"
	evenet "gurimoon/net"
	"gurimoon/server"
)

// boundListStations handles the bound call "ListStations".
func boundListStations(cc *callCtx) error {
	c, addr, pkt, accountID, charID, sqldb := cc.c, cc.addr, cc.pkt, cc.accountID, cc.charID, cc.sqldb
	// invbroker/assetSvc bound: ListStations() → util.Rowset{stationID, itemCount}.
	log.Printf("[%s] bound.ListStations charID=%d sqldb=%v", addr, charID, sqldb != nil)
	var result marshal.PyRep = makeInvRowset([]string{"stationID", "itemCount"}, nil)
	if charID != 0 && sqldb != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if r, err := evedb.GetCharStationsWithItems(ctx, sqldb, charID); err != nil {
			log.Printf("[%s] bound.ListStations db ERROR: %v", addr, err)
		} else {
			result = r
		}
	} else {
		log.Printf("[%s] bound.ListStations SKIPPED: charID=%d sqldb=%v", addr, charID, sqldb != nil)
	}
	log.Printf("[%s] → bound.ListStations done", addr)
	return server.SendCallRsp(c, pkt, accountID, result)
}

// boundListStationBlueprintItems handles the bound call "ListStationBlueprintItems".
func boundListStationBlueprintItems(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, sqldb := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.sqldb
	// S&I blueprint picker: returns blueprint items at stationID with
	// blueprint-specific columns (copy, materialLevel, etc.).
	var lsbStationID int64
	if len(cs.ArgTuple.Items) >= 1 {
		if pt, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
			lsbStationID = int64(pt.Value)
		}
	}
	bpCols := []string{
		"itemID", "typeID", "ownerID", "locationID", "flag",
		"contraband", "singleton", "quantity", "groupID", "categoryID", "customInfo",
		"copy", "materialLevel", "productivityLevel", "licensedProductionRunsRemaining",
		"blueprintID",
	}
	var bpResult marshal.PyRep = makeInvRowset(bpCols, nil)
	if charID != 0 && lsbStationID != 0 && sqldb != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// ListStationBlueprintItems(arrayID, isCorp=1): the corp
		// blueprints in a starbase array's divisions the caller may see.
		if inst, isPOS, _ := evedb.GetPOSInstallation(ctx, sqldb, lsbStationID); isPOS {
			var flags []int64
			if boundCharCorpID(charID, sqldb) == inst.OwnerCorpID {
				flags = corpHangarQueryFlags(ctx, sqldb, inst.OwnerCorpID, charID)
			}
			if len(flags) > 0 {
				if r, err := evedb.GetOwnerBlueprintItemsAt(ctx, sqldb, inst.OwnerCorpID, lsbStationID, flags); err == nil {
					bpResult = r
				}
			}
		} else if r, err := evedb.GetCharStationBlueprintItems(ctx, sqldb, charID, lsbStationID); err != nil {
			log.Printf("[%s] bound.ListStationBlueprintItems db: %v", addr, err)
		} else {
			bpResult = r
		}
	}
	log.Printf("[%s] → bound.ListStationBlueprintItems charID=%d stationID=%d", addr, charID, lsbStationID)
	return server.SendCallRsp(c, pkt, accountID, bpResult)
}

// boundListStationItems handles the bound call "ListStationItems".
func boundListStationItems(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, sqldb := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.sqldb
	// assetSvc bound: ListStationItems(stationID) -> util.Rowset of hangar items
	// (flag=4). None crashes assetsinfo.GetSubContent (iteration over non-sequence), so
	// an empty rowset is the minimum.
	var lsiStationID int64
	if len(cs.ArgTuple.Items) >= 1 {
		if pt, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
			lsiStationID = int64(pt.Value)
		}
	}
	assetCols := []string{
		"itemID", "typeID", "ownerID", "locationID", "flag",
		"contraband", "singleton", "quantity", "groupID", "categoryID", "customInfo",
	}
	var result marshal.PyRep = makeInvRowset(assetCols, nil)
	if charID != 0 && lsiStationID != 0 && sqldb != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if r, err := evedb.GetCharHangarItems(ctx, sqldb, charID, lsiStationID); err != nil {
			log.Printf("[%s] bound.ListStationItems db: %v", addr, err)
		} else {
			result = r
		}
	}
	log.Printf("[%s] → bound.ListStationItems charID=%d stationID=%d", addr, charID, lsiStationID)
	return server.SendCallRsp(c, pkt, accountID, result)
}

// boundGetInventory handles the bound call "GetInventory".
func boundGetInventory(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, stationID, sqldb := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.stationID, cc.sqldb
	// invbroker bound: GetInventory(containerType) -> InventoryBound. containerHangar and
	// containerGlobal (the S&I blueprint picker) use hangarBindID so List serves hangar items.
	bindNum := uint64(2)
	log.Printf("[%s] bound.GetInventory args=%+v", addr, cs.ArgTuple.Items)
	if len(cs.ArgTuple.Items) >= 1 {
		if pt, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
			cv := int64(pt.Value)
			if cv == containerHangar || cv == containerGlobal {
				bindNum = hangarBindID
			}
			// param1 is an ownerID for containerHangar; see the inline path in computeInlineBindResult.
			if cv == containerHangar && len(cs.ArgTuple.Items) >= 2 {
				if ow, ok := cs.ArgTuple.Items[1].(marshal.PyInt); ok {
					if b := memberHangarBindNum(addr, charID, int64(ow.Value), *stationID, sqldb); b != 0 {
						bindNum = uint64(b)
					}
				}
			}
		}
	}
	bindSpec := fmt.Sprintf("N=%d:%d", globalNodeID, bindNum)
	boundObj := marshal.PySubStruct{Value: &marshal.PySubStream{
		Decoded: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyString{Value: bindSpec},
			marshal.PyInt{Value: 0},
		}},
	}}
	log.Printf("[%s] → bound.GetInventory container=%v → inventoryBound %q", addr, cs.ArgTuple.Items, bindSpec)
	return server.SendCallRsp(c, pkt, accountID, boundObj)
}

// boundList handles the bound call "List".
func boundList(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, stationID, sqldb, ds := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.stationID, cc.sqldb, cc.ds
	// InventoryBound.List(): the client sends remoteObject as the full bind-spec string
	// ("N=888444:3"), so cs.RemoteObject is 0 and cs.RemoteObjectStr is checked too.
	isHangar := cs.RemoteObject == hangarBindID ||
		strings.HasSuffix(cs.RemoteObjectStr, fmt.Sprintf(":%d", hangarBindID))
	isCargo := cs.RemoteObject == cargoBindID ||
		strings.HasSuffix(cs.RemoteObjectStr, fmt.Sprintf(":%d", cargoBindID))
	// Someone else's hangar floor (MEMBER HANGARS / ALL HANGARS) binds by the target
	// ownerID. Checked before the office branch: a map lookup, not a query.
	// See docs/corp-notes.md#member-hangars.
	if !isHangar && !isCargo && sqldb != nil {
		if bindNum := bindNumFromCallStream(cs); bindNum > 0 {
			if r, handled := listMemberHangar(addr, bindNum, charID, *stationID, sqldb); handled {
				return server.SendCallRsp(c, pkt, accountID, r)
			}
		}
	}
	// Corp office containers bind by itemID too and list while docked, so this check
	// must not sit behind ds.solarSystem.
	if !isHangar && !isCargo && sqldb != nil {
		// 66M+ covers office folders (66–68M) and entity-band offices;
		// the small fixed bind IDs (2/3/4/7/8) never reach the DB check.
		if contID := bindNumFromCallStream(cs); contID >= 66000000 {
			if r, handled := listOfficeContainer(addr, contID, charID, sqldb); handled {
				return server.SendCallRsp(c, pkt, accountID, r)
			}
		}
	}
	// The pilot's own assembled container (alsc.go): bind by itemID,
	// docked or in their own ship, password-gated.
	if !isHangar && !isCargo && sqldb != nil {
		if contID := bindNumFromCallStream(cs); contID >= 100000000 {
			if handled, err := listOwnedContainer(c, addr, pkt, accountID, charID, contID, sqldb); handled {
				return err
			}
		}
	}
	// Courier crate: bind by itemID while docked; resolve the escrow container and list it.
	if !isHangar && !isCargo && sqldb != nil {
		if crateID := bindNumFromCallStream(cs); crateID > 0 {
			cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
			escrowID, isCrate := evedb.CourierCrateEscrowID(cctx, sqldb, crateID)
			ccancel()
			if isCrate {
				cctx2, ccancel2 := context.WithTimeout(context.Background(), 5*time.Second)
				defer ccancel2()
				r, err := evedb.CourierPackageItems(cctx2, sqldb, crateID, escrowID)
				if err != nil {
					log.Printf("[%s] bound.List(courier crate %d escrow %d): %v", addr, crateID, escrowID, err)
					return server.SendCallRsp(c, pkt, accountID, makeInvRowset(invBrokerItemCols, nil))
				}
				log.Printf("[%s] → bound.List(courier crate %d escrow %d)", addr, crateID, escrowID)
				return server.SendCallRsp(c, pkt, accountID, r)
			}
		}
	}
	// Loot containers bind by itemID (see GetInventoryFromId). The client gates the
	// Open Cargo menu on 2500 m itself.
	if !isHangar && !isCargo && ds != nil && ds.solarSystem != nil && sqldb != nil {
		if bayID := bindNumFromCallStream(cs); isStructureBay(ds, bayID) {
			// POS structure bay / outpost platform hold: contents live at locationID=structureID,
			// flag 0, the same shape as a loot can.
			bctx, bcancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer bcancel()
			// The tower's Access tab (structureAccessAllowed), a no-op unless
			// starbase.enforceAccess is on. A refusal reads as an empty bay.
			if !structureAccessAllowed(bctx, addr, sqldb, bayID, charID, evedb.StructureActionView) {
				return server.SendCallRsp(c, pkt, accountID, makeInvRowset(invBrokerItemCols, nil))
			}
			// A launched secure can opens with its password (secureCanGate): the prompt key
			// makes invCache call EnterPassword on this bind and list again.
			if bslim, ok := ds.solarSystem.GetStarbaseSlim(uint32(bayID)); ok {
				if key, isCan := secureCanGate(bctx, sqldb, bslim, charID); isCan && key == "SCGeneralPasswordRequired" {
					return clientMsgErr(c, pkt, accountID, key, nil)
				} else if isCan && key != "" {
					return server.SendCallRsp(c, pkt, accountID, makeInvRowset(invBrokerItemCols, nil))
				}
			}
			// The client lists a bound inventory once with no flag and filters by flag itself
			// (invCache.py), so every window on this structure reads this one answer: every flag
			// unless one is named.
			bayFlag := int64(-1)
			if len(cs.ArgTuple.Items) >= 1 {
				if pi, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
					bayFlag = int64(pi.Value)
				}
			}
			// A corp hangar (CHA, assembly array, lab): members only, and only the divisions
			// their roles let them query. The client draws its own "no rights" hint over an empty tab.
			if bslim, ok := ds.solarSystem.GetStarbaseSlim(uint32(bayID)); ok && evedb.IsCorpHangarStructureGroup(bslim.GroupID) {
				var flags []int64
				if boundCharCorpID(charID, sqldb) == bslim.CorpID {
					flags = corpHangarQueryFlags(bctx, sqldb, bslim.CorpID, charID)
				}
				rows, err := evedb.GetOfficeContents(bctx, sqldb, bayID, flags)
				if err != nil {
					log.Printf("[%s] bound.List(corp hangar structure %d): %v", addr, bayID, err)
				}
				log.Printf("[%s] → bound.List(corp hangar structure %d) flags=%v %d item(s)", addr, bayID, flags, len(rows))
				return server.SendCallRsp(c, pkt, accountID, makeInvRowset(invBrokerItemCols, rows))
			}
			r, err := evedb.GetContainerItemsFlag(bctx, sqldb, bayID, bayFlag)
			if err != nil {
				log.Printf("[%s] bound.List(starbase bay %d): %v", addr, bayID, err)
				return server.SendCallRsp(c, pkt, accountID, makeInvRowset(invBrokerItemCols, nil))
			}
			log.Printf("[%s] → bound.List(starbase bay %d flag %d)", addr, bayID, bayFlag)
			return server.SendCallRsp(c, pkt, accountID, r)
		}
		if contID := bindNumFromCallStream(cs); contID > 0 {
			if _, isLoot := ds.solarSystem.GetLootContainerPosition(uint32(contID)); isLoot || isNPCCargo(ds, contID) {
				lctx, lcancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer lcancel()
				r, err := evedb.GetContainerItems(lctx, sqldb, contID)
				if err != nil {
					log.Printf("[%s] bound.List(container %d): %v", addr, contID, err)
					return server.SendCallRsp(c, pkt, accountID, makeInvRowset(invBrokerItemCols, nil))
				}
				log.Printf("[%s] → bound.List(container %d)", addr, contID)
				return server.SendCallRsp(c, pkt, accountID, r)
			}
		}
	}
	log.Printf("[%s] bound.List remoteObject=%d remoteObjectStr=%q isHangar=%v isCargo=%v charID=%d stationID=%d",
		addr, cs.RemoteObject, cs.RemoteObjectStr, isHangar, isCargo, charID, *stationID)
	if isCargo {
		// Cargo hold (flag 5) of the current ship: ds.shipID in space, the DB lookup when docked.
		var cargoShipID int64
		if ds != nil && ds.shipID != 0 {
			cargoShipID = ds.shipID
		}
		if cargoShipID == 0 && charID != 0 && sqldb != nil {
			ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel2()
			cargoShipID, _ = evedb.GetCurrentShipID(ctx2, sqldb, charID)
		}
		if charID != 0 && cargoShipID != 0 && sqldb != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			r, err := evedb.GetShipCargoItems(ctx, sqldb, charID, cargoShipID)
			if err != nil {
				log.Printf("[%s] bound.List(cargo) GetShipCargoItems: %v", addr, err)
			} else {
				log.Printf("[%s] → bound.List(cargo) charID=%d shipID=%d", addr, charID, cargoShipID)
				return server.SendCallRsp(c, pkt, accountID, r)
			}
		}
		log.Printf("[%s] → bound.List(cargo) empty", addr)
		return server.SendCallRsp(c, pkt, accountID, makeInvRowset(invBrokerItemCols, nil))
	}
	if isHangar {
		// Hangar items (flag=4) at the character's current station.
		if charID != 0 && *stationID != 0 && sqldb != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			r, err := evedb.GetCharHangarItems(ctx, sqldb, charID, *stationID)
			if err != nil {
				log.Printf("[%s] bound.List(hangar) GetCharHangarItems: %v", addr, err)
			} else {
				log.Printf("[%s] → bound.List(hangar) charID=%d stationID=%d", addr, charID, *stationID)
				return server.SendCallRsp(c, pkt, accountID, r)
			}
		}
		log.Printf("[%s] → bound.List(hangar) empty", addr)
		return server.SendCallRsp(c, pkt, accountID, makeInvRowset(invBrokerItemCols, nil))
	}
	// Skills (flags 7 and 61) are listed for the OnSkillStartTraining lookup.
	// Implants (flag 89) ride along so godma primes them with the skills.
	if charID != 0 && sqldb != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		skills, err := evedb.GetCharacterSkillItems(ctx, sqldb, charID)
		if err != nil {
			log.Printf("[%s] bound.List GetCharacterSkillItems: %v (returning empty)", addr, err)
		} else {
			rolls := make([][]marshal.PyRep, 0, len(skills))
			for _, sk := range skills {
				rolls = append(rolls, []marshal.PyRep{
					marshal.PyInt{Value: uint64(sk.ItemID)},
					marshal.PyInt{Value: uint64(sk.TypeID)},
					marshal.PyInt{Value: uint64(sk.OwnerID)},
					marshal.PyInt{Value: uint64(sk.LocationID)},
					marshal.PyInt{Value: uint64(sk.Flag)}, // flagSkill(7) or flagSkillInTraining(61)
					marshal.PyInt{Value: 0},               // contraband
					marshal.PyInt{Value: 1},               // singleton
					marshal.PyInt{Value: 1},               // quantity
					marshal.PyInt{Value: uint64(sk.GroupID)},
					marshal.PyInt{Value: uint64(sk.CategoryID)},
					marshal.PyNone{}, // customInfo
				})
			}
			if implants, impErr := evedb.GetCharacterImplants(ctx, sqldb, charID); impErr != nil {
				log.Printf("[%s] bound.List GetCharacterImplants: %v", addr, impErr)
			} else {
				for _, imp := range implants {
					rolls = append(rolls, []marshal.PyRep{
						marshal.PyInt{Value: uint64(imp.ItemID)},
						marshal.PyInt{Value: uint64(imp.TypeID)},
						marshal.PyInt{Value: uint64(imp.OwnerID)},
						marshal.PyInt{Value: uint64(imp.LocationID)},
						marshal.PyInt{Value: uint64(imp.Flag)}, // flagImplant(89)
						marshal.PyInt{Value: 0},                // contraband
						marshal.PyInt{Value: 1},                // singleton
						marshal.PyInt{Value: 1},                // quantity
						marshal.PyInt{Value: uint64(imp.GroupID)},
						marshal.PyInt{Value: uint64(imp.CategoryID)},
						marshal.PyNone{}, // customInfo
					})
				}
			}
			log.Printf("[%s] → bound.List charID=%d skills+implants=%d", addr, charID, len(rolls))
			return server.SendCallRsp(c, pkt, accountID, makeInvRowset(invBrokerItemCols, rolls))
		}
	}
	log.Printf("[%s] → bound.List (empty)", addr)
	return server.SendCallRsp(c, pkt, accountID, makeInvRowset(invBrokerItemCols, nil))
}

// boundGetItem handles the bound call "GetItem".
func boundGetItem(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, sqldb, ds := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.sqldb, cc.ds
	// InventoryBound.GetItem(itemID) answers a util.Row. With no argument the client
	// expects the container item itself (the ship entity for a ship inventory).
	giItemID := int64(0)
	if len(cs.ArgTuple.Items) >= 1 {
		if giIDRep, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
			giItemID = int64(giIDRep.Value)
		}
	}
	if giItemID == 0 {
		// No argument: answer with the bound container, not the ship (BadSlotForCapacity otherwise).
		// See docs/starbase-pos-investigation.md#getitem-on-a-bound-container-must-answer-the-container.
		if bayID := bindNumFromCallStream(cs); isStructureBay(ds, bayID) {
			giItemID = bayID
		} else if contID := bindNumFromCallStream(cs); isLootContainer(ds, contID) {
			// Loot container bound: answer with the container row so capacity is computed against the can.
			giItemID = contID
		} else if charID != 0 && sqldb != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			shipID, _ := evedb.GetCurrentShipID(ctx, sqldb, charID)
			cancel()
			if shipID != 0 {
				giItemID = shipID
			}
		}
	}
	if giItemID != 0 && sqldb != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		er, err := evedb.GetEntityRow(ctx, sqldb, giItemID)
		if err == nil && er != nil {
			log.Printf("[%s] → bound.GetItem itemID=%d typeID=%d", addr, giItemID, er.TypeID)
			return server.SendCallRsp(c, pkt, accountID, evedb.BuildEntityRow(er))
		}
		log.Printf("[%s] bound.GetItem: GetEntityRow(%d): %v", addr, giItemID, err)
	}
	log.Printf("[%s] → bound.GetItem (empty)", addr)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// boundMultiMerge handles the bound call "MultiMerge".
func boundMultiMerge(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, sqldb := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.sqldb
	// InventoryBound.MultiMerge([(sourceItemID, destItemID, qty), ...]) is a drag-merge of one
	// stack onto another. The client draws the merge at once, so the server must merge the rows.
	if charID == 0 || sqldb == nil || len(cs.ArgTuple.Items) < 1 {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	var mmOps []marshal.PyRep
	switch v := cs.ArgTuple.Items[0].(type) {
	case marshal.PyList:
		mmOps = v.Items
	case marshal.PyTuple:
		mmOps = v.Items
	}
	mmCtx, mmCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer mmCancel()
	merged := 0
	for _, opRep := range mmOps {
		op, ok := opRep.(marshal.PyTuple)
		if !ok || len(op.Items) < 3 {
			continue
		}
		srcID, sOK := op.Items[0].(marshal.PyInt)
		dstID, dOK := op.Items[1].(marshal.PyInt)
		qtyRep, qOK := op.Items[2].(marshal.PyInt)
		if !sOK || !dOK || !qOK || srcID.Value == dstID.Value {
			continue
		}
		if evedb.CourierSealed(mmCtx, sqldb, int64(srcID.Value)) ||
			evedb.CourierSealed(mmCtx, sqldb, int64(dstID.Value)) {
			continue // courier goods are sealed until delivery
		}
		srcER, sErr := evedb.GetEntityRow(mmCtx, sqldb, int64(srcID.Value))
		dstER, dErr := evedb.GetEntityRow(mmCtx, sqldb, int64(dstID.Value))
		if sErr != nil || dErr != nil || srcER == nil || dstER == nil {
			continue
		}
		// Same type, both stackable, both the caller's, same container+flag.
		if srcER.TypeID != dstER.TypeID || srcER.Singleton != 0 || dstER.Singleton != 0 ||
			srcER.OwnerID != charID || dstER.OwnerID != charID ||
			srcER.LocationID != dstER.LocationID || srcER.Flag != dstER.Flag {
			log.Printf("[%s] bound.MultiMerge: refusing %d→%d (type/owner/location mismatch)", addr, srcER.ItemID, dstER.ItemID)
			continue
		}
		// A worn item (a used crystal) keeps its own damage row and never merges.
		if evedb.GetItemAttrFloat(mmCtx, sqldb, srcER.ItemID, dogma.AttrDamage, 0) > 0 ||
			evedb.GetItemAttrFloat(mmCtx, sqldb, dstER.ItemID, dogma.AttrDamage, 0) > 0 {
			log.Printf("[%s] bound.MultiMerge: refusing %d→%d (worn item)", addr, srcER.ItemID, dstER.ItemID)
			continue
		}
		qty := int64(qtyRep.Value)
		if qty <= 0 || qty > srcER.Quantity {
			qty = srcER.Quantity
		}
		if qty == srcER.Quantity {
			if err := evedb.DeleteEntity(mmCtx, sqldb, srcER.ItemID); err != nil {
				log.Printf("[%s] bound.MultiMerge: delete source %d: %v", addr, srcER.ItemID, err)
				continue
			}
			_ = server.SendNotification(c, accountID, "OnItemChange", "charid", itemDeletedNotifArgs(srcER))
		} else {
			if err := evedb.UpdateCargoItemQty(mmCtx, sqldb, srcER.ItemID, srcER.Quantity-qty); err != nil {
				log.Printf("[%s] bound.MultiMerge: reduce source %d: %v", addr, srcER.ItemID, err)
				continue
			}
			if upER, _ := evedb.GetEntityRow(mmCtx, sqldb, srcER.ItemID); upER != nil {
				_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
					evedb.BuildEntityRow(upER),
					marshal.PyDict{Items: []marshal.DictEntry{
						{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(srcER.Quantity)}},
					}},
				}})
			}
		}
		if err := evedb.UpdateCargoItemQty(mmCtx, sqldb, dstER.ItemID, dstER.Quantity+qty); err != nil {
			log.Printf("[%s] bound.MultiMerge: grow dest %d: %v", addr, dstER.ItemID, err)
			continue
		}
		if upER, _ := evedb.GetEntityRow(mmCtx, sqldb, dstER.ItemID); upER != nil {
			_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
				evedb.BuildEntityRow(upER),
				marshal.PyDict{Items: []marshal.DictEntry{
					{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(dstER.Quantity)}},
				}},
			}})
		}
		merged++
	}
	log.Printf("[%s] → bound.MultiMerge: %d/%d merges applied", addr, merged, len(mmOps))
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// boundSetPassword handles the bound call "SetPassword".
func boundSetPassword(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, sqldb, ds := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.sqldb, cc.ds
	// InventoryBound.SetPassword(which, old, new). Construction platforms answer here;
	// an unmatched bind falls through to the default arm.
	if which, oldPw, newPw, ok := parseSetPasswordArgs(cs); ok {
		if pid := bindNumFromCallStream(cs); pid > 0 && sqldb != nil && charID != 0 {
			// A secure container / ALSC of the pilot's own (alsc.go);
			// otherwise a construction platform.
			if handled, err := handleContainerSetPassword(c, addr, pkt, accountID, charID, pid,
				which, oldPw, newPw, sqldb); handled {
				return err
			}
			return setPlatformPasswordForPlayer(c, addr, pkt, accountID, charID, pid,
				which, oldPw, newPw, sqldb, ds)
		}
	}
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// boundStackAll handles the bound call "StackAll".
func boundStackAll(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, stationID, sqldb, ds := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.stationID, cc.sqldb, cc.ds
	// InventoryBound.StackAll(locationFlag?) merges non-singleton stacks of one typeID in the
	// window's container. See docs/main-s5-notes.md#stackall-flag.
	saFlag := int64(-1)
	if len(cs.ArgTuple.Items) >= 1 {
		if f, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
			saFlag = int64(f.Value)
		}
	}
	saBind := bindNumFromCallStream(cs)

	// A member hangar bind is view only. The default arm would restack the caller's cargo hold.
	// See docs/corp-notes.md#member-hangars.
	if isMemberHangarBind(saBind) {
		log.Printf("[%s] → bound.StackAll: refused — member hangar is view only", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}

	// ownerID 0 = any owner: loot cans, POS bays and platform holds carry
	// other people's items.
	var saLocationID, saOwnerID int64
	switch {
	case isStructureBay(ds, saBind) || isLootContainer(ds, saBind) || isNPCCargo(ds, saBind):
		saLocationID, saOwnerID = saBind, 0
	case saBind >= 100000000 && isOfficeContainerID(sqldb, saBind):
		// Corp office: stack the corp's items in the current division tab.
		saLocationID = saBind
		if sqldb != nil {
			octx, ocancel := context.WithTimeout(context.Background(), 5*time.Second)
			saOwnerID, _, _, _ = evedb.GetOfficeByItemID(octx, sqldb, saBind)
			ocancel()
		}
		if saFlag < 0 {
			saFlag = 4
		}
	case saBind == hangarBindID:
		saLocationID, saOwnerID = *stationID, charID
		if saFlag < 0 {
			saFlag = 4 // flagHangar, the client's List() default
		}
	default:
		// Ship bind (cargo hold, drone bay, ship's corp hangars).
		saOwnerID = charID
		if ds != nil && ds.shipID != 0 {
			saLocationID = ds.shipID
		}
		if saLocationID == 0 && charID != 0 && sqldb != nil {
			ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
			saLocationID, _ = evedb.GetCurrentShipID(ctx2, sqldb, charID)
			cancel2()
		}
		if saFlag < 0 {
			saFlag = 5 // flagCargo
		}
	}
	log.Printf("[%s] bound.StackAll bind=%d remoteObjectStr=%q flagArg=%d location=%d owner=%d",
		addr, saBind, cs.RemoteObjectStr, saFlag, saLocationID, saOwnerID)

	if saLocationID == 0 || charID == 0 || sqldb == nil {
		log.Printf("[%s] → bound.StackAll: cannot determine container (locationID=%d charID=%d)", addr, saLocationID, charID)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	merges, err := evedb.StackAllItems(ctx, sqldb, saOwnerID, saLocationID, saFlag)
	if err != nil {
		log.Printf("[%s] bound.StackAll DB error: %v", addr, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}

	for _, m := range merges {
		// Notify deleted items (merged-away stacks).
		for _, delRow := range m.DeletedRows {
			_ = server.SendNotification(c, accountID, "OnItemChange", "charid", itemDeletedNotifArgs(delRow))
		}
		keptER, _ := evedb.GetEntityRow(ctx, sqldb, m.KeptItemID)
		if keptER != nil {
			qtyChange := marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(m.OldQty)}},
			}}
			_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
				evedb.BuildEntityRow(keptER),
				qtyChange,
			}})
		}
	}
	log.Printf("[%s] → bound.StackAll flag=%d location=%d merges=%d", addr, saFlag, saLocationID, len(merges))
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// boundGetInventoryFromId handles the bound call "GetInventoryFromId".
func boundGetInventoryFromId(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, sqldb, ds := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.sqldb, cc.ds
	// invbroker bound: GetInventoryFromId(itemID, passive) returns an InventoryBound.
	// The character's ship binds as the cargo hold; containers in range bind by itemID.
	bindNum := uint64(2)
	if len(cs.ArgTuple.Items) >= 1 {
		if eid, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok && sqldb != nil && charID != 0 {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			shipID, _ := evedb.GetCurrentShipID(ctx, sqldb, charID)
			cancel()
			if shipID != 0 && int64(eid.Value) == shipID {
				bindNum = cargoBindID
			} else if isStructureBay(ds, int64(eid.Value)) {
				// POS bay or platform hold: bind by itemID, as loot does.
				bindNum = eid.Value
			} else if isDockedHangarPlatform(sqldb, ds, charID, int64(eid.Value)) {
				// Docked platform password dialogs bind by itemID.
				bindNum = eid.Value
			} else if isOfficeContainerID(sqldb, int64(eid.Value)) {
				// Office folder or rented office: bind by itemID.
				bindNum = eid.Value
			} else if isOwnedContainerBind(sqldb, charID, int64(eid.Value)) {
				// The pilot's own assembled container (alsc.go).
				bindNum = eid.Value
			} else if func() bool {
				cx, cn := context.WithTimeout(context.Background(), 5*time.Second)
				defer cn()
				_, ok := evedb.CourierCrateEscrowID(cx, sqldb, int64(eid.Value))
				return ok
			}() {
				// Courier package: bind by itemID.
				bindNum = eid.Value
			} else if ds != nil && ds.solarSystem != nil {
				if _, isLoot := ds.solarSystem.GetLootContainerPosition(uint32(eid.Value)); isLoot {
					bindNum = eid.Value
				} else if bindNPCCargo(ds, sqldb, int64(eid.Value)) {
					bindNum = eid.Value
				}
			}
		}
	}
	bindSpec := fmt.Sprintf("N=%d:%d", globalNodeID, bindNum)
	boundObj := marshal.PySubStruct{Value: &marshal.PySubStream{
		Decoded: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyString{Value: bindSpec},
			marshal.PyInt{Value: 0},
		}},
	}}
	log.Printf("[%s] → bound.GetInventoryFromId → inventoryBound %q", addr, bindSpec)
	return server.SendCallRsp(c, pkt, accountID, boundObj)
}

// boundSetLabel handles the bound call "SetLabel".
func boundSetLabel(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, sqldb, ds := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.sqldb, cc.ds
	// invbroker.SetLabel(itemID, name): "Set Name" on a ship or renameable
	// container. Persists to entity.itemName, which GetLocationsEx serves for ship names.
	if len(cs.ArgTuple.Items) < 2 {
		log.Printf("[%s] bound.SetLabel: missing args", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	slItemIDRep, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		log.Printf("[%s] bound.SetLabel: arg[0] not int: %T", addr, cs.ArgTuple.Items[0])
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	slItemID := int64(slItemIDRep.Value)
	slName, ok := pyStr(cs.ArgTuple.Items[1])
	if !ok {
		log.Printf("[%s] bound.SetLabel: arg[1] not string: %T", addr, cs.ArgTuple.Items[1])
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	slName = strings.TrimSpace(slName)
	if len(slName) > 100 {
		slName = slName[:100]
	}
	if sqldb != nil && charID != 0 && slName != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		slER, err := evedb.GetEntityRow(ctx, sqldb, slItemID)
		if err != nil || slER == nil {
			log.Printf("[%s] bound.SetLabel: GetEntityRow(%d): %v", addr, slItemID, err)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		if !mayRenameItem(ctx, sqldb, charID, slER) {
			log.Printf("[%s] bound.SetLabel: item %d not owned by charID %d (owner %d)", addr, slItemID, charID, slER.OwnerID)
			return server.SendCallException(c, pkt, accountID, server.BuildUserError(
				"CustomInfo", map[string]marshal.PyRep{
					"info": marshal.PyString{Value: "You cannot rename an item you do not own."},
				}))
		}
		// The client's own ship-name cap (menusvc SetName: 20 for a ship,
		// 100 for anything else).
		if slER.CategoryID == 6 && len(slName) > 20 {
			slName = slName[:20]
		}
		if _, err := sqldb.ExecContext(ctx,
			`UPDATE entity SET itemName=? WHERE itemID=?`, slName, slItemID); err != nil {
			log.Printf("[%s] bound.SetLabel: update: %v", addr, err)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		// An Audit Log Secure Container records its own renaming.
		containerLog(ctx, sqldb, slER, charID, evedb.ALSCActionSetName, 0, 0, slER.Flag, true)
		log.Printf("[%s] bound.SetLabel: item %d renamed to %q by charID %d", addr, slItemID, slName, charID)
		// Keep the live ball name in sync: the client hints cfg.evelocations from
		// ball.name on every AddBalls.
		if ds != nil && ds.shipID == slItemID {
			ds.shipName = slName
			if ds.entity != nil {
				ds.entity.ShipName = slName
			}
		}
		// A structure or launched container in space: swap the ball for its renamed
		// self on every grid; the name only travels in AddBalls.
		if ds != nil && ds.solarSystem != nil && slItemID > 0 && slItemID <= math.MaxUint32 {
			if bd, bslim, bdmg, ok := ds.solarSystem.SetStarbaseBallName(uint32(slItemID), slName); ok {
				broadcastBallSwap(ds.solarSystem, uint32(slItemID), bd, bslim, bdmg)
			}
		}
		// Push the changed row so open windows refresh.
		if newER, _ := evedb.GetEntityRow(ctx, sqldb, slItemID); newER != nil {
			_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
				evedb.BuildEntityRow(newER), marshal.PyDict{},
			}})
		}
		// Overwrite the client's cfg.evelocations record before the call response,
		// so the cache is fresh when OnInvNameChange re-reads it. Row shape:
		// locationID, locationName, x, y, z.
		_ = server.SendNotification(c, accountID, "OnCfgDataChanged", "charid", marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyString{Value: "evelocations"},
			marshal.PyList{Items: []marshal.PyRep{
				marshal.PyInt{Value: uint64(slItemID)},
				marshal.PyString{Value: slName},
				marshal.PyReal{}, marshal.PyReal{}, marshal.PyReal{},
			}},
		}})
	}
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// boundGetCapacity handles the bound call "GetCapacity".
func boundGetCapacity(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, sqldb, ds := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.sqldb, cc.ds
	// InventoryBound.GetCapacity(flag). invCache falls back to this call when it
	// cannot compute from godma; None crashes the caller on .used.
	var gcFlag int64
	if len(cs.ArgTuple.Items) >= 1 {
		if pi, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
			gcFlag = int64(pi.Value)
		}
	}
	gcRow := func(capacity, used float64) marshal.PyRep {
		return marshal.PyObject{
			Type: "util.KeyVal",
			Args: marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyString{Value: "capacity"}, Value: marshal.PyReal{Value: capacity}},
				{Key: marshal.PyString{Value: "used"}, Value: marshal.PyReal{Value: used}},
			}},
		}
	}
	gcIsHangar := cs.RemoteObject == hangarBindID ||
		strings.HasSuffix(cs.RemoteObjectStr, fmt.Sprintf(":%d", hangarBindID))
	if gcIsHangar {
		// Station hangars are unlimited in this era.
		return server.SendCallRsp(c, pkt, accountID, gcRow(9e15, 0))
	}
	// Structure bays and loot containers bind by their own itemID and answer
	// with their own capacity.
	if gcBind := bindNumFromCallStream(cs); sqldb != nil && (isStructureBay(ds, gcBind) || isLootContainer(ds, gcBind)) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// The per-group flag table decides which bay is asked about;
		// a corp hangar's divisions share one capacity.
		bayFlag := gcFlag
		if gslim, ok := ds.solarSystem.GetStarbaseSlim(uint32(gcBind)); ok {
			if evedb.IsCorpHangarStructureGroup(gslim.GroupID) {
				bayFlag = -1
			} else {
				bayFlag = evedb.StructureBayFlag(gslim.GroupID, gcFlag)
			}
		} else if bayFlag != evedb.FlagSentryAmmo {
			bayFlag = 0
		}
		capacity, used, err := evedb.StructureBayCapacity(ctx, sqldb, gcBind, bayFlag)
		if err != nil {
			log.Printf("[%s] bound.GetCapacity(container %d): %v", addr, gcBind, err)
		}
		return server.SendCallRsp(c, pkt, accountID, gcRow(capacity, used))
	}
	// An opened dungeon container has no entity row of its own.
	if gcBind := bindNumFromCallStream(cs); sqldb != nil && isNPCCargo(ds, gcBind) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		capacity, used := dunCargoCapacity(ctx, sqldb, ds, gcBind)
		return server.SendCallRsp(c, pkt, accountID, gcRow(capacity, used))
	}
	if sqldb != nil && charID != 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		gcShipID, _ := evedb.GetCurrentShipID(ctx, sqldb, charID)
		if gcShipID != 0 {
			gcShipER, _ := evedb.GetEntityRow(ctx, sqldb, gcShipID)
			switch {
			case gcFlag == 87:
				var droneCap float64
				if gcShipER != nil {
					hullAttrs, _ := evedb.GetTypeAttributes(ctx, sqldb, gcShipER.TypeID)
					droneCap = hullAttrs[dogma.AttrDroneCapacity]
				}
				used, _ := evedb.GetDroneBayUsedVolume(ctx, sqldb, gcShipID)
				return server.SendCallRsp(c, pkt, accountID, gcRow(droneCap, used))
			case gcFlag >= 11 && gcFlag <= 34:
				// Module-slot charge space: the fitted module's capacity and
				// the volume of whatever charge is loaded.
				modER, _ := evedb.FindModuleInSlot(ctx, sqldb, gcShipID, gcFlag)
				if modER == nil {
					return server.SendCallRsp(c, pkt, accountID, gcRow(0, 0))
				}
				modCap, _ := evedb.GetModuleCapacity(ctx, sqldb, modER.TypeID)
				var used float64
				if loaded, _ := evedb.GetLoadedCharge(ctx, sqldb, gcShipID, gcFlag); loaded != nil {
					vol, _ := evedb.GetTypeVolume(ctx, sqldb, loaded.TypeID)
					used = vol * float64(loaded.Quantity)
				}
				return server.SendCallRsp(c, pkt, accountID, gcRow(modCap, used))
			default:
				// Cargo hold (flag 5 / None): effective capacity with cargo
				// expanders, minus what's already inside.
				var cargoCap float64
				if gcShipER != nil {
					cargoCap, _ = evedb.GetShipCargoCapacityFitted(ctx, sqldb, gcShipER.TypeID, gcShipID)
				}
				used, _ := evedb.GetCargoUsedVolume(ctx, sqldb, charID, gcShipID)
				return server.SendCallRsp(c, pkt, accountID, gcRow(cargoCap, used))
			}
		}
	}
	return server.SendCallRsp(c, pkt, accountID, gcRow(0, 0))
}

// boundRemoveItems handles the bound call "RemoveItems".
func boundRemoveItems(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, stationID, sqldb, ds := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.stationID, cc.sqldb, cc.ds
	// InventoryBound.RemoveItems([itemID, ...]): batch unfit from the fitting window.
	if len(cs.ArgTuple.Items) < 1 {
		log.Printf("[%s] bound.RemoveItems: missing args", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	var riItemIDs []int64
	switch v := cs.ArgTuple.Items[0].(type) {
	case marshal.PyList:
		for _, it := range v.Items {
			if pi, ok := it.(marshal.PyInt); ok {
				riItemIDs = append(riItemIDs, int64(pi.Value))
			}
		}
	case marshal.PyTuple:
		for _, it := range v.Items {
			if pi, ok := it.(marshal.PyInt); ok {
				riItemIDs = append(riItemIDs, int64(pi.Value))
			}
		}
	case marshal.PyInt:
		riItemIDs = append(riItemIDs, int64(v.Value))
	}
	log.Printf("[%s] bound.RemoveItems itemIDs=%v charID=%d", addr, riItemIDs, charID)
	if sqldb != nil && charID != 0 && *stationID != 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		riShipID, _ := evedb.GetCurrentShipID(ctx, sqldb, charID)
		for _, riItemID := range riItemIDs {
			oldER, err := evedb.GetEntityRow(ctx, sqldb, riItemID)
			if err != nil || oldER == nil {
				log.Printf("[%s] bound.RemoveItems: GetEntityRow(%d): %v", addr, riItemID, err)
				continue
			}
			if _, why, ok := authorizeItemMove(ctx, sqldb, ds, charID, riShipID, *stationID,
				oldER, destHangar); !ok {
				log.Printf("[%s] bound.RemoveItems: refused item %d (loc %d owner %d): %s",
					addr, riItemID, oldER.LocationID, oldER.OwnerID, why)
				continue
			}
			oldLocID := oldER.LocationID
			oldFlag := oldER.Flag
			// Unload any charge in this module's slot before unfitting.
			if oldFlag >= 11 && oldFlag <= 34 && oldER.CategoryID != 8 {
				if loaded, _ := evedb.GetLoadedCharge(ctx, sqldb, oldLocID, oldFlag); loaded != nil {
					chargeER, _ := evedb.GetEntityRow(ctx, sqldb, loaded.ItemID)
					if chargeER != nil {
						chOldLoc := chargeER.LocationID
						chOldFlag := chargeER.Flag
						destLoc, destFlag := chargeUnloadDest(ctx, sqldb, chargeER, riShipID, *stationID, charID)
						_ = evedb.UnloadChargeToCargoOrHangar(ctx, sqldb, loaded.ItemID, destLoc, destFlag)
						newChER, _ := evedb.GetEntityRow(ctx, sqldb, loaded.ItemID)
						if newChER != nil {
							chgChange := itemMoveChange(newChER, newChER.OwnerID, chOldLoc, chOldFlag)
							_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
								evedb.BuildEntityRow(newChER), chgChange,
							}})
						}
						log.Printf("[%s] bound.RemoveItems: auto-unloaded charge %d from slot %d", addr, loaded.ItemID, oldFlag)
					}
				}
			}
			if err := evedb.UnfitModuleToHangar(ctx, sqldb, riItemID, *stationID); err != nil {
				log.Printf("[%s] bound.RemoveItems: UnfitModuleToHangar(%d): %v", addr, riItemID, err)
				continue
			}
			newER, _ := evedb.GetEntityRow(ctx, sqldb, riItemID)
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
					log.Printf("[%s] bound.RemoveItems OnItemChange: %v", addr, nErr)
				}
			}
			log.Printf("[%s] bound.RemoveItems: unfitted item %d → hangar station %d", addr, riItemID, *stationID)
		}
		if riShipID == 0 {
			riShipID, _ = evedb.GetCurrentShipID(ctx, sqldb, charID)
		}
		sendShipFittingStats(c, accountID, charID, riShipID, sqldb)
		pushWeaponAttrs(c, accountID, charID, riShipID, sqldb, ds.activeEffectIDs())
		broadcastSlimItemChange(c, accountID, ds, ds.shipID, sqldb)
	}
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// boundRemoveCharges handles the bound call "RemoveCharges".
func boundRemoveCharges(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, sqldb, ds := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.sqldb, cc.ds
	// Fitting window: unload charges from a module slot. Args: {flag: slotFlag}
	// in kwargs.
	log.Printf("[%s] → bound.RemoveCharges args=%v kwargs=%v", addr, cs.ArgTuple, cs.ArgDict)
	if sqldb != nil && charID != 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rcShipID, _ := evedb.GetCurrentShipID(ctx, sqldb, charID)
		if rcShipID == 0 {
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		// The slot flag is positional arg 0 or keyword "flag".
		var rcFlag int64
		if len(cs.ArgTuple.Items) >= 1 {
			if pi, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
				rcFlag = int64(pi.Value)
			}
		}
		if rcFlag == 0 && cs.ArgDict.Items != nil {
			for _, de := range cs.ArgDict.Items {
				if ks, ok := de.Key.(marshal.PyString); ok && ks.Value == "flag" {
					if pi, ok := de.Value.(marshal.PyInt); ok {
						rcFlag = int64(pi.Value)
					}
				}
			}
		}
		if rcFlag < 11 || rcFlag > 34 {
			log.Printf("[%s] bound.RemoveCharges: invalid flag %d", addr, rcFlag)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		modER, _ := evedb.FindModuleInSlot(ctx, sqldb, rcShipID, rcFlag)
		if modER == nil {
			log.Printf("[%s] bound.RemoveCharges: no module in slot %d", addr, rcFlag)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		loaded, _ := evedb.GetLoadedCharge(ctx, sqldb, rcShipID, rcFlag)
		if loaded == nil {
			log.Printf("[%s] bound.RemoveCharges: no charge in module %d", addr, modER.ItemID)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		if refused, err := refuseBusyCharge(ctx, c, pkt, accountID, ds, modER, sqldb); refused {
			return err
		}
		chargeER, _ := evedb.GetEntityRow(ctx, sqldb, loaded.ItemID)
		if chargeER == nil {
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		oldLocID := chargeER.LocationID
		oldFlag := chargeER.Flag
		_ = evedb.UnloadChargeToCargoOrHangar(ctx, sqldb, loaded.ItemID, rcShipID, 5)
		newChargeER, _ := evedb.GetEntityRow(ctx, sqldb, loaded.ItemID)
		if newChargeER != nil {
			chgChange := itemMoveChange(newChargeER, newChargeER.OwnerID, oldLocID, oldFlag)
			_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
				evedb.BuildEntityRow(newChargeER), chgChange,
			}})
		}
		log.Printf("[%s] bound.RemoveCharges: unloaded charge %d from module %d (slot %d) → cargo", addr, loaded.ItemID, modER.ItemID, rcFlag)
		defer pushWeaponAttrs(c, accountID, charID, rcShipID, sqldb, ds.activeEffectIDs())
	}
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// boundJettison handles the bound call "Jettison". The client's cargo menu calls
// ship.Jettison(itemIDs) with a list and no quantity (menusvc); the older
// InventoryBound.Jettison(itemID, quantity) shape is still accepted.
func boundJettison(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, sqldb, ds := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.sqldb, cc.ds
	if len(cs.ArgTuple.Items) < 1 {
		log.Printf("[%s] bound.Jettison: missing args", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	jettisonFromArgs(c, addr, accountID, charID, cs.ArgTuple, sqldb, ds)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// jettisonFromArgs parses a Jettison call's arguments, (itemIDs) or (itemID, quantity),
// and ejects the items. Shared by the bound call and the inline ship bind.
func jettisonFromArgs(c *evenet.Conn, addr string, accountID, charID int64, args marshal.PyTuple, sqldb *sql.DB, ds *destinyConn) {
	if len(args.Items) < 1 {
		return
	}
	ids := parseDroneIDs(args.Items[0])
	var qty int64
	if _, single := args.Items[0].(marshal.PyInt); single && len(args.Items) >= 2 {
		if qi, ok := args.Items[1].(marshal.PyInt); ok {
			qty = int64(qi.Value)
		}
	}
	jettisonItems(c, addr, accountID, charID, ids, qty, sqldb, ds)
}

// jettisonItems ejects cargo items into one new cargo container in space,
// broadcasts its ball and arms its despawn timer. qty > 0 splits that much off
// a single item; otherwise each item moves whole. Items that are not the
// pilot's, not in the active ship's cargo hold, or missing are skipped.
func jettisonItems(c *evenet.Conn, addr string, accountID, charID int64, ids []int64, qty int64, sqldb *sql.DB, ds *destinyConn) {
	if charID == 0 || sqldb == nil || ds == nil || ds.solarSystem == nil || ds.entity == nil {
		log.Printf("[%s] Jettison: not in space or missing state", addr)
		return
	}
	if len(ids) == 0 {
		log.Printf("[%s] Jettison: no itemIDs", addr)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Validate first, so a batch with nothing jettisonable leaves no empty can.
	type jetItem struct {
		er *evedb.EntityRow
	}
	var items []jetItem
	for _, id := range ids {
		er, err := evedb.GetEntityRow(ctx, sqldb, id)
		if err != nil || er == nil {
			log.Printf("[%s] Jettison: GetEntityRow(%d): %v", addr, id, err)
			continue
		}
		if er.OwnerID != charID {
			log.Printf("[%s] Jettison: item %d not owned by charID=%d (owner=%d)", addr, id, charID, er.OwnerID)
			continue
		}
		if er.LocationID != ds.shipID || er.Flag != dogma.FlagCargo {
			log.Printf("[%s] Jettison: item %d not in active ship %d cargo (loc=%d flag=%d)", addr, id, ds.shipID, er.LocationID, er.Flag)
			continue
		}
		items = append(items, jetItem{er: er})
	}
	if len(items) == 0 {
		return
	}

	// Offset the container 2500 m from the ship.
	shipPos := ds.entity.Pos()
	containerPos := destiny.Vec3{X: shipPos.X + 2500, Y: shipPos.Y, Z: shipPos.Z}

	containerID, err := evedb.CreateLootContainer(ctx, sqldb, charID, ds.sysID,
		containerPos.X, containerPos.Y, containerPos.Z)
	if err != nil {
		log.Printf("[%s] Jettison: CreateLootContainer: %v", addr, err)
		return
	}

	moved := 0
	types := make(map[int64]struct{})
	for _, it := range items {
		er := it.er
		moveItemID := er.ItemID
		if qty > 0 && len(items) == 1 && qty < er.Quantity {
			splitID, splitErr := evedb.SplitStackN(ctx, sqldb, moveItemID, qty)
			if splitErr != nil {
				log.Printf("[%s] Jettison: SplitStackN: %v", addr, splitErr)
				continue
			}
			if splitID != moveItemID {
				if updatedER, _ := evedb.GetEntityRow(ctx, sqldb, moveItemID); updatedER != nil {
					_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
						evedb.BuildEntityRow(updatedER),
						marshal.PyDict{Items: []marshal.DictEntry{
							{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(er.Quantity)}}, // old quantity
						}},
					}})
				}
				moveItemID = splitID
			}
		}

		// Snapshot the item before the move for the deletion notification.
		preER, _ := evedb.GetEntityRow(ctx, sqldb, moveItemID)
		if preER == nil {
			preER = er
		}
		if err := evedb.MoveItemIntoContainer(ctx, sqldb, moveItemID, containerID); err != nil {
			log.Printf("[%s] Jettison: MoveItemIntoContainer(%d): %v", addr, moveItemID, err)
			continue
		}
		_ = server.SendNotification(c, accountID, "OnItemChange", "charid", itemDeletedNotifArgs(preER))
		types[preER.TypeID] = struct{}{}
		moved++
	}

	if moved == 0 {
		// Every move failed: drop the empty container row rather than spawn an empty can.
		_ = evedb.DeleteLootContainer(ctx, sqldb, containerID)
		return
	}

	spawnLootContainerBall(ds.solarSystem, sqldb, containerID, containerPos, charID)
	for typeID := range types {
		customsNoteJettison(ds.solarSystem, charID, containerID, typeID, containerPos)
	}

	log.Printf("[%s] Jettison: charID=%d jettisoned %d item(s) → container %d", addr, charID, moved, containerID)
}

// boundReplaceCharges handles the bound call "ReplaceCharges".
func boundReplaceCharges(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, stationID, sqldb, ds := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.stationID, cc.sqldb, cc.ds
	// Fitting window: swap charges in a module slot. Wire: (flag, chargeItemID,
	// quantity); arg 1 is an entity itemID, not a typeID.
	log.Printf("[%s] → bound.ReplaceCharges args=%v", addr, cs.ArgTuple)
	if sqldb != nil && charID != 0 && len(cs.ArgTuple.Items) >= 3 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rpShipID, _ := evedb.GetCurrentShipID(ctx, sqldb, charID)
		if rpShipID == 0 {
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		rpFlagRep, _ := cs.ArgTuple.Items[0].(marshal.PyInt)
		rpChargeItemRep, _ := cs.ArgTuple.Items[1].(marshal.PyInt)
		rpFlag := int64(rpFlagRep.Value)
		rpChargeItemID := int64(rpChargeItemRep.Value)

		rpChargeER, err := evedb.GetEntityRow(ctx, sqldb, rpChargeItemID)
		if err != nil || rpChargeER == nil {
			log.Printf("[%s] bound.ReplaceCharges: GetEntityRow(%d): %v", addr, rpChargeItemID, err)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		rpChargeTypeID := rpChargeER.TypeID

		if rpFlag < 11 || rpFlag > 34 {
			log.Printf("[%s] bound.ReplaceCharges: invalid flag %d", addr, rpFlag)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		modER, _ := evedb.FindModuleInSlot(ctx, sqldb, rpShipID, rpFlag)
		if modER == nil {
			log.Printf("[%s] bound.ReplaceCharges: no module in slot %d", addr, rpFlag)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		if err := evedb.ValidateChargeForModule(ctx, sqldb, rpChargeTypeID, modER.TypeID); err != nil {
			log.Printf("[%s] bound.ReplaceCharges: validation failed: %v", addr, err)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		if ds.reloadWait(modER.ItemID) > 0 {
			return clientMsgErr(c, pkt, accountID, "AlreadyBeingReloaded", map[string]marshal.PyRep{
				"item": marshal.PyString{Value: typeNameOrID(ctx, sqldb, modER.TypeID)}})
		}
		stopWeaponForReload(c, addr, accountID, charID, ds, modER.ItemID, sqldb)
		rpReload := time.Duration(0)
		if g, _, gErr := evedb.GetTypeGroupCategory(ctx, sqldb, modER.TypeID); gErr == nil {
			rpReload = reloadDelayFor(g)
		}
		loaded, _ := evedb.GetLoadedCharge(ctx, sqldb, rpShipID, rpFlag)
		if loaded != nil {
			oldChargeER, _ := evedb.GetEntityRow(ctx, sqldb, loaded.ItemID)
			if oldChargeER != nil {
				oldLocID := oldChargeER.LocationID
				oldFlag := oldChargeER.Flag
				_ = evedb.UnloadChargeToCargoOrHangar(ctx, sqldb, loaded.ItemID, rpShipID, 5)
				newOldER, _ := evedb.GetEntityRow(ctx, sqldb, loaded.ItemID)
				if newOldER != nil {
					chgChange := itemMoveChange(newOldER, newOldER.OwnerID, oldLocID, oldFlag)
					_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
						evedb.BuildEntityRow(newOldER), chgChange,
					}})
				}
			}
		}
		// The source is the item the client referenced, in cargo (flag 5) or
		// hangar (flag 4).
		srcChargeQty := rpChargeER.Quantity
		modCap, _ := evedb.GetModuleCapacity(ctx, sqldb, modER.TypeID)
		chargeVol, _ := evedb.GetTypeVolume(ctx, sqldb, rpChargeTypeID)
		maxQty := int64(1)
		if chargeVol > 0 {
			maxQty = int64(modCap / chargeVol)
		}
		if maxQty < 1 {
			maxQty = 1
		}
		loadQty := srcChargeQty
		if loadQty > maxQty {
			loadQty = maxQty
		}
		remaining := srcChargeQty - loadQty
		if remaining > 0 {
			_ = evedb.UpdateCargoItemQty(ctx, sqldb, rpChargeItemID, remaining)
			afterER, _ := evedb.GetEntityRow(ctx, sqldb, rpChargeItemID)
			if afterER != nil {
				srcChange := marshal.PyDict{Items: []marshal.DictEntry{
					{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(srcChargeQty)}},
				}}
				_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
					evedb.BuildEntityRow(afterER), srcChange,
				}})
			}
			chgItemID, crErr := evedb.CreateCargoItem(ctx, sqldb, rpChargeTypeID, charID, rpShipID, loadQty)
			if crErr != nil {
				log.Printf("[%s] bound.ReplaceCharges: CreateCargoItem: %v", addr, crErr)
				return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
			}
			_ = evedb.LoadChargeIntoModule(ctx, sqldb, chgItemID, rpShipID, rpFlag, loadQty)
			chargeER, _ := evedb.GetEntityRow(ctx, sqldb, chgItemID)
			if chargeER != nil {
				_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
					evedb.BuildEntityRow(chargeER), marshal.PyDict{},
				}})
			}
		} else {
			oldLocID := rpChargeER.LocationID
			oldFlag := rpChargeER.Flag
			_ = evedb.LoadChargeIntoModule(ctx, sqldb, rpChargeItemID, rpShipID, rpFlag, loadQty)
			chargeER, _ := evedb.GetEntityRow(ctx, sqldb, rpChargeItemID)
			if chargeER != nil {
				chgChange := marshal.PyDict{Items: []marshal.DictEntry{
					{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: uint64(oldLocID)}},
					{Key: marshal.PyInt{Value: 4}, Value: marshal.PyInt{Value: uint64(oldFlag)}},
					{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(srcChargeQty)}},
				}}
				_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
					evedb.BuildEntityRow(chargeER), chgChange,
				}})
			}
		}
		log.Printf("[%s] bound.ReplaceCharges: loaded %d of type %d into module %d (slot %d)", addr, loadQty, rpChargeTypeID, modER.ItemID, rpFlag)
		pushWeaponAttrs(c, accountID, charID, rpShipID, sqldb, ds.activeEffectIDs())
		return answerChargeLoad(c, pkt, accountID, ds, *stationID, []int64{modER.ItemID}, rpReload)
	}
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// boundItemGetInfo handles the bound call "ItemGetInfo".
func boundItemGetInfo(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, sqldb, ds := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.sqldb, cc.ds
	// dogmaIM bound: ItemGetInfo(itemID) → 5-element PyList with entity row,
	// active effects, and attributes.
	if len(cs.ArgTuple.Items) < 1 {
		log.Printf("[%s] bound.ItemGetInfo: missing args", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	igiItemIDRep, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		log.Printf("[%s] bound.ItemGetInfo: arg[0] not int: %T", addr, cs.ArgTuple.Items[0])
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	igiItemID := int64(igiItemIDRep.Value)
	log.Printf("[%s] bound.ItemGetInfo itemID=%d", addr, igiItemID)
	if sqldb != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		er, err := evedb.GetEntityRow(ctx, sqldb, igiItemID)
		if err != nil || er == nil {
			log.Printf("[%s] bound.ItemGetInfo: GetEntityRow(%d): %v", addr, igiItemID, err)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		attrs, _ := evedb.GetTypeAttributes(ctx, sqldb, er.TypeID)
		if attrs == nil {
			attrs = make(map[int64]float64)
		}
		if entityAttrs, _ := evedb.GetEntityAttributes(ctx, sqldb, igiItemID); entityAttrs != nil {
			for k, v := range entityAttrs {
				attrs[k] = v
			}
		}
		// Attribute 38 (cargo capacity) is recomputed with fitted expander and
		// bulkhead bonuses, overriding the hull base.
		if capVal, capErr := evedb.GetShipCargoCapacityFitted(ctx, sqldb, er.TypeID, er.ItemID); capErr == nil && capVal > 0 {
			attrs[dogma.AttrCapacity] = capVal
		}
		// An online fitted module carries the online effect in activeEffects: godma
		// rebuilds the effect rowset from this response, and an empty dict flips
		// online.isActive to False. Layout as buildShipGetInfoRep: env(7) + [startTime, duration, repeat, seed].
		var igiEffects []marshal.DictEntry
		if er.Flag >= 11 && er.Flag <= 34 && er.CategoryID != 8 && charID != 0 {
			if onlineIDs, onErr := evedb.GetOnlineModuleIDs(ctx, sqldb, er.LocationID); onErr != nil {
				log.Printf("[%s] bound.ItemGetInfo GetOnlineModuleIDs(%d): %v", addr, er.LocationID, onErr)
			} else if onlineIDs[igiItemID] {
				attrs[dogma.AttrIsOnline] = 1
				igiEffects = []marshal.DictEntry{{
					Key: marshal.PyInt{Value: uint64(dogma.EffectOnline)},
					Value: marshal.PyList{Items: []marshal.PyRep{
						marshal.PyInt{Value: uint64(igiItemID)},          // [0] env: self
						marshal.PyInt{Value: uint64(charID)},             // [1] env: char
						marshal.PyInt{Value: uint64(er.LocationID)},      // [2] env: ship
						marshal.PyNone{},                                 // [3] env: target
						marshal.PyNone{},                                 // [4] env: other
						marshal.PyList{},                                 // [5] env: area
						marshal.PyInt{Value: uint64(dogma.EffectOnline)}, // [6] env: effectID
						server.Win32TimeNowRep(),                         // [7] startTime
						marshal.PyInt{Value: 60000},                      // [8] duration
						marshal.PyNone{},                                 // [9] repeat
						marshal.PyNone{},                                 // [10] randomSeed
					}},
				}}
			} else {
				attrs[dogma.AttrIsOnline] = 0
			}
		}
		// A fitted weapon, or the charge in one, refreshes with the numbers it
		// fires with (moduleeffective.go).
		if er.Flag >= 11 && er.Flag <= 34 && charID != 0 {
			for k, v := range fittedWeaponOverrides(ctx, sqldb, charID, ds, er) {
				attrs[k] = v
			}
		}
		infoList, ok := buildGetInfoEntryWithEffects(igiItemID, er, attrs, igiEffects).(marshal.PyList)
		if !ok {
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		dogmaHeader := make([]marshal.PyRep, len(dogmaIMInfoCols))
		for i, col := range dogmaIMInfoCols {
			dogmaHeader[i] = marshal.PyString{Value: col}
		}
		result := marshal.PyObject{
			Type: "util.Row",
			Args: marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyString{Value: "header"}, Value: marshal.PyList{Items: dogmaHeader}},
				{Key: marshal.PyString{Value: "line"}, Value: infoList},
			}},
		}
		return server.SendCallRsp(c, pkt, accountID, result)
	}
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}
