package main

// Crimewatch 1.0 PvP timers, per character. Any PvP act, legal or not, locks the
// aggressor out of docking and stargates for crimewatch.aggressionLockSec (the
// client's own CantDockAfterAggression and CantJumpAfterAggression; the era's
// lock was invisible until tried). Being in PvP as attacker or target keeps a
// logged-off ship in space for crimewatch.pvpLogoffMin (logofflinger.go).
// Keyed by charID, not connection, so a relog or ship swap keeps both.

import (
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
)

type pvpTimes struct {
	aggressedAt int64 // unix seconds this pilot last started a PvP act
	pvpAt       int64 // unix seconds of the last PvP involvement, either side
}

var (
	pvpTimersMu sync.Mutex
	pvpTimers   = map[int64]pvpTimes{}
)

// aggressionLockSec is the dock/jump lock after a PvP act.
func aggressionLockSec() time.Duration {
	return evedb.CachedSettingDuration("crimewatch.aggressionLockSec", time.Second, 60, 0, 900)
}

// stampPvP records a PvP act: the aggressor's lock and both pilots' PvP time.
// victimCharID may be 0 (a structure).
func stampPvP(aggressorCharID, victimCharID int64) {
	if aggressorCharID <= 0 {
		return
	}
	now := time.Now().Unix()
	pvpTimersMu.Lock()
	defer pvpTimersMu.Unlock()
	t := pvpTimers[aggressorCharID]
	t.aggressedAt, t.pvpAt = now, now
	pvpTimers[aggressorCharID] = t
	if victimCharID > 0 && victimCharID != aggressorCharID {
		v := pvpTimers[victimCharID]
		v.pvpAt = now
		pvpTimers[victimCharID] = v
	}
	if len(pvpTimers) > 4096 {
		// An hour outlives any logoff window the dial allows.
		cutoff := now - 3600
		for id, tt := range pvpTimers {
			if tt.pvpAt < cutoff {
				delete(pvpTimers, id)
			}
		}
	}
}

func pvpTimesFor(charID int64) pvpTimes {
	pvpTimersMu.Lock()
	defer pvpTimersMu.Unlock()
	return pvpTimers[charID]
}

// aggressionLockActive is the rule: a lock of lock seconds started at unix
// second at is still running at now.
func aggressionLockActive(now, at int64, lock time.Duration) bool {
	return at > 0 && lock > 0 && now-at < int64(lock/time.Second)
}

// aggressionLocked reports whether charID may not dock or jump yet.
func aggressionLocked(charID int64) bool {
	if charID <= 0 {
		return false
	}
	return aggressionLockActive(time.Now().Unix(), pvpTimesFor(charID).aggressedAt, aggressionLockSec())
}

// stampPvPEntities is stampPvP from two in-space entities.
func stampPvPEntities(attacker, target *destiny.Entity) {
	if attacker == nil || !attacker.IsPlayer {
		return
	}
	var victim int64
	if target != nil && target.IsPlayer {
		victim = target.Slim.CharID
	}
	stampPvP(attacker.Slim.CharID, victim)
}
