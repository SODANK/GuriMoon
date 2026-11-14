package db

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"gurimoon/marshal"
)

// Stat-type constants for mapHistory rows.
const (
	StatShipKill    = 1
	StatPodKill     = 2
	StatFactionKill = 3
	StatJump        = 4
)

var ensureMapHistoryOnce sync.Once

func ensureMapHistoryTable(ctx context.Context, db *sql.DB) error {
	var err error
	ensureMapHistoryOnce.Do(func() {
		_, err = db.ExecContext(ctx, `
			CREATE TABLE IF NOT EXISTS mapHistory (
			  solarSystemID INT UNSIGNED NOT NULL,
			  statType      TINYINT UNSIGNED NOT NULL,
			  hourBucket    INT UNSIGNED NOT NULL,
			  count         INT UNSIGNED NOT NULL DEFAULT 0,
			  PRIMARY KEY (solarSystemID, statType, hourBucket),
			  INDEX idx_maphistory_bucket (hourBucket)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	})
	return err
}

// RecordMapEvent increments the counter for a (system, statType) pair in the
// current hour bucket. Safe for concurrent fire-and-forget use.
func RecordMapEvent(ctx context.Context, db *sql.DB, solarSystemID int64, statType int, count int) error {
	if solarSystemID == 0 || count == 0 {
		return nil
	}
	if err := ensureMapHistoryTable(ctx, db); err != nil {
		return err
	}
	bucket := time.Now().Unix() / 3600
	_, err := db.ExecContext(ctx, `
		INSERT INTO mapHistory (solarSystemID, statType, hourBucket, count)
		VALUES (?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE count = count + VALUES(count)`,
		solarSystemID, statType, bucket, count)
	return err
}

// GetKillHistory returns a util.Rowset of (solarSystemID, value1, value2, value3) over the
// last N hours: value1=every ship destroyed, value2=factionKills, value3=podKills. value1 must
// include the faction kills, because map.py draws Ships Destroyed from `value1 - value2`.
func GetKillHistory(ctx context.Context, db *sql.DB, hours int64) (marshal.PyRep, error) {
	cols := []string{"solarSystemID", "value1", "value2", "value3"}
	if err := ensureMapHistoryTable(ctx, db); err != nil {
		return makeRowset(cols, nil), nil
	}
	minBucket := time.Now().Unix()/3600 - hours
	// SUM() is DECIMAL and would arrive as a PyString; map.py does arithmetic on these columns and
	// a string raises inside the star map's loading Cycle(). CAST keeps them integers.
	return queryRows(ctx, db, cols, `
		SELECT solarSystemID,
		       CAST(SUM(CASE WHEN statType IN (1, 3) THEN count ELSE 0 END) AS SIGNED) AS value1,
		       CAST(SUM(CASE WHEN statType = 3 THEN count ELSE 0 END) AS SIGNED) AS value2,
		       CAST(SUM(CASE WHEN statType = 2 THEN count ELSE 0 END) AS SIGNED) AS value3
		FROM mapHistory
		WHERE hourBucket > ? AND statType IN (1, 2, 3)
		GROUP BY solarSystemID`, minBucket)
}

// GetJumpHistory returns a util.Rowset of (solarSystemID, value1, value2, value3)
// where value1=jumps, value2=0, value3=0, aggregated over the last N hours.
func GetJumpHistory(ctx context.Context, db *sql.DB, hours int64) (marshal.PyRep, error) {
	cols := []string{"solarSystemID", "value1", "value2", "value3"}
	if err := ensureMapHistoryTable(ctx, db); err != nil {
		return makeRowset(cols, nil), nil
	}
	minBucket := time.Now().Unix()/3600 - hours
	return queryRows(ctx, db, cols, `
		SELECT solarSystemID,
		       CAST(SUM(count) AS SIGNED) AS value1,
		       0 AS value2,
		       0 AS value3
		FROM mapHistory
		WHERE hourBucket > ? AND statType = 4
		GROUP BY solarSystemID`, minBucket)
}

// PurgeOldMapHistory deletes buckets older than keepHours.
func PurgeOldMapHistory(ctx context.Context, db *sql.DB, keepHours int64) error {
	if err := ensureMapHistoryTable(ctx, db); err != nil {
		return err
	}
	cutoff := time.Now().Unix()/3600 - keepHours
	_, err := db.ExecContext(ctx, `DELETE FROM mapHistory WHERE hourBucket < ?`, cutoff)
	return err
}

// MakeHistoryRowset builds a util.Rowset with the history columns from
// pre-built rows. Used by the pilot-count overlay which reads from the
// session registry rather than the DB.
func MakeHistoryRowset(rows [][]marshal.PyRep) marshal.PyRep {
	return makeRowset([]string{"solarSystemID", "value1", "value2", "value3"}, rows)
}
