package main

// POS Refining Array; rules in db/starbaserefinedb.go. Run Refining Process is
// a bare InventoryBound.RunRefiningProcess() on the array's bound inventory:
// no arguments, reply ignored, results visible as the Processing Area (flag 5)
// changing. A refusal here may be an exception: it is a direct call and the
// client's standard error box shows it.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"

	evedb "gurimoon/db"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
	"gurimoon/tick"
)

// handleRunRefiningProcess answers InventoryBound.RunRefiningProcess.
func handleRunRefiningProcess(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket,
	accountID, charID int64, sqldb *sql.DB, ds *destinyConn) error {

	arrayID := bindNumFromCallStream(cs)
	if sqldb == nil || ds == nil || ds.solarSystem == nil || !isStructureBay(ds, arrayID) {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	slim, _ := ds.solarSystem.GetStarbaseSlim(uint32(arrayID))
	if slim.GroupID != evedb.GroupRefiningArray {
		return corpUserErr(c, pkt, accountID, "That structure is not a refinery.")
	}
	ctx, cancel := context.WithTimeout(context.Background(), starbaseSweepTimeout)
	defer cancel()

	rec, found, err := evedb.GetStarbaseStructure(ctx, sqldb, arrayID)
	if err != nil || !found || rec.State != evedb.StarbaseStateOnline {
		return corpUserErr(c, pkt, accountID, "The refinery must be anchored and online.")
	}
	if tower, ok, _ := evedb.GetStarbase(ctx, sqldb, rec.ControlTowerID); !ok ||
		(tower.State != evedb.StarbaseStateOnline && tower.State != evedb.StarbaseStateReinforced) {
		return corpUserErr(c, pkt, accountID, "The refinery's control tower is not online.")
	}
	if bd, ok := ds.solarSystem.GetStarbaseBall(uint32(arrayID)); !ok || !shipWithin(ds, bd.Pos, itemTransferRange+bd.Radius) {
		return corpUserErr(c, pkt, accountID, "You are too far away from the refinery.")
	}
	if !structureTakeAllowed(ctx, addr, sqldb, ds, arrayID, charID, evedb.FlagRefineryInput) {
		return corpUserErr(c, pkt, accountID, "You are not permitted to operate this refinery.")
	}

	yield := 0.35
	if v, ok, _ := evedb.GetTypeAttributeFloat(ctx, sqldb, slim.TypeID, "refiningYieldMultiplier"); ok && v > 0 {
		yield = v
	}
	durMs := 3600000.0
	if v, ok, _ := evedb.GetTypeAttributeFloat(ctx, sqldb, slim.TypeID, "operationalDuration"); ok && v > 0 {
		durMs = v
	}

	changes, finishAt, err := evedb.StartRefining(ctx, sqldb, arrayID, charID, yield, durMs)
	switch {
	case errors.Is(err, evedb.ErrRefineryBusy):
		return corpUserErr(c, pkt, accountID, "The refinery is already processing a batch.")
	case errors.Is(err, evedb.ErrNothingToRefine):
		return corpUserErr(c, pkt, accountID, "There is nothing refinable in the processing area.")
	case err != nil:
		log.Printf("[%s] RunRefiningProcess %d: %v", addr, arrayID, err)
		return corpUserErr(c, pkt, accountID, "The refinery could not start.")
	}
	for _, ch := range changes {
		refineryNotify(c, accountID, arrayID, ch)
	}
	if globalTickMgr != nil {
		globalTickMgr.ScheduleOnce(fmt.Sprintf("pos-refine-%d", arrayID),
			evedb.FiletimeToTime(finishAt), func() { finishRefinery(sqldb, arrayID) })
	}
	log.Printf("[%s] refinery %d started by char %d: %d stack(s), yield %.2f, %.0f min",
		addr, arrayID, charID, len(changes), yield, durMs/60000)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// refineryNotify sends the consuming half of a run to the pilot who started
// it: a shrunk stack carries its old quantity, a consumed one is removed.
func refineryNotify(c *evenet.Conn, accountID, arrayID int64, ch evedb.RefineChange) {
	ctx, cancel := context.WithTimeout(context.Background(), starbaseCtxTimeout)
	defer cancel()
	if ch.NewQty > 0 {
		if row, err := evedb.GetEntityRow(ctx, globalDB, ch.ItemID); err == nil && row != nil {
			_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
				evedb.BuildEntityRow(row), marshal.PyDict{Items: []marshal.DictEntry{
					{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(ch.OldQty)}},
				}},
			}})
		}
		return
	}
	// Deleted: the client drops a row whose change is None; rebuild the row
	// it knew from what we know.
	row := &evedb.EntityRow{ItemID: ch.ItemID, TypeID: ch.TypeID, OwnerID: ch.OwnerID,
		LocationID: arrayID, Flag: evedb.FlagRefineryInput, Quantity: ch.OldQty}
	_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
		evedb.BuildEntityRow(row), marshal.PyNone{},
	}})
}

// finishRefinery delivers a finished run and tells the pilots in the system
// who could have the Processing Area open (the owner corp's members).
func finishRefinery(sqldb *sql.DB, arrayID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), starbaseSweepTimeout)
	defer cancel()
	if at := evedb.RefineryBusyUntil(ctx, sqldb, arrayID); at == 0 || at > server.Win32TimeNow() {
		return
	}
	touched, err := evedb.FinishRefining(ctx, sqldb, arrayID)
	if err != nil {
		log.Printf("[pos] refinery %d finish: %v", arrayID, err)
		return
	}
	log.Printf("[pos] refinery %d delivered %d mineral stack(s)", arrayID, len(touched))
	rec, found, err := evedb.GetStarbaseStructure(ctx, sqldb, arrayID)
	if err != nil || !found || globalSysMgr == nil {
		return
	}
	ss := globalSysMgr.Get(rec.SolarSystemID)
	if ss == nil {
		return
	}
	for _, id := range touched {
		row, err := evedb.GetEntityRow(ctx, sqldb, id)
		if err != nil || row == nil {
			continue
		}
		args := marshal.PyTuple{Items: []marshal.PyRep{evedb.BuildEntityRow(row), marshal.PyDict{}}}
		for _, p := range ss.GetAllPlayersInSystem() {
			if p.Conn != nil && p.Slim.CorpID == rec.OwnerCorpID {
				_ = server.SendNotification(p.Conn, int64(p.AccountID), "OnItemChange", "charid", args)
			}
		}
	}
}

// startRefinerySweep delivers finished runs every 30 s, the restart-proof
// half of the one-shot armed at start.
func startRefinerySweep(tickMgr *tick.Manager, sqldb *sql.DB) {
	if tickMgr == nil || sqldb == nil {
		return
	}
	tickMgr.RegisterPeriodic("starbase-refinery-sweep", 30, func(uint64) bool {
		ctx, cancel := context.WithTimeout(context.Background(), starbaseCtxTimeout)
		ids, err := evedb.DueRefineJobs(ctx, sqldb, server.Win32TimeNow())
		cancel()
		if err != nil {
			log.Printf("[pos] refinery sweep: %v", err)
			return true
		}
		for _, id := range ids {
			finishRefinery(sqldb, id)
		}
		return true
	})
}
