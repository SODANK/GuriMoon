package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// ── Player entity ID migration ────────────────────────────────────────────────
//
// The client sorts owners purely by ID range (cfg.py): IsNPC is 10000 < id < 100000000,
// IsCorporation is 1 only for 1000000..2000000 below 100000000, and IsAlliance is 0 below
// 100000000. A player corporation or alliance numbered in the NPC range renders its info window
// through the NPC branch and is denied by util.IsCorporation / util.IsAlliance.
// MigratePlayerEntityIDs renumbers such rows into the player range and rewrites every
// referencing column. It is opt-in (server flag) because it rewrites live data.

// corpRefColumns lists every (table, column) that can hold a player
// corporationID.  channels.channelID is included because AddCorporation uses the
// corporationID as the corp chat channel's ID.
var corpRefColumns = [][2]string{
	{"corporation", "corporationID"},
	{"channels", "channelID"},
	{"channels", "ownerID"},
	{"channelchars", "channelID"},
	{"channelchars", "corpID"},
	{"character_", "corporationID"},
	{"chrEmployment", "corporationID"},
	{"chrApplications", "corporationID"},
	{"crpCharShares", "corporationID"},
	{"crpCharShares", "characterID"}, // a holder may be a corp (the treasury, or a stake in another corp)
	{"crpOffices", "corporationID"},
	{"entity", "ownerID"},
	{"dronestate", "ownerID"},
	{"bookmarks", "ownerID"},
	{"eveStaticOwners", "ownerID"},
	{"eveNames", "itemID"},
	{"insurance", "ownerID"},
	{"ramAssemblyLines", "ownerID"},
	{"ramAssemblyLineStations", "ownerID"},
	{"solarsystemsov", "claimingCorpID"},
	{"starbase", "ownerCorpID"},
	{"alliances", "creatorCorpID"},
	{"alliances", "executorCorpID"},
	{"chrStandings", "toID"},
	{"crpStandings", "corporationID"},
	{"crpStandings", "toID"},
	{"chrStandingChanges", "fromID"},
	{"chrStandingChanges", "toID"},
}

// allianceRefColumns lists every (table, column) that can hold an allianceID. The last block is
// polymorphic owner columns, rewritten by value because every ID in this database is unique
// across one space.
var allianceRefColumns = [][2]string{
	{"alliances", "allianceID"},
	{"alliance_shortnames", "allianceID"},
	// The alliance chat channel is keyed on the allianceID, as the corp channel is on the
	// corporationID (see corpRefColumns); leaving it behind orphans the channel row and its roster.
	{"channels", "channelID"},
	{"channels", "ownerID"},
	{"channelchars", "channelID"},
	{"allianceMembers", "allianceID"},
	{"allianceApplications", "allianceID"},
	{"allianceRelationships", "allianceID"},
	{"allianceRelationships", "toID"},
	{"corporation", "allianceID"},
	{"channelchars", "allianceID"},
	{"eveStaticOwners", "ownerID"},
	{"eveNames", "itemID"},
	{"solarsystemsov", "allianceID"},
	{"mapSolarSystems", "allianceID"},
	{"starbase", "ownerAllianceID"},

	{"killmails", "victimAllianceID"},
	{"killmail_attackers", "allianceID"},
	{"wars", "declaredByID"},
	{"wars", "againstID"},
	{"bills", "debtorID"},
	{"bills", "creditorID"},
	{"chrStandings", "toID"},
	{"crpStandings", "toID"},
}

