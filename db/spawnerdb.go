package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"
)

// EnsureSpawnerTables creates the NPC spawner tables if they do not exist.
// Call once at server startup alongside EnsureDungeonTables.
func EnsureSpawnerTables(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS npcSpawnTemplates (
			templateID    INT PRIMARY KEY AUTO_INCREMENT,
			templateName  VARCHAR(128) NOT NULL,
			factionID     INT NOT NULL DEFAULT 0,
			corporationID INT NOT NULL DEFAULT 0,
			description   VARCHAR(255) DEFAULT NULL
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("EnsureSpawnerTables npcSpawnTemplates: %w", err)
	}
	// Add corporationID column if upgrading from older schema.
	db.ExecContext(ctx, `ALTER TABLE npcSpawnTemplates ADD COLUMN corporationID INT NOT NULL DEFAULT 0 AFTER factionID`)

	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS npcSpawnEntries (
			entryID       INT PRIMARY KEY AUTO_INCREMENT,
			templateID    INT NOT NULL,
			typeID        INT NOT NULL,
			minCount      INT NOT NULL DEFAULT 1,
			maxCount      INT NOT NULL DEFAULT 1,
			weight        INT NOT NULL DEFAULT 1
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("EnsureSpawnerTables npcSpawnEntries: %w", err)
	}

	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS npcSpawners (
			spawnerID       INT PRIMARY KEY AUTO_INCREMENT,
			solarSystemID   INT NOT NULL DEFAULT 0,
			celestialGroup  INT NOT NULL DEFAULT 9,
			templateID      INT NOT NULL,
			factionID       INT NOT NULL,
			corporationID   INT NOT NULL DEFAULT 0,
			maxNPCs         INT NOT NULL DEFAULT 3,
			respawnSec      INT NOT NULL DEFAULT 300,
			triggerRange    DOUBLE NOT NULL DEFAULT 0,
			scatterRadius   DOUBLE NOT NULL DEFAULT 20000
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("EnsureSpawnerTables npcSpawners: %w", err)
	}
	// Add corporationID column if upgrading from older schema.
	db.ExecContext(ctx, `ALTER TABLE npcSpawners ADD COLUMN corporationID INT NOT NULL DEFAULT 0 AFTER factionID`)

	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS npcFactionZones (
			zoneID          INT PRIMARY KEY AUTO_INCREMENT,
			regionID        INT NOT NULL DEFAULT 0,
			constellationID INT NOT NULL DEFAULT 0,
			factionID       INT NOT NULL,
			templateID      INT NOT NULL DEFAULT 0
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("EnsureSpawnerTables npcFactionZones: %w", err)
	}

	// Legacy: retired per-hull spawn-class rungs, read only by migrateSpawnerRules to carry
	// tuning into npcSpawnerRules.
	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS npcSpawnClasses (
			classID            INT PRIMARY KEY AUTO_INCREMENT,
			className          VARCHAR(128) NOT NULL,
			secMin             DOUBLE NOT NULL DEFAULT -1.0,
			secMax             DOUBLE NOT NULL DEFAULT 1.0,
			frigateCount       INT NOT NULL DEFAULT 0,
			destroyerCount     INT NOT NULL DEFAULT 0,
			cruiserCount       INT NOT NULL DEFAULT 0,
			battlecruiserCount INT NOT NULL DEFAULT 0,
			battleshipCount    INT NOT NULL DEFAULT 0,
			description        VARCHAR(255) DEFAULT NULL
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("EnsureSpawnerTables npcSpawnClasses: %w", err)
	}
	db.ExecContext(ctx, `ALTER TABLE npcSpawnClasses ADD COLUMN maxShips INT NOT NULL DEFAULT 0`)

	db.ExecContext(ctx, `ALTER TABLE npcSpawnEntries ADD COLUMN role VARCHAR(32) NOT NULL DEFAULT 'normal'`)
	db.ExecContext(ctx, `ALTER TABLE npcSpawnEntries ADD COLUMN shipClass VARCHAR(32) NOT NULL DEFAULT 'frigate'`)
	db.ExecContext(ctx, `ALTER TABLE npcSpawnTemplates ADD COLUMN spawnClassID INT NOT NULL DEFAULT 0`)
	db.ExecContext(ctx, `ALTER TABLE npcSpawners ADD COLUMN warpIn TINYINT NOT NULL DEFAULT 1`)
	// fullClearDelaySec (belt reseed cooldown), scatterRadius and warpIn are the npcSpawners
	// columns still read; special-spawn chances live in npcSquadTiers.
	db.ExecContext(ctx, `ALTER TABLE npcSpawners ADD COLUMN fullClearDelaySec INT NOT NULL DEFAULT 300`)

	// Spawner rules: how many ships a belt wave holds at a given security. What can spawn
	// comes from the region's list.
	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS npcSpawnerRules (
			ruleID      INT PRIMARY KEY AUTO_INCREMENT,
			ruleName    VARCHAR(128) NOT NULL,
			secMin      DOUBLE NOT NULL DEFAULT -1.0,
			secMax      DOUBLE NOT NULL DEFAULT 1.0,
			minShips    INT NOT NULL DEFAULT 0,
			maxShips    INT NOT NULL DEFAULT 3,
			description VARCHAR(255) DEFAULT NULL
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("EnsureSpawnerTables npcSpawnerRules: %w", err)
	}

	if err := migrateSpawnerRules(ctx, db); err != nil {
		return fmt.Errorf("EnsureSpawnerTables spawner-rule migration: %w", err)
	}

	if err := ensureEwarBanRules(ctx, db); err != nil {
		return fmt.Errorf("EnsureSpawnerTables: %w", err)
	}

	// Migrate existing entries to use shipClass based on weight.
	if _, err := db.ExecContext(ctx, `UPDATE npcSpawnEntries SET shipClass='frigate' WHERE shipClass='frigate' AND weight >= 3`); err != nil {
		log.Printf("[db] EnsureSpawnerTables: best-effort write failed: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE npcSpawnEntries SET shipClass='destroyer' WHERE shipClass='frigate' AND weight = 2`); err != nil {
		log.Printf("[db] EnsureSpawnerTables: best-effort write failed: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE npcSpawnEntries SET shipClass='cruiser' WHERE shipClass='frigate' AND weight = 1`); err != nil {
		log.Printf("[db] EnsureSpawnerTables: best-effort write failed: %v", err)
	}

	// Legacy spawn-pool tables, read only as a migration source.
	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS npcSpawnPools (
			poolID      INT PRIMARY KEY AUTO_INCREMENT,
			poolName    VARCHAR(128) NOT NULL,
			description VARCHAR(255) DEFAULT NULL
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("EnsureSpawnerTables npcSpawnPools: %w", err)
	}
	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS npcSpawnPoolEntries (
			poolEntryID INT PRIMARY KEY AUTO_INCREMENT,
			poolID      INT NOT NULL,
			templateID  INT NOT NULL,
			weight      INT NOT NULL DEFAULT 1,
			secMin      DOUBLE NOT NULL DEFAULT -1.0,
			secMax      DOUBLE NOT NULL DEFAULT 1.0,
			INDEX idx_pool (poolID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("EnsureSpawnerTables npcSpawnPoolEntries: %w", err)
	}
	db.ExecContext(ctx, `ALTER TABLE npcFactionZones ADD COLUMN poolID INT NOT NULL DEFAULT 0`)
	db.ExecContext(ctx, `ALTER TABLE npcSpawners ADD COLUMN poolID INT NOT NULL DEFAULT 0`)

	// Region spawns: the belt-spawn table. One row is one NPC type allowed in a region's belts,
	// from maxSec security downward, with its faction and corp.
	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS npcRegionSpawns (
			spawnID       INT PRIMARY KEY AUTO_INCREMENT,
			regionID      INT NOT NULL,
			typeID        INT NOT NULL,
			maxSec        DOUBLE NOT NULL DEFAULT 1.0,
			weight        INT NOT NULL DEFAULT 1,
			shipClass     VARCHAR(32) NOT NULL DEFAULT 'frigate',
			role          VARCHAR(32) NOT NULL DEFAULT 'normal',
			factionID     INT NOT NULL DEFAULT 0,
			corporationID INT NOT NULL DEFAULT 0,
			INDEX idx_region (regionID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("EnsureSpawnerTables npcRegionSpawns: %w", err)
	}

	if err := migrateRegionSpawns(ctx, db); err != nil {
		return fmt.Errorf("EnsureSpawnerTables region-spawn migration: %w", err)
	}

	// Logged, not returned: EnsureSpawnerTables is fatal at boot and this repair must not stop
	// the server. The marker is written only on success, so it retries on the next boot.
	if err := migrateOfficerMaxSec(ctx, db); err != nil {
		log.Printf("[spawner] officer maxSec repair skipped: %v — "+
			"reimport sql/npcregionspawns.sql, or officers can spawn in high-sec", err)
	}

	// Logged, not returned, for the same reason. Runs after the officer repair, which matches no
	// drone row.
	if err := migrateRogueDroneSpawns(ctx, db); err != nil {
		log.Printf("[spawner] rogue drone belt seed skipped: %v — "+
			"reimport sql/npcregionspawns.sql, or drone and Jove belts stay empty", err)
	}

	// Squad tiers: how hard a belt is, rolled per wave (npcSpawnerRules gives its size). One
	// row per (tier, security band). secFloor filters the base pool at a floor, not an offset,
	// and never applies to the commander or officer pools. See docs/db-notes.md#squad-tiers-and-the-officer-ledger.
	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS npcSquadTiers (
			tierID           INT PRIMARY KEY AUTO_INCREMENT,
			tierName         VARCHAR(32) NOT NULL,
			secMin           DOUBLE NOT NULL DEFAULT -1.0,
			secMax           DOUBLE NOT NULL DEFAULT 1.1,
			weight           INT NOT NULL DEFAULT 1,
			sizeDeltaMin     INT NOT NULL DEFAULT 0,
			sizeDeltaMax     INT NOT NULL DEFAULT 0,
			secFloor         DOUBLE NOT NULL DEFAULT 0,
			cmdrChancePct    DOUBLE NOT NULL DEFAULT 0,
			officerChancePct DOUBLE NOT NULL DEFAULT 0,
			hullBias         VARCHAR(64) NOT NULL DEFAULT '',
			description      VARCHAR(255) DEFAULT NULL,
			INDEX idx_band (secMin, secMax)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("EnsureSpawnerTables npcSquadTiers: %w", err)
	}

	if err := migrateSquadTiers(ctx, db); err != nil {
		return fmt.Errorf("EnsureSpawnerTables squad-tier migration: %w", err)
	}

	// Officer ledger: the per-region cap on named officers, keyed on regionID alone. liveCount
	// is volatile and lastKillAt durable. See docs/db-notes.md#squad-tiers-and-the-officer-ledger.
	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS npcOfficerLedger (
			regionID      INT NOT NULL,
			liveCount     INT NOT NULL DEFAULT 0,
			lastSpawnAt   BIGINT NOT NULL DEFAULT 0,
			lastKillAt    BIGINT NOT NULL DEFAULT 0,
			lastTypeID    INT NOT NULL DEFAULT 0,
			lastSystemID  INT NOT NULL DEFAULT 0,
			lastFactionID INT NOT NULL DEFAULT 0,
			PRIMARY KEY (regionID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("EnsureSpawnerTables npcOfficerLedger: %w", err)
	}

	// Runtime NPC census of all living NPCs. Truncated on restart.
	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS npcLiveEntities (
			entityID       INT UNSIGNED PRIMARY KEY,
			typeID         INT NOT NULL,
			typeName       VARCHAR(128) NOT NULL DEFAULT '',
			solarSystemID  INT NOT NULL,
			celestialID    INT NOT NULL DEFAULT 0,
			spawnerID      INT NOT NULL DEFAULT 0,
			posX           DOUBLE NOT NULL DEFAULT 0,
			posY           DOUBLE NOT NULL DEFAULT 0,
			posZ           DOUBLE NOT NULL DEFAULT 0,
			shieldFrac     DOUBLE NOT NULL DEFAULT 1.0,
			armorFrac      DOUBLE NOT NULL DEFAULT 1.0,
			structureFrac  DOUBLE NOT NULL DEFAULT 1.0,
			factionID      INT NOT NULL DEFAULT 0,
			corpID         INT NOT NULL DEFAULT 0,
			bounty         DOUBLE NOT NULL DEFAULT 0,
			spawnedAt      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updatedAt      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			INDEX idx_system (solarSystemID),
			INDEX idx_celestial (solarSystemID, celestialID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("EnsureSpawnerTables npcLiveEntities: %w", err)
	}

	// Add groupID/categoryID columns if upgrading from older schema.
	db.ExecContext(ctx, `ALTER TABLE npcLiveEntities ADD COLUMN groupID INT NOT NULL DEFAULT 25`)
	db.ExecContext(ctx, `ALTER TABLE npcLiveEntities ADD COLUMN categoryID INT NOT NULL DEFAULT 6`)

	// Persistent belt population (beltwavedb.go): it outlives a system unload and a restart,
	// unlike npcLiveEntities.
	if err := EnsureBeltWaveTables(ctx, db); err != nil {
		return err
	}

	return nil
}

// migrateSpawnerRules seeds npcSpawnerRules once: each retired npcSpawnClasses rung's wave
// size becomes the rule's ship cap, else a default ladder. Runs only while the table is
// empty so admin edits stick.
func migrateSpawnerRules(ctx context.Context, db *sql.DB) error {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM npcSpawnerRules`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}

	rows, err := db.QueryContext(ctx, `
		SELECT className, secMin, secMax,
		       frigateCount + destroyerCount + cruiserCount + battlecruiserCount + battleshipCount,
		       COALESCE(maxShips, 0), COALESCE(description, '')
		FROM npcSpawnClasses ORDER BY secMin DESC`)
	if err == nil {
		defer rows.Close()
		migrated := 0
		for rows.Next() {
			var name, desc string
			var secMin, secMax float64
			var hullSum, cap int
			if err := rows.Scan(&name, &secMin, &secMax, &hullSum, &cap, &desc); err != nil {
				return err
			}
			// The rung's own cap wins when set, otherwise the sum of its per-hull maximums.
			maxShips := cap
			if maxShips <= 0 {
				maxShips = hullSum
			}
			if _, err := db.ExecContext(ctx,
				`INSERT INTO npcSpawnerRules (ruleName, secMin, secMax, minShips, maxShips, description) VALUES (?, ?, ?, 0, ?, ?)`,
				name, secMin, secMax, maxShips, desc); err != nil {
				return err
			}
			migrated++
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if migrated > 0 {
			return nil
		}
	}

	// Nothing to migrate: seed the default ladder.
	defaults := []struct {
		name               string
		secMin, secMax     float64
		minShips, maxShips int
		desc               string
	}{
		{"High-sec Safe", 0.9, 1.1, 0, 2, "Safe high-sec belts (0.9-1.0)"},
		{"High-sec", 0.7, 0.9, 1, 3, "Standard high-sec belts (0.7-0.9)"},
		{"Mid-sec", 0.5, 0.7, 1, 4, "Mid-security belts (0.5-0.7)"},
		{"Low-sec", 0.1, 0.5, 1, 5, "Low-sec belts (0.1-0.5)"},
		{"Null-sec", -1.0, 0.1, 2, 7, "Null-sec belts (-1.0-0.1)"},
	}
	for _, d := range defaults {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO npcSpawnerRules (ruleName, secMin, secMax, minShips, maxShips, description) VALUES (?, ?, ?, ?, ?, ?)`,
			d.name, d.secMin, d.secMax, d.minShips, d.maxShips, d.desc); err != nil {
			return err
		}
	}
	return nil
}

// migrateRegionSpawns seeds npcRegionSpawns once from the legacy zone, pool and template
// data: each region zone becomes flat per-type rows with the template's faction and corp.
// Runs only while npcRegionSpawns is empty so admin edits stick.
func migrateRegionSpawns(ctx context.Context, db *sql.DB) error {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM npcRegionSpawns`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}

	// Region-level zones only; the flat model has no constellation granularity.
	zrows, err := db.QueryContext(ctx, `
		SELECT regionID, factionID, COALESCE(poolID, 0), templateID
		FROM npcFactionZones WHERE regionID != 0 AND constellationID = 0`)
	if err != nil {
		return err
	}
	type zone struct {
		regionID, factionID, poolID, templateID int64
	}
	var zones []zone
	for zrows.Next() {
		var z zone
		if err := zrows.Scan(&z.regionID, &z.factionID, &z.poolID, &z.templateID); err != nil {
			zrows.Close()
			return err
		}
		zones = append(zones, z)
	}
	zrows.Close()
	if err := zrows.Err(); err != nil {
		return err
	}

	for _, z := range zones {
		// Resolve the zone's templates: pool entries, else the bare template, else the faction's
		// lowest templateID.
		templateIDs := map[int64]int{} // templateID -> pool weight multiplier
		if z.poolID > 0 {
			prows, err := db.QueryContext(ctx,
				`SELECT templateID, weight FROM npcSpawnPoolEntries WHERE poolID = ?`, z.poolID)
			if err == nil {
				for prows.Next() {
					var tid int64
					var w int
					if prows.Scan(&tid, &w) == nil {
						if w < 1 {
							w = 1
						}
						templateIDs[tid] = w
					}
				}
				prows.Close()
			}
		}
		if len(templateIDs) == 0 && z.templateID > 0 {
			templateIDs[z.templateID] = 1
		}
		if len(templateIDs) == 0 {
			var tid int64
			_ = db.QueryRowContext(ctx,
				`SELECT templateID FROM npcSpawnTemplates WHERE factionID = ? ORDER BY templateID LIMIT 1`,
				z.factionID).Scan(&tid)
			if tid > 0 {
				templateIDs[tid] = 1
			}
		}

		seen := map[int64]bool{} // typeID dedupe within the region
		for tid, poolWeight := range templateIDs {
			var facID, corpID int64
			if err := db.QueryRowContext(ctx,
				`SELECT factionID, COALESCE(corporationID, 0) FROM npcSpawnTemplates WHERE templateID = ?`,
				tid).Scan(&facID, &corpID); err != nil {
				continue
			}
			if facID == 0 {
				facID = z.factionID
			}
			erows, err := db.QueryContext(ctx, `
				SELECT typeID, weight, COALESCE(shipClass, 'frigate'),
				       COALESCE(role, 'normal')
				FROM npcSpawnEntries WHERE templateID = ?`, tid)
			if err != nil {
				continue
			}
			for erows.Next() {
				var typeID int64
				var weight int
				var shipClass, role string
				if erows.Scan(&typeID, &weight, &shipClass, &role) != nil {
					continue
				}
				if seen[typeID] {
					continue
				}
				seen[typeID] = true
				if weight < 1 {
					weight = 1
				}
				if _, err := db.ExecContext(ctx, `
					INSERT INTO npcRegionSpawns
						(regionID, typeID, maxSec, weight, shipClass, role, factionID, corporationID)
					VALUES (?, ?, 1.0, ?, ?, ?, ?, ?)`,
					z.regionID, typeID, weight*poolWeight, shipClass, role, facID, corpID); err != nil {
					erows.Close()
					return err
				}
			}
			erows.Close()
		}
	}
	return nil
}

// migrateSquadTiers seeds npcSquadTiers from DefaultSquadTiers once, while the table is
// empty so admin edits stick.
func migrateSquadTiers(ctx context.Context, db *sql.DB) error {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM npcSquadTiers`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}

	for _, d := range DefaultSquadTiers() {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO npcSquadTiers
				(tierName, secMin, secMax, weight, sizeDeltaMin, sizeDeltaMax,
				 secFloor, cmdrChancePct, officerChancePct, hullBias, description)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			d.TierName, d.SecMin, d.SecMax, d.Weight, d.SizeDeltaMin, d.SizeDeltaMax,
			d.SecFloor, d.CmdrChancePct, d.OfficerChancePct, d.HullBias, d.Description); err != nil {
			return err
		}
	}
	return nil
}

// DefaultSquadTiers is the shipped tier ladder, shared by the seeder and the tests that
// assert on the resulting rates. Weights are relative within a band and sum to 100. Every
// easy row has secFloor 0.9: an easy belt is a high-sec-grade belt wherever it is.
func DefaultSquadTiers() []SquadTierRow {
	return []SquadTierRow{
		// 0.9 - 1.0. No hard or deadly row: a tier that can never be drawn is a missing row, not a
		// zero weight.
		{TierName: "easy", SecMin: 0.9, SecMax: 1.1, Weight: 30, SizeDeltaMax: -1, SecFloor: 0.9,
			Description: "A quiet high-sec belt"},
		{TierName: "normal", SecMin: 0.9, SecMax: 1.1, Weight: 70,
			Description: "An ordinary high-sec belt"},

		// 0.7 - 0.9.
		{TierName: "easy", SecMin: 0.7, SecMax: 0.9, Weight: 25, SizeDeltaMax: -1, SecFloor: 0.9,
			Description: "A quiet belt"},
		{TierName: "normal", SecMin: 0.7, SecMax: 0.9, Weight: 65, CmdrChancePct: 6,
			Description: "An ordinary belt"},
		{TierName: "hard", SecMin: 0.7, SecMax: 0.9, Weight: 10, SizeDeltaMin: 1, SizeDeltaMax: 1,
			CmdrChancePct: 20, HullBias: "cruiser", Description: "A dangerous belt"},

		// 0.5 - 0.7.
		{TierName: "easy", SecMin: 0.5, SecMax: 0.7, Weight: 20, SizeDeltaMax: -1, SecFloor: 0.9,
			Description: "A quiet belt"},
		{TierName: "normal", SecMin: 0.5, SecMax: 0.7, Weight: 62, CmdrChancePct: 8,
			Description: "An ordinary belt"},
		{TierName: "hard", SecMin: 0.5, SecMax: 0.7, Weight: 16, SizeDeltaMin: 1, SizeDeltaMax: 2,
			CmdrChancePct: 22, HullBias: "cruiser,battlecruiser", Description: "A dangerous belt"},
		{TierName: "deadly", SecMin: 0.5, SecMax: 0.7, Weight: 2, SizeDeltaMin: 2, SizeDeltaMax: 3,
			CmdrChancePct: 40, HullBias: "battlecruiser,battleship", Description: "A killing field"},

		// 0.1 - 0.5.
		{TierName: "easy", SecMin: 0.1, SecMax: 0.5, Weight: 15, SizeDeltaMax: -1, SecFloor: 0.9,
			Description: "A quiet belt"},
		{TierName: "normal", SecMin: 0.1, SecMax: 0.5, Weight: 57, CmdrChancePct: 9,
			Description: "An ordinary belt"},
		{TierName: "hard", SecMin: 0.1, SecMax: 0.5, Weight: 23, SizeDeltaMin: 1, SizeDeltaMax: 2,
			CmdrChancePct: 24, HullBias: "cruiser,battlecruiser", Description: "A dangerous belt"},
		{TierName: "deadly", SecMin: 0.1, SecMax: 0.5, Weight: 5, SizeDeltaMin: 2, SizeDeltaMax: 3,
			CmdrChancePct: 42, HullBias: "battlecruiser,battleship", Description: "A killing field"},

		// -1.0 - 0.1. The easy row's secFloor of 0.9 leaves only the frigate listings: a
		// high-sec-grade squad in null-sec.
		{TierName: "easy", SecMin: -1.0, SecMax: 0.1, Weight: 12, SizeDeltaMin: -1, SizeDeltaMax: -3,
			SecFloor: 0.9, Description: "Somebody has been farming this belt"},
		{TierName: "normal", SecMin: -1.0, SecMax: 0.1, Weight: 48, CmdrChancePct: 10,
			OfficerChancePct: 0.05, Description: "An ordinary null-sec belt"},
		{TierName: "hard", SecMin: -1.0, SecMax: 0.1, Weight: 30, SizeDeltaMin: 1, SizeDeltaMax: 1,
			CmdrChancePct: 25, OfficerChancePct: 0.40, HullBias: "cruiser,battlecruiser",
			Description: "A dangerous belt"},
		{TierName: "deadly", SecMin: -1.0, SecMax: 0.1, Weight: 10, SizeDeltaMin: 2, SizeDeltaMax: 3,
			CmdrChancePct: 45, OfficerChancePct: 1.50, HullBias: "battlecruiser,battleship",
			Description: "A killing field"},
	}
}

// settingOfficerSecFixApplied marks the one-shot officer maxSec repair as done.
const settingOfficerSecFixApplied = "spawner.officerSecFixApplied"

// migrateOfficerMaxSec repairs officer listings authored above 0.0 security, which put a
// named officer in the ordinary weighted pool of high-sec. Guarded by a server_settings
// marker, not a row count. See docs/db-notes.md#squad-tiers-and-the-officer-ledger.
func migrateOfficerMaxSec(ctx context.Context, db *sql.DB) error {
	if v, ok, _ := GetServerSetting(ctx, db, settingOfficerSecFixApplied); ok && v == "1" {
		return nil
	}

	// Matched through invGroups rather than a typeID list, so an officer hull authored under
	// the wrong role is caught too.
	stmts := []struct {
		what string
		sql  string
	}{
		{"role", `UPDATE npcRegionSpawns n
			JOIN invTypes t ON t.typeID = n.typeID
			JOIN invGroups g ON g.groupID = t.groupID
			SET n.role = 'officer'
			WHERE g.groupName LIKE 'Asteroid %Officer' AND n.role <> 'officer'`},
		{"maxSec", `UPDATE npcRegionSpawns n
			JOIN invTypes t ON t.typeID = n.typeID
			JOIN invGroups g ON g.groupID = t.groupID
			SET n.maxSec = 0.0
			WHERE g.groupName LIKE 'Asteroid %Officer' AND n.maxSec > 0.0`},
		{"shipClass", `UPDATE npcRegionSpawns n
			JOIN invTypes t ON t.typeID = n.typeID
			JOIN invGroups g ON g.groupID = t.groupID
			SET n.shipClass = 'battleship'
			WHERE g.groupName LIKE 'Asteroid %Officer' AND n.shipClass <> 'battleship'`},
	}
	for _, s := range stmts {
		res, err := db.ExecContext(ctx, s.sql)
		if err != nil {
			return fmt.Errorf("migrateOfficerMaxSec %s: %w", s.what, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			log.Printf("[spawner] officer repair: corrected %s on %d npcRegionSpawns row(s)", s.what, n)
		}
	}
	return SetServerSetting(ctx, db, settingOfficerSecFixApplied, "1")
}

// SquadTierRow is one (tier, security band) row of npcSquadTiers.
type SquadTierRow struct {
	TierID   int64
	TierName string
	SecMin   float64
	SecMax   float64
	Weight   int
	// SizeDeltaMin/Max are added to the spawner rule's ship window.
	SizeDeltaMin int
	SizeDeltaMax int
	// SecFloor is the lowest security the base pool is filtered at; on the maxSec hull ladder
	// no hull bigger than that security's carries. Never applied to the commander or officer
	// sub-pools.
	SecFloor         float64
	CmdrChancePct    float64
	OfficerChancePct float64
	HullBias         string // csv of shipClass values to favour
	Description      string
}

// GetAllSquadTiers loads every squad-tier row (about 20). The runtime preloads them at boot
// and filters bands in memory.
func GetAllSquadTiers(ctx context.Context, db *sql.DB) ([]SquadTierRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT tierID, tierName, secMin, secMax, weight, sizeDeltaMin, sizeDeltaMax,
		       secFloor, cmdrChancePct, officerChancePct, hullBias, COALESCE(description, '')
		FROM npcSquadTiers ORDER BY secMin DESC, weight DESC, tierID`)
	if err != nil {
		return nil, fmt.Errorf("GetAllSquadTiers: %w", err)
	}
	defer rows.Close()
	var out []SquadTierRow
	for rows.Next() {
		var t SquadTierRow
		if err := rows.Scan(&t.TierID, &t.TierName, &t.SecMin, &t.SecMax, &t.Weight,
			&t.SizeDeltaMin, &t.SizeDeltaMax, &t.SecFloor, &t.CmdrChancePct,
			&t.OfficerChancePct, &t.HullBias, &t.Description); err != nil {
			return nil, fmt.Errorf("GetAllSquadTiers scan: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// OfficerLedgerRow is one region's officer state.
type OfficerLedgerRow struct {
	RegionID      int64
	LiveCount     int
	LastSpawnAt   int64 // unix seconds
	LastKillAt    int64 // unix seconds
	LastTypeID    int64
	LastSystemID  int64
	LastFactionID int64
}

// LoadOfficerLedger reads the whole ledger, keyed by regionID.
func LoadOfficerLedger(ctx context.Context, db *sql.DB) (map[int64]OfficerLedgerRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT regionID, liveCount, lastSpawnAt, lastKillAt, lastTypeID, lastSystemID, lastFactionID
		FROM npcOfficerLedger`)
	if err != nil {
		return nil, fmt.Errorf("LoadOfficerLedger: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]OfficerLedgerRow)
	for rows.Next() {
		var r OfficerLedgerRow
		if err := rows.Scan(&r.RegionID, &r.LiveCount, &r.LastSpawnAt, &r.LastKillAt,
			&r.LastTypeID, &r.LastSystemID, &r.LastFactionID); err != nil {
			return nil, fmt.Errorf("LoadOfficerLedger scan: %w", err)
		}
		out[r.RegionID] = r
	}
	return out, rows.Err()
}

// ResetOfficerLedgerLive zeroes every liveCount at boot. Kill timestamps are kept: a
// restart does not free an officer.
func ResetOfficerLedgerLive(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `UPDATE npcOfficerLedger SET liveCount = 0`)
	return err
}

// RecordOfficerSpawn takes a region's officer slot and stamps who took it.
func RecordOfficerSpawn(ctx context.Context, db *sql.DB, regionID, typeID, systemID, factionID int64) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO npcOfficerLedger (regionID, liveCount, lastSpawnAt, lastTypeID, lastSystemID, lastFactionID)
		VALUES (?, 1, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE liveCount = liveCount + 1, lastSpawnAt = VALUES(lastSpawnAt),
		                        lastTypeID = VALUES(lastTypeID), lastSystemID = VALUES(lastSystemID),
		                        lastFactionID = VALUES(lastFactionID)`,
		regionID, time.Now().Unix(), typeID, systemID, factionID)
	return err
}

// RecordOfficerKill gives the slot back and starts the region's cool-down. GREATEST floors
// the count, so a double release cannot drive it negative and unlock the region.
func RecordOfficerKill(ctx context.Context, db *sql.DB, regionID int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE npcOfficerLedger SET liveCount = GREATEST(liveCount - 1, 0), lastKillAt = ? WHERE regionID = ?`,
		time.Now().Unix(), regionID)
	return err
}

// ReleaseOfficerSlots gives n slots back without starting the cool-down. It is the despawn
// path (a failed spawn, a torn-down system), which must not buy the region quiet.
func ReleaseOfficerSlots(ctx context.Context, db *sql.DB, regionID int64, n int) error {
	if n <= 0 {
		return nil
	}
	_, err := db.ExecContext(ctx,
		`UPDATE npcOfficerLedger SET liveCount = GREATEST(liveCount - ?, 0) WHERE regionID = ?`,
		n, regionID)
	return err
}

// ClearOfficerLedger wipes the ledger: the admin-panel reset for a ledger that has drifted
// closed.
func ClearOfficerLedger(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `DELETE FROM npcOfficerLedger`)
	return err
}

// EnsureDungeonNPCTable creates the npcDungeonEntities table if it does not exist.
// Call once at server startup alongside EnsureSpawnerTables.
func EnsureDungeonNPCTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS npcDungeonEntities (
			entityID       INT UNSIGNED PRIMARY KEY,
			typeID         INT NOT NULL,
			typeName       VARCHAR(128) NOT NULL DEFAULT '',
			solarSystemID  INT NOT NULL,
			posX           DOUBLE NOT NULL DEFAULT 0,
			posY           DOUBLE NOT NULL DEFAULT 0,
			posZ           DOUBLE NOT NULL DEFAULT 0,
			shieldFrac     DOUBLE NOT NULL DEFAULT 1.0,
			armorFrac      DOUBLE NOT NULL DEFAULT 1.0,
			structureFrac  DOUBLE NOT NULL DEFAULT 1.0,
			factionID      INT NOT NULL DEFAULT 0,
			corpID         INT NOT NULL DEFAULT 0,
			bounty         DOUBLE NOT NULL DEFAULT 0,
			groupID        INT NOT NULL DEFAULT 25,
			categoryID     INT NOT NULL DEFAULT 6,
			dunObjectID    INT NOT NULL DEFAULT 0,
			dunRoomID      INT NOT NULL DEFAULT 0,
			missionID      INT NOT NULL DEFAULT 0,
			spawnedAt      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updatedAt      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
			INDEX idx_system (solarSystemID),
			INDEX idx_room (dunRoomID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("EnsureDungeonNPCTable: %w", err)
	}
	return nil
}

// TruncateDungeonNPCs removes all rows from npcDungeonEntities. Call at server startup.
func TruncateDungeonNPCs(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `TRUNCATE TABLE npcDungeonEntities`)
	return err
}

