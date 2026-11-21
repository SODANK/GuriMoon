package db

import (
	"context"
	"database/sql"

	"gurimoon/marshal"
)

// GetStationExtraInfo returns a 3-tuple (stations, opServices, services) mirroring
// MapDB::GetStationExtraInfo, GetStationOpServices and GetStationServiceInfo. The client joins
// the three on operationID itself. See docs/db-10-notes.md#station-services.
//
//	[0] util.Rowset: stationID, solarSystemID, operationID, stationTypeID, ownerID
//	[1] util.Rowset: operationID, serviceID
//	[2] util.Rowset: serviceID, serviceName
func GetStationExtraInfo(ctx context.Context, db *sql.DB) (marshal.PyRep, error) {
	stations, err := queryRows(ctx, db,
		[]string{"stationID", "solarSystemID", "operationID", "stationTypeID", "ownerID"},
		`SELECT stationID, solarSystemID, operationID, stationTypeID,
		        corporationID AS ownerID
		   FROM staStations`)
	if err != nil {
		return nil, err
	}

	opServices, err := queryRows(ctx, db,
		[]string{"operationID", "serviceID"},
		`SELECT operationID, serviceID FROM staOperationServices`)
	if err != nil {
		return nil, err
	}

	services, err := queryRows(ctx, db,
		[]string{"serviceID", "serviceName"},
		`SELECT serviceID, serviceName FROM staServices`)
	if err != nil {
		return nil, err
	}

	return marshal.PyTuple{Items: []marshal.PyRep{stations, opServices, services}}, nil
}

// GetSolarSystemSecurities returns a util.Rowset of solarSystemID+security for
// every solar system, mirroring MapDB::GetPseudoSecurities. Negative security is
// clamped to 0.0: the client colours a system with GetSecColorList()[int(sec*10)]
// (util.FmtSystemSecStatus), so a negative value wraps to the green end of the list.
// The 0e0 literal keeps the column DOUBLE; a DECIMAL would scan as bytes.
func GetSolarSystemSecurities(ctx context.Context, db *sql.DB) (marshal.PyRep, error) {
	return queryRows(ctx, db,
		[]string{"solarSystemID", "security"},
		`SELECT solarSystemID, GREATEST(security, 0e0) FROM mapSolarSystems`)
}

// ─── star-map overlay data ─────────────────────────────────────────────────
//
// These back the F10 map's colour-stars-by modes (ui/shared/maps/map.py). Each must return a
// correctly shaped, possibly empty result: the client indexes rows unchecked, and a bare None
// raises AttributeError in the map redraw.

// ensureSolarSystemVisitsTable creates the per-character visit log used by the
// STARMODE_VISITED overlay.
// ensureSolarSystemVisitsTableGuard runs ensureSolarSystemVisitsTableDDL once per process: it is called on request paths, and
// even a no-op CREATE TABLE IF NOT EXISTS / ALTER takes a metadata lock and can
// touch the table definition each time.
var ensureSolarSystemVisitsTableGuard ensureGuard

func ensureSolarSystemVisitsTable(ctx context.Context, db *sql.DB) error {
	return ensureSolarSystemVisitsTableGuard.do(func() error { return ensureSolarSystemVisitsTableDDL(ctx, db) })
}

