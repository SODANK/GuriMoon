package main

// Player-vs-player aggression bookkeeping (crimewatch 1.0). The client's
// flagging model is one server-owned dict per solar system,
// aggressors[aggressorCharID][victimKey] = timestamp, where victimKey is a
// charID, a corpID (the victim's corpmates may retaliate) or None (the global
// criminal flag). michelle.py receives it in the SetState bag and through
// OnAggressionChange, hardcodes the 15-minute countdown and drives the Neocom
// countdown text, tactical.CheckCriminal and consider.CanAttackFreely from it.
// The server stores Unix seconds and victimKey 0 for None;
// destiny.EncodeCharAggressors converts to FILETIME.

import (
	"log"
	"sync"
	"time"

	"gurimoon/destiny"
	"gurimoon/marshal"
	"gurimoon/server"
	"gurimoon/syncx"
	"gurimoon/tick"
)

// aggressionDuration mirrors the client's hardcoded 15-minute timer
// (michelle.py "15 * MIN"). Server-side expiry must match or stale entries
// resurrect flags on the next SetState.
const aggressionDuration = 15 * time.Minute

// aggroRebroadcastAfter throttles per-hit refreshes: an existing entry only
// re-broadcasts (resetting the visible countdown) when its stored timestamp
// is at least this stale. New entries always broadcast.
const aggroRebroadcastAfter = 60 * time.Second

type aggroTable struct {
	mu sync.Mutex
	// entries[aggressorCharID][victimKey] = Unix seconds. victimKey 0 = the
	// global criminal flag (None on the wire).
	entries map[int64]map[int64]int64
}

// aggroTables maps solarSystemID to *aggroTable.
var aggroTables syncx.Map[int64, *aggroTable]

func aggroTableFor(sysID int64) *aggroTable {
	if t, ok := aggroTables.Load(sysID); ok {
		return t
	}
	t, _ := aggroTables.LoadOrStore(sysID, &aggroTable{entries: make(map[int64]map[int64]int64)})
	return t
}

// set records aggressor->victimKey at now, returning true when the change is
// worth broadcasting (new entry, or a refresh past the throttle window).
func (t *aggroTable) set(aggressorCharID, victimKey, now int64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	victims, ok := t.entries[aggressorCharID]
	if !ok {
		victims = make(map[int64]int64)
		t.entries[aggressorCharID] = victims
	}
	old, existed := victims[victimKey]
	victims[victimKey] = now
	return !existed || now-old >= int64(aggroRebroadcastAfter/time.Second)
}

// snapshot returns a copy of the live entries for encoding.
func (t *aggroTable) snapshot() map[int64]map[int64]int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[int64]map[int64]int64, len(t.entries))
	for aggr, victims := range t.entries {
		vc := make(map[int64]int64, len(victims))
		for k, v := range victims {
			vc[k] = v
		}
		out[aggr] = vc
	}
	return out
}

// expire drops entries older than aggressionDuration; returns true if
// anything was removed.
func (t *aggroTable) expire(now int64) bool {
	cutoff := now - int64(aggressionDuration/time.Second)
	t.mu.Lock()
	defer t.mu.Unlock()
	changed := false
	for aggr, victims := range t.entries {
		for k, ts := range victims {
			if ts < cutoff {
				delete(victims, k)
				changed = true
			}
		}
		if len(victims) == 0 {
			delete(t.entries, aggr)
		}
	}
	return changed
}

// aggressionSnapshotFor returns the current crimewatch entries for a system,
// for embedding into the SetState bag. Nil-safe when no table exists yet.
func aggressionSnapshotFor(sysID int64) map[int64]map[int64]int64 {
	if t, ok := aggroTables.Load(sysID); ok {
		return t.snapshot()
	}
	return nil
}

// globalCriminals returns the charIDs currently carrying an unexpired global
// criminal flag (victim key 0 / None) in this system.
func (t *aggroTable) globalCriminals(now int64) map[int64]bool {
	cutoff := now - int64(aggressionDuration/time.Second)
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[int64]bool)
	for aggr, victims := range t.entries {
		if ts, ok := victims[0]; ok && ts >= cutoff {
			out[aggr] = true
		}
	}
	return out
}

