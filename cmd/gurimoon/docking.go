package main

// Docking clearance. A Dock request is answered at once; the ship burns in toward the
// station, and OnDockingAccepted plus the session change go out once the grant time has
// passed and the ship is inside docking range. destiny/dockclearance.go keeps one line per
// station, shared with the rolebots projections.
//
// See docs/jump-notes.md#docking-clearance.

import (
	"context"
	"database/sql"
	"log"
	"strconv"
	"time"

	"gurimoon/destiny"
	evedb "gurimoon/db"
	"gurimoon/esp"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/safego"
	"gurimoon/server"
	"gurimoon/tick"
)

// dockFollowSurfaceM is how far off the station's skin the burn-in aims to
// stop: well inside docking range, short of the hull.
const dockFollowSurfaceM = 500.0

// pendingDock is a pilot's open docking request.
type pendingDock struct {
	stationID   int64
	requestedAt time.Time
	grantAt     time.Time
	fee         float64       // docking fee the pilot accepted; charged at the door
	stop        chan struct{} // closes the checker goroutine
}

// dockGate is the owner's say over a dock at a player-owned station, run when
// Dock is called: the Docking access rule, then the docking fee. paid is the
// fee the client offers on its retry (Dock's second argument). Returns the fee
// to charge at the door, or the call is already answered with a refusal.
func dockGate(c *evenet.Conn, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB,
	ds *destinyConn, dockStationID int64, paid float64) (fee float64, refused bool, err error) {
	if sqldb == nil || dockStationID == 0 {
		return 0, false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if reason := evedb.StationServiceRefusal(ctx, sqldb, dockStationID, charID, evedb.StationServiceDocking); reason != "" {
		return 0, true, clientMsgErr(c, pkt, accountID, "DockingRequestDenied",
			map[string]marshal.PyRep{"reason": marshal.PyString{Value: reason}})
	}
	fee = evedb.DockingFee(ctx, sqldb, dockStationID, charID, ds.shipTypeID)
	if fee <= 0 {
		return 0, false, nil
	}
	// The client asks AskPayDockingFee and calls Dock(target, amount) again with the
	// amount it was quoted (environment/actions/actions.py); an old or missing offer is quoted anew.
	if paid < fee {
		return 0, true, clientMsgErr(c, pkt, accountID, "DockingRequestDeniedPaymentRequired",
			map[string]marshal.PyRep{"amount": marshal.PyInt{Value: uint64(fee)}})
	}
	if have := evedb.CharBalance(ctx, sqldb, charID); have < fee {
		return 0, true, refuseErr(c, pkt, accountID, evedb.NotEnoughMoneyErr(fee, have))
	}
	return fee, false, nil
}

// chargeDockingFee takes the accepted fee as the ship docks. "" when paid
// (or nothing was owed), else the refusal text for denyDock.
func chargeDockingFee(addr string, charID int64, sqldb *sql.DB, dockStationID int64, fee float64) string {
	if fee <= 0 || sqldb == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := evedb.ChargeStationFee(ctx, sqldb, charID, dockStationID, fee, evedb.RefDockingFee, "Docking fee"); err != nil {
		log.Printf("[%s] dock station=%d: docking fee %.0f not paid: %v", addr, dockStationID, fee, err)
		return "You could not pay the docking fee."
	}
	log.Printf("[%s] dock station=%d: docking fee %.0f ISK paid", addr, dockStationID, fee)
	return ""
}

func init() {
	moduleBootHooks = append(moduleBootHooks, func(sqldb *sql.DB, _ *esp.SessionRegistry, _ *tick.Manager) {
		loadDockClearanceSettings(sqldb)
		go func() {
			defer safego.Recover("init")
			t := time.NewTicker(30 * time.Second)
			defer t.Stop()
			for range t.C {
				loadDockClearanceSettings(sqldb)
			}
		}()
	})
}

// loadDockClearanceSettings reads the docking.* server_settings rows. A row
// that is missing or unparsable keeps its default.
func loadDockClearanceSettings(sqldb *sql.DB) {
	if sqldb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cfg := destiny.DefaultDockClearanceConfig()
	num := func(name string, apply func(float64)) {
		v, ok, err := evedb.GetServerSetting(ctx, sqldb, name)
		if err != nil || !ok {
			return
		}
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			apply(f)
		}
	}
	secs := func(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }
	num("docking.clearanceMinSec", func(f float64) { cfg.MinDelay = secs(f) })
	num("docking.clearanceMaxSec", func(f float64) { cfg.MaxDelay = secs(f) })
	num("docking.spacingSec", func(f float64) { cfg.Spacing = secs(f) })
	num("docking.rangeM", func(f float64) { cfg.RangeM = f })
	num("docking.warpInM", func(f float64) { cfg.WarpInM = f })
	num("docking.requestTimeoutSec", func(f float64) { cfg.Timeout = secs(f) })
	destiny.SetDockClearanceConfig(cfg)
}

