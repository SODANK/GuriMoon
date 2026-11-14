package npc

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sort"
	"time"

	"gurimoon/db"
)

// Officer ledger. The wave roll's officer chance decides how often the game
// asks for an officer; this ledger decides whether it gets one: at most
// officerMaxPerRegion alive per region, and a quiet period after each kill.
// It lives on the SpawnerManager because belts unload and reload, and it is
// mirrored to npcOfficerLedger. Every claim must be released or the region
// stops producing officers. See docs/npc-behaviour.md#officer-ledger.

// officerLedgerEntry is one region's in-memory officer state.
type officerLedgerEntry struct {
	live        int
	lastSpawnAt time.Time
	lastKillAt  time.Time
}

// OfficerLedgerState is a snapshot row for the admin panel.
type OfficerLedgerState struct {
	RegionID     int64
	Live         int
	LastSpawnAt  time.Time
	LastKillAt   time.Time
	CooldownLeft time.Duration
	WouldAllow   bool
	Reason       string
}

// PreloadOfficerLedger loads the squad tiers, the belt-spawn settings and the
// persisted officer ledger, zeroing every live count and keeping the kill
// timestamps. Must run after db.EnsureSpawnerTables.
func (sm *SpawnerManager) PreloadOfficerLedger(ctx context.Context, sqldb *sql.DB) {
	sm.sqldb = sqldb

	tiers, err := db.GetAllSquadTiers(ctx, sqldb)
	if err != nil {
		log.Printf("[spawner] PreloadOfficerLedger: GetAllSquadTiers failed: %v — "+
			"every belt will roll the neutral fallback tier", err)
	}
	sm.tiers = tiers

	s := db.GetBeltSpawnSettings(ctx, sqldb)
	sm.waveCfg = WaveConfig{
		CommandersEnabled:   s.CommandersEnabled,
		OfficersEnabled:     s.OfficersEnabled,
		CommanderChanceMult: float64(s.CommanderChancePct) / 100,
		OfficerChanceMult:   float64(s.OfficerChancePct) / 100,
		CommandersPerWave:   s.CommandersPerWave,
		WaveHardCap:         s.WaveHardCap,
		HullBiasMult:        s.TierHullBiasMult,
	}
	sm.officerMaxPerRegion = s.OfficerMaxAlivePerRegion
	sm.officerCooldown = time.Duration(s.OfficerKillCooldownSec) * time.Second

	if err := db.ResetOfficerLedgerLive(ctx, sqldb); err != nil {
		log.Printf("[spawner] PreloadOfficerLedger: could not reset live counts: %v", err)
	}
	rows, err := db.LoadOfficerLedger(ctx, sqldb)
	if err != nil {
		log.Printf("[spawner] PreloadOfficerLedger: LoadOfficerLedger failed: %v", err)
		rows = nil
	}

	ledger := make(map[int64]*officerLedgerEntry, len(rows))
	for regionID, r := range rows {
		e := &officerLedgerEntry{} // live starts at 0; the reset above is the authority
		if r.LastSpawnAt > 0 {
			e.lastSpawnAt = time.Unix(r.LastSpawnAt, 0)
		}
		if r.LastKillAt > 0 {
			e.lastKillAt = time.Unix(r.LastKillAt, 0)
		}
		ledger[regionID] = e
	}

	sm.officerMu.Lock()
	sm.officerLedger = ledger
	sm.officerMu.Unlock()

	log.Printf("[spawner] loaded %d squad tiers, officer ledger for %d region(s) "+
		"(max %d alive/region, %s cool-down)",
		len(tiers), len(ledger), sm.officerMaxPerRegion, sm.officerCooldown)
}

// officerEntry returns the region's entry, creating it if needed. Caller holds
// officerMu.
func (sm *SpawnerManager) officerEntry(regionID int64) *officerLedgerEntry {
	if sm.officerLedger == nil {
		sm.officerLedger = make(map[int64]*officerLedgerEntry)
	}
	e := sm.officerLedger[regionID]
	if e == nil {
		e = &officerLedgerEntry{}
		sm.officerLedger[regionID] = e
	}
	return e
}