// RegionSpawnRow is one NPC type allowed in a region's belts.
type RegionSpawnRow struct {
	SpawnID       int64
	RegionID      int64
	TypeID        int64
	MaxSec        float64 // spawns in systems with security <= MaxSec
	Weight        int
	ShipClass     string
	Role          string
	FactionID     int64
	CorporationID int64
}

// GetRegionSpawnsForRegion returns the belt-spawn roster for a region.
func GetRegionSpawnsForRegion(ctx context.Context, db *sql.DB, regionID int64) ([]RegionSpawnRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT spawnID, regionID, typeID, maxSec, weight, shipClass, role,
		       factionID, corporationID
		FROM npcRegionSpawns WHERE regionID = ? ORDER BY weight DESC, spawnID`, regionID)
	if err != nil {
		return nil, fmt.Errorf("GetRegionSpawnsForRegion(%d): %w", regionID, err)
	}
	defer rows.Close()
	var out []RegionSpawnRow
	for rows.Next() {
		var r RegionSpawnRow
		if err := rows.Scan(&r.SpawnID, &r.RegionID, &r.TypeID, &r.MaxSec, &r.Weight,
			&r.ShipClass, &r.Role, &r.FactionID, &r.CorporationID); err != nil {
			return nil, fmt.Errorf("GetRegionSpawnsForRegion scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRegionIDForSystem returns the region a solar system belongs to (0 if unknown).
func GetRegionIDForSystem(ctx context.Context, db *sql.DB, solarSystemID int64) int64 {
	var regionID int64
	_ = db.QueryRowContext(ctx,
		`SELECT regionID FROM mapSolarSystems WHERE solarSystemID = ?`, solarSystemID).Scan(&regionID)
	return regionID
}

// GetAllRegionSpawns loads every npcRegionSpawns row in one query,
// grouped by regionID. Used at boot to preload the full spawn roster.
func GetAllRegionSpawns(ctx context.Context, db *sql.DB) (map[int64][]RegionSpawnRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT spawnID, regionID, typeID, maxSec, weight, shipClass, role,
		       factionID, corporationID
		FROM npcRegionSpawns ORDER BY regionID, weight DESC, spawnID`)
	if err != nil {
		return nil, fmt.Errorf("GetAllRegionSpawns: %w", err)
	}
	defer rows.Close()
	out := make(map[int64][]RegionSpawnRow)
	for rows.Next() {
		var r RegionSpawnRow
		if err := rows.Scan(&r.SpawnID, &r.RegionID, &r.TypeID, &r.MaxSec, &r.Weight,
			&r.ShipClass, &r.Role, &r.FactionID, &r.CorporationID); err != nil {
			return nil, fmt.Errorf("GetAllRegionSpawns scan: %w", err)
		}
		out[r.RegionID] = append(out[r.RegionID], r)
	}
	return out, rows.Err()
}