// dockStationBall finds the station's ball in the pilot's system.
func dockStationBall(ds *destinyConn, stationID int64) (destiny.BallDesc, bool) {
	if ds.solarSystem == nil {
		return destiny.BallDesc{}, false
	}
	for i := range ds.solarSystem.CelBalls {
		if int64(ds.solarSystem.CelBalls[i].EntityID) == stationID {
			return ds.solarSystem.CelBalls[i], true
		}
	}
	return destiny.BallDesc{}, false
}

// dockSurfaceDist is the gap between the ship's hull and the station's.
func dockSurfaceDist(ds *destinyConn, st destiny.BallDesc) float64 {
	shipR := 0.0
	if ds.dm != nil {
		shipR = ds.dm.Radius()
	}
	return ds.entity.Pos().Sub(st.Pos).Len() - st.Radius - shipR
}

// requestDock handles destiny.Dock once the caller's own guards (destroyed,
// scrambled, pinned) have passed. It answers the call, files the clearance
// request and starts the burn-in.
func requestDock(c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID, charID int64,
	stationID *int64, sqldb *sql.DB, ds *destinyConn, dockStationID int64, fee float64) error {

	st, ok := dockStationBall(ds, dockStationID)
	if !ok || ds.entity == nil || ds.dm == nil {
		// No ballpark to approach in: dock at once instead of stranding the pilot.
		log.Printf("[%s] → destiny.Dock station=%d: no station ball/entity — docking at once", addr, dockStationID)
		if reason := chargeDockingFee(addr, charID, sqldb, dockStationID, fee); reason != "" {
			return clientMsgErr(c, pkt, accountID, "DockingRequestDenied",
				map[string]marshal.PyRep{"reason": marshal.PyString{Value: reason}})
		}
		if err := server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}); err != nil {
			return err
		}
		return completeDock(c, addr, accountID, charID, stationID, sqldb, ds, dockStationID)
	}
	if err := server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}); err != nil {
		return err
	}

	now := time.Now()
	grant := destiny.RequestDockClearance(dockStationID, ds.shipID, now)
	if pd := ds.pendingDock; pd != nil && pd.stationID == dockStationID {
		// The same request again (the client re-asks after its 30 s timer): keep the running checker.
		pd.grantAt = grant
		pd.fee = fee
	} else {
		cancelPendingDock(ds)
		pd := &pendingDock{stationID: dockStationID, requestedAt: now, grantAt: grant, fee: fee, stop: make(chan struct{})}
		ds.pendingDock = pd
		go runDockChecker(ds, pd.stop)
	}

	cfg := destiny.DockClearanceSettings()
	surface := dockSurfaceDist(ds, st)
	log.Printf("[%s] → destiny.Dock station=%d surface=%.0fm clearance in %.1fs",
		addr, dockStationID, surface, grant.Sub(now).Seconds())
	if surface > cfg.RangeM {
		dockBurnIn(c, accountID, ds, st)
	}
	return nil
}

// dockBurnIn points the ship at the station at full speed: the pilot's own Approach, issued
// by the station's traffic control.
func dockBurnIn(c *evenet.Conn, accountID int64, ds *destinyConn, st destiny.BallDesc) {
	dm := ds.dm
	dm.SetSpeedFraction(1.0)
	// The server sim follows by centre distance and the client's FollowBall range is surface to
	// surface; convert so both stop at the same place.
	dm.Follow(st.EntityID, st.Radius+dm.Radius()+dockFollowSurfaceM)
	dm.SetNavWireRange(dockFollowSurfaceM)
	ds.syncStamp()
	actions := []marshal.PyRep{
		destiny.BuildSimpleAction("SetSpeedFraction",
			marshal.PyInt{Value: uint64(dm.EntityID)},
			marshal.PyReal{Value: 1.0}),
		destiny.BuildSimpleAction("FollowBall",
			marshal.PyInt{Value: uint64(dm.EntityID)},
			marshal.PyInt{Value: uint64(st.EntityID)},
			marshal.PyReal{Value: dockFollowSurfaceM}),
	}
	if err := dm.SendStampedUrgent(func(stamp uint32) error {
		return destiny.SendActionList(c, globalNodeID, uint64(accountID), stamp, actions...)
	}); err != nil {
		log.Printf("dock burn-in: %v", err)
		return
	}
	broadcastActionList(ds, actions...)
}

