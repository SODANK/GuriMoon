package main

// Escrow item exchange handlers. The Missions window's Escrow tab browses with
// missionMgr.GetAvailableEscrows and claims with AcceptCourierMission. Creation arrives as
// CreateCourierMission with reward=0 and dropStation=pickupStation.

import (
	"context"
	"database/sql"
	"log"
	"time"

	evedb "gurimoon/db"
	evenet "gurimoon/net"
	"gurimoon/marshal"
	"gurimoon/server"

	"gurimoon/esp"
)

// buildAvailableEscrows handles missionMgr.GetAvailableEscrows.
// Args: (maxCollateral, regionID, solsysID, stationID, ownerID)
// Returns: util.KeyVal{escrows: [rows...], names: {id: "name"}}
func buildAvailableEscrows(ctx context.Context, addr string, sqldb *sql.DB,
	charID, stationID int64, args marshal.PyTuple) marshal.PyRep {

	var maxPrice float64
	var regionID, sysID, filterStation, ownerID int64

	if len(args.Items) >= 1 {
		maxPrice = repToFloat(args.Items[0])
	}
	if len(args.Items) >= 2 {
		regionID = repToInt(args.Items[1])
	}
	if len(args.Items) >= 3 {
		sysID = repToInt(args.Items[2])
	}
	if len(args.Items) >= 4 {
		filterStation = repToInt(args.Items[3])
	}
	if len(args.Items) >= 5 {
		ownerID = repToInt(args.Items[4])
	}

	corpID, _ := evedb.GetCharCorpID(ctx, sqldb, charID)

	listings, err := evedb.ListAvailableEscrows(ctx, sqldb,
		maxPrice, regionID, sysID, filterStation, ownerID, charID, corpID)
	if err != nil {
		log.Printf("[%s] GetAvailableEscrows: %v", addr, err)
		return emptyEscrowResult()
	}

	idSet := map[int64]bool{}
	for _, l := range listings {
		idSet[l.IssuerID] = true
		if l.AssigneeID != 0 {
			idSet[l.AssigneeID] = true
		}
		idSet[l.StationID] = true
	}
	var ids []int64
	for id := range idSet {
		ids = append(ids, id)
	}
	names := evedb.ResolveEscrowNames(ctx, sqldb, ids)

	rows := make([]marshal.PyRep, 0, len(listings))
	for _, l := range listings {
		expires := time.Unix(l.DateExpires, 0)

		var assigneeRep marshal.PyRep = marshal.PyNone{}
		if l.AssigneeID != 0 {
			assigneeRep = marshal.PyInt{Value: uint64(l.AssigneeID)}
		}

		rows = append(rows, marshal.PyObject{
			Type: "util.KeyVal",
			Args: marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyString{Value: "escrowID"}, Value: marshal.PyInt{Value: uint64(l.MissionID)}},
				{Key: marshal.PyString{Value: "missionID"}, Value: marshal.PyInt{Value: uint64(l.MissionID)}},
				{Key: marshal.PyString{Value: "dateExpires"}, Value: marshal.PyInt{Value: timeToFileTime(expires)}},
				{Key: marshal.PyString{Value: "description"}, Value: marshal.PyString{Value: l.Description}},
				{Key: marshal.PyString{Value: "assigneeID"}, Value: assigneeRep},
				{Key: marshal.PyString{Value: "stationID"}, Value: marshal.PyInt{Value: uint64(l.StationID)}},
				{Key: marshal.PyString{Value: "issuerID"}, Value: marshal.PyInt{Value: uint64(l.IssuerID)}},
				{Key: marshal.PyString{Value: "price"}, Value: marshal.PyReal{Value: l.Price}},
				{Key: marshal.PyString{Value: "regionID"}, Value: marshal.PyInt{Value: uint64(l.RegionID)}},
				{Key: marshal.PyString{Value: "solarsystemID"}, Value: marshal.PyInt{Value: uint64(l.SolarSysID)}},
			}},
		})
	}

	nameEntries := make([]marshal.DictEntry, 0, len(names))
	for id, name := range names {
		nameEntries = append(nameEntries, marshal.DictEntry{
			Key:   marshal.PyInt{Value: uint64(id)},
			Value: marshal.PyString{Value: name},
		})
	}

	log.Printf("[%s] → GetAvailableEscrows (%d listings)", addr, len(rows))

	return marshal.PyObject{
		Type: "util.KeyVal",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "escrows"}, Value: marshal.PyList{Items: rows}},
			{Key: marshal.PyString{Value: "names"}, Value: marshal.PyDict{Items: nameEntries}},
		}},
	}
}

