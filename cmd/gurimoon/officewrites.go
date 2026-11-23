package main

// corpStationMgr bound-object methods: office rental, the move-HQ button and corp-junk
// repossession, dispatched via handleCorpStationMgr. See docs/corp-notes.md#offices.
//
// Client contract (lobby.py, hangar.py, bco_locations.py):
//   - GetQuoteForRentingAnOffice() returns a number. RentOffice(cost) returns officeID, tested for
//     truth; OnOfficeRentalChanged must be on the wire before its CallRsp.
//   - OnOfficeRentalChanged(corporationID, officeID, folderID) with both IDs None is the remove form.
//   - Refusals use the client's keys: RentingYouHaveAnOfficeHere, NoOfficesAreAvailableForRenting,
//     RentingAnOfficeCostsMore, RentingOfficeRequestDenied and
//     RentingOfficeQuotesOnlyGivenToActiveCEOsOrEquivale (truncated in the shipped data).

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"time"

	"gurimoon/corp"
	evedb "gurimoon/db"
	"gurimoon/esp"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
	"gurimoon/tick"
)

// corpStationMgrMethods lists the bound methods handleCorpStationMgr owns.
// All are unique across the client's bound objects, so dispatch by name is
// safe (the corpStationMgr moniker binds with the caller's own stationid).
var corpStationMgrMethods = map[string]bool{
	"GetQuoteForRentingAnOffice":       true,
	"RentOffice":                       true,
	"CancelRentOfOffice":               true,
	"MoveCorpHQHere":                   true,
	"DoesPlayersCorpHaveJunkAtStation": true,
	"GetQuoteForGettingCorpJunkBack":   true,
	"PayForReturnOfCorpJunk":           true,
}

// officeUserErr raises one of the client's own office message keys.
func officeUserErr(c *evenet.Conn, pkt *evenet.PyPacket, accountID int64, key string, dict map[string]marshal.PyRep) error {
	return server.SendCallException(c, pkt, accountID, server.BuildUserError(key, dict))
}

// mayManageOffices: renting is CEO / Director / the explicit Rent Office
// role; the client never gates its buttons, so the server is the only gate.
func mayManageOffices(ctx context.Context, sqldb *sql.DB, corpID, charID int64) bool {
	if isCorpCEO(ctx, sqldb, corpID, charID) {
		return true
	}
	roles := callerRoles(ctx, sqldb, charID)
	return corp.HasRole(roles, corp.RoleDirector) || corp.HasRole(roles, corp.RoleCanRentOffice)
}

// pushOfficeRentalChanged fans OnOfficeRentalChanged out to the corp's online members (their
// corp hangar opens or closes on it) and every other pilot docked in the station (the lobby's
// Offices tab redraws). officeID/folderID are PyInt on add, PyNone on remove.
func pushOfficeRentalChanged(sessReg *esp.SessionRegistry, corpID, stationID int64, officeID, folderID marshal.PyRep) {
	args := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(corpID)}, officeID, folderID,
	}}
	notifyCorp(sessReg, corpID, "OnOfficeRentalChanged", args)
	if sessReg == nil {
		return
	}
	for _, s := range sessReg.FindByStationID(stationID) {
		if s.Conn == nil || s.CorpID == corpID {
			continue // corp members already got it above
		}
		if err := server.SendNotification(s.Conn, s.AccountID, "OnOfficeRentalChanged", "charid", args); err != nil {
			log.Printf("[%s] OnOfficeRentalChanged: %v", s.Addr, err)
		}
	}
}

// pushHQSessionChange fans the new hqID out to every online member of the
// corporation.  Nothing else carries it: the client only ever learns hqID from
// a session change, and it is what decides where rolesAtHQ apply.
func pushHQSessionChange(sessReg *esp.SessionRegistry, corpID, oldHQ, newHQ int64) {
	if sessReg == nil {
		return
	}
	for _, s := range sessReg.FindByCorpID(corpID) {
		if s.Conn == nil {
			continue
		}
		if err := sendHQSessionChange(s.Conn, s.Addr, oldHQ, newHQ); err != nil {
			log.Printf("[%s] hqID session change: %v", s.Addr, err)
		}
	}
}

