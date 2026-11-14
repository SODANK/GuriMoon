// Package corpmgr implements the "corpmgr" service.
package corpmgr

import (
	"context"
	"database/sql"
	"log"
	"time"

	evedb "gurimoon/db"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
)

// Handle dispatches a CallReq addressed to the "corpmgr" service.
func Handle(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, charID int64, sqldb *sql.DB) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	switch cs.Method {
	case "GetPublicInfo":
		return handleGetPublicInfo(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "GetAssetInventory":
		return handleGetAssetInventory(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "GetAssetInventoryForLocation":
		return handleGetAssetInventoryForLocation(ctx, c, addr, cs, pkt, accountID, charID, sqldb)
	case "GetCorporationIDForCharacter":
		return handleGetCorporationIDForCharacter(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "GetCorporations":
		// Same util.Row shape as GetPublicInfo: LoadCorporation reads .header/.line/
		// .corporationID and pvptradewnd reads .ceoID. The arg is a single corporationID.
		return handleGetPublicInfo(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "GetCorporationStations":
		// Stations owned by the session corp. Not populated: an empty rowset keeps the
		// selection list empty, whereas None would crash the iteration.
		log.Printf("[%s] → corpmgr.GetCorporationStations (empty)", addr)
		return server.SendCallRsp(c, pkt, accountID,
			emptyRowset([]string{"itemID", "locationID", "typeID"}))
	case "AuditMember":
		// (memberID, fromDate, toDate, rowsPerPage) answers (logItemEventRows,
		// crpRoleHistoryRows). No audit log is recorded; two empty rowsets draw an empty window.
		log.Printf("[%s] → corpmgr.AuditMember (empty)", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyTuple{Items: []marshal.PyRep{
			emptyRowset([]string{"characterID", "eventDateTime", "corporationID", "eventTypeID"}),
			emptyRowset([]string{"characterID", "changeTime", "corporationID", "oldRoles", "newRoles", "grantable"}),
		}})
	case "GetCorporationWallet":
		// wallet.py caches this as an inventory shell that no RMR UI dereferences, so None is safe.
		log.Printf("[%s] → corpmgr.GetCorporationWallet (None, dead client path)", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	case "GetItemsRented":
		// corp_ui_accounts ShowRents iterates rows with typeID/flag/rental fields. No
		// rental list is served; an empty rowset shows "You haven't rented anything".
		log.Printf("[%s] → corpmgr.GetItemsRented (empty)", addr)
		return server.SendCallRsp(c, pkt, accountID, emptyRowset([]string{
			"typeID", "flag", "rentedFromID", "startDate",
			"rentPeriodInDays", "periodCost", "balanceDueDate"}))
	default:
		server.RecordUnhandled(addr, "corpmgr", cs, false)
		log.Printf("[%s] corpmgr: unhandled method %q — sending exception", addr, cs.Method)
		return server.SendCallException(c, pkt, accountID, nil)
	}
}

// emptyRowset builds an empty util.Rowset with the given column names.
func emptyRowset(cols []string) marshal.PyRep {
	header := make([]marshal.PyRep, len(cols))
	for i, col := range cols {
		header[i] = marshal.PyString{Value: col}
	}
	return marshal.PyObject{
		Type: "util.Rowset",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "header"}, Value: marshal.PyList{Items: header}},
			{Key: marshal.PyString{Value: "RowClass"}, Value: marshal.PyString{Value: "util.Row", IsType1: true}},
			{Key: marshal.PyString{Value: "lines"}, Value: marshal.PyList{}},
		}},
	}
}

