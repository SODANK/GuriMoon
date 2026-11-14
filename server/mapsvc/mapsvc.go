// Package mapsvc implements the "map" service.
package mapsvc

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
)

// The GetStationExtraInfo 3-tuple is built once and handed to every client. The ESP
// station editor can rewrite its tables and the client joins opServices to stations on
// operationID itself, so InvalidateStationExtraInfo must run after such an edit.
var (
	stationExtraInfoMu     sync.Mutex
	stationExtraInfoCached marshal.PyRep
	stationExtraInfoBuilt  bool
)

// InvalidateStationExtraInfo drops the cached GetStationExtraInfo tuple; the next
// caller rebuilds it.
func InvalidateStationExtraInfo() {
	stationExtraInfoMu.Lock()
	stationExtraInfoCached, stationExtraInfoBuilt = nil, false
	stationExtraInfoMu.Unlock()
}

// Caller supplies the session state the overlay queries need. OnlineCorpMates and
// GangMates return character IDs for the "friends" star-colour modes; PilotCounts
// returns per-system counts in space and docked for STARMODE_AVGTRUEPILOTS.
type Caller struct {
	CharID          int64
	CorpID          int64
	OnlineCorpMates func() []int64
	GangMates       func() []int64
	PilotCounts     func() (inSpace, docked map[int64]int64)
}

// Handle dispatches a CallReq addressed to the "map" service.
func Handle(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, caller Caller, sqldb *sql.DB) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	switch cs.Method {
	case "GetStationExtraInfo":
		return handleGetStationExtraInfo(ctx, c, addr, pkt, accountID, sqldb)
	case "GetSolarSystemPseudoSecurities":
		return handleGetSolarSystemPseudoSecurities(ctx, c, addr, pkt, accountID, sqldb)
	case "GetStuckSystems":
		return handleGetStuckSystems(c, addr, pkt, accountID)

	case "GetSolarSystemVisits":
		result, err := evedb.GetSolarSystemVisits(ctx, sqldb, caller.CharID)
		if err != nil {
			log.Printf("[%s] map.GetSolarSystemVisits db: %v", addr, err)
			return server.SendCallRsp(c, pkt, accountID, emptyRowset(visitCols))
		}
		log.Printf("[%s] → map.GetSolarSystemVisits", addr)
		return server.SendCallRsp(c, pkt, accountID, result)

	case "GetStationCount":
		result, err := evedb.GetStationCountPerSystem(ctx, sqldb)
		if err != nil {
			log.Printf("[%s] map.GetStationCount db: %v", addr, err)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyList{})
		}
		log.Printf("[%s] → map.GetStationCount", addr)
		return server.SendCallRsp(c, pkt, accountID, result)

	case "GetDeadspaceComplexMap":
		result, err := evedb.GetDeadspaceComplexMap(ctx, sqldb)
		if err != nil {
			log.Printf("[%s] map.GetDeadspaceComplexMap db: %v", addr, err)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyList{})
		}
		log.Printf("[%s] → map.GetDeadspaceComplexMap", addr)
		return server.SendCallRsp(c, pkt, accountID, result)

	case "GetHistory":
		return handleGetHistory(ctx, c, addr, cs, pkt, accountID, caller, sqldb)

	case "GetSolarSystemLoadRatios":
		// Repurposed: average station-owning corp tax rate per system.
		result, err := evedb.GetSolarSystemTaxRates(ctx, sqldb)
		if err != nil {
			log.Printf("[%s] map.GetSolarSystemLoadRatios db: %v", addr, err)
			return server.SendCallRsp(c, pkt, accountID, emptyRowset(loadRatioCols))
		}
		log.Printf("[%s] → map.GetSolarSystemLoadRatios", addr)
		return server.SendCallRsp(c, pkt, accountID, result)

	case "GetBeaconCount":
		// {solarSystemID: activeFieldCount} for STARMODE_CYNOSURALFIELDS (22). Not cached: it
		// turns over every ten minutes. A failed query answers the empty dict, not an exception.
		counts, err := evedb.GetBeaconCountsBySystem(ctx, sqldb, server.Win32TimeNow())
		if err != nil {
			log.Printf("[%s] map.GetBeaconCount db: %v", addr, err)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyDict{})
		}
		beaconDict := marshal.PyDict{Items: make([]marshal.DictEntry, 0, len(counts))}
		for sysID, n := range counts {
			beaconDict.Items = append(beaconDict.Items, marshal.DictEntry{
				Key:   marshal.PyInt{Value: uint64(sysID)},
				Value: marshal.PyInt{Value: uint64(n)},
			})
		}
		log.Printf("[%s] → map.GetBeaconCount (%d system(s))", addr, len(counts))
		return server.SendCallRsp(c, pkt, accountID, beaconDict)

	case "GetMyExtraMapInfo":
		return handleGetMyExtraMapInfo(ctx, c, addr, pkt, accountID, caller, sqldb)

	case "GetMyExtraMapInfoAgents":
		// NPC standings toward the character for the agent overlay. map.py indexes the rowset
		// by fromID and reads .rank, multiplying by 10.0, so rank = standing / 10.
		result, err := evedb.GetAgentMapStandings(ctx, sqldb, caller.CharID)
		if err != nil {
			log.Printf("[%s] map.GetMyExtraMapInfoAgents db: %v", addr, err)
			result = emptyRowset(agentStandingCols)
		}
		log.Printf("[%s] → map.GetMyExtraMapInfoAgents charID=%d", addr, caller.CharID)
		return server.SendCallRsp(c, pkt, accountID, result)

	default:
		server.RecordUnhandled(addr, "map", cs, true)
		log.Printf("[%s] map: unhandled method %q — returning None", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
}

