package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Persistent belt population. A belt's current wave is two tables: npcBeltWaves (one row per
// visited belt) and npcBeltRats (one row per rat of that wave). npc.RollWave rolls the wave once
// and writes it down. A loaded system materialises balls from the rows, and an unwatched bot
// reads and marks the same rows, so both deplete one belt. A belt gains rows on first request
// and keeps them until its wave is cleared and the respawn cooldown has run; nothing is seeded
// at boot.
// See docs/db-8-notes.md#persistent-belt-population.

// BeltWaveRow is one belt's current wave header.
type BeltWaveRow struct {
	CelestialID   int64
	SolarSystemID int64
	RegionID      int64
	Security      float64
	TierName      string
	SeededAt      int64 // unix seconds
	// LastClearAt is when the last rat of this wave died, or 0 while any are alive. It is the respawn
	// clock and the claim token; a wave that rolled empty is written already cleared.
	LastClearAt int64
}

// Cleared reports whether this wave has been wiped and is waiting out its cooldown.
func (w BeltWaveRow) Cleared() bool { return w.LastClearAt > 0 }

// BeltRatRow is one rat of a belt's wave, carrying what the spawner needs to build the ball and
// what the abstract tier needs to fight it without one.
type BeltRatRow struct {
	BeltRatID     int64
	CelestialID   int64
	SolarSystemID int64
	TypeID        int64
	Role          string
	ShipClass     string
	FactionID     int64
	CorporationID int64
	// Health carries across a system unload, so a belt a player half cleared stays half cleared.
	ShieldFrac    float64
	ArmorFrac     float64
	StructureFrac float64
	Alive         bool
	KilledAt      int64
	// EntityID is the materialised ball while the system is loaded, 0 otherwise. A death on the live
	// path finds its persistent row through it.
	EntityID uint32
}

// EnsureBeltWaveTables creates the persistent belt tables. Called from EnsureSpawnerTables.
func EnsureBeltWaveTables(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS npcBeltWaves (
			celestialID   BIGINT PRIMARY KEY,
			solarSystemID BIGINT NOT NULL,
			regionID      BIGINT NOT NULL DEFAULT 0,
			security      DOUBLE NOT NULL DEFAULT 0,
			tierName      VARCHAR(32) NOT NULL DEFAULT '',
			seededAt      BIGINT NOT NULL DEFAULT 0,
			lastClearAt   BIGINT NOT NULL DEFAULT 0,
			INDEX idx_system (solarSystemID),
			INDEX idx_clear (lastClearAt)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("EnsureBeltWaveTables npcBeltWaves: %w", err)
	}

	// No foreign key to npcBeltWaves (as with the killboard tables); children are deleted
	// explicitly. DeleteBeltRats runs before every path that drops or reseeds a wave.
	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS npcBeltRats (
			beltRatID     BIGINT PRIMARY KEY AUTO_INCREMENT,
			celestialID   BIGINT NOT NULL,
			solarSystemID BIGINT NOT NULL,
			typeID        BIGINT NOT NULL,
			role          VARCHAR(32) NOT NULL DEFAULT 'normal',
			shipClass     VARCHAR(32) NOT NULL DEFAULT '',
			factionID     BIGINT NOT NULL DEFAULT 0,
			corporationID BIGINT NOT NULL DEFAULT 0,
			shieldFrac    DOUBLE NOT NULL DEFAULT 1.0,
			armorFrac     DOUBLE NOT NULL DEFAULT 1.0,
			structureFrac DOUBLE NOT NULL DEFAULT 1.0,
			alive         TINYINT NOT NULL DEFAULT 1,
			killedAt      BIGINT NOT NULL DEFAULT 0,
			entityID      INT UNSIGNED NOT NULL DEFAULT 0,
			INDEX idx_belt (celestialID, alive),
			INDEX idx_system (solarSystemID),
			INDEX idx_entity (entityID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("EnsureBeltWaveTables npcBeltRats: %w", err)
	}

	// Repair belts with a live-looking header and no live rat: they are cleared, dated to their
	// roll. A no-op once every empty roll is stamped.
	if _, err := db.ExecContext(ctx, `
		UPDATE npcBeltWaves w SET w.lastClearAt = GREATEST(w.seededAt, 1)
		WHERE w.lastClearAt = 0
		  AND NOT EXISTS (SELECT 1 FROM npcBeltRats r
		                  WHERE r.celestialID = w.celestialID AND r.alive = 1)`); err != nil {
		return fmt.Errorf("EnsureBeltWaveTables repair empty waves: %w", err)
	}
	return nil
}

// ClearAllBeltRatEntities zeroes every materialised entityID. Called at boot beside
// TruncateLiveNPCs: the balls are gone but the rats persist.
func ClearAllBeltRatEntities(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `UPDATE npcBeltRats SET entityID = 0 WHERE entityID <> 0`)
	return err
}

