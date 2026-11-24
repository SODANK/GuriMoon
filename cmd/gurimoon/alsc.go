package main

// Hangar containers: plain cargo containers (group 12), Secure Cargo Containers (340) and
// Audit Log Secure Containers (448), assembled in a hangar and opened as their own bind.
// Client contract and scope: docs/corp-notes.md#hangar-containers.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/esp"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
	"gurimoon/tick"
)

func init() {
	moduleBootHooks = append(moduleBootHooks, func(sqldb *sql.DB, _ *esp.SessionRegistry, _ *tick.Manager) {
		if sqldb == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := evedb.EnsureContainerTables(ctx, sqldb); err != nil {
			log.Printf("[containers] %v", err)
		}
	})
}

// ── session unlocks ─────────────────────────────────────────────────────────

type containerUnlockKey struct{ charID, itemID int64 }
type containerUnlock struct{ general, config bool }

var (
	containerUnlockMu sync.Mutex
	containerUnlocks  = map[containerUnlockKey]containerUnlock{}
)

func containerUnlocked(charID, itemID int64) containerUnlock {
	containerUnlockMu.Lock()
	defer containerUnlockMu.Unlock()
	return containerUnlocks[containerUnlockKey{charID, itemID}]
}

func rememberContainerUnlock(charID, itemID, which int64) {
	containerUnlockMu.Lock()
	defer containerUnlockMu.Unlock()
	k := containerUnlockKey{charID, itemID}
	u := containerUnlocks[k]
	if which == evedb.ContainerPasswordConfig {
		u.config = true
	} else {
		u.general = true
	}
	containerUnlocks[k] = u
}

// forgetContainerUnlocks clears a pilot's entered passwords on disconnect.
func forgetContainerUnlocks(charID int64) {
	containerUnlockMu.Lock()
	defer containerUnlockMu.Unlock()
	for k := range containerUnlocks {
		if k.charID == charID {
			delete(containerUnlocks, k)
		}
	}
}

// ── gates (pure) ────────────────────────────────────────────────────────────

// passwordGateKey decides whether an action needs a password the pilot has not entered this
// session. need is the ALSCNeedPasswordTo* bit (0 if the action never needs the general
// password); configPw marks actions guarded by the configuration password. It returns the
// client message key to raise, or "" when the action may run.
func passwordGateKey(sec evedb.ContainerSecurity, groupID int64, need int64, configPw bool, u containerUnlock) string {
	if configPw {
		if sec.ConfigPw != "" && !u.config {
			return "SCConfigPasswordRequired"
		}
		return ""
	}
	if sec.GeneralPw == "" || u.general {
		return ""
	}
	switch groupID {
	case evedb.GroupAuditLogContainer:
		if sec.Config&need != 0 {
			return "SCGeneralPasswordRequired"
		}
	case evedb.GroupSecureCargoContainer:
		if need == evedb.ALSCNeedPasswordToOpen {
			return "SCGeneralPasswordRequired"
		}
	}
	return ""
}

// ── lookups ─────────────────────────────────────────────────────────────────

// ownedContainerRow answers "is this an assembled container the caller
// owns": the bind, List, Add and every ALSC call key on it.
func ownedContainerRow(ctx context.Context, sqldb *sql.DB, charID, itemID int64) *evedb.EntityRow {
	if sqldb == nil || charID == 0 || itemID < 100_000_000 {
		return nil
	}
	er, err := evedb.GetEntityRow(ctx, sqldb, itemID)
	if err != nil || er == nil || er.Singleton == 0 || er.OwnerID != charID || !evedb.IsContainerGroup(er.GroupID) {
		return nil
	}
	// A container floating in space (the pilot's own jetcan or launched can) is an in-space
	// inventory: range-checked and open to drops from anyone (jetcan.go), not a hangar one.
	if isSolarSystemID(er.LocationID) {
		return nil
	}
	return er
}

// isSolarSystemID reports whether a locationID names a solar system, i.e. the row is in space.
func isSolarSystemID(id int64) bool {
	return id >= 30000000 && id < 32000000
}

