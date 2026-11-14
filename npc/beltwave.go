package npc

import (
	"context"
	"log"
	"time"

	"gurimoon/db"
	"gurimoon/safego"
)

// EnsureBeltWave is the one place a belt wave is decided. The live spawner and
// the rolebots module both go through it, so the two halves of the game agree
// on what a belt holds. Reseeding is lazy: a cleared belt gets its next wave the
// first time something asks after the cooldown, never from a sweep.
// See docs/npc-behaviour.md#belt-waves.

// beltReseedCooldown returns how long a cleared belt waits before it may be
// re-rolled (the wave timer on Spawner Rules). Read live on every ask.
func (sm *SpawnerManager) beltReseedCooldown() time.Duration {
	if sm != nil {
		if sec := sm.reseedCooldownSec.Load(); sec > 0 {
			return time.Duration(sec) * time.Second
		}
	}
	return DefaultSeedCooldownSec * time.Second
}

// SetReseedCooldown replaces the live wave timer. sec <= 0 goes back to
// DefaultSeedCooldownSec. Belts already cooling are judged against the new value
// from their own lastClearAt, so shortening it releases them at once.
func (sm *SpawnerManager) SetReseedCooldown(sec int) {
	if sm == nil {
		return
	}
	if sec < 0 {
		sec = 0
	}
	sm.reseedCooldownSec.Store(int64(sec))
}

// ReseedCooldown reports the live wave timer.
func (sm *SpawnerManager) ReseedCooldown() time.Duration { return sm.beltReseedCooldown() }

// regionForSystem resolves a system's region from the preloaded map, falling
// back to a query.
func (sm *SpawnerManager) regionForSystem(ctx context.Context, systemID int64) int64 {
	if sm.systemRegion != nil {
		if r, ok := sm.systemRegion[systemID]; ok {
			return r
		}
	}
	if sm.sqldb == nil {
		return 0
	}
	return db.GetRegionIDForSystem(ctx, sm.sqldb, systemID)
}

// rosterForRegion resolves a region's spawn roster from the preloaded map,
// falling back to a query.
func (sm *SpawnerManager) rosterForRegion(ctx context.Context, regionID int64) []db.RegionSpawnRow {
	sm.rosterMu.RLock()
	r, ok := sm.regionRosters[regionID]
	sm.rosterMu.RUnlock()
	if ok {
		return r
	}
	if sm.sqldb == nil {
		return nil
	}
	roster, err := db.GetRegionSpawnsForRegion(ctx, sm.sqldb, regionID)
	if err != nil {
		log.Printf("[beltwave] roster for region %d: %v", regionID, err)
		return nil
	}
	return roster
}

