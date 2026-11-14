// Package db: sovereignty (Layer 1).
//
// solarSystemSov maps each claimed 0.0 solar system to its alliance. Slash commands and
// the majority recompute (sovmajority.go) read and write it.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
)

// sovPendingMigrated is set once the pending-flip columns are known to exist.
var (
	sovPendingMu       sync.Mutex
	sovPendingMigrated bool
)

// ensureSolarSystemSovTable creates the sovereignty table if missing and adds the pending-flip
// columns to a table made before them (sql/solarsystemsov.sql carries them for a fresh install).
func ensureSolarSystemSovTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS solarSystemSov (
		  solarSystemID     INT UNSIGNED NOT NULL,
		  allianceID        INT UNSIGNED NOT NULL,
		  claimingCorpID    INT UNSIGNED NOT NULL DEFAULT 0,
		  claimingCharID    INT UNSIGNED NOT NULL DEFAULT 0,
		  claimedAt         BIGINT(20)   NOT NULL DEFAULT 0,
		  pendingAllianceID INT UNSIGNED NOT NULL DEFAULT 0,
		  pendingSince      BIGINT(20)   NOT NULL DEFAULT 0,
		  PRIMARY KEY (solarSystemID),
		  KEY idx_alliance (allianceID),
		  KEY idx_pending (pendingSince)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return err
	}
	sovPendingMu.Lock()
	defer sovPendingMu.Unlock()
	if sovPendingMigrated {
		return nil
	}
	for _, q := range []string{
		`ALTER TABLE solarSystemSov ADD COLUMN pendingAllianceID INT UNSIGNED NOT NULL DEFAULT 0`,
		`ALTER TABLE solarSystemSov ADD COLUMN pendingSince BIGINT(20) NOT NULL DEFAULT 0`,
		`ALTER TABLE solarSystemSov ADD INDEX idx_pending (pendingSince)`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil && !isDupColumn(err) {
			return fmt.Errorf("solarSystemSov pending columns: %w", err)
		}
	}
	sovPendingMigrated = true
	return nil
}

// SovRecord describes a single system's sovereignty state.
type SovRecord struct {
	SolarSystemID  int64
	AllianceID     int64
	ClaimingCorpID int64
	ClaimingCharID int64
	ClaimedAt      uint64 // Win32 FILETIME
	// The alliance waiting out the flip grace (0 with PendingSince set is a pending loss to
	// nobody) and the Win32 FILETIME it took the majority. PendingSince 0 means nothing waits.
	PendingAllianceID int64
	PendingSince      int64
}

// SetSov records that allianceID has claimed solarSystemID, upserting on conflict, and clears
// any pending flip.
// It also mirrors the owner into mapSolarSystems.allianceID, which stationSvc.GetSolarSystem
// sends to the client for the star map. solarSystemSov stays the source of truth.
func SetSov(ctx context.Context, db *sql.DB, systemID, allianceID, corpID, charID int64) error {
	if err := ensureSolarSystemSovTable(ctx, db); err != nil {
		return fmt.Errorf("SetSov ensureTable: %w", err)
	}
	// Seed the history before the write, so a first-ever claim is logged once, not seeded and logged.
	_ = ensureSovHistoryTable(ctx, db)
	old := currentSovHolder(ctx, db, systemID)
	now := win32TimeNow()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO solarSystemSov
		  (solarSystemID, allianceID, claimingCorpID, claimingCharID, claimedAt)
		VALUES (?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
		  allianceID = VALUES(allianceID),
		  claimingCorpID = VALUES(claimingCorpID),
		  claimingCharID = VALUES(claimingCharID),
		  claimedAt = VALUES(claimedAt),
		  pendingAllianceID = 0, pendingSince = 0`,
		systemID, allianceID, corpID, charID, now,
	); err != nil {
		return fmt.Errorf("SetSov: %w", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE mapSolarSystems SET allianceID = ? WHERE solarSystemID = ?`,
		allianceID, systemID,
	); err != nil {
		return fmt.Errorf("SetSov mirror mapSolarSystems: %w", err)
	}
	solarSystemRowChanged(systemID)
	recordSovChange(ctx, db, systemID, old, allianceID, corpID)
	return nil
}