// spaceSecureCanRow answers whether itemID is an assembled secure container floating or
// anchored in space, the one case where a non-owner may enter its password.
func spaceSecureCanRow(ctx context.Context, sqldb *sql.DB, itemID int64) *evedb.EntityRow {
	if sqldb == nil || itemID < 100_000_000 {
		return nil
	}
	er, err := evedb.GetEntityRow(ctx, sqldb, itemID)
	if err != nil || er == nil || er.Singleton == 0 || !evedb.IsPasswordContainerGroup(er.GroupID) ||
		er.LocationID < 30000000 || er.LocationID >= 32000000 {
		return nil
	}
	return er
}

// hangarContainer is an assembled container the caller may open as its own inventory: their
// own, in their hangar or ship, or their corp's, standing in a division of a corp office.
type hangarContainer struct {
	*evedb.EntityRow
	OfficeCorpID    int64 // non-zero: a corp container in an office
	OfficeStationID int64
}

// hangarContainerRow answers "is this a container the caller may bind": ownedContainerRow, or a
// corp-owned assembled container in an office of the caller's corp. Roles are checked per action.
func hangarContainerRow(ctx context.Context, sqldb *sql.DB, charID, itemID int64) *hangarContainer {
	if er := ownedContainerRow(ctx, sqldb, charID, itemID); er != nil {
		return &hangarContainer{EntityRow: er}
	}
	if sqldb == nil || charID == 0 || itemID < 100_000_000 {
		return nil
	}
	er, err := evedb.GetEntityRow(ctx, sqldb, itemID)
	if err != nil || er == nil || er.Singleton == 0 || !evedb.IsContainerGroup(er.GroupID) {
		return nil
	}
	corpID, stationID, _, err := evedb.GetOfficeByItemID(ctx, sqldb, er.LocationID)
	if err != nil || corpID == 0 || er.OwnerID != corpID || boundCharCorpID(charID, sqldb) != corpID {
		return nil
	}
	return &hangarContainer{EntityRow: er, OfficeCorpID: corpID, OfficeStationID: stationID}
}

// isOwnedContainerBind is the GetInventoryFromId probe: cheap enough for the
// bind path because it only runs for IDs in the entity band.
func isOwnedContainerBind(sqldb *sql.DB, charID, itemID int64) bool {
	if itemID < 100_000_000 {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return hangarContainerRow(ctx, sqldb, charID, itemID) != nil
}

// containerCapacity is a hangar container's hold and what is already in it.
func containerCapacity(ctx context.Context, sqldb *sql.DB, typeID, contID int64) (capacity, used float64) {
	capacity, _ = evedb.GetModuleCapacity(ctx, sqldb, typeID)
	used, _ = evedb.GetContainerUsedVolume(ctx, sqldb, contID)
	return capacity, used
}

func containerLog(ctx context.Context, sqldb *sql.DB, er *evedb.EntityRow, charID, action, arg1, arg2, flag int64, ok bool) {
	if er.GroupID != evedb.GroupAuditLogContainer {
		return
	}
	status := int64(0)
	if ok {
		status = 1
	}
	if err := evedb.AppendContainerLog(ctx, sqldb, evedb.ContainerLogEntry{
		ItemID: er.ItemID, ActorID: charID, ActionID: action, Arg1: arg1, Arg2: arg2,
		Flag: flag, LocationID: er.LocationID, InventoryMgrLocationID: er.LocationID, Status: status,
	}); err != nil {
		log.Printf("container %d log: %v", er.ItemID, err)
	}
}

// gateContainer runs the password gate for one action and answers the call
// with the prompt key when it fails. ok=false means the answer went out.
func gateContainer(ctx context.Context, c *evenet.Conn, pkt *evenet.PyPacket, accountID, charID int64,
	sqldb *sql.DB, er *evedb.EntityRow, need int64, configPw bool) (evedb.ContainerSecurity, bool, error) {
	sec, err := evedb.GetContainerSecurity(ctx, sqldb, er.ItemID)
	if err != nil {
		log.Printf("container %d security: %v", er.ItemID, err)
	}
	if key := passwordGateKey(sec, er.GroupID, need, configPw, containerUnlocked(charID, er.ItemID)); key != "" {
		return sec, false, clientMsgErr(c, pkt, accountID, key, nil)
	}
	return sec, true, nil
}

// ── arms ────────────────────────────────────────────────────────────────────

// listOwnedContainer serves List on a container bind.
func listOwnedContainer(c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID, charID, contID int64, sqldb *sql.DB) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hc := hangarContainerRow(ctx, sqldb, charID, contID)
	if hc == nil {
		return false, nil
	}
	er := hc.EntityRow
	// A corp container shows its contents to whoever may view its division.
	if hc.OfficeCorpID != 0 && !slices.Contains(corpHangarQueryFlagsAt(ctx, sqldb, hc.OfficeCorpID, charID, hc.OfficeStationID), er.Flag) {
		log.Printf("[%s] → bound.List(corp container %d): no view role for division %d", addr, contID, er.Flag)
		return true, server.SendCallRsp(c, pkt, accountID, makeInvRowset(invBrokerItemCols, nil))
	}
	if _, ok, err := gateContainer(ctx, c, pkt, accountID, charID, sqldb, er, evedb.ALSCNeedPasswordToOpen, false); !ok {
		return true, err
	}
	r, err := evedb.GetContainerItems(ctx, sqldb, contID)
	if err != nil {
		log.Printf("[%s] bound.List(container %d): %v", addr, contID, err)
		return true, server.SendCallRsp(c, pkt, accountID, makeInvRowset(invBrokerItemCols, nil))
	}
	log.Printf("[%s] → bound.List(owned container %d)", addr, contID)
	return true, server.SendCallRsp(c, pkt, accountID, r)
}

