package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"time"
)

// ── Persistent asteroid belts ────────────────────────────────────────────────
//
// A belt field is one seed per system (asteroidFields); the rocks are regenerated from it and
// only mined quantities are stored (asteroidDepletion). See docs/db-6-notes.md#persistent-asteroid-belts.

const (
	settingAsteroidRegenDays   = "asteroids.regenDays"
	settingAsteroidRespawnMins = "asteroids.respawnMinutes"

	// DefaultAsteroidRegenDays is how long a field is honoured before it is re-rolled. With respawn
	// off it is the only regrowth, so 0 (never) leaves visited systems eroding. See docs/db-6-notes.md#persistent-asteroid-belts.
	DefaultAsteroidRegenDays = 1

	// DefaultAsteroidRespawnMinutes is 0 (per-rock respawn off). Above zero a depleted rock returns
	// after that many minutes. See docs/db-6-notes.md#persistent-asteroid-belts.
	DefaultAsteroidRespawnMinutes = 0
)

// AsteroidRegenDays is how many days a system's belt field is honoured before
// it is re-rolled. 0 disables regeneration entirely.
func AsteroidRegenDays(ctx context.Context, db *sql.DB) int {
	v, ok, err := GetServerSetting(ctx, db, settingAsteroidRegenDays)
	if err != nil || !ok {
		return DefaultAsteroidRegenDays
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return DefaultAsteroidRegenDays
	}
	return n
}

// SetAsteroidRegenDays writes the regeneration cycle from the admin panel.
func SetAsteroidRegenDays(ctx context.Context, db *sql.DB, days int) error {
	if days < 0 {
		days = 0
	}
	return SetServerSetting(ctx, db, settingAsteroidRegenDays, strconv.Itoa(days))
}

// AsteroidRespawnMinutes is how long a mined-out rock stays gone. 0 disables
// per-rock respawn, which is the shipped behaviour.
func AsteroidRespawnMinutes(ctx context.Context, db *sql.DB) int {
	v, ok, err := GetServerSetting(ctx, db, settingAsteroidRespawnMins)
	if err != nil || !ok {
		return DefaultAsteroidRespawnMinutes
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return DefaultAsteroidRespawnMinutes
	}
	return n
}

// SetAsteroidRespawnMinutes writes the respawn delay from the admin panel.
func SetAsteroidRespawnMinutes(ctx context.Context, db *sql.DB, mins int) error {
	if mins < 0 {
		mins = 0
	}
	return SetServerSetting(ctx, db, settingAsteroidRespawnMins, strconv.Itoa(mins))
}

// AsteroidGenKey is what a stored seed was rolled under. A change in any field makes the stored
// field stale (EnsureAsteroidSeed).
type AsteroidGenKey struct {
	GenVersion int
	Security   float64
	FactionID  int64
	BeltCount  int
}

// AsteroidFieldRow is one system's belt-field header.
type AsteroidFieldRow struct {
	SolarSystemID int64
	Seed          int64
	GenVersion    int
	GeneratedAt   int64 // unix seconds
	Security      float64
	FactionID     int64
	BeltCount     int
}

// AsteroidDepletionRow is one rock whose quantity differs from its seed's. Qty 0 is a
// tombstone: a missing row reads as full, so rows are removed only by a reseed.
type AsteroidDepletionRow struct {
	EntityID uint32
	BeltID   int64
	Qty      int64
}

