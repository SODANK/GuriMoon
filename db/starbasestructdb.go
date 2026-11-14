// Package db: POS slave structures (harvesters, silos, reactors, arrays,
// batteries). A slave anchors to a control tower and lives in its own overlay
// table, not `starbase`, but shares StarbaseRecord with the tower so one ball
// builder serves both. See docs/db-9-notes.md#pos-slave-overlay.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sync"
)

// GroupControlTower is the one anchorable structure group that anchors to a
// moon instead of a tower.
const GroupControlTower = 365

var (
	starbaseStructSchemaOnce sync.Once
	starbaseStructSchemaErr  error
)

func ensureStarbaseStructureTable(ctx context.Context, db *sql.DB) error {
	starbaseStructSchemaOnce.Do(func() {
		_, starbaseStructSchemaErr = db.ExecContext(ctx, `
			CREATE TABLE IF NOT EXISTS starbaseStructures (
			  entityID        INT UNSIGNED NOT NULL,
			  solarSystemID   INT UNSIGNED NOT NULL,
			  towerID         INT UNSIGNED NOT NULL,
			  ownerCorpID     INT UNSIGNED NOT NULL,
			  ownerAllianceID INT UNSIGNED NOT NULL DEFAULT 0,
			  state           VARCHAR(16)  NOT NULL DEFAULT 'anchored',
			  anchoredAt      BIGINT(20)   NOT NULL DEFAULT 0,
			  onlinedAt       BIGINT(20)   NOT NULL DEFAULT 0,
			  provisionTypeID INT UNSIGNED NOT NULL DEFAULT 0,
			  harvestTick     BIGINT(20)   NOT NULL DEFAULT 0,
			  PRIMARY KEY (entityID),
			  KEY idx_tower  (towerID),
			  KEY idx_system (solarSystemID)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
		if starbaseStructSchemaErr != nil {
			return
		}
		// Columns added after the table first shipped.
		for _, col := range []struct{ name, ddl string }{
			{"provisionTypeID", "INT UNSIGNED NOT NULL DEFAULT 0"},
			{"harvestTick", "BIGINT(20) NOT NULL DEFAULT 0"},
			{"unanchorAt", "BIGINT(20) NOT NULL DEFAULT 0"},
		} {
			if err := addStructureColumn(ctx, db, col.name, col.ddl); err != nil {
				starbaseStructSchemaErr = err
				return
			}
		}
		starbaseStructSchemaErr = ensureStructureLinkTable(ctx, db)
	})
	return starbaseStructSchemaErr
}

// addStructureColumn is addStarbaseColumn for starbaseStructures. It runs only
// inside the sync.Once above and must not call back into it.
func addStructureColumn(ctx context.Context, db *sql.DB, column, ddl string) error {
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.columns
		 WHERE table_schema = DATABASE() AND table_name = 'starbaseStructures'
		   AND column_name = ?`, column).Scan(&n); err != nil {
		return fmt.Errorf("addStructureColumn(%s) probe: %w", column, err)
	}
	if n > 0 {
		return nil
	}
	if _, err := db.ExecContext(ctx,
		fmt.Sprintf(`ALTER TABLE starbaseStructures ADD COLUMN %s %s`, column, ddl)); err != nil {
		return fmt.Errorf("addStructureColumn(%s) alter: %w", column, err)
	}
	return nil
}

// ensureStructureLinkTable creates the harvester/silo/reactor wiring table, one
// row per (tower, material, source). The client's model is
// structureConnections[(typeID, sourceItemID)] = destItemID: a source feeds one
// destination per material.
func ensureStructureLinkTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS starbaseStructureLinks (
		  towerID  INT UNSIGNED NOT NULL,
		  typeID   INT UNSIGNED NOT NULL,
		  sourceID INT UNSIGNED NOT NULL,
		  destID   INT UNSIGNED NOT NULL,
		  PRIMARY KEY (typeID, sourceID),
		  KEY idx_tower (towerID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("ensureStructureLinkTable: %w", err)
	}
	return nil
}

// starbaseStructSelect mirrors starbaseSelect column for column, so both feed
// scanStarbase. moonID is 0 for a slave; the tower goes in the last column.
const starbaseStructSelect = `
SELECT s.entityID, s.solarSystemID, 0 AS moonID,
       e.typeID, t.groupID, COALESCE(g.categoryID, 0),
       s.ownerCorpID, s.ownerAllianceID, s.state,
       e.x, e.y, e.z, COALESCE(t.radius, 1500), COALESCE(e.itemName, ''),
       s.anchoredAt, s.onlinedAt, s.towerID, s.provisionTypeID,
       s.unanchorAt, 0 AS reinforceUntil
  FROM starbaseStructures s
  JOIN entity   e ON e.itemID = s.entityID
  JOIN invTypes t ON t.typeID = e.typeID
  LEFT JOIN invGroups g ON g.groupID = t.groupID`

// IsAnchorableStructureType reports whether typeID carries
// anchorDropForStructures (effect 1022), the test pwn.Anchor makes before it
// calls posMgr.AnchorStructure, so the server needs no group list of its own.
func IsAnchorableStructureType(ctx context.Context, db *sql.DB, typeID int64) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM dgmTypeEffects
		 WHERE typeID = ? AND effectID = 1022`, typeID).Scan(&n); err != nil {
		return false, fmt.Errorf("IsAnchorableStructureType(%d): %w", typeID, err)
	}
	return n > 0, nil
}

// AnchorSlaveStructure promotes a launched slave structure into the overlay,
// bound to towerID. The entity row must already exist (launched with ship.Drop).
// It is the slave counterpart of AnchorLaunchedStarbase.
func AnchorSlaveStructure(
	ctx context.Context, db *sql.DB,
	entityID, towerID, solarSystemID, ownerCorpID, ownerAllianceID int64,
	x, y, z float64,
) error {
	if err := ensureStarbaseStructureTable(ctx, db); err != nil {
		return fmt.Errorf("AnchorSlaveStructure ensureTable: %w", err)
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
		return fmt.Errorf("AnchorSlaveStructure entity %d: %w", entityID, err)
	}

	now := win32TimeNow()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("AnchorSlaveStructure begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		UPDATE entity SET locationID = ?, x = ?, y = ?, z = ? WHERE itemID = ?`,
		solarSystemID, x, y, z, entityID); err != nil {
		return fmt.Errorf("AnchorSlaveStructure entity update: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO starbaseStructures
		  (entityID, solarSystemID, towerID, ownerCorpID, ownerAllianceID,
		   state, anchoredAt, onlinedAt)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0)`,
		entityID, solarSystemID, towerID, ownerCorpID, ownerAllianceID,
		StarbaseStateAnchored, now); err != nil {
		if isDuplicateKeyErr(err) {
			return fmt.Errorf("structure %d is already anchored", entityID)
		}
		return fmt.Errorf("AnchorSlaveStructure insert: %w", err)
	}
	cid := int64(23)
	if catID.Valid && catID.Int64 != 0 {
		cid = catID.Int64
	}
	if _, err := tx.ExecContext(ctx,
		`REPLACE INTO eveNames (itemID, itemName, categoryID, groupID, typeID) VALUES (?, ?, ?, ?, ?)`,
		entityID, typeName, cid, groupID, typeID); err != nil {
		return fmt.Errorf("AnchorSlaveStructure eveNames: %w", err)
	}
	return tx.Commit()
}

// GetStarbaseStructure returns one slave record by entityID.
func GetStarbaseStructure(ctx context.Context, db *sql.DB, entityID int64) (StarbaseRecord, bool, error) {
	if err := ensureStarbaseStructureTable(ctx, db); err != nil {
		return StarbaseRecord{}, false, err
	}
	rec, err := scanStarbase(db.QueryRowContext(ctx,
		starbaseStructSelect+` WHERE s.entityID = ?`, entityID))
	if errors.Is(err, sql.ErrNoRows) {
		return StarbaseRecord{}, false, nil
	}
	if err != nil {
		return StarbaseRecord{}, false, err
	}
	return rec, true, nil
}

func listStructures(ctx context.Context, db *sql.DB, where string, args ...interface{}) ([]StarbaseRecord, error) {
	if err := ensureStarbaseStructureTable(ctx, db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, starbaseStructSelect+where, args...)
	if err != nil {
		return nil, fmt.Errorf("listStructures: %w", err)
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

// ListStarbaseStructuresForSystem returns every anchored slave in a system.
func ListStarbaseStructuresForSystem(ctx context.Context, db *sql.DB, solarSystemID int64) ([]StarbaseRecord, error) {
	return listStructures(ctx, db, ` WHERE s.solarSystemID = ? ORDER BY s.entityID`, solarSystemID)
}

// ListStarbaseStructuresForTower returns every slave anchored to one tower.
func ListStarbaseStructuresForTower(ctx context.Context, db *sql.DB, towerID int64) ([]StarbaseRecord, error) {
	return listStructures(ctx, db, ` WHERE s.towerID = ? ORDER BY s.entityID`, towerID)
}

// ListStarbaseStructuresByState returns every slave in a state, shard-wide (the
// onlining sweep's counterpart for slaves).
func ListStarbaseStructuresByState(ctx context.Context, db *sql.DB, state string) ([]StarbaseRecord, error) {
	return listStructures(ctx, db, ` WHERE s.state = ? ORDER BY s.entityID`, state)
}

// OnlineStructure moves a slave anchored → onlining, stamping onlinedAt.
func OnlineStructure(ctx context.Context, db *sql.DB, entityID int64) (string, error) {
	if err := ensureStarbaseStructureTable(ctx, db); err != nil {
		return "", err
	}
	res, err := db.ExecContext(ctx, `
		UPDATE starbaseStructures SET state = ?, onlinedAt = ?
		 WHERE entityID = ? AND state = ?`,
		StarbaseStateOnlining, win32TimeNow(), entityID, StarbaseStateAnchored)
	if err != nil {
		return "", fmt.Errorf("OnlineStructure: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", fmt.Errorf("structure %d is not in the anchored state", entityID)
	}
	return StarbaseStateOnlining, nil
}

// CompleteStructureOnlining finishes an onlining slave. ok=false when the row
// has since been removed or moved on.
func CompleteStructureOnlining(ctx context.Context, db *sql.DB, entityID int64) (bool, error) {
	if err := ensureStarbaseStructureTable(ctx, db); err != nil {
		return false, err
	}
	res, err := db.ExecContext(ctx, `
		UPDATE starbaseStructures SET state = ?
		 WHERE entityID = ? AND state = ?`,
		StarbaseStateOnline, entityID, StarbaseStateOnlining)
	if err != nil {
		return false, fmt.Errorf("CompleteStructureOnlining: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// OfflineStructure moves online (or still-counting onlining) back to anchored.
func OfflineStructure(ctx context.Context, db *sql.DB, entityID int64) (string, error) {
	if err := ensureStarbaseStructureTable(ctx, db); err != nil {
		return "", err
	}
	res, err := db.ExecContext(ctx, `
		UPDATE starbaseStructures SET state = ?, onlinedAt = 0
		 WHERE entityID = ? AND state IN (?, ?)`,
		StarbaseStateAnchored, entityID, StarbaseStateOnline, StarbaseStateOnlining)
	if err != nil {
		return "", fmt.Errorf("OfflineStructure: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", fmt.Errorf("structure %d is not online", entityID)
	}
	return StarbaseStateAnchored, nil
}

// OfflineStructuresForTower takes every online or onlining slave of a tower back
// to anchored and returns the IDs moved. A slave draws power and CPU from its
// tower, so it goes down with it.
func OfflineStructuresForTower(ctx context.Context, db *sql.DB, towerID int64) ([]int64, error) {
	if err := ensureStarbaseStructureTable(ctx, db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT entityID FROM starbaseStructures
		 WHERE towerID = ? AND state IN (?, ?)`,
		towerID, StarbaseStateOnline, StarbaseStateOnlining)
	if err != nil {
		return nil, fmt.Errorf("OfflineStructuresForTower: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("OfflineStructuresForTower scan: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE starbaseStructures SET state = ?, onlinedAt = 0
		 WHERE towerID = ? AND state IN (?, ?)`,
		StarbaseStateAnchored, towerID, StarbaseStateOnline, StarbaseStateOnlining); err != nil {
		return nil, fmt.Errorf("OfflineStructuresForTower update: %w", err)
	}
	return ids, nil
}

// UnanchorStructure drops the overlay row, leaving the entity in space as a
// free ball. Returns the record as it was.
func UnanchorStructure(ctx context.Context, db *sql.DB, entityID int64) (StarbaseRecord, error) {
	rec, ok, err := GetStarbaseStructure(ctx, db, entityID)
	if err != nil {
		return rec, err
	}
	if !ok {
		return rec, fmt.Errorf("structure %d is not anchored", entityID)
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM starbaseStructures WHERE entityID = ?`, entityID); err != nil {
		return rec, fmt.Errorf("UnanchorStructure: %w", err)
	}
	return rec, nil
}

// RemoveStarbaseStructures deletes the overlay rows for every slave of a tower
// and returns their IDs. Called when the tower is unanchored or removed: a
// slave of a missing tower can never come online.
func RemoveStarbaseStructures(ctx context.Context, db *sql.DB, towerID int64) ([]int64, error) {
	recs, err := ListStarbaseStructuresForTower(ctx, db, towerID)
	if err != nil || len(recs) == 0 {
		return nil, err
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM starbaseStructures WHERE towerID = ?`, towerID); err != nil {
		return nil, fmt.Errorf("RemoveStarbaseStructures: %w", err)
	}
	ids := make([]int64, 0, len(recs))
	for _, r := range recs {
		ids = append(ids, r.EntityID)
	}
	return ids, nil
}

// Slave-specific anchor refusal reasons, beyond the shared placement ones.
const (
	AnchorRefuseNoTower      = "notower"      // no tower of yours in range
	AnchorRefuseTowerOffline = "toweroffline" // the tower is not online
	AnchorRefuseTowerRange   = "towerrange"   // outside maxStructureDistance
)

// SlaveAnchorTarget is the tower a slave will bind to, and how far away it is.
type SlaveAnchorTarget struct {
	Tower    StarbaseRecord
	Distance float64
	MaxRange float64
}

// FindAnchorTower picks the control tower a slave anchoring at (x,y,z) binds to,
// by the client's rules (playerOwned.py LocateControlTower): manageable by the
// caller, fully online, within the tower's maxStructureDistance (attribute 650).
// It searches the DB, so a tower off the caller's grid counts.
func FindAnchorTower(
	ctx context.Context, db *sql.DB, solarSystemID, corpID, allianceID int64, x, y, z float64,
) (SlaveAnchorTarget, error) {
	towers, err := ListStarbasesForSystem(ctx, db, solarSystemID)
	if err != nil {
		return SlaveAnchorTarget{}, err
	}
	var (
		best      SlaveAnchorTarget
		found     bool
		sawOwn    bool
		sawOnline bool
	)
	for _, t := range towers {
		if !StarbaseManagePermitted(t, corpID, allianceID) {
			continue
		}
		sawOwn = true
		if t.State != StarbaseStateOnline && t.State != StarbaseStateReinforced {
			continue
		}
		sawOnline = true
		d := math.Sqrt((x-t.X)*(x-t.X) + (y-t.Y)*(y-t.Y) + (z-t.Z)*(z-t.Z))
		maxRange := MaxStructureDistance(ctx, db, t.TypeID)
		if d > maxRange {
			continue
		}
		if !found || d < best.Distance {
			best = SlaveAnchorTarget{Tower: t, Distance: d, MaxRange: maxRange}
			found = true
		}
	}
	if found {
		return best, nil
	}
	// The three refusals are distinct to the pilot: no tower, tower not up, too far.
	switch {
	case !sawOwn:
		return SlaveAnchorTarget{}, &StarbaseAnchorRefusal{Reason: AnchorRefuseNoTower}
	case !sawOnline:
		return SlaveAnchorTarget{}, &StarbaseAnchorRefusal{Reason: AnchorRefuseTowerOffline}
	}
	return SlaveAnchorTarget{}, &StarbaseAnchorRefusal{Reason: AnchorRefuseTowerRange}
}

// ---- Standalone anchorables (Mobile Storage) ----
// Group 364 carries only the generic anchor pair (649/650); sql/dgmtypeeffects.sql
// also gives it 1022/1023 so the structure menu reaches the same path. It is a
// starbaseStructures row with towerID 0.
// See docs/starbase-pos-investigation.md section 16.

// EffectAnchorDrop / EffectAnchorLift are the generic anchor effects.
const (
	EffectAnchorDrop             = 649
	EffectAnchorLift             = 650
	EffectAnchorDropForStructure = 1022
)

// IsStandaloneAnchorable reports whether typeID is a category-23 structure or a
// deployable (IsDeployableGroup) carrying the generic anchorDrop (649). No
// tower-bound type carries 649.
func IsStandaloneAnchorable(ctx context.Context, db *sql.DB, typeID int64) (bool, error) {
	var drop int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM dgmTypeEffects e
		  JOIN invTypes t ON t.typeID = e.typeID
		  JOIN invGroups g ON g.groupID = t.groupID
		 WHERE e.typeID = ? AND e.effectID = ? AND (g.categoryID = 23 OR `+deployableGroupSQL+`)`,
		typeID, EffectAnchorDrop).Scan(&drop); err != nil {
		return false, fmt.Errorf("IsStandaloneAnchorable(%d): %w", typeID, err)
	}
	return drop > 0, nil
}

// AnchorStandaloneStructure anchors a launched standalone structure where it
// floats: an overlay row with towerID 0, owned by ownerCorpID.
func AnchorStandaloneStructure(ctx context.Context, db *sql.DB,
	entityID, solarSystemID, ownerCorpID, ownerAllianceID int64, x, y, z float64) error {
	return AnchorSlaveStructure(ctx, db, entityID, 0, solarSystemID, ownerCorpID, ownerAllianceID, x, y, z)
}

// DelayElapsed reports whether delayMs has run since a Win32 FILETIME start. A
// zero start never elapses (no transition recorded).
func DelayElapsed(start uint64, delayMs float64, now uint64) bool {
	if start == 0 {
		return false
	}
	return now >= start+uint64(delayMs*10000)
}

// BeginStructureUnanchor starts the unanchoring countdown of a standalone
// structure or a tower's slave. Only an anchored, offline row may begin it.
func BeginStructureUnanchor(ctx context.Context, db *sql.DB, entityID int64) (uint64, error) {
	if err := ensureStarbaseStructureTable(ctx, db); err != nil {
		return 0, err
	}
	now := win32TimeNow()
	res, err := db.ExecContext(ctx, `
		UPDATE starbaseStructures SET state = ?, unanchorAt = ?
		 WHERE entityID = ? AND state = ?`,
		StarbaseStateUnanchoring, now, entityID, StarbaseStateAnchored)
	if err != nil {
		return 0, fmt.Errorf("BeginStructureUnanchor: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, fmt.Errorf("it must be anchored and offline to unanchor")
	}
	return now, nil
}