// GetAllSystemRegionMap loads every solarSystemID to regionID pair from mapSolarSystems in
// one query. Used at boot for preload lookups.
func GetAllSystemRegionMap(ctx context.Context, db *sql.DB) (map[int64]int64, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT solarSystemID, regionID FROM mapSolarSystems`)
	if err != nil {
		return nil, fmt.Errorf("GetAllSystemRegionMap: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]int64, 8192)
	for rows.Next() {
		var sysID, regID int64
		if err := rows.Scan(&sysID, &regID); err != nil {
			return nil, fmt.Errorf("GetAllSystemRegionMap scan: %w", err)
		}
		out[sysID] = regID
	}
	return out, rows.Err()
}

// SpawnerRow represents a row from npcSpawners.
type SpawnerRow struct {
	SpawnerID          int64
	SolarSystemID      int64
	CelestialGroup     int64
	PoolID             int64
	TemplateID         int64 // legacy fallback when PoolID is 0
	FactionID          int64
	CorporationID      int64
	MaxNPCs            int
	TriggerRange       float64
	ScatterRadius      float64
	FullClearDelaySec  int // seed-to-seed cooldown between belt waves
	SpawnClassID       int64
	HaulerChancePct    float64
	CommanderChancePct float64
	OfficerChancePct   float64
	WarpIn             bool
}

// GetDefaultSpawnerConfig returns the first spawner row with solarSystemID=0
// matching the given celestialGroup, used as a default template for auto-spawners.
func GetDefaultSpawnerConfig(ctx context.Context, db *sql.DB, celestialGroup int64) (*SpawnerRow, error) {
	s := &SpawnerRow{}
	err := db.QueryRowContext(ctx,
		`SELECT spawnerID, solarSystemID, celestialGroup, COALESCE(poolID, 0), templateID, factionID,
		        COALESCE(corporationID, 0), maxNPCs, triggerRange, scatterRadius,
		        COALESCE(fullClearDelaySec, 300),
		        COALESCE(spawnClassID, 0), COALESCE(haulerChancePct, 0),
		        COALESCE(commanderChancePct, 0), COALESCE(officerChancePct, 0),
		        COALESCE(warpIn, 1)
		 FROM npcSpawners WHERE solarSystemID = 0 AND celestialGroup = ? LIMIT 1`,
		celestialGroup).Scan(&s.SpawnerID, &s.SolarSystemID, &s.CelestialGroup,
		&s.PoolID, &s.TemplateID, &s.FactionID, &s.CorporationID, &s.MaxNPCs,
		&s.TriggerRange, &s.ScatterRadius, &s.FullClearDelaySec,
		&s.SpawnClassID, &s.HaulerChancePct, &s.CommanderChancePct, &s.OfficerChancePct,
		&s.WarpIn)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetDefaultSpawnerConfig(%d): %w", celestialGroup, err)
	}
	return s, nil
}

// SpawnerRuleRow represents a row from npcSpawnerRules: how many ships a belt
// wave holds in a security band. What can fill those slots comes from the
// region's spawn list, not from here.
type SpawnerRuleRow struct {
	RuleID      int64
	RuleName    string
	SecMin      float64
	SecMax      float64
	MinShips    int
	MaxShips    int
	Description string
}

// GetSpawnerRuleForSecurity returns the rule covering the given security level.
// Both bounds are inclusive so a band ending at 1.0 covers sec-1.0 systems;
// where two bands share a boundary the higher band wins via ORDER BY secMin DESC.
func GetSpawnerRuleForSecurity(ctx context.Context, db *sql.DB, security float64) (*SpawnerRuleRow, error) {
	r := &SpawnerRuleRow{}
	err := db.QueryRowContext(ctx,
		`SELECT ruleID, ruleName, secMin, secMax, minShips, maxShips, COALESCE(description, '')
		 FROM npcSpawnerRules WHERE secMin <= ? AND secMax >= ? ORDER BY secMin DESC LIMIT 1`, security, security,
	).Scan(&r.RuleID, &r.RuleName, &r.SecMin, &r.SecMax, &r.MinShips, &r.MaxShips, &r.Description)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetSpawnerRuleForSecurity(%.1f): %w", security, err)
	}
	return r, nil
}

// LiveNPCRow represents a row from npcLiveEntities.
type LiveNPCRow struct {
	EntityID      uint32
	TypeID        int64
	TypeName      string
	SolarSystemID int64
	CelestialID   int64
	SpawnerID     int64
	PosX          float64
	PosY          float64
	PosZ          float64
	ShieldFrac    float64
	ArmorFrac     float64
	StructureFrac float64
	FactionID     int64
	CorpID        int64
	Bounty        float64
	GroupID       int64
	CategoryID    int64
}

// InsertLiveNPC inserts a row into npcLiveEntities for a spawned NPC.
func InsertLiveNPC(ctx context.Context, db *sql.DB, entityID uint32, typeID int64, typeName string,
	solarSystemID, celestialID, spawnerID int64, posX, posY, posZ float64,
	factionID, corpID int64, bounty float64, groupID, categoryID int64) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO npcLiveEntities (entityID, typeID, typeName, solarSystemID, celestialID, spawnerID,
			posX, posY, posZ, factionID, corpID, bounty, groupID, categoryID)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE updatedAt=NOW()`,
		entityID, typeID, typeName, solarSystemID, celestialID, spawnerID,
		posX, posY, posZ, factionID, corpID, bounty, groupID, categoryID)
	return err
}