// globalCriminalsInSystem returns the charIDs flagged as global criminals in
// a system: the CONCORD and station-sentry target list.
func globalCriminalsInSystem(sysID int64) map[int64]bool {
	if t, ok := aggroTables.Load(sysID); ok {
		return t.globalCriminals(time.Now().Unix())
	}
	return nil
}

// isGlobalCriminal reports whether a character carries an unexpired criminal
// timer in a system, i.e. is a legal target for everyone.
func isGlobalCriminal(sysID, charID int64) bool {
	if charID <= 0 {
		return false
	}
	return globalCriminalsInSystem(sysID)[charID]
}

// globalCriminalSince returns the timestamp of an unexpired global criminal
// flag for charID in this system, or 0 when there is none.
func (t *aggroTable) globalCriminalSince(charID, now int64) int64 {
	cutoff := now - int64(aggressionDuration/time.Second)
	t.mu.Lock()
	defer t.mu.Unlock()
	victims, ok := t.entries[charID]
	if !ok {
		return 0
	}
	if ts, ok := victims[0]; ok && ts >= cutoff {
		return ts
	}
	return 0
}

// hasAggressed reports whether aggressorCharID holds an unexpired entry
// against victimKey (a charID or a corpID). Read-only; nothing refreshes a timer.
func (t *aggroTable) hasAggressed(aggressorCharID, victimKey, now int64) bool {
	if aggressorCharID <= 0 || victimKey <= 0 {
		return false
	}
	cutoff := now - int64(aggressionDuration/time.Second)
	t.mu.Lock()
	defer t.mu.Unlock()
	victims, ok := t.entries[aggressorCharID]
	if !ok {
		return false
	}
	ts, ok := victims[victimKey]
	return ok && ts >= cutoff
}

// victimEngaged reports whether the pilot who just died had shot their killer
// or the killer's corp (the crimewatch table, not the killmail, which knows
// only the last blow). The corp key counts because shooting one member of a
// corp engages the corp. Read-only, safe from a death handler.
func victimEngaged(sysID, victimCharID, killerCharID, killerCorpID int64) bool {
	if victimCharID <= 0 || killerCharID <= 0 {
		return false
	}
	t, ok := aggroTables.Load(sysID)
	if !ok {
		return false
	}
	tbl := t
	now := time.Now().Unix()
	return tbl.hasAggressed(victimCharID, killerCharID, now) ||
		tbl.hasAggressed(victimCharID, killerCorpID, now)
}

// setAt records aggressor->victimKey at an explicit timestamp with no refresh
// throttle, so a flag carried between systems keeps its original start time.
func (t *aggroTable) setAt(aggressorCharID, victimKey, ts int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	victims, ok := t.entries[aggressorCharID]
	if !ok {
		victims = make(map[int64]int64)
		t.entries[aggressorCharID] = victims
	}
	if old, existed := victims[victimKey]; existed && old >= ts {
		return
	}
	victims[victimKey] = ts
}

// carryCriminalFlag moves a live criminal timer with a pilot who changes solar
// system. Tables are per-system (the shape the SetState bag wants), so a jump
// would otherwise drop the flag. The timestamp is kept; the countdown continues.
// Called after the pilot is registered in the new system, so CONCORD follows.
func carryCriminalFlag(oldSysID, newSysID, charID int64) {
	if oldSysID == newSysID || charID <= 0 {
		return
	}
	t, ok := aggroTables.Load(oldSysID)
	if !ok {
		return
	}
	ts := t.globalCriminalSince(charID, time.Now().Unix())
	if ts == 0 {
		return
	}
	aggroTableFor(newSysID).setAt(charID, 0, ts)
	log.Printf("[aggro] charID=%d carried criminal timer %d→%d (set %ds ago)",
		charID, oldSysID, newSysID, time.Now().Unix()-ts)
	if globalSysMgr != nil {
		if ss := globalSysMgr.Get(newSysID); ss != nil {
			broadcastAggression(ss)
			concordFollow(ss, findPlayerEntityByChar(ss, charID))
		}
	}
}

