package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/modapi"
)

// Belt fields: hydration, write-back, teardown. A field is one seed (db/asteroiddb.go) plus the
// mined depletion. See docs/db-6-notes.md#persistent-asteroid-belts.

// loadAsteroidsIntoSystem rebuilds a new system's belts from its persisted seed, minting a seed when
// none exists, the field has aged past asteroids.regenDays, or its sov, security or belt set changed,
// then applies persisted depletion. Called by ensureSystemLoaded after SeedCelestialBubbles so rocks
// join the anchored bubbles. A database error falls back to an unpersisted GenerateAsteroids.
func loadAsteroidsIntoSystem(addr string, sqldb *sql.DB, ss *destiny.SolarSystem, sysID int64) {
	started := time.Now()

	if sqldb == nil {
		ss.GenerateAsteroids(ss.CelBalls, ss.CelSlims)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ss.SetAsteroidRespawnDelay(
		time.Duration(evedb.AsteroidRespawnMinutes(ctx, sqldb)) * time.Minute)

	beltCount := 0
	for _, slim := range ss.CelSlims {
		if slim.GroupID == 9 {
			beltCount++
		}
	}
	regenDays := evedb.AsteroidRegenDays(ctx, sqldb)
	ttl := time.Duration(regenDays) * 24 * time.Hour

	seed, reseeded, generatedAt, err := evedb.EnsureAsteroidSeed(ctx, sqldb, sysID,
		evedb.AsteroidGenKey{
			GenVersion: destiny.AsteroidGenVersion,
			Security:   ss.Security,
			FactionID:  ss.FactionID,
			BeltCount:  beltCount,
		}, ttl)
	if err != nil {
		log.Printf("[%s] asteroids: seed lookup failed sysID=%d: %v — generating unpersisted field",
			addr, sysID, err)
		ss.GenerateAsteroids(ss.CelBalls, ss.CelSlims)
		return
	}

	ss.GenerateAsteroidsSeeded(ss.CelBalls, ss.CelSlims, seed)
	genElapsed := time.Since(started)

	mined := 0
	if !reseeded {
		qtys, err := evedb.LoadAsteroidDepletion(ctx, sqldb, sysID)
		if err != nil {
			log.Printf("[%s] asteroids: depletion load failed sysID=%d: %v — field starts full",
				addr, sysID, err)
		} else {
			mined = ss.ApplyAsteroidDepletion(qtys)
		}
	}

	ageDays := 0.0
	if generatedAt > 0 {
		ageDays = time.Since(time.Unix(generatedAt, 0)).Hours() / 24
	}
	state := "hydrated"
	if reseeded {
		state = "reseeded"
	}
	log.Printf("[%s] asteroids: sysID=%d %s seed=%d age=%.1fd belts=%d rocks=%d mined=%d gen=%s",
		addr, sysID, state, seed, ageDays, beltCount, ss.AsteroidRockCount(), mined,
		genElapsed.Round(time.Millisecond))
}

// flushSystemAsteroids writes back one system's mined quantities. full drains
// the whole field rather than the dirty set: the set only records what has moved
// since the last flush, and a system can unload with an empty set but a field
// that has been worked all evening.
func flushSystemAsteroids(ctx context.Context, sqldb *sql.DB, ss *destiny.SolarSystem, full bool) int {
	if sqldb == nil || ss == nil {
		return 0
	}
	var recs []destiny.AsteroidDepletionRecord
	if full {
		recs = ss.ExportAsteroidDepletion()
		ss.DrainDirtyAsteroids() // the export supersedes it
	} else {
		recs = ss.DrainDirtyAsteroids()
	}
	if len(recs) == 0 {
		return 0
	}
	rows := make([]evedb.AsteroidDepletionRow, 0, len(recs))
	for _, r := range recs {
		rows = append(rows, evedb.AsteroidDepletionRow{
			EntityID: r.EntityID, BeltID: r.BeltID, Qty: r.Qty,
		})
	}
	if err := evedb.FlushAsteroidDepletion(ctx, sqldb, ss.ID, rows); err != nil {
		log.Printf("[asteroid-flush] sysID=%d %d rows: %v", ss.ID, len(rows), err)
		return 0
	}
	return len(rows)
}

// flushAllAsteroids drains every loaded system's dirty rock set. A system nobody
// is mining in costs one lock and a nil return.
func flushAllAsteroids(ctx context.Context, sqldb *sql.DB) (systems, rocks int) {
	if sqldb == nil || globalSysMgr == nil {
		return 0, 0
	}
	for _, ss := range globalSysMgr.All() {
		if n := flushSystemAsteroids(ctx, sqldb, ss, false); n > 0 {
			systems++
			rocks += n
		}
	}
	return systems, rocks
}

// releaseSystem is the teardown for a system whose last player left: flush belt state held only in
// memory, tell modules, release NPC state. Every RemoveIfEmpty caller goes through it. The flush runs
// after RemoveIfEmpty returns true, which marks the system dead, so nothing adds dirty entries mid-drain.
func releaseSystem(sysMgr *destiny.SystemManager, ss *destiny.SolarSystem, sqldb *sql.DB) {
	if sysMgr == nil || ss == nil || !sysMgr.RemoveIfEmpty(ss) {
		return
	}
	// The drain is synchronous because the system is about to be dropped; the write could be split off
	// if it shows on the connection-close path.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	flushSystemAsteroids(ctx, sqldb, ss, true)
	cancel()

	modapi.FireSystemUnloading(ss.ID)
	teardownSystemNPCs(ss.ID)
}

// bootstrapAsteroidFields gives every belt-bearing system a field, re-rolls the aged ones and logs
// the state of the set. Two statements; the rocks themselves are never materialised.
func bootstrapAsteroidFields(sqldb *sql.DB) {
	if sqldb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	started := time.Now()
	seeded, err := evedb.SeedAllAsteroidFields(ctx, sqldb, destiny.AsteroidGenVersion)
	if err != nil {
		log.Printf("[startup] asteroid fields: seeding the galaxy failed: %v", err)
	} else if seeded > 0 {
		log.Printf("[startup] asteroid fields: seeded %d new system(s) in %s",
			seeded, time.Since(started).Round(time.Millisecond))
	}
	if n, err := evedb.ReseedStaleAsteroidFields(ctx, sqldb, destiny.AsteroidGenVersion); err != nil {
		log.Printf("[startup] asteroid fields: generator-version sweep failed: %v", err)
	} else if n > 0 {
		log.Printf("[startup] asteroid fields: regenerated %d system(s) rolled under an older generator",
			n)
	}

	// Expiry is swept in bulk so unvisited systems age with the rest. Nothing is loaded at boot, so
	// nothing is excluded; the periodic sweep skips loaded systems.
	days := evedb.AsteroidRegenDays(ctx, sqldb)
	if days > 0 {
		t0 := time.Now()
		n, err := evedb.ReseedExpiredAsteroidFields(ctx, sqldb,
			time.Duration(days)*24*time.Hour, nil)
		if err != nil {
			log.Printf("[startup] asteroid fields: expiry sweep failed: %v", err)
		} else if n > 0 {
			log.Printf("[startup] asteroid fields: regenerated %d expired system(s) in %s",
				n, time.Since(t0).Round(time.Millisecond))
		}
	}
	logAsteroidFieldStats(ctx, sqldb)
}

// sweepExpiredAsteroidFields re-rolls fields past the cycle. A system with players in it keeps its
// field until a later sweep finds it empty, so a field is never swapped under an on-grid client.
func sweepExpiredAsteroidFields(ctx context.Context, sqldb *sql.DB) {
	if sqldb == nil {
		return
	}
	days := evedb.AsteroidRegenDays(ctx, sqldb)
	if days <= 0 {
		return
	}
	var loaded []int64
	if globalSysMgr != nil {
		for _, ss := range globalSysMgr.All() {
			loaded = append(loaded, ss.ID)
		}
	}
	n, err := evedb.ReseedExpiredAsteroidFields(ctx, sqldb,
		time.Duration(days)*24*time.Hour, loaded)
	if err != nil {
		log.Printf("[asteroid-regen] %v", err)
		return
	}
	if n > 0 {
		log.Printf("[asteroid-regen] regenerated %d expired system(s) (%d loaded, left alone)",
			n, len(loaded))
	}
}

// logAsteroidFieldStats reports the state of the persisted belt fields.
func logAsteroidFieldStats(ctx context.Context, sqldb *sql.DB) {
	days := evedb.AsteroidRegenDays(ctx, sqldb)
	st, err := evedb.GetAsteroidStats(ctx, sqldb, days)
	if err != nil {
		log.Printf("[startup] asteroid fields: %v", err)
		return
	}
	regen := "regen off"
	if days > 0 {
		regen = fmt.Sprintf("regen every %dd", days)
	}
	respawn := "rock respawn off"
	if mins := evedb.AsteroidRespawnMinutes(ctx, sqldb); mins > 0 {
		respawn = "rock respawn " + (time.Duration(mins) * time.Minute).String()
	}
	log.Printf("[startup] asteroid fields: %d seeded (%d past expiry), %d mined rocks (%d exhausted), %s, %s",
		st.Fields, st.Expired, st.DepletionRows, st.MinedOut, regen, respawn)
}

// systemHasBelts reports whether the system has any groupID-9 celestial. A
// system with none can never gain an asteroid ball, so it must not be sent down
// the hydration path repeatedly looking for one.
func systemHasBelts(ss *destiny.SolarSystem) bool {
	for _, slim := range ss.CelSlims {
		if slim.GroupID == 9 {
			return true
		}
	}
	return false
}
