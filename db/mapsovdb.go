// Package db: ESP faction sovereignty editor queries.
//
// NPC faction sovereignty lives in mapRegions, mapConstellations and mapSolarSystems (factionID).
// The client reads the system-level value live (stationSvc.GetSolarSystem, config.GetMapObjects),
// so edits show without a data rebuild. Player alliance sov (solarSystemSov, db/sovdb.go) is
// separate and mirrors into mapSolarSystems.allianceID.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// GetSystemSovFactionID returns the NPC faction holding a system, falling back to its
// constellation and then its region when the system row is NULL. Returns 0 for unclaimed space.
// Ore and ice distribution key off this value (the lowsec Jaspet/Hedbergite split and the four
// faction ices).
func GetSystemSovFactionID(ctx context.Context, db *sql.DB, sysID int64) (int64, error) {
	var factionID int64
	err := db.QueryRowContext(ctx, `
		SELECT COALESCE(s.factionID, c.factionID, r.factionID, 0)
		  FROM mapSolarSystems s
		  LEFT JOIN mapConstellations c ON c.constellationID = s.constellationID
		  LEFT JOIN mapRegions r ON r.regionID = s.regionID
		 WHERE s.solarSystemID = ?`, sysID).Scan(&factionID)
	if err != nil {
		return 0, fmt.Errorf("GetSystemSovFactionID sysID=%d: %w", sysID, err)
	}
	return factionID, nil
}

// FactionOption is one row of chrFactions for the sov editor dropdowns.
type FactionOption struct {
	FactionID   int64
	FactionName string
}

