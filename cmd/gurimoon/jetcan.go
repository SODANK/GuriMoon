package main

// Cargo containers in space as shared inventories: jetcans (ship.Jettison), kill loot cans and
// cargo containers launched for self or corp (ship.Drop). All of them ride the system's loot
// slices and the spaceLootContainers clock; a launched one carries kind LootKindLaunched, which
// keeps it on a longer clock, keeps it in space once emptied and lets its owner scoop it.
//
// Access: any pilot in range may drop items from their own cargo hold in. Taking is open too;
// it is theft (loottheft.go) unless the taker has rights to the container (its owner, the
// owner's player corp or fleet) or to the stack itself.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/dogma"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
	"gurimoon/syncx"
)

// jetcanDefaultCapacity is a can's hold when its type carries no capacity: a jetcan's 27,500 m³.
const jetcanDefaultCapacity = 27500.0

// launchedCans is the set of loot-slice containers that were launched (kind LootKindLaunched),
// filled at launch and on system load, emptied on despawn or scoop.
var launchedCans syncx.Map[int64, struct{}]

// isLaunchedCan reports whether a loot container was launched rather than jettisoned or dropped.
func isLaunchedCan(id int64) bool {
	_, ok := launchedCans.Load(id)
	return ok
}

// launchedContainerBallSlim builds the ball for a launched cargo container: the loot can's
// rigid ball with the container's own type, name and size.
func launchedContainerBallSlim(id int64, pos destiny.Vec3, ownerID, typeID, groupID int64, radius float64, name string) (destiny.BallDesc, destiny.SlimItem) {
	bd, slim := lootContainerBallSlim(id, pos, ownerID)
	if radius > 0 {
		bd.Radius = radius
	}
	bd.Name = name
	slim.TypeID = typeID
	if groupID != 0 {
		slim.GroupID = groupID
	}
	return bd, slim
}

// ── call arguments ──────────────────────────────────────────────────────────

// invAddArgs reads the quantity and flag of an inventory Add or MultiAdd. Two shapes arrive:
// positional (items, qty, flag), used by the fitting window and menus, and (items,
// sourceLocationID) with qty= / flag= keywords, used by a drag from another container's
// window. With keywords present the second positional is the source, never a quantity.
func invAddArgs(cs *server.CallStream) (qty, flag int64) {
	kw := cs.ArgDict != nil && (dictHas(cs.ArgDict, "qty") || dictHas(cs.ArgDict, "quantity") || dictHas(cs.ArgDict, "flag"))
	if !kw {
		if len(cs.ArgTuple.Items) >= 2 {
			if pi, ok := cs.ArgTuple.Items[1].(marshal.PyInt); ok {
				qty = int64(pi.Value)
			}
		}
	}
	if len(cs.ArgTuple.Items) >= 3 {
		if pi, ok := cs.ArgTuple.Items[2].(marshal.PyInt); ok {
			flag = int64(pi.Value)
		}
	}
	if kw {
		if v := dictInt(cs.ArgDict, "qty"); v > 0 {
			qty = v
		} else if v := dictInt(cs.ArgDict, "quantity"); v > 0 {
			qty = v
		}
		if v := dictInt(cs.ArgDict, "flag"); v > 0 {
			flag = v
		}
	}
	return qty, flag
}

// lootTakeDefaultsToCargo reports whether a ship-bind Add/MultiAdd that names no flag is a take
// out of in-space containers, which can only land in the cargo hold: every listed item sits in
// a loot can or an opened dungeon container. Without it the ship path reads flag 0 as a fit and
// refuses the take.
func lootTakeDefaultsToCargo(ctx context.Context, sqldb *sql.DB, ds *destinyConn, itemIDs []int64) bool {
	if sqldb == nil || ds == nil || len(itemIDs) == 0 {
		return false
	}
	for _, id := range itemIDs {
		er, err := evedb.GetEntityRow(ctx, sqldb, id)
		if err != nil || er == nil {
			return false
		}
		if !isLootContainer(ds, er.LocationID) && !isNPCCargo(ds, er.LocationID) {
			return false
		}
	}
	return true
}

// dictHas reports whether a call's keyword dict names key.
func dictHas(d *marshal.PyDict, key string) bool {
	if d == nil {
		return false
	}
	for _, e := range d.Items {
		if s, ok := e.Key.(marshal.PyString); ok && s.Value == key {
			return true
		}
	}
	return false
}

// ── loot rights ─────────────────────────────────────────────────────────────

