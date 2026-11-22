// Package db: player-owned starbases. The tower is an entity row; the starbase
// table is the overlay holding its moon, owning alliance and state. Fuel burn is
// StarbaseFuelNeeds and ConsumeFromStarbaseBay, driven by the sweep in
// cmd/gurimoon/starbase.go. See docs/starbase-pos-investigation.md.
package db

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"sync"
)

// Starbase state names, stored as strings in starbase.state.
const (
	StarbaseStateAnchored   = "anchored"
	StarbaseStateOnlining   = "onlining"
	StarbaseStateOnline     = "online"
	StarbaseStateReinforced = "reinforced"
	// StarbaseStateUnanchoring is a structure counting down its unanchoringDelay. It
	// stays anchored (a rigid ball holding its moon and sov) until the delay has run.
	StarbaseStateUnanchoring = "unanchoring"
)

// PosStateInt maps the state string to the client's slimItem.posState
// (playerOwned.py: 0 unanchored, 1 anchored, 2 onlining, 3 reinforced, 4 online).
// "reinforced" maps to 3 only while ReinforceUntil is reinforceSlimLead ahead: the
// client's GetStructureState returns None for state 3 once its timestamp has passed.
func (r StarbaseRecord) PosStateInt() int64 {
	return r.PosStateAt(win32TimeNow())
}

// reinforceSlimLead is how far ahead of ReinforceUntil a reinforced tower
// stops presenting posState 3 (5 s, in FILETIME ticks).
const reinforceSlimLead = 5 * 10000000

// PosStateAt is PosStateInt against an explicit clock, for tests.
func (r StarbaseRecord) PosStateAt(now uint64) int64 {
	switch r.State {
	case StarbaseStateOnlining:
		return 2 // STRUCTURE_ONLINING
	case StarbaseStateReinforced:
		if r.ReinforceUntil > now+reinforceSlimLead {
			return 3 // STRUCTURE_REINFORCED
		}
		return 4
	case StarbaseStateOnline:
		return 4 // STRUCTURE_ONLINE
	case StarbaseStateUnanchoring:
		return 0 // STRUCTURE_UNANCHORED, with a running unanchoring timestamp
	default:
		return 1 // STRUCTURE_ANCHORED
	}
}

// PosTimestamp returns slimItem.posTimestamp for the record's state (Win32 FILETIME,
// 0 = None): plant time while anchored, onlining start while onlining, unanchoring
// start while unanchoring, and the future end of reinforcement while reinforced.
// The client counts the type's delay down from it. Online towers carry none.
func (r StarbaseRecord) PosTimestamp() uint64 {
	return r.PosTimestampAt(win32TimeNow())
}

// PosTimestampAt is PosTimestamp against an explicit clock; it must agree
// with PosStateAt about whether a reinforced tower still reads as state 3.
func (r StarbaseRecord) PosTimestampAt(now uint64) uint64 {
	switch r.State {
	case StarbaseStateAnchored:
		return r.AnchoredAt
	case StarbaseStateOnlining:
		return r.OnlinedAt
	case StarbaseStateUnanchoring:
		return r.UnanchorAt
	case StarbaseStateReinforced:
		if r.PosStateAt(now) == 3 {
			return r.ReinforceUntil
		}
		return 0
	default:
		return 0
	}
}

// StarbaseRecord describes one anchored POS, joining the starbase overlay
// onto the entity row that holds its position/typeID.
type StarbaseRecord struct {
	EntityID        int64
	SolarSystemID   int64
	MoonID          int64
	TypeID          int64
	GroupID         int64
	CategoryID      int64
	OwnerCorpID     int64
	OwnerAllianceID int64
	State           string
	X, Y, Z         float64
	Radius          float64
	ItemName        string
	AnchoredAt      uint64
	OnlinedAt       uint64
	// ControlTowerID is the tower a slave is anchored to; 0 for a tower. The client
	// needs it on the slim to draw onlining FX (playerOwned.py).
	ControlTowerID int64
	// ProvisionTypeID is what a slave handles: the moon material a harvester pulls or
	// the type a silo stores (posMgr.ChangeStructureProvisionType). 0 = unset, which
	// is what the client's "None" entry sends.
	ProvisionTypeID int64
	// UnanchorAt is when unanchoring began; the client counts unanchoringDelay down
	// from it. 0 unless the state is unanchoring.
	UnanchorAt uint64
	// ReinforceUntil is when a reinforced tower comes back online (Win32 FILETIME).
	// The client requires it to be in the future while posState is 3.
	ReinforceUntil uint64
}

var (
	starbaseSchemaOnce sync.Once
	starbaseSchemaErr  error
)

// ensureStarbaseTable creates the starbase overlay table, adds the later columns
// and the moon-uniqueness index. Every query path calls it, so it runs once per
// process.
func ensureStarbaseTable(ctx context.Context, db *sql.DB) error {
	starbaseSchemaOnce.Do(func() {
		if _, err := db.ExecContext(ctx, `
			CREATE TABLE IF NOT EXISTS starbase (
			  entityID        INT UNSIGNED NOT NULL,
			  solarSystemID   INT UNSIGNED NOT NULL,
			  moonID          INT UNSIGNED NOT NULL,
			  ownerCorpID     INT UNSIGNED NOT NULL,
			  ownerAllianceID INT UNSIGNED NOT NULL DEFAULT 0,
			  state           VARCHAR(16)  NOT NULL DEFAULT 'anchored',
			  anchoredAt      BIGINT(20)   NOT NULL DEFAULT 0,
			  onlinedAt       BIGINT(20)   NOT NULL DEFAULT 0,
			  fuelTick        BIGINT(20)   NOT NULL DEFAULT 0,
			  password        VARCHAR(64)  NOT NULL DEFAULT '',
			  PRIMARY KEY (entityID),
			  KEY idx_system (solarSystemID),
			  KEY idx_moon   (moonID),
			  KEY idx_owner  (ownerCorpID)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`); err != nil {
			starbaseSchemaErr = err
			return
		}
		// Columns added after the table first shipped.
		for _, col := range []struct{ name, ddl string }{
			{"password", "VARCHAR(64) NOT NULL DEFAULT ''"},
			{"fuelTick", "BIGINT(20) NOT NULL DEFAULT 0"},
			{"reinforceUntil", "BIGINT(20) NOT NULL DEFAULT 0"},
			{"unanchorAt", "BIGINT(20) NOT NULL DEFAULT 0"},
		} {
			if err := addStarbaseColumn(ctx, db, col.name, col.ddl); err != nil {
				starbaseSchemaErr = err
				return
			}
		}
		ensureStarbaseMoonUnique(ctx, db)
	})
	return starbaseSchemaErr
}

// EnsureStarbaseSchema brings the starbase and starbaseStructures tables up to
// date in one call. The boot reconcile calls it so a DDL failure lands in the
// startup log rather than on a player's first anchor.
func EnsureStarbaseSchema(ctx context.Context, db *sql.DB) error {
	if err := ensureStarbaseTable(ctx, db); err != nil {
		return err
	}
	return ensureStarbaseStructureTable(ctx, db)
}

// ensureStarbaseMoonUnique adds the one-tower-per-moon unique index. The occupancy
// checks in PlantStarbase and AnchorLaunchedStarbase cannot see an uncommitted
// row, so the index is what holds the rule. Towers only: a slave's moonID of 0
// would collide, so slaves have their own overlay. Failure is logged, never fatal.
func ensureStarbaseMoonUnique(ctx context.Context, db *sql.DB) {
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.statistics
		 WHERE table_schema = DATABASE() AND table_name = 'starbase'
		   AND index_name = 'uq_starbase_moon'`).Scan(&n); err != nil {
		log.Printf("starbase: moon-unique index probe: %v", err)
		return
	}
	if n > 0 {
		return
	}
	var dupes int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM (
		  SELECT moonID FROM starbase GROUP BY moonID HAVING COUNT(*) > 1
		) d`).Scan(&dupes); err != nil {
		log.Printf("starbase: moon-duplicate probe: %v", err)
		return
	}
	if dupes > 0 {
		log.Printf("starbase: %d moon(s) carry more than one tower — leaving uq_starbase_moon off; "+
			"unanchor the extras and restart to apply it", dupes)
		return
	}
	if _, err := db.ExecContext(ctx,
		`CREATE UNIQUE INDEX uq_starbase_moon ON starbase (moonID)`); err != nil {
		log.Printf("starbase: create uq_starbase_moon: %v", err)
	}
}

