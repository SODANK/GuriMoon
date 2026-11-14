package main

// Logoff in space, Crimewatch 1.0. A ship whose pilot was in PvP (either side,
// pvptimers.go) within crimewatch.pvpLogoffMin stays in space that long after
// the connection drops; any other ship for crimewatch.cleanLogoffSec. The
// lingering ship is the pilot's own entity and destinyConn behind a closed
// connection, so it can be shot, killed and podded: this goroutine drains
// ds.playerDeath with the ordinary death handlers in place of the gone
// connection goroutine. A relog ends the linger first (endLogoffLinger), so the
// new session loads the ship's live position.

import (
	"database/sql"
	"log"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/npc"
	"gurimoon/safego"
	"gurimoon/syncx"
)

type logoffLinger struct {
	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

// logoffLingers maps charID to the *logoffLinger holding their ship in space.
var logoffLingers syncx.Map[int64, *logoffLinger]

// logoffLingerFor is the rule: the PvP window if the pilot was in PvP within
// it, else the clean-logoff delay. Unix seconds.
func logoffLingerFor(now, pvpAt int64, pvpWindow, clean time.Duration) time.Duration {
	if pvpAt > 0 && pvpWindow > 0 && now-pvpAt < int64(pvpWindow/time.Second) {
		return pvpWindow
	}
	return clean
}

// startLogoffLinger hands ds to a linger goroutine and reports true, or
// reports false when the ship should leave space now. remove is the removal
// half of the disconnect teardown; it runs exactly once, on the goroutine.
func startLogoffLinger(ds *destinyConn, sqldb *sql.DB, npcMgr *npc.NPCManager, remove func()) bool {
	if ds == nil || ds.charID <= 0 || ds.entity == nil || ds.entity.IsDestroyed() {
		return false
	}
	pvpWindow := evedb.CachedSettingDuration("crimewatch.pvpLogoffMin", time.Minute, 15, 0, 60)
	clean := evedb.CachedSettingDuration("crimewatch.cleanLogoffSec", time.Second, 60, 0, 900)
	d := logoffLingerFor(time.Now().Unix(), pvpTimesFor(ds.charID).pvpAt, pvpWindow, clean)
	if d <= 0 {
		return false
	}
	l := &logoffLinger{stop: make(chan struct{}), done: make(chan struct{})}
	if prev, loaded := logoffLingers.LoadOrStore(ds.charID, l); loaded {
		// Cannot happen: a login ends the previous linger before it registers.
		log.Printf("[logoff] charID=%d already lingering (%p) — removing at once", ds.charID, prev)
		return false
	}
	charID := ds.charID
	log.Printf("[logoff] charID=%d ship %d stays in space for %s", charID, ds.shipID, d)
	go func() {
		defer safego.Recover("startLogoffLinger")
		defer close(l.done)
		defer logoffLingers.Delete(charID)
		timer := time.NewTimer(d)
		defer timer.Stop()
		for {
			select {
			case req := <-ds.playerDeath:
				if req.pod {
					handlePlayerPodKilled(ds, sqldb)
				} else {
					handlePlayerShipDestroyed(ds, sqldb, npcMgr)
				}
				if ds.entity == nil {
					// Podded: the pilot is at their clone and out of space.
					log.Printf("[logoff] charID=%d podded while logged off", charID)
					return
				}
			case <-timer.C:
				log.Printf("[logoff] charID=%d linger over — leaving space", charID)
				remove()
				return
			case <-l.stop:
				log.Printf("[logoff] charID=%d logged back in — ending linger", charID)
				remove()
				return
			}
		}
	}()
	return true
}

// endLogoffLinger removes a character's lingering ship before their new
// session loads them, and waits for the removal so the saved position, damage
// and station are final. A death in progress finishes first.
func endLogoffLinger(charID int64) {
	v, ok := logoffLingers.Load(charID)
	if !ok {
		return
	}
	l := v
	l.stopOnce.Do(func() { close(l.stop) })
	select {
	case <-l.done:
	case <-time.After(15 * time.Second):
		log.Printf("[logoff] charID=%d linger did not end within 15s", charID)
	}
}