func emptyEscrowResult() marshal.PyRep {
	return marshal.PyObject{
		Type: "util.KeyVal",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "escrows"}, Value: marshal.PyList{}},
			{Key: marshal.PyString{Value: "names"}, Value: marshal.PyDict{}},
		}},
	}
}

// handleEscrowClaim processes AcceptCourierMission when the row is kind=2.
func handleEscrowClaim(ctx context.Context, addr string, c *evenet.Conn, pkt *evenet.PyPacket,
	accountID, charID int64, cs *server.CallStream, sqldb *sql.DB, sessReg *esp.SessionRegistry) error {

	if len(cs.ArgTuple.Items) < 1 {
		return server.SendCallException(c, pkt, accountID, server.BuildUserError(
			"CustomError", map[string]marshal.PyRep{
				"error": marshal.PyString{Value: "Missing escrow ID."},
			}))
	}
	escrowID := repToInt(cs.ArgTuple.Items[0])

	// Must be docked.
	stationID, err := evedb.GetCharStationID(ctx, sqldb, charID)
	if err != nil || stationID == 0 {
		return server.SendCallException(c, pkt, accountID, server.BuildUserError(
			"CustomError", map[string]marshal.PyRep{
				"error": marshal.PyString{Value: "You must be docked to claim an escrow listing."},
			}))
	}

	corpID, _ := evedb.GetCharCorpID(ctx, sqldb, charID)

	movedItemIDs, err := evedb.ClaimEscrowMission(ctx, sqldb, escrowID, charID, corpID, stationID, false)
	if err != nil {
		log.Printf("[%s] ClaimEscrow %d: %v", addr, escrowID, err)
		return server.SendCallException(c, pkt, accountID, server.BuildUserError(
			"CustomError", map[string]marshal.PyRep{
				"error": marshal.PyString{Value: err.Error()},
			}))
	}

	log.Printf("[%s] → ClaimEscrow charID=%d escrowID=%d (%d items)", addr, charID, escrowID, len(movedItemIDs))

	var newBal float64
	sqldb.QueryRowContext(ctx,
		`SELECT balance FROM character_ WHERE characterID = ?`, charID).Scan(&newBal)
	_ = server.SendNotification(c, accountID, "OnAccountChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: "cash"},
		marshal.PyInt{Value: uint64(charID)},
		marshal.PyReal{Value: newBal},
	}})

	// Push OnItemChange for each item moved into the buyer's hangar so the
	// client's inventory cache sees them without a redock.
	for _, itemID := range movedItemIDs {
		er, qErr := evedb.GetEntityRow(ctx, sqldb, itemID)
		if qErr != nil || er == nil {
			continue
		}
		_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
			evedb.BuildEntityRow(er),
			marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: 0}}, // ixLocationID: old=0 (new item)
			}},
		}})
	}

	// Push wallet update to seller if online.
	// Read the issuerID from the (now delivered) row.
	issuerID, _ := evedb.GetEscrowIssuerID(ctx, sqldb, escrowID)
	if issuerID > 0 {
		if sess := sessReg.FindByCharID(issuerID); sess != nil && sess.Conn != nil {
			var sellerBal float64
			sqldb.QueryRowContext(ctx,
				`SELECT balance FROM character_ WHERE characterID = ?`,
				issuerID).Scan(&sellerBal)
			_ = server.SendNotification(sess.Conn, int64(sess.AccountID), "OnAccountChange", "charid",
				marshal.PyTuple{Items: []marshal.PyRep{
					marshal.PyString{Value: "cash"},
					marshal.PyInt{Value: uint64(issuerID)},
					marshal.PyReal{Value: sellerBal},
				}})
		}
	}

	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}
