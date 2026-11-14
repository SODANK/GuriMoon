package main

// The live half of evedb.CorpMembershipHook. Every corporationID write refreshes the session
// registry's CorpID and pushes OnCorporationMemberChanged(memberID, change) to the old and the
// new corporation.
//
// The client readers (bco_members.OnCorporationMemberChanged, addressbook.py) read only
// change['corporationID'] = (oldCorpID, newCorpID) and compare each side with their own
// session.corpid. The rest of the dict is the corpMemberCols row, None on the absent side.

import (
	"context"
	"database/sql"
	"log"
	"time"

	evedb "gurimoon/db"
	"gurimoon/esp"
	"gurimoon/marshal"
	"gurimoon/tick"
)

func init() {
	moduleBootHooks = append(moduleBootHooks, installCorpMembershipHook)
}

func installCorpMembershipHook(sqldb *sql.DB, sessReg *esp.SessionRegistry, _ *tick.Manager) {
	evedb.CorpMembershipHook = func(charID, oldCorpID, newCorpID int64) {
		onCorpMembershipChanged(sqldb, sessReg, charID, oldCorpID, newCorpID)
	}
}

func onCorpMembershipChanged(sqldb *sql.DB, sessReg *esp.SessionRegistry, charID, oldCorpID, newCorpID int64) {
	if sessReg != nil {
		if s := sessReg.FindByCharID(charID); s != nil {
			sessReg.Update(s.Addr, func(s *esp.Session) { s.CorpID = newCorpID })
		}
	}
	if sqldb == nil || sessReg == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	vals, err := evedb.GetCorpMemberLine(ctx, sqldb, newCorpID, charID)
	if err != nil {
		log.Printf("corp membership push charID=%d: %v", charID, err)
	}
	cols := evedb.CorpMemberColumns()
	memberID := marshal.PyInt{Value: uint64(charID)}
	if oldCorpID != 0 {
		notifyCorp(sessReg, oldCorpID, "OnCorporationMemberChanged", marshal.PyTuple{Items: []marshal.PyRep{
			memberID, marshal.PyDict{Items: memberChangeDict(cols, vals, oldCorpID, newCorpID, false)},
		}})
	}
	if newCorpID != 0 {
		notifyCorp(sessReg, newCorpID, "OnCorporationMemberChanged", marshal.PyTuple{Items: []marshal.PyRep{
			memberID, marshal.PyDict{Items: memberChangeDict(cols, vals, oldCorpID, newCorpID, true)},
		}})
	}
}

// memberChangeDict builds the change dict for one member row. added selects the ADD form (old
// side None) or the REMOVE form (new side None). The corporationID entry always carries
// (oldCorpID, newCorpID). A nil vals (row not found) makes every other value None.
func memberChangeDict(cols []string, vals []marshal.PyRep, oldCorpID, newCorpID int64, added bool) []marshal.DictEntry {
	none := marshal.PyNone{}
	entries := make([]marshal.DictEntry, 0, len(cols))
	for i, col := range cols {
		if col == "corporationID" {
			entries = append(entries, chg(col, corpIDRep(oldCorpID), corpIDRep(newCorpID)))
			continue
		}
		var v marshal.PyRep = none
		if i < len(vals) && vals[i] != nil {
			v = vals[i]
		}
		if added {
			entries = append(entries, chg(col, none, v))
		} else {
			entries = append(entries, chg(col, v, none))
		}
	}
	return entries
}

func corpIDRep(id int64) marshal.PyRep {
	if id == 0 {
		return marshal.PyNone{}
	}
	return marshal.PyInt{Value: uint64(id)}
}