// recordPlayerAggression marks attacker->victim aggression (char and corp
// keys), stamps both pilots' PvP timers (pvptimers.go) and broadcasts the
// updated table to the system. Called from every player->player damage path,
// any security band, lawful or not.
func recordPlayerAggression(attacker, target *destiny.Entity, ss *destiny.SolarSystem) {
	if attacker == nil || target == nil || ss == nil || !target.IsPlayer {
		return
	}
	aggrCharID := attacker.Slim.CharID
	victimCharID := target.Slim.CharID
	if aggrCharID <= 0 || victimCharID <= 0 || aggrCharID == victimCharID {
		return
	}
	stampPvP(aggrCharID, victimCharID)
	t := aggroTableFor(ss.ID)
	now := time.Now().Unix()
	changed := t.set(aggrCharID, victimCharID, now)
	if victimCorpID := target.Slim.CorpID; victimCorpID > 0 {
		if t.set(aggrCharID, victimCorpID, now) {
			changed = true
		}
	}
	if changed {
		broadcastAggression(ss)
	}
}

// markGlobalCriminal sets the aggressor's global criminal flag (victim key
// None). Refreshes the timer on every call; broadcasts on the first mark and
// then only past the rebroadcast throttle, since this fires per damage event.
func markGlobalCriminal(ss *destiny.SolarSystem, charID int64) {
	if ss == nil || charID <= 0 {
		return
	}
	if !aggroTableFor(ss.ID).set(charID, 0, time.Now().Unix()) {
		return
	}
	log.Printf("[aggro] charID=%d GLOBAL CRIMINAL in system %d", charID, ss.ID)
	broadcastAggression(ss)
}

// broadcastAggression pushes the system's full aggression table to every
// player in it via OnAggressionChange(solarsystemID, aggressors); the client
// replaces its copy wholesale and rearms its countdown threads.
func broadcastAggression(ss *destiny.SolarSystem) {
	snap := aggressionSnapshotFor(ss.ID)
	aggrDict := marshal.PyDict{Items: destiny.EncodeCharAggressors(snap)}
	args := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(ss.ID)},
		aggrDict,
	}}
	for _, p := range ss.GetAllPlayersInSystem() {
		if p.Conn == nil {
			continue
		}
		_ = server.SendNotification(p.Conn, int64(p.AccountID), "OnAggressionChange", "charid", args)
	}
}

// startAggressionSweep arms the periodic expiry: every 30s each system's
// table drops entries past the 15-minute window and, if anything lapsed,
// re-broadcasts so lingering flags clear even for clients that missed the
// local countdown (fresh SetState receivers).
func startAggressionSweep(tickMgr *tick.Manager) {
	tickMgr.RegisterPeriodic("aggression-sweep", 30, func(uint64) bool {
		now := time.Now().Unix()
		aggroTables.Range(func(key int64, value *aggroTable) bool {
			sysID := key
			t := value
			changed := t.expire(now)
			if globalSysMgr == nil {
				return true
			}
			ss := globalSysMgr.Get(sysID)
			if ss == nil {
				// System torn down: drop its table unconditionally. A drained table never
				// reports changes, so the delete cannot be gated on expire().
				aggroTables.Delete(sysID)
				return true
			}
			if changed {
				broadcastAggression(ss)
			}
			return true
		})
		sweepEntityAggressors()
		sweepPoliceAggroCharges(now)
		return true
	})
}

// entityAggressorRetention is how long a per-entity aggression entry is kept.
// Longer than aggressionDuration: Entity.Aggressors drives who is red-boxed
// and is re-encoded into every SetState, so a fight gone quiet keeps its boxes.
const entityAggressorRetention = time.Hour

// sweepEntityAggressors prunes stale entries from every live entity's
// Aggressors map and drops the kill-hit record of entities untouched in the
// same window. NPC entity IDs are allocated fresh per spawn, so an unpruned
// map only ever grows.
func sweepEntityAggressors() {
	if globalSysMgr == nil {
		return
	}
	pruned := 0
	for _, ss := range globalSysMgr.All() {
		for _, e := range ss.AllEntities() {
			pruned += e.PruneAggressors(entityAggressorRetention)
		}
	}
	if pruned > 0 {
		log.Printf("[aggro] pruned %d stale entity aggression entries", pruned)
	}
}