// handleGetPublicInfo returns a util.Row with public corporation information.
// Arg[0] is the corporation ID.
func handleGetPublicInfo(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	if len(cs.ArgTuple.Items) < 1 {
		log.Printf("[%s] corpmgr.GetPublicInfo: missing arg", addr)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	id, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		log.Printf("[%s] corpmgr.GetPublicInfo: arg[0] is %T, want PyInt", addr, cs.ArgTuple.Items[0])
		return server.SendCallException(c, pkt, accountID, nil)
	}
	result, err := evedb.GetCorporation(ctx, sqldb, int64(id.Value))
	if err != nil {
		log.Printf("[%s] corpmgr.GetPublicInfo db: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	log.Printf("[%s] → corpmgr.GetPublicInfo corpID=%d", addr, id.Value)
	return server.SendCallRsp(c, pkt, accountID, result)
}

// handleGetAssetInventory returns a PyDict mapping stationID → util.Rowset of
// items owned by the corporation.  Arg[0] is the corporationID.
func handleGetAssetInventory(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	if len(cs.ArgTuple.Items) < 1 {
		log.Printf("[%s] corpmgr.GetAssetInventory: missing arg", addr)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	id, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		log.Printf("[%s] corpmgr.GetAssetInventory: arg[0] is %T, want PyInt", addr, cs.ArgTuple.Items[0])
		return server.SendCallException(c, pkt, accountID, nil)
	}
	result, err := evedb.GetCorpAssets(ctx, sqldb, int64(id.Value))
	if err != nil {
		log.Printf("[%s] corpmgr.GetAssetInventory db: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	log.Printf("[%s] → corpmgr.GetAssetInventory corpID=%d", addr, id.Value)
	return server.SendCallRsp(c, pkt, accountID, result)
}

// handleGetCorporationIDForCharacter returns the corporationID for a character,
// called by the showinfo standings tab. An exception with a None payload crashes
// the client ("exceptions must be classes..."), so errors answer PyNone.
func handleGetCorporationIDForCharacter(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	if len(cs.ArgTuple.Items) < 1 {
		log.Printf("[%s] corpmgr.GetCorporationIDForCharacter: missing arg", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	id, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		log.Printf("[%s] corpmgr.GetCorporationIDForCharacter: arg[0] is %T, want PyInt", addr, cs.ArgTuple.Items[0])
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	targetCharID := int64(id.Value)
	corpID, err := evedb.GetCharCorpID(ctx, sqldb, targetCharID)
	if err != nil {
		log.Printf("[%s] corpmgr.GetCorporationIDForCharacter charID=%d: %v", addr, targetCharID, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	log.Printf("[%s] → corpmgr.GetCorporationIDForCharacter charID=%d corpID=%d", addr, targetCharID, corpID)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: uint64(corpID)})
}

// handleGetAssetInventoryForLocation returns a util.Rowset of items the player's
// corporation owns at locationID. The wire signature is (locationID, which) with
// which a flag selector ("offices", "impounded"); `which` is not applied and every
// corp-owned item at the location is returned.
func handleGetAssetInventoryForLocation(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, charID int64, sqldb *sql.DB) error {
	if len(cs.ArgTuple.Items) < 1 {
		log.Printf("[%s] corpmgr.GetAssetInventoryForLocation: missing arg", addr)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	locRep, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		log.Printf("[%s] corpmgr.GetAssetInventoryForLocation: arg[0] is %T, want PyInt", addr, cs.ArgTuple.Items[0])
		return server.SendCallException(c, pkt, accountID, nil)
	}
	locationID := int64(locRep.Value)
	var which string
	if len(cs.ArgTuple.Items) >= 2 {
		if s, ok := cs.ArgTuple.Items[1].(marshal.PyString); ok {
			which = s.Value
		}
	}
	corpID, err := evedb.GetCharCorpID(ctx, sqldb, charID)
	if err != nil {
		log.Printf("[%s] corpmgr.GetAssetInventoryForLocation: GetCharCorpID(%d): %v", addr, charID, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	result, err := evedb.GetCorpAssetsAtLocation(ctx, sqldb, corpID, locationID)
	if err != nil {
		log.Printf("[%s] corpmgr.GetAssetInventoryForLocation db: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	log.Printf("[%s] → corpmgr.GetAssetInventoryForLocation corpID=%d locationID=%d which=%q",
		addr, corpID, locationID, which)
	return server.SendCallRsp(c, pkt, accountID, result)
}