// LoadBeltWave reads one belt's wave header and its rats (alive and dead, so a caller can tell an
// untouched belt from a wiped one). Returns nil, nil, nil when the belt was never visited.
func LoadBeltWave(ctx context.Context, db *sql.DB, celestialID int64) (*BeltWaveRow, []BeltRatRow, error) {
	w := &BeltWaveRow{}
	err := db.QueryRowContext(ctx, `
		SELECT celestialID, solarSystemID, regionID, security, tierName, seededAt, lastClearAt
		FROM npcBeltWaves WHERE celestialID = ?`, celestialID).Scan(
		&w.CelestialID, &w.SolarSystemID, &w.RegionID, &w.Security, &w.TierName,
		&w.SeededAt, &w.LastClearAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("LoadBeltWave(%d): %w", celestialID, err)
	}
	rats, err := loadBeltRats(ctx, db, celestialID)
	if err != nil {
		return nil, nil, err
	}
	return w, rats, nil
}

func loadBeltRats(ctx context.Context, db *sql.DB, celestialID int64) ([]BeltRatRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT beltRatID, celestialID, solarSystemID, typeID, role, shipClass,
		       factionID, corporationID, shieldFrac, armorFrac, structureFrac,
		       alive, killedAt, entityID
		FROM npcBeltRats WHERE celestialID = ? ORDER BY beltRatID`, celestialID)
	if err != nil {
		return nil, fmt.Errorf("loadBeltRats(%d): %w", celestialID, err)
	}
	defer rows.Close()
	var out []BeltRatRow
	for rows.Next() {
		var r BeltRatRow
		var alive int
		if err := rows.Scan(&r.BeltRatID, &r.CelestialID, &r.SolarSystemID, &r.TypeID,
			&r.Role, &r.ShipClass, &r.FactionID, &r.CorporationID,
			&r.ShieldFrac, &r.ArmorFrac, &r.StructureFrac, &alive, &r.KilledAt,
			&r.EntityID); err != nil {
			return nil, fmt.Errorf("loadBeltRats scan: %w", err)
		}
		r.Alive = alive != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// ClaimNewBeltWave writes a wave for a belt that has none and reports whether this caller won.
// False is not an error: the caller must hand back anything it took (an officer claim above all)
// and re-read. INSERT IGNORE on the primary key is the interlock; a loser's roll is discarded.
func ClaimNewBeltWave(ctx context.Context, db *sql.DB, w BeltWaveRow, rats []BeltRatRow) (bool, error) {
	res, err := db.ExecContext(ctx, `
		INSERT IGNORE INTO npcBeltWaves
			(celestialID, solarSystemID, regionID, security, tierName, seededAt, lastClearAt)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		w.CelestialID, w.SolarSystemID, w.RegionID, w.Security, w.TierName, w.SeededAt,
		w.LastClearAt)
	if err != nil {
		return false, fmt.Errorf("ClaimNewBeltWave(%d): %w", w.CelestialID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, nil
	}
	if err := insertBeltRats(ctx, db, w.CelestialID, w.SolarSystemID, rats); err != nil {
		return true, err
	}
	return true, nil
}