// handleCorpStationMgr owns the corpStationMgrMethods bound calls.
func handleCorpStationMgr(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket,
	accountID, charID int64, stationID *int64, sqldb *sql.DB, sessReg *esp.SessionRegistry) error {

	if sqldb == nil || charID == 0 || stationID == nil || *stationID == 0 {
		log.Printf("[%s] → corpStationMgr.%s: no station context", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	corpID := boundCharCorpID(charID, sqldb)
	if corpID == 0 {
		return corpUserErr(c, pkt, accountID, "You are not in a corporation.")
	}

	switch cs.Method {

	case "GetQuoteForRentingAnOffice":
		// NPC corp members can never rent; their corp is not theirs to spend.
		if npc, _ := evedb.IsNPCCorp(ctx, sqldb, corpID); npc {
			return officeUserErr(c, pkt, accountID, "RentingOfficeQuotesOnlyGivenToActiveCEOsOrEquivale", nil)
		}
		if !mayManageOffices(ctx, sqldb, corpID, charID) {
			return officeUserErr(c, pkt, accountID, "RentingOfficeQuotesOnlyGivenToActiveCEOsOrEquivale", nil)
		}
		if reason := evedb.StationServiceRefusal(ctx, sqldb, *stationID, charID, evedb.StationServiceOfficeRental); reason != "" {
			return officeUserErr(c, pkt, accountID, "RentingOfficeRequestDenied",
				map[string]marshal.PyRep{"reason": marshal.PyString{Value: reason}})
		}
		quote := evedb.OfficeRentQuoteFor(ctx, sqldb, *stationID, corpID)
		log.Printf("[%s] → corpStationMgr.GetQuoteForRentingAnOffice corp=%d station=%d → %.0f",
			addr, corpID, *stationID, quote)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyReal{Value: quote})

	case "RentOffice":
		if npc, _ := evedb.IsNPCCorp(ctx, sqldb, corpID); npc {
			return officeUserErr(c, pkt, accountID, "RentingOfficeRequestDenied",
				map[string]marshal.PyRep{"reason": marshal.PyString{Value: "NPC corporations do not rent offices."}})
		}
		if !mayManageOffices(ctx, sqldb, corpID, charID) {
			return officeUserErr(c, pkt, accountID, "RentingOfficeRequestDenied",
				map[string]marshal.PyRep{"reason": marshal.PyString{Value: "Only the CEO, a Director or a member with the Rent Office role may rent offices."}})
		}
		if officeID, _, _ := evedb.GetCorpOfficeAt(ctx, sqldb, corpID, *stationID); officeID != 0 {
			return officeUserErr(c, pkt, accountID, "RentingYouHaveAnOfficeHere", nil)
		}
		if reason := evedb.StationServiceRefusal(ctx, sqldb, *stationID, charID, evedb.StationServiceOfficeRental); reason != "" {
			return officeUserErr(c, pkt, accountID, "RentingOfficeRequestDenied",
				map[string]marshal.PyRep{"reason": marshal.PyString{Value: reason}})
		}
		quote := evedb.OfficeRentQuoteFor(ctx, sqldb, *stationID, corpID)
		var offered float64
		if len(cs.ArgTuple.Items) >= 1 {
			switch v := cs.ArgTuple.Items[0].(type) {
			case marshal.PyInt:
				offered = float64(int64(v.Value))
			case marshal.PyReal:
				offered = v.Value
			}
		}
		// The client echoes the quote back; a stale (lower) figure is refused
		// with the current price rather than silently charged more.
		if offered < quote {
			return officeUserErr(c, pkt, accountID, "RentingAnOfficeCostsMore",
				map[string]marshal.PyRep{"amount": marshal.PyReal{Value: quote}})
		}
		officeID, folderID, err := evedb.RentOffice(ctx, sqldb, corpID, *stationID, quote)
		switch {
		case errors.Is(err, evedb.ErrOfficeExists):
			return officeUserErr(c, pkt, accountID, "RentingYouHaveAnOfficeHere", nil)
		case errors.Is(err, evedb.ErrNoFreeOffices):
			return officeUserErr(c, pkt, accountID, "NoOfficesAreAvailableForRenting", nil)
		case err != nil:
			log.Printf("[%s] corpStationMgr.RentOffice corp=%d station=%d: %v", addr, corpID, *stationID, err)
			return officeUserErr(c, pkt, accountID, "RentingOfficeRequestDenied",
				map[string]marshal.PyRep{"reason": marshal.PyString{Value: err.Error()}})
		}
		// Notification first: the client's RentOffice handler reads the new
		// office out of the cache this event populates.
		pushOfficeRentalChanged(sessReg, corpID, *stationID,
			marshal.PyInt{Value: uint64(officeID)}, marshal.PyInt{Value: uint64(folderID)})
		log.Printf("[%s] → corpStationMgr.RentOffice corp=%d station=%d → office=%d folder=%d (%.0f ISK)",
			addr, corpID, *stationID, officeID, folderID, quote)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: uint64(officeID)})

	case "CancelRentOfOffice":
		// The client shows UNRENT OFFICE to Directors only; mirror that.
		if !isDirectorOrCEO(ctx, sqldb, corpID, charID) {
			return clientMsgErr(c, pkt, accountID, "CrpOnlyDirectorCanCancelRent", nil)
		}
		officeID, _, contents, err := evedb.ReleaseOffice(ctx, sqldb, corpID, *stationID)
		if err != nil {
			log.Printf("[%s] corpStationMgr.CancelRentOfOffice corp=%d station=%d: %v", addr, corpID, *stationID, err)
			return corpUserErr(c, pkt, accountID, err.Error())
		}
		if officeID == 0 {
			return corpUserErr(c, pkt, accountID, "Your corporation rents no office at this station.")
		}
		pushOfficeRentalChanged(sessReg, corpID, *stationID, marshal.PyNone{}, marshal.PyNone{})
		log.Printf("[%s] → corpStationMgr.CancelRentOfOffice corp=%d station=%d office=%d (%d item(s) impounded)",
			addr, corpID, *stationID, officeID, contents)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})

	case "MoveCorpHQHere":
		if npc, _ := evedb.IsNPCCorp(ctx, sqldb, corpID); npc {
			return corpUserErr(c, pkt, accountID, "An NPC corporation's headquarters cannot be moved.")
		}
		if !isDirectorOrCEO(ctx, sqldb, corpID, charID) {
			return corpUserErr(c, pkt, accountID, "Only the CEO or a Director may move the corporation headquarters.")
		}
		var oldHQ int64
		_ = sqldb.QueryRowContext(ctx,
			`SELECT stationID FROM corporation WHERE corporationID = ?`, corpID).Scan(&oldHQ)
		if oldHQ == *stationID {
			return officeUserErr(c, pkt, accountID, "CorpHQIsAtThisStation", nil)
		}
		// The client's rule (CorpCantSetHQAsNoRealEstateHere): the corp must own the station or an office
		// in it. GetCorpOfficeAt answers the station itself for an owner corp, covering both halves.
		if _, err := evedb.SetCorpHQ(ctx, sqldb, corpID, *stationID); errors.Is(err, evedb.ErrNoRealEstate) {
			return officeUserErr(c, pkt, accountID, "CorpCantSetHQAsNoRealEstateHere", nil)
		} else if err != nil {
			log.Printf("[%s] corpStationMgr.MoveCorpHQHere corp=%d: %v", addr, corpID, err)
			return corpUserErr(c, pkt, accountID, "The headquarters move failed.")
		}
		onCorporationChanged(sessReg, corpID, []marshal.DictEntry{
			chg("stationID",
				marshal.PyInt{Value: uint64(oldHQ)},
				marshal.PyInt{Value: uint64(*stationID)}),
		})
		// hqID is a session value corprole is recomputed from; without this push members keep HQ
		// roles at the old station until relog. See docs/corp-notes.md#hq-session.
		pushHQSessionChange(sessReg, corpID, oldHQ, *stationID)
		log.Printf("[%s] → corpStationMgr.MoveCorpHQHere corp=%d %d → %d", addr, corpID, oldHQ, *stationID)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})

	case "DoesPlayersCorpHaveJunkAtStation":
		// NPC corps own items parked in stations (agent inventory); that is not junk, and their
		// members could not pass the Director gate anyway.
		if npc, _ := evedb.IsNPCCorp(ctx, sqldb, corpID); npc {
			return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: 0})
		}
		n := evedb.CorpJunkCountAt(ctx, sqldb, corpID, *stationID)
		log.Printf("[%s] → corpStationMgr.DoesPlayersCorpHaveJunkAtStation corp=%d station=%d → %d item(s)",
			addr, corpID, *stationID, n)
		if n > 0 {
			return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: 1})
		}
		return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: 0})

	case "GetQuoteForGettingCorpJunkBack":
		quote := evedb.OfficeRentQuote(ctx, sqldb, *stationID)
		log.Printf("[%s] → corpStationMgr.GetQuoteForGettingCorpJunkBack corp=%d station=%d → %.0f",
			addr, corpID, *stationID, quote)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyReal{Value: quote})

	case "PayForReturnOfCorpJunk":
		// The client raises CrpJunkOnlyAvailableToDirector itself; mirror it.
		if !isDirectorOrCEO(ctx, sqldb, corpID, charID) {
			return corpUserErr(c, pkt, accountID, "Repossessed corporate property is only released to a Director.")
		}
		// A locked item stays impounded until the lock is voted off; releasing it would hand the
		// print to the paying director personally.
		if evedb.CorpJunkHasLockedItem(ctx, sqldb, corpID, *stationID) {
			return clientMsgErr(c, pkt, accountID, evedb.LockMsgJunkHasLocked, nil)
		}
		quote := evedb.OfficeRentQuote(ctx, sqldb, *stationID)
		var offered float64
		if len(cs.ArgTuple.Items) >= 1 {
			switch v := cs.ArgTuple.Items[0].(type) {
			case marshal.PyInt:
				offered = float64(int64(v.Value))
			case marshal.PyReal:
				offered = v.Value
			}
		}
		if offered < quote {
			return officeUserErr(c, pkt, accountID, "RentingAnOfficeCostsMore",
				map[string]marshal.PyRep{"amount": marshal.PyReal{Value: quote}})
		}
		ids, err := evedb.ReturnCorpJunk(ctx, sqldb, corpID, *stationID, charID, quote)
		if err != nil {
			log.Printf("[%s] corpStationMgr.PayForReturnOfCorpJunk corp=%d station=%d: %v", addr, corpID, *stationID, err)
			return corpUserErr(c, pkt, accountID, err.Error())
		}
		// The items changed owner corp → payer; OnItemChange lands each in
		// the payer's Items window without a relog.
		for _, id := range ids {
			er, _ := evedb.GetEntityRow(ctx, sqldb, id)
			if er == nil {
				continue
			}
			change := marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyInt{Value: 2}, Value: marshal.PyInt{Value: uint64(corpID)}}, // ixOwnerID old
			}}
			_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
				evedb.BuildEntityRow(er), change,
			}})
		}
		log.Printf("[%s] → corpStationMgr.PayForReturnOfCorpJunk corp=%d station=%d → %d item(s) to char %d (%.0f ISK)",
			addr, corpID, *stationID, len(ids), charID, quote)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}

	log.Printf("[%s] corpStationMgr.%s: unreachable method", addr, cs.Method)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// ─── corp hangar inventory (office containers) ─────────────────────────────

