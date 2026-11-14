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
	return er
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

// isOwnedContainerBind is the GetInventoryFromId probe: cheap enough for the
// bind path because it only runs for IDs in the entity band.
func isOwnedContainerBind(sqldb *sql.DB, charID, itemID int64) bool {
	if itemID < 100_000_000 {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return ownedContainerRow(ctx, sqldb, charID, itemID) != nil
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
	er := ownedContainerRow(ctx, sqldb, charID, contID)
	if er == nil {
		return false, nil
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

// handleContainerAdd moves one of the caller's items INTO a container bind.
// Taking out goes through the generic Add path, where classifyItemSource
// already answers srcOwnNested for the container's contents and refuses a
// locked item.
func handleContainerAdd(c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID, charID, stationID, contID, itemID, qty int64,
	reply bool, sqldb *sql.DB, ds *destinyConn) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cont := ownedContainerRow(ctx, sqldb, charID, contID)
	if cont == nil {
		return false, nil
	}
	answer := func(err error) (bool, error) {
		if err != nil || !reply {
			return true, err
		}
		return true, server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
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
	if _, why, ok := authorizeItemMove(ctx, sqldb, ds, charID, shipID, stationID, er, destCargo); !ok {
		return refuse("You cannot move that item: " + why + ".")
	}
	if er.OwnerID != charID {
		return refuse("You can only put your own items in a container.")
	}
	if cont.LocationID != er.LocationID && cont.ItemID != er.LocationID {
		// Same hangar or same ship as the container, or already inside it
		// (a move between its own flags).
		return refuse("The container and the item must be in the same place.")
	}
	// Capacity: the container's type capacity against what is already in it.
	capacity, _ := evedb.GetModuleCapacity(ctx, sqldb, cont.TypeID)
	used, _ := evedb.GetContainerUsedVolume(ctx, sqldb, contID)
	unitVol, _ := evedb.GetTypeVolume(ctx, sqldb, er.TypeID)
	moveQty := er.Quantity
	if qty > 0 && qty < er.Quantity {
		moveQty = qty
	}
	if er.LocationID != contID && capacity > 0 && used+unitVol*float64(moveQty) > capacity+1e-6 {
		return refuse(fmt.Sprintf("The container is full: %.1f of %.1f m³ used.", used, capacity))
	}
	// Inside an ALSC every item carries its lock state as its flag; in any
	// other container the contents sit at flag 0.
	insideFlag := int64(0)
	if cont.GroupID == evedb.GroupAuditLogContainer {
		insideFlag = evedb.FlagUnlocked
	}
	oldFlag := er.Flag
	moveID, err := moveStackIntoContainer(ctx, c, accountID, sqldb, er, contID, moveQty, insideFlag)
	if err != nil {
		return refuse(err.Error())
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