// ClaimBeltReseed refills a belt whose wave was cleared and whose cooldown has elapsed. The
// conditional UPDATE is the claim: one caller flips lastClearAt back to 0 and replaces the rats.
// clearedBefore is the newest lastClearAt still eligible (now - cooldown).
func ClaimBeltReseed(ctx context.Context, db *sql.DB, w BeltWaveRow, rats []BeltRatRow,
	clearedBefore int64) (bool, error) {

	res, err := db.ExecContext(ctx, `
		UPDATE npcBeltWaves
		SET seededAt = ?, lastClearAt = ?, tierName = ?, security = ?, regionID = ?
		WHERE celestialID = ? AND lastClearAt > 0 AND lastClearAt <= ?`,
		w.SeededAt, w.LastClearAt, w.TierName, w.Security, w.RegionID, w.CelestialID, clearedBefore)
	if err != nil {
		return false, fmt.Errorf("ClaimBeltReseed(%d): %w", w.CelestialID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, nil
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM npcBeltRats WHERE celestialID = ?`, w.CelestialID); err != nil {
		return true, fmt.Errorf("ClaimBeltReseed clear rats(%d): %w", w.CelestialID, err)
	}
	if err := insertBeltRats(ctx, db, w.CelestialID, w.SolarSystemID, rats); err != nil {
		return true, err
	}
	return true, nil
}

// insertBeltRats writes a wave's rats in one statement. A wave that rolled empty writes nothing;
// the header row alone records that the belt was rolled.
func insertBeltRats(ctx context.Context, db *sql.DB, celestialID, systemID int64, rats []BeltRatRow) error {
	if len(rats) == 0 {
		return nil
	}
	var sb strings.Builder
	sb.WriteString(`INSERT INTO npcBeltRats
		(celestialID, solarSystemID, typeID, role, shipClass,
		 factionID, corporationID, shieldFrac, armorFrac, structureFrac, alive, killedAt, entityID)
		VALUES `)
	args := make([]any, 0, len(rats)*10)
	for i, r := range rats {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString("(?,?,?,?,?,?,?,?,?,?,1,0,0)")
		role := r.Role
		if role == "" {
			role = "normal"
		}
		args = append(args, celestialID, systemID, r.TypeID, role, r.ShipClass,
			r.FactionID, r.CorporationID,
			clampFrac(r.ShieldFrac), clampFrac(r.ArmorFrac), clampFrac(r.StructureFrac))
	}
	if _, err := db.ExecContext(ctx, sb.String(), args...); err != nil {
		return fmt.Errorf("insertBeltRats(%d): %w", celestialID, err)
	}
	return nil
}

// clampFrac keeps a health fraction in [0,1]; a rat stored at 0% shield would land in space
// already dying.
func clampFrac(f float64) float64 {
	if f <= 0 {
		return 1
	}
	if f > 1 {
		return 1
	}
	return f
}

// BindBeltRatEntity records the ball a persisted rat was materialised as.
func BindBeltRatEntity(ctx context.Context, db *sql.DB, beltRatID int64, entityID uint32) error {
	_, err := db.ExecContext(ctx,
		`UPDATE npcBeltRats SET entityID = ? WHERE beltRatID = ?`, entityID, beltRatID)
	return err
}

// UnbindBeltRatsForSystem zeroes the materialised entity IDs of a system being torn down. The
// rats stay, since an unload is not a kill.
func UnbindBeltRatsForSystem(ctx context.Context, db *sql.DB, solarSystemID int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE npcBeltRats SET entityID = 0 WHERE solarSystemID = ? AND entityID <> 0`, solarSystemID)
	return err
}

// SaveBeltRatHealth persists a rat's damage. Called by the live path when a system unloads.
func SaveBeltRatHealth(ctx context.Context, db *sql.DB, beltRatID int64, shield, armor, structure float64) error {
	_, err := db.ExecContext(ctx, `
		UPDATE npcBeltRats SET shieldFrac = ?, armorFrac = ?, structureFrac = ?
		WHERE beltRatID = ?`, clampFrac(shield), clampFrac(armor), clampFrac(structure), beltRatID)
	return err
}

// KillBeltRat marks one rat dead and, when it was the last alive in that belt, stamps the wave's
// respawn clock. Both halves are one transaction, because nothing else sets lastClearAt. It
// reports whether this kill emptied the belt.
func KillBeltRat(ctx context.Context, db *sql.DB, beltRatID int64, when int64) (emptied bool, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("KillBeltRat begin: %w", err)
	}
	defer tx.Rollback()

	var celestialID int64
	if err := tx.QueryRowContext(ctx,
		`SELECT celestialID FROM npcBeltRats WHERE beltRatID = ? AND alive = 1`,
		beltRatID).Scan(&celestialID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil // already dead, or never existed
		}
		return false, fmt.Errorf("KillBeltRat lookup(%d): %w", beltRatID, err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE npcBeltRats SET alive = 0, killedAt = ?, entityID = 0, structureFrac = 0
		WHERE beltRatID = ?`, when, beltRatID); err != nil {
		return false, fmt.Errorf("KillBeltRat update(%d): %w", beltRatID, err)
	}

	var remaining int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM npcBeltRats WHERE celestialID = ? AND alive = 1`,
		celestialID).Scan(&remaining); err != nil {
		return false, fmt.Errorf("KillBeltRat count(%d): %w", celestialID, err)
	}
	if remaining == 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE npcBeltWaves SET lastClearAt = ? WHERE celestialID = ? AND lastClearAt = 0`,
			when, celestialID); err != nil {
			return false, fmt.Errorf("KillBeltRat stamp(%d): %w", celestialID, err)
		}
		emptied = true
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("KillBeltRat commit: %w", err)
	}
	return emptied, nil
}

// GetBeltRatByEntity finds the persistent row behind a materialised ball, with its belt's region
// (needed to settle the officer ledger). Returns nil when the ball is not a persisted belt rat
// (a dungeon NPC, a CONCORD response, a drone), which is not an error.
func GetBeltRatByEntity(ctx context.Context, db *sql.DB, entityID uint32) (*BeltRatRow, int64, error) {
	var r BeltRatRow
	var alive int
	var regionID int64
	err := db.QueryRowContext(ctx, `
		SELECT r.beltRatID, r.celestialID, r.solarSystemID, r.typeID, r.role, r.shipClass,
		       r.factionID, r.corporationID, r.shieldFrac, r.armorFrac,
		       r.structureFrac, r.alive, r.killedAt, r.entityID, COALESCE(w.regionID, 0)
		FROM npcBeltRats r
		LEFT JOIN npcBeltWaves w ON w.celestialID = r.celestialID
		WHERE r.entityID = ? AND r.alive = 1`, entityID).Scan(
		&r.BeltRatID, &r.CelestialID, &r.SolarSystemID, &r.TypeID, &r.Role, &r.ShipClass,
		&r.FactionID, &r.CorporationID, &r.ShieldFrac, &r.ArmorFrac,
		&r.StructureFrac, &alive, &r.KilledAt, &r.EntityID, &regionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("GetBeltRatByEntity(%d): %w", entityID, err)
	}
	r.Alive = alive != 0
	return &r, regionID, nil
}

// PurgeBeltWavesForRegion drops a region's persisted belts. Only an npcRegionSpawns edit calls
// it, since existing belts would otherwise never see the change.
func PurgeBeltWavesForRegion(ctx context.Context, db *sql.DB, regionID int64) (int64, error) {
	if _, err := db.ExecContext(ctx, `
		DELETE r FROM npcBeltRats r
		JOIN npcBeltWaves w ON w.celestialID = r.celestialID
		WHERE w.regionID = ?`, regionID); err != nil {
		return 0, fmt.Errorf("PurgeBeltWavesForRegion rats(%d): %w", regionID, err)
	}
	res, err := db.ExecContext(ctx,
		`DELETE FROM npcBeltWaves WHERE regionID = ?`, regionID)
	if err != nil {
		return 0, fmt.Errorf("PurgeBeltWavesForRegion(%d): %w", regionID, err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
