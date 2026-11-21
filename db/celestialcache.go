package db

import (
	"context"
	"database/sql"
	"log"
	"sync"

	"gurimoon/syncx"
)

// CelestialCache holds preloaded static map data (security, faction, position) and
// lazily caches celestials per region on first access.
type CelestialCache struct {
	mu            sync.RWMutex
	systems       map[int64][]CelestialEntity // solarSystemID → celestials
	loadedRegions map[int64]bool              // regionID → fully cached
	SystemRegion  map[int64]int64             // solarSystemID → regionID (exported for spawner preload)
	security      map[int64]float64           // solarSystemID → security
	factions      map[int64]int64             // solarSystemID → sovereign factionID
	positions     map[int64]SystemPos         // solarSystemID → galactic position (m)

	// regionLoad coalesces concurrent misses on one region, which each used to load
	// the whole region and throw all but one copy away.
	regionLoad syncx.Flight[int64, map[int64][]CelestialEntity]
}

// SharedCelestialCache is the process's one CelestialCache, installed by main after
// Preload. Modules read through it (CachedSystemCelestials), so InvalidateSystem
// reaches them too.
var SharedCelestialCache *CelestialCache

// CachedSystemCelestials is GetSystemCelestials through SharedCelestialCache when one
// is installed. The slice is shared: callers must treat it as read-only.
func CachedSystemCelestials(ctx context.Context, db *sql.DB, solarSystemID int64) ([]CelestialEntity, error) {
	if cc := SharedCelestialCache; cc != nil {
		return cc.GetCelestials(ctx, db, solarSystemID)
	}
	return GetSystemCelestials(ctx, db, solarSystemID)
}

// NewCelestialCache creates an empty CelestialCache. Call Preload to fill the
// security, faction and system-region maps at boot.
func NewCelestialCache() *CelestialCache {
	return &CelestialCache{
		systems:       make(map[int64][]CelestialEntity),
		loadedRegions: make(map[int64]bool),
	}
}

// Preload loads the system-region map, security values and sovereign factions for
// every solar system. Called once at boot.
func (cc *CelestialCache) Preload(ctx context.Context, db *sql.DB) error {
	sysRegion, err := GetAllSystemRegionMap(ctx, db)
	if err != nil {
		return err
	}
	cc.SystemRegion = sysRegion

	sec, err := GetAllSystemSecurities(ctx, db)
	if err != nil {
		return err
	}
	cc.security = sec

	fac, err := GetAllSystemSovFactions(ctx, db)
	if err != nil {
		return err
	}
	cc.factions = fac

	pos, err := GetAllSystemPositions(ctx, db)
	if err != nil {
		return err
	}
	cc.positions = pos

	log.Printf("[celestialcache] preloaded %d system-region mappings, %d securities, %d factions, %d positions",
		len(sysRegion), len(sec), len(fac), len(pos))
	return nil
}

// GetCelestials returns the celestials for a solar system. A miss bulk-loads the
// whole region.
func (cc *CelestialCache) GetCelestials(ctx context.Context, db *sql.DB, solarSystemID int64) ([]CelestialEntity, error) {
	// Fast path: already cached.
	cc.mu.RLock()
	if cel, ok := cc.systems[solarSystemID]; ok {
		cc.mu.RUnlock()
		return cel, nil
	}
	cc.mu.RUnlock()

	// Slow path: load entire region.
	regionID := cc.SystemRegion[solarSystemID]
	if regionID == 0 {
		// Unknown system: per-system query.
		return GetSystemCelestials(ctx, db, solarSystemID)
	}

	cc.mu.Lock()
	// Double-check after acquiring write lock.
	if cel, ok := cc.systems[solarSystemID]; ok {
		cc.mu.Unlock()
		return cel, nil
	}
	if cc.loadedRegions[regionID] {
		// Region loaded but this system has no celestials.
		cc.mu.Unlock()
		return nil, nil
	}
	cc.mu.Unlock()

	regionData, err, _ := cc.regionLoad.Do(regionID, func() (map[int64][]CelestialEntity, error) {
		return GetRegionCelestials(ctx, db, regionID)
	})
	if err != nil {
		// Region load failed: per-system query.
		log.Printf("[celestialcache] GetRegionCelestials regionID=%d failed: %v — falling back", regionID, err)
		return GetSystemCelestials(ctx, db, solarSystemID)
	}

	cc.mu.Lock()
	for sysID, cels := range regionData {
		if _, exists := cc.systems[sysID]; !exists {
			cc.systems[sysID] = cels
		}
	}
	cc.loadedRegions[regionID] = true
	cel := cc.systems[solarSystemID]
	cc.mu.Unlock()

	return cel, nil
}

// InvalidateSystem drops one system's cached celestials so the next read rebuilds them.
// It also clears the region's loaded flag: dropping only the system entry would hit
// the "region loaded, no celestials" arm and answer nil. Outpost construction is the
// caller. See docs/db-12-notes.md#celestial-cache-invalidation.
func (cc *CelestialCache) InvalidateSystem(solarSystemID int64) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	delete(cc.systems, solarSystemID)
	if regionID := cc.SystemRegion[solarSystemID]; regionID != 0 {
		delete(cc.loadedRegions, regionID)
	}
}

// GetSecurity returns the preloaded security for a solar system.
func (cc *CelestialCache) GetSecurity(solarSystemID int64) (float64, bool) {
	sec, ok := cc.security[solarSystemID]
	return sec, ok
}

// GetPosition returns the preloaded galactic position of a solar system, in metres,
// for the capital jump range check on an activation path that must not query.
func (cc *CelestialCache) GetPosition(solarSystemID int64) (SystemPos, bool) {
	p, ok := cc.positions[solarSystemID]
	return p, ok
}

// GetSovFaction returns the preloaded sovereign faction for a solar system.
func (cc *CelestialCache) GetSovFaction(solarSystemID int64) (int64, bool) {
	fac, ok := cc.factions[solarSystemID]
	return fac, ok
}
