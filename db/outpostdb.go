// Outposts are player-built stations. A corporation manufactures a construction platform,
// launches and anchors it, and Build turns it into a staStations row (CreateOutpost). The client
// UI and station types already ship; this file is the server half. Platforms use their own
// overlay table, not `starbase`, so the fuel sweep never treats one as a control tower.
// See docs/db-6-notes.md#outposts.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
)

// Outpost station IDs live in the client's station band: cfg.IsStation tests 60000000 <= id <
// 64000000. The shipped dump ends at 61000003, so new outposts start above it.
const (
	OutpostStationIDFloor = 61_000_004
	OutpostStationIDCeil  = 63_999_999
)

// staStationsSecurityConstant is the value staStations.security holds on every shipped row
// (500); new rows copy it.
const staStationsSecurityConstant = 500

// groupPlanet is mapDenormalize.groupID for a planet. An outpost records its planet as orbitID,
// which files the station under it in the client's system tree.
const groupPlanet = 7

// attrStationTypeID is dgmAttributes 472 on a construction platform: the staStations
// stationTypeID it becomes. It is the whole platform-to-station mapping.
const attrStationTypeID = "stationTypeID"

// Platform lifecycle states. A built platform no longer exists, so anchored is the only state.
const (
	PlatformStateAnchored = "anchored"
)

// OutpostPlatform is the overlay row for an anchored construction platform,
// joined onto the entity row that holds its position and typeID.
type OutpostPlatform struct {
	EntityID        int64
	SolarSystemID   int64
	PlanetID        int64
	PlanetName      string
	OwnerCorpID     int64
	OwnerAllianceID int64
	State           string
	AnchoredAt      uint64 // Win32 FILETIME

	// Joined from entity/invTypes for callers that need them.
	TypeID   int64
	TypeName string
	X, Y, Z  float64
}

// ensureOutpostPlatformTable creates the platform overlay table if missing.
// ensureOutpostPlatformTableGuard runs ensureOutpostPlatformTableDDL once per process: it is called on request paths, and
// even a no-op CREATE TABLE IF NOT EXISTS / ALTER takes a metadata lock and can
// touch the table definition each time.
var ensureOutpostPlatformTableGuard ensureGuard

func ensureOutpostPlatformTable(ctx context.Context, db DBTX) error {
	return ensureOutpostPlatformTableGuard.do(func() error { return ensureOutpostPlatformTableDDL(ctx, db) })
}

func ensureOutpostPlatformTableDDL(ctx context.Context, db DBTX) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS outpostPlatforms (
		  entityID        INT UNSIGNED NOT NULL,
		  solarSystemID   INT UNSIGNED NOT NULL,
		  planetID        INT UNSIGNED NOT NULL,
		  ownerCorpID     INT UNSIGNED NOT NULL,
		  ownerAllianceID INT UNSIGNED NOT NULL DEFAULT 0,
		  state           VARCHAR(16)  NOT NULL DEFAULT 'anchored',
		  anchoredAt      BIGINT(20)   NOT NULL DEFAULT 0,
		  PRIMARY KEY (entityID),
		  KEY idx_opf_system (solarSystemID),
		  KEY idx_opf_planet (planetID),
		  KEY idx_opf_owner  (ownerCorpID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("ensureOutpostPlatformTable: %w", err)
	}
	return nil
}

// EnsureOutpostTables creates every table this file writes.  Called once at
// boot beside the other Ensure* calls.
func EnsureOutpostTables(ctx context.Context, db *sql.DB) error {
	if err := ensureOutpostPlatformTable(ctx, db); err != nil {
		return err
	}
	return ensureOutpostPasswordTable(ctx, db)
}

const outpostPlatformSelect = `
	SELECT p.entityID, p.solarSystemID, p.planetID,
	       COALESCE(md.itemName, ''),
	       p.ownerCorpID, p.ownerAllianceID, p.state, p.anchoredAt,
	       e.typeID, COALESCE(t.typeName, ''),
	       COALESCE(e.x, 0), COALESCE(e.y, 0), COALESCE(e.z, 0)
	  FROM outpostPlatforms p
	  JOIN entity e ON e.itemID = p.entityID
	  LEFT JOIN invTypes t ON t.typeID = e.typeID
	  LEFT JOIN mapDenormalize md ON md.itemID = p.planetID`