// DeleteLiveNPC removes a row from npcLiveEntities when an NPC dies.
func DeleteLiveNPC(ctx context.Context, db *sql.DB, entityID uint32) error {
	_, err := db.ExecContext(ctx, `DELETE FROM npcLiveEntities WHERE entityID = ?`, entityID)
	return err
}

// DeleteLiveNPCsBySystem removes all npcLiveEntities rows for a solar system.
// Called when a system is torn down (last player left) and its NPCs despawn.
func DeleteLiveNPCsBySystem(ctx context.Context, db *sql.DB, solarSystemID int64) error {
	_, err := db.ExecContext(ctx, `DELETE FROM npcLiveEntities WHERE solarSystemID = ?`, solarSystemID)
	return err
}

// TruncateLiveNPCs removes all rows from npcLiveEntities. Call at server startup.
func TruncateLiveNPCs(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `TRUNCATE TABLE npcLiveEntities`)
	return err
}

// GetLiveNPCsForSystem returns all live NPC rows for a solar system.
func GetLiveNPCsForSystem(ctx context.Context, db *sql.DB, solarSystemID int64) ([]LiveNPCRow, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT entityID, typeID, typeName, solarSystemID, celestialID, spawnerID,
			posX, posY, posZ, shieldFrac, armorFrac, structureFrac, factionID, corpID, bounty
		 FROM npcLiveEntities WHERE solarSystemID = ?`, solarSystemID)
	if err != nil {
		return nil, fmt.Errorf("GetLiveNPCsForSystem(%d): %w", solarSystemID, err)
	}
	defer rows.Close()
	var out []LiveNPCRow
	for rows.Next() {
		var r LiveNPCRow
		if err := rows.Scan(&r.EntityID, &r.TypeID, &r.TypeName, &r.SolarSystemID,
			&r.CelestialID, &r.SpawnerID, &r.PosX, &r.PosY, &r.PosZ,
			&r.ShieldFrac, &r.ArmorFrac, &r.StructureFrac, &r.FactionID, &r.CorpID, &r.Bounty); err != nil {
			return nil, fmt.Errorf("GetLiveNPCsForSystem scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetLiveNPCByEntityID returns a single live NPC row by entity ID, or nil if not found.
func GetLiveNPCByEntityID(ctx context.Context, db *sql.DB, entityID uint32) (*LiveNPCRow, error) {
	r := &LiveNPCRow{}
	err := db.QueryRowContext(ctx,
		`SELECT entityID, typeID, typeName, solarSystemID, celestialID, spawnerID,
			posX, posY, posZ, shieldFrac, armorFrac, structureFrac, factionID, corpID, bounty,
			COALESCE(groupID, 25), COALESCE(categoryID, 6)
		 FROM npcLiveEntities WHERE entityID = ?`, entityID,
	).Scan(&r.EntityID, &r.TypeID, &r.TypeName, &r.SolarSystemID,
		&r.CelestialID, &r.SpawnerID, &r.PosX, &r.PosY, &r.PosZ,
		&r.ShieldFrac, &r.ArmorFrac, &r.StructureFrac, &r.FactionID, &r.CorpID, &r.Bounty,
		&r.GroupID, &r.CategoryID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetLiveNPCByEntityID(%d): %w", entityID, err)
	}
	return r, nil
}

// UpdateLiveNPCHP updates the HP fractions for a belt NPC.
func UpdateLiveNPCHP(ctx context.Context, db *sql.DB, entityID uint32, shieldFrac, armorFrac, structureFrac float64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE npcLiveEntities SET shieldFrac=?, armorFrac=?, structureFrac=? WHERE entityID=?`,
		shieldFrac, armorFrac, structureFrac, entityID)
	return err
}

