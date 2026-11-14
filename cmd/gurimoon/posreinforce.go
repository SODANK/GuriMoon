package main

// Control tower reinforcement; rules in db/starbasereinforce.go. The trigger
// is the tower entity's shield floor: ApplyDamage stops at the threshold and
// flags the hit, structureDamaged notices, and onTowerShieldFloor decides with
// the database whether the tower reinforces (online, an hour of strontium) or
// keeps taking damage (floor cleared). The floor re-arms only after the shield
// recovers reinforceRearmMargin past the threshold.
// See docs/starbase-pos-investigation.md#18-reinforcement.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/safego"
	"gurimoon/server"
	"gurimoon/tick"
)

// reinforceShieldFrac is starbase.reinforceShieldPct as a fraction, refreshed
// by the reinforcement sweep (the floor is re-evaluated on every shield regen
// broadcast, which must not read the database).
var reinforceShieldFrac atomic.Uint64

// reinforceRearmMargin is how far above the threshold the shield must climb
// before the floor re-arms.
const reinforceRearmMargin = 0.05

func init() {
	reinforceShieldFrac.Store(math.Float64bits(0.25))
	evedb.SubscribeStarbaseEvents(invulnerableOnReinforce)
}

// invulnerableOnReinforce makes a loaded tower's entity invulnerable when the
// tower is reinforced off its entity (db.HeadlessReinforce, used by a rolebot
// siege while a player watches). onTowerShieldFloor does the same on its own
// path, and doing it twice is harmless.
func invulnerableOnReinforce(ev evedb.StarbaseEvent) {
	if ev.Kind != evedb.StarbaseEventReinforced || globalSysMgr == nil {
		return
	}
	if ss := globalSysMgr.Get(ev.SolarSystemID); ss != nil {
		if e := ss.StructureEntity(uint32(ev.TowerID)); e != nil {
			e.SetInvulnerable(true)
			e.SetShieldFloor(0)
		}
	}
}

func reinforceFrac() float64 { return math.Float64frombits(reinforceShieldFrac.Load()) }

// configureTowerDefence sets a freshly built tower entity's reinforcement
// state from its slim: invulnerable while reinforced, floor armed otherwise.
func configureTowerDefence(e *destiny.Entity, slim destiny.SlimItem) {
	if slim.GroupID != evedb.GroupControlTower {
		return
	}
	if slim.PosState != nil && *slim.PosState == 3 {
		e.SetInvulnerable(true)
		e.SetShieldFloor(0)
		return
	}
	if shouldArmFloor(e.DamageStateFrac().Shield, reinforceFrac(), false) {
		e.SetShieldFloor(reinforceFrac())
	}
}

// shouldArmFloor is the floor re-arm rule. Pure.
func shouldArmFloor(shieldFrac, threshold float64, armed bool) bool {
	if armed || threshold <= 0 {
		return false
	}
	return shieldFrac >= threshold+reinforceRearmMargin
}

// maybeRearmTowerFloor is called on every structure HP change.
func maybeRearmTowerFloor(e *destiny.Entity) {
	if e.Slim.GroupID != evedb.GroupControlTower || e.IsInvulnerable() {
		return
	}
	if shouldArmFloor(e.DamageStateFrac().Shield, reinforceFrac(), e.ShieldFloor() > 0) {
		e.SetShieldFloor(reinforceFrac())
	}
}

