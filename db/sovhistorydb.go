// Package db: sovereignty history.
//
// sovHistory is one row per change of a system's holder: an alliance taking a system (from nobody or
// from another alliance) or a holder losing it to nobody. SetSov and ClearSov write it, so majority
// flips and GM painting both land here. The DED registry's Sovereignty tab
// (esp/concordsov.go) reads it. See docs/galnet-igb.md#sovereignty-tab.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

var (
	sovHistoryMu    sync.Mutex
	sovHistoryReady bool
)

// ensureSovHistoryTable creates sovHistory and, when it is empty, seeds it with every current claim
// at its claimedAt, so a server that held sov before the log existed still shows who holds what.
func ensureSovHistoryTable(ctx context.Context, db *sql.DB) error {
	sovHistoryMu.Lock()
	defer sovHistoryMu.Unlock()
	if sovHistoryReady {
		return nil
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS sovHistory (
		  eventID        BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
		  solarSystemID  INT UNSIGNED    NOT NULL,
		  oldAllianceID  INT UNSIGNED    NOT NULL DEFAULT 0,
		  newAllianceID  INT UNSIGNED    NOT NULL DEFAULT 0,
		  claimingCorpID INT UNSIGNED    NOT NULL DEFAULT 0,
		  changedAt      BIGINT(20)      NOT NULL,
		  PRIMARY KEY (eventID),
		  KEY idx_time (changedAt),
		  KEY idx_system (solarSystemID, changedAt),
		  KEY idx_new (newAllianceID, changedAt)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`); err != nil {
		return fmt.Errorf("sovHistory create: %w", err)
	}
	if err := ensureSolarSystemSovTable(ctx, db); err != nil {
		return err
	}
	var probe int
	err := db.QueryRowContext(ctx, `SELECT 1 FROM sovHistory LIMIT 1`).Scan(&probe)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO sovHistory (solarSystemID, oldAllianceID, newAllianceID, claimingCorpID, changedAt)
			SELECT solarSystemID, 0, allianceID, claimingCorpID, FLOOR(claimedAt / 10000000) - ?
			  FROM solarSystemSov
			 WHERE claimedAt > 0`, win32EpochOffset); err != nil {
			return fmt.Errorf("sovHistory seed: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("sovHistory probe: %w", err)
	}
	sovHistoryReady = true
	return nil
}

// recordSovChange appends one holder change. Best-effort: a failure is logged and never fails the
// sov write that called it.
func recordSovChange(ctx context.Context, db *sql.DB, systemID, oldAlliance, newAlliance, corpID int64) {
	if oldAlliance == newAlliance {
		return
	}
	if err := ensureSovHistoryTable(ctx, db); err != nil {
		log.Printf("[sov] history %d: %v", systemID, err)
		return
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO sovHistory (solarSystemID, oldAllianceID, newAllianceID, claimingCorpID, changedAt)
		VALUES (?, ?, ?, ?, ?)`, systemID, oldAlliance, newAlliance, corpID, time.Now().Unix()); err != nil {
		log.Printf("[sov] history %d: %v", systemID, err)
	}
}

// currentSovHolder is the alliance holding systemID now, 0 when unclaimed.
func currentSovHolder(ctx context.Context, db *sql.DB, systemID int64) int64 {
	var id int64
	_ = db.QueryRowContext(ctx, `SELECT allianceID FROM solarSystemSov WHERE solarSystemID = ?`, systemID).Scan(&id)
	return id
}

// SovEvent is one sovHistory row with its names resolved for display. An alliance that no longer
// exists has an empty name.
type SovEvent struct {
	EventID         int64
	SolarSystemID   int64
	SystemName      string
	Security        float64
	RegionName      string
	OldAllianceID   int64
	OldAllianceName string
	OldShortName    string
	NewAllianceID   int64
	NewAllianceName string
	NewShortName    string
	NewExecutorCorp int64
	ClaimingCorpID  int64
	ClaimingCorp    string
	ChangedAt       time.Time
}

// SovHistoryFilter narrows ListSovHistory. AcquiredOnly drops losses to nobody.
type SovHistoryFilter struct {
	AcquiredOnly bool
	SystemID     int64
	AllianceID   int64 // gained or lost by this alliance
	Limit        int
	Offset       int
}

func (f SovHistoryFilter) where() (string, []any) {
	w, args := " WHERE 1=1", []any{}
	if f.AcquiredOnly {
		w += " AND h.newAllianceID <> 0"
	}
	if f.SystemID != 0 {
		w += " AND h.solarSystemID = ?"
		args = append(args, f.SystemID)
	}
	if f.AllianceID != 0 {
		w += " AND (h.newAllianceID = ? OR h.oldAllianceID = ?)"
		args = append(args, f.AllianceID, f.AllianceID)
	}
	return w, args
}

// ListSovHistory returns holder changes, newest first.
func ListSovHistory(ctx context.Context, db *sql.DB, f SovHistoryFilter) ([]SovEvent, error) {
	if err := ensureSovHistoryTable(ctx, db); err != nil {
		return nil, err
	}
	if f.Limit <= 0 {
		f.Limit = 50
	}
	where, args := f.where()
	args = append(args, f.Limit, max(f.Offset, 0))
	rows, err := db.QueryContext(ctx, `
		SELECT h.eventID, h.solarSystemID, COALESCE(s.solarSystemName, ''), COALESCE(s.security, 0),
		       COALESCE(r.regionName, ''),
		       h.oldAllianceID, COALESCE(ao.allianceName, ''), COALESCE(ao.shortName, ''),
		       h.newAllianceID, COALESCE(an.allianceName, ''), COALESCE(an.shortName, ''),
		       COALESCE(an.executorCorpID, 0),
		       h.claimingCorpID, COALESCE(co.corporationName, ''), h.changedAt
		  FROM sovHistory h
		  LEFT JOIN mapSolarSystems s ON s.solarSystemID = h.solarSystemID
		  LEFT JOIN mapRegions r ON r.regionID = s.regionID
		  LEFT JOIN alliances ao ON ao.allianceID = h.oldAllianceID
		  LEFT JOIN alliances an ON an.allianceID = h.newAllianceID
		  LEFT JOIN corporation co ON co.corporationID = h.claimingCorpID`+where+`
		 ORDER BY h.changedAt DESC, h.eventID DESC
		 LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("ListSovHistory: %w", err)
	}
	defer rows.Close()
	var out []SovEvent
	for rows.Next() {
		var e SovEvent
		var at int64
		if err := rows.Scan(&e.EventID, &e.SolarSystemID, &e.SystemName, &e.Security, &e.RegionName,
			&e.OldAllianceID, &e.OldAllianceName, &e.OldShortName,
			&e.NewAllianceID, &e.NewAllianceName, &e.NewShortName, &e.NewExecutorCorp,
			&e.ClaimingCorpID, &e.ClaimingCorp, &at); err != nil {
			return nil, fmt.Errorf("ListSovHistory scan: %w", err)
		}
		e.ChangedAt = time.Unix(at, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

// CountSovHistory is the number of rows ListSovHistory would page through.
func CountSovHistory(ctx context.Context, db *sql.DB, f SovHistoryFilter) (int64, error) {
	if err := ensureSovHistoryTable(ctx, db); err != nil {
		return 0, err
	}
	where, args := f.where()
	var n int64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sovHistory h`+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("CountSovHistory: %w", err)
	}
	return n, nil
}

// SovHolding is an alliance and the number of systems it holds now, or gained or lost in a window.
type SovHolding struct {
	AllianceID   int64
	AllianceName string
	ShortName    string
	ExecutorCorp int64
	Systems      int64
}

// TopSovHolders ranks alliances by systems held now.
func TopSovHolders(ctx context.Context, db *sql.DB, limit int) ([]SovHolding, error) {
	if err := ensureSolarSystemSovTable(ctx, db); err != nil {
		return nil, err
	}
	return scanSovHoldings(ctx, db, "TopSovHolders", `
		SELECT v.allianceID, COALESCE(a.allianceName, ''), COALESCE(a.shortName, ''),
		       COALESCE(a.executorCorpID, 0), COUNT(*)
		  FROM solarSystemSov v
		  LEFT JOIN alliances a ON a.allianceID = v.allianceID
		 GROUP BY v.allianceID
		 ORDER BY COUNT(*) DESC, v.allianceID
		 LIMIT ?`, limit)
}

// TopSovGainers ranks alliances by systems taken since the given time.
func TopSovGainers(ctx context.Context, db *sql.DB, since time.Time, limit int) ([]SovHolding, error) {
	if err := ensureSovHistoryTable(ctx, db); err != nil {
		return nil, err
	}
	return scanSovHoldings(ctx, db, "TopSovGainers", `
		SELECT h.newAllianceID, COALESCE(a.allianceName, ''), COALESCE(a.shortName, ''),
		       COALESCE(a.executorCorpID, 0), COUNT(*)
		  FROM sovHistory h
		  LEFT JOIN alliances a ON a.allianceID = h.newAllianceID
		 WHERE h.newAllianceID <> 0 AND h.changedAt >= ?
		 GROUP BY h.newAllianceID
		 ORDER BY COUNT(*) DESC, h.newAllianceID
		 LIMIT ?`, since.Unix(), limit)
}

func scanSovHoldings(ctx context.Context, db *sql.DB, what, q string, args ...any) ([]SovHolding, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	defer rows.Close()
	var out []SovHolding
	for rows.Next() {
		var h SovHolding
		if err := rows.Scan(&h.AllianceID, &h.AllianceName, &h.ShortName, &h.ExecutorCorp, &h.Systems); err != nil {
			return nil, fmt.Errorf("%s scan: %w", what, err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
