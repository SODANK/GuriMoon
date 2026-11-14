package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"

	evecache "gurimoon/cache"
	"gurimoon/marshal"
)

// buildIn constructs a parenthesised placeholder string and args slice for an
// IN clause.  If ids is empty it returns "(-1)" and nil so the query returns
// no rows rather than causing a syntax error.
func buildIn(ids []int64) (string, []interface{}) {
	if len(ids) == 0 {
		return "(-1)", nil
	}
	placeholders := make([]string, len(ids))
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	return "(" + strings.Join(placeholders, ",") + ")", args
}

// ParseIDList extracts a []int64 from a CallStream arg that is a PyList or a PyTuple of
// integers; the client sometimes sends a tuple to config.GetMulti* calls.
func ParseIDList(arg marshal.PyRep) ([]int64, error) {
	var items []marshal.PyRep
	switch v := arg.(type) {
	case marshal.PyList:
		items = v.Items
	case marshal.PyTuple:
		items = v.Items
	default:
		return nil, fmt.Errorf("expected PyList or PyTuple, got %T", arg)
	}
	ids := make([]int64, 0, len(items))
	for _, item := range items {
		switch v := item.(type) {
		case marshal.PyInt:
			ids = append(ids, int64(v.Value))
		default:
			return nil, fmt.Errorf("expected PyInt in ID list, got %T", item)
		}
	}
	return ids, nil
}

// ── GetMultiOwnersEx ─────────────────────────────────────────────────────────

var multiOwnersCols = []string{"ownerID", "ownerName", "typeID"}

// DunChatOwnerBase is the owner-ID band dungeon localChat triggers speak under (base +
// dunTriggers.triggerID); a lookup resolves to the author's sender name. The band sits above
// every allocated ID and below the int32 ceiling of the client's ID columns.
const DunChatOwnerBase = 2000000000

// GetMultiOwnersEx mirrors ConfigDB::GetMultiOwnersEx. It resolves names from entity,
// character_, corporation, eveStaticOwners, cacheOwners and staStations, one ranked row per
// ID, then appends dungeon chat senders. See docs/db-notes.md#client-config-fetchers.
func GetMultiOwnersEx(ctx context.Context, db *sql.DB, ids []int64) (marshal.PyRep, error) {
	ids = uniqueIDs(ids)
	if len(ids) == 0 {
		return makeTupleSet(multiOwnersCols, nil), nil
	}
	in, args := buildIn(ids)

	// UNION ALL ranks each source by prio; dedup happens below because SQL can only drop
	// wholly identical rows.
	query := `
		SELECT itemID AS ownerID, itemName AS ownerName, typeID, 0 AS prio
		  FROM entity WHERE itemID IN ` + in + `
		UNION ALL
		SELECT characterID, characterName, typeID, 1
		  FROM character_ WHERE characterID IN ` + in + `
		UNION ALL
		SELECT corporationID, corporationName, 2, 2
		  FROM corporation WHERE corporationID IN ` + in + `
		UNION ALL
		SELECT ownerID, ownerName, typeID, 3
		  FROM eveStaticOwners WHERE ownerID IN ` + in + `
		UNION ALL
		SELECT ownerID, ownerName, typeID, 4
		  FROM cacheOwners WHERE ownerID IN ` + in + `
		UNION ALL
		SELECT s.corporationID,
		       SUBSTRING_INDEX(MIN(s.stationName), ' - ', -1),
		       2, 5
		  FROM staStations s WHERE s.corporationID IN ` + in + `
		 GROUP BY s.corporationID`

	// Six sub-queries, so six copies of the args.
	allArgs := make([]interface{}, 0, len(args)*6)
	for i := 0; i < 6; i++ {
		allArgs = append(allArgs, args...)
	}
	rows, err := db.QueryContext(ctx, query, allArgs...)
	if err != nil {
		return nil, fmt.Errorf("GetMultiOwnersEx: %w", err)
	}
	defer rows.Close()

	type ownerRow struct {
		name   string
		typeID int64
		prio   int64
	}
	best := make(map[int64]ownerRow, len(ids))
	for rows.Next() {
		var ownerID int64
		var ownerName string
		var typeID, prio int64
		if err := rows.Scan(&ownerID, &ownerName, &typeID, &prio); err != nil {
			return nil, fmt.Errorf("GetMultiOwnersEx scan: %w", err)
		}
		if cur, ok := best[ownerID]; ok && cur.prio <= prio {
			continue
		}
		best[ownerID] = ownerRow{name: ownerName, typeID: typeID, prio: prio}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetMultiOwnersEx rows: %w", err)
	}

	// Emit in request order. An ID that matched nothing gets an "Unknown" row so cfg.eveowners
	// can cache it instead of raising RecordNotFound.
	var result [][]marshal.PyRep
	for _, id := range ids {
		row, ok := best[id]
		if !ok {
			result = append(result, []marshal.PyRep{intRep(id), strRep("Unknown"), intRep(0)})
			continue
		}
		result = append(result, []marshal.PyRep{intRep(id), strRep(row.name), intRep(row.typeID)})
	}
	for _, row := range dungeonChatOwnerRows(ctx, db, ids) {
		id, ok := row[0].(marshal.PyInt)
		if ok {
			if _, dup := best[int64(id.Value)]; dup {
				continue
			}
		}
		result = append(result, row)
	}
	return makeTupleSet(multiOwnersCols, result), nil
}

