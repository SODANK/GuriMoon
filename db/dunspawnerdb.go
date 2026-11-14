package db

// Dungeon spawners: rules that keep a number of open (non-mission) dungeon sites alive, each a
// beacon any pilot can warp to. A spawner is a dungeon template plus filters that all combine (a
// region list, empty = anywhere, a security band, a per-system cap) and a target count of live
// sites. The runtime half (cmd/gurimoon/opensites.go) keeps that many sites alive, shares one
// copy of each site between pilots and despawns a cleared site. Only the site rows are stored;
// the beacon ball, the NPCs and the kill state live in memory.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"time"
)

const settingOpenSiteClearMinutes = "dunspawner.clearDespawnMinutes"

// defaultExcludedRegions are left out of a spawner with no region list: the Jove regions (no
// stargate leads in, so a site there could never be cleared) and the rogue-drone regions, which
// appear under their unnamed IDs in this data. A spawner that names one explicitly still gets it.
var defaultExcludedRegions = []int64{
	10000004, 10000017, 10000019, // Jove: UUA-F4, J7HZ-F, A821-A
	10000013, 10000018, 10000021, 10000027, // drones: LQ-0QN, VU-WU2, 6HL8-L, 7-KXBJ
	10000034, 10000040, 10000053, 10000066, // drones: 87-1CW, G5KW-3, H3J8-U, S-I6VU
}

