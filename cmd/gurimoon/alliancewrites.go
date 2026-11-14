package main

// The alliance service: the wire behind the client's Alliance window. Two bound objects are
// involved and their method names collide, so the alliance bind number is the allianceID.
// See docs/corp-notes.md#alliance-binds.
//
// Every list the client reads back is a util.IndexRowset, never a Rowset: the UI indexes them
// by key (GetMembers()[corporationID], relationships.has_key(toID)).
//
// Permissions follow the client's labels: answering applications, kicking members, editing the
// alliance and setting relationships are executor-director actions; declaring support is a
// member-director action; applying is a director action in the applying corp.

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"time"

	"gurimoon/corp"
	evedb "gurimoon/db"
	"gurimoon/esp"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/safego"
	"gurimoon/server"
	"gurimoon/server/infocache"
	"gurimoon/syncx"
	"gurimoon/tick"
)

// allianceBoundMethods are the calls that arrive on an allianceRegistry bound object. Names
// shared with corpRegistry are resolved by bind number, not by this map (see allianceBinds).
var allianceBoundMethods = map[string]bool{
	"UpdateAlliance": true, "GetMembers": true, "DeclareExecutorSupport": true,
	"DeleteMember": true, "GetApplications": true, "UpdateApplication": true,
	"GetRelationships": true, "SetRelationship": true, "DeleteRelationship": true,
	// Wars, when the pilot is in an alliance (all_cso.py:184-193).
	"DeclareWarAgainst": true, "RetractWar": true, "ChangeMutualWarFlag": true,
	// Bills (all_cso.py:196-229). GetBills/GetBillsReceivable collide with billMgr names, but
	// billMgr is an addressed service: only the bound path reaches this map.
	"GetBills": true, "GetBillsReceivable": true, "PayBill": true, "GetBillBalance": true,
}

