package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrNotConquerable refuses a capture of a station whose type is not conquerable.
var ErrNotConquerable = errors.New("station is not conquerable")

// Conquerable stations: every station whose type carries staStationTypes.conquerable starts under
// CONCORD (OwnerCONCORD), the neutral first holder, and changes hands when an attacker takes its
// shield to 0 (conquerStation in cmd/gurimoon). The seeds ship it that way
// (sql/stastations.sql, sql/ramassemblylinestations.sql, sql/ramassemblylines.sql);
// MigrateConquerablesToCONCORD brings an older database in line once. Notes:
// docs/station-management.md, "Conquerable stations".

// conquerableNeutralOwner is the WHERE clause for a conquerable station still held by no real
// corporation: an NPC corporation, or an owner ID with no corporation row at all.
const conquerableNeutralOwner = `
	st.stationTypeID IN (SELECT stationTypeID FROM staStationTypes WHERE conquerable = 1)
	AND (st.corporationID IN (SELECT corporationID FROM crpNPCCorporations)
	     OR st.corporationID NOT IN (SELECT corporationID FROM corporation))`

// MigrateConquerablesToCONCORD hands every conquerable station held by an NPC corporation or a
// corporation that does not exist to CONCORD, with the owner of its assembly lines. A station a
// live player or bot corporation holds is left alone. Runs once, guarded by server_migrations.
// Returns the stations moved.
func MigrateConquerablesToCONCORD(ctx context.Context, db *sql.DB) (int64, error) {
	const migrationName = "conquerables_concord_v1"
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS server_migrations (
			name    VARCHAR(64) NOT NULL,
			applied BIGINT NOT NULL,
			PRIMARY KEY (name)
		)`); err != nil {
		return 0, fmt.Errorf("MigrateConquerablesToCONCORD meta table: %w", err)
	}
	var done int
	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM server_migrations WHERE name = ?`, migrationName).Scan(&done)
	if done > 0 {
		return 0, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT st.stationID FROM staStations st WHERE `+conquerableNeutralOwner+
		` AND st.corporationID <> ?`, OwnerCONCORD)
	if err != nil {
		return 0, fmt.Errorf("MigrateConquerablesToCONCORD list: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("MigrateConquerablesToCONCORD scan: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("MigrateConquerablesToCONCORD rows: %w", err)
	}
	for _, id := range ids {
		if err := UpdateStationOwner(ctx, db, id, OwnerCONCORD); err != nil {
			return 0, fmt.Errorf("MigrateConquerablesToCONCORD: %w", err)
		}
		if err := UpdateStationLineOwners(ctx, db, id, OwnerCONCORD); err != nil {
			return 0, fmt.Errorf("MigrateConquerablesToCONCORD: %w", err)
		}
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO server_migrations (name, applied) VALUES (?, UNIX_TIMESTAMP())`,
		migrationName); err != nil {
		return int64(len(ids)), fmt.Errorf("MigrateConquerablesToCONCORD guard: %w", err)
	}
	return int64(len(ids)), nil
}

