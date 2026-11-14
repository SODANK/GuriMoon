package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	evedb "gurimoon/db"
	"gurimoon/marshal"
	_ "gurimoon/modules"
	"gurimoon/server"
	"gurimoon/server/slash"
)

// boundGetCorporation handles the bound call "GetCorporation".
func boundGetCorporation(cc *callCtx) error {
	c, addr, pkt, accountID, charID, sqldb := cc.c, cc.addr, cc.pkt, cc.accountID, cc.charID, cc.sqldb
	// corpRegistry bound: the character's corporation as a util.Row
	// (CorpRegistryBound::Handle_GetCorporation). bco_corporations.py reads .header on it.
	if charID != 0 && sqldb != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		charData, err := evedb.LoadCharData(ctx, sqldb, charID)
		if err != nil {
			log.Printf("[%s] bound.GetCorporation LoadCharData: %v (returning None)", addr, err)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		corp, err := evedb.GetCorporation(ctx, sqldb, charData.CorporationID)
		if err != nil {
			log.Printf("[%s] bound.GetCorporation GetCorporation(%d): %v (returning None)", addr, charData.CorporationID, err)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		log.Printf("[%s] → bound.GetCorporation corpID=%d", addr, charData.CorporationID)
		return server.SendCallRsp(c, pkt, accountID, corp)
	}
	log.Printf("[%s] → bound.GetCorporation (no charID, returning None)", addr)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// boundGetCorporateStationInfo handles the bound call "GetCorporateStationInfo".
func boundGetCorporateStationInfo(cc *callCtx) error {
	c, addr, pkt, accountID, stationID, sqldb := cc.c, cc.addr, cc.pkt, cc.accountID, cc.stationID, cc.sqldb
	// corpStationMgr bound: returns list[eveowners, corporations, offices].
	// bco_locations.py:111 unpacks it as (owners, corps, offices).
	corpCols := []string{
		"corporationID", "corporationName", "description", "shares", "graphicID",
		"memberCount", "ceoID", "stationID", "raceID", "corporationType", "creatorID",
		"hasPlayerPersonnelManager", "tickerName", "sendCharTerminationMessage",
		"shape1", "shape2", "shape3", "color1", "color2", "color3", "typeface",
		"memberLimit", "allowedMemberRaceIDs", "url", "taxRate", "minimumJoinStanding",
		"division1", "division2", "division3", "division4", "division5", "division6",
		"division7", "allianceID", "deleted",
	}
	ownerCols := []string{"ownerID", "ownerName", "typeID"}
	officeCols := []string{"corporationID", "itemID", "officeFolderID"}
	var ownerRows, corpRows, officeRows [][]marshal.PyRep
	if stationID != nil && *stationID != 0 && sqldb != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		o, cr, of, err := evedb.GetStationCorpOffices(ctx, sqldb, *stationID)
		if err != nil {
			log.Printf("[%s] bound.GetCorporateStationInfo: %v (returning empty)", addr, err)
		} else {
			ownerRows, corpRows, officeRows = o, cr, of
		}
	}
	log.Printf("[%s] → bound.GetCorporateStationInfo stationID=%d offices=%d", addr, *stationID, len(officeRows))
	return server.SendCallRsp(c, pkt, accountID, marshal.PyList{Items: []marshal.PyRep{
		makeInvRowset(ownerCols, ownerRows),
		makeInvRowset(corpCols, corpRows),
		makeInvRowset(officeCols, officeRows),
	}})
}

// boundGetMembers handles the bound call "GetMembers".
func boundGetMembers(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, sqldb := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.sqldb
	// Must be a util.SparseRowset (see GetOffices): the Members/Role
	// Management panels page it with Fetch/FetchByKey/GetByKey.
	// Arg[0] may carry the corpID explicitly; fall back to looking up from charID.
	var corpID int64
	if len(cs.ArgTuple.Items) >= 1 {
		if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
			corpID = int64(v.Value)
		}
	}
	if corpID == 0 {
		corpID = boundCharCorpID(charID, sqldb)
	}
	if corpID == 0 {
		log.Printf("[%s] bound.GetMembers: cannot determine corpID", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	ctx3, cancel3 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel3()
	count := evedb.CountCorpMembers(ctx3, sqldb, corpID)
	bindSpec := fmt.Sprintf("N=%d:%d", globalNodeID, membersBindID)
	log.Printf("[%s] → bound.GetMembers corpID=%d (sparse, %d rows, %q)", addr, corpID, count, bindSpec)
	return server.SendCallRsp(c, pkt, accountID, evedb.MakeMembersSparseRowset(bindSpec, count))
}

// boundGetMemberIDsByQuery handles the bound call "GetMemberIDsByQuery".
func boundGetMemberIDsByQuery(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, sqldb := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.sqldb
	// Feeds member-picker lists; without a query engine we return the
	// full membership, which is a superset the client filters client-side.
	corpID := boundCharCorpID(charID, sqldb)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ids, err := evedb.GetCorpMemberIDs(ctx, sqldb, corpID)
	if err != nil {
		log.Printf("[%s] bound.%s db: %v", addr, cs.Method, err)
	}
	items := make([]marshal.PyRep, 0, len(ids))
	for _, id := range ids {
		items = append(items, marshal.PyInt{Value: uint64(id)})
	}
	log.Printf("[%s] → bound.%s corpID=%d (%d ids)", addr, cs.Method, corpID, len(ids))
	return server.SendCallRsp(c, pkt, accountID, marshal.PyList{Items: items})
}

// boundFetchByKey handles the bound call "FetchByKey".
func boundFetchByKey(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, sqldb := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.sqldb
	// Returns [(pk, rowIndex, line), ...] for the Members SparseRowset.
	corpID := boundCharCorpID(charID, sqldb)
	var keys []int64
	if len(cs.ArgTuple.Items) >= 1 {
		if lst, ok := cs.ArgTuple.Items[0].(marshal.PyList); ok {
			for _, it := range lst.Items {
				if v, ok := it.(marshal.PyInt); ok {
					keys = append(keys, int64(v.Value))
				}
			}
		} else if tup, ok := cs.ArgTuple.Items[0].(marshal.PyTuple); ok {
			for _, it := range tup.Items {
				if v, ok := it.(marshal.PyInt); ok {
					keys = append(keys, int64(v.Value))
				}
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	log.Printf("[%s] → bound.FetchByKey (members) corpID=%d keys=%d", addr, corpID, len(keys))
	return server.SendCallRsp(c, pkt, accountID, evedb.FetchCorpMembersByKey(ctx, sqldb, corpID, keys))
}

// boundCanBeKickedOut handles the bound call "CanBeKickedOut".
func boundCanBeKickedOut(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, sqldb := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.sqldb
	// False for the CEO and for members above the average share; those need a
	// kick vote.
	target := boundArgInt(cs, 0)
	kickCorpID := boundCharCorpID(charID, sqldb)
	ctxK, cancelK := context.WithTimeout(context.Background(), 5*time.Second)
	can := 1
	if isCorpCEO(ctxK, sqldb, kickCorpID, target) {
		can = 0
	} else {
		for _, id := range evedb.GetMemberIDsWithMoreThanAvgShares(ctxK, sqldb, kickCorpID) {
			if id == target {
				can = 0
				break
			}
		}
	}
	cancelK()
	log.Printf("[%s] → bound.CanBeKickedOut target=%d → %d", addr, target, can)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: uint64(can)})
}

// boundGetAlliance handles the bound call "GetAlliance".
func boundGetAlliance(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, sqldb := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.sqldb
	// Arg 0 may carry the allianceID; otherwise it comes from the caller's corp.
	var allianceID int64
	if len(cs.ArgTuple.Items) >= 1 {
		if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
			allianceID = int64(v.Value)
		}
	}
	if allianceID == 0 && charID != 0 && sqldb != nil {
		ctxA, cancelA := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelA()
		var aid sql.NullInt64
		_ = sqldb.QueryRowContext(ctxA, `
				SELECT c.allianceID
				FROM character_ ch
				JOIN corporation c ON c.corporationID = ch.corporationID
				WHERE ch.characterID = ?`, charID,
		).Scan(&aid)
		if aid.Valid {
			allianceID = aid.Int64
		}
	}
	if allianceID == 0 {
		log.Printf("[%s] bound.GetAlliance: no allianceID", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	ctxB, cancelB := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelB()
	allianceRow, err := evedb.GetAllianceInfo(ctxB, sqldb, allianceID)
	if err != nil {
		log.Printf("[%s] bound.GetAlliance: allianceID=%d: %v", addr, allianceID, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	log.Printf("[%s] → bound.GetAlliance allianceID=%d", addr, allianceID)
	return server.SendCallRsp(c, pkt, accountID, allianceRow)
}

// boundInsertApplication handles the bound call "InsertApplication".
func boundInsertApplication(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, sqldb, sessReg := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.sqldb, cc.sessReg
	// Args: (corpID int, message string)
	if len(cs.ArgTuple.Items) < 2 {
		log.Printf("[%s] bound.InsertApplication: too few args", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	corpIDArg, ok1 := cs.ArgTuple.Items[0].(marshal.PyInt)
	msgArg, ok2 := cs.ArgTuple.Items[1].(marshal.PyString)
	if !ok1 || !ok2 {
		log.Printf("[%s] bound.InsertApplication: bad arg types", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	targetCorpID := int64(corpIDArg.Value)
	{
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := evedb.InsertApplication(ctx, sqldb, charID, targetCorpID, msgArg.Value); err != nil {
			log.Printf("[%s] bound.InsertApplication db: %v", addr, err)
			return server.SendCallException(c, pkt, accountID, nil)
		}
		// Applications to NPC corps are accepted at once.
		isNPC, err := evedb.IsNPCCorp(ctx, sqldb, targetCorpID)
		if err != nil {
			log.Printf("[%s] bound.InsertApplication IsNPCCorp: %v", addr, err)
		}
		if isNPC {
			oldCorpID, err := evedb.GetCharCorpID(ctx, sqldb, charID)
			if err != nil {
				log.Printf("[%s] bound.InsertApplication GetCharCorpID: %v", addr, err)
			} else if oldCorpID != targetCorpID {
				if roles, err := evedb.JoinCorporation(ctx, sqldb, charID, targetCorpID, oldCorpID); err != nil {
					log.Printf("[%s] bound.InsertApplication JoinCorporation: %v", addr, err)
				} else {
					_ = evedb.DeleteApplication(ctx, sqldb, charID, targetCorpID)
					log.Printf("[%s] bound.InsertApplication: NPC auto-accept charID=%d %d→%d", addr, charID, oldCorpID, targetCorpID)
					if err := slash.SendCorpSessionChange(c, oldCorpID, targetCorpID, roles); err != nil {
						log.Printf("[%s] bound.InsertApplication SCN: %v", addr, err)
					}
					_ = slash.SendCorpAllianceSessionChange(ctx, sqldb, c, oldCorpID, targetCorpID)
				}
			}
		}
	}
	// Push an ADD to the corp and to the applicant: args (applicantID,
	// corporationID, change), the change carrying every column.
	// See docs/main-s6-notes.md#application-pushes.
	{
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		change, ok, err := evedb.ApplicationAddChange(ctx, sqldb, charID, targetCorpID)
		if err != nil {
			log.Printf("[%s] bound.InsertApplication read-back: %v", addr, err)
		}
		if ok {
			args := marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyInt{Value: uint64(charID)},
				marshal.PyInt{Value: uint64(targetCorpID)},
				change,
			}}
			notifyCorp(sessReg, targetCorpID, "OnCorporationApplicationChanged", args)
			server.NotifyCharacter(sessReg, charID, "OnCorporationApplicationChanged", "charid", args)
		}
	}
	log.Printf("[%s] → bound.InsertApplication charID=%d corpID=%d", addr, charID, targetCorpID)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// boundDeleteApplication handles the bound call "DeleteApplication".
func boundDeleteApplication(cc *callCtx) error {
	c, addr, cs, pkt, accountID, sqldb := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.sqldb
	// Args: (corpID int, charID int)
	if len(cs.ArgTuple.Items) < 2 {
		log.Printf("[%s] bound.DeleteApplication: too few args", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: 0})
	}
	delCorpID, ok1 := cs.ArgTuple.Items[0].(marshal.PyInt)
	delCharID, ok2 := cs.ArgTuple.Items[1].(marshal.PyInt)
	if !ok1 || !ok2 {
		log.Printf("[%s] bound.DeleteApplication: bad arg types", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: 0})
	}
	{
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := evedb.DeleteApplication(ctx, sqldb, int64(delCharID.Value), int64(delCorpID.Value)); err != nil {
			log.Printf("[%s] bound.DeleteApplication db: %v", addr, err)
			return server.SendCallException(c, pkt, accountID, nil)
		}
	}
	log.Printf("[%s] → bound.DeleteApplication charID=%d corpID=%d", addr, delCharID.Value, delCorpID.Value)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: 1})
}

// boundUpdateApplication handles the bound call "UpdateApplication".
func boundUpdateApplication(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, sqldb, sessReg := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.sqldb, cc.sessReg
	// Args: (corpID int, message string, status int, date raw). Applicants
	// withdraw; directors accept or reject.
	if len(cs.ArgTuple.Items) < 3 {
		log.Printf("[%s] bound.UpdateApplication: too few args", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: 0})
	}
	upCorpID, ok1 := cs.ArgTuple.Items[0].(marshal.PyInt)
	upMsg, ok2 := cs.ArgTuple.Items[1].(marshal.PyString)
	upStatus, ok3 := cs.ArgTuple.Items[2].(marshal.PyInt)
	if !ok1 || !ok2 || !ok3 {
		log.Printf("[%s] bound.UpdateApplication: bad arg types", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: 0})
	}
	{
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		targetCorp := int64(upCorpID.Value)
		newStatus := int64(upStatus.Value)
		// The applicant's accept is the join: the client wraps the call in
		// PerformSessionChange and waits for the corp session change.
		if newStatus == evedb.AppAcceptedByCharacter {
			var curStatus int64
			err := sqldb.QueryRowContext(ctx, `
					SELECT status FROM chrApplications
					WHERE corporationID = ? AND characterID = ? AND deleted = 0`,
				targetCorp, charID).Scan(&curStatus)
			if err != nil || curStatus != evedb.AppAcceptedByCorporation {
				log.Printf("[%s] bound.UpdateApplication: accept without corp offer (status=%d, err=%v)", addr, curStatus, err)
				return server.SendCallException(c, pkt, accountID, server.BuildUserError(
					"CustomError", map[string]marshal.PyRep{
						"error": marshal.PyString{Value: "The corporation has not accepted your application yet."},
					}))
			}
			// A corp that filled while its offer sat unanswered refuses the join
			// (evedb.CorpHasOpening).
			if ok, used, limit, berr := evedb.CorpHasOpening(ctx, sqldb, targetCorp); berr == nil && !ok {
				return clientMsgErr(c, pkt, accountID, "CorporationMembershipFull", map[string]marshal.PyRep{
					"limit": marshal.PyInt{Value: uint64(limit)},
					"count": marshal.PyInt{Value: uint64(used)},
				})
			}
			oldCorpID := boundCharCorpID(charID, sqldb)
			roles, jerr := evedb.JoinCorporation(ctx, sqldb, charID, targetCorp, oldCorpID)
			if jerr != nil {
				log.Printf("[%s] bound.UpdateApplication join: %v", addr, jerr)
				return server.SendCallException(c, pkt, accountID, nil)
			}
			_ = evedb.UpdateApplication(ctx, sqldb, charID, targetCorp, charID, newStatus, upMsg.Value)
			_ = evedb.SoftDeleteApplication(ctx, sqldb, charID, targetCorp)
			if err := slash.SendCorpSessionChange(c, oldCorpID, targetCorp, roles); err != nil {
				log.Printf("[%s] bound.UpdateApplication SCN: %v", addr, err)
			}
			_ = slash.SendCorpAllianceSessionChange(ctx, sqldb, c, oldCorpID, targetCorp)
			// REMOVE goes to both lists: a corpid change resets only the corp-side cache.
			rem := appRemoveNotification(targetCorp, charID, evedb.AppAcceptedByCorporation)
			notifyCorp(sessReg, targetCorp, "OnCorporationApplicationChanged", rem)
			server.NotifyCharacter(sessReg, charID, "OnCorporationApplicationChanged", "charid", rem)
			log.Printf("[%s] → bound.UpdateApplication charID=%d JOINED corpID=%d", addr, charID, targetCorp)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: 1})
		}
		oldStatus := evedb.GetApplicationStatus(ctx, sqldb, charID, targetCorp)
		if err := evedb.UpdateApplication(ctx, sqldb, charID, targetCorp, charID, newStatus, upMsg.Value); err != nil {
			log.Printf("[%s] bound.UpdateApplication db: %v", addr, err)
			return server.SendCallException(c, pkt, accountID, nil)
		}
		if evedb.IsTerminalAppStatus(newStatus) {
			// Terminal status: soft-delete and REMOVE on both sides.
			_ = evedb.SoftDeleteApplication(ctx, sqldb, charID, targetCorp)
			rem := appRemoveNotification(targetCorp, charID, oldStatus)
			notifyCorp(sessReg, targetCorp, "OnCorporationApplicationChanged", rem)
			server.NotifyCharacter(sessReg, charID, "OnCorporationApplicationChanged", "charid", rem)
		} else {
			// Non-terminal status: UPDATE to the corp and the applicant, args
			// (applicantID, corporationID, change).
			upd := marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyInt{Value: uint64(charID)},
				marshal.PyInt{Value: uint64(targetCorp)},
				marshal.PyDict{Items: []marshal.DictEntry{
					{Key: marshal.PyString{Value: "status"}, Value: marshal.PyTuple{Items: []marshal.PyRep{marshal.PyInt{Value: uint64(oldStatus)}, marshal.PyInt{Value: uint64(newStatus)}}}},
				}},
			}}
			notifyCorp(sessReg, targetCorp, "OnCorporationApplicationChanged", upd)
			server.NotifyCharacter(sessReg, charID, "OnCorporationApplicationChanged", "charid", upd)
		}
	}
	log.Printf("[%s] → bound.UpdateApplication charID=%d corpID=%d status=%d", addr, charID, upCorpID.Value, upStatus.Value)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: 1})
}

// boundGetRelationships handles the bound call "GetRelationships".
func boundGetRelationships(cc *callCtx) error {
	c, addr, pkt, accountID := cc.c, cc.addr, cc.pkt, cc.accountID
	// corpRegistry/allianceRegistry bound: relationships for overview colours
	// and war indicators. Answers an empty util.Rowset so the client gets no None.
	log.Printf("[%s] → bound.GetRelationships (empty)", addr)
	emptyRowset := marshal.PyObject{
		Type: "util.Rowset",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "header"}, Value: marshal.PyList{Items: []marshal.PyRep{
				marshal.PyString{Value: "toID"},
				marshal.PyString{Value: "relationship"},
			}}},
			{Key: marshal.PyString{Value: "RowClass"}, Value: marshal.PyString{Value: "util.Row", IsType1: true}},
			{Key: marshal.PyString{Value: "lines"}, Value: marshal.PyList{}},
		}},
	}
	return server.SendCallRsp(c, pkt, accountID, emptyRowset)
}
