package db

// Queries behind CONCORD's Capsuleer Incident Registry pages that the profile
// aggregates (killstats.go) do not already answer: corporation leaderboards,
// and the per-system record.
//
// The credit rule is the profile pages' rule: one credit per killmail, never
// one per attacker row. A four-pilot gang from one corporation on one mail is
// one incident for that corporation, and its value is counted once.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// KillboardCorpLeader is one row of a corporation leaderboard.
type KillboardCorpLeader struct {
	CorpID int64
	Name   string
	Count  int64
	ISK    float64
}

// TopKillerCorps ranks corporations by the incidents their pilots were party
// to (as aggressors) over the window.
func TopKillerCorps(ctx context.Context, db *sql.DB, since time.Time, limit int, includeNPC bool) ([]KillboardCorpLeader, error) {
	return topAggressorCorps(ctx, db, since, 0, limit, includeNPC)
}

// SystemTopCorps is TopKillerCorps inside one solar system. Hostile-entity
// corporations are included: in a system's record, the pirates working it
// are the answer to "who is active here".
func SystemTopCorps(ctx context.Context, db *sql.DB, systemID int64, since time.Time, limit int) ([]KillboardCorpLeader, error) {
	return topAggressorCorps(ctx, db, since, systemID, limit, true)
}

func topAggressorCorps(ctx context.Context, db *sql.DB, since time.Time, systemID int64, limit int, includeNPC bool) ([]KillboardCorpLeader, error) {
	if limit <= 0 {
		limit = 10
	}
	where := "k.killTime >= ?"
	args := []any{since.Unix()}
	if systemID != 0 {
		where += " AND k.solarSystemID = ?"
		args = append(args, systemID)
	}
	npcAtk := " AND a.isNPC = 0"
	if includeNPC {
		npcAtk = ""
	} else {
		where += " AND k.npcOnly = 0"
	}
	args = append(args, limit)
	// The derived table collapses attacker rows to one (corp, kill) pair
	// first, so the value summed outside is each kill's value once per corp.
	rows, err := db.QueryContext(ctx, `
		SELECT x.corpID, MAX(x.corpName), COUNT(*), SUM(x.totalValue)
		  FROM (SELECT a.corpID, MAX(a.corpName) AS corpName, a.killID, MAX(k.totalValue) AS totalValue
		          FROM killmail_attackers a
		          JOIN killmails k ON k.killID = a.killID
		         WHERE a.corpID <> 0`+npcAtk+` AND `+where+`
		         GROUP BY a.corpID, a.killID) x
		 GROUP BY x.corpID
		 ORDER BY COUNT(*) DESC, SUM(x.totalValue) DESC
		 LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("topAggressorCorps: %w", err)
	}
	defer rows.Close()
	var out []KillboardCorpLeader
	for rows.Next() {
		var l KillboardCorpLeader
		var name sql.NullString
		var isk sql.NullFloat64
		if err := rows.Scan(&l.CorpID, &name, &l.Count, &isk); err != nil {
			return nil, fmt.Errorf("topAggressorCorps scan: %w", err)
		}
		l.Name, l.ISK = name.String, isk.Float64
		out = append(out, l)
	}
	return out, rows.Err()
}

// SystemDayCount is one day of a system's record: losses with a capsuleer
// among the aggressors, and losses to hostile entities alone.
type SystemDayCount struct {
	Day          time.Time
	Capsuleer    int64
	HostileOnly  int64
	AssessedLoss float64
}

// SystemActivity buckets a system's losses by day over the last n days,
// oldest first, with empty days present as zeroes (see ActorActivity: the
// strip is read as a shape, and a gap closed up is a lie about it).
func SystemActivity(ctx context.Context, db *sql.DB, systemID int64, days int) ([]SystemDayCount, error) {
	if days <= 0 {
		days = 14
	}
	start := time.Now().Truncate(24*time.Hour).AddDate(0, 0, -(days - 1))
	buckets := make([]SystemDayCount, days)
	index := map[int64]int{}
	for i := range buckets {
		d := start.AddDate(0, 0, i)
		buckets[i].Day = d
		index[d.Unix()] = i
	}
	_, offset := start.Zone()
	rows, err := db.QueryContext(ctx, `
		SELECT FLOOR((k.killTime + ?) / 86400) * 86400 - ? AS bucket,
		       SUM(k.npcOnly = 0), SUM(k.npcOnly = 1), SUM(k.totalValue)
		  FROM killmails k
		 WHERE k.solarSystemID = ? AND k.killTime >= ?
		 GROUP BY bucket`, offset, offset, systemID, start.Unix())
	if err != nil {
		return nil, fmt.Errorf("SystemActivity: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var bucket int64
		var caps, hostile sql.NullInt64
		var isk sql.NullFloat64
		if err := rows.Scan(&bucket, &caps, &hostile, &isk); err != nil {
			return nil, fmt.Errorf("SystemActivity scan: %w", err)
		}
		if i, ok := index[bucket]; ok {
			buckets[i].Capsuleer, buckets[i].HostileOnly, buckets[i].AssessedLoss = caps.Int64, hostile.Int64, isk.Float64
		}
	}
	return buckets, rows.Err()
}

// RegistrySystem is the header of a system record.
type RegistrySystem struct {
	SystemID          int64
	Name              string
	Security          float64
	ConstellationID   int64
	ConstellationName string
	RegionID          int64
	RegionName        string
}

// GetRegistrySystem resolves a solar system for the registry, or (nil, nil)
// when there is no such system.
func GetRegistrySystem(ctx context.Context, db *sql.DB, systemID int64) (*RegistrySystem, error) {
	s := &RegistrySystem{SystemID: systemID}
	err := db.QueryRowContext(ctx, `
		SELECT ss.solarSystemName, ss.security, ss.constellationID, COALESCE(c.constellationName, ''),
		       ss.regionID, COALESCE(r.regionName, '')
		  FROM mapSolarSystems ss
		  LEFT JOIN mapConstellations c ON c.constellationID = ss.constellationID
		  LEFT JOIN mapRegions r ON r.regionID = ss.regionID
		 WHERE ss.solarSystemID = ?`, systemID).Scan(&s.Name, &s.Security, &s.ConstellationID,
		&s.ConstellationName, &s.RegionID, &s.RegionName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("GetRegistrySystem: %w", err)
	}
	return s, nil
}