// UpdateStationLineOwners sets the owner of a station's assembly lines, both the per-station
// summary (ramassemblylinestations) and every line (ramassemblylines), which the client is served.
// Call it beside UpdateStationOwner when a station changes hands.
func UpdateStationLineOwners(ctx context.Context, db *sql.DB, stationID, ownerID int64) error {
	if _, err := db.ExecContext(ctx,
		`UPDATE ramassemblylinestations SET ownerID = ? WHERE stationID = ?`, ownerID, stationID); err != nil {
		return fmt.Errorf("UpdateStationLineOwners stations %d: %w", stationID, err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE ramassemblylines SET ownerID = ? WHERE containerID = ?`, ownerID, stationID); err != nil {
		return fmt.Errorf("UpdateStationLineOwners lines %d: %w", stationID, err)
	}
	return nil
}

// SetStationAccessRules writes a player owner's access rules alone, keyed by service, leaving the
// station's details, cost modifiers and office slots as they are (SaveStationManagement rewrites the
// details too). corpID must own the station (StationManageableBy).
func SetStationAccessRules(ctx context.Context, db *sql.DB, stationID, corpID int64, rules []ServiceAccessRule) error {
	if ok, reason := StationManageableBy(ctx, db, stationID, corpID); !ok {
		return fmt.Errorf("SetStationAccessRules %d: %s", stationID, reason)
	}
	if err := ensureStationMgmtTables(ctx, db); err != nil {
		return err
	}
	for _, r := range rules {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO stationServiceAccessRules (stationID, serviceID, minimumStanding,
			       minimumCharSecurity, maximumCharSecurity, minimumCorpSecurity, maximumCorpSecurity)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE minimumStanding = VALUES(minimumStanding),
			       minimumCharSecurity = VALUES(minimumCharSecurity), maximumCharSecurity = VALUES(maximumCharSecurity),
			       minimumCorpSecurity = VALUES(minimumCorpSecurity), maximumCorpSecurity = VALUES(maximumCorpSecurity)`,
			stationID, r.ServiceID, clampFloat(r.MinimumStanding, -10, 10),
			clampFloat(r.MinimumCharSecurity, -10, 10), clampFloat(r.MaximumCharSecurity, -10, 10),
			clampFloat(r.MinimumCorpSecurity, -10, 10), clampFloat(r.MaximumCorpSecurity, -10, 10)); err != nil {
			return fmt.Errorf("SetStationAccessRules %d service %d: %w", stationID, r.ServiceID, err)
		}
	}
	return nil
}

// ClearStationAccessRules drops every access rule at a station corpID owns: open to everyone again.
func ClearStationAccessRules(ctx context.Context, db *sql.DB, stationID, corpID int64) error {
	if ok, reason := StationManageableBy(ctx, db, stationID, corpID); !ok {
		return fmt.Errorf("ClearStationAccessRules %d: %s", stationID, reason)
	}
	if err := ensureStationMgmtTables(ctx, db); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM stationServiceAccessRules WHERE stationID = ?`, stationID); err != nil {
		return fmt.Errorf("ClearStationAccessRules %d: %w", stationID, err)
	}
	return nil
}

// ResetStationManagement drops everything an owner set at a station: access rules, cost modifiers
// and private office slots. A conquest runs it, so a new owner starts with a freeport and never
// inherits the old owner's doors.
func ResetStationManagement(ctx context.Context, db *sql.DB, stationID int64) error {
	if err := ensureStationMgmtTables(ctx, db); err != nil {
		return err
	}
	for _, table := range []string{"stationServiceAccessRules", "stationServiceCostModifiers", "stationOfficeSlots"} {
		if _, err := db.ExecContext(ctx, `DELETE FROM `+table+` WHERE stationID = ?`, stationID); err != nil {
			return fmt.Errorf("ResetStationManagement %d %s: %w", stationID, table, err)
		}
	}
	return nil
}

// ServiceMarket is the Market station service (staServices 64). Conquerable stations have none;
// outposts do.
const ServiceMarket int64 = 64

// StationHasService reports whether a station's operation offers a service
// (staOperationServices through staStations.operationID).
func StationHasService(ctx context.Context, db *sql.DB, stationID, serviceID int64) bool {
	var n int
	_ = db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM staStations st
		  JOIN staOperationServices os ON os.operationID = st.operationID
		 WHERE st.stationID = ? AND os.serviceID = ?`, stationID, serviceID).Scan(&n)
	return n > 0
}

// IsConquerableStation reports whether a station's type carries the conquerable flag.
func IsConquerableStation(ctx context.Context, db *sql.DB, stationID int64) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM staStations st JOIN staStationTypes t ON t.stationTypeID = st.stationTypeID
		 WHERE st.stationID = ? AND t.conquerable = 1`, stationID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("IsConquerableStation %d: %w", stationID, err)
	}
	return n > 0, nil
}
