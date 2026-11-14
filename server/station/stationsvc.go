// Package station implements the "station" service.
package station

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

// Handle dispatches a CallReq addressed to the "station" service.
func Handle(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, stationID int64, sqldb *sql.DB, stationReg *StationRegistry) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	switch cs.Method {
	case "GetStationItemBits":
		return handleGetStationItemBits(ctx, c, addr, cs, pkt, accountID, stationID, sqldb)
	case "GetGuests":
		return handleGetGuests(ctx, c, addr, pkt, accountID, stationID, stationReg)
	default:
		server.RecordUnhandled(addr, "station", cs, false)
		log.Printf("[%s] station: unhandled method %q — sending exception", addr, cs.Method)
		return server.SendCallException(c, pkt, accountID, nil)
	}
}

// handleGetStationItemBits returns the 5-tuple (hangarGraphicID, ownerID, stationID,
// serviceMask, stationTypeID). The client sends no positional args (only
// machoVersion in kwargs), so the per-connection stationID stored during
// SelectCharacterID is used.
func handleGetStationItemBits(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sessionStationID int64, sqldb *sql.DB) error {
	var sid int64
	if len(cs.ArgTuple.Items) >= 1 {
		if sidRep, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
			sid = int64(sidRep.Value)
		}
	}
	if sid == 0 {
		sid = sessionStationID
	}
	if sid == 0 {
		log.Printf("[%s] station.GetStationItemBits: no stationID (missing arg and no session stationID)", addr)
		return server.SendCallException(c, pkt, accountID, nil)
	}

	bits, err := evedb.GetStationItemBits(ctx, sqldb, sid)
	if err != nil {
		log.Printf("[%s] station.GetStationItemBits db: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}

	result := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(bits.HangarGraphicID)},
		marshal.PyInt{Value: uint64(bits.OwnerID)},
		marshal.PyInt{Value: uint64(bits.StationID)},
		marshal.PyInt{Value: uint64(bits.ServiceMask)},
		marshal.PyInt{Value: uint64(bits.StationTypeID)},
	}}
	log.Printf("[%s] → station.GetStationItemBits stationID=%d", addr, sid)
	return server.SendCallRsp(c, pkt, accountID, result)
}

// handleGetGuests returns a list of character IDs currently at the station.
func handleGetGuests(ctx context.Context, c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID int64, stationID int64, stationReg *StationRegistry) error {
	charIDs := stationReg.Guests(stationID)
	items := make([]marshal.PyRep, len(charIDs))
	for i, charID := range charIDs {
		items[i] = marshal.PyInt{Value: uint64(charID)}
	}
	result := marshal.PyList{Items: items}
	log.Printf("[%s] → station.GetGuests stationID=%d guests=%d", addr, stationID, len(charIDs))
	return server.SendCallRsp(c, pkt, accountID, result)
}
