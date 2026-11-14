package main

// Overview refresh: dump what the client's overview still lists that its ballpark no longer holds.
// Two ghosts survive a grid or system change: a ball the client holds that the server forgot (only
// a SetState removes it), and an overview row with no ball (tactical.py appends a row per
// DoBallsAdded and DoBallRemove removes only the first match). OnStateSetupChance fixes the second:
// tactical reloads the overview from ballpark.balls. See docs/cmd-misc-notes.md#overview-refresh.

import (
	"fmt"
	"log"
	"time"

	"gurimoon/destiny"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
)

// overviewRebuildDelay is how long after a SetState the overview rebuild is pushed. It must land
// after michelle's ballQueue fires DoBallsAdded, since only a later rebuild clears the duplicate
// rows a mid-system SetState appends.
const overviewRebuildDelay = 4 * time.Second

// pushOverviewRebuild tells the client to rebuild its overview from its own
// ballpark. See the file comment.
func pushOverviewRebuild(c *evenet.Conn, accountID int64) error {
	return server.SendNotification(c, accountID, "OnStateSetupChance", "charid",
		marshal.PyTuple{Items: []marshal.PyRep{marshal.PyString{Value: "refresh"}}})
}

// scheduleOverviewRebuild pushes an overview rebuild overviewRebuildDelay from now if the pilot is
// still in the same ship and system. It also wakes the ballpark reconciler, so balls the server
// knows are stale are removed first.
func scheduleOverviewRebuild(ds *destinyConn, c *evenet.Conn, accountID int64) {
	ent, ss := ds.entity, ds.solarSystem
	if ent == nil || ss == nil || c == nil {
		return
	}
	time.AfterFunc(overviewRebuildDelay, func() {
		if ds.entity != ent || ds.solarSystem != ss {
			return
		}
		wakeBallparkReconciler(ent.ID)
		if err := pushOverviewRebuild(c, accountID); err != nil {
			log.Printf("[overview] rebuild push to entity=%d: %v", ent.ID, err)
		}
	})
}

// RefreshOverview implements /refresh [hard] (slash.BallparkRefresher). Plain prunes every ball the
// server knows is stale, then rebuilds the overview from the ballpark. Hard is a full ballpark
// resync (SetState + target replay) and the same rebuild; it reaches a ball the server forgot.
func (t *slashTeleporter) RefreshOverview(hard bool) (string, error) {
	ds := t.ds
	if ds == nil || ds.entity == nil || ds.solarSystem == nil || ds.dm == nil || t.isDocked() {
		return "/refresh: you must be in space.", nil
	}
	ent := ds.entity
	if ent.IsInWarp() || ent.IsWarpPending() {
		return "/refresh: wait until you land -- the landing batch is still rebuilding your grid.", nil
	}

	if hard {
		resyncBallpark(t.c, t.addr, uint64(t.accountID), ds)
		return fmt.Sprintf("/refresh hard: ballpark rebuilt; overview will reload in %s.", overviewRebuildDelay), nil
	}

	pruned := 0
	if doomed := staleBallparkIDs(ds); len(doomed) > 0 {
		if err := ent.SendStamped(func(stamp uint32) error {
			return destiny.SendRemoveBalls(ent.Conn, globalNodeID, ent.AccountID, stamp, doomed)
		}); err != nil {
			// Same rollback as the reconciler: a claim that outlives its packet
			// is the server believing balls gone that the client still holds.
			ent.MarkBallsKnown(doomed...)
			log.Printf("[%s] /refresh: prune send failed, rolled back %d claims: %v", t.addr, len(doomed), err)
		} else {
			pruned = len(doomed)
			log.Printf("[%s] /refresh: pruned %d stale balls: %v", t.addr, pruned, doomed)
		}
	}
	if err := pushOverviewRebuild(t.c, t.accountID); err != nil {
		return "", err
	}
	return fmt.Sprintf("/refresh: pruned %d stale ball(s), overview reloaded from your ballpark. "+
		"A ghost that survives this is a ball the server has forgotten -- use /refresh hard.", pruned), nil
}