// officerAllowed is the gate itself. Caller holds officerMu.
func (sm *SpawnerManager) officerAllowed(e *officerLedgerEntry, now time.Time) (bool, string) {
	if sm.officerMaxPerRegion <= 0 {
		return false, "officers are disabled (max alive per region is 0)"
	}
	if e.live >= sm.officerMaxPerRegion {
		return false, fmt.Sprintf("this region already has %d officer(s) alive", e.live)
	}
	if sm.officerCooldown > 0 && !e.lastKillAt.IsZero() {
		if left := sm.officerCooldown - now.Sub(e.lastKillAt); left > 0 {
			return false, fmt.Sprintf("cooling down for another %s", left.Round(time.Minute))
		}
	}
	return true, ""
}

// TryClaimOfficer takes a region's officer slot, or refuses with a reason. The
// wave roll calls this only once it knows it has an officer to place.
func (sm *SpawnerManager) TryClaimOfficer(regionID, typeID, systemID, factionID int64) (bool, string) {
	sm.officerMu.Lock()
	e := sm.officerEntry(regionID)
	now := time.Now()
	ok, why := sm.officerAllowed(e, now)
	if ok {
		e.live++
		e.lastSpawnAt = now
	}
	sm.officerMu.Unlock()

	if !ok {
		return false, why
	}

	// Persist outside the lock. A failed write costs only the admin panel's
	// mirror; the in-memory count gates the next claim.
	if sm.sqldb != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := db.RecordOfficerSpawn(ctx, sm.sqldb, regionID, typeID, systemID, factionID); err != nil {
			log.Printf("[spawner] RecordOfficerSpawn region=%d type=%d: %v", regionID, typeID, err)
		}
	}
	log.Printf("[spawner] officer claimed: region=%d type=%d system=%d", regionID, typeID, systemID)
	return true, ""
}

// PeekOfficer answers the same question as TryClaimOfficer without taking the
// slot. The ESP preview uses it; a page reload must not lock a region out.
func (sm *SpawnerManager) PeekOfficer(regionID int64) (bool, string) {
	sm.officerMu.Lock()
	defer sm.officerMu.Unlock()
	return sm.officerAllowed(sm.officerEntry(regionID), time.Now())
}

// ReleaseOfficer gives a slot back. killed=true stamps the post-kill cool-down;
// killed=false is the despawn path (failed spawn, system torn down) and must
// not stamp it. Never call this while holding sm.mu: NotifyDeath runs under
// sm.mu.RLock and this takes officerMu, and the two never nest.
func (sm *SpawnerManager) ReleaseOfficer(regionID int64, killed bool) {
	sm.officerMu.Lock()
	e := sm.officerEntry(regionID)
	if e.live > 0 {
		e.live--
	}
	if killed {
		e.lastKillAt = time.Now()
	}
	sm.officerMu.Unlock()

	if sm.sqldb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var err error
	if killed {
		err = db.RecordOfficerKill(ctx, sm.sqldb, regionID)
	} else {
		err = db.ReleaseOfficerSlots(ctx, sm.sqldb, regionID, 1)
	}
	if err != nil {
		log.Printf("[spawner] ReleaseOfficer region=%d killed=%v: %v", regionID, killed, err)
	}
}

// OfficerLedgerSnapshot returns the live ledger, region-ordered. This is the
// diagnostic that answers "why have I not seen an officer in a week".
func (sm *SpawnerManager) OfficerLedgerSnapshot() []OfficerLedgerState {
	now := time.Now()
	sm.officerMu.Lock()
	defer sm.officerMu.Unlock()

	out := make([]OfficerLedgerState, 0, len(sm.officerLedger))
	for regionID, e := range sm.officerLedger {
		ok, why := sm.officerAllowed(e, now)
		st := OfficerLedgerState{
			RegionID:    regionID,
			Live:        e.live,
			LastSpawnAt: e.lastSpawnAt,
			LastKillAt:  e.lastKillAt,
			WouldAllow:  ok,
			Reason:      why,
		}
		if sm.officerCooldown > 0 && !e.lastKillAt.IsZero() {
			if left := sm.officerCooldown - now.Sub(e.lastKillAt); left > 0 {
				st.CooldownLeft = left
			}
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RegionID < out[j].RegionID })
	return out
}