// handleContainerAdd moves an item INTO a container bind: from the pilot's hangar or ship into a
// container in their hangar or ship (either way while docked), between an ALSC's flags, or, for
// a corp container in an office, from the pilot or another division of that office under the
// division take role. Taking out goes through the generic Add path, where classifyItemSource
// answers srcOwnNested for the container's contents and refuses a locked item, and through
// handleOfficeTake for a corp container. handled=false means contID is not a container bind.
// reply=false (MultiAdd) answers only a refusal; see handleContainerAddRefused.
func handleContainerAdd(c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID, charID, stationID, contID, itemID, qty int64,
	reply bool, sqldb *sql.DB, ds *destinyConn) (bool, error) {
	_, err := handleContainerAddRefused(c, addr, pkt, accountID, charID, stationID, contID, itemID, qty, reply, sqldb, ds)
	if err == errContainerNotMine {
		return false, nil
	}
	return true, err
}

// errContainerNotMine is handleContainerAddRefused's "not a container bind" answer.
var errContainerNotMine = errors.New("not a hangar container")

// handleContainerAddRefused is handleContainerAdd reporting whether the call was already answered
// with a refusal, so a MultiAdd stops there. err is errContainerNotMine when contID is not a
// container the caller may bind.
func handleContainerAddRefused(c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID, charID, stationID, contID, itemID, qty int64,
	reply bool, sqldb *sql.DB, ds *destinyConn) (refused bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hc := hangarContainerRow(ctx, sqldb, charID, contID)
	if hc == nil {
		return false, errContainerNotMine
	}
	cont := hc.EntityRow
	answer := func(err error) (bool, error) {
		if err != nil || !reply {
			return false, err
		}
		return false, server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	refuse := func(msg string) (bool, error) {
		return true, corpUserErr(c, pkt, accountID, msg)
	}
	if itemID == contID {
		return refuse("A container cannot be put inside itself.")
	}
	if _, ok, err := gateContainer(ctx, c, pkt, accountID, charID, sqldb, cont, evedb.ALSCNeedPasswordToOpen, false); !ok {
		return true, err
	}
	er, err := evedb.GetEntityRow(ctx, sqldb, itemID)
	if err != nil || er == nil {
		return refuse("That item no longer exists.")
	}
	if evedb.IsContainerGroup(er.GroupID) && er.Singleton != 0 {
		return refuse("An assembled container cannot be put inside another container.")
	}
	shipID, _ := evedb.GetCurrentShipID(ctx, sqldb, charID)
	alreadyInside := er.LocationID == contID
	newOwner := er.OwnerID
	if hc.OfficeCorpID != 0 {
		// Corp container: docked at its office, with the take role on its division.
		if stationID == 0 || stationID != hc.OfficeStationID {
			return refuse("You must be docked at that office's station.")
		}
		if !corpHangarMayTakeAt(ctx, sqldb, hc.OfficeCorpID, charID, hc.OfficeStationID, cont.Flag) {
			return refuse("You do not have the hangar role for that division.")
		}
		if !alreadyInside {
			if officeID, _, _, divFlag, ok := officeOfRow(ctx, sqldb, er); ok && officeID == cont.LocationID {
				// From another division of the same office (or another container in it).
				if !corpHangarMayTakeAt(ctx, sqldb, hc.OfficeCorpID, charID, hc.OfficeStationID, divFlag) {
					return refuse("You do not have the hangar role for that division.")
				}
				if evedb.IsItemLockedDown(ctx, sqldb, er.ItemID) {
					return true, clientMsgErr(c, pkt, accountID, evedb.LockMsgItemIsLocked, nil)
				}
			} else {
				if _, why, ok := authorizeItemMove(ctx, sqldb, ds, charID, shipID, stationID, er, destCargo); !ok {
					return refuse("You cannot move that item: " + why + ".")
				}
				if er.OwnerID != charID {
					return refuse("You can only donate your own items to the corporation hangar.")
				}
				newOwner = hc.OfficeCorpID
			}
		}
	} else if !alreadyInside {
		// The pilot's own container, reachable from where they are: in their ship, or on the
		// floor of the station they are docked in.
		reachable := (shipID != 0 && cont.LocationID == shipID) || (stationID != 0 && cont.LocationID == stationID)
		if !reachable {
			return refuse("You cannot reach that container from here.")
		}
		if _, why, ok := authorizeItemMove(ctx, sqldb, ds, charID, shipID, stationID, er, destCargo); !ok {
			return refuse("You cannot move that item: " + why + ".")
		}
		if er.OwnerID != charID {
			return refuse("You can only put your own items in a container.")
		}
	}
	// Capacity: the container's type capacity against what is already in it.
	capacity, used := containerCapacity(ctx, sqldb, cont.TypeID, contID)
	unitVol := evedb.ItemVolume(ctx, sqldb, er.ItemID, er.TypeID)
	moveQty := er.Quantity
	if qty > 0 && qty < er.Quantity {
		moveQty = qty
	}
	if !alreadyInside && capacity > 0 && used+unitVol*float64(moveQty) > capacity+1e-6 {
		return true, clientMsgErr(c, pkt, accountID, "NotEnoughCargoSpace", map[string]marshal.PyRep{
			"volume":    marshal.PyReal{Value: unitVol * float64(moveQty)},
			"available": marshal.PyReal{Value: max(capacity-used, 0)},
		})
	}
	// Inside an ALSC every item carries its lock state as its flag; in any
	// other container the contents sit at flag 0.
	insideFlag := int64(0)
	if cont.GroupID == evedb.GroupAuditLogContainer {
		insideFlag = evedb.FlagUnlocked
	}
	oldFlag := er.Flag
	if er.Flag >= 11 && er.Flag <= 34 && er.LocationID == shipID {
		// A fitted module or loaded charge: take it off the ship cleanly first.
		prepFittedModuleForCargo(ctx, c, addr, accountID, charID, shipID, er, sqldb)
	}
	moveID, err := moveStackIntoContainer(ctx, c, accountID, sqldb, er, contID, moveQty, insideFlag)
	if err != nil {
		return refuse(err.Error())
	}
	if newOwner != er.OwnerID {
		_ = evedb.SetEntityOwner(ctx, sqldb, moveID, newOwner)
	}
	containerLog(ctx, sqldb, cont, charID, evedb.ALSCActionAdd, er.TypeID, moveQty, oldFlag, true)
	log.Printf("[%s] → container %d: item %d (type %d ×%d) added by char %d", addr, contID, moveID, er.TypeID, moveQty, charID)
	return answer(nil)
}

// moveStackIntoContainer moves moveQty of er into contID at insideFlag and tells the client,
// returning the moved row's itemID. A partial stack splits: the moved part is born inside the
// container and the remainder stays put. The error text is fit to show the pilot.
func moveStackIntoContainer(ctx context.Context, c *evenet.Conn, accountID int64, sqldb *sql.DB,
	er *evedb.EntityRow, contID, moveQty, insideFlag int64) (int64, error) {
	moveID := er.ItemID
	oldLoc, oldFlag := er.LocationID, er.Flag
	if moveQty < er.Quantity {
		newID, err := evedb.CreateItem(ctx, sqldb, er.TypeID, er.OwnerID, contID, int(moveQty))
		if err != nil {
			return 0, errors.New("Could not split that stack.")
		}
		_ = evedb.UpdateCargoItemQty(ctx, sqldb, er.ItemID, er.Quantity-moveQty)
		if rest, _ := evedb.GetEntityRow(ctx, sqldb, er.ItemID); rest != nil {
			_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
				evedb.BuildEntityRow(rest),
				marshal.PyDict{Items: []marshal.DictEntry{{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(er.Quantity)}}}},
			}})
		}
		moveID = newID
	} else if err := evedb.MoveItemIntoContainer(ctx, sqldb, moveID, contID); err != nil {
		return 0, errors.New("Could not move that item.")
	}
	_ = evedb.SetItemFlag(ctx, sqldb, moveID, insideFlag)
	if moved, _ := evedb.GetEntityRow(ctx, sqldb, moveID); moved != nil {
		_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
			evedb.BuildEntityRow(moved),
			marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: uint64(oldLoc)}},
				{Key: marshal.PyInt{Value: 4}, Value: marshal.PyInt{Value: uint64(oldFlag)}},
			}},
		}})
	}
	return moveID, nil
}

