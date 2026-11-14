package main

// Aggression is a crime charged to whoever committed it. Opening fire on a
// pilot seclaw.ShotIsCrime does not exempt costs security.aggressionPenaltyPct
// of the distance to -10 (the era's small first-shot penalty; the kill is
// charged again on its own), once per attacker->victim pair per criminal
// window, not per shot.
// There is no passive forgiveness; a record is worked back up by killing rats.
// See docs/aggression-concord-investigation.md#6-aggression-charge.

import (
	"context"
	"database/sql"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/safego"
	"gurimoon/seclaw"
)

// How long one attacker->victim pair stays paid up: the crimewatch flag's
// fifteen minutes, so the charge and the flag cover the same engagement.
const aggressionChargeWindow = 15 * time.Minute

type aggressionChargeKey struct{ attacker, victim int64 }

// chargeLedger records the pairs already billed for one kind of act. A map
// under a mutex, not a sync.Map: a Load-then-Store races two shots in the same
// instant into two charges.
type chargeLedger struct {
	mu sync.Mutex
	m  map[aggressionChargeKey]int64
}

var (
	aggroCharged = &chargeLedger{m: map[aggressionChargeKey]int64{}}
	// aidCharged bills a repair on a criminal once per aider->target pair.
	aidCharged = &chargeLedger{m: map[aggressionChargeKey]int64{}}
)

// claim reports whether this pair still owes for this engagement and takes the
// claim if so. Expired entries are pruned in place under the lock when the map
// grows.
func (l *chargeLedger) claim(attacker, victim int64) bool {
	if attacker <= 0 || victim <= 0 || attacker == victim {
		return false
	}
	now := time.Now().Unix()
	win := int64(aggressionChargeWindow / time.Second)
	k := aggressionChargeKey{attacker: attacker, victim: victim}

	l.mu.Lock()
	defer l.mu.Unlock()
	if prev, ok := l.m[k]; ok && now-prev < win {
		return false
	}
	l.m[k] = now
	if len(l.m) > 4096 {
		for kk, ts := range l.m {
			if now-ts >= win {
				delete(l.m, kk)
			}
		}
	}
	return true
}

// claimAggressionCharge is the shot ledger's claim.
func claimAggressionCharge(attacker, victim int64) bool {
	return aggroCharged.claim(attacker, victim)
}

// applyAggressionPenalty is the synchronous entry for a caller that has not
// run the law yet and wants the result: the module's abstract combat tier.
func applyAggressionPenalty(offender *destiny.Entity, f seclaw.KillFacts) (float64, string) {
	if globalDB == nil {
		return 0, ""
	}
	crime, why := seclaw.ShotIsCrime(f, seclawDeps(globalDB))
	if !crime {
		return 0, string(why)
	}
	if !claimAggressionCharge(f.KillerCharID, f.VictimCharID) {
		return 0, ""
	}
	return chargeAggression(offender, f), ""
}

// noteAggressionCrime is the damage-path entry for a caller that has already
// ruled the shot a crime (maybeTriggerConcordResponse). The claim is taken
// synchronously, so a burst of shots cannot race into several charges, and the
// row is written off-thread.
func noteAggressionCrime(offender *destiny.Entity, f seclaw.KillFacts) {
	if globalDB == nil || !claimAggressionCharge(f.KillerCharID, f.VictimCharID) {
		return
	}
	safego.Go("chargeAggression", func() { chargeAggression(offender, f) })
}

// chargeAggression writes the charge. Both entries above have already decided
// that it is owed.
func chargeAggression(offender *destiny.Entity, f seclaw.KillFacts) float64 {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	delta := applySecStatusPenalty(ctx, globalDB, offender, f.KillerCharID, aggressionPenaltyPct(),
		evedb.EventStandingCombatAggression, f.VictimCharID, f.SystemID, f.KillerShipTypeID,
		"SecurityStatusAggressionChange", nil)
	if delta == 0 {
		return 0
	}

	log.Printf("[secstatus] aggression: charID=%d opened fire on %d in system %d sec=%.2f — %+.3f",
		f.KillerCharID, f.VictimCharID, f.SystemID, f.Security, delta)
	return delta
}

// secPctSetting reads a percent dial per charge (one SELECT per engagement),
// clamped to [0, 100]: a negative percent would pay the pilot for the crime.
func secPctSetting(ctx context.Context, sqldb *sql.DB, key string, def float64) float64 {
	if sqldb == nil {
		return def
	}
	s, ok, err := evedb.GetServerSetting(ctx, sqldb, key)
	if err != nil || !ok {
		return def
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return def
	}
	switch {
	case v < 0:
		return 0
	case v > 100:
		return 100
	}
	return v
}
