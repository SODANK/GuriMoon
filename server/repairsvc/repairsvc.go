// Package repairsvc implements the "repairSvc" service backing the station repair
// shop (ui/station/repairshop/base_repairshop.py).
//
// The shop binds the service to its station (util.Moniker('repairSvc', stationid)),
// so calls arrive inlined into MachoBindObject, as bound-object calls, or addressed
// to the service by name (UnasembleItems from the item menu). Result() computes the
// reply for all three.
package repairsvc

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"time"

	evedb "gurimoon/db"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
)

// quoteCols is the row shape base_repairshop.py reads off each quote line.
var quoteCols = []string{"itemID", "typeID", "damage", "maxHealth", "costToRepairOneUnitOfDamage"}

// errUserFacing marks a failure the client should see as a message rather than
// a generic exception.
type errUserFacing struct{ msg string }

func (e errUserFacing) Error() string { return e.msg }

// Change is one item a call mutated. An in-place change (repair, repackage) carries
// only the itemID; an item a repackage moved out of a ship also carries its origin,
// which the client's OnItemChange needs to remove it from the old container.
type Change struct {
	ItemID int64
	Moved  *evedb.RepackagedContent
}

func inPlace(ids []int64) []Change {
	out := make([]Change, len(ids))
	for i, id := range ids {
		out[i] = Change{ItemID: id}
	}
	return out
}

// unassemble repackages one item and lists everything it touched: the
// contents a ship was emptied of first, then the item itself.
func unassemble(ctx context.Context, sqldb *sql.DB, charID, itemID int64) ([]Change, error) {
	moved, err := evedb.UnassembleItem(ctx, sqldb, charID, itemID)
	out := make([]Change, 0, len(moved)+1)
	for i := range moved {
		out = append(out, Change{ItemID: moved[i].ItemID, Moved: &moved[i]})
	}
	if err != nil {
		var r evedb.RepackageRefusal
		if errors.As(err, &r) {
			return out, errUserFacing{r.Error()}
		}
		return out, err
	}
	return append(out, Change{ItemID: itemID}), nil
}

// IsMethod reports whether a method name belongs to the repair shop API.
func IsMethod(method string) bool {
	switch method {
	case "GetDamageReports", "RepairItems", "DamageModules", "UnasembleItem", "UnasembleItems":
		return true
	}
	return false
}

// Result computes the reply for a repairSvc call.  handled is false for
// methods this service does not implement, so callers can fall through.
// changed lists the items whose entity row was mutated; callers must feed it
// to NotifyChanged so the hangar view refreshes without a re-dock.
func Result(
	ctx context.Context, addr, method string, args []marshal.PyRep,
	charID int64, sqldb *sql.DB,
) (result marshal.PyRep, changed []Change, handled bool, err error) {
	switch method {
	case "GetDamageReports":
		r, err := damageReports(ctx, addr, argIntList(args, 0), charID, sqldb)
		return r, nil, true, err

	case "RepairItems":
		// RepairItems(itemIDs, iskBudget)
		itemIDs := argIntList(args, 0)
		budget := argFloat(args, 1)
		spent, err := evedb.RepairItems(ctx, sqldb, charID, itemIDs, budget)
		if err != nil {
			return nil, nil, true, err
		}
		log.Printf("[%s] → repairSvc.RepairItems: %d items, %.2f ISK spent", addr, len(itemIDs), spent)
		return marshal.PyNone{}, inPlace(itemIDs), true, nil

	case "DamageModules":
		// DamageModules([(itemID, amountOfDamage), ...]) is the shop's test control.
		for _, entry := range argList(args, 0) {
			t, ok := entry.(marshal.PyTuple)
			if !ok || len(t.Items) < 2 {
				continue
			}
			itemID := asInt(t.Items[0])
			if applyDamage(ctx, addr, charID, itemID, asFloat(t.Items[1]), sqldb) {
				changed = append(changed, Change{ItemID: itemID})
			}
		}
		log.Printf("[%s] → repairSvc.DamageModules: %d items damaged", addr, len(changed))
		return marshal.PyNone{}, changed, true, nil

	case "UnasembleItem":
		itemID := argInt(args, 0)
		ch, err := unassemble(ctx, sqldb, charID, itemID)
		if err != nil {
			log.Printf("[%s] repairSvc.UnasembleItem %d: %v", addr, itemID, err)
			var ue errUserFacing
			if !errors.As(err, &ue) {
				err = errUserFacing{"That item cannot be repackaged."}
			}
			return nil, ch, true, err
		}
		log.Printf("[%s] → repairSvc.UnasembleItem %d (%d items unloaded)", addr, itemID, len(ch)-1)
		return marshal.PyNone{}, ch, true, nil

	case "UnasembleItems":
		// menusvc.py sends {stationID: [itemID, …]}.  One refusal does not
		// stop the rest; the first one is shown once they have all run, with
		// every change made so far already pushed.
		var refusal error
		n := 0
		for _, itemID := range collectItemIDs(argAt(args, 0)) {
			ch, err := unassemble(ctx, sqldb, charID, itemID)
			changed = append(changed, ch...)
			if err != nil {
				log.Printf("[%s] repairSvc.UnasembleItems %d: %v", addr, itemID, err)
				var ue errUserFacing
				if refusal == nil && errors.As(err, &ue) {
					refusal = ue
				}
				continue
			}
			n++
		}
		log.Printf("[%s] → repairSvc.UnasembleItems: %d repackaged, %d items changed", addr, n, len(changed))
		if refusal != nil {
			return nil, changed, true, refusal
		}
		return marshal.PyNone{}, changed, true, nil
	}

	return nil, nil, false, nil
}