// PlantStarbase creates a Control Tower entity at moonID for ownerCorpID and its
// starbase row in state "anchored"; OnlineStarbase brings it online. Returns the
// new entity ID. The group is not validated, and placement policy is the caller's
// (CheckStarbaseAnchorPolicy).
func PlantStarbase(
	ctx context.Context, db *sql.DB,
	typeID, moonID, ownerCorpID, ownerAllianceID int64,
) (int64, error) {
	if err := ensureStarbaseTable(ctx, db); err != nil {
		return 0, fmt.Errorf("PlantStarbase ensureTable: %w", err)
	}

	// Look up the moon's position and host solar system from mapDenormalize.
	var (
		solarSystemID int64
		mx, my, mz    float64
		moonRadius    sql.NullFloat64
		moonName      string
	)
	err := db.QueryRowContext(ctx, `
		SELECT solarSystemID, x, y, z, radius, itemName
		  FROM mapDenormalize
		 WHERE itemID = ?`, moonID,
	).Scan(&solarSystemID, &mx, &my, &mz, &moonRadius, &moonName)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("PlantStarbase: moon %d not found in mapDenormalize", moonID)
	}
	if err != nil {
		return 0, fmt.Errorf("PlantStarbase moon lookup: %w", err)
	}
	if solarSystemID == 0 {
		return 0, fmt.Errorf("PlantStarbase: moon %d has no parent solar system", moonID)
	}

	// The tower sits where TowerAnchorPosition puts one: just in from the moon's
	// warp-in (MoonTowerAnchorPoint), clear of the moon's surface.
	_, towerAt, _, err := MoonTowerAnchorPoint(ctx, db, moonID)
	if err != nil {
		return 0, fmt.Errorf("PlantStarbase moon position: %w", err)
	}
	tx, ty, tz := towerAt[0], towerAt[1], towerAt[2]

	// Look up the type's display name (entity row) and group/category
	// (eveNames row) in one go.
	var (
		typeName    string
		typeGroupID int64
		typeCatID   sql.NullInt64
	)
	if err := db.QueryRowContext(ctx, `
		SELECT t.typeName, t.groupID, g.categoryID
		  FROM invTypes t
		  LEFT JOIN invGroups g ON g.groupID = t.groupID
		 WHERE t.typeID = ?`, typeID,
	).Scan(&typeName, &typeGroupID, &typeCatID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("PlantStarbase: typeID %d not found in invTypes", typeID)
		}
		return 0, fmt.Errorf("PlantStarbase type lookup: %w", err)
	}

	now := win32TimeNow()

	tx2, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("PlantStarbase begin: %w", err)
	}
	defer tx2.Rollback()

	// One tower per moon, checked inside the transaction; uq_starbase_moon covers a
	// concurrent plant this SELECT cannot see.
	occupied, err := moonOccupied(ctx, tx2, moonID)
	if err != nil {
		return 0, fmt.Errorf("PlantStarbase moon-occupancy check: %w", err)
	}
	if occupied {
		return 0, fmt.Errorf("PlantStarbase: moon %d (%s) already has a starbase", moonID, moonName)
	}

	// Entity row: flag 0 (in space), singleton 1 (assembled), quantity 1, located at
	// the solar system, owned by the corp.
	res, err := tx2.ExecContext(ctx, `
		INSERT INTO entity
		  (itemName, typeID, ownerID, locationID, flag, contraband, singleton,
		   quantity, x, y, z, customInfo)
		VALUES (?, ?, ?, ?, 0, 0, 1, 1, ?, ?, ?, NULL)`,
		typeName, typeID, ownerCorpID, solarSystemID, tx, ty, tz,
	)
	if err != nil {
		return 0, fmt.Errorf("PlantStarbase entity insert: %w", err)
	}
	entityID, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("PlantStarbase LastInsertId: %w", err)
	}

	// Create the starbase overlay row.
	if _, err := tx2.ExecContext(ctx, `
		INSERT INTO starbase
		  (entityID, solarSystemID, moonID, ownerCorpID, ownerAllianceID,
		   state, anchoredAt, onlinedAt)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0)`,
		entityID, solarSystemID, moonID, ownerCorpID, ownerAllianceID,
		StarbaseStateAnchored, now,
	); err != nil {
		if isDuplicateKeyErr(err) {
			return 0, fmt.Errorf("PlantStarbase: moon %d (%s) already has a starbase", moonID, moonName)
		}
		return 0, fmt.Errorf("PlantStarbase starbase insert: %w", err)
	}

	// eveNames row so itemID to name lookups (celestial statistics, lookupSvc)
	// resolve the tower.
	catID := int64(23) // category Structure
	if typeCatID.Valid && typeCatID.Int64 != 0 {
		catID = typeCatID.Int64
	}
	if _, err := tx2.ExecContext(ctx,
		`REPLACE INTO eveNames (itemID, itemName, categoryID, groupID, typeID) VALUES (?, ?, ?, ?, ?)`,
		entityID, typeName, catID, typeGroupID, typeID,
	); err != nil {
		return 0, fmt.Errorf("PlantStarbase eveNames: %w", err)
	}

	if err := tx2.Commit(); err != nil {
		return 0, fmt.Errorf("PlantStarbase commit: %w", err)
	}
	return entityID, nil
}

// GroupConstructionPlatform is invGroups 307, the Outpost Construction Platforms.
// It is categoryID 2, not 23, so every category-23 test names it separately; its
// client menu is the generic AnchorObject, not posMgr.AnchorStructure.
const GroupConstructionPlatform = 307

// LaunchedStructure describes a cargo item eligible for ship.Drop as a
// structure: category 23 (POS structures) or group 307 (outpost platforms).
type LaunchedStructure struct {
	ItemID     int64
	TypeID     int64
	GroupID    int64
	CategoryID int64
	TypeName   string
	Radius     float64
	Mass       float64
}

// IsLaunchableStructure reports whether a type may be dropped into space as a
// standing structure: category 23, an outpost construction platform (group 307)
// or a deployable (IsDeployableGroup). All follow the same launch and anchor path.
func IsLaunchableStructure(categoryID, groupID int64) bool {
	return categoryID == 23 || groupID == GroupConstructionPlatform || IsDeployableGroup(groupID)
}

// Deployables: anchorable groups outside category 23 with the generic anchor
// effects 649/650 (Secure Cargo 340, Audit Log 448, Mobile Sentry Gun 336, Mobile
// Warp Disruptor 361). They launch, anchor standalone (towerID 0) and scoop like Mobile Storage.
const (
	GroupMobileSentryGun     int64 = 336
	GroupMobileWarpDisruptor int64 = 361
)

// IsDeployableGroup reports whether groupID is one of the deployables above.
func IsDeployableGroup(groupID int64) bool {
	switch groupID {
	case GroupSecureCargoContainer, GroupAuditLogContainer, GroupMobileSentryGun, GroupMobileWarpDisruptor:
		return true
	}
	return false
}

// IsStructureLike is the launched-ball test the standalone anchor, scoop and
// reload paths share: category 23 or a deployable.
func IsStructureLike(categoryID, groupID int64) bool {
	return categoryID == 23 || IsDeployableGroup(groupID)
}

// deployableGroupSQL is IsDeployableGroup for a WHERE clause on invTypes t.
const deployableGroupSQL = "t.groupID IN (340, 448, 336, 361)"

// GetCargoStructure returns type/geometry info for a structure item currently
// in shipID's cargo. ok=false when the item isn't in that cargo or isn't a
// launchable structure (see IsLaunchableStructure).
func GetCargoStructure(ctx context.Context, db *sql.DB, itemID, shipID int64) (LaunchedStructure, bool, error) {
	var s LaunchedStructure
	var radius, mass sql.NullFloat64
	err := db.QueryRowContext(ctx, `
		SELECT e.itemID, e.typeID, t.groupID, COALESCE(g.categoryID, 0),
		       t.typeName, t.radius, t.mass
		  FROM entity e
		  JOIN invTypes t ON t.typeID = e.typeID
		  LEFT JOIN invGroups g ON g.groupID = t.groupID
		 WHERE e.itemID = ? AND e.locationID = ?`, itemID, shipID,
	).Scan(&s.ItemID, &s.TypeID, &s.GroupID, &s.CategoryID, &s.TypeName, &radius, &mass)
	if errors.Is(err, sql.ErrNoRows) {
		return s, false, nil
	}
	if err != nil {
		return s, false, fmt.Errorf("GetCargoStructure: %w", err)
	}
	if !IsLaunchableStructure(s.CategoryID, s.GroupID) {
		return s, false, nil
	}
	s.Radius = 1500
	if radius.Valid && radius.Float64 > 0 {
		s.Radius = radius.Float64
	}
	s.Mass = 100000
	if mass.Valid && mass.Float64 > 0 {
		s.Mass = mass.Float64
	}
	return s, true, nil
}

