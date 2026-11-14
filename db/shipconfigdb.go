package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Ship configuration: the Configure Ship window (ui/inflight/shipconfig.py), drawn for a ship
// with a Ship Maintenance Bay. Its one setting, allowGangSMBUsage, is read back from
// GetShipConfiguration on every open, so it must persist. Nothing consumes the flag yet:
// StoreVessel / BoardStoredShip serve only the POS Ship Maintenance Array (smaship.go).

// EnsureShipConfigTable creates shipConfig.
func EnsureShipConfigTable(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS shipConfig (
			shipID            BIGINT NOT NULL PRIMARY KEY,
			allowGangSMBUsage TINYINT NOT NULL DEFAULT 0
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`); err != nil {
		return fmt.Errorf("EnsureShipConfigTable: %w", err)
	}
	return nil
}

// GetShipAllowGangSMB reads a ship's allowGangSMBUsage (false when unset).
func GetShipAllowGangSMB(ctx context.Context, db *sql.DB, shipID int64) (bool, error) {
	var v int
	err := db.QueryRowContext(ctx,
		`SELECT allowGangSMBUsage FROM shipConfig WHERE shipID = ?`, shipID).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("GetShipAllowGangSMB: %w", err)
	}
	return v != 0, nil
}

// SetShipAllowGangSMB stores a ship's allowGangSMBUsage.
func SetShipAllowGangSMB(ctx context.Context, db *sql.DB, shipID int64, allow bool) error {
	v := 0
	if allow {
		v = 1
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO shipConfig (shipID, allowGangSMBUsage) VALUES (?, ?)
		ON DUPLICATE KEY UPDATE allowGangSMBUsage = VALUES(allowGangSMBUsage)`, shipID, v); err != nil {
		return fmt.Errorf("SetShipAllowGangSMB: %w", err)
	}
	return nil
}