// allianceExists bounds the bind map: a bind number is only remembered for an
// alliance that is actually in the table, so a client cannot grow the map with
// arbitrary IDs.
func allianceExists(sqldb *sql.DB, allianceID int64) bool {
	if sqldb == nil || allianceID <= 0 {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var n int64
	if err := sqldb.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM alliances WHERE allianceID = ? AND deleted = 0`, allianceID).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// corpAllianceMethods are the alliance calls that arrive on the corpRegistry
// bound object.  These names are unique across every service, so they need no
// bind disambiguation.
var corpAllianceMethods = map[string]bool{
	"GetSuggestedAllianceShortNames": true, "ApplyToJoinAlliance": true,
	"GetAllianceApplications": true, "DeleteAllianceApplication": true,
}

// allianceBinds maps a bind number handed out for an allianceRegistry bind to
// the alliance it was bound to.  MachoBindObject fills it; handleBoundCall
// reads it to tell an alliance GetMembers from a corporation one.
var allianceBinds syncx.Map[int64, int64] // int64 bindNum → int64 allianceID

func rememberAllianceBind(bindNum, allianceID int64) {
	if bindNum > 0 && allianceID > 0 {
		allianceBinds.Store(bindNum, allianceID)
	}
}

// allianceBindLookup returns the alliance a bind number belongs to, 0 if it is
// not an alliance bind.
func allianceBindLookup(bindNum int64) int64 {
	if bindNum <= 0 {
		return 0
	}
	if id, ok := allianceBinds.Load(bindNum); ok {
		return id
	}
	return 0
}

// ── Notifications ────────────────────────────────────────────────────────────

// allianceMemberCorps lists the corporations currently in an alliance.
func allianceMemberCorps(ctx context.Context, sqldb *sql.DB, allianceID int64) []int64 {
	rows, err := sqldb.QueryContext(ctx,
		`SELECT corporationID FROM corporation WHERE allianceID = ?`, allianceID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			out = append(out, id)
		}
	}
	return out
}

// notifyAlliance pushes an event to every online member of every corporation in an alliance.
// The client filters on allianceID itself, but sending only to the alliance keeps a corp that
// just left from acting on its old alliance's traffic.
func notifyAlliance(ctx context.Context, sqldb *sql.DB, sessReg *esp.SessionRegistry, allianceID int64, eventType string, args marshal.PyRep) {
	if sessReg == nil || allianceID == 0 {
		return
	}
	for _, corpID := range allianceMemberCorps(ctx, sqldb, allianceID) {
		notifyCorp(sessReg, corpID, eventType, args)
	}
}

// changeDict builds the (old, new) change dict every alliance On* handler expects. The reading
// rules are the corporation ones (docs/corp-notes.md#change-dicts): adds send the full row and
// removes a full row of Nones.
func changeDict(entries ...marshal.DictEntry) marshal.PyDict {
	return marshal.PyDict{Items: entries}
}

func onAllianceChanged(ctx context.Context, sqldb *sql.DB, sessReg *esp.SessionRegistry, allianceID int64, entries []marshal.DictEntry) {
	notifyAlliance(ctx, sqldb, sessReg, allianceID, "OnAllianceChanged", marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(allianceID)},
		changeDict(entries...),
	}})
}

func onAllianceMemberChanged(ctx context.Context, sqldb *sql.DB, sessReg *esp.SessionRegistry, allianceID, corpID int64, entries []marshal.DictEntry) {
	notifyAlliance(ctx, sqldb, sessReg, allianceID, "OnAllianceMemberChanged", marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(allianceID)},
		marshal.PyInt{Value: uint64(corpID)},
		changeDict(entries...),
	}})
}

func onAllianceRelationshipChanged(ctx context.Context, sqldb *sql.DB, sessReg *esp.SessionRegistry, allianceID, toID int64, entries []marshal.DictEntry) {
	notifyAlliance(ctx, sqldb, sessReg, allianceID, "OnAllianceRelationshipChanged", marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(allianceID)},
		marshal.PyInt{Value: uint64(toID)},
		changeDict(entries...),
	}})
}

// onAllianceApplicationChanged pushes to both sides of an application: the alliance's
// Applications tab and the applicant's My Applications tab, which are different bound objects
// reading the same event.
func onAllianceApplicationChanged(ctx context.Context, sqldb *sql.DB, sessReg *esp.SessionRegistry, allianceID, corpID int64, entries []marshal.DictEntry) {
	args := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(allianceID)},
		marshal.PyInt{Value: uint64(corpID)},
		changeDict(entries...),
	}}
	// Alliance side: the executor corporation only. GetApplications gates the row to executor
	// directors, so pushing the applicant's text to every member pilot would leak it.
	var executor int64
	_ = sqldb.QueryRowContext(ctx,
		`SELECT executorCorpID FROM alliances WHERE allianceID = ?`, allianceID).Scan(&executor)
	if executor > 0 {
		notifyCorp(sessReg, executor, "OnAllianceApplicationChanged", args)
	}
	notifyCorp(sessReg, corpID, "OnAllianceApplicationChanged", args)
}

// applicationChangeEntries renders an application row as a change dict.  old
// nil means the row is new, new nil means it is gone; both carry every column,
// because the client's add branch throws away a dict that does not.
func applicationChangeEntries(old, now *evedb.AllianceApplication) []marshal.DictEntry {
	col := func(name string, oldV, newV marshal.PyRep) marshal.DictEntry {
		return chg(name, oldV, newV)
	}
	val := func(a *evedb.AllianceApplication, f func(*evedb.AllianceApplication) marshal.PyRep) marshal.PyRep {
		if a == nil {
			return marshal.PyNone{}
		}
		return f(a)
	}
	return []marshal.DictEntry{
		col("allianceID",
			val(old, func(a *evedb.AllianceApplication) marshal.PyRep { return marshal.PyInt{Value: uint64(a.AllianceID)} }),
			val(now, func(a *evedb.AllianceApplication) marshal.PyRep { return marshal.PyInt{Value: uint64(a.AllianceID)} })),
		col("corporationID",
			val(old, func(a *evedb.AllianceApplication) marshal.PyRep { return marshal.PyInt{Value: uint64(a.CorporationID)} }),
			val(now, func(a *evedb.AllianceApplication) marshal.PyRep { return marshal.PyInt{Value: uint64(a.CorporationID)} })),
		col("applicationText",
			val(old, func(a *evedb.AllianceApplication) marshal.PyRep { return marshal.PyString{Value: a.ApplicationText} }),
			val(now, func(a *evedb.AllianceApplication) marshal.PyRep { return marshal.PyString{Value: a.ApplicationText} })),
		col("applicationDateTime",
			val(old, func(a *evedb.AllianceApplication) marshal.PyRep { return marshal.PyInt{Value: uint64(a.AppliedAt)} }),
			val(now, func(a *evedb.AllianceApplication) marshal.PyRep { return marshal.PyInt{Value: uint64(a.AppliedAt)} })),
		col("state",
			val(old, func(a *evedb.AllianceApplication) marshal.PyRep { return marshal.PyInt{Value: uint64(a.State)} }),
			val(now, func(a *evedb.AllianceApplication) marshal.PyRep { return marshal.PyInt{Value: uint64(a.State)} })),
	}
}

// memberChangeEntries renders a member row as a change dict, with the same
// full-row rule.
func memberChangeEntries(allianceID, corpID int64, oldChosen, newChosen *int64, added, removed bool) []marshal.DictEntry {
	idRep := func(present bool, v int64) marshal.PyRep {
		if !present {
			return marshal.PyNone{}
		}
		return marshal.PyInt{Value: uint64(v)}
	}
	chosenRep := func(v *int64) marshal.PyRep {
		if v == nil || *v == 0 {
			return marshal.PyNone{}
		}
		return marshal.PyInt{Value: uint64(*v)}
	}
	return []marshal.DictEntry{
		chg("allianceID", idRep(!added, allianceID), idRep(!removed, allianceID)),
		chg("corporationID", idRep(!added, corpID), idRep(!removed, corpID)),
		chg("chosenExecutorID",
			func() marshal.PyRep {
				if added {
					return marshal.PyNone{}
				}
				return chosenRep(oldChosen)
			}(),
			func() marshal.PyRep {
				if removed {
					return marshal.PyNone{}
				}
				return chosenRep(newChosen)
			}()),
	}
}

// allianceRelationshipsMemoMethod keys alliance.GetRelationships replies in infocache.Shared.
const allianceRelationshipsMemoMethod = "alliance.GetRelationships"

// cachedAllianceRelationshipsRowset is GetAllianceRelationshipsRowset through the shared reply
// memo. The overview's standing checks ask up to four times per ball before the client caches
// the first reply. evedb.AllianceRelationshipsChanged drops the entry on every write.
func cachedAllianceRelationshipsRowset(ctx context.Context, sqldb *sql.DB, allianceID int64) (marshal.PyRep, error) {
	return infocache.Shared.Get(allianceRelationshipsMemoMethod, allianceID, infocache.TTLIdentity, func() (marshal.PyRep, error) {
		return evedb.GetAllianceRelationshipsRowset(ctx, sqldb, allianceID)
	})
}

// ── Permission helpers ───────────────────────────────────────────────────────

// allianceCallerCorp returns the caller's corporation and the alliance it is
// in.
func allianceCallerCorp(ctx context.Context, sqldb *sql.DB, charID int64) (corpID, allianceID int64) {
	_ = sqldb.QueryRowContext(ctx, `
		SELECT c.corporationID, c.allianceID
		  FROM character_ ch JOIN corporation c ON c.corporationID = ch.corporationID
		 WHERE ch.characterID = ?`, charID).Scan(&corpID, &allianceID)
	return corpID, allianceID
}

// isAllianceExecutorDirector reports whether the caller may act for the
// alliance: a Director (or CEO) of its executor corporation.
func isAllianceExecutorDirector(ctx context.Context, sqldb *sql.DB, allianceID, charID int64) bool {
	corpID, callerAlliance := allianceCallerCorp(ctx, sqldb, charID)
	if corpID == 0 || callerAlliance != allianceID {
		return false
	}
	var executor int64
	if err := sqldb.QueryRowContext(ctx,
		`SELECT executorCorpID FROM alliances WHERE allianceID = ?`, allianceID).Scan(&executor); err != nil {
		return false
	}
	if executor != corpID {
		return false
	}
	return isDirectorOrCEO(ctx, sqldb, corpID, charID)
}

// isCorpDirector reports whether the caller may act for their own corporation.
func isCorpDirector(ctx context.Context, sqldb *sql.DB, charID int64) (int64, bool) {
	corpID, _ := allianceCallerCorp(ctx, sqldb, charID)
	if corpID == 0 {
		return 0, false
	}
	return corpID, isCorpCEO(ctx, sqldb, corpID, charID) ||
		corp.HasRole(callerRoles(ctx, sqldb, charID), corp.RoleDirector)
}

// ── corpRegistry-side calls ──────────────────────────────────────────────────

// handleCorpAllianceCall serves the alliance methods that live on the
// corporation's own bound object.
func handleCorpAllianceCall(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket,
	accountID, charID int64, sqldb *sql.DB, sessReg *esp.SessionRegistry) error {

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	arg := func(i int) marshal.PyRep {
		if i < len(cs.ArgTuple.Items) {
			return cs.ArgTuple.Items[i]
		}
		return nil
	}

	switch cs.Method {

	case "GetSuggestedAllianceShortNames":
		name, _ := pyStr(arg(0))
		var lines []marshal.PyRep
		for _, s := range evedb.SuggestedAllianceShortNames(ctx, sqldb, name) {
			lines = append(lines, marshal.PyList{Items: []marshal.PyRep{marshal.PyString{Value: s}}})
		}
		log.Printf("[%s] → corpRegistry.GetSuggestedAllianceShortNames %q (%d)", addr, name, len(lines))
		return server.SendCallRsp(c, pkt, accountID, marshal.PyObject{
			Type: "util.Rowset",
			Args: marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyString{Value: "header"}, Value: marshal.PyList{Items: []marshal.PyRep{
					marshal.PyString{Value: "shortName"}}}},
				{Key: marshal.PyString{Value: "RowClass"}, Value: marshal.PyString{Value: "util.Row", IsType1: true}},
				{Key: marshal.PyString{Value: "lines"}, Value: marshal.PyList{Items: lines}},
			}},
		})

	case "GetAllianceApplications":
		corpID, _ := allianceCallerCorp(ctx, sqldb, charID)
		rowset, err := evedb.GetCorpAllianceApplicationsRowset(ctx, sqldb, corpID)
		if err != nil {
			log.Printf("[%s] corpRegistry.GetAllianceApplications: %v", addr, err)
			return corpUserErr(c, pkt, accountID, "Could not read your applications.")
		}
		log.Printf("[%s] → corpRegistry.GetAllianceApplications corpID=%d", addr, corpID)
		return server.SendCallRsp(c, pkt, accountID, rowset)

	case "ApplyToJoinAlliance":
		corpID, ok := isCorpDirector(ctx, sqldb, charID)
		if !ok {
			return corpUserErr(c, pkt, accountID, "Only a Director may apply to join an alliance.")
		}
		allianceID := int64(0)
		if v, ok := arg(0).(marshal.PyInt); ok {
			allianceID = int64(v.Value)
		}
		text, _ := pyStr(arg(1))
		if err := evedb.ApplyToJoinAlliance(ctx, sqldb, corpID, allianceID, text); err != nil {
			log.Printf("[%s] corpRegistry.ApplyToJoinAlliance: %v", addr, err)
			return refuseErr(c, pkt, accountID, err)
		}
		app, _ := evedb.GetAllianceApplication(ctx, sqldb, allianceID, corpID)
		if app != nil {
			onAllianceApplicationChanged(ctx, sqldb, sessReg, allianceID, corpID,
				applicationChangeEntries(nil, app))
		}
		log.Printf("[%s] → corpRegistry.ApplyToJoinAlliance corpID=%d allianceID=%d", addr, corpID, allianceID)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})

	case "DeleteAllianceApplication":
		corpID, ok := isCorpDirector(ctx, sqldb, charID)
		if !ok {
			return corpUserErr(c, pkt, accountID, "Only a Director may withdraw an application.")
		}
		allianceID := int64(0)
		if v, ok := arg(0).(marshal.PyInt); ok {
			allianceID = int64(v.Value)
		}
		old, _ := evedb.GetAllianceApplication(ctx, sqldb, allianceID, corpID)
		if err := evedb.DeleteAllianceApplication(ctx, sqldb, corpID, allianceID); err != nil {
			log.Printf("[%s] corpRegistry.DeleteAllianceApplication: %v", addr, err)
			return corpUserErr(c, pkt, accountID, "Could not withdraw the application.")
		}
		if old != nil {
			onAllianceApplicationChanged(ctx, sqldb, sessReg, allianceID, corpID,
				applicationChangeEntries(old, nil))
		}
		log.Printf("[%s] → corpRegistry.DeleteAllianceApplication corpID=%d allianceID=%d", addr, corpID, allianceID)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}

	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// ── allianceRegistry-side calls ──────────────────────────────────────────────

// handleAllianceBound serves the calls made on an alliance's own bound object.
func handleAllianceBound(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket,
	accountID, charID, allianceID int64, sqldb *sql.DB, sessReg *esp.SessionRegistry) error {

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	arg := func(i int) marshal.PyRep {
		if i < len(cs.ArgTuple.Items) {
			return cs.ArgTuple.Items[i]
		}
		return nil
	}
	argInt := func(i int) int64 {
		if v, ok := arg(i).(marshal.PyInt); ok {
			return int64(v.Value)
		}
		return 0
	}

	// Reads and writes are scoped to the caller's own alliance: the bind's allianceID is
	// caller-supplied, and without this check any pilot could bind a rival alliance and read it.
	callerCorpID, callerAllianceID := allianceCallerCorp(ctx, sqldb, charID)
	if callerAllianceID != allianceID {
		return corpUserErr(c, pkt, accountID, "Your corporation is not in that alliance.")
	}
	_ = callerCorpID

	switch cs.Method {

	case "GetMembers":
		rowset, err := evedb.GetAllianceMembersRowset(ctx, sqldb, allianceID)
		if err != nil {
			log.Printf("[%s] alliance.GetMembers: %v", addr, err)
			return corpUserErr(c, pkt, accountID, "Could not read the member list.")
		}
		log.Printf("[%s] → alliance.GetMembers allianceID=%d", addr, allianceID)
		return server.SendCallRsp(c, pkt, accountID, rowset)

	case "GetApplications":
		if !isAllianceExecutorDirector(ctx, sqldb, allianceID, charID) {
			// A member-corp director can open the tab (the client gates on plain corpRoleDirector); answer
			// empty rather than raising into a UI that cannot survive an exception.
			empty, _ := evedb.GetAllianceApplicationsRowset(ctx, sqldb, -1)
			log.Printf("[%s] → alliance.GetApplications (non-executor, empty)", addr)
			return server.SendCallRsp(c, pkt, accountID, empty)
		}
		rowset, err := evedb.GetAllianceApplicationsRowset(ctx, sqldb, allianceID)
		if err != nil {
			log.Printf("[%s] alliance.GetApplications: %v", addr, err)
			return corpUserErr(c, pkt, accountID, "Could not read the applications.")
		}
		log.Printf("[%s] → alliance.GetApplications allianceID=%d", addr, allianceID)
		return server.SendCallRsp(c, pkt, accountID, rowset)

	case "GetRelationships":
		rowset, err := cachedAllianceRelationshipsRowset(ctx, sqldb, allianceID)
		if err != nil {
			log.Printf("[%s] alliance.GetRelationships: %v", addr, err)
			return corpUserErr(c, pkt, accountID, "Could not read the standings.")
		}
		log.Printf("[%s] → alliance.GetRelationships allianceID=%d", addr, allianceID)
		return server.SendCallRsp(c, pkt, accountID, rowset)

	case "UpdateAlliance":
		if !isAllianceExecutorDirector(ctx, sqldb, allianceID, charID) {
			return corpUserErr(c, pkt, accountID,
				"Only a Director of the executor corporation may edit the alliance.")
		}
		desc, _ := pyStr(arg(0))
		url, _ := pyStr(arg(1))
		oldDesc, oldURL, err := evedb.UpdateAllianceDetails(ctx, sqldb, allianceID, desc, url)
		if err != nil {
			log.Printf("[%s] alliance.UpdateAlliance: %v", addr, err)
			return corpUserErr(c, pkt, accountID, "Update failed.")
		}
		onAllianceChanged(ctx, sqldb, sessReg, allianceID, []marshal.DictEntry{
			chg("allianceID", marshal.PyInt{Value: uint64(allianceID)}, marshal.PyInt{Value: uint64(allianceID)}),
			chg("description", marshal.PyString{Value: oldDesc}, marshal.PyString{Value: desc}),
			chg("url", marshal.PyString{Value: oldURL}, marshal.PyString{Value: url}),
		})
		log.Printf("[%s] → alliance.UpdateAlliance allianceID=%d", addr, allianceID)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})

	case "UpdateApplication":
		if !isAllianceExecutorDirector(ctx, sqldb, allianceID, charID) {
			return corpUserErr(c, pkt, accountID,
				"Only a Director of the executor corporation may answer applications.")
		}
		corpID := argInt(0)
		text, _ := pyStr(arg(1))
		state := argInt(2)
		old, _ := evedb.GetAllianceApplication(ctx, sqldb, allianceID, corpID)
		if err := evedb.UpdateAllianceApplication(ctx, sqldb, allianceID, corpID, text, state); err != nil {
			log.Printf("[%s] alliance.UpdateApplication: %v", addr, err)
			return corpUserErr(c, pkt, accountID, err.Error())
		}
		now, _ := evedb.GetAllianceApplication(ctx, sqldb, allianceID, corpID)
		onAllianceApplicationChanged(ctx, sqldb, sessReg, allianceID, corpID,
			applicationChangeEntries(old, now))
		log.Printf("[%s] → alliance.UpdateApplication allianceID=%d corpID=%d state=%d",
			addr, allianceID, corpID, state)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})

	case "DeclareExecutorSupport":
		callerCorp, ok := isCorpDirector(ctx, sqldb, charID)
		if !ok {
			return corpUserErr(c, pkt, accountID, "Only a Director may declare executor support.")
		}
		if _, callerAlliance := allianceCallerCorp(ctx, sqldb, charID); callerAlliance != allianceID {
			return corpUserErr(c, pkt, accountID, "Your corporation is not in this alliance.")
		}
		supported := argInt(0)
		var oldChosen int64
		_ = sqldb.QueryRowContext(ctx,
			`SELECT chosenExecutorID FROM allianceMembers WHERE corporationID = ?`, callerCorp).Scan(&oldChosen)
		var oldExecutor int64
		_ = sqldb.QueryRowContext(ctx,
			`SELECT executorCorpID FROM alliances WHERE allianceID = ?`, allianceID).Scan(&oldExecutor)

		executor, err := evedb.DeclareExecutorSupport(ctx, sqldb, allianceID, callerCorp, supported)
		if err != nil {
			log.Printf("[%s] alliance.DeclareExecutorSupport: %v", addr, err)
			return corpUserErr(c, pkt, accountID, err.Error())
		}
		onAllianceMemberChanged(ctx, sqldb, sessReg, allianceID, callerCorp,
			memberChangeEntries(allianceID, callerCorp, &oldChosen, &supported, false, false))
		if executor != oldExecutor {
			onAllianceChanged(ctx, sqldb, sessReg, allianceID, []marshal.DictEntry{
				chg("allianceID", marshal.PyInt{Value: uint64(allianceID)}, marshal.PyInt{Value: uint64(allianceID)}),
				chg("executorCorpID", marshal.PyInt{Value: uint64(oldExecutor)}, marshal.PyInt{Value: uint64(executor)}),
			})
		}
		log.Printf("[%s] → alliance.DeclareExecutorSupport allianceID=%d corp=%d backs=%d executor=%d",
			addr, allianceID, callerCorp, supported, executor)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})

	case "DeleteMember":
		corpID := argInt(0)
		// The target must belong to the bound alliance: LeaveAlliance resolves the row from the target
		// corp itself, so an executor director of any alliance could otherwise evict from another.
		var targetAlliance int64
		if err := sqldb.QueryRowContext(ctx,
			`SELECT allianceID FROM corporation WHERE corporationID = ?`, corpID,
		).Scan(&targetAlliance); err != nil || targetAlliance != allianceID {
			return corpUserErr(c, pkt, accountID, "That corporation is not a member of this alliance.")
		}
		callerCorp, isDir := isCorpDirector(ctx, sqldb, charID)
		// A corporation may always take itself out; removing anyone else is an
		// executor-director action.
		selfLeave := isDir && corpID == callerCorp
		if !selfLeave && !isAllianceExecutorDirector(ctx, sqldb, allianceID, charID) {
			return corpUserErr(c, pkt, accountID,
				"Only a Director of the executor corporation may remove another corporation.")
		}
		return allianceRemoveMember(c, ctx, addr, pkt, accountID, allianceID, corpID, sqldb, sessReg)

	case "SetRelationship":
		if !isAllianceExecutorDirector(ctx, sqldb, allianceID, charID) {
			return corpUserErr(c, pkt, accountID,
				"Only a Director of the executor corporation may set standings.")
		}
		// Argument order is (relationship, toID), as all_cso_relationships.Set passes them.
		relationship := argInt(0)
		toID := argInt(1)
		var oldRel int64
		hadOld := sqldb.QueryRowContext(ctx,
			`SELECT relationship FROM allianceRelationships WHERE allianceID = ? AND toID = ?`,
			allianceID, toID).Scan(&oldRel) == nil
		if err := evedb.SetAllianceRelationship(ctx, sqldb, allianceID, toID, relationship); err != nil {
			log.Printf("[%s] alliance.SetRelationship: %v", addr, err)
			return corpUserErr(c, pkt, accountID, err.Error())
		}
		oldRep := marshal.PyRep(marshal.PyNone{})
		oldIDRep := marshal.PyRep(marshal.PyNone{})
		if hadOld {
			oldRep = marshal.PyInt{Value: uint64(oldRel)}
			oldIDRep = marshal.PyInt{Value: uint64(toID)}
		}
		onAllianceRelationshipChanged(ctx, sqldb, sessReg, allianceID, toID, []marshal.DictEntry{
			chg("allianceID", func() marshal.PyRep {
				if hadOld {
					return marshal.PyInt{Value: uint64(allianceID)}
				}
				return marshal.PyNone{}
			}(), marshal.PyInt{Value: uint64(allianceID)}),
			chg("toID", oldIDRep, marshal.PyInt{Value: uint64(toID)}),
			chg("relationship", oldRep, marshal.PyInt{Value: uint64(relationship)}),
		})
		log.Printf("[%s] → alliance.SetRelationship allianceID=%d toID=%d rel=%d", addr, allianceID, toID, relationship)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})

	case "DeleteRelationship":
		if !isAllianceExecutorDirector(ctx, sqldb, allianceID, charID) {
			return corpUserErr(c, pkt, accountID,
				"Only a Director of the executor corporation may clear standings.")
		}
		toID := argInt(0)
		var oldRel int64
		if err := sqldb.QueryRowContext(ctx,
			`SELECT relationship FROM allianceRelationships WHERE allianceID = ? AND toID = ?`,
			allianceID, toID).Scan(&oldRel); err != nil {
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		if err := evedb.DeleteAllianceRelationship(ctx, sqldb, allianceID, toID); err != nil {
			log.Printf("[%s] alliance.DeleteRelationship: %v", addr, err)
			return corpUserErr(c, pkt, accountID, "Could not clear the standing.")
		}
		onAllianceRelationshipChanged(ctx, sqldb, sessReg, allianceID, toID, []marshal.DictEntry{
			chg("allianceID", marshal.PyInt{Value: uint64(allianceID)}, marshal.PyNone{}),
			chg("toID", marshal.PyInt{Value: uint64(toID)}, marshal.PyNone{}),
			chg("relationship", marshal.PyInt{Value: uint64(oldRel)}, marshal.PyNone{}),
		})
		log.Printf("[%s] → alliance.DeleteRelationship allianceID=%d toID=%d", addr, allianceID, toID)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})

	case "DeclareWarAgainst":
		if !isAllianceExecutorDirector(ctx, sqldb, allianceID, charID) {
			return corpUserErr(c, pkt, accountID,
				"Only a Director of the executor corporation may declare war.")
		}
		var executor int64
		if err := sqldb.QueryRowContext(ctx,
			`SELECT executorCorpID FROM alliances WHERE allianceID = ?`, allianceID,
		).Scan(&executor); err != nil || executor == 0 {
			return corpUserErr(c, pkt, accountID, "The alliance has no executor corporation to pay the fee.")
		}
		war, err := declareWar(ctx, sqldb, sessReg, allianceID, executor, argInt(0), false)
		if err != nil {
			return refuseErr(c, pkt, accountID, err)
		}
		log.Printf("[%s] → alliance.DeclareWarAgainst alliance=%d against=%d war=%d",
			addr, allianceID, argInt(0), war.WarID)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})

	case "RetractWar":
		// The wire argument is a warID; the client parameter is misnamed againstID.
		if !isAllianceExecutorDirector(ctx, sqldb, allianceID, charID) {
			return corpUserErr(c, pkt, accountID,
				"Only a Director of the executor corporation may retract a war.")
		}
		if err := retractWarFor(ctx, sqldb, sessReg, allianceID, argInt(0)); err != nil {
			return corpUserErr(c, pkt, accountID, err.Error())
		}
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})

	case "ChangeMutualWarFlag":
		if !isAllianceExecutorDirector(ctx, sqldb, allianceID, charID) {
			return corpUserErr(c, pkt, accountID,
				"Only a Director of the executor corporation may change the mutual flag.")
		}
		if err := setWarMutualFor(ctx, sqldb, sessReg, allianceID, argInt(0), argInt(1) != 0); err != nil {
			return corpUserErr(c, pkt, accountID, err.Error())
		}
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})

	case "GetBills":
		// No bill is issued with the alliance as debtor, but the rowset must stay shaped: None is a
		// client TypeError.
		rowset, err := evedb.GetBillsRowset(ctx, sqldb, allianceID, true)
		if err != nil {
			rowset, _ = evedb.GetBillsRowset(ctx, sqldb, -1, true)
		}
		log.Printf("[%s] → alliance.GetBills allianceID=%d", addr, allianceID)
		return server.SendCallRsp(c, pkt, accountID, rowset)

	case "GetBillsReceivable":
		rowset, err := evedb.GetBillsRowset(ctx, sqldb, allianceID, false)
		if err != nil {
			rowset, _ = evedb.GetBillsRowset(ctx, sqldb, -1, false)
		}
		log.Printf("[%s] → alliance.GetBillsReceivable allianceID=%d", addr, allianceID)
		return server.SendCallRsp(c, pkt, accountID, rowset)

	case "PayBill":
		if !isAllianceExecutorDirector(ctx, sqldb, allianceID, charID) {
			return corpUserErr(c, pkt, accountID,
				"Only a Director of the executor corporation may pay alliance bills.")
		}
		billID := argInt(0)
		pending, err := evedb.GetBill(ctx, sqldb, billID)
		if err != nil || pending == nil {
			return corpUserErr(c, pkt, accountID, "No such bill.")
		}
		if pending.DebtorID != allianceID {
			return corpUserErr(c, pkt, accountID, "That bill is not addressed to this alliance.")
		}
		bill, err := evedb.PayBill(ctx, sqldb, billID, 0)
		if err != nil {
			return corpUserErr(c, pkt, accountID, err.Error())
		}
		announceBillPaid(ctx, sqldb, sessReg, bill)
		log.Printf("[%s] → alliance.PayBill %d allianceID=%d", addr, billID, allianceID)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})

	case "GetBillBalance":
		// No caller anywhere in the decompiles; a number is the safe answer.
		return server.SendCallRsp(c, pkt, accountID, marshal.PyReal{Value: 0})
	}

	log.Printf("[%s] → alliance.%s (None)", addr, cs.Method)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// allianceRemoveMember takes a corporation out of an alliance, pushes the
// member removal to the alliance and the new session value to the corp's own
// online pilots.
func allianceRemoveMember(c *evenet.Conn, ctx context.Context, addr string, pkt *evenet.PyPacket,
	accountID, allianceID, corpID int64, sqldb *sql.DB, sessReg *esp.SessionRegistry) error {

	left, executor, err := removeCorpFromAlliance(ctx, sqldb, sessReg, allianceID, corpID)
	if err != nil {
		log.Printf("[%s] alliance.DeleteMember: %v", addr, err)
		return corpUserErr(c, pkt, accountID, "Could not remove the corporation.")
	}
	if left != 0 {
		log.Printf("[%s] → alliance.DeleteMember allianceID=%d corpID=%d executor=%d", addr, left, corpID, executor)
	}
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// removeCorpFromAlliance is a corporation leaving its alliance with no call to answer:
// LeaveAlliance (member row, stale votes, executor recompute), the member-removed and executor
// pushes, and the allianceid session change to the corp's pilots, which recomputes sov.
// allianceID is the alliance the caller believes the corp is in, used only for the old executor.
func removeCorpFromAlliance(ctx context.Context, sqldb *sql.DB, sessReg *esp.SessionRegistry,
	allianceID, corpID int64) (left, executor int64, err error) {

	var oldChosen int64
	_ = sqldb.QueryRowContext(ctx,
		`SELECT chosenExecutorID FROM allianceMembers WHERE corporationID = ?`, corpID).Scan(&oldChosen)
	var oldExecutor int64
	_ = sqldb.QueryRowContext(ctx,
		`SELECT executorCorpID FROM alliances WHERE allianceID = ?`, allianceID).Scan(&oldExecutor)

	left, executor, err = evedb.LeaveAlliance(ctx, sqldb, corpID)
	if err != nil || left == 0 {
		return left, executor, err
	}
	onAllianceMemberChanged(ctx, sqldb, sessReg, left, corpID,
		memberChangeEntries(left, corpID, &oldChosen, nil, false, true))
	if executor != oldExecutor {
		onAllianceChanged(ctx, sqldb, sessReg, left, []marshal.DictEntry{
			chg("allianceID", marshal.PyInt{Value: uint64(left)}, marshal.PyInt{Value: uint64(left)}),
			chg("executorCorpID", marshal.PyInt{Value: uint64(oldExecutor)}, marshal.PyInt{Value: uint64(executor)}),
		})
	}
	pushAllianceSession(sessReg, corpID, left, 0)
	return left, executor, nil
}

// pushAllianceSession hands every online member of a corporation its new
// allianceid.  Same path the ESP panel uses; see esp.AllianceMembershipHook.
func pushAllianceSession(sessReg *esp.SessionRegistry, corpID, oldAllianceID, newAllianceID int64) {
	// A corporation's towers and outposts move their sov weight with it (db/sovmajority.go), off
	// the caller's goroutine.
	if oldAllianceID != newAllianceID && globalDB != nil {
		go func() {
			defer safego.Recover("pushAllianceSession")
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			evedb.RecomputeSovForCorp(ctx, globalDB, corpID)
		}()
	}
	if sessReg == nil {
		return
	}
	for _, s := range sessReg.FindByCorpID(corpID) {
		if s.Conn == nil {
			continue
		}
		if err := sendAllianceSessionChange(s.Conn, s.Addr, oldAllianceID, newAllianceID); err != nil {
			log.Printf("[%s] alliance session change: %v", s.Addr, err)
		}
	}
}

// ── Accepted-application sweep ───────────────────────────────────────────────

// sweepAllianceApplications completes acceptances whose waiting period is up:
// the corporation joins, the application turns Effective, the alliance and the
// joining corp are both told.
func sweepAllianceApplications(sqldb *sql.DB, sessReg *esp.SessionRegistry) {
	allianceAppSweepMu.Lock()
	defer allianceAppSweepMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	delay := evedb.AllianceAcceptDelayHours(ctx, sqldb)
	promoted, rejected, err := evedb.PromoteAcceptedApplications(ctx, sqldb, delay)
	if err != nil {
		log.Printf("alliance application sweep: %v", err)
		return
	}
	for _, r := range rejected {
		// The corp joined something else while it waited; its acceptance was
		// turned into a rejection and both tabs need to hear it.
		app, _ := evedb.GetAllianceApplication(ctx, sqldb, r.AllianceID, r.CorporationID)
		if app != nil {
			before := *app
			before.State = evedb.AllianceApplicationAccepted
			onAllianceApplicationChanged(ctx, sqldb, sessReg, r.AllianceID, r.CorporationID,
				applicationChangeEntries(&before, app))
		}
	}
	for _, p := range promoted {
		app, _ := evedb.GetAllianceApplication(ctx, sqldb, p.AllianceID, p.CorporationID)
		if app != nil {
			accepted := *app
			accepted.State = evedb.AllianceApplicationAccepted
			onAllianceApplicationChanged(ctx, sqldb, sessReg, p.AllianceID, p.CorporationID,
				applicationChangeEntries(&accepted, app))
		}
		onAllianceMemberChanged(ctx, sqldb, sessReg, p.AllianceID, p.CorporationID,
			memberChangeEntries(p.AllianceID, p.CorporationID, nil, nil, true, false))
		pushAllianceSession(sessReg, p.CorporationID, 0, p.AllianceID)
		announceWarAdoptions(ctx, sqldb, sessReg, p.Adopted)
		log.Printf("alliance %d: corporation %d joined (application effective, executor %d)",
			p.AllianceID, p.CorporationID, p.ExecutorCorpID)
	}
	if len(promoted) > 0 {
		if globalCacheStore != nil {
			globalCacheStore.InvalidateOwnerNames()
			infocache.Shared.Clear() // names live in the identity memo too
		}
	}
}

// allianceSweepSeconds is how often the sweep runs.  The waiting period is
// measured in hours, so a five-minute cadence is precise enough and cheap.
const allianceSweepSeconds = 300

// espAllianceRefresh composes the OnAlliance*Changed push for a panel edit from the current
// database state. The client caches apply an add-shaped dict (all-None olds, one entry per
// column) as an upsert, so the row as it now is is a safe shape for a set; a remove-shaped
// dict deletes.
func espAllianceRefresh(sqldb *sql.DB, sessReg *esp.SessionRegistry, kind string, allianceID, subjectID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	switch kind {

	case "details":
		// The Home tab re-reads what it holds; send the editable columns as an update. The anchor
		// (allianceID, NOT NULL on both sides) pins the dict to the update branch.
		var desc, url sql.NullString
		var executor, dict int64
		if err := sqldb.QueryRowContext(ctx, `
			SELECT description, url, executorCorpID, dictatorial
			  FROM alliances WHERE allianceID = ?`, allianceID,
		).Scan(&desc, &url, &executor, &dict); err != nil {
			return
		}
		onAllianceChanged(ctx, sqldb, sessReg, allianceID, []marshal.DictEntry{
			chg("allianceID", marshal.PyInt{Value: uint64(allianceID)}, marshal.PyInt{Value: uint64(allianceID)}),
			chg("description", marshal.PyString{Value: desc.String}, marshal.PyString{Value: desc.String}),
			chg("url", marshal.PyString{Value: url.String}, marshal.PyString{Value: url.String}),
			chg("executorCorpID", marshal.PyInt{Value: uint64(executor)}, marshal.PyInt{Value: uint64(executor)}),
			chg("dictatorial", marshal.PyInt{Value: uint64(dict)}, marshal.PyInt{Value: uint64(dict)}),
		})

	case "member":
		// Upsert: the member row as it now is, add-shaped.
		var chosen int64
		_ = sqldb.QueryRowContext(ctx,
			`SELECT COALESCE(chosenExecutorID, 0) FROM allianceMembers
			  WHERE allianceID = ? AND corporationID = ?`, allianceID, subjectID).Scan(&chosen)
		var chosenP *int64
		if chosen > 0 {
			chosenP = &chosen
		}
		onAllianceMemberChanged(ctx, sqldb, sessReg, allianceID, subjectID,
			memberChangeEntries(allianceID, subjectID, nil, chosenP, true, false))

	case "member-removed":
		old := int64(0)
		onAllianceMemberChanged(ctx, sqldb, sessReg, allianceID, subjectID,
			memberChangeEntries(allianceID, subjectID, &old, nil, false, true))

	case "rel-set":
		var rel int64
		if err := sqldb.QueryRowContext(ctx,
			`SELECT relationship FROM allianceRelationships WHERE allianceID = ? AND toID = ?`,
			allianceID, subjectID).Scan(&rel); err != nil {
			return
		}
		onAllianceRelationshipChanged(ctx, sqldb, sessReg, allianceID, subjectID, []marshal.DictEntry{
			chg("allianceID", marshal.PyNone{}, marshal.PyInt{Value: uint64(allianceID)}),
			chg("toID", marshal.PyNone{}, marshal.PyInt{Value: uint64(subjectID)}),
			chg("relationship", marshal.PyNone{}, marshal.PyInt{Value: uint64(rel)}),
		})

	case "rel-del":
		onAllianceRelationshipChanged(ctx, sqldb, sessReg, allianceID, subjectID, []marshal.DictEntry{
			chg("allianceID", marshal.PyInt{Value: uint64(allianceID)}, marshal.PyNone{}),
			chg("toID", marshal.PyInt{Value: uint64(subjectID)}, marshal.PyNone{}),
			chg("relationship", marshal.PyInt{Value: 0}, marshal.PyNone{}),
		})

	case "app":
		app, err := evedb.GetAllianceApplication(ctx, sqldb, allianceID, subjectID)
		if err != nil || app == nil {
			return
		}
		onAllianceApplicationChanged(ctx, sqldb, sessReg, allianceID, subjectID,
			applicationChangeEntries(nil, app))

	case "app-removed":
		gone := &evedb.AllianceApplication{AllianceID: allianceID, CorporationID: subjectID}
		onAllianceApplicationChanged(ctx, sqldb, sessReg, allianceID, subjectID,
			applicationChangeEntries(gone, nil))
	}
}

// allianceAppSweepMu serialises the accepted-application sweep: the tick
// manager and the ESP "make effective now" button both run it, and the
// promotion path is not written for two concurrent copies.
var allianceAppSweepMu sync.Mutex

// startAllianceApplicationSweep wires the accepted-application sweep into the
// tick manager.
func startAllianceApplicationSweep(tickMgr *tick.Manager, sqldb *sql.DB, sessReg *esp.SessionRegistry) {
	tickMgr.RegisterPeriodic("alliance-application-sweep", allianceSweepSeconds, func(uint64) bool {
		sweepAllianceApplications(sqldb, sessReg)
		return true
	})
}