// EnsureDungeonSpawnerTables creates the spawner rule and live-site tables.
func EnsureDungeonSpawnerTables(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS dunSpawners (
			spawnerID           INT AUTO_INCREMENT PRIMARY KEY,
			name                VARCHAR(100) NOT NULL DEFAULT '',
			dungeonVID          INT NOT NULL,
			regionIDs           VARCHAR(2048) NOT NULL DEFAULT '',
			secMin              DOUBLE NOT NULL DEFAULT -1,
			secMax              DOUBLE NOT NULL DEFAULT 1,
			maxPerSystem        INT NOT NULL DEFAULT 1,
			targetActive        INT NOT NULL DEFAULT 1,
			respawnDelayMinutes INT NOT NULL DEFAULT 30,
			enabled             TINYINT NOT NULL DEFAULT 1,
			lastDespawnAt       BIGINT NOT NULL DEFAULT 0,
			placement           VARCHAR(16) NOT NULL DEFAULT 'planet'
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`,
		`ALTER TABLE dunSpawners ADD COLUMN IF NOT EXISTS placement VARCHAR(16) NOT NULL DEFAULT 'planet'`,
		`ALTER TABLE dunSpawners ADD COLUMN IF NOT EXISTS beaconVisible TINYINT NOT NULL DEFAULT 1`,
		`CREATE TABLE IF NOT EXISTS dunOpenSites (
			siteID        INT AUTO_INCREMENT PRIMARY KEY,
			spawnerID     INT NOT NULL,
			dungeonVID    INT NOT NULL,
			solarSystemID INT NOT NULL,
			planetID      INT NOT NULL DEFAULT 0,
			x             DOUBLE NOT NULL,
			y             DOUBLE NOT NULL,
			z             DOUBLE NOT NULL,
			createdAt     BIGINT NOT NULL,
			clearedAt     BIGINT DEFAULT NULL,
			INDEX idx_opensite_spawner (spawnerID),
			INDEX idx_opensite_system (solarSystemID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`,
		`ALTER TABLE dunOpenSites ADD COLUMN IF NOT EXISTS beaconVisible TINYINT NOT NULL DEFAULT 1`,
	}
	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("EnsureDungeonSpawnerTables: %w", err)
		}
	}
	return nil
}

// OpenSiteClearMinutes is how long a cleared site stays up before it
// despawns: long enough to loot the wrecks.
func OpenSiteClearMinutes(ctx context.Context, db *sql.DB) int {
	return settingInt(ctx, db, settingOpenSiteClearMinutes, 10, 1, 1440)
}

// SetOpenSiteClearMinutes stores the post-clear despawn delay.
func SetOpenSiteClearMinutes(ctx context.Context, db *sql.DB, minutes int) error {
	if minutes < 1 {
		minutes = 1
	}
	if minutes > 1440 {
		minutes = 1440
	}
	return SetServerSetting(ctx, db, settingOpenSiteClearMinutes, strconv.Itoa(minutes))
}

// ─────────────────────────────────────────────────────────────────────────────
// Spawner rules
// ─────────────────────────────────────────────────────────────────────────────

// Where a spawner's sites go (dunSpawners.placement).
const (
	// SitePlacementPlanet puts a site in orbit of a planet, at the distance
	// that system's belts keep from theirs.
	SitePlacementPlanet = "planet"
	// SitePlacementDeadspace puts a site out in open space between the
	// planets' orbits, far from every celestial.
	SitePlacementDeadspace = "deadspace"
)

// DungeonSpawner is one spawner rule.
type DungeonSpawner struct {
	SpawnerID           int64
	Name                string
	DungeonVID          int64
	DungeonName         string // resolved on list, empty if the dungeon is gone
	RegionIDs           []int64
	SecMin, SecMax      float64
	MaxPerSystem        int
	TargetActive        int
	RespawnDelayMinutes int
	Enabled             bool
	LastDespawnAt       int64  // unix seconds, 0 = never
	Placement           string // SitePlacementPlanet or SitePlacementDeadspace
	// BeaconVisible puts each site's beacon on every overview (the default). Off, a site is a
	// grid-local cosmic signature that only a probe Analyze finds. Copied onto each site at spawn, so
	// an edit applies to new sites only.
	BeaconVisible bool
}

// IsDeadspace reports whether the rule places its sites in open space.
func (sp DungeonSpawner) IsDeadspace() bool { return sp.Placement == SitePlacementDeadspace }

// HasRegion reports whether the spawner's region list names regionID.
func (sp DungeonSpawner) HasRegion(regionID int64) bool {
	for _, r := range sp.RegionIDs {
		if r == regionID {
			return true
		}
	}
	return false
}

// Validate checks a rule's numbers before it is stored.
func (sp DungeonSpawner) Validate() error {
	switch {
	case strings.TrimSpace(sp.Name) == "":
		return fmt.Errorf("a spawner needs a name")
	case sp.DungeonVID <= 0:
		return fmt.Errorf("pick a dungeon")
	case sp.SecMin < -1 || sp.SecMax > 1:
		return fmt.Errorf("security must lie between -1.0 and 1.0")
	case sp.SecMin > sp.SecMax:
		return fmt.Errorf("security min %.1f is above max %.1f", sp.SecMin, sp.SecMax)
	case sp.MaxPerSystem < 1:
		return fmt.Errorf("max per system must be at least 1")
	case sp.TargetActive < 0:
		return fmt.Errorf("target sites cannot be negative")
	case sp.RespawnDelayMinutes < 0:
		return fmt.Errorf("respawn delay cannot be negative")
	case sp.Placement != SitePlacementPlanet && sp.Placement != SitePlacementDeadspace:
		return fmt.Errorf("choose whether sites spawn in deadspace or in orbit of a planet")
	}
	return nil
}

func joinIDs(ids []int64) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	return strings.Join(parts, ",")
}

func splitIDs(s string) []int64 {
	var out []int64
	for _, p := range strings.Split(s, ",") {
		if id, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64); err == nil && id > 0 {
			out = append(out, id)
		}
	}
	return out
}

const dungeonSpawnerSelect = `
	SELECT s.spawnerID, s.name, s.dungeonVID, COALESCE(d.dungeonVName, ''), s.regionIDs,
	       s.secMin, s.secMax, s.maxPerSystem, s.targetActive, s.respawnDelayMinutes,
	       s.enabled, s.lastDespawnAt, s.placement, s.beaconVisible
	  FROM dunSpawners s
	  LEFT JOIN dunDungeons d ON d.dungeonVID = s.dungeonVID`

func scanDungeonSpawner(sc interface{ Scan(...any) error }) (DungeonSpawner, error) {
	var sp DungeonSpawner
	var regions string
	err := sc.Scan(&sp.SpawnerID, &sp.Name, &sp.DungeonVID, &sp.DungeonName, &regions,
		&sp.SecMin, &sp.SecMax, &sp.MaxPerSystem, &sp.TargetActive, &sp.RespawnDelayMinutes,
		&sp.Enabled, &sp.LastDespawnAt, &sp.Placement, &sp.BeaconVisible)
	sp.RegionIDs = splitIDs(regions)
	return sp, err
}

// ListDungeonSpawners returns every spawner rule in ID order.
func ListDungeonSpawners(ctx context.Context, db *sql.DB) ([]DungeonSpawner, error) {
	rows, err := db.QueryContext(ctx, dungeonSpawnerSelect+` ORDER BY s.spawnerID`)
	if err != nil {
		return nil, fmt.Errorf("ListDungeonSpawners: %w", err)
	}
	defer rows.Close()
	var out []DungeonSpawner
	for rows.Next() {
		sp, err := scanDungeonSpawner(rows)
		if err != nil {
			return nil, fmt.Errorf("ListDungeonSpawners scan: %w", err)
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

// GetDungeonSpawner returns one rule, or nil when it does not exist.
func GetDungeonSpawner(ctx context.Context, db *sql.DB, spawnerID int64) (*DungeonSpawner, error) {
	sp, err := scanDungeonSpawner(db.QueryRowContext(ctx, dungeonSpawnerSelect+` WHERE s.spawnerID = ?`, spawnerID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("GetDungeonSpawner(%d): %w", spawnerID, err)
	}
	return &sp, nil
}

// SaveDungeonSpawner inserts a rule (SpawnerID 0) or updates one. The dungeon
// must have at least one rat that spawns with its room: a site with nothing to
// kill could never clear, and sites only ever leave by being cleared.
func SaveDungeonSpawner(ctx context.Context, db *sql.DB, sp DungeonSpawner) (int64, error) {
	if err := sp.Validate(); err != nil {
		return 0, err
	}
	census, err := LoadDungeonRatCensus(ctx, db, sp.DungeonVID)
	if err != nil {
		return 0, err
	}
	if census.BaseCount() == 0 {
		return 0, fmt.Errorf("that dungeon has no NPC that spawns with its rooms, so its sites could never be cleared")
	}
	regions := joinIDs(sp.RegionIDs)
	if len(regions) > 2048 {
		return 0, fmt.Errorf("too many regions selected")
	}
	if sp.SpawnerID == 0 {
		res, err := db.ExecContext(ctx, `
			INSERT INTO dunSpawners (name, dungeonVID, regionIDs, secMin, secMax, maxPerSystem,
			                         targetActive, respawnDelayMinutes, enabled, placement, beaconVisible)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			strings.TrimSpace(sp.Name), sp.DungeonVID, regions, sp.SecMin, sp.SecMax,
			sp.MaxPerSystem, sp.TargetActive, sp.RespawnDelayMinutes, sp.Enabled, sp.Placement, sp.BeaconVisible)
		if err != nil {
			return 0, fmt.Errorf("SaveDungeonSpawner insert: %w", err)
		}
		return res.LastInsertId()
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE dunSpawners SET name = ?, dungeonVID = ?, regionIDs = ?, secMin = ?, secMax = ?,
		       maxPerSystem = ?, targetActive = ?, respawnDelayMinutes = ?, enabled = ?, placement = ?,
		       beaconVisible = ?
		 WHERE spawnerID = ?`,
		strings.TrimSpace(sp.Name), sp.DungeonVID, regions, sp.SecMin, sp.SecMax,
		sp.MaxPerSystem, sp.TargetActive, sp.RespawnDelayMinutes, sp.Enabled, sp.Placement,
		sp.BeaconVisible, sp.SpawnerID); err != nil {
		return 0, fmt.Errorf("SaveDungeonSpawner update: %w", err)
	}
	return sp.SpawnerID, nil
}

// SetDungeonSpawnerEnabled switches a rule on or off. Off stops new sites;
// the live ones stay until cleared.
func SetDungeonSpawnerEnabled(ctx context.Context, db *sql.DB, spawnerID int64, on bool) error {
	_, err := db.ExecContext(ctx, `UPDATE dunSpawners SET enabled = ? WHERE spawnerID = ?`, on, spawnerID)
	return err
}

// DeleteDungeonSpawner removes a rule. The caller despawns its live sites
// first; any row left behind is removed here so nothing is orphaned.
func DeleteDungeonSpawner(ctx context.Context, db *sql.DB, spawnerID int64) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM dunOpenSites WHERE spawnerID = ?`, spawnerID); err != nil {
		return fmt.Errorf("DeleteDungeonSpawner sites: %w", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM dunSpawners WHERE spawnerID = ?`, spawnerID); err != nil {
		return fmt.Errorf("DeleteDungeonSpawner: %w", err)
	}
	return nil
}

// StampSpawnerDespawn records when one of the rule's sites last went away;
// the respawn delay counts from it.
func StampSpawnerDespawn(ctx context.Context, db *sql.DB, spawnerID, unix int64) error {
	_, err := db.ExecContext(ctx, `UPDATE dunSpawners SET lastDespawnAt = ? WHERE spawnerID = ?`, unix, spawnerID)
	return err
}

// ─────────────────────────────────────────────────────────────────────────────
// Live sites
// ─────────────────────────────────────────────────────────────────────────────

// OpenSite is one live site.
type OpenSite struct {
	SiteID        int64
	SpawnerID     int64
	DungeonVID    int64
	SolarSystemID int64
	PlanetID      int64 // 0 = a deadspace site
	X, Y, Z       float64
	CreatedAt     int64 // unix seconds
	ClearedAt     int64 // unix seconds, 0 = not cleared
	// BeaconVisible false = a probe-only site: its marker is a grid-local
	// cosmic signature, not an overview beacon (see DungeonSpawner).
	BeaconVisible bool
}

// InsertOpenSite stores a new site and returns its ID.
func InsertOpenSite(ctx context.Context, db *sql.DB, s OpenSite) (int64, error) {
	res, err := db.ExecContext(ctx, `
		INSERT INTO dunOpenSites (spawnerID, dungeonVID, solarSystemID, planetID, x, y, z, createdAt, beaconVisible)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.SpawnerID, s.DungeonVID, s.SolarSystemID, s.PlanetID, s.X, s.Y, s.Z, s.CreatedAt, s.BeaconVisible)
	if err != nil {
		return 0, fmt.Errorf("InsertOpenSite: %w", err)
	}
	return res.LastInsertId()
}

// ListOpenSites returns every live site.
func ListOpenSites(ctx context.Context, db *sql.DB) ([]OpenSite, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT siteID, spawnerID, dungeonVID, solarSystemID, planetID, x, y, z,
		       createdAt, COALESCE(clearedAt, 0), beaconVisible
		  FROM dunOpenSites ORDER BY siteID`)
	if err != nil {
		return nil, fmt.Errorf("ListOpenSites: %w", err)
	}
	defer rows.Close()
	var out []OpenSite
	for rows.Next() {
		var s OpenSite
		if err := rows.Scan(&s.SiteID, &s.SpawnerID, &s.DungeonVID, &s.SolarSystemID, &s.PlanetID,
			&s.X, &s.Y, &s.Z, &s.CreatedAt, &s.ClearedAt, &s.BeaconVisible); err != nil {
			return nil, fmt.Errorf("ListOpenSites scan: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SetOpenSiteCleared stamps (unix > 0) or lifts (0) a site's cleared time.
func SetOpenSiteCleared(ctx context.Context, db *sql.DB, siteID, unix int64) error {
	at := sql.NullInt64{Int64: unix, Valid: unix > 0}
	_, err := db.ExecContext(ctx, `UPDATE dunOpenSites SET clearedAt = ? WHERE siteID = ?`, at, siteID)
	return err
}

// DeleteOpenSite removes a site row. found is false when it was already gone.
func DeleteOpenSite(ctx context.Context, db *sql.DB, siteID int64) (found bool, err error) {
	res, err := db.ExecContext(ctx, `DELETE FROM dunOpenSites WHERE siteID = ?`, siteID)
	if err != nil {
		return false, fmt.Errorf("DeleteOpenSite(%d): %w", siteID, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Where a spawner may place sites
// ─────────────────────────────────────────────────────────────────────────────

// SpawnerSystem is one system a spawner may place a site in.
type SpawnerSystem struct {
	SolarSystemID int64
	RegionID      int64
	Security      float64
}

// SpawnerCandidateSystems lists the systems matching a region list (empty = every region bar the
// Jove and drone ones) and a security band, keeping only systems with a planet to sit beside.
// The band is read as the client displays security (rounded to one decimal), so it is truesec in
// [min-0.05, max+0.05).
func SpawnerCandidateSystems(ctx context.Context, db *sql.DB, regionIDs []int64, secMin, secMax float64) ([]SpawnerSystem, error) {
	q := `SELECT s.solarSystemID, s.regionID, s.security
	        FROM mapSolarSystems s
	       WHERE s.security >= ? AND s.security < ?
	         AND EXISTS (SELECT 1 FROM mapDenormalize d
	                      WHERE d.solarSystemID = s.solarSystemID AND d.groupID = 7)`
	args := []any{secMin - 0.05, secMax + 0.05}
	ids, not := regionIDs, ""
	if len(ids) == 0 {
		ids, not = defaultExcludedRegions, "NOT "
	}
	q += ` AND s.regionID ` + not + `IN (` + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + `)`
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("SpawnerCandidateSystems: %w", err)
	}
	defer rows.Close()
	var out []SpawnerSystem
	for rows.Next() {
		var s SpawnerSystem
		if err := rows.Scan(&s.SolarSystemID, &s.RegionID, &s.Security); err != nil {
			return nil, fmt.Errorf("SpawnerCandidateSystems scan: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ─────────────────────────────────────────────────────────────────────────────
// The clear rule
// ─────────────────────────────────────────────────────────────────────────────

// DungeonRatCensus is every rat of a dungeon: roomID -> spawnWave group (0 = spawns with the
// room) -> objectIDs. Rats are what RoomNPCObjectIDs counts (ship and entity types with a cruise
// speed or maxVelocity), so structures never hold a site open.
type DungeonRatCensus map[int64]map[int64][]int64

// LoadDungeonRatCensus reads a dungeon's census across all its rooms.
func LoadDungeonRatCensus(ctx context.Context, db *sql.DB, dungeonVID int64) (DungeonRatCensus, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT o.roomID, COALESCE(o.spawnWave, 0), o.objectID
		  FROM dunObjects o
		  JOIN dunRooms r ON r.roomID = o.roomID
		  JOIN invTypes t ON t.typeID = o.typeID
		  JOIN invGroups g ON g.groupID = t.groupID
		 WHERE r.dungeonVID = ?
		   AND g.categoryID IN (6, 11)
		   AND EXISTS (SELECT 1 FROM dgmTypeAttributes a
		                WHERE a.typeID = o.typeID AND a.attributeID IN (37, 508)
		                  AND COALESCE(NULLIF(a.valueFloat, 0), CAST(a.valueInt AS DECIMAL(20,6)), 0) > 0)`,
		dungeonVID)
	if err != nil {
		return nil, fmt.Errorf("LoadDungeonRatCensus dungeonVID=%d: %w", dungeonVID, err)
	}
	defer rows.Close()
	c := DungeonRatCensus{}
	for rows.Next() {
		var room, wave, obj int64
		if err := rows.Scan(&room, &wave, &obj); err != nil {
			return nil, fmt.Errorf("LoadDungeonRatCensus scan: %w", err)
		}
		if wave < 0 {
			wave = 0
		}
		if c[room] == nil {
			c[room] = map[int64][]int64{}
		}
		c[room][wave] = append(c[room][wave], obj)
	}
	return c, rows.Err()
}

// BaseCount is the number of rats that spawn with their rooms, waves aside.
func (c DungeonRatCensus) BaseCount() int {
	n := 0
	for _, waves := range c {
		n += len(waves[0])
	}
	return n
}

// HasRats reports whether a room's wave group holds any rat.
func (c DungeonRatCensus) HasRats(roomID, wave int64) bool {
	return len(c[roomID][wave]) > 0
}

// Count is every NPC of the dungeon: the ones that spawn with their rooms
// plus every wave group, released or not.
func (c DungeonRatCensus) Count() int {
	n := 0
	for _, waves := range c {
		for _, objs := range waves {
			n += len(objs)
		}
	}
	return n
}

// Required is the set a site must kill to count as cleared: every room's base rats plus the rats
// of each wave group a trigger has released. An unreleased wave never spawned, so it does not hold
// the site open. Rooms nobody has entered still count.
func (c DungeonRatCensus) Required(released func(roomID, wave int64) bool) []int64 {
	var out []int64
	for room, waves := range c {
		for wave, objs := range waves {
			if wave == 0 || (released != nil && released(room, wave)) {
				out = append(out, objs...)
			}
		}
	}
	return out
}

// Cleared reports whether every required rat is dead. A census with nothing
// required never clears (SaveDungeonSpawner refuses such a dungeon).
func (c DungeonRatCensus) Cleared(killed func(objectID int64) bool, released func(roomID, wave int64) bool) (dead, required int, cleared bool) {
	req := c.Required(released)
	for _, oid := range req {
		if killed(oid) {
			dead++
		}
	}
	return dead, len(req), len(req) > 0 && dead == len(req)
}

// ─────────────────────────────────────────────────────────────────────────────
// Placement: off a planet, at a distance belts actually keep
// ─────────────────────────────────────────────────────────────────────────────

const (
	groupSun          = 6
	groupAsteroidBelt = 9 // groupPlanet lives in outpostdb.go

	// siteClearanceM keeps a site's grid clear of every other celestial and
	// live site: 500 km beyond the other body's own radius.
	siteClearanceM = 500_000
	// sitePlanetMarginM keeps a site off the planet's surface.
	sitePlanetMarginM = 100_000
	sitePlaceTries    = 12

	// deadspaceClearanceM keeps a deadspace site a quarter AU beyond every
	// celestial's own radius: off every grid, and a real warp from anywhere.
	deadspaceClearanceM = 0.25 * 149_597_870_700
	deadspacePlaceTries = 32
)

// BeltPlanetSample is one asteroid belt's offset from the planet it orbits:
// the distance and the share of it that is vertical (dy/dist, signed).
type BeltPlanetSample struct {
	Dist      float64
	VertRatio float64
}

func beltSample(belt, planet CelestialEntity) (BeltPlanetSample, bool) {
	dx, dy, dz := belt.X-planet.X, belt.Y-planet.Y, belt.Z-planet.Z
	d := math.Sqrt(dx*dx + dy*dy + dz*dz)
	if d <= 0 {
		return BeltPlanetSample{}, false
	}
	return BeltPlanetSample{Dist: d, VertRatio: dy / d}, true
}

var (
	beltSampleMu     sync.Mutex
	beltSampleGlobal []BeltPlanetSample
)

// GlobalBeltPlanetSamples returns every belt→planet offset in the galaxy,
// loaded once. It is the fallback for a system with planets but no belts.
func GlobalBeltPlanetSamples(ctx context.Context, db *sql.DB) ([]BeltPlanetSample, error) {
	beltSampleMu.Lock()
	defer beltSampleMu.Unlock()
	if beltSampleGlobal != nil {
		return beltSampleGlobal, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT b.x, b.y, b.z, p.x, p.y, p.z
		  FROM mapDenormalize b
		  JOIN mapDenormalize p ON p.itemID = b.orbitID AND p.groupID = ?
		 WHERE b.groupID = ?`, groupPlanet, groupAsteroidBelt)
	if err != nil {
		return nil, fmt.Errorf("GlobalBeltPlanetSamples: %w", err)
	}
	defer rows.Close()
	out := make([]BeltPlanetSample, 0, 56000)
	for rows.Next() {
		var b, p CelestialEntity
		if err := rows.Scan(&b.X, &b.Y, &b.Z, &p.X, &p.Y, &p.Z); err != nil {
			return nil, fmt.Errorf("GlobalBeltPlanetSamples scan: %w", err)
		}
		if s, ok := beltSample(b, p); ok {
			out = append(out, s)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	beltSampleGlobal = out
	return out, nil
}

// SitePoint is a position in system space.
type SitePoint struct{ X, Y, Z float64 }

func (a SitePoint) dist(x, y, z float64) float64 {
	dx, dy, dz := a.X-x, a.Y-y, a.Z-z
	return math.Sqrt(dx*dx + dy*dy + dz*dz)
}

// PlaceSiteNearPlanet picks a spot beside a random planet of the system, at a distance and
// inclination drawn from the system's own belt-to-planet offsets (the galaxy's when it has no
// belts). avoid holds the system's other live sites. ok is false when the system has no planet or
// every try landed too close to something.
func PlaceSiteNearPlanet(cels []CelestialEntity, avoid []SitePoint, global []BeltPlanetSample, rng *rand.Rand) (pos SitePoint, planetID int64, ok bool) {
	planets := map[int64]CelestialEntity{}
	var planetIDs []int64
	for _, c := range cels {
		if c.GroupID == groupPlanet {
			planets[c.ItemID] = c
			planetIDs = append(planetIDs, c.ItemID)
		}
	}
	if len(planetIDs) == 0 {
		return SitePoint{}, 0, false
	}
	var local []BeltPlanetSample
	for _, c := range cels {
		if c.GroupID != groupAsteroidBelt {
			continue
		}
		if p, isPlanet := planets[c.OrbitID]; isPlanet {
			if s, ok := beltSample(c, p); ok {
				local = append(local, s)
			}
		}
	}
	samples := local
	if len(samples) == 0 {
		samples = global
	}
	if len(samples) == 0 {
		return SitePoint{}, 0, false
	}

	for try := 0; try < sitePlaceTries; try++ {
		p := planets[planetIDs[rng.Intn(len(planetIDs))]]
		s := samples[rng.Intn(len(samples))]
		az := rng.Float64() * 2 * math.Pi
		vr := math.Max(-1, math.Min(1, s.VertRatio))
		horiz := s.Dist * math.Sqrt(1-vr*vr)
		cand := SitePoint{
			X: p.X + horiz*math.Cos(az),
			Y: p.Y + s.Dist*vr,
			Z: p.Z + horiz*math.Sin(az),
		}
		if siteClear(cand, p, cels, avoid) {
			return cand, p.ItemID, true
		}
	}
	return SitePoint{}, 0, false
}

func siteClear(cand SitePoint, planet CelestialEntity, cels []CelestialEntity, avoid []SitePoint) bool {
	if cand.dist(planet.X, planet.Y, planet.Z) < planet.Radius+sitePlanetMarginM {
		return false
	}
	for _, c := range cels {
		if c.ItemID == planet.ItemID {
			continue
		}
		if cand.dist(c.X, c.Y, c.Z) < c.Radius+siteClearanceM {
			return false
		}
	}
	for _, a := range avoid {
		if cand.dist(a.X, a.Y, a.Z) < siteClearanceM {
			return false
		}
	}
	return true
}

// PlaceSiteInDeadspace picks a spot in open space: a random distance from the star between half
// the innermost planet's orbit and 1.2x the outermost, at one planet's inclination, clear of every
// celestial by deadspaceClearanceM. avoid holds the system's other live sites. ok is false when
// the system has no planet or every try landed too close to something.
func PlaceSiteInDeadspace(cels []CelestialEntity, avoid []SitePoint, rng *rand.Rand) (pos SitePoint, ok bool) {
	var star CelestialEntity
	for _, c := range cels {
		if c.GroupID == groupSun {
			star = c
			break
		}
	}
	var orbits []BeltPlanetSample // each planet's offset from the star
	minR, maxR := math.MaxFloat64, 0.0
	for _, c := range cels {
		if c.GroupID != groupPlanet {
			continue
		}
		if s, ok := beltSample(c, star); ok {
			orbits = append(orbits, s)
			minR, maxR = math.Min(minR, s.Dist), math.Max(maxR, s.Dist)
		}
	}
	if len(orbits) == 0 {
		return SitePoint{}, false
	}
	lo, hi := 0.5*minR, 1.2*maxR
	for try := 0; try < deadspacePlaceTries; try++ {
		r := lo + rng.Float64()*(hi-lo)
		vr := math.Max(-1, math.Min(1, orbits[rng.Intn(len(orbits))].VertRatio))
		az := rng.Float64() * 2 * math.Pi
		horiz := r * math.Sqrt(1-vr*vr)
		cand := SitePoint{
			X: star.X + horiz*math.Cos(az),
			Y: star.Y + r*vr,
			Z: star.Z + horiz*math.Sin(az),
		}
		if deadspaceClear(cand, cels, avoid) {
			return cand, true
		}
	}
	return SitePoint{}, false
}

func deadspaceClear(cand SitePoint, cels []CelestialEntity, avoid []SitePoint) bool {
	for _, c := range cels {
		if cand.dist(c.X, c.Y, c.Z) < c.Radius+deadspaceClearanceM {
			return false
		}
	}
	for _, a := range avoid {
		if cand.dist(a.X, a.Y, a.Z) < siteClearanceM {
			return false
		}
	}
	return true
}

// NowUnix is the clock the site rows are stamped with.
func NowUnix() int64 { return time.Now().Unix() }

// ESPOpenSiteRow is one live site with its names resolved, for the ESP page.
type ESPOpenSiteRow struct {
	OpenSite
	SpawnerName string
	DungeonName string
	SystemName  string
	RegionName  string
	Security    float64
	PlanetName  string
}

// ESPListOpenSites lists every live site, newest first, with names resolved.
func ESPListOpenSites(ctx context.Context, db *sql.DB) ([]ESPOpenSiteRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT o.siteID, o.spawnerID, o.dungeonVID, o.solarSystemID, o.planetID, o.x, o.y, o.z,
		       o.createdAt, COALESCE(o.clearedAt, 0), o.beaconVisible,
		       COALESCE(sp.name, ''), COALESCE(d.dungeonVName, ''),
		       COALESCE(s.solarSystemName, ''), COALESCE(r.regionName, ''), COALESCE(s.security, 0),
		       COALESCE(p.itemName, '')
		  FROM dunOpenSites o
		  LEFT JOIN dunSpawners sp ON sp.spawnerID = o.spawnerID
		  LEFT JOIN dunDungeons d ON d.dungeonVID = o.dungeonVID
		  LEFT JOIN mapSolarSystems s ON s.solarSystemID = o.solarSystemID
		  LEFT JOIN mapRegions r ON r.regionID = s.regionID
		  LEFT JOIN mapDenormalize p ON p.itemID = o.planetID
		 ORDER BY o.siteID DESC`)
	if err != nil {
		return nil, fmt.Errorf("ESPListOpenSites: %w", err)
	}
	defer rows.Close()
	var out []ESPOpenSiteRow
	for rows.Next() {
		var r ESPOpenSiteRow
		if err := rows.Scan(&r.SiteID, &r.SpawnerID, &r.DungeonVID, &r.SolarSystemID, &r.PlanetID,
			&r.X, &r.Y, &r.Z, &r.CreatedAt, &r.ClearedAt, &r.BeaconVisible,
			&r.SpawnerName, &r.DungeonName, &r.SystemName, &r.RegionName, &r.Security,
			&r.PlanetName); err != nil {
			return nil, fmt.Errorf("ESPListOpenSites scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
