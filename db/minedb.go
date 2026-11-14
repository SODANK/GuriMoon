package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Mines laid in space (cmd/gurimoon/mines.go). The mine is an entity row in
// the solar system, as a loot container is; spaceMines holds what the entity
// row cannot: the owner's corp and alliance at laying time and the clocks.

// GroupMine is invGroups 92, category 8 (Charge).
const GroupMine = 92

// mineLauncherGroups are the missile launcher groups that take mines as a
// charge: rocket, siege, standard, heavy, assault and citadel launchers.
var mineLauncherGroups = []int64{507, 508, 509, 510, 511, 524}

// attrChargeGroup5 is the free charge-group slot on every launcher group above.
const attrChargeGroup5 = 610

// ErrMineGone is returned when a mine is no longer in space (scooped,
// detonated or expired by another path).
var ErrMineGone = errors.New("the mine is no longer in space")

// EnsureMineTables creates the spaceMines table if absent.
func EnsureMineTables(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS spaceMines (
			mineID          BIGINT NOT NULL PRIMARY KEY,
			solarSystemID   BIGINT NOT NULL,
			ownerCharID     BIGINT NOT NULL,
			ownerCorpID     BIGINT NOT NULL DEFAULT 0,
			ownerAllianceID BIGINT NOT NULL DEFAULT 0,
			laidAt          BIGINT NOT NULL,
			armAt           BIGINT NOT NULL,
			expiresAt       BIGINT NOT NULL,
			KEY idx_sm_system (solarSystemID),
			KEY idx_sm_owner (ownerCharID, solarSystemID),
			KEY idx_sm_expires (expiresAt)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8`)
	if err != nil {
		return fmt.Errorf("EnsureMineTables: %w", err)
	}
	return nil
}

// EnsureMineLauncherCharges lets every missile launcher load mines by adding
// group 92 in chargeGroup5. sql/dgmtypeattributes.sql carries the same rows for
// a fresh install; this adds them to an older database. Run before the login
// cachables are primed, so config.BulkData.dgmtypeattribs carries the rows the
// client's ammo menu reads. Returns the number of rows added.
func EnsureMineLauncherCharges(ctx context.Context, db *sql.DB) (int64, error) {
	args := make([]any, 0, len(mineLauncherGroups)+2)
	args = append(args, attrChargeGroup5, GroupMine)
	for _, g := range mineLauncherGroups {
		args = append(args, g)
	}
	res, err := db.ExecContext(ctx, `
		INSERT IGNORE INTO dgmTypeAttributes (typeID, attributeID, valueInt, valueFloat)
		SELECT typeID, ?, ?, NULL FROM invTypes
		 WHERE groupID IN (?, ?, ?, ?, ?, ?)`, args...)
	if err != nil {
		return 0, fmt.Errorf("EnsureMineLauncherCharges: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// SpaceMine is one mine in space.
type SpaceMine struct {
	MineID          int64
	TypeID          int64
	SolarSystemID   int64
	OwnerCharID     int64
	OwnerCorpID     int64
	OwnerAllianceID int64
	X, Y, Z         float64
	LaidAt          int64 // unix seconds
	ArmAt           int64
	ExpiresAt       int64
}

// CreateMine inserts the mine's entity row in the solar system and its
// spaceMines row in one transaction. Returns the new itemID.
func CreateMine(ctx context.Context, db *sql.DB, m SpaceMine, name string) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("CreateMine begin: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		INSERT INTO entity
		  (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
		VALUES (?, ?, ?, ?, 0, 0, 1, 1, ?, ?, ?, NULL)`,
		name, m.TypeID, m.OwnerCharID, m.SolarSystemID, m.X, m.Y, m.Z)
	if err != nil {
		return 0, fmt.Errorf("CreateMine insert: %w", err)
	}
	itemID, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("CreateMine LastInsertId: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO spaceMines
		  (mineID, solarSystemID, ownerCharID, ownerCorpID, ownerAllianceID, laidAt, armAt, expiresAt)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		itemID, m.SolarSystemID, m.OwnerCharID, m.OwnerCorpID, m.OwnerAllianceID,
		m.LaidAt, m.ArmAt, m.ExpiresAt); err != nil {
		return 0, fmt.Errorf("CreateMine spaceMines: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("CreateMine commit: %w", err)
	}
	return itemID, nil
}

// ListMinesForSystem returns the unexpired mines in a solar system.
func ListMinesForSystem(ctx context.Context, db *sql.DB, sysID int64) ([]SpaceMine, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT m.mineID, e.typeID, m.solarSystemID, m.ownerCharID, m.ownerCorpID, m.ownerAllianceID,
		       e.x, e.y, e.z, m.laidAt, m.armAt, m.expiresAt
		  FROM spaceMines m
		  JOIN entity e ON e.itemID = m.mineID
		 WHERE m.solarSystemID = ? AND m.expiresAt > ?`, sysID, time.Now().Unix())
	if err != nil {
		return nil, fmt.Errorf("ListMinesForSystem: %w", err)
	}
	defer rows.Close()
	var out []SpaceMine
	for rows.Next() {
		var m SpaceMine
		if err := rows.Scan(&m.MineID, &m.TypeID, &m.SolarSystemID, &m.OwnerCharID, &m.OwnerCorpID,
			&m.OwnerAllianceID, &m.X, &m.Y, &m.Z, &m.LaidAt, &m.ArmAt, &m.ExpiresAt); err != nil {
			return nil, fmt.Errorf("ListMinesForSystem scan: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeleteMine removes a mine's spaceMines and entity rows.
func DeleteMine(ctx context.Context, db *sql.DB, mineID int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("DeleteMine begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM spaceMines WHERE mineID = ?`, mineID); err != nil {
		return fmt.Errorf("DeleteMine spaceMines: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM entity WHERE itemID = ? AND flag = 0`, mineID); err != nil {
		return fmt.Errorf("DeleteMine entity: %w", err)
	}
	return tx.Commit()
}

// ScoopMineToCargo moves a mine floating in solarSystemID into shipID's cargo
// hold (flag 5) and drops its spaceMines row, in one transaction. The WHERE
// re-checks that the mine is still in space, so a scoop racing a detonation
// loses cleanly with ErrMineGone.
func ScoopMineToCargo(ctx context.Context, db *sql.DB, mineID, solarSystemID, shipID int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ScoopMineToCargo begin: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `
		UPDATE entity SET locationID = ?, flag = 5, singleton = 0, x = 0, y = 0, z = 0
		 WHERE itemID = ? AND locationID = ?`, shipID, mineID, solarSystemID)
	if err != nil {
		return fmt.Errorf("ScoopMineToCargo move: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrMineGone
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM spaceMines WHERE mineID = ?`, mineID); err != nil {
		return fmt.Errorf("ScoopMineToCargo spaceMines: %w", err)
	}
	return tx.Commit()
}

// AllianceRelationshipToward returns the relationship allianceID declares
// toward the first of toIDs that has one (AllianceRelationshipNAP and the
// rest), or 0 when none does.
func AllianceRelationshipToward(ctx context.Context, db *sql.DB, allianceID int64, toIDs ...int64) int64 {
	if allianceID <= 0 {
		return 0
	}
	for _, to := range toIDs {
		if to <= 0 {
			continue
		}
		var rel int64
		if err := db.QueryRowContext(ctx,
			`SELECT relationship FROM allianceRelationships WHERE allianceID = ? AND toID = ?`,
			allianceID, to).Scan(&rel); err == nil {
			return rel
		}
	}
	return 0
}

// ExpiredMine names a mine past its expiry.
type ExpiredMine struct {
	MineID        int64
	SolarSystemID int64
}

// ListExpiredMines returns the mines whose expiry is at or before nowUnix.
func ListExpiredMines(ctx context.Context, db *sql.DB, nowUnix int64) ([]ExpiredMine, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT mineID, solarSystemID FROM spaceMines WHERE expiresAt <= ?`, nowUnix)
	if err != nil {
		return nil, fmt.Errorf("ListExpiredMines: %w", err)
	}
	defer rows.Close()
	var out []ExpiredMine
	for rows.Next() {
		var m ExpiredMine
		if err := rows.Scan(&m.MineID, &m.SolarSystemID); err != nil {
			return nil, fmt.Errorf("ListExpiredMines scan: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
