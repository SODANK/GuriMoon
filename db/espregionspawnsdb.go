package db

// ESP queries for the flat Region Spawns system: per-region belt-spawn
// listings (npcRegionSpawns) and the global belt-settings row.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
)

// ---- ESP Region Spawns (flat belt-spawn lists) ----

// ESPRegionSpawnSummary is one region on the Region Spawns list page.
type ESPRegionSpawnSummary struct {
	RegionID     int64
	RegionName   string
	SystemCount  int
	ListingCount int
	FactionNames string // distinct faction names among listings, comma-joined
}

// ESPGetRegionSpawnSummaries returns every region with its belt-spawn counts.
func ESPGetRegionSpawnSummaries(ctx context.Context, db *sql.DB) ([]ESPRegionSpawnSummary, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT r.regionID, r.regionName,
		       (SELECT COUNT(*) FROM mapSolarSystems s WHERE s.regionID = r.regionID),
		       (SELECT COUNT(*) FROM npcRegionSpawns n WHERE n.regionID = r.regionID),
		       COALESCE((SELECT GROUP_CONCAT(DISTINCT f.ownerName ORDER BY f.ownerName SEPARATOR ', ')
		                 FROM npcRegionSpawns n
		                 JOIN eveStaticOwners f ON f.ownerID = n.factionID AND f.typeID IN (30, 500)
		                 WHERE n.regionID = r.regionID), '')
		FROM mapRegions r
		ORDER BY r.regionName`)
	if err != nil {
		return nil, fmt.Errorf("ESPGetRegionSpawnSummaries: %w", err)
	}
	defer rows.Close()
	var out []ESPRegionSpawnSummary
	for rows.Next() {
		var r ESPRegionSpawnSummary
		if err := rows.Scan(&r.RegionID, &r.RegionName, &r.SystemCount, &r.ListingCount, &r.FactionNames); err != nil {
			return nil, fmt.Errorf("ESPGetRegionSpawnSummaries scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ESPRegionSpawnRow is one belt-spawn listing with resolved names.
type ESPRegionSpawnRow struct {
	SpawnID       int64
	RegionID      int64
	TypeID        int64
	TypeName      string
	MaxSec        float64
	Weight        int
	ShipClass     string
	Role          string
	FactionID     int64
	FactionName   string
	CorporationID int64
	CorpName      string
}

// ESPGetRegionSpawnRows returns a region's belt-spawn listings.
func ESPGetRegionSpawnRows(ctx context.Context, db *sql.DB, regionID int64) ([]ESPRegionSpawnRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT n.spawnID, n.regionID, n.typeID, COALESCE(it.typeName, ''),
		       n.maxSec, n.weight, n.shipClass, n.role,
		       n.factionID, COALESCE(f.ownerName, ''),
		       n.corporationID,
		       COALESCE(corp.corporationName, COALESCE(eso.ownerName, ''))
		FROM npcRegionSpawns n
		LEFT JOIN invTypes it ON it.typeID = n.typeID
		LEFT JOIN eveStaticOwners f ON f.ownerID = n.factionID AND f.typeID IN (30, 500)
		LEFT JOIN corporation corp ON corp.corporationID = n.corporationID
		LEFT JOIN eveStaticOwners eso ON eso.ownerID = n.corporationID AND n.corporationID > 0
		WHERE n.regionID = ?
		ORDER BY FIELD(n.shipClass, 'frigate','destroyer','cruiser','battlecruiser','battleship'),
		         n.role, n.weight DESC, it.typeName`, regionID)
	if err != nil {
		return nil, fmt.Errorf("ESPGetRegionSpawnRows(%d): %w", regionID, err)
	}
	defer rows.Close()
	var out []ESPRegionSpawnRow
	for rows.Next() {
		var r ESPRegionSpawnRow
		if err := rows.Scan(&r.SpawnID, &r.RegionID, &r.TypeID, &r.TypeName,
			&r.MaxSec, &r.Weight, &r.ShipClass, &r.Role,
			&r.FactionID, &r.FactionName, &r.CorporationID, &r.CorpName); err != nil {
			return nil, fmt.Errorf("ESPGetRegionSpawnRows scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ESPGetRegionName resolves a region's display name ("" if unknown).
func ESPGetRegionName(ctx context.Context, db *sql.DB, regionID int64) string {
	var name string
	_ = db.QueryRowContext(ctx,
		`SELECT regionName FROM mapRegions WHERE regionID = ?`, regionID).Scan(&name)
	return name
}

// ESPAddRegionSpawn adds one NPC listing to every given region (shotgun add).
// Returns how many rows were inserted.
func ESPAddRegionSpawn(ctx context.Context, db *sql.DB, regionIDs []int64, typeID int64,
	maxSec float64, weight int, shipClass, role string, factionID, corporationID int64) (int, error) {
	added := 0
	for _, regionID := range regionIDs {
		if regionID == 0 {
			continue
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO npcRegionSpawns
				(regionID, typeID, maxSec, weight, shipClass, role, factionID, corporationID)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			regionID, typeID, maxSec, weight, shipClass, role, factionID, corporationID); err != nil {
			return added, fmt.Errorf("ESPAddRegionSpawn(region %d): %w", regionID, err)
		}
		added++
		invalidateRegionBelts(ctx, db, regionID)
	}
	return added, nil
}

// invalidateRegionBelts drops the region's persisted belt waves so an edited
// roster takes effect. A belt is rolled once and kept (db/beltwavedb.go) until
// cleared and cooled down. Failure is logged, not returned: the listing edit
// already succeeded.
func invalidateRegionBelts(ctx context.Context, db *sql.DB, regionID int64) {
	if regionID == 0 {
		return
	}
	if n, err := PurgeBeltWavesForRegion(ctx, db, regionID); err != nil {
		log.Printf("[spawner] region %d roster edited but its belt waves could not be dropped: %v — "+
			"existing belts keep their old contents until cleared", regionID, err)
	} else if n > 0 {
		log.Printf("[spawner] region %d roster edited — dropped %d persisted belt wave(s) so the change takes", regionID, n)
	}
}

// ESPRegionSpawnUpdate is one edited listing handed to ESPUpdateRegionSpawns.
type ESPRegionSpawnUpdate struct {
	SpawnID       int64
	TypeID        int64
	MaxSec        float64
	Weight        int
	ShipClass     string
	Role          string
	FactionID     int64
	CorporationID int64
}

// ESPUpdateRegionSpawns writes a batch of edited listings in one transaction,
// so a Save all lands complete or not at all.
func ESPUpdateRegionSpawns(ctx context.Context, db *sql.DB, rows []ESPRegionSpawnUpdate) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ESPUpdateRegionSpawns begin: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		UPDATE npcRegionSpawns
		SET typeID=?, maxSec=?, weight=?, shipClass=?, role=?, factionID=?, corporationID=?
		WHERE spawnID=?`)
	if err != nil {
		return fmt.Errorf("ESPUpdateRegionSpawns prepare: %w", err)
	}
	defer stmt.Close()

	for _, r := range rows {
		if _, err := stmt.ExecContext(ctx, r.TypeID, r.MaxSec, r.Weight, r.ShipClass,
			r.Role, r.FactionID, r.CorporationID, r.SpawnID); err != nil {
			return fmt.Errorf("ESPUpdateRegionSpawns(spawn %d): %w", r.SpawnID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ESPUpdateRegionSpawns commit: %w", err)
	}
	// After the commit: the purge is a cache drop and its failure must not roll
	// back the listing edit.
	for _, regionID := range regionsOfSpawns(ctx, db, rows) {
		invalidateRegionBelts(ctx, db, regionID)
	}
	return nil
}

// regionsOfSpawns resolves an edited batch back to the regions it touched,
// read after the update so a listing moved between regions reports its new one.
func regionsOfSpawns(ctx context.Context, db *sql.DB, rows []ESPRegionSpawnUpdate) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, r := range rows {
		var regionID int64
		if err := db.QueryRowContext(ctx,
			`SELECT regionID FROM npcRegionSpawns WHERE spawnID = ?`, r.SpawnID).Scan(&regionID); err != nil {
			continue
		}
		if regionID != 0 && !seen[regionID] {
			seen[regionID] = true
			out = append(out, regionID)
		}
	}
	return out
}

// ESPDeleteRegionSpawn removes one belt-spawn listing.
func ESPDeleteRegionSpawn(ctx context.Context, db *sql.DB, spawnID int64) error {
	// Read the region before the row is deleted, so its persisted belts can be
	// purged.
	var regionID int64
	_ = db.QueryRowContext(ctx,
		`SELECT regionID FROM npcRegionSpawns WHERE spawnID = ?`, spawnID).Scan(&regionID)
	_, err := db.ExecContext(ctx, `DELETE FROM npcRegionSpawns WHERE spawnID = ?`, spawnID)
	if err != nil {
		return fmt.Errorf("ESPDeleteRegionSpawn: %w", err)
	}
	invalidateRegionBelts(ctx, db, regionID)
	return nil
}

// ESPSetRegionSpawnOwner assigns one faction/corp pair to every listing in a
// region (bulk ownership change).
func ESPSetRegionSpawnOwner(ctx context.Context, db *sql.DB, regionID, factionID, corporationID int64) (int, error) {
	res, err := db.ExecContext(ctx,
		`UPDATE npcRegionSpawns SET factionID=?, corporationID=? WHERE regionID=?`,
		factionID, corporationID, regionID)
	if err != nil {
		return 0, fmt.Errorf("ESPSetRegionSpawnOwner: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ESPClearRegionSpawns removes every listing in a region.
func ESPClearRegionSpawns(ctx context.Context, db *sql.DB, regionID int64) error {
	_, err := db.ExecContext(ctx, `DELETE FROM npcRegionSpawns WHERE regionID = ?`, regionID)
	if err != nil {
		return fmt.Errorf("ESPClearRegionSpawns: %w", err)
	}
	return nil
}

// ESPCopyRegionSpawns copies every listing of one region into others.
// replace=true wipes each target region first. Returns rows copied in total.
func ESPCopyRegionSpawns(ctx context.Context, db *sql.DB, fromRegionID int64, toRegionIDs []int64, replace bool) (int, error) {
	copied := 0
	for _, to := range toRegionIDs {
		if to == 0 || to == fromRegionID {
			continue
		}
		if replace {
			if err := ESPClearRegionSpawns(ctx, db, to); err != nil {
				return copied, err
			}
		}
		res, err := db.ExecContext(ctx, `
			INSERT INTO npcRegionSpawns
				(regionID, typeID, maxSec, weight, shipClass, role, factionID, corporationID)
			SELECT ?, typeID, maxSec, weight, shipClass, role, factionID, corporationID
			FROM npcRegionSpawns WHERE regionID = ?`, to, fromRegionID)
		if err != nil {
			return copied, fmt.Errorf("ESPCopyRegionSpawns(%d to %d): %w", fromRegionID, to, err)
		}
		if n, err := res.RowsAffected(); err == nil {
			copied += int(n)
		}
	}
	return copied, nil
}

// ESPFactionCorpOption is an NPC corporation with its owning faction, for
// faction-filtered corp pickers.
type ESPFactionCorpOption struct {
	CorporationID int64  `json:"id"`
	CorpName      string `json:"name"`
	FactionID     int64  `json:"faction"`
}

// ESPGetNPCCorpsWithFaction returns every NPC corporation with its faction, so
// a picker can narrow corps once a faction is chosen.
func ESPGetNPCCorpsWithFaction(ctx context.Context, db *sql.DB) ([]ESPFactionCorpOption, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT c.corporationID,
		       COALESCE(corp.corporationName, COALESCE(eso.ownerName, CAST(c.corporationID AS CHAR))),
		       COALESCE(c.factionID, 0)
		FROM crpNPCCorporations c
		LEFT JOIN corporation corp ON corp.corporationID = c.corporationID
		LEFT JOIN eveStaticOwners eso ON eso.ownerID = c.corporationID
		ORDER BY 2`)
	if err != nil {
		return nil, fmt.Errorf("ESPGetNPCCorpsWithFaction: %w", err)
	}
	defer rows.Close()
	var out []ESPFactionCorpOption
	for rows.Next() {
		var r ESPFactionCorpOption
		if err := rows.Scan(&r.CorporationID, &r.CorpName, &r.FactionID); err != nil {
			return nil, fmt.Errorf("ESPGetNPCCorpsWithFaction scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ESPGetSpawnFactionOptions returns the factions that own at least one NPC
// corporation, plus RogueDroneFactionID, which owns the rogue drone listings with no corp.
// A listing whose faction is missing here saves back as faction 0.
func ESPGetSpawnFactionOptions(ctx context.Context, db *sql.DB) ([]ESPFaction, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT f.ownerID, f.ownerName
		FROM eveStaticOwners f
		WHERE f.typeID IN (30, 500)
		  AND (EXISTS (SELECT 1 FROM crpNPCCorporations c WHERE c.factionID = f.ownerID)
		       OR f.ownerID = ?)
		ORDER BY f.ownerName`, RogueDroneFactionID)
	if err != nil {
		return nil, fmt.Errorf("ESPGetSpawnFactionOptions: %w", err)
	}
	defer rows.Close()
	var out []ESPFaction
	for rows.Next() {
		var f ESPFaction
		if err := rows.Scan(&f.FactionID, &f.FactionName); err != nil {
			return nil, fmt.Errorf("ESPGetSpawnFactionOptions scan: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ESPRegionOption is an ID+name pair for region pickers.
type ESPRegionOption struct {
	RegionID int64
	Name     string
}

// ESPGetAllRegionOptions returns all regions for pickers.
func ESPGetAllRegionOptions(ctx context.Context, db *sql.DB) ([]ESPRegionOption, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT regionID, regionName FROM mapRegions ORDER BY regionName`)
	if err != nil {
		return nil, fmt.Errorf("ESPGetAllRegionOptions: %w", err)
	}
	defer rows.Close()
	var out []ESPRegionOption
	for rows.Next() {
		var o ESPRegionOption
		if err := rows.Scan(&o.RegionID, &o.Name); err != nil {
			return nil, fmt.Errorf("ESPGetAllRegionOptions scan: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ---- ESP Spawner Rules (wave size per security band) ----

// ESPSpawnerRuleRow is one spawner rule for the admin page.
type ESPSpawnerRuleRow struct {
	RuleID      int64
	RuleName    string
	SecMin      float64
	SecMax      float64
	MinShips    int
	MaxShips    int
	Description string
}

// ESPGetAllSpawnerRules returns every rule, safest band first.
func ESPGetAllSpawnerRules(ctx context.Context, db *sql.DB) ([]ESPSpawnerRuleRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT ruleID, ruleName, secMin, secMax, minShips, maxShips, COALESCE(description, '')
		FROM npcSpawnerRules ORDER BY secMin DESC`)
	if err != nil {
		return nil, fmt.Errorf("ESPGetAllSpawnerRules: %w", err)
	}
	defer rows.Close()
	var out []ESPSpawnerRuleRow
	for rows.Next() {
		var r ESPSpawnerRuleRow
		if err := rows.Scan(&r.RuleID, &r.RuleName, &r.SecMin, &r.SecMax,
			&r.MinShips, &r.MaxShips, &r.Description); err != nil {
			return nil, fmt.Errorf("ESPGetAllSpawnerRules scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ESPCreateSpawnerRule adds a rule and returns its ID.
func ESPCreateSpawnerRule(ctx context.Context, db *sql.DB, name string, secMin, secMax float64, minShips, maxShips int, description string) (int64, error) {
	res, err := db.ExecContext(ctx,
		`INSERT INTO npcSpawnerRules (ruleName, secMin, secMax, minShips, maxShips, description) VALUES (?, ?, ?, ?, ?, ?)`,
		name, secMin, secMax, minShips, maxShips, description)
	if err != nil {
		return 0, fmt.Errorf("ESPCreateSpawnerRule: %w", err)
	}
	return res.LastInsertId()
}

// ESPUpdateSpawnerRule updates a rule.
func ESPUpdateSpawnerRule(ctx context.Context, db *sql.DB, ruleID int64, name string, secMin, secMax float64, minShips, maxShips int, description string) error {
	_, err := db.ExecContext(ctx,
		`UPDATE npcSpawnerRules SET ruleName=?, secMin=?, secMax=?, minShips=?, maxShips=?, description=? WHERE ruleID=?`,
		name, secMin, secMax, minShips, maxShips, description, ruleID)
	if err != nil {
		return fmt.Errorf("ESPUpdateSpawnerRule: %w", err)
	}
	return nil
}

// ESPDeleteSpawnerRule removes a rule.
func ESPDeleteSpawnerRule(ctx context.Context, db *sql.DB, ruleID int64) error {
	_, err := db.ExecContext(ctx, `DELETE FROM npcSpawnerRules WHERE ruleID = ?`, ruleID)
	if err != nil {
		return fmt.Errorf("ESPDeleteSpawnerRule: %w", err)
	}
	return nil
}

// ---- ESP Squad Tiers (how hard a belt is, per security band) ----

// ESPSquadTierRow is one squad tier for the admin page. BandPct is the row's
// share of its own band, computed for display.
type ESPSquadTierRow struct {
	TierID           int64
	TierName         string
	SecMin           float64
	SecMax           float64
	Weight           int
	SizeDeltaMin     int
	SizeDeltaMax     int
	SecFloor         float64
	CmdrChancePct    float64
	OfficerChancePct float64
	HullBias         string
	Description      string
	BandPct          float64
}

// ESPGetAllSquadTiers returns every tier, safest band first, each stamped with
// its share of its band.
func ESPGetAllSquadTiers(ctx context.Context, db *sql.DB) ([]ESPSquadTierRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT tierID, tierName, secMin, secMax, weight, sizeDeltaMin, sizeDeltaMax,
		       secFloor, cmdrChancePct, officerChancePct, hullBias, COALESCE(description, '')
		FROM npcSquadTiers ORDER BY secMin DESC, weight DESC, tierID`)
	if err != nil {
		return nil, fmt.Errorf("ESPGetAllSquadTiers: %w", err)
	}
	defer rows.Close()
	var out []ESPSquadTierRow
	for rows.Next() {
		var t ESPSquadTierRow
		if err := rows.Scan(&t.TierID, &t.TierName, &t.SecMin, &t.SecMax, &t.Weight,
			&t.SizeDeltaMin, &t.SizeDeltaMax, &t.SecFloor, &t.CmdrChancePct,
			&t.OfficerChancePct, &t.HullBias, &t.Description); err != nil {
			return nil, fmt.Errorf("ESPGetAllSquadTiers scan: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	AnnotateSquadTierBands(out)
	return out, nil
}

// AnnotateSquadTierBands fills in each row's BandPct: its weight over the sum of
// the weights in the same (secMin, secMax) band. Bands group by exact bounds,
// as the roll does; a row alone in its bounds reads 100%.
func AnnotateSquadTierBands(rows []ESPSquadTierRow) {
	type band struct{ lo, hi float64 }
	totals := map[band]int{}
	for _, r := range rows {
		if r.Weight > 0 {
			totals[band{r.SecMin, r.SecMax}] += r.Weight
		}
	}
	for i := range rows {
		total := totals[band{rows[i].SecMin, rows[i].SecMax}]
		if total > 0 && rows[i].Weight > 0 {
			rows[i].BandPct = float64(rows[i].Weight) * 100 / float64(total)
		}
	}
}

// ESPCreateSquadTier adds a tier and returns its ID.
func ESPCreateSquadTier(ctx context.Context, db *sql.DB, t ESPSquadTierRow) (int64, error) {
	res, err := db.ExecContext(ctx, `
		INSERT INTO npcSquadTiers
			(tierName, secMin, secMax, weight, sizeDeltaMin, sizeDeltaMax,
			 secFloor, cmdrChancePct, officerChancePct, hullBias, description)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.TierName, t.SecMin, t.SecMax, t.Weight, t.SizeDeltaMin, t.SizeDeltaMax,
		t.SecFloor, t.CmdrChancePct, t.OfficerChancePct, t.HullBias, t.Description)
	if err != nil {
		return 0, fmt.Errorf("ESPCreateSquadTier: %w", err)
	}
	return res.LastInsertId()
}

// ESPUpdateSquadTier writes an edited tier.
func ESPUpdateSquadTier(ctx context.Context, db *sql.DB, t ESPSquadTierRow) error {
	_, err := db.ExecContext(ctx, `
		UPDATE npcSquadTiers
		SET tierName=?, secMin=?, secMax=?, weight=?, sizeDeltaMin=?, sizeDeltaMax=?,
		    secFloor=?, cmdrChancePct=?, officerChancePct=?, hullBias=?, description=?
		WHERE tierID=?`,
		t.TierName, t.SecMin, t.SecMax, t.Weight, t.SizeDeltaMin, t.SizeDeltaMax,
		t.SecFloor, t.CmdrChancePct, t.OfficerChancePct, t.HullBias, t.Description, t.TierID)
	if err != nil {
		return fmt.Errorf("ESPUpdateSquadTier: %w", err)
	}
	return nil
}

// ESPDeleteSquadTier removes a tier.
func ESPDeleteSquadTier(ctx context.Context, db *sql.DB, tierID int64) error {
	_, err := db.ExecContext(ctx, `DELETE FROM npcSquadTiers WHERE tierID = ?`, tierID)
	if err != nil {
		return fmt.Errorf("ESPDeleteSquadTier: %w", err)
	}
	return nil
}

// ---- ESP Officer Ledger ----

// ESPOfficerLedgerRow is one region's officer state with names resolved. It is
// read from npcOfficerLedger, so LiveCount is the persisted mirror; the running
// server's in-memory count is the one that gates a spawn.
type ESPOfficerLedgerRow struct {
	RegionID      int64
	RegionName    string
	LiveCount     int
	LastSpawnAt   int64
	LastKillAt    int64
	LastTypeID    int64
	LastTypeName  string
	LastSystemID  int64
	LastSystem    string
	LastFactionID int64
	FactionName   string
}

// ESPGetOfficerLedger returns every region that has ever held an officer.
func ESPGetOfficerLedger(ctx context.Context, db *sql.DB) ([]ESPOfficerLedgerRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT l.regionID, COALESCE(r.regionName, ''), l.liveCount, l.lastSpawnAt, l.lastKillAt,
		       l.lastTypeID, COALESCE(it.typeName, ''),
		       l.lastSystemID, COALESCE(ss.solarSystemName, ''),
		       l.lastFactionID, COALESCE(f.ownerName, '')
		FROM npcOfficerLedger l
		LEFT JOIN mapRegions r ON r.regionID = l.regionID
		LEFT JOIN invTypes it ON it.typeID = l.lastTypeID
		LEFT JOIN mapSolarSystems ss ON ss.solarSystemID = l.lastSystemID
		LEFT JOIN eveStaticOwners f ON f.ownerID = l.lastFactionID AND f.typeID IN (30, 500)
		ORDER BY l.liveCount DESC, l.lastSpawnAt DESC, r.regionName`)
	if err != nil {
		return nil, fmt.Errorf("ESPGetOfficerLedger: %w", err)
	}
	defer rows.Close()
	var out []ESPOfficerLedgerRow
	for rows.Next() {
		var r ESPOfficerLedgerRow
		if err := rows.Scan(&r.RegionID, &r.RegionName, &r.LiveCount, &r.LastSpawnAt, &r.LastKillAt,
			&r.LastTypeID, &r.LastTypeName, &r.LastSystemID, &r.LastSystem,
			&r.LastFactionID, &r.FactionName); err != nil {
			return nil, fmt.Errorf("ESPGetOfficerLedger scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ESPCountOfficerListingsAboveSec counts officer listings in a region eligible
// above a security (the page's warning that officers can reach high-sec).
func ESPCountOfficerListingsAboveSec(ctx context.Context, db *sql.DB, regionID int64, sec float64) int {
	var n int
	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM npcRegionSpawns WHERE regionID = ? AND role = 'officer' AND maxSec > ?`,
		regionID, sec).Scan(&n)
	return n
}

// ESPFixRegionOfficerMaxSec applies the officer repair to one region: any
// officer-group hull becomes role 'officer', maxSec 0.0, shipClass 'battleship'.
func ESPFixRegionOfficerMaxSec(ctx context.Context, db *sql.DB, regionID int64) (int, error) {
	fixed := 0
	stmts := []string{
		`UPDATE npcRegionSpawns n
		 JOIN invTypes t ON t.typeID = n.typeID
		 JOIN invGroups g ON g.groupID = t.groupID
		 SET n.role = 'officer'
		 WHERE n.regionID = ? AND g.groupName LIKE 'Asteroid %Officer' AND n.role <> 'officer'`,
		`UPDATE npcRegionSpawns n
		 JOIN invTypes t ON t.typeID = n.typeID
		 JOIN invGroups g ON g.groupID = t.groupID
		 SET n.maxSec = 0.0
		 WHERE n.regionID = ? AND g.groupName LIKE 'Asteroid %Officer' AND n.maxSec > 0.0`,
		`UPDATE npcRegionSpawns n
		 JOIN invTypes t ON t.typeID = n.typeID
		 JOIN invGroups g ON g.groupID = t.groupID
		 SET n.shipClass = 'battleship'
		 WHERE n.regionID = ? AND g.groupName LIKE 'Asteroid %Officer' AND n.shipClass <> 'battleship'`,
	}
	for _, q := range stmts {
		res, err := db.ExecContext(ctx, q, regionID)
		if err != nil {
			return fixed, fmt.Errorf("ESPFixRegionOfficerMaxSec(%d): %w", regionID, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			fixed += int(n)
		}
	}
	return fixed, nil
}

// ---- ESP Global Belt Settings (the solarSystemID=0 npcSpawners row) ----

// ESPSaveGlobalBeltSettings creates or updates the global belt-settings row.
// fullClearDelaySec is the cooldown between belt waves; wave size belongs to
// the spawner rules.
func ESPSaveGlobalBeltSettings(ctx context.Context, db *sql.DB, fullClearDelaySec int,
	scatterRadius float64, warpIn bool) error {
	warpInInt := 0
	if warpIn {
		warpInInt = 1
	}
	var exists int
	db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM npcSpawners WHERE solarSystemID = 0 AND celestialGroup = 9`).Scan(&exists)
	if exists > 0 {
		_, err := db.ExecContext(ctx, `
			UPDATE npcSpawners SET fullClearDelaySec=?, scatterRadius=?, warpIn=?
			WHERE solarSystemID = 0 AND celestialGroup = 9`,
			fullClearDelaySec, scatterRadius, warpInInt)
		if err != nil {
			return fmt.Errorf("ESPSaveGlobalBeltSettings update: %w", err)
		}
		return nil
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO npcSpawners
			(solarSystemID, celestialGroup, templateID, factionID, maxNPCs,
			 fullClearDelaySec, scatterRadius, warpIn)
		VALUES (0, 9, 0, 0, 0, ?, ?, ?)`,
		fullClearDelaySec, scatterRadius, warpInInt)
	if err != nil {
		return fmt.Errorf("ESPSaveGlobalBeltSettings insert: %w", err)
	}
	return nil
}

// ESPSetBeltReseedCooldown writes only the wave timer (fullClearDelaySec) on
// the global belt row. With no row it creates one whose scatter is 0, meaning
// the security-scaled default (see LoadForSystem).
func ESPSetBeltReseedCooldown(ctx context.Context, db *sql.DB, sec int) error {
	res, err := db.ExecContext(ctx, `
		UPDATE npcSpawners SET fullClearDelaySec = ?
		WHERE solarSystemID = 0 AND celestialGroup = 9`, sec)
	if err != nil {
		return fmt.Errorf("ESPSetBeltReseedCooldown update: %w", err)
	}
	var exists int
	if n, _ := res.RowsAffected(); n == 0 {
		// RowsAffected is 0 for an unchanged value too, so check before inserting.
		db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM npcSpawners WHERE solarSystemID = 0 AND celestialGroup = 9`).Scan(&exists)
		if exists == 0 {
			if _, err := db.ExecContext(ctx, `
				INSERT INTO npcSpawners
					(solarSystemID, celestialGroup, templateID, factionID, maxNPCs,
					 fullClearDelaySec, scatterRadius, warpIn)
				VALUES (0, 9, 0, 0, 0, ?, 0, 0)`, sec); err != nil {
				return fmt.Errorf("ESPSetBeltReseedCooldown insert: %w", err)
			}
		}
	}
	return nil
}

// ESPDeleteGlobalBeltSettings removes the global belt-settings row, returning
// belts to built-in security scaling.
func ESPDeleteGlobalBeltSettings(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx,
		`DELETE FROM npcSpawners WHERE solarSystemID = 0 AND celestialGroup = 9`)
	if err != nil {
		return fmt.Errorf("ESPDeleteGlobalBeltSettings: %w", err)
	}
	return nil
}