// NotifyChanged pushes an OnItemChange for every mutated item so the hangar redraws
// the row. ixSingleton (6) is always named in the change dict because inventorysvc
// keys its UpdateItem path off a non-empty dict; an item moved out of a ship names
// its old ixLocationID (3) and ixFlag (4) instead, like a drag to the hangar.
func NotifyChanged(c *evenet.Conn, accountID int64, changed []Change, sqldb *sql.DB) {
	if c == nil || sqldb == nil || len(changed) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, ch := range changed {
		er, err := evedb.GetEntityRow(ctx, sqldb, ch.ItemID)
		if err != nil || er == nil {
			continue
		}
		change := marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyInt{Value: 6}, Value: marshal.PyInt{Value: uint64(1 - er.Singleton)}}, // ixSingleton
		}}
		if m := ch.Moved; m != nil {
			change = marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: uint64(m.OldLocationID)}}, // ixLocationID
				{Key: marshal.PyInt{Value: 4}, Value: marshal.PyInt{Value: uint64(m.OldFlag)}},       // ixFlag
			}}
		}
		args := marshal.PyTuple{Items: []marshal.PyRep{evedb.BuildEntityRow(er), change}}
		_ = server.SendNotification(c, accountID, "OnItemChange", "charid", args)
	}
}