// LaunchStructureToSpace moves a structure item from a ship's cargo into
// space: locationID = solar system, ownerID = launching corp, flag 0.
func LaunchStructureToSpace(ctx context.Context, db *sql.DB, itemID, ownerID, solarSystemID int64, x, y, z float64) error {
	res, err := db.ExecContext(ctx, `
		UPDATE entity SET locationID = ?, ownerID = ?, flag = 0, singleton = 1,
		       x = ?, y = ?, z = ?
		 WHERE itemID = ?`,
		solarSystemID, ownerID, x, y, z, itemID)
	if err != nil {
		return fmt.Errorf("LaunchStructureToSpace: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("LaunchStructureToSpace: item %d not found", itemID)
	}
	return nil
}

// FreeStructure is an unanchored category-23 structure sitting in space:
// everything needed to rebuild its ball, plus where it is and who owns it.
type FreeStructure struct {
	Structure     LaunchedStructure
	OwnerID       int64
	X, Y, Z       float64
	SolarSystemID int64
}

// ListFreeStructuresForSystem returns the launched-but-unanchored structures in a
// system: category-23 or deployable entity rows parked at the solar system with no
// starbase or starbaseStructures row. The system loader rebuilds their balls; the
// Anchor menu hangs off the ball.
func ListFreeStructuresForSystem(ctx context.Context, db *sql.DB, solarSystemID int64) ([]FreeStructure, error) {
	if err := ensureStarbaseTable(ctx, db); err != nil {
		return nil, err
	}
	// Both overlays, or an anchored slave reads as free and gets a second ball.
	if err := ensureStarbaseStructureTable(ctx, db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, t.groupID, COALESCE(g.categoryID, 0),
		       COALESCE(t.typeName, ''), COALESCE(t.radius, 1500),
		       COALESCE(t.mass, 100000), e.ownerID,
		       COALESCE(e.x, 0), COALESCE(e.y, 0), COALESCE(e.z, 0)
		  FROM entity e
		  JOIN invTypes t  ON t.typeID = e.typeID
		  JOIN invGroups g ON g.groupID = t.groupID
		  LEFT JOIN starbase s ON s.entityID = e.itemID
		  LEFT JOIN starbaseStructures ss ON ss.entityID = e.itemID
		 WHERE e.locationID = ? AND (g.categoryID = 23 OR `+deployableGroupSQL+`)
		   AND s.entityID IS NULL AND ss.entityID IS NULL`,
		solarSystemID)
	if err != nil {
		return nil, fmt.Errorf("ListFreeStructuresForSystem: %w", err)
	}
	defer rows.Close()
	var out []FreeStructure
	for rows.Next() {
		var f FreeStructure
		if err := rows.Scan(
			&f.Structure.ItemID, &f.Structure.TypeID, &f.Structure.GroupID,
			&f.Structure.CategoryID, &f.Structure.TypeName, &f.Structure.Radius,
			&f.Structure.Mass, &f.OwnerID, &f.X, &f.Y, &f.Z,
		); err != nil {
			return nil, fmt.Errorf("ListFreeStructuresForSystem scan: %w", err)
		}
		f.SolarSystemID = solarSystemID
		out = append(out, f)
	}
	return out, rows.Err()
}

// AnchorLaunchedStarbase promotes a launched, unanchored control tower entity into
// the starbase overlay: a starbase row in state "anchored", the entity snapped to
// the anchor position, an eveNames row. The entity row must already exist;
// PlantStarbase creates one from nothing.
func AnchorLaunchedStarbase(
	ctx context.Context, db *sql.DB,
	entityID, moonID, ownerCorpID, ownerAllianceID int64,
	x, y, z float64,
) error {
	if err := ensureStarbaseTable(ctx, db); err != nil {
		return fmt.Errorf("AnchorLaunchedStarbase ensureTable: %w", err)
	}
	var (
		solarSystemID int64
		moonName      string
	)
	err := db.QueryRowContext(ctx,
		`SELECT solarSystemID, itemName FROM mapDenormalize WHERE itemID = ?`, moonID,
	).Scan(&solarSystemID, &moonName)
	if err != nil {
		return fmt.Errorf("AnchorLaunchedStarbase moon %d: %w", moonID, err)
	}


	var (
		typeID   int64
		typeName string
		groupID  int64
		catID    sql.NullInt64
	)
	if err := db.QueryRowContext(ctx, `
		SELECT e.typeID, t.typeName, t.groupID, g.categoryID
		  FROM entity e
		  JOIN invTypes t ON t.typeID = e.typeID
		  LEFT JOIN invGroups g ON g.groupID = t.groupID
		 WHERE e.itemID = ?`, entityID,
	).Scan(&typeID, &typeName, &groupID, &catID); err != nil {
		return fmt.Errorf("AnchorLaunchedStarbase entity %d: %w", entityID, err)
	}

	now := win32TimeNow()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("AnchorLaunchedStarbase begin: %w", err)
	}
	defer tx.Rollback()
	// One tower per moon, checked inside the transaction (see PlantStarbase).
	occupied, err := moonOccupied(ctx, tx, moonID)
	if err != nil {
		return fmt.Errorf("AnchorLaunchedStarbase occupancy: %w", err)
	}
	if occupied {
		return fmt.Errorf("moon %s already has a starbase", moonName)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE entity SET locationID = ?, x = ?, y = ?, z = ? WHERE itemID = ?`,
		solarSystemID, x, y, z, entityID); err != nil {
		return fmt.Errorf("AnchorLaunchedStarbase entity update: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO starbase
		  (entityID, solarSystemID, moonID, ownerCorpID, ownerAllianceID,
		   state, anchoredAt, onlinedAt)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0)`,
		entityID, solarSystemID, moonID, ownerCorpID, ownerAllianceID,
		StarbaseStateAnchored, now); err != nil {
		if isDuplicateKeyErr(err) {
			return fmt.Errorf("moon %s already has a starbase", moonName)
		}
		return fmt.Errorf("AnchorLaunchedStarbase starbase insert: %w", err)
	}
	cid := int64(23)
	if catID.Valid && catID.Int64 != 0 {
		cid = catID.Int64
	}
	if _, err := tx.ExecContext(ctx,
		`REPLACE INTO eveNames (itemID, itemName, categoryID, groupID, typeID) VALUES (?, ?, ?, ?, ?)`,
		entityID, typeName, cid, groupID, typeID); err != nil {
		return fmt.Errorf("AnchorLaunchedStarbase eveNames: %w", err)
	}
	return tx.Commit()
}

// OfflineStarbase transitions "online" (or a still-counting "onlining") back
// to "anchored".  Returns the new state.
func OfflineStarbase(ctx context.Context, db *sql.DB, entityID int64) (string, error) {
	if err := ensureStarbaseTable(ctx, db); err != nil {
		return "", fmt.Errorf("OfflineStarbase ensureTable: %w", err)
	}
	res, err := db.ExecContext(ctx,
		`UPDATE starbase SET state = ? WHERE entityID = ? AND state IN (?, ?)`,
		StarbaseStateAnchored, entityID, StarbaseStateOnline, StarbaseStateOnlining)
	if err != nil {
		return "", fmt.Errorf("OfflineStarbase: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var state string
		if err := db.QueryRowContext(ctx,
			`SELECT state FROM starbase WHERE entityID = ?`, entityID).Scan(&state); err != nil {
			return "", fmt.Errorf("OfflineStarbase: starbase %d not found", entityID)
		}
		return state, fmt.Errorf("starbase is in state %q; cannot offline", state)
	}
	return StarbaseStateAnchored, nil
}

// BeginStarbaseUnanchor starts a tower's unanchoring countdown (the type's
// unanchoringDelay). Only an anchored, offline tower may begin it, as the client's
// CanUnanchorStructure asks. Returns the start time.
func BeginStarbaseUnanchor(ctx context.Context, db *sql.DB, entityID int64) (uint64, error) {
	if err := ensureStarbaseTable(ctx, db); err != nil {
		return 0, fmt.Errorf("BeginStarbaseUnanchor ensureTable: %w", err)
	}
	now := win32TimeNow()
	res, err := db.ExecContext(ctx, `
		UPDATE starbase SET state = ?, unanchorAt = ?
		 WHERE entityID = ? AND state = ?`,
		StarbaseStateUnanchoring, now, entityID, StarbaseStateAnchored)
	if err != nil {
		return 0, fmt.Errorf("BeginStarbaseUnanchor: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, fmt.Errorf("the control tower must be anchored and offline to unanchor")
	}
	return now, nil
}

// UnanchorStarbase demotes an anchored tower back to a launched structure: the
// starbase and eveNames rows are removed, the entity row stays in space
// (RemoveStarbase deletes everything). Allowed from "unanchoring" (the caller
// checks the delay) and "anchored"; an online tower must be offlined first.
func UnanchorStarbase(ctx context.Context, db *sql.DB, entityID int64) (StarbaseRecord, error) {
	if err := ensureStarbaseTable(ctx, db); err != nil {
		return StarbaseRecord{}, fmt.Errorf("UnanchorStarbase ensureTable: %w", err)
	}
	rec, ok, err := GetStarbase(ctx, db, entityID)
	if err != nil {
		return StarbaseRecord{}, err
	}
	if !ok {
		return StarbaseRecord{}, fmt.Errorf("UnanchorStarbase: starbase %d not found", entityID)
	}
	if rec.State != StarbaseStateAnchored && rec.State != StarbaseStateUnanchoring {
		return rec, fmt.Errorf("starbase is in state %q; offline it before unanchoring", rec.State)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return rec, fmt.Errorf("UnanchorStarbase begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM starbase WHERE entityID = ?`, entityID); err != nil {
		return rec, fmt.Errorf("UnanchorStarbase starbase delete: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM eveNames WHERE itemID = ?`, entityID); err != nil {
		return rec, fmt.Errorf("UnanchorStarbase eveNames delete: %w", err)
	}
	return rec, tx.Commit()
}

// moonOccupied reports whether a starbase already sits on moonID. It takes the
// transaction so the check runs in the same snapshot as the insert.
func moonOccupied(ctx context.Context, tx *sql.Tx, moonID int64) (bool, error) {
	var n int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM starbase WHERE moonID = ?`, moonID).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// isDuplicateKeyErr reports MySQL error 1062 (duplicate entry), matched on the
// message so db adds no driver dependency.
func isDuplicateKeyErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Error 1062")
}

// BackfillStarbaseMoon fills a starbase row whose moonID is 0 with the nearest
// moon to the tower's position. The management window reports "(not attached to
// moon)" (moonmining.py) for such a row and its Moon tab stays empty. Returns the
// moonID now stored, 0 when nothing was done.
func BackfillStarbaseMoon(ctx context.Context, db *sql.DB, entityID int64) (int64, error) {
	if err := ensureStarbaseTable(ctx, db); err != nil {
		return 0, err
	}
	var (
		moonID        int64
		solarSystemID int64
		x, y, z       float64
	)
	err := db.QueryRowContext(ctx, `
		SELECT s.moonID, s.solarSystemID, e.x, e.y, e.z
		  FROM starbase s JOIN entity e ON e.itemID = s.entityID
		 WHERE s.entityID = ?`, entityID,
	).Scan(&moonID, &solarSystemID, &x, &y, &z)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("BackfillStarbaseMoon %d: %w", entityID, err)
	}
	if moonID != 0 {
		return moonID, nil
	}
	found, name, dist, ok, err := NearestMoon(ctx, db, solarSystemID, x, y, z)
	if err != nil {
		return 0, fmt.Errorf("BackfillStarbaseMoon %d nearest: %w", entityID, err)
	}
	if !ok {
		log.Printf("starbase %d has moonID 0 and its system has no moons — left alone", entityID)
		return 0, nil
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE starbase SET moonID = ? WHERE entityID = ? AND moonID = 0`,
		found, entityID); err != nil {
		if isDuplicateKeyErr(err) {
			log.Printf("starbase %d: nearest moon %s already carries a tower — moonID left 0", entityID, name)
			return 0, nil
		}
		return 0, fmt.Errorf("BackfillStarbaseMoon %d update: %w", entityID, err)
	}
	log.Printf("starbase %d: backfilled moonID %d (%s, %.0f km away)", entityID, found, name, dist/1000)
	return found, nil
}

// NearestMoon returns the closest moon (groupID 8) to (x,y,z) in the system and
// the distance in metres from the moon's surface (centre distance minus radius;
// moons are 100-300 km bodies). ok=false when the system has no moons.
func NearestMoon(ctx context.Context, db *sql.DB, solarSystemID int64, x, y, z float64) (moonID int64, moonName string, dist float64, ok bool, err error) {
	rows, err := db.QueryContext(ctx, `
		SELECT itemID, itemName, x, y, z, COALESCE(radius, 0) FROM mapDenormalize
		 WHERE solarSystemID = ? AND groupID = 8`, solarSystemID)
	if err != nil {
		return 0, "", 0, false, fmt.Errorf("NearestMoon: %w", err)
	}
	defer rows.Close()
	best := -1.0
	for rows.Next() {
		var id int64
		var name string
		var mx, my, mz, radius float64
		if err := rows.Scan(&id, &name, &mx, &my, &mz, &radius); err != nil {
			return 0, "", 0, false, fmt.Errorf("NearestMoon scan: %w", err)
		}
		dx, dy, dz := x-mx, y-my, z-mz
		d := math.Sqrt(dx*dx+dy*dy+dz*dz) - radius
		if d < 0 {
			d = 0
		}
		if best < 0 || d < best {
			best = d
			moonID, moonName = id, name
		}
	}
	if err := rows.Err(); err != nil {
		return 0, "", 0, false, err
	}
	if best < 0 {
		return 0, "", 0, false, nil
	}
	return moonID, moonName, best, true, nil
}

// BayAddResult reports what AddToStarbaseBay changed so the caller can send
// the matching OnItemChange notifications.
type BayAddResult struct {
	SourceItemID  int64 // the row the player dragged
	SourceDeleted bool  // whole stack consumed (merged into an existing one)
	SourceMoved   bool  // whole row re-parented into the bay
	SourceOldQty  int64 // quantity before a partial move (0 when not partial)
	TargetItemID  int64 // the row inside the bay that now holds the goods
	TargetOldQty  int64 // its quantity before the add (0 when newly created)
	TargetNew     bool  // a new row was inserted in the bay
}

// ErrBayFull is returned by AddToStarbaseBay when the units would not fit in
// the structure's capacity.
var ErrBayFull = errors.New("not enough room in the structure")

// BayHasRoom reports whether add m3 fits beside used m3 in a bay of capacity m3.
// A capacity of zero or less means the type declares none and reads as unlimited.
// See docs/starbase-pos-investigation.md#15-item-sources-bay-withdraw-and-capacity.
func BayHasRoom(capacity, used, add float64) bool {
	if capacity <= 0 {
		return true
	}
	return used+add <= capacity+1e-6
}

// bayQuerier is the subset of *sql.DB / *sql.Tx the bay volume query needs.
type bayQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row
}