// MigratePlayerEntityIDs renumbers player corporations and alliances that sit
// below MinPlayerEntityID.  With apply=false it only reports what it would do.
// It returns one log line per action taken (or planned).
func MigratePlayerEntityIDs(ctx context.Context, db *sql.DB, apply bool) ([]string, error) {
	var report []string

	// Corporations and alliances draw from one counter: both are owner IDs and
	// share the eveNames / eveStaticOwners key space.
	next, err := nextPlayerEntityID(ctx, db)
	if err != nil {
		return report, err
	}
	corpMap, err := planCorpRenumber(ctx, db, &next)
	if err != nil {
		return report, err
	}
	allianceMap, err := planAllianceRenumber(ctx, db, &next)
	if err != nil {
		return report, err
	}

	for _, m := range corpMap {
		report = append(report, fmt.Sprintf("corporation %d %q → %d", m.oldID, m.name, m.newID))
	}
	for _, m := range allianceMap {
		report = append(report, fmt.Sprintf("alliance %d %q → %d", m.oldID, m.name, m.newID))
	}
	if len(corpMap) == 0 && len(allianceMap) == 0 {
		report = append(report, "no player corporations or alliances below the player ID floor")
	}
	if !apply {
		report = append(report, "dry run — nothing written")
		return report, nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return report, fmt.Errorf("MigratePlayerEntityIDs begin: %w", err)
	}
	defer tx.Rollback()

	for _, m := range corpMap {
		n, err := renumber(ctx, tx, db, corpRefColumns, m.oldID, m.newID)
		if err != nil {
			return report, err
		}
		report = append(report, fmt.Sprintf("corporation %d → %d: %d rows updated", m.oldID, m.newID, n))
	}
	for _, m := range allianceMap {
		n, err := renumber(ctx, tx, db, allianceRefColumns, m.oldID, m.newID)
		if err != nil {
			return report, err
		}
		report = append(report, fmt.Sprintf("alliance %d → %d: %d rows updated", m.oldID, m.newID, n))
	}

	// Player corporations must be flagged so they stay out of the NPC-only
	// config.BulkData.tickernames bulk cache.
	res, err := tx.ExecContext(ctx, `
		UPDATE corporation SET hasPlayerPersonnelManager = 1
		 WHERE hasPlayerPersonnelManager = 0
		   AND corporationID NOT IN (SELECT corporationID FROM crpNPCCorporations)`)
	if err != nil {
		return report, fmt.Errorf("MigratePlayerEntityIDs flag player corps: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		report = append(report, fmt.Sprintf("hasPlayerPersonnelManager set on %d player corporation(s)", n))
	}

	if err := tx.Commit(); err != nil {
		return report, fmt.Errorf("MigratePlayerEntityIDs commit: %w", err)
	}

	// AUTO_INCREMENT cannot be changed inside the transaction above (DDL is
	// implicitly committed anyway), so raise it afterwards.
	if err := ensurePlayerCorpIDFloor(ctx, db); err != nil {
		return report, err
	}
	report = append(report, "corporation AUTO_INCREMENT at or above the player ID floor")
	return report, nil
}

type idRemap struct {
	oldID, newID int64
	name         string
}

// planCorpRenumber finds player corporations below the floor and assigns each a
// new ID from the shared player-entity counter.
func planCorpRenumber(ctx context.Context, db *sql.DB, next *int64) ([]idRemap, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT corporationID, corporationName FROM corporation
		 WHERE corporationID < ?
		   AND corporationID NOT IN (SELECT corporationID FROM crpNPCCorporations)
		 ORDER BY corporationID`, MinPlayerEntityID)
	if err != nil {
		return nil, fmt.Errorf("planCorpRenumber: %w", err)
	}
	defer rows.Close()

	var out []idRemap
	for rows.Next() {
		var m idRemap
		if err := rows.Scan(&m.oldID, &m.name); err != nil {
			return nil, fmt.Errorf("planCorpRenumber scan: %w", err)
		}
		m.newID = *next
		*next++
		out = append(out, m)
	}
	return out, rows.Err()
}

// planAllianceRenumber does the same for alliances.  The alliances table is
// created on demand, so a missing table simply means nothing to migrate.
func planAllianceRenumber(ctx context.Context, db *sql.DB, next *int64) ([]idRemap, error) {
	ok, err := tableExists(ctx, db, "alliances")
	if err != nil || !ok {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT allianceID, allianceName FROM alliances
		 WHERE allianceID < ? ORDER BY allianceID`, MinPlayerEntityID)
	if err != nil {
		return nil, fmt.Errorf("planAllianceRenumber: %w", err)
	}
	defer rows.Close()

	var out []idRemap
	for rows.Next() {
		var m idRemap
		if err := rows.Scan(&m.oldID, &m.name); err != nil {
			return nil, fmt.Errorf("planAllianceRenumber scan: %w", err)
		}
		m.newID = *next
		*next++
		out = append(out, m)
	}
	return out, rows.Err()
}

// renumber rewrites oldID to newID across every existing column in cols.
// Tables or columns absent from this schema are skipped.
func renumber(ctx context.Context, tx *sql.Tx, db *sql.DB, cols [][2]string, oldID, newID int64) (int64, error) {
	var total int64
	for _, tc := range cols {
		ok, err := columnExists(ctx, db, tc[0], tc[1])
		if err != nil {
			return total, err
		}
		if !ok {
			continue
		}
		stmt := fmt.Sprintf("UPDATE `%s` SET `%s` = ? WHERE `%s` = ?", tc[0], tc[1], tc[1])
		res, err := tx.ExecContext(ctx, stmt, newID, oldID)
		if err != nil {
			return total, fmt.Errorf("renumber %s.%s: %w", tc[0], tc[1], err)
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}

func tableExists(ctx context.Context, db *sql.DB, table string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.TABLES
		 WHERE TABLE_SCHEMA = DATABASE() AND LOWER(TABLE_NAME) = ?`,
		strings.ToLower(table)).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("tableExists %s: %w", table, err)
	}
	return n > 0, nil
}

func columnExists(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.COLUMNS
		 WHERE TABLE_SCHEMA = DATABASE()
		   AND LOWER(TABLE_NAME) = ? AND LOWER(COLUMN_NAME) = ?`,
		strings.ToLower(table), strings.ToLower(column)).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("columnExists %s.%s: %w", table, column, err)
	}
	return n > 0, nil
}
