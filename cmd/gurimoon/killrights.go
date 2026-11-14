package main

// Kill rights, live half. seclaw/killright.go states the rule; this file mints
// the right when applyKillPenalty finds a kill unlawful, spends it when a kill
// was made under it, answers the legality
// gate (seclawDeps.KillRightHeld, run on every hit) from a short cache, serves
// standing2.GetMyKillRights, pushes OnKillRightChange(set, whenTimestamp,
// fromID, toID) to both parties and sweeps expiries. Client readers:
// consider.py CanAttackFreely, the character sheet's Kill Rights section
// (infosvc.py labels the timestamp killTime; it is the expiry) and
// consider.OnKillRightChange, which maintains both caches.

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/esp"
	"gurimoon/marshal"
	"gurimoon/seclaw"
	"gurimoon/server"
	"gurimoon/server/standing2"
	"gurimoon/tick"
)

const killRightCacheTTL = 30 * time.Second

type killRightKey struct{ holder, target int64 }
type killRightEntry struct {
	held  bool
	until time.Time
}

var (
	killRightDB    *sql.DB
	killRightMu    sync.Mutex
	killRightCache = map[killRightKey]killRightEntry{}
)

func init() {
	moduleBootHooks = append(moduleBootHooks, func(sqldb *sql.DB, _ *esp.SessionRegistry, tickMgr *tick.Manager) {
		if sqldb == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := evedb.EnsureKillRightsTable(ctx, sqldb)
		cancel()
		if err != nil {
			log.Printf("[killrights] %v — kill rights disabled", err)
			return
		}
		killRightDB = sqldb
		standing2.KillRightsProvider = killRightsReply
		if tickMgr != nil {
			startKillRightSweep(tickMgr)
		}
	})
}

// killRightHeldCached is the legality gate's lookup.
func killRightHeldCached(holder, target int64) bool {
	if killRightDB == nil || holder <= 0 || target <= 0 {
		return false
	}
	key := killRightKey{holder, target}
	now := time.Now()
	killRightMu.Lock()
	if e, ok := killRightCache[key]; ok && now.Before(e.until) {
		killRightMu.Unlock()
		return e.held
	}
	killRightMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	held, err := evedb.KillRightHeld(ctx, killRightDB, holder, target, now)
	cancel()
	if err != nil {
		log.Printf("[killrights] held %d→%d: %v", holder, target, err)
		return false
	}
	killRightMu.Lock()
	killRightCache[key] = killRightEntry{held: held, until: now.Add(killRightCacheTTL)}
	killRightMu.Unlock()
	return held
}

func forgetKillRightCache(holder, target int64) {
	killRightMu.Lock()
	delete(killRightCache, killRightKey{holder, target})
	killRightMu.Unlock()
}

// grantKillRightFor mints (or refreshes) the victim's right against the
// killer after an unlawful kill. Both parties are told if online; a bot on
// either side simply has no connection to tell.
func grantKillRightFor(ctx context.Context, sqldb *sql.DB, f seclaw.KillFacts) {
	if sqldb == nil || killRightDB == nil || f.VictimIsStructure || f.VictimCharID <= 0 || f.KillerCharID <= 0 {
		return
	}
	now := time.Now()
	// server_settings security.killRightDays; default is the era's 30 days.
	days := evedb.CachedSettingFloat("security.killRightDays", seclaw.KillRightDuration.Hours()/24, 1, 3650)
	expires := now.Add(time.Duration(days * 24 * float64(time.Hour)))
	if err := evedb.GrantKillRight(ctx, sqldb, f.VictimCharID, f.KillerCharID, now, expires, 0); err != nil {
		log.Printf("[killrights] grant %d→%d: %v", f.VictimCharID, f.KillerCharID, err)
		return
	}
	forgetKillRightCache(f.VictimCharID, f.KillerCharID)
	pushKillRightChange(true, expires, f.VictimCharID, f.KillerCharID)
	log.Printf("[killrights] %d holds a kill right against %d until %s", f.VictimCharID, f.KillerCharID, expires.Format(time.RFC3339))
}

