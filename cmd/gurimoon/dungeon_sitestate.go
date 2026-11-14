// Mission dungeon site persistence. A mission site's progress (destroyed NPCs, released wave
// groups) survives the player leaving the grid and warping back. State is in memory with async DB
// persistence, and loadDungeonSiteStates restores it at boot. It is keyed by missionID, since
// dunObjects.objectID is unique across a dungeon's rooms. ESP-spawned test dungeons
// (dunMissionID = 0) respawn on every load.
package main

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/safego"
	"gurimoon/syncx"
	"gurimoon/tick"
)

// dunSiteDB is the package-level DB handle used for async site state
// persistence. Set once during server startup.
var dunSiteDB *sql.DB

type dunSiteState struct {
	mu        sync.Mutex
	missionID int64
	lastUsed  time.Time                // last access, drives the idle sweep
	killed    map[int64]bool           // dunObjects.objectID to destroyed
	waves     map[int64]map[int64]bool // roomID to spawnWave group to released
}

func (st *dunSiteState) touch() {
	st.mu.Lock()
	st.lastUsed = time.Now()
	st.mu.Unlock()
}

// dunSiteStates maps missionID to *dunSiteState. Entries live until mission completion or server
// shutdown.
var dunSiteStates syncx.Map[int64, *dunSiteState]

// dunSiteFor returns the site state for a mission, creating it when create is
// set. Returns nil for missionID <= 0 or when absent and create is false.
func dunSiteFor(missionID int64, create bool) *dunSiteState {
	if missionID <= 0 {
		return nil
	}
	if v, ok := dunSiteStates.Load(missionID); ok {
		st := v
		st.touch()
		return st
	}
	if !create {
		return nil
	}
	v, _ := dunSiteStates.LoadOrStore(missionID, &dunSiteState{
		missionID: missionID,
		lastUsed:  time.Now(),
		killed:    make(map[int64]bool),
		waves:     make(map[int64]map[int64]bool),
	})
	return v
}

// dunSiteIdleTTL is how long a site state may go untouched before the sweep drops it. Missions
// abandoned or expired offline never reach cleanupMissionDungeon; the DB rows persist, so a
// resumed site is restored at the next start.
const dunSiteIdleTTL = 7 * 24 * time.Hour

// startDunSiteSweep arms the hourly idle-site sweep on the central ticker.
func startDunSiteSweep(tickMgr *tick.Manager) {
	tickMgr.RegisterPeriodic("dunsite-sweep", 3600, func(uint64) bool {
		sweepIdleDunSites()
		return true
	})
}

func sweepIdleDunSites() {
	cutoff := time.Now().Add(-dunSiteIdleTTL)
	dunSiteStates.Range(func(key int64, value *dunSiteState) bool {
		st := value
		st.mu.Lock()
		idle := st.lastUsed.Before(cutoff)
		st.mu.Unlock()
		if idle {
			dunSiteStates.Delete(key)
			log.Printf("[sitestate] dropped idle site state for mission %v", key)
		}
		return true
	})
}

func (st *dunSiteState) MarkKilled(objectID int64) {
	if st == nil || objectID <= 0 {
		return
	}
	st.mu.Lock()
	st.killed[objectID] = true
	mid := st.missionID
	st.mu.Unlock()
	// Async DB persist.
	if dunSiteDB != nil && mid > 0 {
		go func() {
			defer safego.Recover("MarkKilled")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := evedb.PersistDungeonKill(ctx, dunSiteDB, mid, objectID); err != nil {
				log.Printf("[sitestate] PersistDungeonKill mission=%d obj=%d: %v", mid, objectID, err)
			}
		}()
	}
}

func (st *dunSiteState) IsKilled(objectID int64) bool {
	if st == nil {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.killed[objectID]
}

func (st *dunSiteState) MarkWaveReleased(roomID, wave int64) {
	if st == nil || wave <= 0 {
		return
	}
	st.mu.Lock()
	if st.waves[roomID] == nil {
		st.waves[roomID] = make(map[int64]bool)
	}
	st.waves[roomID][wave] = true
	mid := st.missionID
	st.mu.Unlock()
	// Async DB persist.
	if dunSiteDB != nil && mid > 0 {
		go func() {
			defer safego.Recover("MarkWaveReleased")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := evedb.PersistDungeonWaveRelease(ctx, dunSiteDB, mid, roomID, wave); err != nil {
				log.Printf("[sitestate] PersistDungeonWaveRelease mission=%d room=%d wave=%d: %v", mid, roomID, wave, err)
			}
		}()
	}
}

// Unkill puts a destroyed object back on the site, so the next spawn of its room builds it again.
// Memory only: a fixed dungeon's respawn (fixedsites.go) is the one caller, and a mission site
// never calls it, so nothing persisted is touched.
func (st *dunSiteState) Unkill(objectID int64) {
	if st == nil {
		return
	}
	st.mu.Lock()
	delete(st.killed, objectID)
	st.mu.Unlock()
}

// UnreleaseWave holds a wave group again, so its trigger can release it once more. Memory only,
// like Unkill.
func (st *dunSiteState) UnreleaseWave(roomID, wave int64) {
	if st == nil {
		return
	}
	st.mu.Lock()
	delete(st.waves[roomID], wave)
	st.mu.Unlock()
}

func (st *dunSiteState) WaveReleased(roomID, wave int64) bool {
	if st == nil {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.waves[roomID][wave]
}

// loadDungeonSiteStates restores in-memory site state from DB for all active
// mission dungeon instances. Called once at server startup.
func loadDungeonSiteStates(sqldb *sql.DB) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	instances, err := evedb.GetAllDungeonInstances(ctx, sqldb)
	if err != nil {
		log.Printf("[startup] loadDungeonSiteStates: GetAllDungeonInstances: %v", err)
		return
	}

	loaded := 0
	for _, inst := range instances {
		site := dunSiteFor(inst.MissionID, true)

		// Load killed objects.
		kills, kerr := evedb.LoadDungeonSiteKills(ctx, sqldb, inst.MissionID)
		if kerr != nil {
			log.Printf("[startup] loadDungeonSiteStates: LoadDungeonSiteKills mission=%d: %v", inst.MissionID, kerr)
			continue
		}
		site.mu.Lock()
		for _, objID := range kills {
			site.killed[objID] = true
		}
		site.mu.Unlock()

		// Load released waves.
		waves, werr := evedb.LoadDungeonSiteWaves(ctx, sqldb, inst.MissionID)
		if werr != nil {
			log.Printf("[startup] loadDungeonSiteStates: LoadDungeonSiteWaves mission=%d: %v", inst.MissionID, werr)
			continue
		}
		site.mu.Lock()
		for roomID, waveGroups := range waves {
			if site.waves[roomID] == nil {
				site.waves[roomID] = make(map[int64]bool)
			}
			for _, wg := range waveGroups {
				site.waves[roomID][wg] = true
			}
		}
		site.mu.Unlock()

		killCount := len(kills)
		waveCount := 0
		for _, wgs := range waves {
			waveCount += len(wgs)
		}
		if killCount > 0 || waveCount > 0 {
			log.Printf("[startup] loadDungeonSiteStates: mission %d — %d killed, %d waves released",
				inst.MissionID, killCount, waveCount)
			loaded++
		}
	}

	if loaded > 0 {
		log.Printf("[startup] loadDungeonSiteStates: restored site state for %d missions", loaded)
	}
}