func handleAssembleContainer(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB) error {
	none := marshal.PyNone{}
	if len(cs.ArgTuple.Items) < 1 || sqldb == nil || charID == 0 {
		return server.SendCallRsp(c, pkt, accountID, none)
	}
	id, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		return server.SendCallRsp(c, pkt, accountID, none)
	}
	itemID := int64(id.Value)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	er, err := evedb.GetEntityRow(ctx, sqldb, itemID)
	if err != nil || er == nil || er.OwnerID != charID || !evedb.IsContainerGroup(er.GroupID) {
		return corpUserErr(c, pkt, accountID, "That is not a container of yours.")
	}
	if err := evedb.AssembleContainer(ctx, sqldb, itemID); err != nil {
		return corpUserErr(c, pkt, accountID, err.Error())
	}
	if upd, _ := evedb.GetEntityRow(ctx, sqldb, itemID); upd != nil {
		_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
			evedb.BuildEntityRow(upd),
			marshal.PyDict{Items: []marshal.DictEntry{{Key: marshal.PyInt{Value: 6}, Value: marshal.PyInt{Value: 0}}}}, // ixSingleton
		}})
		containerLog(ctx, sqldb, upd, charID, evedb.ALSCActionAssemble, charID, upd.TypeID, upd.Flag, true)
	}
	log.Printf("[%s] → AssembleCargoContainer %d by char %d", addr, itemID, charID)
	return server.SendCallRsp(c, pkt, accountID, none)
}