// corpHangarDivisions maps each corp hangar division flag to the roles that may look at it and
// take from it, the same rolesByFlag table as the client's form.CorpHangar. Division 1 is
// flagHangar (4); divisions 2-7 are CorpSAG2-7 (116-121).
var corpHangarDivisions = []struct {
	Flag      int64
	QueryRole uint64
	TakeRole  uint64
}{
	{evedb.FlagCorpHangarDiv1, corp.RoleHangarCanQuery1, corp.RoleHangarCanTake1},
	{evedb.FlagCorpSAGFirst + 0, corp.RoleHangarCanQuery2, corp.RoleHangarCanTake2},
	{evedb.FlagCorpSAGFirst + 1, corp.RoleHangarCanQuery3, corp.RoleHangarCanTake3},
	{evedb.FlagCorpSAGFirst + 2, corp.RoleHangarCanQuery4, corp.RoleHangarCanTake4},
	{evedb.FlagCorpSAGFirst + 3, corp.RoleHangarCanQuery5, corp.RoleHangarCanTake5},
	{evedb.FlagCorpSAGFirst + 4, corp.RoleHangarCanQuery6, corp.RoleHangarCanTake6},
	{evedb.FlagCorpSAGFirst + 5, corp.RoleHangarCanQuery7, corp.RoleHangarCanTake7},
}