// uniqueIDs strips duplicate IDs, preserving request order. The client keys its primed
// Recordsets by ID.
func uniqueIDs(ids []int64) []int64 {
	if len(ids) < 2 {
		return ids
	}
	seen := make(map[int64]bool, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// dungeonChatOwnerRows resolves owner IDs in the DunChatOwnerBase band to the trigger's
// authored sender name. It is kept out of the main UNION so a database without dunTriggers
// still resolves other owners; failures are logged and dropped.
func dungeonChatOwnerRows(ctx context.Context, db *sql.DB, ids []int64) [][]marshal.PyRep {
	var band []int64
	for _, id := range ids {
		if id >= DunChatOwnerBase {
			band = append(band, id-DunChatOwnerBase)
		}
	}
	if len(band) == 0 {
		return nil
	}
	in, args := buildIn(band)
	rows, err := db.QueryContext(ctx,
		`SELECT triggerID, senderName FROM dunTriggers
		  WHERE triggerID IN `+in+` AND senderName IS NOT NULL AND senderName <> ''`, args...)
	if err != nil {
		log.Printf("GetMultiOwnersEx dungeon chat owners: %v", err)
		return nil
	}
	defer rows.Close()

	var out [][]marshal.PyRep
	for rows.Next() {
		var triggerID int64
		var senderName string
		if err := rows.Scan(&triggerID, &senderName); err != nil {
			log.Printf("GetMultiOwnersEx dungeon chat owners scan: %v", err)
			return out
		}
		out = append(out, []marshal.PyRep{
			intRep(DunChatOwnerBase + triggerID), strRep(senderName), intRep(1374),
		})
	}
	return out
}

// ── GetMultiLocationsEx ──────────────────────────────────────────────────────

var multiLocationsCols = []string{"locationID", "locationName", "x", "y", "z"}

// GetMultiLocationsEx mirrors ConfigDB::GetMultiLocationsEx: map items from mapDenormalize,
// dynamic items (stations, structures) from entity, one row per ID.
// See docs/db-notes.md#client-config-fetchers.
func GetMultiLocationsEx(ctx context.Context, db *sql.DB, ids []int64) (marshal.PyRep, error) {
	ids = uniqueIDs(ids)
	if len(ids) == 0 {
		return makeTupleSet(multiLocationsCols, nil), nil
	}
	best, err := resolveLocations(ctx, db, ids)
	if err != nil {
		return nil, err
	}
	var result [][]marshal.PyRep
	for _, id := range ids {
		row, ok := best[id]
		if !ok {
			continue
		}
		result = append(result, []marshal.PyRep{
			intRep(id), strRep(row.name),
			floatRep(row.x), floatRep(row.y), floatRep(row.z),
		})
	}
	return makeTupleSet(multiLocationsCols, result), nil
}

// LocatableIDs reports which of ids GetMultiLocationsEx can answer. The
// directional scanner filters its results through it: the client names every
// scan result with cfg.evelocations.Get, which raises on a miss and leaves
// the scanner window stuck mid-search.
func LocatableIDs(ctx context.Context, db *sql.DB, ids []int64) (map[int64]bool, error) {
	ids = uniqueIDs(ids)
	out := make(map[int64]bool, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	best, err := resolveLocations(ctx, db, ids)
	if err != nil {
		return nil, err
	}
	for id := range best {
		out[id] = true
	}
	return out, nil
}

type locRow struct {
	name    string
	x, y, z float64
	prio    int64
}

// resolveLocations is the lookup behind GetMultiLocationsEx: mapDenormalize,
// then the entity table, then the live world for anything with no row.
func resolveLocations(ctx context.Context, db *sql.DB, ids []int64) (map[int64]locRow, error) {
	in, args := buildIn(ids)

	query := `
		SELECT itemID AS locationID, itemName AS locationName, x, y, z, 0 AS prio
		  FROM mapDenormalize WHERE itemID IN ` + in + `
		UNION ALL
		SELECT itemID, itemName, x, y, z, 1
		  FROM entity WHERE itemID IN ` + in

	doubleArgs := append(args, args...)
	rows, err := db.QueryContext(ctx, query, doubleArgs...)
	if err != nil {
		return nil, fmt.Errorf("GetMultiLocationsEx: %w", err)
	}
	defer rows.Close()

	best := make(map[int64]locRow, len(ids))
	for rows.Next() {
		var locationID int64
		var locationName string
		var x, y, z float64
		var prio int64
		if err := rows.Scan(&locationID, &locationName, &x, &y, &z, &prio); err != nil {
			return nil, fmt.Errorf("GetMultiLocationsEx scan: %w", err)
		}
		if cur, ok := best[locationID]; ok && cur.prio <= prio {
			continue
		}
		best[locationID] = locRow{name: locationName, x: x, y: y, z: z, prio: prio}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetMultiLocationsEx rows: %w", err)
	}
	if LiveLocationHook != nil {
		for _, id := range ids {
			if _, ok := best[id]; ok {
				continue
			}
			// Not a row anywhere: ask the live world (NPCs, drones, probes) before leaving the client
			// to raise on the miss.
			if name, x, y, z, live := LiveLocationHook(id); live {
				best[id] = locRow{name: name, x: x, y: y, z: z}
			}
		}
	}
	return best, nil
}

// ── GetMultiAllianceShortNamesEx ─────────────────────────────────────────────

var multiAllianceCols = []string{"allianceID", "shortName"}

// GetMultiAllianceShortNamesEx mirrors ConfigDB::GetMultiAllianceShortNamesEx.
// Alliance type ID in EVE Online is 16159.
func GetMultiAllianceShortNamesEx(ctx context.Context, db *sql.DB, ids []int64) (marshal.PyRep, error) {
	if len(ids) == 0 {
		return makeTupleSet(multiAllianceCols, nil), nil
	}
	in, args := buildIn(ids)
	// alliance_shortnames, not entity: an explicit high itemID in entity raises the item
	// allocator's AUTO_INCREMENT onto the alliance band.
	rows, err := db.QueryContext(ctx,
		`SELECT allianceID, shortName
		   FROM alliance_shortnames WHERE allianceID IN `+in, args...)
	if err != nil {
		return nil, fmt.Errorf("GetMultiAllianceShortNamesEx: %w", err)
	}
	defer rows.Close()

	var result [][]marshal.PyRep
	for rows.Next() {
		var allianceID int64
		var shortName string
		if err := rows.Scan(&allianceID, &shortName); err != nil {
			return nil, fmt.Errorf("GetMultiAllianceShortNamesEx scan: %w", err)
		}
		result = append(result, []marshal.PyRep{intRep(allianceID), strRep(shortName)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetMultiAllianceShortNamesEx rows: %w", err)
	}
	return makeTupleSet(multiAllianceCols, result), nil
}

// ── GetMultiCorpTickerNamesEx ─────────────────────────────────────────────────

var multiCorpTickerCols = []string{
	"corporationID", "tickerName",
	"shape1", "shape2", "shape3",
	"color1", "color2", "color3",
}

// GetMultiCorpTickerNamesEx mirrors ConfigDB::GetMultiCorpTickerNamesEx. Every requested ID
// returns a row: the client's CrpTickerNames Recordset caches a fetched-but-absent key, and
// a later Get raises RecordNotFound in tactical.py and targetMgr.py. IDs missing from
// corporation (NPC slims can carry the factionless sentinel 500021) get a blank-logo row.
func GetMultiCorpTickerNamesEx(ctx context.Context, db *sql.DB, ids []int64) (marshal.PyRep, error) {
	if len(ids) == 0 {
		return makeRowList(multiCorpTickerCols, nil), nil
	}
	in, args := buildIn(ids)
	rows, err := db.QueryContext(ctx,
		`SELECT corporationID, tickerName,
		        shape1, shape2, shape3,
		        color1, color2, color3
		   FROM corporation WHERE corporationID IN `+in, args...)
	if err != nil {
		return nil, fmt.Errorf("GetMultiCorpTickerNamesEx: %w", err)
	}
	defer rows.Close()

	found := make(map[int64]bool, len(ids))
	var result [][]marshal.PyRep
	for rows.Next() {
		var corporationID int64
		var tickerName string
		// shape*/color* are graphicIDs: uix.GetCorpLogo draws a shape only when shape == int(shape),
		// so these go out as ints.
		var shape1, shape2, shape3 sql.NullInt64
		var color1, color2, color3 sql.NullInt64
		if err := rows.Scan(&corporationID, &tickerName,
			&shape1, &shape2, &shape3, &color1, &color2, &color3); err != nil {
			return nil, fmt.Errorf("GetMultiCorpTickerNamesEx scan: %w", err)
		}
		result = append(result, []marshal.PyRep{
			intRep(corporationID), strRep(tickerName),
			nullIntRep(shape1), nullIntRep(shape2), nullIntRep(shape3),
			nullIntRep(color1), nullIntRep(color2), nullIntRep(color3),
		})
		found[corporationID] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetMultiCorpTickerNamesEx rows: %w", err)
	}
	for _, id := range ids {
		if found[id] {
			continue
		}
		found[id] = true
		result = append(result, []marshal.PyRep{
			intRep(id), strRep("-"),
			nullIntRep(sql.NullInt64{}), nullIntRep(sql.NullInt64{}), nullIntRep(sql.NullInt64{}),
			nullIntRep(sql.NullInt64{}), nullIntRep(sql.NullInt64{}), nullIntRep(sql.NullInt64{}),
		})
	}
	return makeRowList(multiCorpTickerCols, result), nil
}

// ── GetMultiGraphicsEx ───────────────────────────────────────────────────────

var multiGraphicsCols = []string{"graphicID", "url3D", "urlWeb", "icon", "urlSound", "explosionID"}

// GetMultiGraphicsEx mirrors ConfigDB::GetMultiGraphicsEx.
func GetMultiGraphicsEx(ctx context.Context, db *sql.DB, ids []int64) (marshal.PyRep, error) {
	if len(ids) == 0 {
		return makeRowList(multiGraphicsCols, nil), nil
	}
	in, args := buildIn(ids)
	rows, err := db.QueryContext(ctx,
		`SELECT graphicID, url3D, urlWeb, icon, urlSound, explosionID
		   FROM eveGraphics WHERE graphicID IN `+in, args...)
	if err != nil {
		return nil, fmt.Errorf("GetMultiGraphicsEx: %w", err)
	}
	defer rows.Close()

	var result [][]marshal.PyRep
	for rows.Next() {
		var graphicID int64
		var url3D, urlWeb, icon, urlSound sql.NullString
		var explosionID sql.NullInt64
		if err := rows.Scan(&graphicID, &url3D, &urlWeb, &icon, &urlSound, &explosionID); err != nil {
			return nil, fmt.Errorf("GetMultiGraphicsEx scan: %w", err)
		}
		result = append(result, []marshal.PyRep{
			intRep(graphicID),
			nullStrRep(url3D), nullStrRep(urlWeb), nullStrRep(icon), nullStrRep(urlSound),
			nullIntRep(explosionID),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetMultiGraphicsEx rows: %w", err)
	}
	return makeRowList(multiGraphicsCols, result), nil
}

// ── GetMultiInvTypesEx ───────────────────────────────────────────────────────

var multiInvTypesCols = []string{
	"typeID", "groupID", "typeName", "description", "graphicID",
	"radius", "mass", "volume", "capacity", "portionSize", "raceID",
	"basePrice", "published", "marketGroupID", "chanceOfDuplicating",
}

// GetMultiInvTypesEx mirrors ConfigDB::GetMultiInvTypesEx.
func GetMultiInvTypesEx(ctx context.Context, db *sql.DB, ids []int64) (marshal.PyRep, error) {
	if len(ids) == 0 {
		return makeRowList(multiInvTypesCols, nil), nil
	}
	in, args := buildIn(ids)
	rows, err := db.QueryContext(ctx,
		`SELECT typeID, groupID, typeName, description, graphicID,
		        radius, mass, volume, capacity, portionSize, raceID,
		        basePrice, published, marketGroupID, chanceOfDuplicating
		   FROM invTypes WHERE typeID IN `+in, args...)
	if err != nil {
		return nil, fmt.Errorf("GetMultiInvTypesEx: %w", err)
	}
	defer rows.Close()

	var result [][]marshal.PyRep
	for rows.Next() {
		var typeID, groupID int64
		var typeName, description sql.NullString
		var graphicID sql.NullInt64
		var radius, mass, volume, capacity sql.NullFloat64
		var portionSize sql.NullInt64
		var raceID sql.NullInt64
		var basePrice sql.NullFloat64
		var published sql.NullInt64
		var marketGroupID sql.NullInt64
		var chanceOfDuplicating sql.NullFloat64
		if err := rows.Scan(
			&typeID, &groupID, &typeName, &description, &graphicID,
			&radius, &mass, &volume, &capacity, &portionSize, &raceID,
			&basePrice, &published, &marketGroupID, &chanceOfDuplicating,
		); err != nil {
			return nil, fmt.Errorf("GetMultiInvTypesEx scan: %w", err)
		}
		result = append(result, []marshal.PyRep{
			intRep(typeID), intRep(groupID), nullStrRep(typeName), nullStrRep(description),
			nullIntRep(graphicID),
			nullFloatRep(radius), nullFloatRep(mass), nullFloatRep(volume), nullFloatRep(capacity),
			nullIntRep(portionSize), nullIntRep(raceID),
			nullFloatRep(basePrice), nullIntRep(published), nullIntRep(marketGroupID),
			nullFloatRep(chanceOfDuplicating),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetMultiInvTypesEx rows: %w", err)
	}
	return makeRowList(multiInvTypesCols, result), nil
}

// ── GetMap ───────────────────────────────────────────────────────────────────

var getMapCols = []string{
	"itemID", "itemName", "typeID",
	"x", "y", "z",
	"locationID",
	"orbitID",
	"destinations",
	"xMin", "xMax", "yMin", "yMax", "zMin", "zMax",
	"luminosity",
}

// GetMap mirrors ConfigDB::GetMap: all mapDenormalize rows in solarSystemID.
func GetMap(ctx context.Context, db *sql.DB, solarSystemID int64) (marshal.PyRep, error) {
	// x/y/z are cast to integers: map.py SetSolarSystemPoints feeds them to an int-only call,
	// and a PyReal logs a DeprecationWarning on every starmap open.
	return queryRows(ctx, db, getMapCols,
		`SELECT itemID, itemName, typeID,
		        CAST(x AS SIGNED) AS x,
		        CAST(y AS SIGNED) AS y,
		        CAST(z AS SIGNED) AS z,
		        solarSystemID AS locationID,
		        NULL AS orbitID,
		        NULL AS destinations,
		        NULL AS xMin, NULL AS xMax,
		        NULL AS yMin, NULL AS yMax,
		        NULL AS zMin, NULL AS zMax,
		        NULL AS luminosity
		   FROM mapDenormalize
		  WHERE solarSystemID = ?`, solarSystemID)
}

// ── GetMapObjects ────────────────────────────────────────────────────────────

var mapObjectsCols = []string{
	"groupID", "typeID", "itemID", "itemName", "locationID",
	"orbitID", "connector", "x", "y", "z",
}

// GetMapObjects mirrors ConfigDB::GetMapObjects. The want flags pick the hierarchy level and
// entityID is its parent. Region, constellation and system levels filter on groupID (3, 4,
// 5); the station branch stays unfiltered. See docs/db-notes.md#client-config-fetchers.
func GetMapObjects(ctx context.Context, db *sql.DB,
	entityID int64, wantRegions, wantConstellations, wantSystems, wantStations bool,
) (marshal.PyRep, error) {
	var where string
	var arg int64
	groupFilter := ""
	switch {
	case wantRegions:
		where = "typeID"
		arg = 3 // universe typeID
		groupFilter = " AND groupID = 3"
	case wantConstellations:
		where = "regionID"
		arg = entityID
		groupFilter = " AND groupID = 4"
	case wantSystems:
		where = "constellationID"
		arg = entityID
		groupFilter = " AND groupID = 5"
	case wantStations:
		where = "solarSystemID"
		arg = entityID
	default:
		where = "solarSystemID"
		arg = entityID
	}
	// orbitID is COALESCEd to 0 (never a valid itemID) for parentless rows. x/y/z are cast as in
	// GetMap; GetMultiLocationsEx is not, because its dedup compares exact coordinates.
	return queryRows(ctx, db, mapObjectsCols,
		`SELECT groupID, typeID, itemID, itemName,
		        solarSystemID AS locationID,
		        COALESCE(orbitID, 0) AS orbitID, 0 AS connector,
		        CAST(x AS SIGNED) AS x,
		        CAST(y AS SIGNED) AS y,
		        CAST(z AS SIGNED) AS z
		   FROM mapDenormalize
		  WHERE `+where+` = ?`+groupFilter, arg)
}

// ── GetMapConnections ────────────────────────────────────────────────────────

var mapConnectionsCols = []string{
	"connectionType",
	"fromRegionID", "fromConstellationID", "fromSolarSystemID",
	"stargateID", "celestialID",
	"toSolarSystemID", "toConstellationID", "toRegionID",
}

// locationUniverse is the client's const.locationUniverse, the root of the map hierarchy.
const locationUniverse = 9

// GetMapConnections mirrors ConfigDB::GetMapConnections. The want flags pick the scope:
// systems (entityID is a constellation, or a system when >= 30000000), constellations (a
// region) or regions (every cross-region jump). connectionType is 0 within a constellation,
// 1 across constellations in a region, 2 across regions.
func GetMapConnections(
	ctx context.Context, db *sql.DB,
	entityID int64, wantRegions, wantConstellations, wantSystems bool,
) (marshal.PyRep, error) {
	var filter string
	var args []interface{}

	switch {
	case wantSystems && entityID == locationUniverse:
		// Every system jump in the universe, the scope svc.map.CreateCacheFile asks for.
		// Without this case it fell through to fromConstellationID = 9 and returned nothing.
	case wantSystems && entityID >= 30000000:
		// System-level view of a single system (map browser / photo service pass
		// a solarSystemID here): return that system's own gate jumps.
		filter = "AND j.fromSolarSystemID = ?"
		args = []interface{}{entityID}
	case wantSystems:
		filter = "AND j.fromConstellationID = ?"
		args = []interface{}{entityID}
	case wantConstellations:
		filter = "AND j.fromRegionID = ?"
		args = []interface{}{entityID}
	case wantRegions:
		filter = "AND j.fromRegionID != j.toRegionID"
		// entityID not used for region-level view; no placeholder needed
	default:
		filter = "AND j.fromRegionID = ?"
		args = []interface{}{entityID}
	}

	query := `
SELECT
  CASE
    WHEN j.fromRegionID != j.toRegionID          THEN 2
    WHEN j.fromConstellationID != j.toConstellationID THEN 1
    ELSE 0
  END AS connectionType,
  j.fromRegionID,
  j.fromConstellationID,
  j.fromSolarSystemID,
  mj.stargateID,
  mj.celestialID,
  j.toSolarSystemID,
  j.toConstellationID,
  j.toRegionID
FROM mapSolarSystemJumps j
JOIN mapDenormalize gfrom
  ON gfrom.solarSystemID = j.fromSolarSystemID AND gfrom.groupID = 10
JOIN mapJumps mj
  ON mj.stargateID = gfrom.itemID
JOIN mapDenormalize gto
  ON gto.itemID = mj.celestialID AND gto.solarSystemID = j.toSolarSystemID
WHERE 1=1 ` + filter

	return queryRows(ctx, db, mapConnectionsCols, query, args...)
}

// ── GetCelestialStatistic ────────────────────────────────────────────────────

// invGroup constants mirror EVEDB::invGroups in the C++ source.
const (
	invGroupSun          = 6
	invGroupPlanet       = 7
	invGroupMoon         = 8
	invGroupAsteroidBelt = 9
)

// formatScientific formats m as a 1-decimal mantissa, the literal "e+0", the rounded log10
// exponent and suffix (the C++ FORMAT/CONCAT expression).
func formatScientific(m float64, suffix string) string {
	if !(m > 0) {
		return "0.0e+00" + suffix
	}
	exp := math.Round(math.Log10(m))
	mantissa := m / math.Pow(10, exp)
	return fmt.Sprintf("%.1fe+0%.0f%s", mantissa, exp, suffix)
}

// GetCelestialStatistic mirrors ConfigDB::GetCelestialStatistic: a util.Rowset with one row
// of pre-formatted strings (units like " K", " AU", " kg"). infosvc.GetWndData needs a
// Rowset; a raw util.Row of doubles raises on .header.
func GetCelestialStatistic(ctx context.Context, db *sql.DB, celestialID int64) (marshal.PyRep, error) {
	// An exception on any ID in 40M-50M kills Show Info. infosvc skips the block on an empty
	// rowset, so anything without statistics (custom celestials, unlisted groups) answers that.
	empty := makeRowset([]string{}, nil)
	var groupID int64
	err := db.QueryRowContext(ctx,
		`SELECT groupID FROM eveNames WHERE itemID = ?`, celestialID).Scan(&groupID)
	if errors.Is(err, sql.ErrNoRows) {
		err = db.QueryRowContext(ctx,
			`SELECT groupID FROM mapDenormalize WHERE itemID = ?`, celestialID).Scan(&groupID)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return empty, nil
	}
	if err != nil {
		return nil, fmt.Errorf("GetCelestialStatistic group lookup: %w", err)
	}
	switch groupID {
	case invGroupSun, invGroupPlanet, invGroupMoon, invGroupAsteroidBelt:
	default:
		return empty, nil
	}

	var (
		temperature, luminosity, age            sql.NullFloat64
		orbitRadius, eccentricity, massDust     sql.NullFloat64
		density, surfaceGravity, escapeVelocity sql.NullFloat64
		orbitPeriod, pressure, radius           sql.NullFloat64
		spectralClass                           sql.NullString
	)
	err = db.QueryRowContext(ctx, `
		SELECT temperature, spectralClass, luminosity, age,
		       orbitRadius, eccentricity, massDust,
		       density, surfaceGravity, escapeVelocity,
		       orbitPeriod, pressure, radius
		  FROM mapCelestialStatistics
		 WHERE celestialID = ?`, celestialID).Scan(
		&temperature, &spectralClass, &luminosity, &age,
		&orbitRadius, &eccentricity, &massDust,
		&density, &surfaceGravity, &escapeVelocity,
		&orbitPeriod, &pressure, &radius,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return empty, nil
	}
	if err != nil {
		return nil, fmt.Errorf("GetCelestialStatistic stats: %w", err)
	}

	var cols []string
	var vals []marshal.PyRep
	switch groupID {
	case invGroupSun:
		ageYears := math.Floor(age.Float64/60/60/24/365/1000000) * 1000000
		cols = []string{"temperature", "spectralClass", "luminosity", "age", "radius"}
		vals = []marshal.PyRep{
			strRep(fmt.Sprintf("%.0f K", temperature.Float64)),
			strRep(spectralClass.String),
			strRep(fmt.Sprintf("%.5f", luminosity.Float64)),
			strRep(fmt.Sprintf("%.0f Years", ageYears)),
			strRep(fmt.Sprintf("%.0f km", radius.Float64/1000)),
		}
	case invGroupPlanet:
		cols = []string{"temperature", "Orbit Radius", "eccentricity", "mass", "density",
			"Surface Gravity", "Escape Velocity", "Orbit Period", "pressure", "radius"}
		vals = []marshal.PyRep{
			strRep(fmt.Sprintf("%.0f K", temperature.Float64)),
			strRep(fmt.Sprintf("%.3f AU", orbitRadius.Float64/1.49598e11)),
			strRep(fmt.Sprintf("%.3f", eccentricity.Float64)),
			strRep(formatScientific(massDust.Float64, " kg")),
			strRep(fmt.Sprintf("%.1f g/cm^3", density.Float64)),
			strRep(fmt.Sprintf("%.1f m/s^2", surfaceGravity.Float64)),
			strRep(fmt.Sprintf("%.1f km/s", escapeVelocity.Float64/1000)),
			strRep(fmt.Sprintf("%.0f days", orbitPeriod.Float64/864000)),
			strRep(fmt.Sprintf("%.0f kPa", pressure.Float64/100000)),
			strRep(fmt.Sprintf("%.0f km", radius.Float64/1000)),
		}
	case invGroupMoon:
		cols = []string{"temperature", "Orbit Radius", "mass", "density",
			"Surface Gravity", "Escape Velocity", "Orbit Period", "pressure", "radius"}
		vals = []marshal.PyRep{
			strRep(fmt.Sprintf("%.0f K", temperature.Float64)),
			strRep(fmt.Sprintf("%.0f km", orbitRadius.Float64)),
			strRep(formatScientific(massDust.Float64, " kg")),
			strRep(fmt.Sprintf("%.1f g/cm^3", density.Float64)),
			strRep(fmt.Sprintf("%.1f m/s^2", surfaceGravity.Float64)),
			strRep(fmt.Sprintf("%.1f km/s", escapeVelocity.Float64/1000)),
			strRep(fmt.Sprintf("%.3f days", orbitPeriod.Float64/864000)),
			strRep(fmt.Sprintf("%.0f kPa", pressure.Float64/100000)),
			strRep(fmt.Sprintf("%.0f km", radius.Float64/1000)),
		}
	case invGroupAsteroidBelt:
		cols = []string{"Orbit Radius", "eccentricity", "mass", "density", "Orbit Period"}
		vals = []marshal.PyRep{
			strRep(fmt.Sprintf("%.0f km", orbitRadius.Float64)),
			strRep(fmt.Sprintf("%.3f", eccentricity.Float64)),
			strRep(formatScientific(massDust.Float64, " kg")),
			strRep(fmt.Sprintf("%.1f g/cm^3", density.Float64)),
			strRep(fmt.Sprintf("%.0f days", orbitPeriod.Float64/864000)),
		}
	default:
		return nil, fmt.Errorf("GetCelestialStatistic: invalid groupID %d for %d", groupID, celestialID)
	}

	return makeRowset(cols, [][]marshal.PyRep{vals}), nil
}

// ── GetUnits ─────────────────────────────────────────────────────────────────

var unitsCols = []string{"unitID", "unitName", "displayName"}

// GetStationSolarSystemsByOwner returns a Rowset of (corporationID, solarSystemID) for every
// station owned by ownerID (ConfigDB::GetStationSolarSystemsByOwner).
func GetStationSolarSystemsByOwner(ctx context.Context, db *sql.DB, ownerID int64) (marshal.PyRep, error) {
	cols := []string{"corporationID", "solarSystemID"}
	rows, err := db.QueryContext(ctx,
		`SELECT corporationID, solarSystemID FROM staStations WHERE corporationID = ?`, ownerID)
	if err != nil {
		return makeRowset(cols, nil), fmt.Errorf("GetStationSolarSystemsByOwner: %w", err)
	}
	defer rows.Close()
	var result [][]marshal.PyRep
	for rows.Next() {
		var corpID, solarSystemID int64
		if err := rows.Scan(&corpID, &solarSystemID); err != nil {
			return nil, fmt.Errorf("GetStationSolarSystemsByOwner scan: %w", err)
		}
		result = append(result, []marshal.PyRep{intRep(corpID), intRep(solarSystemID)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetStationSolarSystemsByOwner rows: %w", err)
	}
	return makeRowset(cols, result), nil
}

// ── GetMultiAssemblyLineTypeEx ───────────────────────────────────────────────

// assemblyLineTypeCols must match, in order, the column list of the config.BulkData.ramaltypes
// query in cache/catalog.go. On a key miss cfg.py __LoadData assigns this answer's header over
// the whole recordset, so a different header re-decodes every cached row at the wrong offsets.
var assemblyLineTypeCols = []string{
	"assemblyLineTypeID", "assemblyLineTypeName", "typeName", "description",
	"activityID", "baseTimeMultiplier", "baseMaterialMultiplier", "volume",
}

// GetMultiAssemblyLineTypeEx returns the line type rows the S&I window requests after
// AssemblyLinesSelect. It reads the full ramAssemblyLineTypes so a station's Invention lines
// resolve.
func GetMultiAssemblyLineTypeEx(ctx context.Context, db *sql.DB, ids []int64) (marshal.PyRep, error) {
	if len(ids) == 0 {
		return makeTupleSet(assemblyLineTypeCols, nil), nil
	}
	in, args := buildIn(ids)
	query := `SELECT assemblyLineTypeID, COALESCE(assemblyLineTypeName,''),
	                 COALESCE(description,''), COALESCE(activityID,1),
	                 COALESCE(baseTimeMultiplier,1), COALESCE(baseMaterialMultiplier,1),
	                 COALESCE(volume,0)
	            FROM ramassemblylinetypes WHERE assemblyLineTypeID IN ` + in
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return makeTupleSet(assemblyLineTypeCols, nil), fmt.Errorf("GetMultiAssemblyLineTypeEx: %w", err)
	}
	defer rows.Close()

	var result [][]marshal.PyRep
	for rows.Next() {
		var id, activityID int64
		var name, desc string
		var timeMul, matMul, vol float64
		if err := rows.Scan(&id, &name, &desc, &activityID, &timeMul, &matMul, &vol); err != nil {
			continue
		}
		result = append(result, []marshal.PyRep{
			intRep(id), strRep(name), strRep(name), strRep(desc),
			intRep(activityID),
			floatRep(timeMul), floatRep(matMul), floatRep(vol),
		})
	}
	return makeTupleSet(assemblyLineTypeCols, result), nil
}

// ── Recordset cache-miss fetchers ────────────────────────────────────────────
// cfg.py Recordsets fetch keys missing from the bulk snapshot through these per-table
// fetchers. The SELECT column lists must match the bulk queries in cache/catalog.go: the
// client merges fetched rows into the bulk table by the bulk header positions.

func getMultiFromTable(ctx context.Context, db *sql.DB, ids []int64, selectCols, table, keyCol, name string) (marshal.PyRep, error) {
	in, args := buildIn(ids)
	rows, err := db.QueryContext(ctx,
		`SELECT `+selectCols+` FROM `+table+` WHERE `+keyCol+` IN `+in, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	defer rows.Close()
	rep, err := evecache.BuildTupleSet(rows)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return rep, nil
}

// GetMultiInvCategoriesEx mirrors config.BulkData.categories per key.
func GetMultiInvCategoriesEx(ctx context.Context, db *sql.DB, ids []int64) (marshal.PyRep, error) {
	return getMultiFromTable(ctx, db, ids,
		"categoryID,categoryName,description,graphicID",
		"eveCategories", "categoryID", "GetMultiInvCategoriesEx")
}

// GetMultiInvGroupsEx mirrors config.BulkData.groups per key.
func GetMultiInvGroupsEx(ctx context.Context, db *sql.DB, ids []int64) (marshal.PyRep, error) {
	return getMultiFromTable(ctx, db, ids,
		"groupID,categoryID,groupName,description,graphicID,useBasePrice,allowManufacture,allowRecycler,anchored,anchorable,fittableNonSingleton",
		"invGroups", "groupID", "GetMultiInvGroupsEx")
}

// GetMultiInvMetaGroupsEx mirrors config.BulkData.metagroups per key.
func GetMultiInvMetaGroupsEx(ctx context.Context, db *sql.DB, ids []int64) (marshal.PyRep, error) {
	return getMultiFromTable(ctx, db, ids,
		"metaGroupID,metaGroupName,description,graphicID",
		"invMetaGroups", "metaGroupID", "GetMultiInvMetaGroupsEx")
}

// GetMultiBlueprintTypeEx mirrors config.BulkData.bptypes per key.
func GetMultiBlueprintTypeEx(ctx context.Context, db *sql.DB, ids []int64) (marshal.PyRep, error) {
	in, args := buildIn(ids)
	rows, err := db.QueryContext(ctx,
		`SELECT invTypes.typeName AS blueprintTypeName,invTypes.description,invTypes.graphicID,invTypes.basePrice,`+
			`blueprintTypeID,parentBlueprintTypeID,productTypeID,productionTime,techLevel,`+
			`researchProductivityTime,researchMaterialTime,researchCopyTime,researchTechTime,`+
			`productivityModifier,materialModifier,wasteFactor,chanceOfReverseEngineering,maxProductionLimit`+
			` FROM invBlueprintTypes,invTypes WHERE invBlueprintTypes.blueprintTypeID=invTypes.typeID`+
			` AND blueprintTypeID IN `+in, args...)
	if err != nil {
		return nil, fmt.Errorf("GetMultiBlueprintTypeEx: %w", err)
	}
	defer rows.Close()
	rep, err := evecache.BuildTupleSet(rows)
	if err != nil {
		return nil, fmt.Errorf("GetMultiBlueprintTypeEx: %w", err)
	}
	return rep, nil
}

// GetUnits mirrors ConfigDB::GetUnits as a util.IndexRowset keyed by unitID.
func GetUnits(ctx context.Context, db *sql.DB) (marshal.PyRep, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT unitID, unitName, displayName FROM eveUnits`)
	if err != nil {
		return nil, fmt.Errorf("GetUnits: %w", err)
	}
	defer rows.Close()

	var result [][]marshal.PyRep
	for rows.Next() {
		var unitID int64
		var unitName string
		var displayName sql.NullString
		if err := rows.Scan(&unitID, &unitName, &displayName); err != nil {
			return nil, fmt.Errorf("GetUnits scan: %w", err)
		}
		result = append(result, []marshal.PyRep{intRep(unitID), strRep(unitName), nullStrRep(displayName)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetUnits rows: %w", err)
	}
	return makeIndexRowset("unitID", unitsCols, result), nil
}