func ensureSolarSystemVisitsTableDDL(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS chrSolarSystemVisits (
		  characterID   INT UNSIGNED NOT NULL,
		  solarSystemID INT UNSIGNED NOT NULL,
		  visits        INT UNSIGNED NOT NULL DEFAULT 0,
		  lastDateTime  BIGINT(20)   NOT NULL DEFAULT 0,
		  PRIMARY KEY (characterID, solarSystemID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	return err
}

// RecordSolarSystemVisit bumps the visit counter for a character entering a
// system.  win32Now is a Windows FILETIME, matching what the client renders.
func RecordSolarSystemVisit(ctx context.Context, db *sql.DB, charID, solarSystemID int64, win32Now uint64) error {
	if charID == 0 || solarSystemID == 0 {
		return nil
	}
	if err := ensureSolarSystemVisitsTable(ctx, db); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO chrSolarSystemVisits (characterID, solarSystemID, visits, lastDateTime)
		VALUES (?, ?, 1, ?)
		ON DUPLICATE KEY UPDATE visits = visits + 1, lastDateTime = VALUES(lastDateTime)`,
		charID, solarSystemID, win32Now)
	return err
}

// GetSolarSystemVisits returns (solarSystemID, visits, lastDateTime) for every
// system the character has entered.
func GetSolarSystemVisits(ctx context.Context, db *sql.DB, charID int64) (marshal.PyRep, error) {
	cols := []string{"solarSystemID", "visits", "lastDateTime"}
	if err := ensureSolarSystemVisitsTable(ctx, db); err != nil {
		return makeRowset(cols, nil), nil
	}
	return queryRows(ctx, db, cols,
		`SELECT solarSystemID, visits, lastDateTime
		 FROM chrSolarSystemVisits WHERE characterID = ?`, charID)
}

// GetStationCountPerSystem returns a list of (solarSystemID, stationCount) tuples; map.py
// unpacks the pairs directly rather than reading a rowset.
func GetStationCountPerSystem(ctx context.Context, db *sql.DB) (marshal.PyRep, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT solarSystemID, COUNT(*) FROM staStations GROUP BY solarSystemID`)
	if err != nil {
		return marshal.PyList{}, nil
	}
	defer rows.Close()

	var items []marshal.PyRep
	for rows.Next() {
		var systemID, count int64
		if err := rows.Scan(&systemID, &count); err != nil {
			return nil, err
		}
		items = append(items, marshal.PyTuple{Items: []marshal.PyRep{
			intRep(systemID), intRep(count),
		}})
	}
	return marshal.PyList{Items: items}, rows.Err()
}

// GetDeadspaceComplexMap returns (solarSystemID, dungeonID, difficulty, dungeonName) tuples for
// the star map's DED Deadspace Report (STARMODE_DUNGEONS). map.py sizes a star by tuple count,
// colours it by the highest difficulty and lists "name (DED: n)" per tuple; difficulty 0 prints
// no rating. Sources and exclusions: docs/db-11-notes.md#deadspace-report.
func GetDeadspaceComplexMap(ctx context.Context, db *sql.DB) (marshal.PyRep, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT x.solarSystemID, d.dungeonVID, COALESCE(d.dedLevel, 0), d.dungeonVName
		FROM (
			SELECT o.solarSystemID, o.dungeonVID
			FROM dunOpenSites o
			WHERE o.clearedAt IS NULL AND o.beaconVisible = 1
			UNION ALL
			SELECT DISTINCT e.solarSystemID, r.dungeonVID
			FROM npcDungeonEntities e
			JOIN dunRooms r ON r.roomID = e.dunRoomID
			WHERE e.solarSystemID > 0 AND e.missionID = 0
			  AND NOT EXISTS (SELECT 1 FROM dunOpenSites o2
			                  WHERE o2.solarSystemID = e.solarSystemID
			                    AND o2.dungeonVID = r.dungeonVID)
		) x
		JOIN dunDungeons d ON d.dungeonVID = x.dungeonVID
		ORDER BY x.solarSystemID, d.dungeonVName`)
	if err != nil {
		return marshal.PyList{}, nil
	}
	defer rows.Close()

	var items []marshal.PyRep
	for rows.Next() {
		var systemID, dungeonID, ded int64
		var name string
		if err := rows.Scan(&systemID, &dungeonID, &ded, &name); err != nil {
			return nil, err
		}
		items = append(items, marshal.PyTuple{Items: []marshal.PyRep{
			intRep(systemID), intRep(dungeonID), intRep(ded), strRep(name),
		}})
	}
	return marshal.PyList{Items: items}, rows.Err()
}

// GetAllianceSystems returns (solarSystemID, allianceID) for every claimed
// system.  Drives the alliance-territory colouring on the star map.
func GetAllianceSystems(ctx context.Context, db *sql.DB) (marshal.PyRep, error) {
	cols := []string{"solarSystemID", "allianceID"}
	if err := ensureSolarSystemSovTable(ctx, db); err != nil {
		return makeRowset(cols, nil), nil
	}
	return queryRows(ctx, db, cols,
		`SELECT solarSystemID, allianceID FROM solarSystemSov WHERE allianceID > 0`)
}

// GetCorpMemberLocations returns (characterID, locationID) for every online
// member of a corporation, where locationID is the solar system they are in.
// Feeds the "show corp mates" overlay.
func GetCorpMemberLocations(ctx context.Context, db *sql.DB, corpID int64, charIDs []int64) (marshal.PyRep, error) {
	cols := []string{"characterID", "locationID"}
	if corpID == 0 || len(charIDs) == 0 {
		return makeRowset(cols, nil), nil
	}
	args := make([]any, 0, len(charIDs)+1)
	args = append(args, corpID)
	for _, id := range charIDs {
		args = append(args, id)
	}
	return queryRows(ctx, db, cols, `
		SELECT c.characterID, c.solarSystemID
		FROM character_ c
		WHERE c.corporationID = ? AND c.characterID IN (`+placeholders(len(charIDs))+`)
		  AND c.solarSystemID > 0`, args...)
}

// GetCharLocations returns (characterID, locationID) for the given characters regardless of
// corp. Used for the gang overlay.
func GetCharLocations(ctx context.Context, db *sql.DB, charIDs []int64) (marshal.PyRep, error) {
	cols := []string{"characterID", "locationID"}
	if len(charIDs) == 0 {
		return makeRowset(cols, nil), nil
	}
	args := make([]any, 0, len(charIDs))
	for _, id := range charIDs {
		args = append(args, id)
	}
	return queryRows(ctx, db, cols, `
		SELECT characterID, solarSystemID
		FROM character_
		WHERE characterID IN (`+placeholders(len(charIDs))+`) AND solarSystemID > 0`, args...)
}

// GetSolarSystemTaxRates returns average station-owning corporation tax rate
// per system as (solarSystemID, loadRatio).  Feeds the F10 "Tax Level" overlay.
func GetSolarSystemTaxRates(ctx context.Context, db *sql.DB) (marshal.PyRep, error) {
	return queryRows(ctx, db,
		[]string{"solarSystemID", "loadRatio"},
		`SELECT s.solarSystemID, AVG(c.taxRate) AS loadRatio
		   FROM staStations s
		   JOIN corporation c ON c.corporationID = s.corporationID
		  GROUP BY s.solarSystemID`)
}

// landmarkCols is the row shape ui/shared/maps/map.py:ReadLandmarks expects.
var landmarkCols = []string{
	"landmarkID", "landmarkName", "description", "locationID",
	"x", "y", "z", "radius", "importance", "url2d", "url3d",
}

// MakeLandmarkRowset wraps landmark rows in the util.Rowset the map service
// returns.  Passing nil yields a well-formed empty set.
func MakeLandmarkRowset(rows [][]marshal.PyRep) marshal.PyRep {
	return makeRowset(landmarkCols, rows)
}