// bayUsedVolume sums the volume of everything in structureID's bay flag (flag < 0:
// every flag; the corp hangar divisions share one capacity).
func bayUsedVolume(ctx context.Context, q bayQuerier, structureID, flag int64) (float64, error) {
	var used sql.NullFloat64
	err := q.QueryRowContext(ctx, `
		SELECT SUM(e.quantity * COALESCE(t.volume, 0))
		  FROM entity e JOIN invTypes t ON t.typeID = e.typeID
		 WHERE e.locationID = ? AND (? < 0 OR e.flag = ?)`, structureID, flag, flag).Scan(&used)
	if err != nil {
		return 0, err
	}
	return used.Float64, nil
}

// StructureBayCapacity answers InventoryBound.GetCapacity for a structure bay:
// the type's invTypes.capacity and the volume already in that flag (flag < 0:
// all flags).
func StructureBayCapacity(ctx context.Context, db *sql.DB, structureID, flag int64) (capacity, used float64, err error) {
	var c sql.NullFloat64
	if err = db.QueryRowContext(ctx, `
		SELECT t.capacity FROM entity e JOIN invTypes t ON t.typeID = e.typeID
		 WHERE e.itemID = ?`, structureID).Scan(&c); err != nil {
		return 0, 0, fmt.Errorf("StructureBayCapacity %d: %w", structureID, err)
	}
	used, err = bayUsedVolume(ctx, db, structureID, flag)
	if err != nil {
		return 0, 0, fmt.Errorf("StructureBayCapacity used %d: %w", structureID, err)
	}
	return c.Float64, used, nil
}

// AddToStarbaseBay moves qty units of itemID into structureID's bay at flag (0 for
// an ordinary bay, 27 for a sentry's loaded ammunition); qty <= 0 or over the stack
// moves everything. ownerOverride > 0 re-owns the moved units; sharedCapacity
// checks volume across every flag. A row already inside the structure skips the check.
func AddToStarbaseBay(ctx context.Context, db *sql.DB, structureID, itemID, qty, flag, ownerOverride int64, sharedCapacity bool) (BayAddResult, error) {
	var res BayAddResult
	res.SourceItemID = itemID

	var (
		typeID, ownerID, srcQty, singleton, srcLoc, srcFlag int64
		typeName                                            string
		unitVol                                             float64
	)
	err := db.QueryRowContext(ctx, `
		SELECT e.typeID, e.ownerID, e.quantity, e.singleton, COALESCE(e.itemName, ''),
		       COALESCE(t.volume, 0), e.locationID, e.flag
		  FROM entity e JOIN invTypes t ON t.typeID = e.typeID WHERE e.itemID = ?`, itemID,
	).Scan(&typeID, &ownerID, &srcQty, &singleton, &typeName, &unitVol, &srcLoc, &srcFlag)
	if errors.Is(err, sql.ErrNoRows) {
		return res, fmt.Errorf("AddToStarbaseBay: item %d not found", itemID)
	}
	if err != nil {
		return res, fmt.Errorf("AddToStarbaseBay lookup: %w", err)
	}
	if srcQty <= 0 {
		return res, fmt.Errorf("AddToStarbaseBay: item %d has no units", itemID)
	}
	moveQty := qty
	if moveQty <= 0 || moveQty > srcQty {
		moveQty = srcQty
	}
	if ownerOverride > 0 {
		ownerID = ownerOverride
	}
	internal := srcLoc == structureID

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("AddToStarbaseBay begin: %w", err)
	}
	defer tx.Rollback()

	// Lock the structure row alone: a FOR UPDATE with a JOIN locks the joined rows too.
	var structTypeID int64
	if err := tx.QueryRowContext(ctx,
		`SELECT typeID FROM entity WHERE itemID = ? FOR UPDATE`, structureID).Scan(&structTypeID); err != nil {
		return res, fmt.Errorf("AddToStarbaseBay lock structure %d: %w", structureID, err)
	}
	var capacity sql.NullFloat64
	if err := tx.QueryRowContext(ctx,
		`SELECT capacity FROM invTypes WHERE typeID = ?`, structTypeID).Scan(&capacity); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return res, fmt.Errorf("AddToStarbaseBay capacity: %w", err)
	}
	capFlag := flag
	if sharedCapacity {
		capFlag = -1
	}
	used, err := bayUsedVolume(ctx, tx, structureID, capFlag)
	if err != nil {
		return res, fmt.Errorf("AddToStarbaseBay used volume: %w", err)
	}
	if !internal && !BayHasRoom(capacity.Float64, used, unitVol*float64(moveQty)) {
		return res, ErrBayFull
	}

	// An existing same-type, same-owner loose stack in the bay absorbs the
	// new units. A singleton (an assembled item) never merges, nor does a split
	// in place (same bay flag): the pilot asked for a second stack.
	var dstItemID, dstQty int64
	hasTarget := false
	if singleton == 0 && !(internal && srcFlag == flag) {
		err = tx.QueryRowContext(ctx, `
			SELECT itemID, quantity FROM entity
			 WHERE locationID = ? AND flag = ? AND typeID = ? AND ownerID = ?
			   AND singleton = 0 AND itemID <> ?
			 ORDER BY itemID LIMIT 1`, structureID, flag, typeID, ownerID, itemID,
		).Scan(&dstItemID, &dstQty)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return res, fmt.Errorf("AddToStarbaseBay target lookup: %w", err)
		}
		hasTarget = err == nil
	}

	full := moveQty >= srcQty
	switch {
	case full && !hasTarget:
		// Re-parent the whole row into the bay.
		if _, err := tx.ExecContext(ctx,
			`UPDATE entity SET locationID = ?, flag = ?, ownerID = ? WHERE itemID = ?`,
			structureID, flag, ownerID, itemID); err != nil {
			return res, fmt.Errorf("AddToStarbaseBay move: %w", err)
		}
		// Loose modules carry no online state inside a bay.
		_, _ = tx.ExecContext(ctx,
			`DELETE FROM entity_attributes WHERE itemID = ? AND attributeID = 2`, itemID)
		res.SourceMoved = true
		res.TargetItemID = itemID

	case full && hasTarget:
		if _, err := tx.ExecContext(ctx,
			`UPDATE entity SET quantity = quantity + ? WHERE itemID = ?`,
			moveQty, dstItemID); err != nil {
			return res, fmt.Errorf("AddToStarbaseBay merge: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM entity_attributes WHERE itemID = ?`, itemID); err != nil {
			return res, fmt.Errorf("AddToStarbaseBay source attrs: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM entity WHERE itemID = ?`, itemID); err != nil {
			return res, fmt.Errorf("AddToStarbaseBay source delete: %w", err)
		}
		res.SourceDeleted = true
		res.TargetItemID = dstItemID
		res.TargetOldQty = dstQty

	default: // partial move
		if _, err := tx.ExecContext(ctx,
			`UPDATE entity SET quantity = quantity - ? WHERE itemID = ?`,
			moveQty, itemID); err != nil {
			return res, fmt.Errorf("AddToStarbaseBay decrement: %w", err)
		}
		res.SourceOldQty = srcQty
		if hasTarget {
			if _, err := tx.ExecContext(ctx,
				`UPDATE entity SET quantity = quantity + ? WHERE itemID = ?`,
				moveQty, dstItemID); err != nil {
				return res, fmt.Errorf("AddToStarbaseBay merge partial: %w", err)
			}
			res.TargetItemID = dstItemID
			res.TargetOldQty = dstQty
		} else {
			r, err := tx.ExecContext(ctx, `
				INSERT INTO entity
				  (itemName, typeID, ownerID, locationID, flag, contraband,
				   singleton, quantity, x, y, z, customInfo)
				VALUES (?, ?, ?, ?, ?, 0, 0, ?, 0, 0, 0, NULL)`,
				typeName, typeID, ownerID, structureID, flag, moveQty)
			if err != nil {
				return res, fmt.Errorf("AddToStarbaseBay insert: %w", err)
			}
			newID, err := r.LastInsertId()
			if err != nil {
				return res, fmt.Errorf("AddToStarbaseBay LastInsertId: %w", err)
			}
			res.TargetItemID = newID
			res.TargetNew = true
		}
	}

	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("AddToStarbaseBay commit: %w", err)
	}
	return res, nil
}

