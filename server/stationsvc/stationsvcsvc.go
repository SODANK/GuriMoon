// Package stationsvc implements the "stationSvc" service.
package stationsvc

import (
	"context"
	"database/sql"
	"log"
	"time"

	evedb "gurimoon/db"
	"gurimoon/logx"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
	"gurimoon/server/infocache"
)

// SolarSystemMemoMethod keys GetSolarSystem replies in infocache.Shared.
const SolarSystemMemoMethod = "stationSvc.GetSolarSystem"

// Handle dispatches a CallReq addressed to the "stationSvc" service.
func Handle(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	switch cs.Method {
	case "GetStation":
		return handleGetStation(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "GetSolarSystem":
		return handleGetSolarSystem(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "GetAllianceSystems":
		// (solarSystemID, allianceID) for every sovereignty-claimed system;
		// map.py reads x.solarSystemID / x.allianceID off each row.
		result, err := evedb.GetAllianceSystems(ctx, sqldb)
		if err != nil {
			log.Printf("[%s] stationSvc.GetAllianceSystems db: %v", addr, err)
			return server.SendCallException(c, pkt, accountID, nil)
		}
		log.Printf("[%s] → stationSvc.GetAllianceSystems", addr)
		return server.SendCallRsp(c, pkt, accountID, result)
	default:
		server.RecordUnhandled(addr, "stationSvc", cs, false)
		log.Printf("[%s] stationSvc: unhandled method %q — sending exception", addr, cs.Method)
		return server.SendCallException(c, pkt, accountID, nil)
	}
}

// handleGetStation returns a util.KeyVal with full station data.
func handleGetStation(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	if len(cs.ArgTuple.Items) < 1 {
		log.Printf("[%s] stationSvc.GetStation: missing stationID arg", addr)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	sidRep, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		log.Printf("[%s] stationSvc.GetStation: arg[0] is %T, want PyInt", addr, cs.ArgTuple.Items[0])
		return server.SendCallException(c, pkt, accountID, nil)
	}

	result, err := evedb.GetStation(ctx, sqldb, int64(sidRep.Value))
	if err != nil {
		log.Printf("[%s] stationSvc.GetStation db: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}

	log.Printf("[%s] → stationSvc.GetStation stationID=%d", addr, sidRep.Value)
	return server.SendCallRsp(c, pkt, accountID, result)
}

// handleGetSolarSystem returns a "dbrow.SolarSystemRow" typed row.
func handleGetSolarSystem(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	if len(cs.ArgTuple.Items) < 1 {
		log.Printf("[%s] stationSvc.GetSolarSystem: missing solarSystemID arg", addr)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	ssIDRep, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		log.Printf("[%s] stationSvc.GetSolarSystem: arg[0] is %T, want PyInt", addr, cs.ArgTuple.Items[0])
		return server.SendCallException(c, pkt, accountID, nil)
	}

	// neocom.UpdateLoctionText asks again whenever the nearest ball changes, so the
	// row is memoized; every sov writer calls evedb.SolarSystemRowChanged.
	ssID := int64(ssIDRep.Value)
	result, err := infocache.Shared.Get(SolarSystemMemoMethod, ssID, infocache.TTLStatic, func() (marshal.PyRep, error) {
		return evedb.GetSolarSystem(ctx, sqldb, ssID)
	})
	if err != nil {
		log.Printf("[%s] stationSvc.GetSolarSystem db: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}

	logx.Debugf("[%s] → stationSvc.GetSolarSystem solarSystemID=%d", addr, ssIDRep.Value)
	return server.SendCallRsp(c, pkt, accountID, result)
}
