package npc

import (
	"context"
	"database/sql"
	"gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/safego"
	"log"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

// SpawnerManager holds all active spawners across all systems.
type SpawnerManager struct {
	mu           sync.RWMutex
	spawners     map[int64][]*Spawner           // solarSystemID -> spawners
	npcToSpawner map[uint32]*Spawner            // entityID -> owning spawner
	systems      map[int64]*destiny.SolarSystem // solarSystemID -> system ref

	// regionRosters is preloaded at boot. ReloadRegionRosters replaces entries after a Region
	// Spawns edit, so every read and write holds rosterMu. Never nest rosterMu inside mu.
	rosterMu      sync.RWMutex
	regionRosters map[int64][]db.RegionSpawnRow // regionID -> spawn roster

	// Preloaded at boot; read-only after PreloadRosters returns.
	systemRegion  map[int64]int64 // solarSystemID -> regionID
	defaultConfig *db.SpawnerRow  // global belt defaults (solarSystemID=0)

	// reseedCooldownSec is the live wave timer: how long a cleared belt waits
	// before it re-rolls. Seeded from defaultConfig at boot, replaced by
	// SetReseedCooldown. 0 = not set, use the default.
	reseedCooldownSec atomic.Int64

	// Preloaded by PreloadOfficerLedger; read-only after it returns.
	sqldb   *sql.DB           // the death path has no DB parameter to pass one
	tiers   []db.SquadTierRow // squad tiers, band-filtered per wave in memory
	waveCfg WaveConfig        // belt-spawn settings snapshot

	// Officer ledger (officer_ledger.go). Its own mutex, never sm.mu: the death
	// path enters NotifyDeath under sm.mu.RLock.
	officerMu           sync.Mutex
	officerLedger       map[int64]*officerLedgerEntry // regionID -> state
	officerMaxPerRegion int
	officerCooldown     time.Duration

	// Free-belt quota (beltwave.go). seedLocks: systemID -> *sync.Mutex held across one belt's
	// quota check and claim. beltCounts: systemID -> int, static belt counts.
	seedLocks  sync.Map
	beltCounts sync.Map
}

// NewSpawnerManager creates an empty SpawnerManager.
func NewSpawnerManager() *SpawnerManager {
	return &SpawnerManager{
		spawners:      make(map[int64][]*Spawner),
		npcToSpawner:  make(map[uint32]*Spawner),
		systems:       make(map[int64]*destiny.SolarSystem),
		officerLedger: make(map[int64]*officerLedgerEntry),
		// Usable before PreloadOfficerLedger runs: belts seeded in that window
		// roll the neutral fallback tier rather than panicking on a nil config.
		waveCfg:             DefaultWaveConfig(),
		officerMaxPerRegion: db.DefaultOfficerMaxAlivePerRegion,
		officerCooldown:     db.DefaultOfficerKillCooldownSec * time.Second,
	}
}

// PreloadRosters bulk-loads NPC region rosters, system-region mappings, and
// the default spawner config at boot. Pass the sysRegion map from a shared
// source (e.g. CelestialCache.SystemRegion) to avoid a duplicate query; if
// nil, the map is loaded independently.
func (sm *SpawnerManager) PreloadRosters(ctx context.Context, sqldb *sql.DB, sysRegion map[int64]int64) {
	var err error

	// Tiers, settings and the officer ledger come from PreloadOfficerLedger,
	// which runs after EnsureSpawnerTables.
	sm.sqldb = sqldb

	if sysRegion != nil {
		sm.systemRegion = sysRegion
	} else {
		sm.systemRegion, err = db.GetAllSystemRegionMap(ctx, sqldb)
		if err != nil {
			log.Printf("[spawner] PreloadRosters: GetAllSystemRegionMap failed: %v — will query per-system", err)
		}
	}

	rosters, err := db.GetAllRegionSpawns(ctx, sqldb)
	if err != nil {
		log.Printf("[spawner] PreloadRosters: GetAllRegionSpawns failed: %v — will query per-system", err)
	}
	sm.rosterMu.Lock()
	sm.regionRosters = rosters
	sm.rosterMu.Unlock()

	sm.defaultConfig, err = db.GetDefaultSpawnerConfig(ctx, sqldb, AsteroidBeltGroupID)
	if err != nil {
		log.Printf("[spawner] PreloadRosters: GetDefaultSpawnerConfig failed: %v", err)
	}
	if sm.defaultConfig != nil {
		sm.SetReseedCooldown(sm.defaultConfig.FullClearDelaySec)
	}

	regionCount := len(rosters)
	sysCount := len(sm.systemRegion)
	var totalRows int
	for _, r := range rosters {
		totalRows += len(r)
	}
	log.Printf("[spawner] preloaded %d region rosters (%d spawn rows), %d system-region mappings",
		regionCount, totalRows, sysCount)
}

// LoadForSystem creates belt spawners for a solar system from its region's
// spawn list. Idempotent for the same SolarSystem instance. If the system was
// torn down and recreated (new pointer, same ID), stale spawner state is
// discarded and spawners are rebuilt against the new instance.
func (sm *SpawnerManager) LoadForSystem(ctx context.Context, sqldb *sql.DB,
	ss *destiny.SolarSystem) error {

	sm.mu.RLock()
	oldSS, exists := sm.systems[ss.ID]
	sm.mu.RUnlock()
	if exists {
		if oldSS == ss {
			return nil
		}
		// System instance was recreated: drop state tied to the old one.
		sm.UnloadSystem(ss.ID)
	}

	var spawners []*Spawner

	// Use preloaded maps when available, fall back to per-system DB queries.
	var regionID int64
	if sm.systemRegion != nil {
		regionID = sm.systemRegion[ss.ID]
	} else {
		regionID = db.GetRegionIDForSystem(ctx, sqldb, ss.ID)
	}

	var roster []db.RegionSpawnRow
	var err error
	sm.rosterMu.RLock()
	preloaded := sm.regionRosters != nil
	if preloaded {
		roster = sm.regionRosters[regionID]
	}
	sm.rosterMu.RUnlock()
	if !preloaded {
		roster, err = db.GetRegionSpawnsForRegion(ctx, sqldb, regionID)
		if err != nil {
			return err
		}
	}

	if len(roster) > 0 {
		// Security-scaled defaults, optionally overridden by the global
		// belt-settings row (solarSystemID = 0).
		p := AutoBeltDefaults(ss.Security)
		warpIn := false
		dflt := sm.defaultConfig
		if dflt == nil {
			dflt, _ = db.GetDefaultSpawnerConfig(ctx, sqldb, AsteroidBeltGroupID)
		}
		if dflt != nil {
			p.SeedCooldown = dflt.FullClearDelaySec
			if dflt.ScatterRadius > 0 { // 0 = keep the security-scaled scatter
				p.ScatterRadius = dflt.ScatterRadius
			}
			warpIn = dflt.WarpIn
		}

		base := &Spawner{
			SystemID:      ss.ID,
			RegionID:      regionID,
			Roster:        roster,
			SeedCooldown:  time.Duration(p.SeedCooldown) * time.Second,
			ScatterRadius: p.ScatterRadius,
			WarpIn:        warpIn,
			liveNPCs:      make(map[uint32]bool),
			smgr:          sm,
		}
		spawners = sm.matchCelestials(ss, AsteroidBeltGroupID, base)
	}

	sm.mu.Lock()
	sm.spawners[ss.ID] = spawners
	sm.systems[ss.ID] = ss
	sm.mu.Unlock()

	log.Printf("[spawner] loaded %d belt spawners for system %d (region=%d, roster=%d types)",
		len(spawners), ss.ID, regionID, len(roster))
	return nil
}

// ReloadRegionRosters re-reads the belt rosters of the given regions after a Region Spawns edit.
// Spawners already built in those regions take the new roster, and a loaded system whose region
// had no listings gets its belt spawners built now.
func (sm *SpawnerManager) ReloadRegionRosters(ctx context.Context, regionIDs ...int64) {
	if sm == nil || sm.sqldb == nil {
		return
	}
	for _, regionID := range regionIDs {
		roster, err := db.GetRegionSpawnsForRegion(ctx, sm.sqldb, regionID)
		if err != nil {
			log.Printf("[spawner] reload roster region %d: %v", regionID, err)
			continue
		}
		sm.rosterMu.Lock()
		if sm.regionRosters != nil {
			sm.regionRosters[regionID] = roster
		}
		sm.rosterMu.Unlock()

		// Collect under mu, then lock each spawner on its own: never hold mu while taking s.mu.
		type loaded struct {
			ss       *destiny.SolarSystem
			spawners []*Spawner
		}
		var systems []loaded
		sm.mu.RLock()
		for id, ss := range sm.systems {
			systems = append(systems, loaded{ss, sm.spawners[id]})
		}
		sm.mu.RUnlock()

		updated, built := 0, 0
		for _, l := range systems {
			if sm.regionForSystem(ctx, l.ss.ID) != regionID {
				continue
			}
			if len(l.spawners) == 0 {
				if len(roster) == 0 {
					continue
				}
				// LoadForSystem returns early for a system it already holds.
				sm.mu.Lock()
				if sm.systems[l.ss.ID] == l.ss && len(sm.spawners[l.ss.ID]) == 0 {
					delete(sm.systems, l.ss.ID)
				}
				sm.mu.Unlock()
				if err := sm.LoadForSystem(ctx, sm.sqldb, l.ss); err != nil {
					log.Printf("[spawner] reload: LoadForSystem %d: %v", l.ss.ID, err)
					continue
				}
				built++
				continue
			}
			for _, sp := range l.spawners {
				sp.mu.Lock()
				sp.Roster = roster
				sp.mu.Unlock()
			}
			updated++
		}
		log.Printf("[spawner] region %d roster reloaded: %d types, %d loaded systems updated, %d built",
			regionID, len(roster), updated, built)
	}
}

// matchCelestials creates one spawner per celestial of groupID, copying the
// base config field by field (Spawner holds a mutex and per-belt maps). A field
// set in LoadForSystem but not here is silently zero on every live spawner.
func (sm *SpawnerManager) matchCelestials(ss *destiny.SolarSystem, groupID int64, base *Spawner) []*Spawner {
	var result []*Spawner
	for i, slim := range ss.CelSlims {
		if slim.GroupID != groupID {
			continue
		}
		s := &Spawner{
			SystemID:      ss.ID,
			RegionID:      base.RegionID,
			CelestialID:   slim.ItemID,
			AnchorPos:     ss.CelBalls[i].Pos,
			Roster:        base.Roster,
			SeedCooldown:  base.SeedCooldown,
			ScatterRadius: base.ScatterRadius,
			WarpIn:        base.WarpIn,
			liveNPCs:      make(map[uint32]bool),
			officerNPCs:   make(map[uint32]bool),
			smgr:          base.smgr,
		}
		result = append(result, s)
	}
	// Belts tick in this order, and with the free-belt quota the first to ask takes the open
	// slots. Shuffled so the free belts on a fresh load are not always the last ones listed.
	rand.Shuffle(len(result), func(i, j int) { result[i], result[j] = result[j], result[i] })
	return result
}

// UnloadSystem removes all spawner state for a solar system: spawners, the
// system reference the tick loop uses, and npcToSpawner entries. Rats are rows
// and survive: health is written back, entity IDs cleared. Only in-memory-path
// officers are released here. See docs/npc-behaviour.md#officer-ledger.
func (sm *SpawnerManager) UnloadSystem(sysID int64) {
	released := map[int64]int{}

	sm.mu.RLock()
	spawners := sm.spawners[sysID]
	ss := sm.systems[sysID]
	sm.mu.RUnlock()

	if sm.sqldb != nil && ss != nil {
		for _, s := range spawners {
			s.SaveLiveHealth(ss, sm.sqldb)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := db.UnbindBeltRatsForSystem(ctx, sm.sqldb, sysID); err != nil {
			log.Printf("[spawner] UnbindBeltRatsForSystem(%d): %v", sysID, err)
		}
		cancel()
	}

	sm.mu.Lock()
	for entityID, s := range sm.npcToSpawner {
		if s.SystemID == sysID {
			delete(sm.npcToSpawner, entityID)
		}
	}
	for _, s := range sm.spawners[sysID] {
		if n := s.OfficerCount(); n > 0 {
			released[s.RegionID] += n
		}
	}
	delete(sm.spawners, sysID)
	delete(sm.systems, sysID)
	sm.mu.Unlock()

	// Outside sm.mu: ReleaseOfficer takes officerMu, and the two locks must
	// never nest.
	for regionID, n := range released {
		for i := 0; i < n; i++ {
			sm.ReleaseOfficer(regionID, false)
		}
	}
}

// NotifyDeath routes a death to the owning spawner, then settles the region
// ledger if the dead ball was that belt's in-memory officer and the persisted
// row off the death path. The ledger update runs after every sm.mu section
// is released, since Spawner.NotifyDeath runs under sm.mu.
func (sm *SpawnerManager) NotifyDeath(entityID uint32) {
	sm.mu.RLock()
	s := sm.npcToSpawner[entityID]
	sm.mu.RUnlock()
	if s == nil {
		return
	}
	wasOfficer := s.NotifyDeath(entityID)
	sm.mu.Lock()
	delete(sm.npcToSpawner, entityID)
	sm.mu.Unlock()
	if wasOfficer {
		// Legacy in-memory path only. A persisted officer is never in
		// officerNPCs; its ledger release rides KillBeltRatEntity below.
		sm.ReleaseOfficer(s.RegionID, true)
	}
	// Settle the persistent row off the death path: this is a DB write and the
	// caller is mid-kill with a client waiting on the explosion.
	if sm.sqldb != nil {
		go sm.KillBeltRatEntity(entityID)
	}
}

// RegisterNPC associates an NPC entityID with its owning spawner.
func (sm *SpawnerManager) RegisterNPC(entityID uint32, s *Spawner) {
	sm.mu.Lock()
	sm.npcToSpawner[entityID] = s
	sm.mu.Unlock()
}

// TickSystem runs a single tick for all spawners in a system.
func (sm *SpawnerManager) TickSystem(ss *destiny.SolarSystem, mgr *NPCManager, sqldb *sql.DB) {
	sm.mu.RLock()
	spawners := sm.spawners[ss.ID]
	sm.mu.RUnlock()
	for _, s := range spawners {
		s.Tick(ss, mgr, sqldb)
	}
}

// StartTickLoop runs a background goroutine that ticks all spawners every 5 seconds.
func (sm *SpawnerManager) StartTickLoop(mgr *NPCManager, sqldb *sql.DB) {
	go func() {
		defer safego.Recover("StartTickLoop")
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			sm.mu.RLock()
			systemIDs := make([]int64, 0, len(sm.systems))
			for id := range sm.systems {
				systemIDs = append(systemIDs, id)
			}
			sm.mu.RUnlock()

			for _, sysID := range systemIDs {
				sm.mu.RLock()
				ss := sm.systems[sysID]
				spawners := sm.spawners[sysID]
				sm.mu.RUnlock()
				if ss == nil {
					continue
				}
				for _, s := range spawners {
					s.Tick(ss, mgr, sqldb)
				}
			}
		}
	}()
}

// DefaultSeedCooldownSec is how long a belt waits between full waves. Keeping
// it well above the tick interval is what stops every belt in a system sitting
// permanently full: clear one and it stays quiet for a while.
const DefaultSeedCooldownSec = 300

// AutoBeltParams holds security-scaled defaults for belt spawners. Wave size
// lives in the spawner rules.
type AutoBeltParams struct {
	SeedCooldown  int // seconds between waves at one belt
	ScatterRadius float64
}

// AutoBeltDefaults returns spawner parameters scaled to system security.
func AutoBeltDefaults(security float64) AutoBeltParams {
	switch {
	case security >= 0.9:
		return AutoBeltParams{DefaultSeedCooldownSec, 15000}
	case security >= 0.7:
		return AutoBeltParams{DefaultSeedCooldownSec, 18000}
	case security >= 0.5:
		return AutoBeltParams{DefaultSeedCooldownSec, 20000}
	case security >= 0.3:
		return AutoBeltParams{DefaultSeedCooldownSec, 25000}
	case security >= 0.0:
		return AutoBeltParams{DefaultSeedCooldownSec, 30000}
	default: // negative sec
		return AutoBeltParams{DefaultSeedCooldownSec, 35000}
	}
}