// containerLootRights reports whether charID may take from containerID without it being
// theft: the container is theirs, their player corp's or a fleetmate's, or nobody's (an NPC
// can). takerCorpID 0 is looked up.
func containerLootRights(ctx context.Context, sqldb *sql.DB, charID, takerCorpID, containerID int64) bool {
	if sqldb == nil || charID <= 0 || containerID <= 0 {
		return false
	}
	er, err := evedb.GetEntityRow(ctx, sqldb, containerID)
	if err != nil || er == nil {
		return false
	}
	if er.OwnerID == charID {
		return true
	}
	if takerCorpID == 0 {
		takerCorpID, _ = evedb.GetCharCorpID(ctx, sqldb, charID)
	}
	owner := resolveLootOwner(ctx, sqldb, er.OwnerID)
	return !lootTakeIsTheft(charID, takerCorpID, owner, sameRealGang(charID, owner.CharID))
}

// checkLootRightExceptions answers aggressionMgr.CheckLootRightExceptions(containerID): True
// skips the client's ConfirmStealItem prompt. A pilot with rights to the container is never
// asked.
func checkLootRightExceptions(addr string, cs *server.CallStream, charID, corpID int64, sqldb *sql.DB) bool {
	var containerID int64
	if len(cs.ArgTuple.Items) >= 1 {
		containerID = repToInt(cs.ArgTuple.Items[0])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ok := containerLootRights(ctx, sqldb, charID, corpID, containerID)
	log.Printf("[%s] → CheckLootRightExceptions args=%v kwargs=%v container=%d → %v", addr, cs.ArgTuple, cs.ArgDict, containerID, ok)
	return ok
}

// ── dropping into a can ─────────────────────────────────────────────────────

// lootCanCapacity is a loot container's hold and what is already in it.
func lootCanCapacity(ctx context.Context, sqldb *sql.DB, contID int64) (capacity, used float64) {
	capacity = jetcanDefaultCapacity
	if er, err := evedb.GetEntityRow(ctx, sqldb, contID); err == nil && er != nil {
		if v, err := evedb.GetModuleCapacity(ctx, sqldb, er.TypeID); err == nil && v > 0 {
			capacity = v
		}
	}
	used, _ = evedb.GetContainerUsedVolume(ctx, sqldb, contID)
	return capacity, used
}

// handleLootCanAdd moves one stack from the caller's cargo hold into a jetcan, kill can or
// launched container in space. Anyone in range may drop in; the stack keeps its owner, so a
// later take by someone without rights is theft from the pilot who dropped it. refused means
// the call was answered with the refusal, otherwise the caller still owes the reply.
func handleLootCanAdd(c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID, charID, contID, itemID, qty int64,
	sqldb *sql.DB, ds *destinyConn) (refused bool, err error) {

	refuse := func(msg string) (bool, error) {
		return true, corpUserErr(c, pkt, accountID, msg)
	}
	if sqldb == nil || ds == nil || ds.solarSystem == nil {
		return refuse("That container is gone.")
	}
	pos, ok := ds.solarSystem.GetLootContainerPosition(uint32(contID))
	if !ok {
		return refuse("That container is gone.")
	}
	if !shipWithin(ds, pos, itemTransferRange) {
		return refuse("You are too far away from that container.")
	}
	if itemID == contID {
		return refuse("A container cannot be put inside itself.")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	er, gerr := evedb.GetEntityRow(ctx, sqldb, itemID)
	if gerr != nil || er == nil {
		return refuse("That item no longer exists.")
	}
	if er.LocationID == contID {
		return false, nil // already inside: a drop onto its own window
	}
	shipID, _ := evedb.GetCurrentShipID(ctx, sqldb, charID)
	if shipID == 0 || er.LocationID != shipID || er.Flag != dogma.FlagCargo {
		return refuse("Only items in your own cargo hold can go in there.")
	}
	if evedb.IsContainerGroup(er.GroupID) && er.Singleton != 0 {
		return refuse("An assembled container cannot be put inside another container.")
	}
	moveQty := er.Quantity
	if qty > 0 && qty < er.Quantity {
		moveQty = qty
	}
	capacity, used := lootCanCapacity(ctx, sqldb, contID)
	unitVol := evedb.ItemVolume(ctx, sqldb, er.ItemID, er.TypeID)
	if need := unitVol * float64(moveQty); used+need > capacity+1e-6 {
		return true, clientMsgErr(c, pkt, accountID, "NotEnoughCargoSpace", map[string]marshal.PyRep{
			"volume":    marshal.PyReal{Value: need},
			"available": marshal.PyReal{Value: max(capacity-used, 0)},
		})
	}
	moveID, merr := moveStackIntoContainer(ctx, c, accountID, sqldb, er, contID, moveQty, 0)
	if merr != nil {
		return refuse(merr.Error())
	}
	log.Printf("[%s] → loot can %d: item %d (type %d ×%d) dropped in by char %d", addr, contID, moveID, er.TypeID, moveQty, charID)
	return false, nil
}

// ── launch for self / corp ──────────────────────────────────────────────────

// splitCargoContainerDropIDs takes the plain cargo containers (group 12) in the caller's cargo
// hold out of a ship.Drop list. Secure and audit-log containers launch as deployables
// (starbase.go) and stay in rest.
func splitCargoContainerDropIDs(sqldb *sql.DB, shipID int64, ids []int64) (cans, rest []int64) {
	if sqldb == nil || shipID == 0 {
		return nil, ids
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	isCan := make(map[int64]bool, len(ids))
	for _, id := range ids {
		known, seen := isCan[id]
		if !seen {
			er, err := evedb.GetEntityRow(ctx, sqldb, id)
			known = err == nil && er != nil && er.GroupID == evedb.GroupCargoContainer &&
				er.LocationID == shipID && er.Flag == dogma.FlagCargo
			isCan[id] = known
		}
		if known {
			cans = append(cans, id)
		} else {
			rest = append(rest, id)
		}
	}
	return cans, rest
}

// dropFromShip is ship.Drop: cargo containers launch as long-lived cans, structures and
// deployables launch as free structures, and the rest are drones. Returns what went out.
func dropFromShip(ds *destinyConn, addr string, charID, dropOwnerID int64, ids []int64, sqldb *sql.DB) []int64 {
	canIDs, rest := splitCargoContainerDropIDs(sqldb, ds.shipID, ids)
	structIDs, droneIDs := splitStructureDropIDs(sqldb, ds.shipID, rest)
	newIDs := launchCargoContainersForPlayer(ds, addr, charID, dropOwnerID, canIDs, sqldb)
	newIDs = append(newIDs, launchStructuresForPlayer(ds, addr, charID, dropOwnerID, structIDs, sqldb)...)
	newIDs = append(newIDs, launchDronesForPlayer(ds, charID, droneIDs, sqldb)...)
	log.Printf("[%s] → ship.Drop %v owner=%d → launched %v (%d containers, %d structures)",
		addr, ids, dropOwnerID, newIDs, len(canIDs), len(structIDs))
	return newIDs
}

// launchCargoContainersForPlayer launches one unit of each listed cargo container stack into
// space beside the ship. dropOwnerID == charID is Launch for Self; anything else is Launch for
// Corporation, which falls back to the pilot in an NPC corp. Anything already inside an
// assembled container goes with it.
func launchCargoContainersForPlayer(ds *destinyConn, addr string, charID, dropOwnerID int64, ids []int64, sqldb *sql.DB) []int64 {
	var launched []int64
	if ds.solarSystem == nil || ds.entity == nil || sqldb == nil || len(ids) == 0 {
		return launched
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ownerID := charID
	if dropOwnerID != charID {
		if corpID, err := evedb.GetCharCorpID(ctx, sqldb, charID); err == nil && corpID > 0 {
			if npc, err := evedb.IsNPCCorp(ctx, sqldb, corpID); err == nil && !npc {
				ownerID = corpID
			}
		}
	}
	ss := ds.solarSystem
	shipPos := ds.entity.Pos()
	// The client sends a stack's itemID once per unit; each stack gives up one unit per Drop,
	// as the structure launch does.
	seen := make(map[int64]bool, len(ids))
	for _, stackID := range ids {
		if seen[stackID] {
			continue
		}
		seen[stackID] = true
		stack, err := evedb.GetEntityRow(ctx, sqldb, stackID)
		if err != nil || stack == nil {
			continue
		}
		itemID, err := evedb.SplitOneFromStack(ctx, sqldb, stackID)
		if err != nil {
			log.Printf("[%s] launchCargoContainers: split %d: %v", addr, stackID, err)
			continue
		}
		i := len(launched)
		pos := destiny.Vec3{X: shipPos.X + 500 + float64(i)*150, Y: shipPos.Y, Z: shipPos.Z + 500}
		if err := evedb.LaunchContainerIntoSpace(ctx, sqldb, itemID, ownerID, ds.sysID, pos.X, pos.Y, pos.Z); err != nil {
			log.Printf("[%s] launchCargoContainers: %v", addr, err)
			continue
		}
		launchedCans.Store(itemID, struct{}{})
		var radius float64
		if ti, err := evedb.GetTypeInfo(ctx, sqldb, stack.TypeID); err == nil {
			radius = ti.Radius
		}
		name, _ := evedb.GetTypeName(ctx, sqldb, stack.TypeID)
		bd, slim := launchedContainerBallSlim(itemID, pos, ownerID, stack.TypeID, stack.GroupID, radius, name)
		spawnLootBall(ss, sqldb, bd, slim)
		launched = append(launched, itemID)

		// Tell the launcher's inventory: the stack shrank, or the item left the hold.
		conn, acct := ds.entity.Conn, int64(ds.entity.AccountID)
		if itemID != stackID {
			if er, _ := evedb.GetEntityRow(ctx, sqldb, stackID); er != nil {
				_ = server.SendNotification(conn, acct, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
					evedb.BuildEntityRow(er),
					marshal.PyDict{Items: []marshal.DictEntry{{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(stack.Quantity)}}}},
				}})
			}
		} else if er, _ := evedb.GetEntityRow(ctx, sqldb, itemID); er != nil {
			_ = server.SendNotification(conn, acct, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
				evedb.BuildEntityRow(er), itemMoveChange(er, stack.OwnerID, stack.LocationID, stack.Flag),
			}})
		}
		log.Printf("[%s] cargo container launched: item=%d type=%d owner=%d in system %d", addr, itemID, stack.TypeID, ownerID, ds.sysID)
	}
	return launched
}