// UpdateLiveNPCPosition updates the position columns for a belt NPC.
// Called from the position-correction callback so the DB reflects the
// NPC's actual location rather than only its spawn origin.
func UpdateLiveNPCPosition(ctx context.Context, db *sql.DB, entityID uint32, x, y, z float64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE npcLiveEntities SET posX=?, posY=?, posZ=? WHERE entityID=?`,
		x, y, z, entityID)
	return err
}

// DungeonNPCRow represents a row from npcDungeonEntities.
type DungeonNPCRow struct {
	EntityID      uint32
	TypeID        int64
	TypeName      string
	SolarSystemID int64
	PosX          float64
	PosY          float64
	PosZ          float64
	ShieldFrac    float64
	ArmorFrac     float64
	StructureFrac float64
	FactionID     int64
	CorpID        int64
	Bounty        float64
	GroupID       int64
	CategoryID    int64
	DunObjectID   int64
	DunRoomID     int64
	MissionID     int64
}

// InsertDungeonNPC inserts a row into npcDungeonEntities for a spawned dungeon NPC.
func InsertDungeonNPC(ctx context.Context, db *sql.DB, entityID uint32, typeID int64, typeName string,
	solarSystemID int64, posX, posY, posZ float64,
	factionID, corpID int64, bounty float64, groupID, categoryID int64,
	dunObjectID, dunRoomID, missionID int64) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO npcDungeonEntities (entityID, typeID, typeName, solarSystemID,
			posX, posY, posZ, factionID, corpID, bounty, groupID, categoryID,
			dunObjectID, dunRoomID, missionID)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE updatedAt=NOW()`,
		entityID, typeID, typeName, solarSystemID,
		posX, posY, posZ, factionID, corpID, bounty, groupID, categoryID,
		dunObjectID, dunRoomID, missionID)
	return err
}