// OnlineStarbase moves a starbase from "anchored" to "onlining", storing the start
// time in onlinedAt (the client counts the type's onliningDelay down from it). The
// caller schedules the CompleteStarbaseOnlining tick. Returns the new state name;
// fails from any other state.
func OnlineStarbase(ctx context.Context, db *sql.DB, entityID int64) (string, error) {
	if err := ensureStarbaseTable(ctx, db); err != nil {
		return "", fmt.Errorf("OnlineStarbase ensureTable: %w", err)
	}
	var state string
	err := db.QueryRowContext(ctx,
		`SELECT state FROM starbase WHERE entityID = ?`, entityID,
	).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("OnlineStarbase: starbase %d not found", entityID)
	}
	if err != nil {
		return "", fmt.Errorf("OnlineStarbase lookup: %w", err)
	}
	if state == StarbaseStateOnline || state == StarbaseStateOnlining {
		return state, nil
	}
	if state != StarbaseStateAnchored {
		return state, fmt.Errorf("starbase is in state %q; can only online from %q",
			state, StarbaseStateAnchored)
	}
	now := win32TimeNow()
	if _, err := db.ExecContext(ctx,
		`UPDATE starbase SET state = ?, onlinedAt = ? WHERE entityID = ?`,
		StarbaseStateOnlining, now, entityID,
	); err != nil {
		return "", fmt.Errorf("OnlineStarbase update: %w", err)
	}
	return StarbaseStateOnlining, nil
}

