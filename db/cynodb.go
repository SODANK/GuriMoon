// Package db: cynosural fields.
//
// A field is a beacon lit by a Cynosural Field Generator (typeID 21096, effect 1729). The
// cyno_beacon table is its whole overlay, kept so a field survives a system unload and a restart.
// It has no entity row and no HP. See docs/jump-notes.md#cyno.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
)

// CynoBeacon is one lit cynosural field.
type CynoBeacon struct {
	ShipID        int64 // the ship that lit it; the anchor and the primary key
	CharID        int64
	ModuleID      int64
	ModuleTypeID  int64
	SolarSystemID int64
	CorpID        int64
	AllianceID    int64
	// GangID is the gang at light time, not read live: the gang registry is not persisted.
	GangID    int64
	X, Y, Z   float64
	LitAt     uint64 // Win32 FILETIME
	ExpiresAt uint64 // LitAt + duration(73), 600 s
}

var (
	cynoSchemaOnce sync.Once
	cynoSchemaErr  error
)

// EnsureCynoSchema creates the beacon table, once per process (every query path calls it).
func EnsureCynoSchema(ctx context.Context, db *sql.DB) error {
	cynoSchemaOnce.Do(func() {
		// Keyed on shipID: one field per ship, and the ball ID (cynoBeaconIDBase + shipID) stays
		// stable across a restart.
		if _, err := db.ExecContext(ctx, `
			CREATE TABLE IF NOT EXISTS cyno_beacon (
			  shipID        BIGINT(20)   NOT NULL,
			  charID        BIGINT(20)   NOT NULL,
			  moduleID      BIGINT(20)   NOT NULL,
			  moduleTypeID  INT UNSIGNED NOT NULL,
			  solarSystemID INT UNSIGNED NOT NULL,
			  corpID        INT UNSIGNED NOT NULL DEFAULT 0,
			  allianceID    INT UNSIGNED NOT NULL DEFAULT 0,
			  gangID        BIGINT(20)   NOT NULL DEFAULT 0,
			  x             DOUBLE       NOT NULL DEFAULT 0,
			  y             DOUBLE       NOT NULL DEFAULT 0,
			  z             DOUBLE       NOT NULL DEFAULT 0,
			  litAt         BIGINT(20)   NOT NULL DEFAULT 0,
			  expiresAt     BIGINT(20)   NOT NULL DEFAULT 0,
			  PRIMARY KEY (shipID),
			  KEY idx_system  (solarSystemID),
			  KEY idx_char    (charID),
			  KEY idx_expires (expiresAt)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`); err != nil {
			cynoSchemaErr = err
		}
	})
	return cynoSchemaErr
}

const cynoBeaconCols = `shipID, charID, moduleID, moduleTypeID, solarSystemID,
	corpID, allianceID, gangID, x, y, z, litAt, expiresAt`

func scanCynoBeacon(sc interface{ Scan(...any) error }) (CynoBeacon, error) {
	var b CynoBeacon
	err := sc.Scan(&b.ShipID, &b.CharID, &b.ModuleID, &b.ModuleTypeID, &b.SolarSystemID,
		&b.CorpID, &b.AllianceID, &b.GangID, &b.X, &b.Y, &b.Z, &b.LitAt, &b.ExpiresAt)
	return b, err
}

// LightCynoBeacon records a newly lit field. REPLACE overwrites a stale row that a collapse
// failed to clear.
func LightCynoBeacon(ctx context.Context, db *sql.DB, b CynoBeacon) error {
	if err := EnsureCynoSchema(ctx, db); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `
		REPLACE INTO cyno_beacon (`+cynoBeaconCols+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		b.ShipID, b.CharID, b.ModuleID, b.ModuleTypeID, b.SolarSystemID,
		b.CorpID, b.AllianceID, b.GangID, b.X, b.Y, b.Z, b.LitAt, b.ExpiresAt)
	if err != nil {
		return fmt.Errorf("LightCynoBeacon ship=%d: %w", b.ShipID, err)
	}
	return nil
}