// DeleteDungeonNPC removes a single dungeon NPC row.
func DeleteDungeonNPC(ctx context.Context, db *sql.DB, entityID uint32) error {
	_, err := db.ExecContext(ctx, `DELETE FROM npcDungeonEntities WHERE entityID = ?`, entityID)
	return err
}

// DeleteDungeonNPCsByRoom removes all dungeon NPC rows for a given room.
func DeleteDungeonNPCsByRoom(ctx context.Context, db *sql.DB, dunRoomID int64) error {
	_, err := db.ExecContext(ctx, `DELETE FROM npcDungeonEntities WHERE dunRoomID = ?`, dunRoomID)
	return err
}

// GetDungeonNPCByEntityID returns a single dungeon NPC row by entity ID, or nil if not found.
func GetDungeonNPCByEntityID(ctx context.Context, db *sql.DB, entityID uint32) (*DungeonNPCRow, error) {
	r := &DungeonNPCRow{}
	err := db.QueryRowContext(ctx,
		`SELECT entityID, typeID, typeName, solarSystemID,
			posX, posY, posZ, shieldFrac, armorFrac, structureFrac,
			factionID, corpID, bounty, groupID, categoryID,
			dunObjectID, dunRoomID, missionID
		 FROM npcDungeonEntities WHERE entityID = ?`, entityID,
	).Scan(&r.EntityID, &r.TypeID, &r.TypeName, &r.SolarSystemID,
		&r.PosX, &r.PosY, &r.PosZ, &r.ShieldFrac, &r.ArmorFrac, &r.StructureFrac,
		&r.FactionID, &r.CorpID, &r.Bounty, &r.GroupID, &r.CategoryID,
		&r.DunObjectID, &r.DunRoomID, &r.MissionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetDungeonNPCByEntityID(%d): %w", entityID, err)
	}
	return r, nil
}

// UpdateDungeonNPCHP updates the HP fractions for a dungeon NPC.
func UpdateDungeonNPCHP(ctx context.Context, db *sql.DB, entityID uint32, shieldFrac, armorFrac, structureFrac float64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE npcDungeonEntities SET shieldFrac=?, armorFrac=?, structureFrac=? WHERE entityID=?`,
		shieldFrac, armorFrac, structureFrac, entityID)
	return err
}