func scanOutpostPlatform(r rowScanner) (OutpostPlatform, error) {
	var p OutpostPlatform
	err := r.Scan(&p.EntityID, &p.SolarSystemID, &p.PlanetID, &p.PlanetName,
		&p.OwnerCorpID, &p.OwnerAllianceID, &p.State, &p.AnchoredAt,
		&p.TypeID, &p.TypeName, &p.X, &p.Y, &p.Z)
	return p, err
}

// GetOutpostPlatform returns the overlay row for one anchored platform.
func GetOutpostPlatform(ctx context.Context, db *sql.DB, entityID int64) (OutpostPlatform, bool, error) {
	if err := ensureOutpostPlatformTable(ctx, db); err != nil {
		return OutpostPlatform{}, false, err
	}
	p, err := scanOutpostPlatform(db.QueryRowContext(ctx,
		outpostPlatformSelect+` WHERE p.entityID = ?`, entityID))
	if errors.Is(err, sql.ErrNoRows) {
		return OutpostPlatform{}, false, nil
	}
	if err != nil {
		return OutpostPlatform{}, false, fmt.Errorf("GetOutpostPlatform: %w", err)
	}
	return p, true, nil
}

// ListOutpostPlatforms returns every anchored platform, newest first.
func ListOutpostPlatforms(ctx context.Context, db *sql.DB) ([]OutpostPlatform, error) {
	if err := ensureOutpostPlatformTable(ctx, db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, outpostPlatformSelect+` ORDER BY p.anchoredAt DESC`)
	if err != nil {
		return nil, fmt.Errorf("ListOutpostPlatforms: %w", err)
	}
	defer rows.Close()
	var out []OutpostPlatform
	for rows.Next() {
		p, err := scanOutpostPlatform(rows)
		if err != nil {
			return nil, fmt.Errorf("ListOutpostPlatforms scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListOutpostPlatformsForSystem returns every anchored platform in a system, for the system
// loader to rebuild their balls (the Build menu hangs off the ball).
func ListOutpostPlatformsForSystem(ctx context.Context, db *sql.DB, solarSystemID int64) ([]OutpostPlatform, error) {
	if err := ensureOutpostPlatformTable(ctx, db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx,
		outpostPlatformSelect+` WHERE p.solarSystemID = ?`, solarSystemID)
	if err != nil {
		return nil, fmt.Errorf("ListOutpostPlatformsForSystem: %w", err)
	}
	defer rows.Close()
	var out []OutpostPlatform
	for rows.Next() {
		p, err := scanOutpostPlatform(rows)
		if err != nil {
			return nil, fmt.Errorf("ListOutpostPlatformsForSystem scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// LaunchedPlatform is a platform floating free in space: launched but not anchored.
type LaunchedPlatform struct {
	ItemID   int64
	TypeID   int64
	TypeName string
	OwnerID  int64
	X, Y, Z  float64
}

// ListFreePlatformsForSystem returns unanchored platforms in a system: entity rows parked at the
// solar system with no overlay row. The system loader rebuilds them, since a system unloads when
// its last player leaves and the Anchor menu hangs off the ball.
func ListFreePlatformsForSystem(ctx context.Context, db *sql.DB, solarSystemID int64) ([]LaunchedPlatform, error) {
	if err := ensureOutpostPlatformTable(ctx, db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, COALESCE(t.typeName, ''), e.ownerID,
		       COALESCE(e.x, 0), COALESCE(e.y, 0), COALESCE(e.z, 0)
		  FROM entity e
		  JOIN invTypes t ON t.typeID = e.typeID
		  LEFT JOIN outpostPlatforms p ON p.entityID = e.itemID
		 WHERE e.locationID = ? AND t.groupID = ? AND p.entityID IS NULL`,
		solarSystemID, GroupConstructionPlatform)
	if err != nil {
		return nil, fmt.Errorf("ListFreePlatformsForSystem: %w", err)
	}
	defer rows.Close()
	var out []LaunchedPlatform
	for rows.Next() {
		var p LaunchedPlatform
		if err := rows.Scan(&p.ItemID, &p.TypeID, &p.TypeName, &p.OwnerID,
			&p.X, &p.Y, &p.Z); err != nil {
			return nil, fmt.Errorf("ListFreePlatformsForSystem scan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetCorpAllianceID returns a corporation's alliance, or 0 when it is in none. Callers use it to
// colour a free platform's slim; a missing row is not an error.
func GetCorpAllianceID(ctx context.Context, db *sql.DB, corpID int64) (int64, error) {
	var allianceID sql.NullInt64
	err := db.QueryRowContext(ctx,
		`SELECT allianceID FROM corporation WHERE corporationID = ?`, corpID).Scan(&allianceID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("GetCorpAllianceID: %w", err)
	}
	if !allianceID.Valid {
		return 0, nil
	}
	return allianceID.Int64, nil
}

// PlatformStationTypeID returns the staStations.stationTypeID a platform type becomes
// (dgmTypeAttributes 472); ok is false for a type that is not a platform.
func PlatformStationTypeID(ctx context.Context, db *sql.DB, typeID int64) (int64, bool, error) {
	v, ok, err := GetTypeAttributeFloat(ctx, db, typeID, attrStationTypeID)
	if err != nil || !ok {
		return 0, false, err
	}
	return int64(v), v > 0, nil
}

// NearestPlanet returns the closest planet (groupID 7) to (x,y,z) and the distance from its
// surface, not its center, since planets span tens of thousands of km. Mirrors NearestMoon.
func NearestPlanet(ctx context.Context, db *sql.DB, solarSystemID int64, x, y, z float64) (planetID int64, planetName string, dist float64, ok bool, err error) {
	rows, err := db.QueryContext(ctx, `
		SELECT itemID, itemName, x, y, z, COALESCE(radius, 0) FROM mapDenormalize
		 WHERE solarSystemID = ? AND groupID = ?`, solarSystemID, groupPlanet)
	if err != nil {
		return 0, "", 0, false, fmt.Errorf("NearestPlanet: %w", err)
	}
	defer rows.Close()
	best := -1.0
	for rows.Next() {
		var id int64
		var name string
		var px, py, pz, radius float64
		if err := rows.Scan(&id, &name, &px, &py, &pz, &radius); err != nil {
			return 0, "", 0, false, fmt.Errorf("NearestPlanet scan: %w", err)
		}
		dx, dy, dz := x-px, y-py, z-pz
		d := math.Sqrt(dx*dx+dy*dy+dz*dz) - radius
		if d < 0 {
			d = 0
		}
		if best < 0 || d < best {
			best = d
			planetID, planetName = id, name
		}
	}
	if err := rows.Err(); err != nil {
		return 0, "", 0, false, err
	}
	if best < 0 {
		return 0, "", 0, false, nil
	}
	return planetID, planetName, best, true, nil
}

// PlanetOccupancy reports what already sits at a planet: an anchored platform or a finished
// outpost. One outpost per planet keeps the station's orbitID unambiguous.
func PlanetOccupancy(ctx context.Context, db *sql.DB, planetID int64) (occupied bool, what string, err error) {
	if err := ensureOutpostPlatformTable(ctx, db); err != nil {
		return false, "", err
	}
	var n int64
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM outpostPlatforms WHERE planetID = ?`, planetID,
	).Scan(&n); err != nil {
		return false, "", fmt.Errorf("PlanetOccupancy platforms: %w", err)
	}
	if n > 0 {
		return true, "an anchored construction platform", nil
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mapDenormalize WHERE orbitID = ? AND groupID = 15`, planetID,
	).Scan(&n); err != nil {
		return false, "", fmt.Errorf("PlanetOccupancy stations: %w", err)
	}
	if n > 0 {
		return true, "a station", nil
	}
	return false, "", nil
}

// AnchorPlatform promotes a launched, free platform into the overlay table and snaps the entity
// to the ball position. The client's AnchorObject sends no coordinates, so the caller passes
// the ball's position.
func AnchorPlatform(
	ctx context.Context, db *sql.DB,
	entityID, solarSystemID, planetID, ownerCorpID, ownerAllianceID int64,
	x, y, z float64,
) error {
	if err := ensureOutpostPlatformTable(ctx, db); err != nil {
		return err
	}
	var typeID int64
	var typeName string
	var groupID int64
	var catID sql.NullInt64
	if err := db.QueryRowContext(ctx, `
		SELECT e.typeID, t.typeName, t.groupID, g.categoryID
		  FROM entity e
		  JOIN invTypes t ON t.typeID = e.typeID
		  LEFT JOIN invGroups g ON g.groupID = t.groupID
		 WHERE e.itemID = ?`, entityID,
	).Scan(&typeID, &typeName, &groupID, &catID); err != nil {
		return fmt.Errorf("AnchorPlatform entity %d: %w", entityID, err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("AnchorPlatform begin: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		UPDATE entity SET locationID = ?, x = ?, y = ?, z = ? WHERE itemID = ?`,
		solarSystemID, x, y, z, entityID); err != nil {
		return fmt.Errorf("AnchorPlatform entity update: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO outpostPlatforms
		  (entityID, solarSystemID, planetID, ownerCorpID, ownerAllianceID, state, anchoredAt)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		entityID, solarSystemID, planetID, ownerCorpID, ownerAllianceID,
		PlatformStateAnchored, win32TimeNow()); err != nil {
		return fmt.Errorf("AnchorPlatform insert: %w", err)
	}
	cid := int64(2)
	if catID.Valid && catID.Int64 != 0 {
		cid = catID.Int64
	}
	if _, err := tx.ExecContext(ctx,
		`REPLACE INTO eveNames (itemID, itemName, categoryID, groupID, typeID) VALUES (?, ?, ?, ?, ?)`,
		entityID, typeName, cid, groupID, typeID); err != nil {
		return fmt.Errorf("AnchorPlatform eveNames: %w", err)
	}
	return tx.Commit()
}

// UnanchorPlatform drops the overlay row, leaving a free in-space item that can be scooped. The
// entity row survives.
func UnanchorPlatform(ctx context.Context, db *sql.DB, entityID int64) error {
	if err := ensureOutpostPlatformTable(ctx, db); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM outpostPlatforms WHERE entityID = ?`, entityID); err != nil {
		return fmt.Errorf("UnanchorPlatform: %w", err)
	}
	return nil
}

// ─── platform passwords ───

// Passwords are set while docked, on the packaged platform, so they key on the platform's itemID
// and not on the overlay row. The values are the client's SCCPasswordTypeGeneral (access) and
// SCCPasswordTypeConfig (build).
const (
	PlatformPasswordAccess = 1
	PlatformPasswordBuild  = 2
)

// ensureOutpostPasswordTableGuard runs ensureOutpostPasswordTableDDL once per process: it is called on request paths, and
// even a no-op CREATE TABLE IF NOT EXISTS / ALTER takes a metadata lock and can
// touch the table definition each time.
var ensureOutpostPasswordTableGuard ensureGuard

func ensureOutpostPasswordTable(ctx context.Context, db DBTX) error {
	return ensureOutpostPasswordTableGuard.do(func() error { return ensureOutpostPasswordTableDDL(ctx, db) })
}

func ensureOutpostPasswordTableDDL(ctx context.Context, db DBTX) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS outpostPlatformPasswords (
		  itemID         INT UNSIGNED NOT NULL,
		  accessPassword VARCHAR(64)  NOT NULL DEFAULT '',
		  buildPassword  VARCHAR(64)  NOT NULL DEFAULT '',
		  PRIMARY KEY (itemID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("ensureOutpostPasswordTable: %w", err)
	}
	return nil
}

// GetPlatformPasswords returns both passwords for a platform item.  Missing
// row = no passwords set, which is not an error.
func GetPlatformPasswords(ctx context.Context, db *sql.DB, itemID int64) (access, build string, err error) {
	if err := ensureOutpostPasswordTable(ctx, db); err != nil {
		return "", "", err
	}
	err = db.QueryRowContext(ctx,
		`SELECT accessPassword, buildPassword FROM outpostPlatformPasswords WHERE itemID = ?`,
		itemID).Scan(&access, &build)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("GetPlatformPasswords: %w", err)
	}
	return access, build, nil
}

// DeleteOutpostPlatform destroys an anchored platform with its overlay row, passwords, name,
// hold contents and the item. It is the admin path for a platform whose corporation can no
// longer reach Build.
func DeleteOutpostPlatform(ctx context.Context, db *sql.DB, entityID int64) error {
	if err := ensureOutpostPlatformTable(ctx, db); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("DeleteOutpostPlatform begin: %w", err)
	}
	defer tx.Rollback()
	// Hold contents first: deleting the parent alone leaves them at a locationID that resolves to
	// nothing.
	for _, stmt := range []string{
		`DELETE FROM entity WHERE locationID = ?`,
		`DELETE FROM outpostPlatforms WHERE entityID = ?`,
		`DELETE FROM outpostPlatformPasswords WHERE itemID = ?`,
		`DELETE FROM eveNames WHERE itemID = ?`,
		`DELETE FROM entity WHERE itemID = ?`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, entityID); err != nil {
			return fmt.Errorf("DeleteOutpostPlatform %q: %w", stmt, err)
		}
	}
	return tx.Commit()
}

// SetPlatformPassword writes one of a platform's two passwords. A set on a platform that already
// has one is refused unless oldPw matches; the client collects it and only the server can check.
func SetPlatformPassword(ctx context.Context, db *sql.DB, itemID int64, which int64, oldPw, newPw string) error {
	if which != PlatformPasswordAccess && which != PlatformPasswordBuild {
		return fmt.Errorf("unknown password type %d", which)
	}
	access, build, err := GetPlatformPasswords(ctx, db, itemID)
	if err != nil {
		return err
	}
	current := access
	col := "accessPassword"
	if which == PlatformPasswordBuild {
		current, col = build, "buildPassword"
	}
	if current != "" && current != oldPw {
		return fmt.Errorf("old password does not match")
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO outpostPlatformPasswords (itemID, `+col+`) VALUES (?, ?)
		ON DUPLICATE KEY UPDATE `+col+` = VALUES(`+col+`)`, itemID, newPw); err != nil {
		return fmt.Errorf("SetPlatformPassword: %w", err)
	}
	return nil
}

// IsHangarPlatform reports whether itemID is a construction platform in a hangar that charID may
// configure (the docked half of the password flow, where the item has no ball). Corp-owned
// platforms count.
func IsHangarPlatform(ctx context.Context, db *sql.DB, itemID, charID, corpID int64) bool {
	var n int64
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM entity e
		  JOIN invTypes t ON t.typeID = e.typeID
		 WHERE e.itemID = ? AND t.groupID = ? AND e.ownerID IN (?, ?)`,
		itemID, GroupConstructionPlatform, charID, corpID).Scan(&n)
	return err == nil && n > 0
}

// NewOutpost carries what CreateOutpost cannot look up. X, Y, Z is the platform's position.
type NewOutpost struct {
	PlatformID    int64
	SolarSystemID int64
	PlanetID      int64
	OwnerCorpID   int64
	StationTypeID int64
	X, Y, Z       float64
}

// CreateOutpost converts an anchored platform into a station in one transaction and returns the
// stationID. staStations and mapDenormalize are written together: space draws the ball from
// mapDenormalize.typeID. The platform's hold becomes corp hangar stock. See docs/db-6-notes.md#outposts.
func CreateOutpost(ctx context.Context, db *sql.DB, n NewOutpost) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("CreateOutpost begin: %w", err)
	}
	defer tx.Rollback()

	// Station geometry and behaviour all come off the station TYPE row; the
	// hangar graphic and undock offset follow the join on their own.
	var (
		operationID   sql.NullInt64
		reproEff      sql.NullFloat64
		reproTake     sql.NullFloat64
		reproFlag     sql.NullInt64
		stationRadius sql.NullFloat64
		typeName      string
	)
	if err := tx.QueryRowContext(ctx, `
		SELECT st.operationID, st.reprocessingEfficiency, st.reprocessingStationsTake,
		       st.reprocessingHangarFlag, t.radius, t.typeName
		  FROM staStationTypes st
		  JOIN invTypes t ON t.typeID = st.stationTypeID
		 WHERE st.stationTypeID = ?`, n.StationTypeID,
	).Scan(&operationID, &reproEff, &reproTake, &reproFlag, &stationRadius, &typeName); err != nil {
		return 0, fmt.Errorf("CreateOutpost station type %d: %w", n.StationTypeID, err)
	}
	if !operationID.Valid || operationID.Int64 == 0 {
		// An operation with no service rows is a KeyError in the client's star map and S&I window, so
		// refuse it.
		return 0, fmt.Errorf("station type %d has no operation", n.StationTypeID)
	}

	var (
		constellationID, regionID int64
		systemSec                 sql.NullFloat64
	)
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(constellationID, 0), COALESCE(regionID, 0), security
		  FROM mapSolarSystems WHERE solarSystemID = ?`, n.SolarSystemID,
	).Scan(&constellationID, &regionID, &systemSec); err != nil {
		return 0, fmt.Errorf("CreateOutpost system %d: %w", n.SolarSystemID, err)
	}

	var planetName string
	if err := tx.QueryRowContext(ctx,
		`SELECT itemName FROM mapDenormalize WHERE itemID = ?`, n.PlanetID,
	).Scan(&planetName); err != nil {
		return 0, fmt.Errorf("CreateOutpost planet %d: %w", n.PlanetID, err)
	}
	var corpName string
	if err := tx.QueryRowContext(ctx,
		`SELECT corporationName FROM corporation WHERE corporationID = ?`, n.OwnerCorpID,
	).Scan(&corpName); err != nil {
		return 0, fmt.Errorf("CreateOutpost corp %d: %w", n.OwnerCorpID, err)
	}

	// Shipped naming, e.g. "DB1R-4 II - duperTum Corp Minmatar Service Outpost". stationName is
	// VARCHAR(85) and strict mode turns an overrun into a rolled-back transaction, hence the clamp.
	stationName := clampStationName(planetName, corpName, typeName)

	stationID, err := nextOutpostStationID(ctx, tx)
	if err != nil {
		return 0, err
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO staStations
		  (stationID, security, dockingCostPerVolume, maxShipVolumeDockable,
		   officeRentalCost, operationID, stationTypeID, corporationID,
		   solarSystemID, constellationID, regionID, stationName, x, y, z,
		   reprocessingEfficiency, reprocessingStationsTake, reprocessingHangarFlag)
		VALUES (?, ?, 0, 50000000, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		stationID, staStationsSecurityConstant, operationID.Int64, n.StationTypeID,
		n.OwnerCorpID, n.SolarSystemID, constellationID, regionID, stationName,
		n.X, n.Y, n.Z,
		nullFloatOr(reproEff, 0), nullFloatOr(reproTake, 0), nullIntOr(reproFlag, 4),
	); err != nil {
		return 0, fmt.Errorf("CreateOutpost staStations: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO mapDenormalize
		  (itemID, typeID, groupID, solarSystemID, constellationID, regionID,
		   orbitID, x, y, z, radius, itemName, security)
		VALUES (?, ?, 15, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		stationID, n.StationTypeID, n.SolarSystemID, constellationID, regionID,
		n.PlanetID, n.X, n.Y, n.Z, nullFloatOr(stationRadius, 1),
		stationName, nullFloatOr(systemSec, 0),
	); err != nil {
		return 0, fmt.Errorf("CreateOutpost mapDenormalize: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`REPLACE INTO eveNames (itemID, itemName, categoryID, groupID, typeID) VALUES (?, ?, 3, 15, ?)`,
		stationID, stationName, n.StationTypeID); err != nil {
		return 0, fmt.Errorf("CreateOutpost eveNames: %w", err)
	}

	// The hold's contents become corp hangar stock (flag 4, owned by the corp): an item left
	// character-owned in a corp hangar is invisible in every window.
	if _, err := tx.ExecContext(ctx, `
		UPDATE entity SET locationID = ?, flag = 4, ownerID = ?, x = 0, y = 0, z = 0
		 WHERE locationID = ?`,
		stationID, n.OwnerCorpID, n.PlatformID); err != nil {
		return 0, fmt.Errorf("CreateOutpost hold transfer: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM outpostPlatforms WHERE entityID = ?`, n.PlatformID); err != nil {
		return 0, fmt.Errorf("CreateOutpost platform overlay delete: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM eveNames WHERE itemID = ?`, n.PlatformID); err != nil {
		return 0, fmt.Errorf("CreateOutpost platform eveNames delete: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM entity WHERE itemID = ?`, n.PlatformID); err != nil {
		return 0, fmt.Errorf("CreateOutpost platform delete: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("CreateOutpost commit: %w", err)
	}
	return stationID, nil
}

// nextOutpostStationID allocates the next free ID in the outpost band from MAX(stationID) inside
// the caller's transaction; staStations is the only authority on what exists.
func nextOutpostStationID(ctx context.Context, tx DBTX) (int64, error) {
	var maxID sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT MAX(stationID) FROM staStations`).Scan(&maxID); err != nil {
		return 0, fmt.Errorf("nextOutpostStationID: %w", err)
	}
	next := int64(OutpostStationIDFloor)
	if maxID.Valid && maxID.Int64 >= next {
		next = maxID.Int64 + 1
	}
	if next > OutpostStationIDCeil {
		return 0, fmt.Errorf("outpost station ID band exhausted (%d)", next)
	}
	return next, nil
}

// clampStationName builds "<planet> - <corp> <type>" and trims it to the 85 characters
// stationName holds, dropping the corp name first and the type second so the planet, which
// locates the station, goes last.
func clampStationName(planetName, corpName, typeName string) string {
	const maxLen = 85
	full := fmt.Sprintf("%s - %s %s", planetName, corpName, typeName)
	if len(full) <= maxLen {
		return full
	}
	shorter := fmt.Sprintf("%s - %s", planetName, typeName)
	if len(shorter) <= maxLen {
		return shorter
	}
	return strings.TrimSpace(full[:maxLen])
}

func nullFloatOr(v sql.NullFloat64, def float64) float64 {
	if v.Valid {
		return v.Float64
	}
	return def
}

func nullIntOr(v sql.NullInt64, def int64) int64 {
	if v.Valid {
		return v.Int64
	}
	return def
}

// IsStationTypeConquerable reports staStationTypes.conquerable, set on the four outpost types
// and three Conquerable hulls only.
func IsStationTypeConquerable(ctx context.Context, db *sql.DB, stationTypeID int64) (bool, error) {
	var conquerable sql.NullInt64
	err := db.QueryRowContext(ctx,
		`SELECT conquerable FROM staStationTypes WHERE stationTypeID = ?`, stationTypeID,
	).Scan(&conquerable)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("IsStationTypeConquerable: %w", err)
	}
	return conquerable.Valid && conquerable.Int64 != 0, nil
}

// CountContainerItems counts the stacks inside an in-space container (loot can, POS bay, platform
// hold): rows whose locationID is the container. Unanchoring refuses a platform that holds cargo.
func CountContainerItems(ctx context.Context, db *sql.DB, containerID int64) (int64, error) {
	var n int64
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entity WHERE locationID = ?`, containerID).Scan(&n); err != nil {
		return 0, fmt.Errorf("CountContainerItems: %w", err)
	}
	return n, nil
}

// OutpostRow is one outpost or conquerable station, for the admin panel.
type OutpostRow struct {
	StationID     int64
	StationName   string
	StationTypeID int64
	TypeName      string
	OperationID   int64
	OperationName string
	CorporationID int64
	CorpName      string
	SolarSystemID int64
	SystemName    string
	Security      float64
	RegionName    string
	OfficeSlots   int64
	DockedPilots  int64
	Offices       int64
}

// ListOutposts returns every station in the outpost band, including the three shipped ones.
func ListOutposts(ctx context.Context, db *sql.DB) ([]OutpostRow, error) {
	return listOutpostRows(ctx, db, `s.stationID >= ?`, OutpostStationIDFloor-3)
}

// ListConquerableStations returns the NPC-built conquerable stations of nullsec: every station
// whose type has staStationTypes.conquerable set, below the outpost band.
func ListConquerableStations(ctx context.Context, db *sql.DB) ([]OutpostRow, error) {
	return listOutpostRows(ctx, db, `st.conquerable = 1 AND s.stationID < ?`, OutpostStationIDFloor-3)
}

// listOutpostRows runs the shared outpost panel query narrowed by where. Office slots follow
// stationOfficeCapacity, and docked pilots count accountID > 0 because NPC agents carry a
// stationID too.
func listOutpostRows(ctx context.Context, db *sql.DB, where string, args ...any) ([]OutpostRow, error) {
	maxSlots := int64(FlagOfficeSlotLast - FlagOfficeSlotFirst + 1)
	rows, err := db.QueryContext(ctx, `
		SELECT s.stationID, s.stationName, s.stationTypeID,
		       COALESCE(t.typeName, ''), COALESCE(s.operationID, 0),
		       COALESCE(op.operationName, ''),
		       s.corporationID, COALESCE(c.corporationName, ''),
		       s.solarSystemID, COALESCE(sys.solarSystemName, ''),
		       COALESCE(sys.security, 0), COALESCE(r.regionName, ''),
		       CASE WHEN st.officeSlots BETWEEN 1 AND ? THEN st.officeSlots ELSE ? END,
		       COALESCE(pc.n, 0), COALESCE(oc.n, 0)
		  FROM staStations s
		  LEFT JOIN invTypes t ON t.typeID = s.stationTypeID
		  LEFT JOIN staStationTypes st ON st.stationTypeID = s.stationTypeID
		  LEFT JOIN staOperations op ON op.operationID = s.operationID
		  LEFT JOIN corporation c ON c.corporationID = s.corporationID
		  LEFT JOIN mapSolarSystems sys ON sys.solarSystemID = s.solarSystemID
		  LEFT JOIN mapRegions r ON r.regionID = s.regionID
		  LEFT JOIN (SELECT stationID, COUNT(*) n FROM character_
		              WHERE accountID > 0 GROUP BY stationID) pc
		         ON pc.stationID = s.stationID
		  LEFT JOIN (SELECT stationID, COUNT(*) n FROM crpoffices
		              GROUP BY stationID) oc
		         ON oc.stationID = s.stationID
		 WHERE `+where+`
		 ORDER BY s.stationName`, append([]any{maxSlots, maxSlots}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("listOutpostRows: %w", err)
	}
	defer rows.Close()
	var out []OutpostRow
	for rows.Next() {
		var o OutpostRow
		if err := rows.Scan(&o.StationID, &o.StationName, &o.StationTypeID,
			&o.TypeName, &o.OperationID, &o.OperationName,
			&o.CorporationID, &o.CorpName,
			&o.SolarSystemID, &o.SystemName, &o.Security, &o.RegionName,
			&o.OfficeSlots, &o.DockedPilots, &o.Offices); err != nil {
			return nil, fmt.Errorf("listOutpostRows scan: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