// GetCynoBeaconByShip answers the field a ship is holding, if any.
func GetCynoBeaconByShip(ctx context.Context, db *sql.DB, shipID int64) (*CynoBeacon, error) {
	if err := EnsureCynoSchema(ctx, db); err != nil {
		return nil, err
	}
	b, err := scanCynoBeacon(db.QueryRowContext(ctx,
		`SELECT `+cynoBeaconCols+` FROM cyno_beacon WHERE shipID = ?`, shipID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetCynoBeaconByShip %d: %w", shipID, err)
	}
	return &b, nil
}

// GetCynoBeaconByChar answers the field a character holds. The disconnect path has a charID
// only.
func GetCynoBeaconByChar(ctx context.Context, db *sql.DB, charID int64) (*CynoBeacon, error) {
	if err := EnsureCynoSchema(ctx, db); err != nil {
		return nil, err
	}
	b, err := scanCynoBeacon(db.QueryRowContext(ctx,
		`SELECT `+cynoBeaconCols+` FROM cyno_beacon WHERE charID = ? LIMIT 1`, charID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetCynoBeaconByChar %d: %w", charID, err)
	}
	return &b, nil
}

// ListCynoBeaconsForSystem answers every field still burning in a system. Expired rows are
// filtered out so a reload does not rebuild a lapsed field.
func ListCynoBeaconsForSystem(ctx context.Context, db *sql.DB, sysID int64, now uint64) ([]CynoBeacon, error) {
	if err := EnsureCynoSchema(ctx, db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx,
		`SELECT `+cynoBeaconCols+` FROM cyno_beacon
		  WHERE solarSystemID = ? AND expiresAt > ?`, sysID, now)
	if err != nil {
		return nil, fmt.Errorf("ListCynoBeaconsForSystem %d: %w", sysID, err)
	}
	defer rows.Close()
	var out []CynoBeacon
	for rows.Next() {
		b, sErr := scanCynoBeacon(rows)
		if sErr != nil {
			return nil, fmt.Errorf("ListCynoBeaconsForSystem scan: %w", sErr)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ListLiveCynoBeacons answers every live field. It resolves a beacon ball ID when the system is
// unknown (cynoShipFromBallID).
func ListLiveCynoBeacons(ctx context.Context, db *sql.DB, now uint64) ([]CynoBeacon, error) {
	if err := EnsureCynoSchema(ctx, db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx,
		`SELECT `+cynoBeaconCols+` FROM cyno_beacon WHERE expiresAt > ?`, now)
	if err != nil {
		return nil, fmt.Errorf("ListLiveCynoBeacons: %w", err)
	}
	defer rows.Close()
	var out []CynoBeacon
	for rows.Next() {
		b, sErr := scanCynoBeacon(rows)
		if sErr != nil {
			return nil, fmt.Errorf("ListLiveCynoBeacons scan: %w", sErr)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ListExpiredCynoBeacons answers every field whose time is up. Drives the expiry sweep, the
// backstop for a lost one-shot.
func ListExpiredCynoBeacons(ctx context.Context, db *sql.DB, now uint64) ([]CynoBeacon, error) {
	if err := EnsureCynoSchema(ctx, db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx,
		`SELECT `+cynoBeaconCols+` FROM cyno_beacon WHERE expiresAt <= ?`, now)
	if err != nil {
		return nil, fmt.Errorf("ListExpiredCynoBeacons: %w", err)
	}
	defer rows.Close()
	var out []CynoBeacon
	for rows.Next() {
		b, sErr := scanCynoBeacon(rows)
		if sErr != nil {
			return nil, fmt.Errorf("ListExpiredCynoBeacons scan: %w", sErr)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// DeleteCynoBeacon drops one field's row. Idempotent: several collapse paths can fire for one
// event.
func DeleteCynoBeacon(ctx context.Context, db *sql.DB, shipID int64) error {
	if err := EnsureCynoSchema(ctx, db); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM cyno_beacon WHERE shipID = ?`, shipID); err != nil {
		return fmt.Errorf("DeleteCynoBeacon %d: %w", shipID, err)
	}
	return nil
}

// PurgeExpiredCynoBeacons clears every field already past its time. Run at boot.
func PurgeExpiredCynoBeacons(ctx context.Context, db *sql.DB, now uint64) (int64, error) {
	if err := EnsureCynoSchema(ctx, db); err != nil {
		return 0, err
	}
	res, err := db.ExecContext(ctx,
		`DELETE FROM cyno_beacon WHERE expiresAt <= ?`, now)
	if err != nil {
		return 0, fmt.Errorf("PurgeExpiredCynoBeacons: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// GetBeaconCountsBySystem answers {solarSystemID: liveFieldCount} for the star map's
// Cynosural Fields overlay (map.GetBeaconCount). Filters on expiresAt, not on a live join, so an
// unswept row is not counted.
func GetBeaconCountsBySystem(ctx context.Context, db *sql.DB, now uint64) (map[int64]int64, error) {
	if err := EnsureCynoSchema(ctx, db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT solarSystemID, COUNT(*) FROM cyno_beacon
		 WHERE expiresAt > ? GROUP BY solarSystemID`, now)
	if err != nil {
		return nil, fmt.Errorf("GetBeaconCountsBySystem: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]int64)
	for rows.Next() {
		var sysID, n int64
		if err := rows.Scan(&sysID, &n); err != nil {
			return nil, fmt.Errorf("GetBeaconCountsBySystem scan: %w", err)
		}
		out[sysID] = n
	}
	return out, rows.Err()
}
