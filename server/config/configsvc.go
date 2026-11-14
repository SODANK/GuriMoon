// Package config implements the "config" service.
package config

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

// Handle dispatches a CallReq addressed to the "config" service.
func Handle(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	switch cs.Method {
	case "GetMultiOwnersEx":
		return handleIDListMethod(ctx, c, addr, cs, pkt, accountID, sqldb,
			"GetMultiOwnersEx", evedb.GetMultiOwnersEx)
	case "GetMultiLocationsEx":
		return handleIDListMethod(ctx, c, addr, cs, pkt, accountID, sqldb,
			"GetMultiLocationsEx", evedb.GetMultiLocationsEx)
	case "GetMultiAllianceShortNamesEx":
		return handleIDListMethod(ctx, c, addr, cs, pkt, accountID, sqldb,
			"GetMultiAllianceShortNamesEx", evedb.GetMultiAllianceShortNamesEx)
	case "GetMultiCorpTickerNamesEx":
		return handleIDListMethod(ctx, c, addr, cs, pkt, accountID, sqldb,
			"GetMultiCorpTickerNamesEx", evedb.GetMultiCorpTickerNamesEx)
	case "GetMultiGraphicsEx":
		return handleIDListMethod(ctx, c, addr, cs, pkt, accountID, sqldb,
			"GetMultiGraphicsEx", evedb.GetMultiGraphicsEx)
	case "GetMultiInvTypesEx":
		return handleIDListMethod(ctx, c, addr, cs, pkt, accountID, sqldb,
			"GetMultiInvTypesEx", evedb.GetMultiInvTypesEx)
	case "GetMultiAssemblyLineTypeEx":
		return handleIDListMethod(ctx, c, addr, cs, pkt, accountID, sqldb,
			"GetMultiAssemblyLineTypeEx", evedb.GetMultiAssemblyLineTypeEx)
	case "GetMultiInvCategoriesEx":
		return handleIDListMethod(ctx, c, addr, cs, pkt, accountID, sqldb,
			"GetMultiInvCategoriesEx", evedb.GetMultiInvCategoriesEx)
	case "GetMultiInvGroupsEx":
		return handleIDListMethod(ctx, c, addr, cs, pkt, accountID, sqldb,
			"GetMultiInvGroupsEx", evedb.GetMultiInvGroupsEx)
	case "GetMultiInvMetaGroupsEx":
		return handleIDListMethod(ctx, c, addr, cs, pkt, accountID, sqldb,
			"GetMultiInvMetaGroupsEx", evedb.GetMultiInvMetaGroupsEx)
	case "GetMultiBlueprintTypeEx":
		return handleIDListMethod(ctx, c, addr, cs, pkt, accountID, sqldb,
			"GetMultiBlueprintTypeEx", evedb.GetMultiBlueprintTypeEx)

	// Single-key Recordset cache-miss fetchers: cfg.py Recordset.Get(key) calls these
	// when a key is absent from the bulk snapshot. The client accepts the same
	// (header, lines) tuple-set as the multi variants, so each delegates with a one-element ID list.
	case "GetOwnersEx":
		return handleSingleIDMethod(ctx, c, addr, cs, pkt, accountID, sqldb,
			"GetOwnersEx", evedb.GetMultiOwnersEx)
	case "GetLocationsEx":
		return handleSingleIDMethod(ctx, c, addr, cs, pkt, accountID, sqldb,
			"GetLocationsEx", evedb.GetMultiLocationsEx)
	case "GetInvTypesEx":
		return handleSingleIDMethod(ctx, c, addr, cs, pkt, accountID, sqldb,
			"GetInvTypesEx", evedb.GetMultiInvTypesEx)
	case "GetInvCategoriesEx":
		return handleSingleIDMethod(ctx, c, addr, cs, pkt, accountID, sqldb,
			"GetInvCategoriesEx", evedb.GetMultiInvCategoriesEx)
	case "GetInvGroupsEx":
		return handleSingleIDMethod(ctx, c, addr, cs, pkt, accountID, sqldb,
			"GetInvGroupsEx", evedb.GetMultiInvGroupsEx)
	case "GetInvMetaGroupsEx":
		return handleSingleIDMethod(ctx, c, addr, cs, pkt, accountID, sqldb,
			"GetInvMetaGroupsEx", evedb.GetMultiInvMetaGroupsEx)
	case "GetBlueprintTypeEx":
		return handleSingleIDMethod(ctx, c, addr, cs, pkt, accountID, sqldb,
			"GetBlueprintTypeEx", evedb.GetMultiBlueprintTypeEx)
	case "GetGraphicsEx":
		return handleSingleIDMethod(ctx, c, addr, cs, pkt, accountID, sqldb,
			"GetGraphicsEx", evedb.GetMultiGraphicsEx)
	case "GetCorpTickerNamesEx":
		return handleSingleIDMethod(ctx, c, addr, cs, pkt, accountID, sqldb,
			"GetCorpTickerNamesEx", evedb.GetMultiCorpTickerNamesEx)
	case "GetAllianceShortNamesEx":
		return handleSingleIDMethod(ctx, c, addr, cs, pkt, accountID, sqldb,
			"GetAllianceShortNamesEx", evedb.GetMultiAllianceShortNamesEx)
	case "GetAssemblyLineTypeEx":
		return handleSingleIDMethod(ctx, c, addr, cs, pkt, accountID, sqldb,
			"GetAssemblyLineTypeEx", evedb.GetMultiAssemblyLineTypeEx)
	case "GetStationSolarSystemsByOwner":
		if len(cs.ArgTuple.Items) < 1 {
			log.Printf("[%s] config.GetStationSolarSystemsByOwner: missing arg", addr)
			return server.SendCallException(c, pkt, accountID, nil)
		}
		var ownerID int64
		if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
			ownerID = int64(v.Value)
		}
		result, err := evedb.GetStationSolarSystemsByOwner(ctx, sqldb, ownerID)
		if err != nil {
			log.Printf("[%s] config.GetStationSolarSystemsByOwner db: %v", addr, err)
			return server.SendCallException(c, pkt, accountID, nil)
		}
		log.Printf("[%s] → config.GetStationSolarSystemsByOwner ownerID=%d", addr, ownerID)
		return server.SendCallRsp(c, pkt, accountID, result)
	case "GetCelestialStatistic":
		return handleGetCelestialStatistic(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "GetUnits":
		return handleGetUnits(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "GetMapObjects":
		return handleGetMapObjects(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "GetMap":
		return handleGetMap(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "GetMapConnections":
		return handleGetMapConnections(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "GetMapLandmarks":
		// map.py iterates the rows for landmarkID, landmarkName, description, radius,
		// x/y/z, importance and url2d. No mapLandmarks data ships, so the list is empty.
		log.Printf("[%s] → config.GetMapLandmarks (empty — no landmark data)", addr)
		return server.SendCallRsp(c, pkt, accountID, evedb.MakeLandmarkRowset(nil))
	case "SetMapLandmarks":
		// GM-only landmark editor; it echoes the stored set back. Nothing is
		// persisted, so reply with the same empty set GetMapLandmarks returns.
		log.Printf("[%s] → config.SetMapLandmarks (not persisted)", addr)
		return server.SendCallRsp(c, pkt, accountID, evedb.MakeLandmarkRowset(nil))

	default:
		server.RecordUnhandled(addr, "config", cs, false)
		log.Printf("[%s] config: unhandled method %q — sending exception", addr, cs.Method)
		return server.SendCallException(c, pkt, accountID, nil)
	}
}

// handleIDListMethod is a generic handler for config methods that take a single
// list-of-integers argument and return a DB-derived PyRep.
func handleIDListMethod(
	ctx context.Context,
	c *evenet.Conn, addr string,
	cs *server.CallStream, pkt *evenet.PyPacket,
	accountID int64, sqldb *sql.DB,
	name string,
	fn func(context.Context, *sql.DB, []int64) (marshal.PyRep, error),
) error {
	if len(cs.ArgTuple.Items) < 1 {
		log.Printf("[%s] config.%s: missing arg", addr, name)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	ids, err := evedb.ParseIDList(cs.ArgTuple.Items[0])
	if err != nil {
		log.Printf("[%s] config.%s: parse IDs: %v", addr, name, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	// The one-ID case is cfg.Recordset.Get priming a missing key, once per ball and
	// serially on this connection, so it is memoized. Multi-ID calls are login-time
	// bulk primes with different IDs each time and would never hit a memo.
	var result marshal.PyRep
	if len(ids) == 1 {
		result, err = infocache.Shared.Get(name, ids[0], infocache.TTLIdentity, func() (marshal.PyRep, error) {
			return fn(ctx, sqldb, ids)
		})
	} else {
		result, err = fn(ctx, sqldb, ids)
	}
	if err != nil {
		log.Printf("[%s] config.%s db: %v", addr, name, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	logx.Debugf("[%s] → config.%s (%d IDs)", addr, name, len(ids))
	return server.SendCallRsp(c, pkt, accountID, result)
}

// handleSingleIDMethod handles the single-key Recordset fetchers by
// delegating to the matching GetMulti* function with a one-element ID list.
func handleSingleIDMethod(
	ctx context.Context,
	c *evenet.Conn, addr string,
	cs *server.CallStream, pkt *evenet.PyPacket,
	accountID int64, sqldb *sql.DB,
	name string,
	fn func(context.Context, *sql.DB, []int64) (marshal.PyRep, error),
) error {
	if len(cs.ArgTuple.Items) < 1 {
		log.Printf("[%s] config.%s: missing arg", addr, name)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	var id int64
	switch v := cs.ArgTuple.Items[0].(type) {
	case marshal.PyInt:
		id = int64(v.Value)
	case marshal.PyReal:
		id = int64(v.Value)
	default:
		log.Printf("[%s] config.%s: arg is %T, want PyInt", addr, name, cs.ArgTuple.Items[0])
		return server.SendCallException(c, pkt, accountID, nil)
	}
	result, err := infocache.Shared.Get(name, id, infocache.TTLIdentity, func() (marshal.PyRep, error) {
		return fn(ctx, sqldb, []int64{id})
	})
	if err != nil {
		log.Printf("[%s] config.%s db: %v", addr, name, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	logx.Debugf("[%s] → config.%s id=%d", addr, name, id)
	return server.SendCallRsp(c, pkt, accountID, result)
}

// handleGetCelestialStatistic handles config.GetCelestialStatistic(celestialID),
// called through eve.GetCachedObjectWithVersionCheck when Show Info opens on a
// celestial. A server exception crashes the client (bluepy cannot raise the
// wrapped None), so a lookup miss answers a util.Row of nulls.
func handleGetCelestialStatistic(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	if len(cs.ArgTuple.Items) < 1 {
		log.Printf("[%s] config.GetCelestialStatistic: missing arg", addr)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	v, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		log.Printf("[%s] config.GetCelestialStatistic: arg is %T, want PyInt", addr, cs.ArgTuple.Items[0])
		return server.SendCallException(c, pkt, accountID, nil)
	}
	result, err := evedb.GetCelestialStatistic(ctx, sqldb, int64(v.Value))
	if err != nil {
		log.Printf("[%s] config.GetCelestialStatistic db: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	log.Printf("[%s] → config.GetCelestialStatistic celestialID=%d", addr, v.Value)
	return server.SendCallRsp(c, pkt, accountID, result)
}

func handleGetUnits(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	result, err := evedb.GetUnits(ctx, sqldb)
	if err != nil {
		log.Printf("[%s] config.GetUnits db: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	log.Printf("[%s] → config.GetUnits", addr)
	return server.SendCallRsp(c, pkt, accountID, result)
}

// scalarArgs returns positional int/bool accessors over a call's argument tuple.
// The map calls mix wire types per slot: GetMapObjects gets literal ints while
// GetMapConnections gets comparison results, which marshal as PyBool. A PyInt-only
// reader reads every flag as false and GetMapConnections returns no jump lines.
func scalarArgs(args []marshal.PyRep) (intArg func(int) int64, boolArg func(int) bool) {
	intArg = func(i int) int64 {
		if i >= len(args) {
			return 0
		}
		switch v := args[i].(type) {
		case marshal.PyInt:
			return int64(v.Value)
		case marshal.PyBool:
			if v.Value {
				return 1
			}
			return 0
		case marshal.PyReal:
			return int64(v.Value)
		}
		return 0
	}
	boolArg = func(i int) bool { return intArg(i) != 0 }
	return intArg, boolArg
}

// handleGetMapObjects handles config.GetMapObjects(entityID, wantRegions,
// wantConstellations, wantSystems, wantStations [, unknown]).
func handleGetMapObjects(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	intArg, boolArg := scalarArgs(cs.ArgTuple.Items)

	entityID := intArg(0)
	wantRegions := boolArg(1)
	wantConstellations := boolArg(2)
	wantSystems := boolArg(3)
	wantStations := boolArg(4)

	result, err := evedb.GetMapObjects(ctx, sqldb, entityID,
		wantRegions, wantConstellations, wantSystems, wantStations)
	if err != nil {
		log.Printf("[%s] config.GetMapObjects db: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	log.Printf("[%s] → config.GetMapObjects entityID=%d regions=%v constellations=%v systems=%v stations=%v",
		addr, entityID, wantRegions, wantConstellations, wantSystems, wantStations)
	return server.SendCallRsp(c, pkt, accountID, result)
}

// handleGetMap handles config.GetMap(solarSystemID).
func handleGetMap(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	if len(cs.ArgTuple.Items) < 1 {
		log.Printf("[%s] config.GetMap: missing arg", addr)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	v, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		log.Printf("[%s] config.GetMap: arg is %T, want PyInt", addr, cs.ArgTuple.Items[0])
		return server.SendCallException(c, pkt, accountID, nil)
	}
	result, err := evedb.GetMap(ctx, sqldb, int64(v.Value))
	if err != nil {
		log.Printf("[%s] config.GetMap db: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	log.Printf("[%s] → config.GetMap solarSystemID=%d", addr, v.Value)
	return server.SendCallRsp(c, pkt, accountID, result)
}

// handleGetMapConnections handles config.GetMapConnections(entityID, wantRegions,
// wantConstellations, wantSystems, arg4, arg5).
func handleGetMapConnections(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	intArg, boolArg := scalarArgs(cs.ArgTuple.Items)

	entityID := intArg(0)
	wantRegions := boolArg(1)
	wantConstellations := boolArg(2)
	wantSystems := boolArg(3)

	result, err := evedb.GetMapConnections(ctx, sqldb, entityID, wantRegions, wantConstellations, wantSystems)
	if err != nil {
		log.Printf("[%s] config.GetMapConnections db: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	log.Printf("[%s] → config.GetMapConnections entityID=%d regions=%v constellations=%v systems=%v",
		addr, entityID, wantRegions, wantConstellations, wantSystems)
	return server.SendCallRsp(c, pkt, accountID, result)
}

// handleGetStationSolarSystemsByOwner handles config.GetStationSolarSystemsByOwner(ownerID).
func handleGetStationSolarSystemsByOwner(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	if len(cs.ArgTuple.Items) < 1 {
		log.Printf("[%s] config.GetStationSolarSystemsByOwner: missing arg", addr)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	v, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		log.Printf("[%s] config.GetStationSolarSystemsByOwner: arg is %T, want PyInt", addr, cs.ArgTuple.Items[0])
		return server.SendCallException(c, pkt, accountID, nil)
	}
	result, err := evedb.GetStationSolarSystemsByOwner(ctx, sqldb, int64(v.Value))
	if err != nil {
		log.Printf("[%s] config.GetStationSolarSystemsByOwner db: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	log.Printf("[%s] → config.GetStationSolarSystemsByOwner ownerID=%d", addr, v.Value)
	return server.SendCallRsp(c, pkt, accountID, result)
}