// ClearSov removes any sov claim on the given system and clears the mirror
// in mapSolarSystems.allianceID so the F10 map / system info no longer paints
// the system with an alliance color. Returns rows affected in solarSystemSov.
func ClearSov(ctx context.Context, db *sql.DB, systemID int64) (int64, error) {
	if err := ensureSolarSystemSovTable(ctx, db); err != nil {
		return 0, fmt.Errorf("ClearSov ensureTable: %w", err)
	}
	_ = ensureSovHistoryTable(ctx, db)
	old := currentSovHolder(ctx, db, systemID)
	res, err := db.ExecContext(ctx,
		`DELETE FROM solarSystemSov WHERE solarSystemID = ?`, systemID)
	if err != nil {
		return 0, fmt.Errorf("ClearSov: %w", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE mapSolarSystems SET allianceID = NULL WHERE solarSystemID = ?`,
		systemID,
	); err != nil {
		return 0, fmt.Errorf("ClearSov mirror mapSolarSystems: %w", err)
	}
	solarSystemRowChanged(systemID)
	recordSovChange(ctx, db, systemID, old, 0, 0)
	n, _ := res.RowsAffected()
	return n, nil
}

// GetSov returns the sov record for a system. ok=false when the system is
// unclaimed (no row in the table).
func GetSov(ctx context.Context, db *sql.DB, systemID int64) (rec SovRecord, ok bool, err error) {
	if err := ensureSolarSystemSovTable(ctx, db); err != nil {
		return SovRecord{}, false, fmt.Errorf("GetSov ensureTable: %w", err)
	}
	rec.SolarSystemID = systemID
	err = db.QueryRowContext(ctx, `
		SELECT allianceID, claimingCorpID, claimingCharID, claimedAt, pendingAllianceID, pendingSince
		  FROM solarSystemSov
		 WHERE solarSystemID = ?`, systemID,
	).Scan(&rec.AllianceID, &rec.ClaimingCorpID, &rec.ClaimingCharID, &rec.ClaimedAt,
		&rec.PendingAllianceID, &rec.PendingSince)
	if errors.Is(err, sql.ErrNoRows) {
		return SovRecord{SolarSystemID: systemID}, false, nil
	}
	if err != nil {
		return SovRecord{}, false, fmt.Errorf("GetSov: %w", err)
	}
	return rec, true, nil
}

// SetSovPending stores a system's pending flip (since 0 clears it). The row must exist: a pending
// flip only waits against a holder.
func SetSovPending(ctx context.Context, db *sql.DB, systemID, allianceID, since int64) error {
	if err := ensureSolarSystemSovTable(ctx, db); err != nil {
		return fmt.Errorf("SetSovPending ensureTable: %w", err)
	}
	if since == 0 {
		allianceID = 0
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE solarSystemSov SET pendingAllianceID = ?, pendingSince = ? WHERE solarSystemID = ?`,
		allianceID, since, systemID,
	); err != nil {
		return fmt.Errorf("SetSovPending: %w", err)
	}
	return nil
}

// ListSovPendingDue returns the systems whose pending flip started at or before the Win32
// FILETIME before.
func ListSovPendingDue(ctx context.Context, db *sql.DB, before int64) ([]int64, error) {
	if err := ensureSolarSystemSovTable(ctx, db); err != nil {
		return nil, fmt.Errorf("ListSovPendingDue ensureTable: %w", err)
	}
	rows, err := db.QueryContext(ctx, `
		SELECT solarSystemID FROM solarSystemSov
		 WHERE pendingSince > 0 AND pendingSince <= ?
		 ORDER BY solarSystemID`, before)
	if err != nil {
		return nil, fmt.Errorf("ListSovPendingDue: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("ListSovPendingDue scan: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ListSovForAlliance returns the solarSystemIDs claimed by allianceID.
func ListSovForAlliance(ctx context.Context, db *sql.DB, allianceID int64) ([]int64, error) {
	if err := ensureSolarSystemSovTable(ctx, db); err != nil {
		return nil, fmt.Errorf("ListSovForAlliance ensureTable: %w", err)
	}
	rows, err := db.QueryContext(ctx,
		`SELECT solarSystemID FROM solarSystemSov WHERE allianceID = ? ORDER BY solarSystemID`,
		allianceID)
	if err != nil {
		return nil, fmt.Errorf("ListSovForAlliance: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("ListSovForAlliance scan: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// GetCharSovContext returns the caller's current solar system plus the corp
// and alliance they're acting on behalf of. allianceID is 0 when the
// character's corp is unaffiliated.
func GetCharSovContext(ctx context.Context, db *sql.DB, charID int64) (solarSystemID, corpID, allianceID int64, err error) {
	var aID sql.NullInt64
	err = db.QueryRowContext(ctx, `
		SELECT c.solarSystemID, c.corporationID, co.allianceID
		  FROM character_ c
		  LEFT JOIN corporation co ON co.corporationID = c.corporationID
		 WHERE c.characterID = ?`, charID,
	).Scan(&solarSystemID, &corpID, &aID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, 0, fmt.Errorf("GetCharSovContext: character %d not found", charID)
	}
	if err != nil {
		return 0, 0, 0, fmt.Errorf("GetCharSovContext: %w", err)
	}
	if aID.Valid {
		allianceID = aID.Int64
	}
	return solarSystemID, corpID, allianceID, nil
}

// GetSystemSecurity returns the security rating (e.g. 0.5, -0.4) for a
// solar system. Returns the value and ok=false if the system doesn't exist.
func GetSystemSecurity(ctx context.Context, db *sql.DB, systemID int64) (security float64, ok bool, err error) {
	err = db.QueryRowContext(ctx,
		`SELECT security FROM mapSolarSystems WHERE solarSystemID = ?`, systemID,
	).Scan(&security)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("GetSystemSecurity: %w", err)
	}
	return security, true, nil
}

// GetAllianceName resolves an alliance's short display name for slash output.
// Returns "" when the alliance is missing.
func GetAllianceName(ctx context.Context, db *sql.DB, allianceID int64) (string, error) {
	var name string
	err := db.QueryRowContext(ctx,
		`SELECT allianceName FROM alliances WHERE allianceID = ?`, allianceID,
	).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("GetAllianceName: %w", err)
	}
	return name, nil
}

// GetSystemName resolves a solar system's display name. Returns "" if missing.
func GetSystemName(ctx context.Context, db *sql.DB, systemID int64) (string, error) {
	var name string
	err := db.QueryRowContext(ctx,
		`SELECT solarSystemName FROM mapSolarSystems WHERE solarSystemID = ?`, systemID,
	).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("GetSystemName: %w", err)
	}
	return name, nil
}
