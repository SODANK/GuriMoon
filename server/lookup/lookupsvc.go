// Package lookup implements the "lookupSvc" service: search-by-name for
// characters, corporations, alliances, factions, stations and celestial objects.
package lookup

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

// Handle dispatches a CallReq addressed to the "lookupSvc" service.
func Handle(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	switch cs.Method {
	case "LookupCharacters":
		return handleLookupCharacters(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "LookupCorporations":
		return handleLookupCorporations(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "LookupPlayerCorporations":
		return handleLookupPlayerCorporations(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "LookupCorporationTickers":
		return handleLookupCorporationTickers(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "LookupStations":
		return handleLookupStations(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "LookupFactions":
		return handleLookupFactions(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "LookupOwners":
		return handleLookupOwners(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "LookupPCOwners":
		return handleLookupPCOwners(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "LookupCorporationsOrAlliances":
		return handleLookupCorporationsOrAlliances(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "LookupNoneNPCAccountOwners":
		return handleLookupNoneNPCAccountOwners(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "LookupLocationsByGroup":
		return handleLookupLocationsByGroup(ctx, c, addr, cs, pkt, accountID, sqldb)
	default:
		server.RecordUnhandled(addr, "lookupSvc", cs, true)
		log.Printf("[%s] lookupSvc: unhandled method %q — returning None", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
}

// extractStringInt extracts (string, int64) from cs.ArgTuple positions 0 and 1.
func extractStringInt(cs *server.CallStream) (string, int64, bool) {
	if len(cs.ArgTuple.Items) < 2 {
		return "", 0, false
	}
	s, ok := cs.ArgTuple.Items[0].(marshal.PyString)
	if !ok {
		return "", 0, false
	}
	i, ok := cs.ArgTuple.Items[1].(marshal.PyInt)
	if !ok {
		return "", 0, false
	}
	return s.Value, int64(i.Value), true
}

func handleLookupCharacters(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	searchStr, exact, ok := extractStringInt(cs)
	if !ok {
		log.Printf("[%s] lookupSvc.LookupCharacters: bad args", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	log.Printf("[%s] → lookupSvc.LookupCharacters(%q, exact=%d)", addr, searchStr, exact)
	result, err := evedb.LookupCharacters(ctx, sqldb, searchStr, exact)
	if err != nil {
		log.Printf("[%s] lookupSvc.LookupCharacters error: %v", addr, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	return server.SendCallRsp(c, pkt, accountID, result)
}

func handleLookupCorporations(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	searchStr, exact, ok := extractStringInt(cs)
	if !ok {
		log.Printf("[%s] lookupSvc.LookupCorporations: bad args", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	log.Printf("[%s] → lookupSvc.LookupCorporations(%q, exact=%d)", addr, searchStr, exact)
	result, err := evedb.LookupCorporations(ctx, sqldb, searchStr, exact)
	if err != nil {
		log.Printf("[%s] lookupSvc.LookupCorporations error: %v", addr, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	return server.SendCallRsp(c, pkt, accountID, result)
}

func handleLookupPlayerCorporations(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	searchStr, exact, ok := extractStringInt(cs)
	if !ok {
		log.Printf("[%s] lookupSvc.LookupPlayerCorporations: bad args", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	log.Printf("[%s] → lookupSvc.LookupPlayerCorporations(%q, exact=%d)", addr, searchStr, exact)
	result, err := evedb.LookupPlayerCorporations(ctx, sqldb, searchStr, exact)
	if err != nil {
		log.Printf("[%s] lookupSvc.LookupPlayerCorporations error: %v", addr, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	return server.SendCallRsp(c, pkt, accountID, result)
}

func handleLookupCorporationTickers(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	searchStr, exact, ok := extractStringInt(cs)
	if !ok {
		log.Printf("[%s] lookupSvc.LookupCorporationTickers: bad args", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	log.Printf("[%s] → lookupSvc.LookupCorporationTickers(%q, exact=%d)", addr, searchStr, exact)
	result, err := evedb.LookupCorporationTickers(ctx, sqldb, searchStr, exact)
	if err != nil {
		log.Printf("[%s] lookupSvc.LookupCorporationTickers error: %v", addr, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	return server.SendCallRsp(c, pkt, accountID, result)
}

func handleLookupStations(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	searchStr, exact, ok := extractStringInt(cs)
	if !ok {
		log.Printf("[%s] lookupSvc.LookupStations: bad args", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	log.Printf("[%s] → lookupSvc.LookupStations(%q, exact=%d)", addr, searchStr, exact)
	result, err := evedb.LookupStations(ctx, sqldb, searchStr, exact)
	if err != nil {
		log.Printf("[%s] lookupSvc.LookupStations error: %v", addr, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	return server.SendCallRsp(c, pkt, accountID, result)
}

func handleLookupFactions(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	searchStr, exact, ok := extractStringInt(cs)
	if !ok {
		log.Printf("[%s] lookupSvc.LookupFactions: bad args", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	log.Printf("[%s] → lookupSvc.LookupFactions(%q, exact=%d)", addr, searchStr, exact)
	result, err := evedb.LookupFactions(ctx, sqldb, searchStr, exact)
	if err != nil {
		log.Printf("[%s] lookupSvc.LookupFactions error: %v", addr, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	return server.SendCallRsp(c, pkt, accountID, result)
}

func handleLookupOwners(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	searchStr, exact, ok := extractStringInt(cs)
	if !ok {
		log.Printf("[%s] lookupSvc.LookupOwners: bad args", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	log.Printf("[%s] → lookupSvc.LookupOwners(%q, exact=%d)", addr, searchStr, exact)
	result, err := evedb.LookupOwners(ctx, sqldb, searchStr, exact)
	if err != nil {
		log.Printf("[%s] lookupSvc.LookupOwners error: %v", addr, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	return server.SendCallRsp(c, pkt, accountID, result)
}

func handleLookupPCOwners(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	searchStr, exact, ok := extractStringInt(cs)
	if !ok {
		log.Printf("[%s] lookupSvc.LookupPCOwners: bad args", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	log.Printf("[%s] → lookupSvc.LookupPCOwners(%q, exact=%d)", addr, searchStr, exact)
	result, err := evedb.LookupPCOwners(ctx, sqldb, searchStr, exact)
	if err != nil {
		log.Printf("[%s] lookupSvc.LookupPCOwners error: %v", addr, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	return server.SendCallRsp(c, pkt, accountID, result)
}

func handleLookupCorporationsOrAlliances(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	if len(cs.ArgTuple.Items) < 3 {
		log.Printf("[%s] lookupSvc.LookupCorporationsOrAlliances: bad args (need 3, got %d)", addr, len(cs.ArgTuple.Items))
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	s, ok := cs.ArgTuple.Items[0].(marshal.PyString)
	if !ok {
		log.Printf("[%s] lookupSvc.LookupCorporationsOrAlliances: arg[0] not string", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	exactArg, ok := cs.ArgTuple.Items[1].(marshal.PyInt)
	if !ok {
		log.Printf("[%s] lookupSvc.LookupCorporationsOrAlliances: arg[1] not int", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	warableArg, ok := cs.ArgTuple.Items[2].(marshal.PyInt)
	if !ok {
		// Some callers may pass a bool.
		if b, ok2 := cs.ArgTuple.Items[2].(marshal.PyBool); ok2 {
			if b.Value {
				warableArg = marshal.PyInt{Value: 1}
			} else {
				warableArg = marshal.PyInt{Value: 0}
			}
		} else {
			log.Printf("[%s] lookupSvc.LookupCorporationsOrAlliances: arg[2] not int/bool", addr)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
	}
	searchStr := s.Value
	exact := int64(exactArg.Value)
	warableOnly := warableArg.Value != 0
	log.Printf("[%s] → lookupSvc.LookupCorporationsOrAlliances(%q, exact=%d, warable=%v)", addr, searchStr, exact, warableOnly)
	result, err := evedb.LookupCorporationsOrAlliances(ctx, sqldb, searchStr, exact, warableOnly)
	if err != nil {
		log.Printf("[%s] lookupSvc.LookupCorporationsOrAlliances error: %v", addr, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	return server.SendCallRsp(c, pkt, accountID, result)
}

func handleLookupNoneNPCAccountOwners(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	searchStr, exact, ok := extractStringInt(cs)
	if !ok {
		log.Printf("[%s] lookupSvc.LookupNoneNPCAccountOwners: bad args", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	log.Printf("[%s] → lookupSvc.LookupNoneNPCAccountOwners(%q, exact=%d)", addr, searchStr, exact)
	result, err := evedb.LookupNoneNPCAccountOwners(ctx, sqldb, searchStr, exact)
	if err != nil {
		log.Printf("[%s] lookupSvc.LookupNoneNPCAccountOwners error: %v", addr, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	return server.SendCallRsp(c, pkt, accountID, result)
}

func handleLookupLocationsByGroup(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	if len(cs.ArgTuple.Items) < 2 {
		log.Printf("[%s] lookupSvc.LookupLocationsByGroup: bad args", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	groupArg, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		log.Printf("[%s] lookupSvc.LookupLocationsByGroup: arg[0] not int", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	searchArg, ok := cs.ArgTuple.Items[1].(marshal.PyString)
	if !ok {
		log.Printf("[%s] lookupSvc.LookupLocationsByGroup: arg[1] not string", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	groupID := int64(groupArg.Value)
	searchStr := searchArg.Value
	log.Printf("[%s] → lookupSvc.LookupLocationsByGroup(group=%d, %q)", addr, groupID, searchStr)
	result, err := evedb.LookupLocationsByGroup(ctx, sqldb, groupID, searchStr)
	if err != nil {
		log.Printf("[%s] lookupSvc.LookupLocationsByGroup error: %v", addr, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	return server.SendCallRsp(c, pkt, accountID, result)
}
