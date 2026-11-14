package main

// Every way into space (undock, gate jump, jump drive, login, in-space ship
// swap, a module projection) passes onPilotEnteredSpace. Being an outlaw is not
// a crime: a pilot at or below seclaw.OutlawStatus is a free target for other
// pilots (seclaw.ShotIsCrime's VictimOutlaw rung) and nothing more, so no flag
// is raised here. The empire police or navy may send a squad (policeCheck,
// factionpolice.go). A pilot who arrives still carrying a global criminal flag
// meets CONCORD again (concordFollow).

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	"gurimoon/destiny"
	"gurimoon/npc"
	"gurimoon/seclaw"
)

// spaceEntry says how a pilot came to be in space in this system. A login is
// the only entry whose arrival rules are delayed: the client has no ballpark yet.
type spaceEntry uint8

const (
	entryMove  spaceEntry = iota // undock, gate jump, teleport, in-space ship swap
	entryLogin                   // logged in where they logged off
)

// loginEntryGrace keeps a pilot who logged off in space from meeting the
// police before their client has drawn the ballpark.
const loginEntryGrace = 30 * time.Second

// entryDelay is how long after arrival the entry rules run.
func entryDelay(entry spaceEntry) time.Duration {
	if entry == entryLogin {
		return loginEntryGrace
	}
	return 0
}

// onPilotEnteredSpace runs the arrival rules for a pilot that just entered ss.
// A delayed run first re-checks that the same entity is still registered in ss,
// so a pilot who docked, jumped or died inside the grace is left alone.
func onPilotEnteredSpace(e *destiny.Entity, ss *destiny.SolarSystem, sqldb *sql.DB, npcMgr *npc.NPCManager, entry spaceEntry) {
	if e == nil || ss == nil || !e.IsPlayer || e.Slim.CharID <= 0 {
		return
	}
	if e.Slim.SecurityStatus <= seclaw.OutlawStatus {
		log.Printf("[outlaw] charID=%d (sec status %.2f) entered system %d sec=%.2f — free target",
			e.Slim.CharID, e.Slim.SecurityStatus, ss.ID, ss.Security)
	}
	fire := func() {
		if ss.GetEntity(e.ID) != e {
			return
		}
		concordFollow(ss, e)
		policeCheck(ss, e)
	}
	delay := entryDelay(entry)
	if delay <= 0 || globalTickMgr == nil {
		fire()
		return
	}
	globalTickMgr.ScheduleOnce(fmt.Sprintf("space-entry-%d", e.Slim.CharID), time.Now().Add(delay), fire)
}
