package main

// Server-side hooks for starbase industry (server/ram/posinstall.go): the
// RAM service has no live grid and no corp-role helpers, so it asks here.

import (
	"context"
	"database/sql"
	"log"

	evedb "gurimoon/db"
	"gurimoon/server/ram"
	"gurimoon/tick"
)

// posIndustryRange is how close a pilot must be to use an array: the arrays'
// maxOperationalDistance (attribute 715) is 3000 m, measured from the ball's
// surface.
const posIndustryRange = 3000.0 + 1000.0

func installPOSIndustryHooks(tickMgr *tick.Manager, sqldb *sql.DB) {
	if sqldb == nil {
		return
	}
	ram.StructureInRange = func(charID, structureID, solarSystemID int64) bool {
		if globalSysMgr == nil {
			return false
		}
		ss := globalSysMgr.Get(solarSystemID)
		if ss == nil {
			return false
		}
		p := ss.GetPlayerByCharID(charID)
		bd, ok := ss.GetStarbaseBall(uint32(structureID))
		if p == nil || !ok {
			return false
		}
		return p.Pos().Sub(bd.Pos).Len() <= posIndustryRange+bd.Radius
	}
	ram.CorpHangarMayTake = func(corpID, charID, flag int64) bool {
		ctx, cancel := context.WithTimeout(context.Background(), starbaseCtxTimeout)
		defer cancel()
		return corpHangarMayTake(ctx, sqldb, corpID, charID, flag)
	}

	// Lines follow the arrays' states: reconcile at boot and every minute, which
	// covers every path an array goes offline, unanchors or dies by.
	sync := func() {
		ctx, cancel := context.WithTimeout(context.Background(), starbaseSweepTimeout)
		defer cancel()
		if err := evedb.SyncStructureAssemblyLines(ctx, sqldb, 0); err != nil {
			log.Printf("[pos] assembly line sync: %v", err)
		}
	}
	sync()
	if tickMgr != nil {
		tickMgr.RegisterPeriodic("starbase-assembly-line-sync", 60, func(uint64) bool {
			sync()
			return true
		})
	}
}