// corpHangarQueryFlags returns the division flags charID may view in corpID's
// hangars.  The CEO sees everything regardless of the stored mask.
func corpHangarQueryFlags(ctx context.Context, sqldb *sql.DB, corpID, charID int64) []int64 {
	return corpHangarQueryFlagsAt(ctx, sqldb, corpID, charID, 0)
}

// corpHangarQueryFlagsAt is corpHangarQueryFlags for an office at one station:
// with location roles on (db/locationroles.go) a query role held at Base
// opens the divisions at the Base only.
func corpHangarQueryFlagsAt(ctx context.Context, sqldb *sql.DB, corpID, charID, stationID int64) []int64 {
	ceo := isCorpCEO(ctx, sqldb, corpID, charID)
	roles := evedb.MemberRolesAt(ctx, sqldb, charID, corpID, stationID)
	var out []int64
	for _, d := range corpHangarDivisions {
		if ceo || corp.HasRole(roles, d.QueryRole) {
			out = append(out, d.Flag)
		}
	}
	return out
}

// corpHangarMayTake reports whether charID may take from or put into the given division flag of
// corpID's hangar (the client greys the same drags via CanTake).
func corpHangarMayTake(ctx context.Context, sqldb *sql.DB, corpID, charID, flag int64) bool {
	return corpHangarMayTakeAt(ctx, sqldb, corpID, charID, 0, flag)
}