// EnsureBeltWave returns a belt's current wave, rolling and persisting one when
// the belt has never been visited or its cleared wave has aged past the
// cooldown. The slice holds every rat of the wave, dead ones included (see
// BeltRatRow.Alive). A wave that rolls empty is written as already cleared.
func (sm *SpawnerManager) EnsureBeltWave(ctx context.Context, celestialID, systemID int64,
	security float64) (db.BeltWaveRow, []db.BeltRatRow, error) {

	if sm == nil || sm.sqldb == nil {
		return db.BeltWaveRow{}, nil, nil
	}
	sqldb := sm.sqldb

	existing, rats, err := db.LoadBeltWave(ctx, sqldb, celestialID)
	if err != nil {
		return db.BeltWaveRow{}, nil, err
	}

	now := time.Now()
	if existing != nil {
		if !existing.Cleared() {
			return *existing, rats, nil
		}
		if now.Sub(time.Unix(existing.LastClearAt, 0)) < sm.beltReseedCooldown() {
			// Still cooling down. An empty belt is a real answer.
			return *existing, rats, nil
		}
	}

	regionID := sm.regionForSystem(ctx, systemID)
	roster := sm.rosterForRegion(ctx, regionID)
	if len(roster) == 0 {
		// A region that seeds nothing. Record the visit so this is not re-asked
		// on every pass, but there is nothing to roll.
		w := db.BeltWaveRow{
			CelestialID: celestialID, SolarSystemID: systemID, RegionID: regionID,
			Security: security, TierName: "", SeededAt: now.Unix(), LastClearAt: now.Unix(),
		}
		if existing == nil {
			if _, err := db.ClaimNewBeltWave(ctx, sqldb, w, nil); err != nil {
				return db.BeltWaveRow{}, nil, err
			}
		}
		return w, nil, nil
	}

	rule, err := db.GetSpawnerRuleForSecurity(ctx, sqldb, security)
	if err != nil {
		return db.BeltWaveRow{}, nil, err
	}
	if rule == nil {
		log.Printf("[beltwave] no spawner rule covers sec %.4f (system=%d, belt=%d) — empty wave; "+
			"check npcSpawnerRules for a gap between bands", security, systemID, celestialID)
		return db.BeltWaveRow{}, nil, nil
	}

	in := WaveInput{
		Roster:   roster,
		Rule:     rule,
		Tiers:    sm.tiers,
		Cfg:      sm.waveCfg,
		Security: security,
	}
	// The real ledger, not a peek: a bot's belt and a player's belt draw from
	// the same one-officer-per-region budget.
	in.ClaimOfficer = func(row db.RegionSpawnRow) bool {
		ok, _ := sm.TryClaimOfficer(regionID, row.TypeID, systemID, row.FactionID)
		return ok
	}

	out := RollWave(in)

	w := db.BeltWaveRow{
		CelestialID:   celestialID,
		SolarSystemID: systemID,
		RegionID:      regionID,
		Security:      security,
		TierName:      out.Tier.TierName,
		SeededAt:      now.Unix(),
	}
	newRats := make([]db.BeltRatRow, 0, len(out.Rows))
	for _, row := range out.Rows {
		role := row.Role
		if role == "" {
			role = "normal"
		}
		newRats = append(newRats, db.BeltRatRow{
			TypeID:        row.TypeID,
			Role:          role,
			ShipClass:     row.ShipClass,
			FactionID:     row.FactionID,
			CorporationID: row.CorporationID,
			ShieldFrac:    1, ArmorFrac: 1, StructureFrac: 1,
			Alive: true,
		})
	}
	if len(newRats) == 0 {
		w.LastClearAt = now.Unix() // rolled empty = cleared now; see the doc comment
	}

	var won bool
	if existing == nil {
		won, err = db.ClaimNewBeltWave(ctx, sqldb, w, newRats)
	} else {
		clearedBefore := now.Add(-sm.beltReseedCooldown()).Unix()
		won, err = db.ClaimBeltReseed(ctx, sqldb, w, newRats, clearedBefore)
	}
	if err != nil {
		// The write failed after the claim was taken. Hand the slot back with
		// killed=false, as spawnFullWave does when a ball fails to build.
		if out.HasOfficer {
			sm.ReleaseOfficer(regionID, false)
		}
		return db.BeltWaveRow{}, nil, err
	}
	if !won {
		// Somebody else seeded this belt between our read and our write. Our
		// roll is discarded; give back anything it took and read theirs.
		if out.HasOfficer {
			sm.ReleaseOfficer(regionID, false)
		}
		got, gotRats, err := db.LoadBeltWave(ctx, sqldb, celestialID)
		if err != nil {
			return db.BeltWaveRow{}, nil, err
		}
		if got == nil {
			return db.BeltWaveRow{}, nil, nil
		}
		return *got, gotRats, nil
	}

	stored, storedRats, err := db.LoadBeltWave(ctx, sqldb, celestialID)
	if err != nil || stored == nil {
		// The rows are written; only the read-back failed. Answer with what was
		// rolled so the caller is not left with an empty belt it will re-roll.
		return w, newRats, err
	}
	return *stored, storedRats, nil
}

// KillBeltRat marks a persisted rat dead and settles the region ledger when it
// was that region's officer. The live death path and the module's abstract
// kills both come through here, so an officer is released exactly once. The
// slot is held by the persistent row, not the ball; an unload does not free it.
func (sm *SpawnerManager) KillBeltRat(ctx context.Context, rat db.BeltRatRow, regionID int64) error {
	if sm == nil || sm.sqldb == nil {
		return nil
	}
	if _, err := db.KillBeltRat(ctx, sm.sqldb, rat.BeltRatID, time.Now().Unix()); err != nil {
		return err
	}
	if rat.Role == "officer" && regionID != 0 {
		sm.ReleaseOfficer(regionID, true)
	}
	return nil
}

// KillBeltRatEntity is the live path's entry: a ball died, find its persistent
// row and settle it. A ball that is not a persisted belt rat is ignored, which
// is most of them.
func (sm *SpawnerManager) KillBeltRatEntity(entityID uint32) {
	defer safego.Recover("KillBeltRatEntity")
	if sm == nil || sm.sqldb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rat, regionID, err := db.GetBeltRatByEntity(ctx, sm.sqldb, entityID)
	if err != nil {
		log.Printf("[beltwave] GetBeltRatByEntity(%d): %v", entityID, err)
		return
	}
	if rat == nil {
		return
	}
	if err := sm.KillBeltRat(ctx, *rat, regionID); err != nil {
		log.Printf("[beltwave] KillBeltRat(%d): %v", rat.BeltRatID, err)
	}
}