// onTowerShieldFloor runs when a tower's shield has been shot down to the
// floor. Called in its own goroutine (it reads and writes the database).
func onTowerShieldFloor(ss *destiny.SolarSystem, e *destiny.Entity, sqldb *sql.DB) {
	defer safego.Recover("onTowerShieldFloor")
	if sqldb == nil {
		e.SetShieldFloor(0)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), starbaseCtxTimeout)
	defer cancel()
	rec, found, err := evedb.GetStarbase(ctx, sqldb, int64(e.ID))
	if err != nil || !found {
		e.SetShieldFloor(0)
		return
	}
	if rec.State == evedb.StarbaseStateReinforced {
		return // a second floor hit racing the first
	}
	if rec.State != evedb.StarbaseStateOnline {
		// Only an online tower reinforces; an anchored or onlining one is
		// simply shot through.
		e.SetShieldFloor(0)
		return
	}
	hours, until, err := evedb.EnterReinforcement(ctx, sqldb, rec)
	if err != nil {
		log.Printf("[pos] tower %d reinforcement: %v", rec.EntityID, err)
	}
	if hours <= 0 {
		log.Printf("[pos] tower %d reached reinforcement with no strontium — it keeps taking damage", rec.EntityID)
		e.SetShieldFloor(0)
		return
	}
	e.SetInvulnerable(true)
	e.SetShieldFloor(0)
	markStructureDirty(e)
	flushStructureDamage(sqldb)
	msg := starbasePushStateAndSchedule("pos-reinforce", sqldb, globalSysMgr, rec.EntityID)
	scheduleReinforceExit(sqldb, rec.EntityID, until)
	rec.State = evedb.StarbaseStateReinforced
	rec.ReinforceUntil = until
	evedb.EmitStarbaseEvent(evedb.StarbaseEventFor(evedb.StarbaseEventReinforced, rec))
	log.Printf("[pos] tower %d REINFORCED for %d h (strontium burnt).%s", rec.EntityID, hours, msg)
}

// scheduleReinforceExit arms the one-shot that ends a reinforcement two
// seconds before its timestamp, so the client never holds posState 3 with a
// past time. The sweep is the restart-proof backstop.
func scheduleReinforceExit(sqldb *sql.DB, towerID int64, until uint64) {
	if globalTickMgr == nil || until == 0 {
		return
	}
	now := server.Win32TimeNow()
	var in time.Duration
	if until > now {
		in = time.Duration(until-now) * 100 * time.Nanosecond
	}
	in -= 2 * time.Second
	if in < 0 {
		in = 0
	}
	globalTickMgr.ScheduleOnce(fmt.Sprintf("pos-reinforce-%d", towerID), time.Now().Add(in), func() {
		finishReinforcement(sqldb, towerID)
	})
}

// finishReinforcement brings a tower out of reinforcement. Safe to call more
// than once.
func finishReinforcement(sqldb *sql.DB, towerID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), starbaseCtxTimeout)
	defer cancel()
	rec, found, err := evedb.GetStarbase(ctx, sqldb, towerID)
	if err != nil || !found || !evedb.ReinforcementDue(rec, server.Win32TimeNow()) {
		return
	}
	ok, err := evedb.ExitReinforcement(ctx, sqldb, towerID)
	if err != nil || !ok {
		return
	}
	if globalSysMgr != nil {
		if ss := globalSysMgr.Get(rec.SolarSystemID); ss != nil {
			if e := ss.StructureEntity(uint32(towerID)); e != nil {
				e.SetInvulnerable(false)
				maybeRearmTowerFloor(e)
			}
		}
	}
	msg := starbasePushStateAndSchedule("pos-reinforce", sqldb, globalSysMgr, towerID)
	log.Printf("[pos] tower %d out of reinforcement.%s", towerID, msg)
}

// startStarbaseReinforceSweep finishes due reinforcements every 30 s and
// refreshes the threshold setting.
func startStarbaseReinforceSweep(tickMgr *tick.Manager, sqldb *sql.DB) {
	if tickMgr == nil || sqldb == nil {
		return
	}
	tickMgr.RegisterPeriodic("starbase-reinforce-sweep", 30, func(uint64) bool {
		ctx, cancel := context.WithTimeout(context.Background(), starbaseCtxTimeout)
		pct := evedb.StarbaseReinforceShieldPct(ctx, sqldb)
		recs, err := evedb.ListStarbasesByState(ctx, sqldb, evedb.StarbaseStateReinforced)
		cancel()
		reinforceShieldFrac.Store(math.Float64bits(pct / 100))
		if err != nil {
			log.Printf("[pos] reinforce sweep: %v", err)
			return true
		}
		now := server.Win32TimeNow()
		for _, r := range recs {
			if evedb.ReinforcementDue(r, now) {
				finishReinforcement(sqldb, r.EntityID)
			}
		}
		return true
	})
}