// handleContainerSetPassword is SetPassword on a container bind (the
// platform path hands non-platform binds here).
func handleContainerSetPassword(c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID, charID, contID, which int64, oldPw, newPw string, sqldb *sql.DB) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	er := ownedContainerRow(ctx, sqldb, charID, contID)
	if er == nil || !evedb.IsPasswordContainerGroup(er.GroupID) {
		return false, nil
	}
	if which == evedb.ContainerPasswordConfig && er.GroupID != evedb.GroupAuditLogContainer {
		return true, corpUserErr(c, pkt, accountID, "Only an Audit Log Secure Container has a configuration password.")
	}
	if len(newPw) < 3 || len(newPw) > 50 {
		return true, corpUserErr(c, pkt, accountID, "The password must be 3 to 50 characters.")
	}
	err := evedb.SetContainerPassword(ctx, sqldb, contID, which, oldPw, newPw)
	containerLog(ctx, sqldb, er, charID, evedb.ALSCActionSetPassword, which, 0, er.Flag, err == nil)
	if err != nil {
		return true, corpUserErr(c, pkt, accountID, "The old password does not match.")
	}
	rememberContainerUnlock(charID, contID, which)
	log.Printf("[%s] → container %d password %d set by char %d", addr, contID, which, charID)
	return true, server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