// repackageCorpItemAllowed is db.RepackageCorpItemAllowed: a corporation item in one of the
// caller's corporation's offices, in a division they may take from, not locked down. It is
// handleOfficeTake's rule, since repackaging moves the item as taking would.
func repackageCorpItemAllowed(sqldb *sql.DB) func(ctx context.Context, charID, itemID, ownerID, locationID, flag int64) bool {
	return func(ctx context.Context, charID, itemID, ownerID, locationID, flag int64) bool {
		officeCorpID, officeStationID, _, err := evedb.GetOfficeByItemID(ctx, sqldb, locationID)
		if err != nil || officeCorpID == 0 || officeCorpID != ownerID || boundCharCorpID(charID, sqldb) != ownerID {
			return false
		}
		if !corpHangarMayTakeAt(ctx, sqldb, ownerID, charID, officeStationID, flag) {
			return false
		}
		return !evedb.IsItemLockedDown(ctx, sqldb, itemID)
	}
}

func init() {
	moduleBootHooks = append(moduleBootHooks, func(sqldb *sql.DB, _ *esp.SessionRegistry, _ *tick.Manager) {
		if sqldb != nil {
			evedb.RepackageCorpItemAllowed = repackageCorpItemAllowed(sqldb)
		}
	})
}

// corpHangarMayTakeAt is corpHangarMayTake for an office at one station.
func corpHangarMayTakeAt(ctx context.Context, sqldb *sql.DB, corpID, charID, stationID, flag int64) bool {
	if isCorpCEO(ctx, sqldb, corpID, charID) {
		return true
	}
	roles := evedb.MemberRolesAt(ctx, sqldb, charID, corpID, stationID)
	for _, d := range corpHangarDivisions {
		if d.Flag == flag {
			return corp.HasRole(roles, d.TakeRole)
		}
	}
	return false
}

// listOfficeContainer answers bound.List for an office folder or a rented
// office.  handled=false means the bind is neither, and List falls through to
// its other branches.
func listOfficeContainer(addr string, contID, charID int64, sqldb *sql.DB) (marshal.PyRep, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if evedb.IsOfficeFolderID(contID) {
		rows, err := evedb.GetOfficeFolderItems(ctx, sqldb, contID)
		if err != nil {
			log.Printf("[%s] bound.List(office folder %d): %v", addr, contID, err)
		}
		log.Printf("[%s] → bound.List(office folder %d) %d office(s)", addr, contID, len(rows))
		return makeInvRowset(invBrokerItemCols, rows), true
	}

	officeCorpID, officeStationID, _, err := evedb.GetOfficeByItemID(ctx, sqldb, contID)
	if err != nil || officeCorpID == 0 {
		return nil, false
	}
	// Only members of the renting corp see contents, and only the divisions their roles allow;
	// the client draws its own hint over an empty list.
	var flags []int64
	if boundCharCorpID(charID, sqldb) == officeCorpID {
		flags = corpHangarQueryFlagsAt(ctx, sqldb, officeCorpID, charID, officeStationID)
	} else if stationOwnerMaySpy(ctx, sqldb, charID, officeStationID) {
		// form.SpyHangar (ALL HANGARS) lists other corps' offices in a station this pilot's corp owns.
		// Its IsMine has no flag test, so the view is every division: the landlord has no division roles.
		flags = allCorpHangarFlags()
	}
	rows, err := evedb.GetOfficeContents(ctx, sqldb, contID, flags)
	if err != nil {
		log.Printf("[%s] bound.List(office %d): %v", addr, contID, err)
	}
	log.Printf("[%s] → bound.List(office %d) corp=%d flags=%v %d item(s)", addr, contID, officeCorpID, flags, len(rows))
	return makeInvRowset(invBrokerItemCols, rows), true
}

// pushOfficeItemChange sends OnItemChange for a corp hangar move to the actor and to every other
// corp member docked in the same station, the only clients that can have the window open.
func pushOfficeItemChange(c *evenet.Conn, accountID int64, sessReg *esp.SessionRegistry,
	corpID, stationID, actorCharID int64, row marshal.PyRep, change marshal.PyDict) {
	args := marshal.PyTuple{Items: []marshal.PyRep{row, change}}
	_ = server.SendNotification(c, accountID, "OnItemChange", "charid", args)
	if sessReg == nil {
		return
	}
	for _, s := range sessReg.FindByCorpID(corpID) {
		if s.Conn == nil || s.CharID == actorCharID || s.StationID != stationID {
			continue
		}
		_ = server.SendNotification(s.Conn, s.AccountID, "OnItemChange", "charid", args)
	}
}