// spendKillRightFor uses up the killer's right after a kill made under it
// (seclaw.KillRightPodWindow). The sweep deletes the row and pushes the unset
// again; the client's unset handler tolerates the repeat.
func spendKillRightFor(ctx context.Context, sqldb *sql.DB, f seclaw.KillFacts) {
	if sqldb == nil || killRightDB == nil || f.KillerCharID <= 0 || f.VictimCharID <= 0 {
		return
	}
	now := time.Now()
	until := now
	if !f.Pod {
		until = now.Add(seclaw.KillRightPodWindow)
	}
	spent, err := evedb.SpendKillRight(ctx, sqldb, f.KillerCharID, f.VictimCharID, now, until)
	if err != nil {
		log.Printf("[killrights] spend %d→%d: %v", f.KillerCharID, f.VictimCharID, err)
		return
	}
	if !spent {
		return
	}
	forgetKillRightCache(f.KillerCharID, f.VictimCharID)
	// A pod kill unsets at once so the client's CanAttackFreely stops skipping
	// the confirmation; a ship kill re-sets with the short expiry.
	pushKillRightChange(!f.Pod, until, f.KillerCharID, f.VictimCharID)
	log.Printf("[killrights] %d spent their kill right against %d (pod=%v)", f.KillerCharID, f.VictimCharID, f.Pod)
}

// pushKillRightChange tells holder and target. The client keys its caches on
// fromID (holder) / toID (target) against session.charid, so the same
// tuple serves both.
func pushKillRightChange(set bool, expires time.Time, fromID, toID int64) {
	setV := uint64(0)
	if set {
		setV = 1
	}
	args := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: setV},
		marshal.PyInt{Value: uint64(evedb.UnixToWin32(expires.Unix()))},
		marshal.PyInt{Value: uint64(fromID)},
		marshal.PyInt{Value: uint64(toID)},
	}}
	for _, id := range []int64{fromID, toID} {
		if conn, acct := charConnLookup(id); conn != nil {
			if err := server.SendNotification(conn, acct, "OnKillRightChange", "charid", args); err != nil {
				log.Printf("[killrights] push to %d: %v", id, err)
			}
		}
	}
}

// killRightsReply is standing2.GetMyKillRights: ({target: expiry}, {holder:
// expiry}), rights I hold and rights held against me, as win32 filetimes.
func killRightsReply(ctx context.Context, sqldb *sql.DB, charID int64) marshal.PyRep {
	held, against := map[int64]time.Time{}, map[int64]time.Time{}
	if sqldb != nil && killRightDB != nil && charID != 0 {
		var err error
		held, against, err = evedb.ListKillRights(ctx, sqldb, charID, time.Now())
		if err != nil {
			log.Printf("[killrights] list for %d: %v", charID, err)
			held, against = map[int64]time.Time{}, map[int64]time.Time{}
		}
	}
	dict := func(m map[int64]time.Time) marshal.PyDict {
		d := marshal.PyDict{}
		for id, t := range m {
			d.Items = append(d.Items, marshal.DictEntry{
				Key:   marshal.PyInt{Value: uint64(id)},
				Value: marshal.PyInt{Value: uint64(evedb.UnixToWin32(t.Unix()))},
			})
		}
		return d
	}
	return marshal.PyTuple{Items: []marshal.PyRep{dict(held), dict(against)}}
}

func startKillRightSweep(tickMgr *tick.Manager) {
	tickMgr.RegisterPeriodic("kill-right-sweep", 60, func(uint64) bool {
		if killRightDB == nil {
			return true
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		now := time.Now()
		gone, err := evedb.ExpireKillRights(ctx, killRightDB, now)
		if err != nil {
			log.Printf("[killrights] sweep: %v", err)
			return true
		}
		for _, p := range gone {
			forgetKillRightCache(p.FromID, p.ToID)
			pushKillRightChange(false, now, p.FromID, p.ToID)
		}
		if len(gone) > 0 {
			log.Printf("[killrights] %d right(s) expired", len(gone))
		}
		return true
	})
}