// runDockChecker pokes the connection goroutine twice a second while a
// request is open. All the deciding happens there, in tickPendingDock.
func runDockChecker(ds *destinyConn, stop chan struct{}) {
	defer safego.Recover("runDockChecker")
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			select {
			case ds.dockCheck <- struct{}{}:
			default:
			}
		}
	}
}

// cancelPendingDock drops the pilot's open request, if any.
func cancelPendingDock(ds *destinyConn) {
	if ds == nil || ds.pendingDock == nil {
		return
	}
	close(ds.pendingDock.stop)
	ds.pendingDock = nil
	if ds.shipID != 0 {
		destiny.ReleaseDockClearance(ds.shipID)
	}
}

// denyDock refuses the open request with the client's own message, which
// carries the "docking request denied" voice line.
func denyDock(c *evenet.Conn, accountID int64, ds *destinyConn, reason string) {
	cancelPendingDock(ds)
	pushRemoteMessage(c, accountID, "DockingRequestDenied", map[string]marshal.PyRep{
		"reason": marshal.PyString{Value: reason},
	})
}

// tickPendingDock runs on the connection goroutine: dock the ship once it is
// cleared and in range, drop the request if it can no longer be honoured.
func tickPendingDock(c *evenet.Conn, addr string, accountID, charID int64, stationID *int64,
	sqldb *sql.DB, ds *destinyConn) error {

	pd := ds.pendingDock
	if pd == nil {
		return nil
	}
	if ds.dm == nil || ds.entity == nil || ds.entity.IsDestroyed() || ds.entity.IsInWarp() {
		cancelPendingDock(ds)
		return nil
	}
	cfg := destiny.DockClearanceSettings()
	now := time.Now()
	if now.Sub(pd.requestedAt) > cfg.Timeout {
		log.Printf("[%s] dock request station=%d timed out", addr, pd.stationID)
		denyDock(c, accountID, ds, "Your ship did not reach the docking perimeter in time.")
		return nil
	}
	if now.Before(pd.grantAt) {
		return nil
	}
	st, ok := dockStationBall(ds, pd.stationID)
	if !ok {
		cancelPendingDock(ds)
		return nil
	}
	if dockSurfaceDist(ds, st) > cfg.RangeM {
		return nil
	}
	// Cleared and at the door. The request-time guards are re-run: a pilot can
	// open fire or be pinned during the burn-in.
	if aggressionLocked(charID) {
		log.Printf("[%s] dock station=%d: DENIED at clearance — recent aggression", addr, pd.stationID)
		cancelPendingDock(ds)
		pushRemoteMessage(c, accountID, "CantDockAfterAggression", nil)
		return nil
	}
	if pinned, mod := shipImmobilised(ds); pinned {
		log.Printf("[%s] dock station=%d: DENIED at clearance — pinned by %s", addr, pd.stationID, mod)
		denyDock(c, accountID, ds, "Your ship is immobilised.")
		return nil
	}
	// The owner can change the Docking rule during the burn-in.
	ruleCtx, ruleCancel := context.WithTimeout(context.Background(), 5*time.Second)
	reason := evedb.StationServiceRefusal(ruleCtx, sqldb, pd.stationID, charID, evedb.StationServiceDocking)
	ruleCancel()
	if reason == "" {
		reason = chargeDockingFee(addr, charID, sqldb, pd.stationID, pd.fee)
	}
	if reason != "" {
		log.Printf("[%s] dock station=%d: DENIED at clearance — %s", addr, pd.stationID, reason)
		denyDock(c, accountID, ds, reason)
		return nil
	}
	dockStationID := pd.stationID
	log.Printf("[%s] dock station=%d: cleared after %.1fs", addr, dockStationID, now.Sub(pd.requestedAt).Seconds())
	cancelPendingDock(ds)
	return completeDock(c, addr, accountID, charID, stationID, sqldb, ds, dockStationID)
}