// EnsureAsteroidTables creates the belt-field schema. Asteroid entity IDs are fixed offsets from
// destiny.AsteroidIDBase, so no boot-time truncate is needed.
func EnsureAsteroidTables(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS asteroidFields (
			solarSystemID BIGINT NOT NULL PRIMARY KEY,
			seed          BIGINT NOT NULL,
			genVersion    INT    NOT NULL DEFAULT 0,
			generatedAt   BIGINT NOT NULL DEFAULT 0,
			security      DOUBLE NOT NULL DEFAULT 0,
			factionID     BIGINT NOT NULL DEFAULT 0,
			beltCount     INT    NOT NULL DEFAULT 0,
			INDEX idx_generated (generatedAt)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`); err != nil {
		return fmt.Errorf("create asteroidFields: %w", err)
	}
	// The primary key is (solarSystemID, entityID), never entityID alone: asteroid IDs restart at
	// AsteroidIDBase in every system. No helper here takes a bare entityID.
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS asteroidDepletion (
			solarSystemID BIGINT       NOT NULL,
			entityID      INT UNSIGNED NOT NULL,
			beltID        BIGINT       NOT NULL DEFAULT 0,
			qty           BIGINT       NOT NULL DEFAULT 0,
			updatedAt     BIGINT       NOT NULL DEFAULT 0,
			PRIMARY KEY (solarSystemID, entityID),
			INDEX idx_belt (solarSystemID, beltID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`); err != nil {
		return fmt.Errorf("create asteroidDepletion: %w", err)
	}
	return nil
}

// beltFieldSelect mints one field row per belt-bearing system. security, factionID and beltCount
// must match registerUndock (GetAllSystemSecurities, the system/constellation/region COALESCE,
// the groupID 9 count) or every field fails its freshness test. RAND() is per row.
const beltFieldSelect = `
	SELECT s.solarSystemID,
	       CAST(FLOOR(RAND() * 9223372036854775807) AS SIGNED),
	       ?, ?,
	       s.security,
	       COALESCE(s.factionID, c.factionID, r.factionID, 0),
	       b.belts
	  FROM mapSolarSystems s
	  LEFT JOIN mapConstellations c ON c.constellationID = s.constellationID
	  LEFT JOIN mapRegions r ON r.regionID = s.regionID
	  JOIN (SELECT solarSystemID, COUNT(*) AS belts
	          FROM mapDenormalize WHERE groupID = 9
	         GROUP BY solarSystemID) b ON b.solarSystemID = s.solarSystemID`