// handleOfficeAdd is bound.Add/MultiAdd arriving on a rented office: deposit an item into a corp
// hangar division. The item becomes corp property. handled=false when the bind is not an office.
func handleOfficeAdd(c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID, charID, stationID int64,
	officeID, itemID, qty, flag int64, respond bool, sqldb *sql.DB, sessReg *esp.SessionRegistry) (bool, error) {

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	officeCorpID, officeStationID, _, err := evedb.GetOfficeByItemID(ctx, sqldb, officeID)
	if err != nil || officeCorpID == 0 {
		return false, nil
	}
	rsp := func(e error) (bool, error) {
		if respond {
			return true, e
		}
		return true, nil
	}

	if boundCharCorpID(charID, sqldb) != officeCorpID || stationID != officeStationID {
		return rsp(corpUserErr(c, pkt, accountID, "That is not your corporation's office."))
	}
	if flag == 0 {
		flag = evedb.FlagCorpHangarDiv1
	}
	if !evedb.IsCorpHangarFlag(flag) {
		return rsp(corpUserErr(c, pkt, accountID, "That is not a corp hangar division."))
	}
	if !corpHangarMayTakeAt(ctx, sqldb, officeCorpID, charID, officeStationID, flag) {
		return rsp(corpUserErr(c, pkt, accountID, "You do not have the hangar role for that division."))
	}
	oldER, err := evedb.GetEntityRow(ctx, sqldb, itemID)
	if err != nil || oldER == nil {
		return rsp(server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}))
	}
	// Moving WITHIN the office (division tab to division tab) is a corp-item
	// move; anything coming in from outside must be the pilot's own property.
	fromInside := oldER.LocationID == officeID
	if !fromInside && oldER.OwnerID != charID {
		return rsp(corpUserErr(c, pkt, accountID, "You can only donate your own items to the corporation hangar."))
	}
	if fromInside && !corpHangarMayTakeAt(ctx, sqldb, officeCorpID, charID, officeStationID, oldER.Flag) {
		return rsp(corpUserErr(c, pkt, accountID, "You do not have the hangar role for that division."))
	}
	// A locked-down print stays in its division (corplockdown.go). Using it
	// for a job is not a move and is unaffected.
	if fromInside && evedb.IsItemLockedDown(ctx, sqldb, itemID) {
		return rsp(clientMsgErr(c, pkt, accountID, evedb.LockMsgItemIsLocked, nil))
	}

	oldLoc, oldFlag, oldOwner, oldQty := oldER.LocationID, oldER.Flag, oldER.OwnerID, oldER.Quantity
	if qty > 0 && qty < oldQty {
		// Split: shrink the source stack, land the split piece in the office.
		if _, err := sqldb.ExecContext(ctx,
			`UPDATE entity SET quantity = ? WHERE itemID = ?`, oldQty-qty, itemID); err != nil {
			log.Printf("[%s] office Add split shrink %d: %v", addr, itemID, err)
			return rsp(server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}))
		}
		if srcER, _ := evedb.GetEntityRow(ctx, sqldb, itemID); srcER != nil {
			srcChange := marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(oldQty)}},
			}}
			_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
				evedb.BuildEntityRow(srcER), srcChange,
			}})
		}
		newItemID, err := evedb.CreateCargoItem(ctx, sqldb, oldER.TypeID, officeCorpID, officeID, qty)
		if err != nil {
			log.Printf("[%s] office Add split create: %v", addr, err)
			return rsp(server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}))
		}
		if _, err := sqldb.ExecContext(ctx,
			`UPDATE entity SET flag = ?, singleton = 0 WHERE itemID = ?`, flag, newItemID); err != nil {
			log.Printf("[%s] office Add split flag: %v", addr, err)
		}
		if newER, _ := evedb.GetEntityRow(ctx, sqldb, newItemID); newER != nil {
			// New item: old locationID 0 (never None, which reads as removal).
			change := marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: 0}},
			}}
			pushOfficeItemChange(c, accountID, sessReg, officeCorpID, stationID, charID,
				evedb.BuildEntityRow(newER), change)
		}
		log.Printf("[%s] → office Add: split %d×%d → office %d flag %d", addr, qty, oldER.TypeID, officeID, flag)
		if h := esp.OfficeDepositHook; h != nil && !fromInside {
			h(officeCorpID, charID, newItemID, oldER.TypeID, flag)
		}
		return rsp(server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}))
	}

	// A ship's fit and cargo become corp property with it (same transaction).
	if _, err := evedb.MoveItemFollowingContents(ctx, sqldb, itemID, officeID, officeCorpID, flag); err != nil {
		log.Printf("[%s] office Add move %d: %v", addr, itemID, err)
		return rsp(server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}))
	}
	if newER, _ := evedb.GetEntityRow(ctx, sqldb, itemID); newER != nil {
		change := marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: uint64(oldLoc)}},
			{Key: marshal.PyInt{Value: 4}, Value: marshal.PyInt{Value: uint64(oldFlag)}},
			{Key: marshal.PyInt{Value: 2}, Value: marshal.PyInt{Value: uint64(oldOwner)}},
		}}
		pushOfficeItemChange(c, accountID, sessReg, officeCorpID, stationID, charID,
			evedb.BuildEntityRow(newER), change)
	}
	log.Printf("[%s] → office Add: item %d → office %d flag %d (corp %d)", addr, itemID, officeID, flag, officeCorpID)
	if h := esp.OfficeDepositHook; h != nil && !fromInside {
		h(officeCorpID, charID, itemID, oldER.TypeID, flag)
	}
	return rsp(server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}))
}