// ESPListFactions returns all NPC factions ordered by name.
func ESPListFactions(ctx context.Context, db *sql.DB) ([]FactionOption, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT factionID, factionName FROM chrFactions ORDER BY factionName`)
	if err != nil {
		return nil, fmt.Errorf("ESPListFactions: %w", err)
	}
	defer rows.Close()
	var out []FactionOption
	for rows.Next() {
		var f FactionOption
		if err := rows.Scan(&f.FactionID, &f.FactionName); err != nil {
			return nil, fmt.Errorf("ESPListFactions scan: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// RegionSovRow summarizes one region for the sov overview: the region-level
// faction plus how uniform its systems are (mixed regions show a faction count).
type RegionSovRow struct {
	RegionID            int64
	RegionName          string
	FactionID           int64  // 0 = no region-level faction
	FactionName         string // "" when FactionID is 0
	SystemCount         int64
	SystemsWithSov      int64  // systems with a non-NULL factionID
	DistinctFactions    int64  // distinct non-NULL faction values among systems
	SystemFactionName   string // the single system-level faction name when DistinctFactions==1
	SystemsWithAlliance int64  // systems with a player alliance claim
}

// ESPListRegionsSov returns every region with sov summary stats, optionally
// filtered by a case-insensitive name substring.
func ESPListRegionsSov(ctx context.Context, db *sql.DB, query string) ([]RegionSovRow, error) {
	q := `
		SELECT r.regionID, r.regionName,
		       COALESCE(r.factionID, 0),
		       COALESCE(f.factionName, ''),
		       COUNT(s.solarsystemID),
		       COUNT(s.factionID),
		       COUNT(DISTINCT s.factionID),
		       COALESCE(MIN(sf.factionName), ''),
		       COUNT(s.allianceID)
		FROM mapRegions r
		LEFT JOIN chrFactions f ON f.factionID = r.factionID
		LEFT JOIN mapSolarSystems s ON s.regionID = r.regionID
		LEFT JOIN chrFactions sf ON sf.factionID = s.factionID`
	var args []any
	if query != "" {
		q += ` WHERE r.regionName LIKE ?`
		args = append(args, "%"+query+"%")
	}
	q += ` GROUP BY r.regionID, r.regionName, r.factionID, f.factionName
	       ORDER BY r.regionName`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("ESPListRegionsSov: %w", err)
	}
	defer rows.Close()
	var out []RegionSovRow
	for rows.Next() {
		var r RegionSovRow
		if err := rows.Scan(&r.RegionID, &r.RegionName, &r.FactionID, &r.FactionName,
			&r.SystemCount, &r.SystemsWithSov, &r.DistinctFactions, &r.SystemFactionName,
			&r.SystemsWithAlliance); err != nil {
			return nil, fmt.Errorf("ESPListRegionsSov scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SystemSovRow is one solar system on the region detail page.
type SystemSovRow struct {
	SolarSystemID int64
	SystemName    string
	Security      float64
	FactionID     int64  // 0 = no faction sov
	FactionName   string // "" when FactionID is 0
	AllianceID    int64  // 0 = no player alliance sov (read-only display)
	AllianceName  string
}

// ESPListSystemsSovInRegion returns all systems in a region with their faction
// sov and any player alliance sov (shown read-only alongside).
func ESPListSystemsSovInRegion(ctx context.Context, db *sql.DB, regionID int64) ([]SystemSovRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT s.solarsystemID, s.solarSystemName, COALESCE(s.security, 0),
		       COALESCE(s.factionID, 0), COALESCE(f.factionName, ''),
		       COALESCE(s.allianceID, 0), COALESCE(a.allianceName, '')
		FROM mapSolarSystems s
		LEFT JOIN chrFactions f ON f.factionID = s.factionID
		LEFT JOIN alliances a ON a.allianceID = s.allianceID
		WHERE s.regionID = ?
		ORDER BY s.solarSystemName`, regionID)
	if err != nil {
		return nil, fmt.Errorf("ESPListSystemsSovInRegion: %w", err)
	}
	defer rows.Close()
	var out []SystemSovRow
	for rows.Next() {
		var s SystemSovRow
		if err := rows.Scan(&s.SolarSystemID, &s.SystemName, &s.Security,
			&s.FactionID, &s.FactionName, &s.AllianceID, &s.AllianceName); err != nil {
			return nil, fmt.Errorf("ESPListSystemsSovInRegion scan: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ESPGetRegionSov returns a region's name and current region-level faction.
// ok=false when the region doesn't exist.
func ESPGetRegionSov(ctx context.Context, db *sql.DB, regionID int64) (name string, factionID int64, ok bool, err error) {
	var fid sql.NullInt64
	err = db.QueryRowContext(ctx,
		`SELECT regionName, factionID FROM mapRegions WHERE regionID = ?`, regionID,
	).Scan(&name, &fid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, fmt.Errorf("ESPGetRegionSov: %w", err)
	}
	return name, fid.Int64, true, nil
}

// AllianceOption is one row of alliances for the sov editor dropdowns.
type AllianceOption struct {
	AllianceID   int64
	AllianceName string
}

// ESPListAlliances returns all player alliances ordered by name.
func ESPListAlliances(ctx context.Context, db *sql.DB) ([]AllianceOption, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT allianceID, allianceName FROM alliances ORDER BY allianceName`)
	if err != nil {
		return nil, fmt.Errorf("ESPListAlliances: %w", err)
	}
	defer rows.Close()
	var out []AllianceOption
	for rows.Next() {
		var a AllianceOption
		if err := rows.Scan(&a.AllianceID, &a.AllianceName); err != nil {
			return nil, fmt.Errorf("ESPListAlliances scan: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// sovOwnerArg maps id<=0 to SQL NULL (clear).
func sovOwnerArg(id int64) any {
	if id <= 0 {
		return nil
	}
	return id
}

// SetRegionSov applies one sovereignty owner to a region: at most one of factionID and allianceID
// may be >0, both zero clears all sov. Faction sov sets factionID at all three map levels and
// wipes alliance claims (solarSystemSov and the allianceID mirror); alliance sov upserts a
// solarSystemSov claim per system, mirrors allianceID and clears factionID. Returns the system count.
func SetRegionSov(ctx context.Context, db *sql.DB, regionID, factionID, allianceID int64) (int64, error) {
	if factionID > 0 && allianceID > 0 {
		return 0, fmt.Errorf("SetRegionSov: faction and alliance are mutually exclusive")
	}
	if err := ensureSolarSystemSovTable(ctx, db); err != nil {
		return 0, fmt.Errorf("SetRegionSov ensureTable: %w", err)
	}
	fArg := sovOwnerArg(factionID)
	if allianceID > 0 {
		fArg = nil
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE mapRegions SET factionID = ? WHERE regionID = ?`, fArg, regionID); err != nil {
		return 0, fmt.Errorf("SetRegionSov regions: %w", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE mapConstellations SET factionID = ? WHERE regionID = ?`, fArg, regionID); err != nil {
		return 0, fmt.Errorf("SetRegionSov constellations: %w", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE mapSolarSystems SET factionID = ?, allianceID = ? WHERE regionID = ?`,
		fArg, sovOwnerArg(allianceID), regionID); err != nil {
		return 0, fmt.Errorf("SetRegionSov systems: %w", err)
	}
	solarSystemRowChanged(0)
	if allianceID > 0 {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO solarSystemSov (solarSystemID, allianceID, claimingCorpID, claimingCharID, claimedAt)
			SELECT solarsystemID, ?, 0, 0, ? FROM mapSolarSystems WHERE regionID = ?
			ON DUPLICATE KEY UPDATE
			  allianceID = VALUES(allianceID),
			  claimingCorpID = 0, claimingCharID = 0,
			  claimedAt = VALUES(claimedAt),
			  pendingAllianceID = 0, pendingSince = 0`,
			allianceID, win32TimeNow(), regionID); err != nil {
			return 0, fmt.Errorf("SetRegionSov claims: %w", err)
		}
	} else {
		if _, err := db.ExecContext(ctx, `
			DELETE FROM solarSystemSov
			WHERE solarSystemID IN (SELECT solarsystemID FROM mapSolarSystems WHERE regionID = ?)`,
			regionID); err != nil {
			return 0, fmt.Errorf("SetRegionSov clear claims: %w", err)
		}
	}
	var n int64
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mapSolarSystems WHERE regionID = ?`, regionID,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("SetRegionSov count: %w", err)
	}
	return n, nil
}

// sovInPlaceholders builds "?,?,..." and the matching args slice.
func sovInPlaceholders(ids []int64) (string, []any) {
	ph := make([]byte, 0, len(ids)*2)
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		if i > 0 {
			ph = append(ph, ',')
		}
		ph = append(ph, '?')
		args = append(args, id)
	}
	return string(ph), args
}

// SetRegionsSov applies one sovereignty owner to several regions (same semantics as
// SetRegionSov). An empty regionIDs slice targets every region. Returns regions and total
// systems touched.
func SetRegionsSov(ctx context.Context, db *sql.DB, regionIDs []int64, factionID, allianceID int64) (regions, systems int64, err error) {
	if len(regionIDs) == 0 {
		rows, err := db.QueryContext(ctx, `SELECT regionID FROM mapRegions`)
		if err != nil {
			return 0, 0, fmt.Errorf("SetRegionsSov list: %w", err)
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return 0, 0, fmt.Errorf("SetRegionsSov scan: %w", err)
			}
			regionIDs = append(regionIDs, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, 0, fmt.Errorf("SetRegionsSov list: %w", err)
		}
	}
	for _, id := range regionIDs {
		n, err := SetRegionSov(ctx, db, id, factionID, allianceID)
		if err != nil {
			return regions, systems, err
		}
		regions++
		systems += n
	}
	return regions, systems, nil
}

// SetSystemsSov applies one sovereignty owner to a set of solar systems in
// bulk (same faction/alliance semantics as SetSystemSov; region/constellation
// rows are left alone). Returns the number of systems targeted.
func SetSystemsSov(ctx context.Context, db *sql.DB, systemIDs []int64, factionID, allianceID int64) (int64, error) {
	if factionID > 0 && allianceID > 0 {
		return 0, fmt.Errorf("SetSystemsSov: faction and alliance are mutually exclusive")
	}
	if len(systemIDs) == 0 {
		return 0, fmt.Errorf("SetSystemsSov: no systems given")
	}
	if err := ensureSolarSystemSovTable(ctx, db); err != nil {
		return 0, fmt.Errorf("SetSystemsSov ensureTable: %w", err)
	}
	ph, idArgs := sovInPlaceholders(systemIDs)
	fArg := sovOwnerArg(factionID)
	if allianceID > 0 {
		fArg = nil
	}
	args := append([]any{fArg, sovOwnerArg(allianceID)}, idArgs...)
	if _, err := db.ExecContext(ctx,
		`UPDATE mapSolarSystems SET factionID = ?, allianceID = ? WHERE solarsystemID IN (`+ph+`)`,
		args...); err != nil {
		return 0, fmt.Errorf("SetSystemsSov systems: %w", err)
	}
	for _, id := range systemIDs {
		solarSystemRowChanged(id)
	}
	if allianceID > 0 {
		args := append([]any{allianceID, win32TimeNow()}, idArgs...)
		if _, err := db.ExecContext(ctx, `
			INSERT INTO solarSystemSov (solarSystemID, allianceID, claimingCorpID, claimingCharID, claimedAt)
			SELECT solarsystemID, ?, 0, 0, ? FROM mapSolarSystems WHERE solarsystemID IN (`+ph+`)
			ON DUPLICATE KEY UPDATE
			  allianceID = VALUES(allianceID),
			  claimingCorpID = 0, claimingCharID = 0,
			  claimedAt = VALUES(claimedAt),
			  pendingAllianceID = 0, pendingSince = 0`,
			args...); err != nil {
			return 0, fmt.Errorf("SetSystemsSov claims: %w", err)
		}
	} else {
		if _, err := db.ExecContext(ctx,
			`DELETE FROM solarSystemSov WHERE solarSystemID IN (`+ph+`)`,
			idArgs...); err != nil {
			return 0, fmt.Errorf("SetSystemsSov clear claims: %w", err)
		}
	}
	return int64(len(systemIDs)), nil
}

// SetSystemSov applies one sovereignty owner to a single solar system, with the same
// faction/alliance semantics as SetRegionSov. Region and constellation rows are left alone.
func SetSystemSov(ctx context.Context, db *sql.DB, systemID, factionID, allianceID int64) error {
	if factionID > 0 && allianceID > 0 {
		return fmt.Errorf("SetSystemSov: faction and alliance are mutually exclusive")
	}
	fArg := sovOwnerArg(factionID)
	if allianceID > 0 {
		fArg = nil
	}
	res, err := db.ExecContext(ctx,
		`UPDATE mapSolarSystems SET factionID = ?, allianceID = ? WHERE solarsystemID = ?`,
		fArg, sovOwnerArg(allianceID), systemID)
	if err != nil {
		return fmt.Errorf("SetSystemSov: %w", err)
	}
	solarSystemRowChanged(systemID)
	if n, _ := res.RowsAffected(); n == 0 {
		var exists int
		if err := db.QueryRowContext(ctx,
			`SELECT 1 FROM mapSolarSystems WHERE solarsystemID = ?`, systemID,
		).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("SetSystemSov: solar system %d not found", systemID)
		}
	}
	if allianceID > 0 {
		if err := SetSov(ctx, db, systemID, allianceID, 0, 0); err != nil {
			return fmt.Errorf("SetSystemSov claim: %w", err)
		}
	} else {
		if _, err := ClearSov(ctx, db, systemID); err != nil {
			return fmt.Errorf("SetSystemSov clear claim: %w", err)
		}
	}
	return nil
}

// SovSystemHit is a system search result for the sov panel's jump box.
type SovSystemHit struct {
	SolarSystemID int64   `json:"solarSystemID"`
	SystemName    string  `json:"systemName"`
	RegionID      int64   `json:"regionID"`
	RegionName    string  `json:"regionName"`
	Security      float64 `json:"security"`
	FactionName   string  `json:"factionName"`
	AllianceName  string  `json:"allianceName"`
}

// ESPSearchSovSystems finds solar systems by name substring for the jump box.
func ESPSearchSovSystems(ctx context.Context, db *sql.DB, query string) ([]SovSystemHit, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT s.solarsystemID, s.solarSystemName, s.regionID,
		       COALESCE(r.regionName, ''), COALESCE(s.security, 0),
		       COALESCE(f.factionName, ''), COALESCE(a.allianceName, '')
		FROM mapSolarSystems s
		LEFT JOIN mapRegions r ON r.regionID = s.regionID
		LEFT JOIN chrFactions f ON f.factionID = s.factionID
		LEFT JOIN alliances a ON a.allianceID = s.allianceID
		WHERE s.solarSystemName LIKE ?
		ORDER BY s.solarSystemName
		LIMIT 20`, "%"+query+"%")
	if err != nil {
		return nil, fmt.Errorf("ESPSearchSovSystems: %w", err)
	}
	defer rows.Close()
	var out []SovSystemHit
	for rows.Next() {
		var h SovSystemHit
		if err := rows.Scan(&h.SolarSystemID, &h.SystemName, &h.RegionID,
			&h.RegionName, &h.Security, &h.FactionName, &h.AllianceName); err != nil {
			return nil, fmt.Errorf("ESPSearchSovSystems scan: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