// completeDock is the dock itself: acceptance push, persistence, leaving
// space and the session change into the station.
func completeDock(c *evenet.Conn, addr string, accountID, charID int64, stationID *int64,
	sqldb *sql.DB, ds *destinyConn, dockStationID int64) error {
	// OnDockingAccepted stops client navigation and shows the docking caption while the session
	// change is in flight (spaceMgr.OnDockingAccepted). Only stationID is read from its args.
	zeroPos := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyReal{Value: 0}, marshal.PyReal{Value: 0}, marshal.PyReal{Value: 0},
	}}
	_ = server.SendNotification(c, accountID, "OnDockingAccepted", "charid",
		marshal.PyTuple{Items: []marshal.PyRep{
			zeroPos, zeroPos, marshal.PyInt{Value: uint64(dockStationID)},
		}})
	// Persist ship entity location and character station before clearing state.
	if sqldb != nil && dockStationID != 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if ds.shipID != 0 {
			if err := evedb.UpdateEntityLocation(ctx, sqldb, ds.shipID, dockStationID); err != nil {
				log.Printf("[%s] Dock: UpdateEntityLocation shipID=%d: %v", addr, ds.shipID, err)
			}
		}
		if charID != 0 {
			if err := evedb.UpdateCharacterStation(ctx, sqldb, charID, dockStationID, ds.sysID, ds.constellationID, ds.regionID); err != nil {
				log.Printf("[%s] Dock: UpdateCharacterStation charID=%d: %v", addr, charID, err)
			}
			// Auto-set home station on first dock if not yet configured.
			if homeID, err := evedb.GetHomeStationID(ctx, sqldb, charID); err == nil && homeID == 0 {
				if err := evedb.SetHomeStationID(ctx, sqldb, charID, dockStationID); err != nil {
					log.Printf("[%s] Dock: SetHomeStationID charID=%d: %v", addr, charID, err)
				} else {
					log.Printf("[%s] Dock: auto-set homeStation=%d for charID=%d", addr, dockStationID, charID)
				}
			}
		}
		// Seed a rookie ship if docking in a capsule with no other ships at station.
		if ds.shipTypeID == 670 && charID != 0 {
			if rookieID, err := evedb.EnsureRookieShip(ctx, sqldb, charID, dockStationID); err != nil {
				log.Printf("[%s] Dock EnsureRookieShip: %v", addr, err)
			} else if rookieID != 0 {
				log.Printf("[%s] Dock: seeded rookie ship %d", addr, rookieID)
			}
		}
	}
	// Cancel pending target locks and clear locked targets before docking.
	ds.targetMu.Lock()
	for tid, cancelFn := range ds.lockingTargets {
		cancelFn()
		delete(ds.lockingTargets, tid)
	}
	ds.targets = nil
	ds.targetMu.Unlock()
	_ = server.SendNotification(c, accountID, "OnTarget", "charid",
		marshal.PyTuple{Items: []marshal.PyRep{marshal.PyString{Value: "clear"}}})

	// Cancel active weapon/mining/repair cycles.
	for modID, cancelFn := range ds.cancelWeapon {
		cancelFn()
		delete(ds.cancelWeapon, modID)
	}
	for modID, sess := range ds.miningSessions {
		sess.cancel()
		delete(ds.miningSessions, modID)
	}
	// A reload in progress completes on docking: docked loads are instant.
	ds.finishReloadsNow()

	// Remove this player from the shared system state and notify bubble-mates.
	unregisterDock(addr, ds, sqldb)
	shipID := ds.shipID
	ds.dm = nil             // clear in-space state
	ds.setStateSent = false // allow next undock's GetFormations to send SetState
	// Update the connection's stationID so subsequent station service calls
	// (GetStationItemBits, hangar items, undock) use the correct station.
	*stationID = dockStationID
	updateSessionLocation(addr, dockStationID, ds.sysID)
	if globalStationReg != nil && dockStationID != 0 && charID != 0 {
		globalStationReg.Join(c, dockStationID, charID)
	}
	if ds.sysID != 0 && dockStationID != 0 {
		return sendDockSessionChange(c, addr, uint64(accountID), ds.sysID, dockStationID, shipID)
	}
	return nil
}