// handleOfficeTakeToHangar is bound.Add/MultiAdd arriving on the station hangar for an item that
// lives inside a corp office: the take-out drag. The item becomes the taker's property, role-gated
// on the source division. handled=false when the item is not in an office.
func handleOfficeTakeToHangar(c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID, charID, stationID int64,
	oldER *evedb.EntityRow, qty int64, respond bool, sqldb *sql.DB, sessReg *esp.SessionRegistry) (bool, error) {
	return handleOfficeTake(c, addr, pkt, accountID, charID, stationID, oldER, qty, true, respond, sqldb, sessReg)
}

// handleOfficeTake is the take-out drag from a corp office into the taker's personal hangar or
// docked ship's cargo hold. Both transfer ownership to the taker under the division's take role,
// since a corp-owned row is invisible in every personal window. handled=false when the item is
// not in an office.
func handleOfficeTake(c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID, charID, stationID int64,
	oldER *evedb.EntityRow, qty int64, toHangar, respond bool, sqldb *sql.DB, sessReg *esp.SessionRegistry) (bool, error) {

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	officeCorpID, officeStationID, _, err := evedb.GetOfficeByItemID(ctx, sqldb, oldER.LocationID)
	if err != nil || officeCorpID == 0 {
		return false, nil
	}
	rsp := func(e error) (bool, error) {
		if respond {
			return true, e
		}
		return true, nil
	}

	if boundCharCorpID(charID, sqldb) != officeCorpID || stationID != officeStationID {
		return rsp(corpUserErr(c, pkt, accountID, "That item belongs to another corporation."))
	}
	if !corpHangarMayTakeAt(ctx, sqldb, officeCorpID, charID, officeStationID, oldER.Flag) {
		return rsp(corpUserErr(c, pkt, accountID, "You do not have the hangar role to take from that division."))
	}
	if evedb.IsItemLockedDown(ctx, sqldb, oldER.ItemID) {
		return rsp(clientMsgErr(c, pkt, accountID, evedb.LockMsgItemIsLocked, nil))
	}

	// Destination: the hangar floor, or the docked ship's cargo hold.
	destLoc, destFlag := stationID, int64(4)
	where := "hangar"
	if !toHangar {
		shipID, _ := evedb.GetCurrentShipID(ctx, sqldb, charID)
		if shipID == 0 {
			return rsp(server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}))
		}
		destLoc, destFlag, where = shipID, 5, "cargo"
		moveQty := qty
		if moveQty <= 0 || moveQty > oldER.Quantity {
			moveQty = oldER.Quantity
		}
		if shipER, _ := evedb.GetEntityRow(ctx, sqldb, shipID); shipER != nil {
			cargoCap, _ := evedb.GetShipCargoCapacityFitted(ctx, sqldb, shipER.TypeID, shipID)
			usedVol, _ := evedb.GetCargoUsedVolume(ctx, sqldb, charID, shipID)
			need := evedb.ItemVolume(ctx, sqldb, oldER.ItemID, oldER.TypeID) * float64(moveQty)
			if need > cargoCap-usedVol {
				return rsp(server.SendCallException(c, pkt, accountID, server.BuildUserError(
					"NotEnoughCargoSpace", map[string]marshal.PyRep{
						"volume":    marshal.PyReal{Value: need},
						"available": marshal.PyReal{Value: cargoCap - usedVol},
					})))
			}
		}
	}

	oldLoc, oldFlag, oldOwner, oldQty := oldER.LocationID, oldER.Flag, oldER.OwnerID, oldER.Quantity
	if qty > 0 && qty < oldQty {
		if _, err := sqldb.ExecContext(ctx,
			`UPDATE entity SET quantity = ? WHERE itemID = ?`, oldQty-qty, oldER.ItemID); err != nil {
			log.Printf("[%s] office take split shrink %d: %v", addr, oldER.ItemID, err)
			return rsp(server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}))
		}
		if srcER, _ := evedb.GetEntityRow(ctx, sqldb, oldER.ItemID); srcER != nil {
			srcChange := marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(oldQty)}},
			}}
			pushOfficeItemChange(c, accountID, sessReg, officeCorpID, stationID, charID,
				evedb.BuildEntityRow(srcER), srcChange)
		}
		newItemID, err := evedb.CreateCargoItem(ctx, sqldb, oldER.TypeID, charID, destLoc, qty)
		if err != nil {
			log.Printf("[%s] office take split create: %v", addr, err)
			return rsp(server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}))
		}
		if _, err := sqldb.ExecContext(ctx,
			`UPDATE entity SET flag = ?, singleton = 0 WHERE itemID = ?`, destFlag, newItemID); err != nil {
			log.Printf("[%s] office take split flag: %v", addr, err)
		}
		if newER, _ := evedb.GetEntityRow(ctx, sqldb, newItemID); newER != nil {
			change := marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: 0}},
			}}
			_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
				evedb.BuildEntityRow(newER), change,
			}})
			mergeLandedStack(ctx, c, addr, accountID, newItemID, sqldb)
		}
		log.Printf("[%s] → office take: split %d×%d → char %d %s", addr, qty, oldER.TypeID, charID, where)
		if h := esp.OfficeTakeHook; h != nil {
			h(officeCorpID, charID, officeStationID, oldER.TypeID, qty, oldFlag)
		}
		return rsp(server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}))
	}

	// A ship's fit and cargo become the taker's with it (same transaction).
	if _, err := evedb.MoveItemFollowingContents(ctx, sqldb, oldER.ItemID, destLoc, charID, destFlag); err != nil {
		log.Printf("[%s] office take move %d: %v", addr, oldER.ItemID, err)
		return rsp(server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}))
	}
	if newER, _ := evedb.GetEntityRow(ctx, sqldb, oldER.ItemID); newER != nil {
		change := marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: uint64(oldLoc)}},
			{Key: marshal.PyInt{Value: 4}, Value: marshal.PyInt{Value: uint64(oldFlag)}},
			{Key: marshal.PyInt{Value: 2}, Value: marshal.PyInt{Value: uint64(oldOwner)}},
		}}
		pushOfficeItemChange(c, accountID, sessReg, officeCorpID, stationID, charID,
			evedb.BuildEntityRow(newER), change)
		mergeLandedStack(ctx, c, addr, accountID, oldER.ItemID, sqldb)
	}
	log.Printf("[%s] → office take: item %d office %d → char %d %s", addr, oldER.ItemID, oldLoc, charID, where)
	if h := esp.OfficeTakeHook; h != nil {
		h(officeCorpID, charID, officeStationID, oldER.TypeID, oldQty, oldFlag)
	}
	return rsp(server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}))
}

// allCorpHangarFlags lists every corp hangar division flag.
func allCorpHangarFlags() []int64 {
	out := make([]int64, 0, len(corpHangarDivisions))
	for _, d := range corpHangarDivisions {
		out = append(out, d.Flag)
	}
	return out
}

// stationOwnerMaySpy reports whether charID may look inside another corp's office at stationID:
// ALL HANGARS mode (ui/station/hangar.py:121), drawn for a Station Manager whose corp owns the
// station. Security Officer is required too: the button is gated on it and ShowHangars
// re-raises CrpAccessDenied without it.
func stationOwnerMaySpy(ctx context.Context, sqldb *sql.DB, charID, stationID int64) bool {
	if sqldb == nil || charID == 0 || stationID == 0 {
		return false
	}
	viewerCorp := boundCharCorpID(charID, sqldb)
	if viewerCorp == 0 || evedb.StationOwnerCorp(ctx, sqldb, stationID) != viewerCorp {
		return false
	}
	if isCorpCEO(ctx, sqldb, viewerCorp, charID) {
		return true
	}
	roles := callerRoles(ctx, sqldb, charID)
	return corp.HasRole(roles, corp.RoleSecurityOfficer) && corp.HasRole(roles, corp.RoleStationManager)
}