// CompleteStarbaseOnlining finishes the "onlining" to "online" transition. A no-op
// (ok=false) when the tower is gone or no longer onlining, so a remove or
// re-anchor between schedule and fire cannot resurrect state.
func CompleteStarbaseOnlining(ctx context.Context, db *sql.DB, entityID int64) (bool, error) {
	if err := ensureStarbaseTable(ctx, db); err != nil {
		return false, fmt.Errorf("CompleteStarbaseOnlining ensureTable: %w", err)
	}
	res, err := db.ExecContext(ctx,
		`UPDATE starbase SET state = ?, onlinedAt = ? WHERE entityID = ? AND state = ?`,
		StarbaseStateOnline, win32TimeNow(), entityID, StarbaseStateOnlining,
	)
	if err != nil {
		return false, fmt.Errorf("CompleteStarbaseOnlining: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetStarbasePassword stores the tower's force-field password
// (posMgr.SetTowerPassword).
func SetStarbasePassword(ctx context.Context, db *sql.DB, entityID int64, password string) error {
	if err := ensureStarbasePasswordColumn(ctx, db); err != nil {
		return err
	}
	res, err := db.ExecContext(ctx,
		`UPDATE starbase SET password = ? WHERE entityID = ?`, password, entityID)
	if err != nil {
		return fmt.Errorf("SetStarbasePassword: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// RowsAffected is also 0 when the password is unchanged, so verify
		// the tower actually exists before reporting an error.
		var one int
		if err := db.QueryRowContext(ctx,
			`SELECT 1 FROM starbase WHERE entityID = ?`, entityID).Scan(&one); errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("SetStarbasePassword: starbase %d not found", entityID)
		}
	}
	return nil
}

// GetStarbasePassword returns the stored force-field password ("" when unset
// or the tower is unknown).
func GetStarbasePassword(ctx context.Context, db *sql.DB, entityID int64) (string, error) {
	if err := ensureStarbasePasswordColumn(ctx, db); err != nil {
		return "", err
	}
	var pw string
	err := db.QueryRowContext(ctx,
		`SELECT password FROM starbase WHERE entityID = ?`, entityID).Scan(&pw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("GetStarbasePassword: %w", err)
	}
	return pw, nil
}

// CheckStarbasePassword reports whether password matches the tower's stored
// force-field secret. A tower with no password set matches nothing: an empty
// secret is no password, not any password.
func CheckStarbasePassword(ctx context.Context, db *sql.DB, entityID int64, password string) (bool, error) {
	if err := ensureStarbasePasswordColumn(ctx, db); err != nil {
		return false, err
	}
	var stored string
	err := db.QueryRowContext(ctx, `SELECT password FROM starbase WHERE entityID = ?`, entityID).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("CheckStarbasePassword: %w", err)
	}
	return stored != "" && subtle.ConstantTimeCompare([]byte(stored), []byte(password)) == 1, nil
}

// ensureStarbasePasswordColumn and ensureStarbaseFuelColumn are the names their
// call sites use; both run ensureStarbaseTable.
func ensureStarbasePasswordColumn(ctx context.Context, db *sql.DB) error {
	return ensureStarbaseTable(ctx, db)
}

func ensureStarbaseFuelColumn(ctx context.Context, db *sql.DB) error {
	return ensureStarbaseTable(ctx, db)
}

// addStarbaseColumn adds a column to an existing starbase table, probing
// information_schema first (MySQL 8 has no ADD COLUMN IF NOT EXISTS). Called only
// inside ensureStarbaseTable's sync.Once, so it must not call back into it.
func addStarbaseColumn(ctx context.Context, db *sql.DB, column, ddl string) error {
	var n int
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.columns
		 WHERE table_schema = DATABASE() AND table_name = 'starbase'
		   AND column_name = ?`, column).Scan(&n)
	if err != nil {
		return fmt.Errorf("addStarbaseColumn(%s) probe: %w", column, err)
	}
	if n > 0 {
		return nil
	}
	if _, err := db.ExecContext(ctx,
		fmt.Sprintf(`ALTER TABLE starbase ADD COLUMN %s %s`, column, ddl)); err != nil {
		return fmt.Errorf("addStarbaseColumn(%s) alter: %w", column, err)
	}
	return nil
}

// RemoveStarbase deletes the starbase overlay row and its backing entity (the
// /posremove teardown).
func RemoveStarbase(ctx context.Context, db *sql.DB, entityID int64) error {
	if err := ensureStarbaseTable(ctx, db); err != nil {
		return fmt.Errorf("RemoveStarbase ensureTable: %w", err)
	}
	if err := ensureStarbaseStructureTable(ctx, db); err != nil {
		return fmt.Errorf("RemoveStarbase ensureStructureTable: %w", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("RemoveStarbase begin: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`DELETE FROM starbase WHERE entityID = ?`, entityID)
	if err != nil {
		return fmt.Errorf("RemoveStarbase starbase delete: %w", err)
	}
	// Slaves bound to the tower lose their binding row; the structures stay in space
	// as free balls.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM starbaseStructures WHERE towerID = ?`, entityID); err != nil {
		return fmt.Errorf("RemoveStarbase structures delete: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("RemoveStarbase: starbase %d not found", entityID)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM entity WHERE itemID = ?`, entityID,
	); err != nil {
		return fmt.Errorf("RemoveStarbase entity delete: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM eveNames WHERE itemID = ?`, entityID,
	); err != nil {
		return fmt.Errorf("RemoveStarbase eveNames delete: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// The tower's Access / Force Field / Defense settings go with it. Outside the
	// transaction and best-effort: a stale row is inert, no entity reuses this ID.
	_ = DeleteStarbasePolicy(ctx, db, entityID)
	return nil
}

// GetStarbase returns one record by entityID.
func GetStarbase(ctx context.Context, db *sql.DB, entityID int64) (StarbaseRecord, bool, error) {
	if err := ensureStarbaseTable(ctx, db); err != nil {
		return StarbaseRecord{}, false, fmt.Errorf("GetStarbase ensureTable: %w", err)
	}
	rec, err := scanStarbase(db.QueryRowContext(ctx, starbaseSelect+` WHERE s.entityID = ?`, entityID))
	if errors.Is(err, sql.ErrNoRows) {
		return StarbaseRecord{}, false, nil
	}
	if err != nil {
		return StarbaseRecord{}, false, err
	}
	return rec, true, nil
}

// CountStarbasesForAllianceInSystem returns how many starbases allianceID has
// anchored in solarSystemID.
func CountStarbasesForAllianceInSystem(ctx context.Context, db *sql.DB, solarSystemID, allianceID int64) (int, error) {
	if err := ensureStarbaseTable(ctx, db); err != nil {
		return 0, fmt.Errorf("CountStarbasesForAllianceInSystem ensureTable: %w", err)
	}
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM starbase WHERE solarSystemID = ? AND ownerAllianceID = ?`,
		solarSystemID, allianceID,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("CountStarbasesForAllianceInSystem: %w", err)
	}
	return n, nil
}

// ListStarbasesByState returns every starbase in the given state across all
// systems; the onlining sweep uses it to finish transitions lost to a restart.
func ListStarbasesByState(ctx context.Context, db *sql.DB, state string) ([]StarbaseRecord, error) {
	if err := ensureStarbaseTable(ctx, db); err != nil {
		return nil, fmt.Errorf("ListStarbasesByState ensureTable: %w", err)
	}
	rows, err := db.QueryContext(ctx,
		starbaseSelect+` WHERE s.state = ? ORDER BY s.entityID`, state)
	if err != nil {
		return nil, fmt.Errorf("ListStarbasesByState: %w", err)
	}
	defer rows.Close()
	var out []StarbaseRecord
	for rows.Next() {
		rec, err := scanStarbase(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// ListAllStarbases returns every starbase on the shard, whatever its state. The
// boot reconcile is the only caller; every other list is scoped.
func ListAllStarbases(ctx context.Context, db *sql.DB) ([]StarbaseRecord, error) {
	if err := ensureStarbaseTable(ctx, db); err != nil {
		return nil, fmt.Errorf("ListAllStarbases ensureTable: %w", err)
	}
	rows, err := db.QueryContext(ctx, starbaseSelect+` ORDER BY s.entityID`)
	if err != nil {
		return nil, fmt.Errorf("ListAllStarbases: %w", err)
	}
	defer rows.Close()
	var out []StarbaseRecord
	for rows.Next() {
		rec, err := scanStarbase(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// StarbaseOverview is one row of the ESP starbase list: the record plus the names
// the panel shows.
type StarbaseOverview struct {
	StarbaseRecord
	MoonName       string
	SystemName     string
	TypeName       string
	CorpName       string
	AllianceName   string
	StructureCount int
}

// ListStarbaseOverview returns every tower with its names and how many structures
// are anchored to it, in one pass; names are joined, not looked up per row.
func ListStarbaseOverview(ctx context.Context, db *sql.DB) ([]StarbaseOverview, error) {
	if err := ensureStarbaseTable(ctx, db); err != nil {
		return nil, err
	}
	if err := ensureStarbaseStructureTable(ctx, db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT s.entityID, s.solarSystemID, s.moonID, e.typeID, t.groupID,
		       COALESCE(g.categoryID, 0), s.ownerCorpID, s.ownerAllianceID, s.state,
		       e.x, e.y, e.z, COALESCE(t.radius, 1500), COALESCE(e.itemName, ''),
		       s.anchoredAt, s.onlinedAt,
		       COALESCE(m.itemName, ''), COALESCE(sys.itemName, ''),
		       COALESCE(t.typeName, ''), COALESCE(c.corporationName, ''),
		       COALESCE(a.allianceName, ''),
		       COALESCE(sc.n, 0)
		  FROM starbase s
		  JOIN entity   e ON e.itemID = s.entityID
		  JOIN invTypes t ON t.typeID = e.typeID
		  LEFT JOIN invGroups g ON g.groupID = t.groupID
		  LEFT JOIN mapDenormalize m   ON m.itemID = s.moonID
		  LEFT JOIN mapDenormalize sys ON sys.itemID = s.solarSystemID
		  LEFT JOIN corporation c ON c.corporationID = s.ownerCorpID
		  LEFT JOIN alliances a ON a.allianceID = s.ownerAllianceID
		  LEFT JOIN (SELECT towerID, COUNT(*) n FROM starbaseStructures GROUP BY towerID) sc
		         ON sc.towerID = s.entityID
		 ORDER BY s.entityID`)
	if err != nil {
		return nil, fmt.Errorf("ListStarbaseOverview: %w", err)
	}
	defer rows.Close()
	var out []StarbaseOverview
	for rows.Next() {
		var o StarbaseOverview
		if err := rows.Scan(
			&o.EntityID, &o.SolarSystemID, &o.MoonID, &o.TypeID, &o.GroupID,
			&o.CategoryID, &o.OwnerCorpID, &o.OwnerAllianceID, &o.State,
			&o.X, &o.Y, &o.Z, &o.Radius, &o.ItemName,
			&o.AnchoredAt, &o.OnlinedAt,
			&o.MoonName, &o.SystemName, &o.TypeName, &o.CorpName, &o.AllianceName,
			&o.StructureCount,
		); err != nil {
			return nil, fmt.Errorf("ListStarbaseOverview scan: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// OrphanStarbaseIDs returns starbase rows whose backing entity has gone.
// starbaseSelect INNER JOINs entity, so such a row is invisible to every other
// query here (not listed, loaded, unanchored or removed) and goes on holding its
// moon against the uniqueness rule.
func OrphanStarbaseIDs(ctx context.Context, db *sql.DB) ([]int64, error) {
	if err := ensureStarbaseTable(ctx, db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT s.entityID FROM starbase s
		 LEFT JOIN entity e ON e.itemID = s.entityID
		 WHERE e.itemID IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("OrphanStarbaseIDs: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("OrphanStarbaseIDs scan: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// UnbackedSovSystems returns systems whose sovereignty is held by an alliance with
// no tower left there. Reported, never auto-cleared: sov has other sources (an
// outpost, an admin grant).
func UnbackedSovSystems(ctx context.Context, db *sql.DB) ([]int64, error) {
	if err := ensureStarbaseTable(ctx, db); err != nil {
		return nil, err
	}
	if err := ensureSolarSystemSovTable(ctx, db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT v.solarSystemID FROM solarSystemSov v
		 LEFT JOIN starbase s
		        ON s.solarSystemID = v.solarSystemID
		       AND s.ownerAllianceID = v.allianceID
		 WHERE s.entityID IS NULL
		 GROUP BY v.solarSystemID`)
	if err != nil {
		return nil, fmt.Errorf("UnbackedSovSystems: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("UnbackedSovSystems scan: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ListStarbasesForSystem returns every starbase anchored in solarSystemID.
// Used by /poslist and by the in-space loader (registerUndock).
func ListStarbasesForSystem(ctx context.Context, db *sql.DB, solarSystemID int64) ([]StarbaseRecord, error) {
	if err := ensureStarbaseTable(ctx, db); err != nil {
		return nil, fmt.Errorf("ListStarbasesForSystem ensureTable: %w", err)
	}
	rows, err := db.QueryContext(ctx,
		starbaseSelect+` WHERE s.solarSystemID = ? ORDER BY s.entityID`, solarSystemID)
	if err != nil {
		return nil, fmt.Errorf("ListStarbasesForSystem: %w", err)
	}
	defer rows.Close()
	var out []StarbaseRecord
	for rows.Next() {
		rec, err := scanStarbase(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// ControlTowerResource is one row of invControlTowerResources, the static fuel
// spec for a tower type. purpose is 1 Online, 2 Power, 3 CPU, 4 Reinforce.
// MinSecurityLevel and FactionID are set only on faction charter rows.
type ControlTowerResource struct {
	ControlTowerTypeID int64
	ResourceTypeID     int64
	Purpose            int64
	Quantity           int64
	MinSecurityLevel   sql.NullFloat64
	FactionID          sql.NullInt64
}

// GetTypeAttributeFloat returns a dogma attribute value for a type, resolved by
// attribute name (dgmAttributeTypes.attributeName). Used for the POS timing and
// geometry attributes (anchoringDelay, onliningDelay, shieldRadius) that must
// match what the client derives from the same tables. ok=false when absent.
func GetTypeAttributeFloat(ctx context.Context, db *sql.DB, typeID int64, attributeName string) (float64, bool, error) {
	var vInt, vFloat sql.NullFloat64
	err := db.QueryRowContext(ctx, `
		SELECT ta.valueInt, ta.valueFloat
		  FROM dgmTypeAttributes ta
		  JOIN dgmAttributeTypes at ON at.attributeID = ta.attributeID
		 WHERE ta.typeID = ? AND at.attributeName = ?`,
		typeID, attributeName,
	).Scan(&vInt, &vFloat)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("GetTypeAttributeFloat(%d,%s): %w", typeID, attributeName, err)
	}
	if vInt.Valid {
		return vInt.Float64, true, nil
	}
	if vFloat.Valid {
		return vFloat.Float64, true, nil
	}
	return 0, false, nil
}

// StarbaseManagePermitted reports whether a pilot may operate a tower (online,
// offline, unanchor, change its settings): the owning alliance when the tower has
// one, else the owning corp. corpRoleStarbaseConfig is not enforced here.
// See docs/starbase-pos-investigation.md#27-anchoring-policy-and-majority-sovereignty.
func StarbaseManagePermitted(rec StarbaseRecord, corpID, allianceID int64) bool {
	if rec.OwnerAllianceID != 0 {
		return allianceID == rec.OwnerAllianceID
	}
	return corpID == rec.OwnerCorpID
}

// DropSovIfLastStarbase recomputes the system's sovereignty (sovmajority.go) and
// reports whether the system lost its holder. Kept for callers that still phrase
// it as dropping the claim.
func DropSovIfLastStarbase(ctx context.Context, db *sql.DB, solarSystemID, allianceID int64) (bool, error) {
	ch, err := RecomputeSystemSov(ctx, db, solarSystemID)
	if err != nil {
		return false, err
	}
	return ch.Changed && ch.NewAlliance == 0, nil
}

// ─── placement policy ──────────────────────────────────────────────────────
//
// One rule set for the client anchor path (posMgr.AnchorStructure) and /posplant,
// phrased as a decision the caller words.
// See docs/starbase-pos-investigation.md#27-anchoring-policy-and-majority-sovereignty.

// DefaultMaxAnchorSecurity is the control-tower ceiling: towers anchor in 0.7 and
// below. Expressed as the first refused security, so the test is sec >= this.
const DefaultMaxAnchorSecurity = 0.75

// StarbaseMaxAnchorSecurity is DefaultMaxAnchorSecurity unless the shard has
// set an override.
func StarbaseMaxAnchorSecurity(ctx context.Context, db *sql.DB) float64 {
	if v, ok := StarbaseSecurityLevelMaxOverride(ctx, db); ok {
		return v
	}
	return DefaultMaxAnchorSecurity
}

// Refusal reasons for StarbaseAnchorRefusal.
const (
	AnchorRefuseSecurity     = "security"     // system is too high-sec
	// AnchorRefuseNeedAlliance and AnchorRefuseSystemHeld are never produced (sov is
	// recomputed by majority, sovmajority.go); kept so callers switching on them compile.
	AnchorRefuseNeedAlliance = "needalliance"
	AnchorRefuseSystemHeld   = "systemheld"
)

// StarbaseAnchorRefusal is a placement rule saying no. Callers switch on
// Reason to phrase it; the Error() text is the fallback wording.
type StarbaseAnchorRefusal struct {
	Reason           string
	Security         float64
	MaxSecurity      float64
	HolderAllianceID int64
}

func (r *StarbaseAnchorRefusal) Error() string {
	switch r.Reason {
	case AnchorRefuseSecurity:
		return fmt.Sprintf("control towers cannot be anchored in %.1f-sec space (%.1f and below only)",
			r.Security, r.MaxSecurity-0.05)
	case AnchorRefuseNeedAlliance:
		return "0.0 control towers require an alliance (sovereignty)"
	case AnchorRefuseSystemHeld:
		return fmt.Sprintf("system is held by alliance %d", r.HolderAllianceID)
	case AnchorRefuseNoTower:
		return "this structure must be anchored near a control tower of your own"
	case AnchorRefuseTowerOffline:
		return "the control tower must be online before structures can be anchored to it"
	case AnchorRefuseTowerRange:
		return "no control tower of yours is close enough to anchor this structure to"
	}
	return "this structure cannot be anchored here"
}

// StarbaseAnchorDecision is what the rules allow, once they allow it.
type StarbaseAnchorDecision struct {
	Security   float64
	Nullsec    bool
	SovOwned   bool  // the system currently has a sov holder
	AllianceID int64 // alliance to record on the tower (0 in empire, or a corp with none)
}

// CheckStarbaseAnchorPolicy applies the control-tower placement rules to one
// system; it reads, never writes. Only the security ceiling refuses: anchoring
// claims no sovereignty, and an empire tower (sec > 0.0) records no alliance.
// See docs/starbase-pos-investigation.md#27-anchoring-policy-and-majority-sovereignty.
func CheckStarbaseAnchorPolicy(
	ctx context.Context, db *sql.DB, solarSystemID, corpID, allianceID int64,
) (StarbaseAnchorDecision, error) {
	sec, secOK, err := GetSystemSecurity(ctx, db, solarSystemID)
	if err != nil {
		return StarbaseAnchorDecision{}, err
	}
	maxSec := StarbaseMaxAnchorSecurity(ctx, db)
	if secOK && sec >= maxSec {
		return StarbaseAnchorDecision{}, &StarbaseAnchorRefusal{
			Reason: AnchorRefuseSecurity, Security: sec, MaxSecurity: maxSec,
		}
	}
	d := StarbaseAnchorDecision{Security: sec, Nullsec: !secOK || sec < 0.0}
	if !d.Nullsec {
		return d, nil
	}
	_, owned, err := GetSov(ctx, db, solarSystemID)
	if err != nil {
		return StarbaseAnchorDecision{}, err
	}
	d.SovOwned = owned
	d.AllianceID = allianceID
	return d, nil
}

// ─── data-driven anchoring rules ───────────────────────────────────────────
//
// The rules live in dgmTypeAttributes, read so the server agrees with the
// client's Show Info and the four tower sizes differ without constants here.

// DefaultMoonAnchorDistance is the fallback for a tower type carrying no
// moonAnchorDistance (attribute 711). Every shipped tower carries 100 km; the
// fallback keeps a hand-added tower type anchorable.
const DefaultMoonAnchorDistance = 250000.0

// MoonAnchorDistance is how far from a moon's surface typeID may anchor. The
// client's own 30 km ship-distance check never runs, so this is the only gate.
// See docs/starbase-pos-investigation.md#4-client-side-pitfalls--the-be-mindful-list.
func MoonAnchorDistance(ctx context.Context, db *sql.DB, typeID int64) float64 {
	if v, ok, err := GetTypeAttributeFloat(ctx, db, typeID, "moonAnchorDistance"); err == nil && ok && v > 0 {
		return v
	}
	return DefaultMoonAnchorDistance
}

// MaxStructureDistance is how far from its control tower a slave structure may
// anchor: attribute 650 on the tower (50 km large, 35 km medium, 25 km small).
func MaxStructureDistance(ctx context.Context, db *sql.DB, towerTypeID int64) float64 {
	if v, ok, err := GetTypeAttributeFloat(ctx, db, towerTypeID, "maxStructureDistance"); err == nil && ok && v > 0 {
		return v
	}
	return 50000.0
}

// settingStarbaseSecMaxOverride moves every anchoring security ceiling at once,
// for a shard that wants moon mining in highsec. Unset or negative means the
// shipped attribute applies.
const settingStarbaseSecMaxOverride = "starbase.securityLevelMaxOverride"

// StarbaseSecurityLevelMaxOverride returns the configured ceiling.
// ok=false when nothing is set, which is the normal case.
func StarbaseSecurityLevelMaxOverride(ctx context.Context, db *sql.DB) (float64, bool) {
	v, present, err := GetServerSetting(ctx, db, settingStarbaseSecMaxOverride)
	if err != nil || !present {
		return 0, false
	}
	f, perr := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if perr != nil || f < 0 {
		return 0, false
	}
	return f, true
}

// SetStarbaseSecurityLevelMaxOverride writes the ceiling; a negative value
// clears it back to "use the shipped attribute".
func SetStarbaseSecurityLevelMaxOverride(ctx context.Context, db *sql.DB, sec float64) error {
	return SetServerSetting(ctx, db, settingStarbaseSecMaxOverride,
		strconv.FormatFloat(sec, 'f', -1, 64))
}

// AnchoringSecurityLevelMax is the highest truesec at which typeID may be anchored:
// attribute 1032, present on slave structures (0.4 on Moon Harvesting Array, 0.3
// on II/III) and absent on control towers, which use DefaultMaxAnchorSecurity.
// The server setting wins when set. ok=false means the type carries no ceiling.
func AnchoringSecurityLevelMax(ctx context.Context, db *sql.DB, typeID int64) (float64, bool) {
	if v, ok := StarbaseSecurityLevelMaxOverride(ctx, db); ok {
		return v, true
	}
	v, ok, err := GetTypeAttributeFloat(ctx, db, typeID, "anchoringSecurityLevelMax")
	if err != nil || !ok {
		return 0, false
	}
	return v, true
}

// AnchoringRequiresSovereignty reports attribute 1033. It is 0 on every shipped
// structure; a custom type may set it.
func AnchoringRequiresSovereignty(ctx context.Context, db *sql.DB, typeID int64) bool {
	v, ok, err := GetTypeAttributeFloat(ctx, db, typeID, "anchoringRequiresSovereignty")
	return err == nil && ok && v > 0
}

// GetControlTowerFuelRequirements returns the full invControlTowerResources
// table. The client (moonmining.py ShowFuel, infosvc "Fuel Requirements" tab)
// filters by controlTowerTypeID itself, so one unfiltered fetch serves both.
func GetControlTowerFuelRequirements(ctx context.Context, db *sql.DB) ([]ControlTowerResource, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT controlTowerTypeID, resourceTypeID,
		       COALESCE(purpose, 0), COALESCE(quantity, 0),
		       minSecurityLevel, factionID
		  FROM invControlTowerResources
		 ORDER BY controlTowerTypeID, purpose, resourceTypeID`)
	if err != nil {
		return nil, fmt.Errorf("GetControlTowerFuelRequirements: %w", err)
	}
	defer rows.Close()
	var out []ControlTowerResource
	for rows.Next() {
		var r ControlTowerResource
		if err := rows.Scan(
			&r.ControlTowerTypeID, &r.ResourceTypeID, &r.Purpose, &r.Quantity,
			&r.MinSecurityLevel, &r.FactionID,
		); err != nil {
			return nil, fmt.Errorf("GetControlTowerFuelRequirements scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// starbaseSelect is the SELECT body every starbase query shares: entity gives
// position and itemName, invTypes/invGroups the group and category a destiny
// SlimItem needs at load time.
const starbaseSelect = `
SELECT s.entityID, s.solarSystemID, s.moonID,
       e.typeID, t.groupID, COALESCE(g.categoryID, 0),
       s.ownerCorpID, s.ownerAllianceID, s.state,
       e.x, e.y, e.z, COALESCE(t.radius, 1500), COALESCE(e.itemName, ''),
       s.anchoredAt, s.onlinedAt, 0 AS controlTowerID, 0 AS provisionTypeID,
       s.unanchorAt, s.reinforceUntil
  FROM starbase s
  JOIN entity   e ON e.itemID = s.entityID
  JOIN invTypes t ON t.typeID = e.typeID
  LEFT JOIN invGroups g ON g.groupID = t.groupID`

// rowScanner abstracts *sql.Row vs *sql.Rows for scanStarbase.
type rowScanner interface {
	Scan(dest ...interface{}) error
}

func scanStarbase(r rowScanner) (StarbaseRecord, error) {
	var rec StarbaseRecord
	if err := r.Scan(
		&rec.EntityID, &rec.SolarSystemID, &rec.MoonID,
		&rec.TypeID, &rec.GroupID, &rec.CategoryID,
		&rec.OwnerCorpID, &rec.OwnerAllianceID, &rec.State,
		&rec.X, &rec.Y, &rec.Z, &rec.Radius, &rec.ItemName,
		&rec.AnchoredAt, &rec.OnlinedAt, &rec.ControlTowerID, &rec.ProvisionTypeID,
		&rec.UnanchorAt, &rec.ReinforceUntil,
	); err != nil {
		return StarbaseRecord{}, err
	}
	return rec, nil
}

// ─── fuel ──────────────────────────────────────────────────────────────────

// StarbaseFuelRow is what the fuel sweep needs per online tower: enough to
// resolve the cycle length, the charter rules, and the sov discount without
// re-reading the full record.
type StarbaseFuelRow struct {
	EntityID        int64
	TypeID          int64
	SolarSystemID   int64
	OwnerCorpID     int64
	OwnerAllianceID int64
	OnlinedAt       uint64 // Win32 FILETIME the tower became online
	FuelTick        uint64 // Win32 FILETIME of the last completed fuel cycle (0 = never)
}

// ListOnlineStarbaseFuel returns the fuel bookkeeping row for every online tower
// across all systems: the sweep burns fuel in unloaded systems too.
func ListOnlineStarbaseFuel(ctx context.Context, db *sql.DB) ([]StarbaseFuelRow, error) {
	if err := ensureStarbaseFuelColumn(ctx, db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT s.entityID, e.typeID, s.solarSystemID, s.ownerCorpID,
		       s.ownerAllianceID, s.onlinedAt, s.fuelTick
		  FROM starbase s
		  JOIN entity e ON e.itemID = s.entityID
		 WHERE s.state = ?
		 ORDER BY s.entityID`, StarbaseStateOnline)
	if err != nil {
		return nil, fmt.Errorf("ListOnlineStarbaseFuel: %w", err)
	}
	defer rows.Close()
	var out []StarbaseFuelRow
	for rows.Next() {
		var r StarbaseFuelRow
		if err := rows.Scan(&r.EntityID, &r.TypeID, &r.SolarSystemID,
			&r.OwnerCorpID, &r.OwnerAllianceID, &r.OnlinedAt, &r.FuelTick); err != nil {
			return nil, fmt.Errorf("ListOnlineStarbaseFuel scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetStarbaseFuelTick stores the FILETIME of the last completed fuel cycle.
func SetStarbaseFuelTick(ctx context.Context, db *sql.DB, entityID int64, tick uint64) error {
	if err := ensureStarbaseFuelColumn(ctx, db); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE starbase SET fuelTick = ? WHERE entityID = ?`, tick, entityID); err != nil {
		return fmt.Errorf("SetStarbaseFuelTick: %w", err)
	}
	return nil
}

// GetControlTowerOnlineNeeds returns the purpose-1 (Online) resource rows for one
// tower type: the per-cycle bill before charter filtering and the sov discount.
func GetControlTowerOnlineNeeds(ctx context.Context, db *sql.DB, towerTypeID int64) ([]ControlTowerResource, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT controlTowerTypeID, resourceTypeID,
		       COALESCE(purpose, 0), COALESCE(quantity, 0),
		       minSecurityLevel, factionID
		  FROM invControlTowerResources
		 WHERE controlTowerTypeID = ? AND purpose = 1
		 ORDER BY resourceTypeID`, towerTypeID)
	if err != nil {
		return nil, fmt.Errorf("GetControlTowerOnlineNeeds: %w", err)
	}
	defer rows.Close()
	var out []ControlTowerResource
	for rows.Next() {
		var r ControlTowerResource
		if err := rows.Scan(
			&r.ControlTowerTypeID, &r.ResourceTypeID, &r.Purpose, &r.Quantity,
			&r.MinSecurityLevel, &r.FactionID,
		); err != nil {
			return nil, fmt.Errorf("GetControlTowerOnlineNeeds scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetSystemFactionAndSecurity returns a solar system's owning NPC faction (0 when
// none) and security status, for the charter rule: faction-charter fuel rows apply
// only in that faction's space at or above the row's minSecurityLevel.
func GetSystemFactionAndSecurity(ctx context.Context, db *sql.DB, systemID int64) (factionID int64, security float64, ok bool, err error) {
	var f sql.NullInt64
	err = db.QueryRowContext(ctx,
		`SELECT factionID, COALESCE(security, 0) FROM mapSolarSystems WHERE solarSystemID = ?`,
		systemID).Scan(&f, &security)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, fmt.Errorf("GetSystemFactionAndSecurity: %w", err)
	}
	if f.Valid {
		factionID = f.Int64
	}
	return factionID, security, true, nil
}

// FuelNeed is one resolved per-cycle fuel demand (after charter filtering and
// the sov discount).
type FuelNeed struct {
	TypeID   int64
	Quantity int64
}

// StarbaseFuelCycleMs returns the tower type's fuel cycle length in ms
// (posControlTowerPeriod, attributeID 722; 1 h on the shipped towers). The
// dgmAttributeTypes default of 10 s is not used; the fallback is one hour.
func StarbaseFuelCycleMs(ctx context.Context, db *sql.DB, typeID int64) float64 {
	const fallback = 3_600_000
	if db == nil {
		return fallback
	}
	if v, ok, err := GetTypeAttributeFloat(ctx, db, typeID, "posControlTowerPeriod"); err == nil && ok && v > 0 {
		return v
	}
	return fallback
}

// StarbaseFuelNeeds resolves one tower's per-cycle fuel bill: purpose-1 rows for
// the type, charter rows filtered by system faction and security, Liquid Ozone and
// Heavy Water scaled by the grid in use, and the 25% sovereignty discount.
// See docs/starbase-pos-investigation.md#12-fuel-phase-4-first-slice.
func StarbaseFuelNeeds(ctx context.Context, db *sql.DB, r StarbaseFuelRow) ([]FuelNeed, error) {
	return starbaseFuelNeeds(ctx, db, r, false)
}

// StarbaseFuelNeedsFullGrid is StarbaseFuelNeeds with Liquid Ozone and Heavy Water
// billed as though the tower's grid were fully in use: what /fuel loads, so a tower
// that gains structures after fuelling is not starved of the two grid fuels.
func StarbaseFuelNeedsFullGrid(ctx context.Context, db *sql.DB, r StarbaseFuelRow) ([]FuelNeed, error) {
	return starbaseFuelNeeds(ctx, db, r, true)
}

// starbaseFuelNeeds is the per-cycle bill: every purpose-1 row flat, and the
// purpose-2/3 rows (Liquid Ozone / Heavy Water) scaled by the share of the tower's
// power grid / CPU its structures draw, the factor the client's Fuel tab applies
// (starbasegrid.go).
func starbaseFuelNeeds(ctx context.Context, db *sql.DB, r StarbaseFuelRow, fullGrid bool) ([]FuelNeed, error) {
	rows, err := getControlTowerBill(ctx, db, r.TypeID, TowerPurposeOnline, TowerPurposePower, TowerPurposeCPU)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	// No purpose-1 rows means a tower type with no bill at all (GM towers);
	// the grid fuels alone do not make one.
	hasOnline := false
	for _, row := range rows {
		hasOnline = hasOnline || row.Purpose == TowerPurposeOnline
	}
	if !hasOnline {
		return nil, nil
	}
	grid := StarbaseGrid{PowerOutput: 1, CPUOutput: 1, PowerUsed: 1, CPUUsed: 1}
	if !fullGrid {
		if grid, err = GetStarbaseGrid(ctx, db, r.EntityID, r.TypeID); err != nil {
			return nil, err
		}
	}
	factionID, sec, _, err := GetSystemFactionAndSecurity(ctx, db, r.SolarSystemID)
	if err != nil {
		return nil, err
	}
	discount := false
	if r.OwnerAllianceID != 0 {
		if sov, owned, err := GetSov(ctx, db, r.SolarSystemID); err == nil && owned && sov.AllianceID == r.OwnerAllianceID {
			discount = true
		}
	}
	var needs []FuelNeed
	for _, row := range rows {
		if row.FactionID.Valid && row.FactionID.Int64 != 0 {
			// Faction charter: consumed only in that faction's space at or
			// above the row's minimum security.
			minSec := 0.0
			if row.MinSecurityLevel.Valid {
				minSec = row.MinSecurityLevel.Float64
			}
			if factionID != row.FactionID.Int64 || sec < minSec {
				continue
			}
		}
		if q := fuelBillQuantity(row.Quantity, row.Purpose, discount, grid); q > 0 {
			needs = append(needs, FuelNeed{TypeID: row.ResourceTypeID, Quantity: q})
		}
	}
	return needs, nil
}

// GrantFuelToStarbaseBay creates qty units of typeID in a structure's bay
// (locationID = structure, flag 0), merging into an existing same-type stack,
// owned by ownerID. The /fuel admin path; no capacity check, so a GM fill may
// overflow.
func GrantFuelToStarbaseBay(ctx context.Context, db *sql.DB, structureID, typeID, qty, ownerID int64) error {
	if qty <= 0 {
		return nil
	}
	var typeName string
	err := db.QueryRowContext(ctx,
		`SELECT typeName FROM invTypes WHERE typeID = ?`, typeID).Scan(&typeName)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("GrantFuelToStarbaseBay: typeID %d not found in invTypes", typeID)
	}
	if err != nil {
		return fmt.Errorf("GrantFuelToStarbaseBay: %w", err)
	}
	res, err := db.ExecContext(ctx, `
		UPDATE entity SET quantity = quantity + ?
		 WHERE locationID = ? AND flag = 0 AND typeID = ?
		 ORDER BY itemID LIMIT 1`, qty, structureID, typeID)
	if err != nil {
		return fmt.Errorf("GrantFuelToStarbaseBay merge: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO entity
		  (itemName, typeID, ownerID, locationID, flag, contraband,
		   singleton, quantity, x, y, z, customInfo)
		VALUES (?, ?, ?, ?, 0, 0, 0, ?, 0, 0, 0, NULL)`,
		typeName, typeID, ownerID, structureID, qty); err != nil {
		return fmt.Errorf("GrantFuelToStarbaseBay insert: %w", err)
	}
	return nil
}

// ConsumeFromStarbaseBay deducts every need from the structure's bay (locationID =
// structure, flag 0) in one transaction. All-or-nothing: when any resource is
// short nothing is consumed and the first missing typeID is returned; the caller
// offlines the tower with the fuel still in the bay.
func ConsumeFromStarbaseBay(ctx context.Context, db *sql.DB, structureID int64, needs []FuelNeed) (missingTypeID int64, ok bool, err error) {
	if len(needs) == 0 {
		return 0, true, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("ConsumeFromStarbaseBay begin: %w", err)
	}
	defer tx.Rollback()

	type stack struct {
		itemID int64
		qty    int64
	}
	for _, need := range needs {
		if need.Quantity <= 0 {
			continue
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT itemID, quantity FROM entity
			 WHERE locationID = ? AND flag = 0 AND typeID = ?
			 ORDER BY itemID FOR UPDATE`, structureID, need.TypeID)
		if err != nil {
			return 0, false, fmt.Errorf("ConsumeFromStarbaseBay select: %w", err)
		}
		var stacks []stack
		var have int64
		for rows.Next() {
			var s stack
			if err := rows.Scan(&s.itemID, &s.qty); err != nil {
				rows.Close()
				return 0, false, fmt.Errorf("ConsumeFromStarbaseBay scan: %w", err)
			}
			stacks = append(stacks, s)
			have += s.qty
		}
		if err := rows.Close(); err != nil {
			return 0, false, err
		}
		if have < need.Quantity {
			return need.TypeID, false, nil // rollback via defer, nothing consumed
		}
		remaining := need.Quantity
		for _, s := range stacks {
			if remaining <= 0 {
				break
			}
			take := remaining
			if take >= s.qty {
				take = s.qty
				if _, err := tx.ExecContext(ctx,
					`DELETE FROM entity WHERE itemID = ?`, s.itemID); err != nil {
					return 0, false, fmt.Errorf("ConsumeFromStarbaseBay delete: %w", err)
				}
				if _, err := tx.ExecContext(ctx,
					`DELETE FROM entity_attributes WHERE itemID = ?`, s.itemID); err != nil {
					return 0, false, fmt.Errorf("ConsumeFromStarbaseBay attrs: %w", err)
				}
			} else {
				if _, err := tx.ExecContext(ctx,
					`UPDATE entity SET quantity = quantity - ? WHERE itemID = ?`,
					take, s.itemID); err != nil {
					return 0, false, fmt.Errorf("ConsumeFromStarbaseBay decrement: %w", err)
				}
			}
			remaining -= take
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("ConsumeFromStarbaseBay commit: %w", err)
	}
	return 0, true, nil
}