// ── scoop ───────────────────────────────────────────────────────────────────

// scoopLaunchedContainer answers ship.Scoop on a launched cargo container: its owner, or a
// member of its owning corp, takes it back into the cargo hold with its contents inside.
// handled=false means the object is not a launched container.
func scoopLaunchedContainer(ds *destinyConn, addr string, charID, itemID int64, sqldb *sql.DB) (handled bool, err error) {
	if ds.solarSystem == nil || !isLaunchedCan(itemID) {
		return false, nil
	}
	ss := ds.solarSystem
	bd, slim, ok := ss.GetLootBall(uint32(itemID))
	if !ok {
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
	cont, err := evedb.GetEntityRow(ctx, sqldb, itemID)
	if err != nil || cont == nil {
		return true, fmt.Errorf("there is nothing here to scoop")
	}
	if cont.OwnerID != charID {
		corpID, _ := evedb.GetCharCorpID(ctx, sqldb, charID)
		if corpID == 0 || cont.OwnerID != corpID {
			return true, fmt.Errorf("that container is not yours to scoop")
		}
	}
	if shipER, _ := evedb.GetEntityRow(ctx, sqldb, ds.shipID); shipER != nil {
		cargoCap, _ := evedb.GetShipCargoCapacityFitted(ctx, sqldb, shipER.TypeID, ds.shipID)
		usedVol, _ := evedb.GetCargoUsedVolume(ctx, sqldb, charID, ds.shipID)
		need, _ := evedb.GetTypeVolume(ctx, sqldb, cont.TypeID)
		if free := cargoCap - usedVol; need > free {
			return true, fmt.Errorf("not enough cargo space: the container needs %.0f m3 and your hold has %.0f m3 free",
				need, max(free, 0))
		}
	}
	// Taking the ball out of the system is the claim: of two pilots scooping one can, only
	// one gets past this.
	if !ss.RemoveLootContainer(uint32(itemID)) {
		return true, fmt.Errorf("there is nothing here to scoop")
	}
	if err := evedb.ScoopContainerToCargo(ctx, sqldb, itemID, ds.sysID, ds.shipID, charID); err != nil {
		if !errors.Is(err, evedb.ErrContainerGone) {
			ss.AddLootContainer(bd, slim) // still floating; put it back
			return true, err
		}
		launchedCans.Delete(itemID)
		removeLootBallFromGrid(ss, itemID)
		return true, err
	}
	launchedCans.Delete(itemID)
	removeLootBallFromGrid(ss, itemID)
	if newER, err := evedb.GetEntityRow(ctx, sqldb, itemID); err == nil && newER != nil {
		_ = server.SendNotification(ds.entity.Conn, int64(ds.entity.AccountID), "OnItemChange", "charid",
			marshal.PyTuple{Items: []marshal.PyRep{evedb.BuildEntityRow(newER), itemMoveChange(newER, cont.OwnerID, ds.sysID, 0)}})
	}
	log.Printf("[%s] launched container %d scooped by char %d into ship %d", addr, itemID, charID, ds.shipID)
	return true, nil
}