// SeedAllAsteroidFields gives every belt-bearing system a field in one INSERT IGNORE. It is safe
// at every boot and adds systems that gained a first belt.
func SeedAllAsteroidFields(ctx context.Context, db *sql.DB, genVersion int) (int64, error) {
	res, err := db.ExecContext(ctx,
		"INSERT IGNORE INTO asteroidFields "+
			"(solarSystemID, seed, genVersion, generatedAt, security, factionID, beltCount) "+
			beltFieldSelect,
		genVersion, time.Now().Unix())
	if err != nil {
		return 0, fmt.Errorf("SeedAllAsteroidFields: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ReseedExpiredAsteroidFields re-rolls every field older than ttl and deletes its depletion in
// one transaction. skip lists loaded systems, which keep their field until a later sweep.
// ttl <= 0 means regeneration is off.
func ReseedExpiredAsteroidFields(ctx context.Context, db *sql.DB, ttl time.Duration, skip []int64) (int64, error) {
	if ttl <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-ttl).Unix()
	where := "generatedAt < ?"
	args := []any{time.Now().Unix(), cutoff}
	if len(skip) > 0 {
		where += " AND solarSystemID NOT IN (" + placeholders(len(skip)) + ")"
		for _, id := range skip {
			args = append(args, id)
		}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// Depletion goes first: the join selects fields by their old generatedAt.
	delWhere := "f.generatedAt < ?"
	delArgs := []any{cutoff}
	if len(skip) > 0 {
		delWhere += " AND f.solarSystemID NOT IN (" + placeholders(len(skip)) + ")"
		for _, id := range skip {
			delArgs = append(delArgs, id)
		}
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE d FROM asteroidDepletion d JOIN asteroidFields f "+
			"ON f.solarSystemID = d.solarSystemID WHERE "+delWhere,
		delArgs...); err != nil {
		return 0, fmt.Errorf("ReseedExpiredAsteroidFields depletion: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		"UPDATE asteroidFields SET seed = CAST(FLOOR(RAND() * 9223372036854775807) AS SIGNED), "+
			"generatedAt = ? WHERE "+where, args...)
	if err != nil {
		return 0, fmt.Errorf("ReseedExpiredAsteroidFields: %w", err)
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// ReseedStaleAsteroidFields re-rolls fields stored under another generator version and clears
// their depletion in one transaction. The rolebots abstract tier rebuilds unloaded fields from the
// stored seed, so run it at boot before anything loads.
func ReseedStaleAsteroidFields(ctx context.Context, db *sql.DB, genVersion int) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		"DELETE d FROM asteroidDepletion d JOIN asteroidFields f "+
			"ON f.solarSystemID = d.solarSystemID WHERE f.genVersion <> ?",
		genVersion); err != nil {
		return 0, fmt.Errorf("ReseedStaleAsteroidFields depletion: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		"UPDATE asteroidFields SET seed = CAST(FLOOR(RAND() * 9223372036854775807) AS SIGNED), "+
			"genVersion = ?, generatedAt = ? WHERE genVersion <> ?",
		genVersion, time.Now().Unix(), genVersion)
	if err != nil {
		return 0, fmt.Errorf("ReseedStaleAsteroidFields: %w", err)
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// randomAsteroidSeed mints a seed; kept here so db does not import destiny.
func randomAsteroidSeed() int64 {
	return rand.New(rand.NewSource(time.Now().UnixNano())).Int63()
}

// EnsureAsteroidSeed returns the system's seed, minting one when there is no row or the stored
// key or age no longer matches. A reseed clears the system's depletion in the same transaction.
// The system-manager lock serialises callers per system.
func EnsureAsteroidSeed(ctx context.Context, db *sql.DB, systemID int64,
	gen AsteroidGenKey, ttl time.Duration) (seed int64, reseeded bool, generatedAt int64, err error) {

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, 0, err
	}
	defer tx.Rollback()

	var (
		haveRow  bool
		row      AsteroidFieldRow
		scanSeed sql.NullInt64
	)
	err = tx.QueryRowContext(ctx, `
		SELECT seed, genVersion, generatedAt, security, factionID, beltCount
		  FROM asteroidFields WHERE solarSystemID = ?`, systemID).
		Scan(&scanSeed, &row.GenVersion, &row.GeneratedAt, &row.Security, &row.FactionID, &row.BeltCount)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		err = nil
	case err != nil:
		return 0, false, 0, err
	default:
		haveRow = scanSeed.Valid
		row.Seed = scanSeed.Int64
	}

	now := time.Now().Unix()
	fresh := haveRow &&
		row.GenVersion == gen.GenVersion &&
		row.FactionID == gen.FactionID &&
		row.BeltCount == gen.BeltCount &&
		sameSecurity(row.Security, gen.Security) &&
		(ttl <= 0 || now-row.GeneratedAt < int64(ttl.Seconds()))

	if fresh {
		return row.Seed, false, row.GeneratedAt, tx.Commit()
	}

	seed = randomAsteroidSeed()
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO asteroidFields
			(solarSystemID, seed, genVersion, generatedAt, security, factionID, beltCount)
		VALUES (?,?,?,?,?,?,?)
		ON DUPLICATE KEY UPDATE
			seed = VALUES(seed), genVersion = VALUES(genVersion),
			generatedAt = VALUES(generatedAt), security = VALUES(security),
			factionID = VALUES(factionID), beltCount = VALUES(beltCount)`,
		systemID, seed, gen.GenVersion, now, gen.Security, gen.FactionID, gen.BeltCount); err != nil {
		return 0, false, 0, err
	}
	if _, err = tx.ExecContext(ctx,
		`DELETE FROM asteroidDepletion WHERE solarSystemID = ?`, systemID); err != nil {
		return 0, false, 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, false, 0, err
	}
	return seed, true, now, nil
}

// sameSecurity compares with a tolerance: the column is a DOUBLE round trip and an exact test
// would reseed on float noise.
func sameSecurity(a, b float64) bool {
	d := a - b
	return d < 1e-6 && d > -1e-6
}

// LoadAsteroidDepletion returns every rock in the system whose quantity differs
// from what the seed produces, as entityID → units remaining.
func LoadAsteroidDepletion(ctx context.Context, db *sql.DB, systemID int64) (map[uint32]int64, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT entityID, qty FROM asteroidDepletion WHERE solarSystemID = ?`, systemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[uint32]int64)
	for rows.Next() {
		var id uint32
		var qty int64
		if err := rows.Scan(&id, &qty); err != nil {
			return nil, err
		}
		out[id] = qty
	}
	return out, rows.Err()
}

// FlushAsteroidDepletion writes back mined quantities in one batched upsert.
// Called from the flush tick and, mandatorily, when a system unloads.
func FlushAsteroidDepletion(ctx context.Context, db *sql.DB, systemID int64, rows []AsteroidDepletionRow) error {
	if len(rows) == 0 {
		return nil
	}
	now := time.Now().Unix()
	const chunk = 500
	for start := 0; start < len(rows); start += chunk {
		end := start + chunk
		if end > len(rows) {
			end = len(rows)
		}
		batch := rows[start:end]
		var sb strings.Builder
		sb.WriteString("INSERT INTO asteroidDepletion (solarSystemID, entityID, beltID, qty, updatedAt) VALUES ")
		args := make([]any, 0, len(batch)*5)
		for i, r := range batch {
			if i > 0 {
				sb.WriteString(",")
			}
			sb.WriteString("(?,?,?,?,?)")
			qty := r.Qty
			if qty < 0 {
				qty = 0
			}
			args = append(args, systemID, r.EntityID, r.BeltID, qty, now)
		}
		sb.WriteString(" ON DUPLICATE KEY UPDATE beltID = VALUES(beltID), qty = VALUES(qty), updatedAt = VALUES(updatedAt)")
		if _, err := db.ExecContext(ctx, sb.String(), args...); err != nil {
			return err
		}
	}
	return nil
}

// ReseedAllAsteroids re-rolls every system's seed and clears all depletion (the ESP button).
// Loaded systems keep the field their players are on until they empty.
func ReseedAllAsteroids(ctx context.Context, db *sql.DB, genVersion int) (int64, error) {
	// Seed first so a system with no row is covered by the UPDATE below.
	if _, err := SeedAllAsteroidFields(ctx, db, genVersion); err != nil {
		return 0, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// RAND() is per row, so each system gets its own seed.
	res, err := tx.ExecContext(ctx, `
		UPDATE asteroidFields
		   SET seed = CAST(FLOOR(RAND() * 9223372036854775807) AS SIGNED),
		       generatedAt = ?`, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx, `DELETE FROM asteroidDepletion`); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// ReseedAsteroidField re-rolls one system's seed and clears its depletion.
func ReseedAsteroidField(ctx context.Context, db *sql.DB, systemID int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE asteroidFields SET seed = ?, generatedAt = ? WHERE solarSystemID = ?`,
		randomAsteroidSeed(), time.Now().Unix(), systemID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM asteroidDepletion WHERE solarSystemID = ?`, systemID); err != nil {
		return err
	}
	return tx.Commit()
}

// AsteroidStats is the belt page's status panel.
type AsteroidStats struct {
	Fields        int64
	Expired       int64
	DepletionRows int64
	MinedOut      int64
	Oldest        int64 // unix seconds, 0 when there are no fields
	Newest        int64
}

// GetAsteroidStats reads the counters behind the ESP status panel. regenDays 0
// (never regenerate) reports nothing expired, because nothing can be.
func GetAsteroidStats(ctx context.Context, db *sql.DB, regenDays int) (AsteroidStats, error) {
	var st AsteroidStats
	var oldest, newest sql.NullInt64
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*), MIN(generatedAt), MAX(generatedAt) FROM asteroidFields`).
		Scan(&st.Fields, &oldest, &newest)
	if err != nil {
		return st, err
	}
	st.Oldest, st.Newest = oldest.Int64, newest.Int64
	if regenDays > 0 {
		cutoff := time.Now().Add(-time.Duration(regenDays) * 24 * time.Hour).Unix()
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM asteroidFields WHERE generatedAt < ?`, cutoff).
			Scan(&st.Expired); err != nil {
			return st, err
		}
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(qty = 0), 0) FROM asteroidDepletion`).
		Scan(&st.DepletionRows, &st.MinedOut); err != nil {
		return st, err
	}
	return st, nil
}