func handleContainerEnterPassword(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB) error {
	none := marshal.PyNone{}
	contID := bindNumFromCallStream(cs)
	if len(cs.ArgTuple.Items) < 2 {
		return server.SendCallRsp(c, pkt, accountID, none)
	}
	var which int64 = evedb.ContainerPasswordGeneral
	if w, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
		which = int64(w.Value)
	}
	pw, _ := pyStr(cs.ArgTuple.Items[1])
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	er := ownedContainerRow(ctx, sqldb, charID, contID)
	if er == nil {
		// A secure can launched into space takes a password from anyone:
		// that is how its owner shares it (starbase.go secureCanGate).
		er = spaceSecureCanRow(ctx, sqldb, contID)
	}
	if er == nil {
		return server.SendCallRsp(c, pkt, accountID, none)
	}
	sec, _ := evedb.GetContainerSecurity(ctx, sqldb, contID)
	want := sec.GeneralPw
	if which == evedb.ContainerPasswordConfig {
		want = sec.ConfigPw
	}
	ok := want != "" && pw == want
	containerLog(ctx, sqldb, er, charID, evedb.ALSCActionEnterPassword, which, 0, er.Flag, ok)
	if !ok {
		log.Printf("[%s] container %d: wrong password %d from char %d", addr, contID, which, charID)
		return corpUserErr(c, pkt, accountID, "Incorrect password.")
	}
	rememberContainerUnlock(charID, contID, which)
	return server.SendCallRsp(c, pkt, accountID, none)
}

func handleALSCConfig(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB) error {
	none := marshal.PyNone{}
	contID := bindNumFromCallStream(cs)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	er := ownedContainerRow(ctx, sqldb, charID, contID)
	if er == nil || er.GroupID != evedb.GroupAuditLogContainer {
		return server.SendCallRsp(c, pkt, accountID, none)
	}
	sec, ok, err := gateContainer(ctx, c, pkt, accountID, charID, sqldb, er, 0, true)
	if !ok {
		return err
	}
	if cs.Method == "ALSCConfigGet" {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: uint64(sec.Config)})
	}
	var mask int64
	if len(cs.ArgTuple.Items) > 0 {
		if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
			mask = int64(v.Value) & 15
		}
	}
	err = evedb.SetALSCConfig(ctx, sqldb, contID, mask)
	containerLog(ctx, sqldb, er, charID, evedb.ALSCActionConfigure, mask, 0, er.Flag, err == nil)
	if err != nil {
		return corpUserErr(c, pkt, accountID, "Could not save the configuration.")
	}
	log.Printf("[%s] → container %d configured %d by char %d", addr, contID, mask, charID)
	return server.SendCallRsp(c, pkt, accountID, none)
}

var alscLogCols = []string{"logID", "logDateTime", "actorID", "actionID", "arg1", "arg2", "flag", "locationID", "inventoryMgrLocationID", "status"}

func handleALSCLogGet(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB) error {
	contID := bindNumFromCallStream(cs)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	er := ownedContainerRow(ctx, sqldb, charID, contID)
	if er == nil || er.GroupID != evedb.GroupAuditLogContainer {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyList{})
	}
	if _, ok, err := gateContainer(ctx, c, pkt, accountID, charID, sqldb, er, evedb.ALSCNeedPasswordToViewLog, false); !ok {
		return err
	}
	var fromDate, fromLogID int64
	forward := true
	if len(cs.ArgTuple.Items) > 0 {
		if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
			fromDate = evedb.Win32ToUnix(int64(v.Value))
		}
	}
	if len(cs.ArgTuple.Items) > 1 {
		if v, ok := cs.ArgTuple.Items[1].(marshal.PyInt); ok {
			fromLogID = int64(v.Value)
		}
	}
	if len(cs.ArgTuple.Items) > 2 {
		switch v := cs.ArgTuple.Items[2].(type) {
		case marshal.PyInt:
			forward = v.Value != 0
		case marshal.PyBool:
			forward = v.Value
		}
	}
	rows, err := evedb.ListContainerLog(ctx, sqldb, contID, fromDate, fromLogID, forward, 50)
	if err != nil {
		log.Printf("[%s] ALSCLogGet %d: %v", addr, contID, err)
	}
	items := make([]marshal.PyRep, 0, len(rows))
	for _, e := range rows {
		items = append(items, keyValRep(alscLogCols, []marshal.PyRep{
			marshal.PyInt{Value: uint64(e.LogID)},
			marshal.PyInt{Value: uint64(evedb.UnixToWin32(e.LogDateTime))},
			marshal.PyInt{Value: uint64(e.ActorID)},
			marshal.PyInt{Value: uint64(e.ActionID)},
			marshal.PyInt{Value: uint64(e.Arg1)},
			marshal.PyInt{Value: uint64(e.Arg2)},
			marshal.PyInt{Value: uint64(e.Flag)},
			marshal.PyInt{Value: uint64(e.LocationID)},
			marshal.PyInt{Value: uint64(e.InventoryMgrLocationID)},
			marshal.PyInt{Value: uint64(e.Status)},
		}))
	}
	log.Printf("[%s] → ALSCLogGet %d: %d row(s)", addr, contID, len(items))
	return server.SendCallRsp(c, pkt, accountID, marshal.PyList{Items: items})
}

