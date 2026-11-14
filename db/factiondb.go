package db

import (
	"context"
	"database/sql"
	"fmt"
	"sync"

	"gurimoon/marshal"
)

// ---- corporationSvc.GetFactionInfo ----
// An 8-tuple of dicts read once per session by ui/services/factionsvc.py GetData():
//	[0] factionIDbyNPCCorpID: corpID -> factionID
//	[1] factionRegions: factionID -> [regionID, ...]
//	[2] factionConstellations: factionID -> [constellationID, ...]
//	[3] factionSolarSystems: factionID -> [solarSystemID, ...]
//	[4] factionRaces: factionID -> [raceID, ...]
//	[5] factionStationCount: factionID -> count
//	[6] factionSolarSystemCount: factionID -> count
//	[7] npcCorpInfo: corpID -> util.Row(size, extent)

var (
	factionInfoOnce  sync.Mutex
	factionInfoCache marshal.PyRep
)

// npcCorpInfoCols are the fields infosvc.py reads off the npcCorpInfo row (the Size and
// Extent entries in the corporation info window's Attributes tab).
var npcCorpInfoCols = []string{"size", "extent"}

// GetFactionInfo builds (and caches) the corporationSvc.GetFactionInfo 8-tuple.
func GetFactionInfo(ctx context.Context, db *sql.DB) (marshal.PyRep, error) {
	factionInfoOnce.Lock()
	defer factionInfoOnce.Unlock()
	if factionInfoCache != nil {
		return factionInfoCache, nil
	}

	corpFaction, err := queryIntPairDict(ctx, db,
		`SELECT corporationID, factionID FROM crpNPCCorporations WHERE factionID IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("GetFactionInfo corps: %w", err)
	}

	regions, err := queryIntListDict(ctx, db,
		`SELECT factionID, regionID FROM mapRegions WHERE factionID IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("GetFactionInfo regions: %w", err)
	}
	constellations, err := queryIntListDict(ctx, db,
		`SELECT factionID, constellationID FROM mapConstellations WHERE factionID IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("GetFactionInfo constellations: %w", err)
	}
	solarSystems, err := queryIntListDict(ctx, db,
		`SELECT factionID, solarSystemID FROM mapSolarSystems WHERE factionID IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("GetFactionInfo solar systems: %w", err)
	}
	races, err := queryIntListDict(ctx, db,
		`SELECT factionID, raceID FROM factionRaces`)
	if err != nil {
		return nil, fmt.Errorf("GetFactionInfo races: %w", err)
	}

	// A faction "owns" every station belonging to one of its NPC corporations.
	stationCount, err := queryIntPairDict(ctx, db,
		`SELECT c.factionID, COUNT(*)
		   FROM staStations s
		   JOIN crpNPCCorporations c ON c.corporationID = s.corporationID
		  WHERE c.factionID IS NOT NULL
		  GROUP BY c.factionID`)
	if err != nil {
		return nil, fmt.Errorf("GetFactionInfo station count: %w", err)
	}
	systemCount, err := queryIntPairDict(ctx, db,
		`SELECT factionID, COUNT(*) FROM mapSolarSystems
		  WHERE factionID IS NOT NULL GROUP BY factionID`)
	if err != nil {
		return nil, fmt.Errorf("GetFactionInfo system count: %w", err)
	}

	// npcCorpInfo: infosvc.py indexes fixed dicts by size/extent chars, so rows with a NULL
	// or unknown value are dropped to avoid a client KeyError.
	npcCorpInfo, err := queryNPCCorpInfo(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("GetFactionInfo npc corp info: %w", err)
	}

	result := marshal.PyTuple{Items: []marshal.PyRep{
		corpFaction,
		regions,
		constellations,
		solarSystems,
		races,
		stationCount,
		systemCount,
		npcCorpInfo,
	}}
	factionInfoCache = result
	return result, nil
}

// queryIntPairDict runs a two-column (key, value) integer query and returns a PyDict
// mapping the first column to the second.
func queryIntPairDict(ctx context.Context, db *sql.DB, query string) (marshal.PyDict, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return marshal.PyDict{}, err
	}
	defer rows.Close()

	var items []marshal.DictEntry
	for rows.Next() {
		var k, v int64
		if err := rows.Scan(&k, &v); err != nil {
			return marshal.PyDict{}, err
		}
		items = append(items, marshal.DictEntry{Key: intRep(k), Value: intRep(v)})
	}
	if err := rows.Err(); err != nil {
		return marshal.PyDict{}, err
	}
	return marshal.PyDict{Items: items}, nil
}

// queryIntListDict runs a two-column (key, member) integer query and returns a PyDict
// mapping the first column to a PyList of every member seen for it. Key order follows
// first appearance, member order follows the row order.
func queryIntListDict(ctx context.Context, db *sql.DB, query string) (marshal.PyDict, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return marshal.PyDict{}, err
	}
	defer rows.Close()

	var order []int64
	grouped := make(map[int64][]marshal.PyRep)
	for rows.Next() {
		var k, v int64
		if err := rows.Scan(&k, &v); err != nil {
			return marshal.PyDict{}, err
		}
		if _, seen := grouped[k]; !seen {
			order = append(order, k)
		}
		grouped[k] = append(grouped[k], intRep(v))
	}
	if err := rows.Err(); err != nil {
		return marshal.PyDict{}, err
	}

	items := make([]marshal.DictEntry, 0, len(order))
	for _, k := range order {
		items = append(items, marshal.DictEntry{
			Key:   intRep(k),
			Value: marshal.PyList{Items: grouped[k]},
		})
	}
	return marshal.PyDict{Items: items}, nil
}

// queryNPCCorpInfo returns corpID -> util.Row(size, extent) for every NPC corp with a
// size/extent the client can render.
func queryNPCCorpInfo(ctx context.Context, db *sql.DB) (marshal.PyDict, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT corporationID, size, extent
		  FROM crpNPCCorporations
		 WHERE size IS NOT NULL AND extent IS NOT NULL`)
	if err != nil {
		return marshal.PyDict{}, err
	}
	defer rows.Close()

	validSize := map[string]bool{"T": true, "S": true, "M": true, "L": true, "H": true}
	validExtent := map[string]bool{"N": true, "G": true, "R": true, "L": true, "C": true}

	var items []marshal.DictEntry
	for rows.Next() {
		var corpID int64
		var size, extent string
		if err := rows.Scan(&corpID, &size, &extent); err != nil {
			return marshal.PyDict{}, err
		}
		if !validSize[size] || !validExtent[extent] {
			continue
		}
		items = append(items, marshal.DictEntry{
			Key:   intRep(corpID),
			Value: makeRow(npcCorpInfoCols, []marshal.PyRep{strRep(size), strRep(extent)}),
		})
	}
	if err := rows.Err(); err != nil {
		return marshal.PyDict{}, err
	}
	return marshal.PyDict{Items: items}, nil
}