// Handle dispatches a CallReq addressed to the repairSvc service or to its
// bound object.
func Handle(
	c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket,
	accountID, charID int64, sqldb *sql.DB,
) error {
	result, changed, handled, err := Result(context.Background(), addr, cs.Method, cs.ArgTuple.Items, charID, sqldb)
	if !handled {
		server.RecordUnhandled(addr, "repairSvc", cs, true)
		log.Printf("[%s] repairSvc: unhandled method %q — returning None", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	NotifyChanged(c, accountID, changed, sqldb)
	if err != nil {
		var ue errUserFacing
		if errors.As(err, &ue) {
			return server.SendCallException(c, pkt, accountID, server.BuildUserError(
				"CustomInfo", map[string]marshal.PyRep{
					"info": marshal.PyString{Value: ue.msg},
				}))
		}
		log.Printf("[%s] repairSvc.%s: %v", addr, cs.Method, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	PushBalance(c, accountID, charID, cs.Method, sqldb)
	return server.SendCallRsp(c, pkt, accountID, result)
}

// PushBalance repaints the wallet after a repair bill. The repair wire carries no
// balance and the neocom ISK tooltip only advances on OnAccountChange. Callers that
// inline the call must invoke this too.
func PushBalance(c *evenet.Conn, accountID, charID int64, method string, sqldb *sql.DB) {
	if c == nil || sqldb == nil || charID == 0 || method != "RepairItems" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bal, err := evedb.GetCharacterBalance(ctx, sqldb, charID)
	if err != nil {
		log.Printf("repairSvc: balance push char %d: %v", charID, err)
		return
	}
	server.PushCashBalance(c, accountID, charID, bal)
}

// damageReports replies with {itemID: KeyVal(quote=Rowset)}. The client indexes
// the dict by every itemID it asked about, so undamageable items get an empty quote.
func damageReports(ctx context.Context, addr string, itemIDs []int64, charID int64, sqldb *sql.DB) (marshal.PyRep, error) {
	reports, err := evedb.GetDamageReports(ctx, sqldb, charID, itemIDs)
	if err != nil {
		return nil, err
	}

	entries := make([]marshal.DictEntry, 0, len(itemIDs))
	for _, itemID := range itemIDs {
		var lines []marshal.PyRep
		if r, ok := reports[itemID]; ok {
			lines = append(lines, marshal.PyList{Items: []marshal.PyRep{
				marshal.PyInt{Value: uint64(r.ItemID)},
				marshal.PyInt{Value: uint64(r.TypeID)},
				marshal.PyReal{Value: r.Damage},
				marshal.PyReal{Value: r.MaxHealth},
				marshal.PyReal{Value: r.CostToRepairOneUnitOfDamage},
			}})
		}
		entries = append(entries, marshal.DictEntry{
			Key: marshal.PyInt{Value: uint64(itemID)},
			Value: marshal.PyObject{
				Type: "util.KeyVal",
				Args: marshal.PyDict{Items: []marshal.DictEntry{
					{Key: marshal.PyString{Value: "itemID"}, Value: marshal.PyInt{Value: uint64(itemID)}},
					{Key: marshal.PyString{Value: "quote"}, Value: rowset(quoteCols, lines)},
				}},
			},
		})
	}

	log.Printf("[%s] → repairSvc.GetDamageReports: %d items, %d damageable", addr, len(itemIDs), len(reports))
	return marshal.PyDict{Items: entries}, nil
}

// applyDamage adds damage to one item, clamped to its hit points.
func applyDamage(ctx context.Context, addr string, charID, itemID int64, amount float64, sqldb *sql.DB) bool {
	reports, err := evedb.GetDamageReports(ctx, sqldb, charID, []int64{itemID})
	if err != nil {
		log.Printf("[%s] repairSvc.DamageModules quote %d: %v", addr, itemID, err)
		return false
	}
	r, ok := reports[itemID]
	if !ok {
		return false
	}
	newDamage := r.Damage + amount
	if newDamage > r.MaxHealth {
		newDamage = r.MaxHealth
	}
	if err := evedb.SetItemDamage(ctx, sqldb, itemID, newDamage); err != nil {
		log.Printf("[%s] repairSvc.DamageModules %d: %v", addr, itemID, err)
		return false
	}
	return true
}

func rowset(cols []string, lines []marshal.PyRep) marshal.PyRep {
	header := make([]marshal.PyRep, len(cols))
	for i, col := range cols {
		header[i] = marshal.PyString{Value: col}
	}
	headerList := marshal.PyList{Items: header}
	return marshal.PyObject{
		Type: "util.Rowset",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "header"}, Value: headerList},
			{Key: marshal.PyString{Value: "RowClass"}, Value: marshal.PyString{Value: "util.Row", IsType1: true}},
			{Key: marshal.PyString{Value: "lines"}, Value: marshal.PyList{Items: lines}},
			{Key: marshal.PyString{Value: "columns"}, Value: headerList},
		}},
	}
}

// ─── argument helpers ──────────────────────────────────────────────────────

func argAt(args []marshal.PyRep, idx int) marshal.PyRep {
	if idx >= len(args) {
		return nil
	}
	return args[idx]
}

func asInt(v marshal.PyRep) int64 {
	if i, ok := v.(marshal.PyInt); ok {
		return int64(i.Value)
	}
	return 0
}

func asFloat(v marshal.PyRep) float64 {
	switch n := v.(type) {
	case marshal.PyReal:
		return n.Value
	case marshal.PyInt:
		return float64(n.Value)
	}
	return 0
}

func argInt(args []marshal.PyRep, idx int) int64     { return asInt(argAt(args, idx)) }
func argFloat(args []marshal.PyRep, idx int) float64 { return asFloat(argAt(args, idx)) }

func argList(args []marshal.PyRep, idx int) []marshal.PyRep {
	switch v := argAt(args, idx).(type) {
	case marshal.PyList:
		return v.Items
	case marshal.PyTuple:
		return v.Items
	}
	return nil
}

func argIntList(args []marshal.PyRep, idx int) []int64 {
	items := argList(args, idx)
	out := make([]int64, 0, len(items))
	for _, it := range items {
		if id := asInt(it); id != 0 {
			out = append(out, id)
		}
	}
	return out
}

// collectItemIDs flattens the nested {stationID: [(itemID, …), …]} argument
// menusvc.py builds for UnasembleItems, and also accepts a plain ID list.
func collectItemIDs(v marshal.PyRep) []int64 {
	var out []int64
	var walk func(marshal.PyRep, int)
	walk = func(node marshal.PyRep, depth int) {
		if depth > 4 {
			return
		}
		switch n := node.(type) {
		case marshal.PyInt:
			out = append(out, int64(n.Value))
		case marshal.PyList:
			for _, it := range n.Items {
				walk(it, depth+1)
			}
		case marshal.PyTuple:
			// (itemID, stationID): only the first element is an item.
			if len(n.Items) > 0 {
				walk(n.Items[0], depth+1)
			}
		case marshal.PyDict:
			for _, e := range n.Items {
				walk(e.Value, depth+1)
			}
		}
	}
	walk(v, 0)
	return out
}