// handleContainerLock is LockItem / UnlockItem on a container bind.
func handleContainerLock(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB, lock bool) error {
	none := marshal.PyNone{}
	contID := bindNumFromCallStream(cs)
	if len(cs.ArgTuple.Items) < 1 {
		return server.SendCallRsp(c, pkt, accountID, none)
	}
	id, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		return server.SendCallRsp(c, pkt, accountID, none)
	}
	itemID := int64(id.Value)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cont := ownedContainerRow(ctx, sqldb, charID, contID)
	if cont == nil || cont.GroupID != evedb.GroupAuditLogContainer {
		return corpUserErr(c, pkt, accountID, "Only items in an Audit Log Secure Container can be locked.")
	}
	need, action, flag := int64(evedb.ALSCNeedPasswordToLock), int64(evedb.ALSCActionLock), int64(evedb.FlagLocked)
	if !lock {
		need, action, flag = evedb.ALSCNeedPasswordToUnlock, evedb.ALSCActionUnlock, evedb.FlagUnlocked
	}
	if _, ok, err := gateContainer(ctx, c, pkt, accountID, charID, sqldb, cont, need, false); !ok {
		return err
	}
	er, err := evedb.GetEntityRow(ctx, sqldb, itemID)
	if err != nil || er == nil || er.LocationID != contID {
		return corpUserErr(c, pkt, accountID, "That item is not in this container.")
	}
	oldFlag := er.Flag
	err = evedb.SetItemFlag(ctx, sqldb, itemID, flag)
	containerLog(ctx, sqldb, cont, charID, action, er.TypeID, er.Quantity, flag, err == nil)
	if err != nil {
		return corpUserErr(c, pkt, accountID, "Could not change that item.")
	}
	if upd, _ := evedb.GetEntityRow(ctx, sqldb, itemID); upd != nil {
		_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
			evedb.BuildEntityRow(upd),
			marshal.PyDict{Items: []marshal.DictEntry{{Key: marshal.PyInt{Value: 4}, Value: marshal.PyInt{Value: uint64(oldFlag)}}}}, // ixFlag
		}})
	}
	log.Printf("[%s] → container %d: item %d flag %d→%d by char %d", addr, contID, itemID, oldFlag, flag, charID)
	return server.SendCallRsp(c, pkt, accountID, none)
}

// handleContainerRetrievePassword mails the owner the password they asked for.
func handleContainerRetrievePassword(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB) error {
	none := marshal.PyNone{}
	contID := bindNumFromCallStream(cs)
	var which int64 = evedb.ContainerPasswordGeneral
	if len(cs.ArgTuple.Items) > 0 {
		if w, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
			which = int64(w.Value)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	er := ownedContainerRow(ctx, sqldb, charID, contID)
	if er == nil || er.GroupID != evedb.GroupAuditLogContainer {
		return server.SendCallRsp(c, pkt, accountID, none)
	}
	sec, _ := evedb.GetContainerSecurity(ctx, sqldb, contID)
	kind, pw := "general", sec.GeneralPw
	if which == evedb.ContainerPasswordConfig {
		kind, pw = "configuration", sec.ConfigPw
	}
	if pw == "" {
		return corpUserErr(c, pkt, accountID, "No "+kind+" password is set on this container.")
	}
	name := typeNameOrID(ctx, sqldb, er.TypeID)
	body := fmt.Sprintf("The %s password of your %s (item %d) is:\n\n%s", kind, name, contID, pw)
	if _, err := evedb.SendEveMail(ctx, sqldb, charID, charID, "Container password: "+strings.ToUpper(kind[:1])+kind[1:], body, 0); err != nil {
		log.Printf("[%s] RetrievePassword %d: mail: %v", addr, contID, err)
		return corpUserErr(c, pkt, accountID, "The password could not be mailed.")
	}
	log.Printf("[%s] → container %d %s password mailed to char %d", addr, contID, kind, charID)
	return server.SendCallRsp(c, pkt, accountID, none)
}