var (
	visitCols         = []string{"solarSystemID", "visits", "lastDateTime"}
	historyCols       = []string{"solarSystemID", "value1", "value2", "value3"}
	loadRatioCols     = []string{"solarSystemID", "loadRatio"}
	agentStandingCols = []string{"fromID", "rank"}
	memberLocCols     = []string{"characterID", "locationID"}
)

// emptyRowset builds a zero-row util.Rowset with the given header; the map's redraw
// loop throws on None.
func emptyRowset(cols []string) marshal.PyRep {
	header := make([]marshal.PyRep, len(cols))
	for i, col := range cols {
		header[i] = marshal.PyString{Value: col}
	}
	headerList := marshal.PyList{Items: header}
	return marshal.PyObject{
		Type: "util.Rowset",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "header"}, Value: headerList},
			{Key: marshal.PyString{Value: "RowClass"}, Value: marshal.PyString{Value: "util.Row", IsType1: true}},
			{Key: marshal.PyString{Value: "lines"}, Value: marshal.PyList{}},
			{Key: marshal.PyString{Value: "columns"}, Value: headerList},
		}},
	}
}

// handleGetMyExtraMapInfo returns (corpMates, gangMates, None). map.py reads
// .locationID / .characterID off the rows of the first two.
func handleGetMyExtraMapInfo(
	ctx context.Context, c *evenet.Conn, addr string, pkt *evenet.PyPacket,
	accountID int64, caller Caller, sqldb *sql.DB,
) error {
	corpRows := emptyRowset(memberLocCols)
	if caller.OnlineCorpMates != nil {
		if r, err := evedb.GetCorpMemberLocations(ctx, sqldb, caller.CorpID, caller.OnlineCorpMates()); err == nil {
			corpRows = r
		} else {
			log.Printf("[%s] map.GetMyExtraMapInfo corp: %v", addr, err)
		}
	}

	gangRows := emptyRowset(memberLocCols)
	if caller.GangMates != nil {
		if r, err := evedb.GetCharLocations(ctx, sqldb, caller.GangMates()); err == nil {
			gangRows = r
		} else {
			log.Printf("[%s] map.GetMyExtraMapInfo gang: %v", addr, err)
		}
	}

	log.Printf("[%s] → map.GetMyExtraMapInfo", addr)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyTuple{Items: []marshal.PyRep{
		corpRows, gangRows, marshal.PyNone{},
	}})
}

// handleGetStationExtraInfo returns a 3-tuple of rowsets:
//
//	[0] stations:   stationID, solarSystemID, operationID, stationTypeID, ownerID
//	[1] opServices: operationID, serviceID
//	[2] services:   serviceID, serviceName
func handleGetStationExtraInfo(ctx context.Context, c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	stationExtraInfoMu.Lock()
	if !stationExtraInfoBuilt {
		result, err := evedb.GetStationExtraInfo(ctx, sqldb)
		if err != nil {
			// A failure is not cached: a lock timeout would otherwise latch a permanent exception.
			stationExtraInfoMu.Unlock()
			log.Printf("[%s] map.GetStationExtraInfo db: %v", addr, err)
			return server.SendCallException(c, pkt, accountID, nil)
		}
		stationExtraInfoCached, stationExtraInfoBuilt = result, true
	}
	cached := stationExtraInfoCached
	stationExtraInfoMu.Unlock()
	log.Printf("[%s] → map.GetStationExtraInfo (cached)", addr)
	return server.SendCallRsp(c, pkt, accountID, cached)
}

