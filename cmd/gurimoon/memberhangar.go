package main

// Member hangar viewing: the station hangar window's MEMBER HANGARS / ALL HANGARS button
// (ui/station/hangar.py). The server answers GetEveOwners, the
// GetInventory(containerHangar, ownerID) pick and SpyHangar's office listing.
//
// See docs/corp-notes.md#member-hangars.

import (
	"context"
	"database/sql"
	"log"
	"time"

	"gurimoon/corp"
	evedb "gurimoon/db"
	"gurimoon/marshal"
	"gurimoon/syncx"
)

// memberHangarBinds holds the bind numbers handed out for another owner's station hangar.
// The bind number is the target ownerID; membership is a routing test only, and List
// re-runs the role check against the caller. See docs/corp-notes.md#member-hangars.
var memberHangarBinds syncx.Map[int64, struct{}] // int64 ownerID → struct{}

func rememberMemberHangarBind(ownerID int64) {
	if ownerID > 0 {
		memberHangarBinds.Store(ownerID, struct{}{})
	}
}

// isMemberHangarBind reports whether bindNum was handed out as a member hangar bind. The
// mutating inventory verbs refuse these binds, since both client windows are oneWay.
func isMemberHangarBind(bindNum int64) bool {
	if bindNum <= 0 {
		return false
	}
	_, ok := memberHangarBinds.Load(bindNum)
	return ok
}

// isKnownOwnerID reports whether id names a character or a corporation. It keeps an
// arbitrary number, such as the ordinary hangar bind, out of memberHangarBinds.
func isKnownOwnerID(ctx context.Context, sqldb *sql.DB, id int64) bool {
	if sqldb == nil || id <= 0 {
		return false
	}
	var n int
	err := sqldb.QueryRowContext(ctx, `
		SELECT 1 FROM character_   WHERE characterID   = ?
		 UNION ALL
		SELECT 1 FROM corporation  WHERE corporationID = ?
		 LIMIT 1`, id, id).Scan(&n)
	return err == nil
}

// resolveMemberHangarTarget decides whether viewerID may look at targetID's hangar floor at
// stationID and returns the bind number to hand out (0 = no). Security Officer reaches any
// player in the viewer's corporation; Security Officer plus Station Manager of the owning
// corporation reaches any owner's floor there. The viewer's own hangar answers 0.
func resolveMemberHangarTarget(sqldb *sql.DB, viewerID, targetID, stationID int64) int64 {
	if sqldb == nil || viewerID == 0 || targetID <= 0 || stationID == 0 || targetID == viewerID {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	viewerCorp := boundCharCorpID(viewerID, sqldb)
	if viewerCorp == 0 {
		return 0
	}
	// The CEO holds every role whether or not the roles table says so.
	ceo := isCorpCEO(ctx, sqldb, viewerCorp, viewerID)
	roles := callerRoles(ctx, sqldb, viewerID)
	if !ceo && !corp.HasRole(roles, corp.RoleSecurityOfficer) {
		return 0
	}

	// MEMBER HANGARS: a character in the caller's own corporation. boundCharCorpID answers 0 for
	// anything that is not a character row, which keeps corporations and offices out.
	if boundCharCorpID(targetID, sqldb) == viewerCorp {
		return targetID
	}
	// ALL HANGARS: the landlord sees every floor in a station its corp owns.
	if ceo || corp.HasRole(roles, corp.RoleStationManager) {
		if evedb.StationOwnerCorp(ctx, sqldb, stationID) == viewerCorp &&
			isKnownOwnerID(ctx, sqldb, targetID) {
			return targetID
		}
	}
	return 0
}

// memberHangarBindNum validates a GetInventory(containerHangar, ownerID) pick and returns the
// bind number to answer with, remembering it for List. 0 means the caller keeps the ordinary
// hangar bind.
func memberHangarBindNum(addr string, viewerID, ownerID, stationID int64, sqldb *sql.DB) int64 {
	bindNum := resolveMemberHangarTarget(sqldb, viewerID, ownerID, stationID)
	if bindNum == 0 {
		if ownerID != 0 && ownerID != viewerID {
			log.Printf("[%s] invbroker.GetInventory(containerHangar, %d): refused for char %d at station %d",
				addr, ownerID, viewerID, stationID)
		}
		return 0
	}
	rememberMemberHangarBind(bindNum)
	log.Printf("[%s] → invbroker.GetInventory(member hangar) owner=%d station=%d viewer=%d",
		addr, ownerID, stationID, viewerID)
	return bindNum
}

// listMemberHangar answers bound.List for a member hangar bind; handled=false falls through
// to the other List branches. Authorization is re-resolved on every call. A refusal answers
// the empty rowset, which the open window draws as an empty floor.
func listMemberHangar(addr string, bindNum, viewerID, stationID int64, sqldb *sql.DB) (marshal.PyRep, bool) {
	if !isMemberHangarBind(bindNum) {
		return nil, false
	}
	if resolveMemberHangarTarget(sqldb, viewerID, bindNum, stationID) == 0 {
		log.Printf("[%s] bound.List(member hangar %d): refused for char %d at station %d",
			addr, bindNum, viewerID, stationID)
		return makeInvRowset(invBrokerItemCols, nil), true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Flag 4 at this station, which is what CorpMemberHangar.IsMine keeps.
	r, err := evedb.GetCharHangarItems(ctx, sqldb, bindNum, stationID)
	if err != nil {
		log.Printf("[%s] bound.List(member hangar %d): %v", addr, bindNum, err)
		return makeInvRowset(invBrokerItemCols, nil), true
	}
	log.Printf("[%s] → bound.List(member hangar) owner=%d station=%d viewer=%d",
		addr, bindNum, stationID, viewerID)
	return r, true
}

// corpEveOwnersRep answers corpRegistry.GetEveOwners for the caller. Any failure answers the
// empty rowset: bco_members iterates it unconditionally and a None kills the corp service.
func corpEveOwnersRep(addr string, charID int64, sqldb *sql.DB) marshal.PyRep {
	empty := makeInvRowset([]string{"ownerID", "ownerName", "typeID"}, nil)
	if sqldb == nil || charID == 0 {
		return empty
	}
	corpID := boundCharCorpID(charID, sqldb)
	if corpID == 0 {
		log.Printf("[%s] → corpRegistry.GetEveOwners charID=%d: no corporation", addr, charID)
		return empty
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := evedb.CorpMemberOwners(ctx, sqldb, corpID)
	if err != nil {
		log.Printf("[%s] corpRegistry.GetEveOwners corpID=%d: %v", addr, corpID, err)
		return empty
	}
	log.Printf("[%s] → corpRegistry.GetEveOwners corpID=%d", addr, corpID)
	return r
}
