// Package db: persisted hit points for player-owned structures.
//
// A structure's HP lives on its destiny.Entity while its system is loaded, and the system is
// torn down when the last pilot leaves, so damage is stored here: one row per structure that has
// taken damage, a missing row reading as full HP (as asteroidDepletion does). Fractions, not
// absolute HP, so a type edit that changes max HP cannot grant extra HP.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
)

// StructureDamageRow is one structure's remaining HP as fractions of max.
type StructureDamageRow struct {
	EntityID int64
	Shield   float64
	Armor    float64
	Hull     float64
	SavedAt  uint64 // Win32 FILETIME; shield regenerates across the gap
}

var (
	structureDamageOnce sync.Once
	structureDamageErr  error
)

func ensureStructureDamageTable(ctx context.Context, db *sql.DB) error {
	structureDamageOnce.Do(func() {
		_, structureDamageErr = db.ExecContext(ctx, `
			CREATE TABLE IF NOT EXISTS starbaseDamage (
			  entityID INT UNSIGNED NOT NULL,
			  shield   DOUBLE       NOT NULL DEFAULT 1,
			  armor    DOUBLE       NOT NULL DEFAULT 1,
			  hull     DOUBLE       NOT NULL DEFAULT 1,
			  savedAt  BIGINT(20)   NOT NULL DEFAULT 0,
			  PRIMARY KEY (entityID)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	})
	return structureDamageErr
}

// LoadStructureDamage returns the saved damage for one structure; ok=false
// means none is saved (full HP).
func LoadStructureDamage(ctx context.Context, db *sql.DB, entityID int64) (StructureDamageRow, bool, error) {
	if err := ensureStructureDamageTable(ctx, db); err != nil {
		return StructureDamageRow{}, false, err
	}
	r := StructureDamageRow{EntityID: entityID}
	err := db.QueryRowContext(ctx,
		`SELECT shield, armor, hull, savedAt FROM starbaseDamage WHERE entityID = ?`, entityID,
	).Scan(&r.Shield, &r.Armor, &r.Hull, &r.SavedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	if err != nil {
		return r, false, fmt.Errorf("LoadStructureDamage %d: %w", entityID, err)
	}
	return r, true, nil
}

// SaveStructureDamage upserts a batch of rows. A structure back at full HP is
// deleted rather than stored, so the table stays as sparse as the damage.
func SaveStructureDamage(ctx context.Context, db *sql.DB, rows []StructureDamageRow) error {
	if len(rows) == 0 {
		return nil
	}
	if err := ensureStructureDamageTable(ctx, db); err != nil {
		return err
	}
	for _, r := range rows {
		if r.Shield >= 0.9999 && r.Armor >= 0.9999 && r.Hull >= 0.9999 {
			if _, err := db.ExecContext(ctx, `DELETE FROM starbaseDamage WHERE entityID = ?`, r.EntityID); err != nil {
				return fmt.Errorf("SaveStructureDamage delete %d: %w", r.EntityID, err)
			}
			continue
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO starbaseDamage (entityID, shield, armor, hull, savedAt)
			VALUES (?, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE shield = VALUES(shield), armor = VALUES(armor),
			                        hull = VALUES(hull), savedAt = VALUES(savedAt)`,
			r.EntityID, r.Shield, r.Armor, r.Hull, r.SavedAt); err != nil {
			return fmt.Errorf("SaveStructureDamage %d: %w", r.EntityID, err)
		}
	}
	return nil
}

// DeleteStructureDamage drops a destroyed or removed structure's row.
func DeleteStructureDamage(ctx context.Context, db *sql.DB, entityID int64) error {
	if err := ensureStructureDamageTable(ctx, db); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `DELETE FROM starbaseDamage WHERE entityID = ?`, entityID)
	return err
}