// handleGetSolarSystemPseudoSecurities returns a util.Rowset of (solarSystemID,
// security) for every solar system. The session change is sent before this call, so
// nothing else is needed on login.
func handleGetSolarSystemPseudoSecurities(ctx context.Context, c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	result, err := evedb.GetSolarSystemSecurities(ctx, sqldb)
	if err != nil {
		log.Printf("[%s] map.GetSolarSystemPseudoSecurities db: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	log.Printf("[%s] → map.GetSolarSystemPseudoSecurities", addr)
	return server.SendCallRsp(c, pkt, accountID, result)
}

// handleGetStuckSystems returns an empty dict.
func handleGetStuckSystems(c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID int64) error {
	log.Printf("[%s] → map.GetStuckSystems (empty)", addr)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyDict{})
}

// handleGetHistory dispatches GetHistory(statisticTypeID, hours). The type IDs are the
// client's, read off the call sites in map.py, not the STARMODE constants:
//
//	1 = jumps                    GetHistory(1, 1)
//	3 = kills (ship/faction/pod) GetHistory(3, 1) and (3, 24)
//
// Kill and jump modes share one 4-column rowset (solarSystemID, value1, value2,
// value3); the client reads ship kills as value1 - value2, faction kills as value2 and
// pod kills as value3. Types 4 and 5 answer from the session registry; no shipped
// client asks for them.
func handleGetHistory(
	ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream,
	pkt *evenet.PyPacket, accountID int64, caller Caller, sqldb *sql.DB,
) error {
	empty := emptyRowset(historyCols)

	if len(cs.ArgTuple.Items) < 2 {
		log.Printf("[%s] → map.GetHistory: missing args", addr)
		return server.SendCallRsp(c, pkt, accountID, empty)
	}
	statArg, ok1 := cs.ArgTuple.Items[0].(marshal.PyInt)
	hoursArg, ok2 := cs.ArgTuple.Items[1].(marshal.PyInt)
	if !ok1 || !ok2 {
		log.Printf("[%s] → map.GetHistory: bad arg types", addr)
		return server.SendCallRsp(c, pkt, accountID, empty)
	}
	statType := int64(statArg.Value)
	hours := int64(hoursArg.Value)
	if hours <= 0 {
		hours = 24
	}

	var result marshal.PyRep
	var err error

	switch statType {
	case 1: // jumps
		result, err = evedb.GetJumpHistory(ctx, sqldb, hours)
		if err != nil {
			log.Printf("[%s] map.GetHistory jumps: %v", addr, err)
			result = empty
		}
		log.Printf("[%s] → map.GetHistory type=jumps hours=%d", addr, hours)

	case 2, 3: // kills (ship / faction / pod)
		result, err = evedb.GetKillHistory(ctx, sqldb, hours)
		if err != nil {
			log.Printf("[%s] map.GetHistory kills: %v", addr, err)
			result = empty
		}
		log.Printf("[%s] → map.GetHistory type=kills hours=%d", addr, hours)

	case 4: // pilots in space
		result = buildPilotCountRowset(caller, false)
		log.Printf("[%s] → map.GetHistory type=pilotsInSpace", addr)

	case 5: // pilots docked
		result = buildPilotCountRowset(caller, true)
		log.Printf("[%s] → map.GetHistory type=pilotsDocked", addr)

	default:
		log.Printf("[%s] → map.GetHistory type=%d (unknown — empty)", addr, statType)
		result = empty
	}

	return server.SendCallRsp(c, pkt, accountID, result)
}

// buildPilotCountRowset builds a history-shaped rowset from the live session registry;
// docked selects docked pilots, otherwise pilots in space.
func buildPilotCountRowset(caller Caller, docked bool) marshal.PyRep {
	if caller.PilotCounts == nil {
		return emptyRowset(historyCols)
	}
	inSpace, dockedMap := caller.PilotCounts()
	var counts map[int64]int64
	if docked {
		counts = dockedMap
	} else {
		counts = inSpace
	}
	if len(counts) == 0 {
		return emptyRowset(historyCols)
	}
	rows := make([][]marshal.PyRep, 0, len(counts))
	for sysID, n := range counts {
		rows = append(rows, []marshal.PyRep{
			marshal.PyInt{Value: uint64(sysID)},
			marshal.PyInt{Value: uint64(n)},
			marshal.PyInt{Value: 0},
			marshal.PyInt{Value: 0},
		})
	}
	return evedb.MakeHistoryRowset(rows)
}
