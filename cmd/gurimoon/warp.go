package main

import (
	"context"
	"database/sql"
	"log"
	"math"
	"sort"
	"strconv"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/dogma"
	"gurimoon/logx"
	"gurimoon/marshal"
	_ "gurimoon/modules"
	evenet "gurimoon/net"
	"gurimoon/safego"
	"gurimoon/server"
	"gurimoon/tick"
)

// startPlayerWarp tears down combat state (weapons, mining, locks) and runs the player
// warp (align, tunnel, arrival) to celestialPos on a goroutine. The landing is offset
// toward the ship by targetRadius+landingBuffer unless landingOverride is set. The RPC
// response must already be sent. onArrive runs at tunnel end (dungeon room pre-render).
func startPlayerWarp(c *evenet.Conn, addr string, accountID int64, sqldb *sql.DB,
	ds *destinyConn, dm *destiny.Manager, celestialPos destiny.Vec3,
	targetRadius, landingBuffer float64, landingOverride *destiny.Vec3, onArrive func()) {

	// Only the covert ops cloak warps. WarpToStuff refuses on its call; the journal, gang and
	// acceleration-gate warps have answered theirs, so the refusal is a pushed message.
	if cloakBlocksWarp(ds) {
		log.Printf("[%s] warp: BLOCKED — cloaked", addr)
		pushRemoteMessage(c, accountID, "CantWarpWhileCloaked", nil)
		return
	}
	// Warping away withdraws any docking request (a warp-to-dock files its request after landing).
	cancelPendingDock(ds)
	if ds.entity != nil && destiny.TraceAccount.Load() == uint64(accountID) {
		destiny.TraceEntity.Store(ds.entity)
	}

	var landing destiny.Vec3
	if landingOverride != nil {
		landing = *landingOverride
	} else {
		landing = warpLandingPoint(dm.Pos, celestialPos, targetRadius+landingBuffer)
	}
	// roomAnchor is the landing before the arrival lane moves this ship, so every pilot
	// entering one site builds the room in the same place.
	roomAnchor := landing
	// Arrival lane: slot 0 is the landing itself, so a quiet destination is unchanged. The
	// claim also sees ships parked there. See docs/destiny-arrival-lanes.md#why-a-lane.
	laneDir := landing.Sub(celestialPos)
	if laneDir.IsZero() {
		laneDir = destiny.Vec3{X: 1}
	}
	var laneExclude uint32
	if ds.entity != nil {
		laneExclude = ds.entity.ID
	}
	landing, laneClaim := destiny.ClaimArrivalPointEx(ds.solarSystem, ds.sysID, roomAnchor, laneDir, dm.Radius(), laneExclude)
	alignSecs, tunnelSecs, totalSecs := dm.WarpPhases(celestialPos)
	// The dll drops out of warp at max sub-warp speed about warpTunnelPad before tunnel end, while the
	// server's landing step (and its BoosterStop) runs after the exit buffer. A booster still lit when
	// the ship emerges shows its warp-length trail streaming past the hull, so the pilot's engines go
	// out when the decel starts. Short hops are a sub-warp GotoPoint and keep their engines to landing.
	engineOffAt := dm.WarpDecelStart(celestialPos, tunnelSecs)
	realWarp := celestialPos.Sub(dm.Pos).Len() >= destiny.MinWarpDistM

	// Entering warp drops the gate cloak. It is here and not in WarpToStuff because journal
	// warps (runAgentWarpToLocation) and acceleration gates call startPlayerWarp directly.
	breakGateCloak(ds)

	// Cancel any in-flight warp goroutine before starting a new one.
	// This prevents multiple concurrent warps from interfering with each other.
	if ds.cancelWarp != nil {
		ds.cancelWarp()
	}
	if ds.entity != nil {
		ds.entity.SetWarpPending(false)
	}

	// Modules that need a target, area modules and mining stop here, each with its stop event:
	// the cycle loops send none on a cancel. Modules working on the ship itself keep cycling
	// through the warp (modulestop.go).
	stopModulesForWarp(c, addr, accountID, ds.charID, ds, sqldb)

	// Clear target locks: warping drops all locks.
	ds.targetMu.Lock()
	for tid, cancelFn := range ds.lockingTargets {
		cancelFn()
		delete(ds.lockingTargets, tid)
	}
	ds.targets = nil
	ds.targetMu.Unlock()
	_ = server.SendNotification(c, int64(accountID), "OnTarget", "charid",
		marshal.PyTuple{Items: []marshal.PyRep{marshal.PyString{Value: "clear"}}})

	// Create a new context for this warp so it can be canceled if another warp starts.
	warpCtx, warpCancel := context.WithCancel(context.Background())
	ds.cancelWarp = warpCancel
	ds.warpGen++
	warpGen := ds.warpGen

	// Mark warp pending before the goroutine starts; abortPendingWarp and the stale-ball reconciler
	// key on it. It is not a targeting rule: NPCs and guns keep firing until IsInWarp.
	if ds.entity != nil {
		ds.entity.SetWarpPending(true)
	}

	// Run the warp in a goroutine so the connection handler keeps serving requests. warpExitDist
	// includes the arrival lane offset, so the warp sim and observers' FollowRange match the stop.
	warpExitDist := landing.Sub(celestialPos).Len()
	// destiny.dll owns warp physics (accel, 3 AU/s cruise, decel). The server sleeps through
	// the phases, manages bubble membership and syncs position at the end.
	go func() {
		defer safego.Recover("startPlayerWarp")
		// A manager left in navWarp stays frozen and the reconciler spares its ball, so every exit stops it;
		// only the generation holding the helm does, so a cancelled warp cannot undo its successor.
		defer func() {
			if ds.warpGen != warpGen {
				return
			}
			if dm.CurrentBallMode() == destiny.ModeWarp {
				log.Printf("[%s] warp: goroutine left nav in warp — stopping", addr)
				dm.Stop()
			}
		}()
		warpStartTime := time.Now()
		warpLog := logx.Conn(addr, accountID, ds.charID).With("warpGen", warpGen)
		warpLog.Info("warp START", "alignSecs", alignSecs, "tunnelSecs", tunnelSecs, "totalSecs", totalSecs)

		// Phase 1, alignment: the ship accelerates and turns toward the warp target.
		dm.GotoDirection(celestialPos)
		warpDir := celestialPos.Sub(dm.Pos).Normalize()
		log.Printf("[%s] warp ALIGN: dir=(%.6f,%.6f,%.6f) ship=(%.0f,%.0f,%.0f) celestial=(%.0f,%.0f,%.0f)",
			addr, warpDir.X, warpDir.Y, warpDir.Z, dm.Pos.X, dm.Pos.Y, dm.Pos.Z, celestialPos.X, celestialPos.Y, celestialPos.Z)
		ds.syncStamp()

		// Observers get SetSpeedFraction 1 and a GotoDirection far along the warp line; the ego gets neither
		// (autopilot.py reads it as manual nav). See docs/main-s11-notes.md#warp-engage-ordering.
		gotoTarget := dm.Pos.Add(warpDir.Scale(1e16))
		mateAlignActions := []marshal.PyRep{
			destiny.BuildSimpleAction("SetSpeedFraction",
				marshal.PyInt{Value: uint64(dm.EntityID)},
				marshal.PyReal{Value: 1.0}),
			destiny.BuildSimpleAction("GotoDirection",
				marshal.PyInt{Value: uint64(dm.EntityID)},
				marshal.PyReal{Value: gotoTarget.X}, marshal.PyReal{Value: gotoTarget.Y}, marshal.PyReal{Value: gotoTarget.Z}),
		}
		broadcastActionList(ds, mateAlignActions...)

		// The warp mirror's witnesses. The mirror is armed late in the tunnel, not here (see
		// armWarpMirror).
		var warpMirrorMates []*destiny.Entity
		warpLanded := false
		// A warp that never lands leaves witnesses holding a ball that never arrives, and the
		// reconciler spares mid-warp balls. Withdraw it on every exit that is not a landing.
		defer func() {
			if warpLanded || len(warpMirrorMates) == 0 || ds.entity == nil {
				return
			}
			for _, m := range warpMirrorMates {
				if m.Conn == nil {
					continue
				}
				if len(m.ClaimKnownBalls(ds.entity.ID)) == 0 {
					continue
				}
				mm := m
				_ = mm.SendStamped(func(stamp uint32) error {
					return destiny.SendRemoveBalls(mm.Conn, globalNodeID, mm.AccountID, stamp,
						[]uint32{ds.entity.ID})
				})
			}
			log.Printf("[%s] warp MIRROR: warp did not land — withdrew from %d witness(es)",
				addr, len(warpMirrorMates))
		}()

		// Thruster burst for the departing ship: the ego is denied GotoDirection above, so the
		// client's local booster trigger never fires. Observers get it too; see sendBoosterFX.
		sendBoosterFX(ds, c, uint64(accountID), dogma.FXGUIDBoosterShort)

		// WarpTo goes to the pilot at once so destiny.dll runs the full warp, with exitRange 0 so the client
		// rests on the landing. effectStamp anchors the warp FX. See docs/destiny-space.md#warpto-arguments.
		var effectStamp uint32
		// Logged only when the lane moved this ship, since slot 0 is nearly every arrival at a
		// quiet destination. The pre-lane point is where other pilots warping here were aimed.
		if laneClaim.Moved() {
			log.Printf("[%s] warp LANE: slot=%d overflow=%v rejected=%d res=%d occupied=%d "+
				"prelane=(%.0f,%.0f,%.0f) moved=%.0fm",
				addr, laneClaim.Slot, laneClaim.Overflow, laneClaim.Rejected,
				laneClaim.Reservations, laneClaim.Occupied,
				roomAnchor.X, roomAnchor.Y, roomAnchor.Z, landing.Sub(roomAnchor).Len())
		}
		if err := dm.SendStampedUrgent(func(stamp uint32) error {
			effectStamp = stamp
			return destiny.SendWarpTo(c, globalNodeID, uint64(accountID), stamp,
				dm.EntityID, landing.X, landing.Y, landing.Z, 0, destiny.WarpSpeedAUperSec)
		}); err != nil {
			log.Printf("[%s] warp: SendWarpTo FAILED: %v", addr, err)
			if ds.entity != nil {
				ds.entity.SetWarpPending(false)
			}
			return
		}
		log.Printf("[%s] warp ENGAGE: landing=(%.0f,%.0f,%.0f) celestial=(%.0f,%.0f,%.0f) effectStamp=%d",
			addr, landing.X, landing.Y, landing.Z, celestialPos.X, celestialPos.Y, celestialPos.Z, effectStamp)

		// A cancel during the align ends the wait at once.
		if !tick.Sleep(warpCtx, time.Duration(alignSecs*float64(time.Second))) {
			warpLog.Info("warp CANCELED", "phase", "align")
			// A successor warp owns the pending flag; only an uncontested cancel clears it.
			if ds.entity != nil && ds.warpGen == warpGen {
				ds.entity.SetWarpPending(false)
			}
			return
		}

		// Re-check the scramble when the tunnel would open; the request-time gate misses a point that lands
		// during the align. See docs/main-s11-notes.md#warp-engage-ordering.
		if held, net := warpScrambleHold(ds, sqldb); held {
			warpLog.Info("warp ABORTED: warp scrambled at align end", "net", net)
			dm.Stop()
			if ds.entity != nil {
				ds.entity.SetWarpPending(false)
			}
			ds.syncStamp()
			if err := dm.SendStampedUrgent(func(stamp uint32) error {
				return destiny.SendSimpleAction(c, globalNodeID, uint64(accountID), stamp,
					"Stop", marshal.PyInt{Value: uint64(dm.EntityID)})
			}); err != nil {
				log.Printf("[%s] warp ABORTED: Stop to ego: %v", addr, err)
			}
			broadcastAction(ds, "Stop", marshal.PyInt{Value: uint64(dm.EntityID)})
			sendBoosterFX(ds, c, uint64(accountID), dogma.FXGUIDBoosterStop)
			pushRemoteMessage(c, accountID, "WarpScrambled", nil)
			return
		}

		// Phase 2, warp tunnel. Leave the bubble before dm.WarpTo: with a valid BubbleID the tick
		// loop sees the bubble exit and re-inserts the entity into a transit bubble, which
		// re-engages NPCs and leaves the pilot stuck in the warp animation.
		var departMates []*destiny.Entity
		var departNPCs []*destiny.Entity
		var departPos destiny.Vec3
		if ds.solarSystem != nil && ds.entity != nil {
			departPos = dm.Pos
			departNPCs = ds.solarSystem.GetBubbleNPCs(ds.entity.ID)
			departMates = ds.solarSystem.RemoveFromBubble(ds.entity.ID)
		}
		// Snapshot the departure grid's rigid dungeon balls now: the arrival pre-render may add
		// the destination room's balls to ds.dunObjects before the depart sweep runs.
		var departDunIDs []uint32
		for eid := range ds.dunObjSnapshot() {
			if destiny.IsDunRigidBallID(eid) {
				departDunIDs = append(departDunIDs, eid)
			}
		}
		departRoomID := ds.dunRoomID

		ds.syncStamp()
		dm.WarpTo(celestialPos, effectStamp, uint32(ds.charID), warpExitDist)
		if ds.entity != nil {
			ds.entity.SetWarpPending(false) // IsInWarp() now true
		}
		log.Printf("[%s] warp TUNNEL: bubble removed, server nav set to warp", addr)
		// Entering warp switches the afterburner or MWD off (not at the click: the prop mod pushes the align).
		// Handed to the connection goroutine, which owns the module maps; a stale request is replaced.
		select {
		case ds.warpPropModOff <- warpGen:
		default:
			select {
			case <-ds.warpPropModOff:
			default:
			}
			select {
			case ds.warpPropModOff <- warpGen:
			default:
			}
		}

		// Fighter drones follow their controller through warp (RMR carrier fighters); other
		// drone groups stay behind.
		fighterWarpFollow(ds, celestialPos, landing, warpDir, warpExitDist, tunnelSecs)

		// Clear target locks at warp engage: locks break when the ship enters warp.
		if len(ds.targets) > 0 {
			ds.targets = nil
			clearArgs := marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyString{Value: "clear"},
			}}
			_ = server.SendNotification(c, accountID, "OnTarget", "charid", clearArgs)
			log.Printf("[%s] warp engage: cleared targets", addr)
		}

		// Broadcast the warp to bubble-mates (captured before removal).
		if ds.solarSystem != nil && ds.entity != nil {
			for _, mate := range departMates {
				if mate.Conn == nil || !mate.KnowsBall(dm.EntityID) {
					continue
				}
				// Drain first: the align packets sit in the coalescing queue for up to maxPendingHold, longer than
				// the align sleep, and must stay ahead of the WarpTo.
				mate.FlushQueuedActions(globalNodeID, true)
				_ = mate.SendStamped(func(mateStamp uint32) error {
					return destiny.SendWarpTo(mate.Conn, globalNodeID, mate.AccountID, mateStamp,
						dm.EntityID, landing.X, landing.Y, landing.Z, 0, destiny.WarpSpeedAUperSec)
				})
			}
		}

		// Sleep through the tunnel in two parts around the observers' removal, which only needs the streak-out.
		// See docs/main-s11-notes.md#departure-removal-timing.
		hold := math.Min(tunnelSecs, observerWarpVisibleSecs)
		time.Sleep(time.Duration(hold * float64(time.Second)))

		// Deferred RemoveBalls: the warp-out animation has played on the mates' clients.
		for _, mate := range departMates {
			if mate.Conn == nil || !mate.KnowsBall(ds.entity.ID) {
				continue
			}
			_ = mate.SendStamped(func(mateStamp uint32) error {
				return destiny.SendRemoveBalls(mate.Conn, globalNodeID, mate.AccountID, mateStamp, []uint32{ds.entity.ID})
			})
			mate.ForgetBalls(ds.entity.ID)
		}

		// Bound the wait before the departure grid's common content comes off the pilot's ballpark
		// (selfDepartureHold). Dungeon rigid balls come off later, after the arrival pre-render.
		selfHold := selfDepartureHold(alignSecs, tunnelSecs)
		tunnelElapsed := hold
		sleepUntil := func(t float64) {
			if d := t - tunnelElapsed; d > 0 {
				time.Sleep(time.Duration(d * float64(time.Second)))
				tunnelElapsed = t
			}
		}
		departSweep := func() {
			if ds.solarSystem == nil || ds.entity == nil {
				return
			}
			var earlyRemoveIDs []uint32
			earlyRemove := func(kind string, ids []uint32) {
				ids = ds.entity.ClaimKnownBalls(ids...)
				if len(ids) == 0 {
					return
				}
				earlyRemoveIDs = append(earlyRemoveIDs, ids...)
				log.Printf("[%s] warp depart: removing %d %s from ballpark", addr, len(ids), kind)
			}
			if len(departMates) > 0 {
				mateIDs := make([]uint32, len(departMates))
				for i, mate := range departMates {
					mateIDs[i] = mate.ID
				}
				earlyRemove("bubble mates", mateIDs)
			}
			if astBalls, _, _ := ds.solarSystem.NearbyAsteroids(departPos); len(astBalls) > 0 {
				astIDs := make([]uint32, len(astBalls))
				for i, ab := range astBalls {
					astIDs[i] = ab.EntityID
				}
				earlyRemove("asteroids", astIDs)
			}
			if bbBalls, _, _ := ds.solarSystem.NearbyBillboards(departPos); len(bbBalls) > 0 {
				bbIDs := make([]uint32, len(bbBalls))
				for i, bb := range bbBalls {
					bbIDs[i] = bb.EntityID
				}
				earlyRemove("billboards", bbIDs)
			}
			if len(departNPCs) > 0 {
				npcIDs := make([]uint32, len(departNPCs))
				for i, n := range departNPCs {
					npcIDs[i] = n.ID
				}
				earlyRemove("NPCs", npcIDs)
			}
			if lcBalls, _, _ := ds.solarSystem.NearbyLootContainers(departPos); len(lcBalls) > 0 {
				lcIDs := make([]uint32, len(lcBalls))
				for i, b := range lcBalls {
					lcIDs[i] = b.EntityID
				}
				earlyRemove("loot containers", lcIDs)
			}
			if sbBalls, _ := ds.solarSystem.NearbyGridStarbases(departPos); len(sbBalls) > 0 {
				sbIDs := make([]uint32, len(sbBalls))
				for i, b := range sbBalls {
					sbIDs[i] = b.EntityID
				}
				earlyRemove("POS structures", sbIDs)
			}
			if len(earlyRemoveIDs) > 0 {
				ds.syncStamp()
				_ = dm.SendStampedUrgent(func(stamp uint32) error {
					return destiny.SendRemoveBalls(c, globalNodeID, uint64(accountID), stamp, earlyRemoveIDs)
				})
			}
		}

		// Phase 2b, warp mirror: send the destination grid's witnesses the same WarpTo, WarpMirrorLeadSec
		// before landing. See docs/destiny-arrival-lanes.md#warp-mirror-the-destination-grid.
		mirrorAt := math.Max(0, tunnelSecs-destiny.WarpMirrorLeadSec)
		armWarpMirror := func() {
			if ds.solarSystem == nil || ds.entity == nil {
				return
			}
			mirrorPos, mirrorVel, mirrorOK := destiny.WarpMirrorPoint(
				landing, departPos, destiny.WarpMirrorLeadSec, dm.EffectiveMaxVelocity())
			if mirrorOK {
				mirrorBall := destiny.WarpMirrorBallDesc(dm.BallDesc(), mirrorPos, mirrorVel)
				mirrorBall.CorpID = uint32(ds.corpID)
				mirrorBall.Name = ds.ballName()
				mirrorSlim := ds.entity.Slim
				var mirrorDmg destiny.DamageState
				if ds.entity.HasHP() {
					mirrorDmg = ds.entity.DamageStateFrac()
					mirrorDmg.Timestamp = server.Win32TimeNow()
				} else {
					mirrorDmg = ds.entity.Damage
				}
				mirrorDmgMap := map[uint32]destiny.DamageState{ds.entity.ID: mirrorDmg}
				warpActions := destiny.BuildWarpToActions(dm.EntityID,
					landing.X, landing.Y, landing.Z, 0, destiny.WarpSpeedAUperSec)
				for _, mate := range ds.solarSystem.PlayersAtPoint(landing, ds.entity.ID) {
					if mate.Conn == nil || !mate.HasBallparkState() {
						continue
					}
					if len(mate.ClaimUnknownBalls(ds.entity.ID)) == 0 {
						continue
					}
					m := mate
					// One packet, one stamp: the AddBalls and the WarpTo that arms it must not be split
					// across a tick, or the ball coasts without its warp command.
					if err := m.SendStamped(func(stamp uint32) error {
						actions := make([]marshal.PyRep, 0, 1+len(warpActions))
						actions = append(actions, destiny.BuildAddBallsAction(stamp,
							[]destiny.BallDesc{mirrorBall}, []destiny.SlimItem{mirrorSlim}, mirrorDmgMap))
						actions = append(actions, warpActions...)
						return destiny.SendActionList(m.Conn, globalNodeID, m.AccountID, stamp, actions...)
					}); err != nil {
						// Roll back as ensureBallsKnown does: a ball marked known but never delivered is
						// invisible to the reconciler's add pass.
						m.ForgetBalls(ds.entity.ID)
						log.Printf("[%s] warp MIRROR to entity=%d: %v (rolled back the claim)", addr, m.ID, err)
						continue
					}
					warpMirrorMates = append(warpMirrorMates, m)
				}
				if len(warpMirrorMates) > 0 {
					log.Printf("[%s] warp MIRROR: %d destination witness(es) armed %.0f m out",
						addr, len(warpMirrorMates), landing.Sub(mirrorPos).Len())
				}
			}
		}
		// Engines out for the pilot as the dll's decel begins; bubble-mates at the destination get
		// their BoosterStop with the landing below.
		engineOff := func() {
			sendBoosterFXSelf(ds, c, uint64(accountID), dogma.FXGUIDBoosterStop)
			log.Printf("[%s] warp DECEL: pilot engines off at tunnel+%.1fs (tunnel %.0fs)", addr, engineOffAt, tunnelSecs)
		}
		// Tunnel events in time order; on a tie the depart sweep goes before the mirror.
		tunnelEvents := []struct {
			at float64
			fn func()
		}{
			{selfHold, departSweep},
			{mirrorAt, armWarpMirror},
		}
		if realWarp {
			tunnelEvents = append(tunnelEvents, struct {
				at float64
				fn func()
			}{math.Max(hold, engineOffAt), engineOff})
		}
		sort.SliceStable(tunnelEvents, func(i, j int) bool { return tunnelEvents[i].at < tunnelEvents[j].at })
		for _, ev := range tunnelEvents {
			sleepUntil(ev.at)
			ev.fn()
		}
		// The dll's own exit is about 2 s before this point.
		sleepUntil(tunnelSecs - 2)
		if ds.entity != nil {
			log.Printf("[%s] warp EXIT (modelled dll exit): wall=%d lead=%+d", addr, ds.entity.WallStamp(), ds.entity.StampLead())
		}
		sleepUntil(tunnelSecs)

		// Check if warp was canceled during tunnel phase.
		select {
		case <-warpCtx.Done():
			warpLog.Info("warp CANCELED", "phase", "tunnel")
			return
		default:
		}

		// Phase 3, natural arrival. Join the destination bubble before dm.Stop() so the BallDesc
		// is still in warp mode and observers see the ship decelerate in.
		var destMates []*destiny.Entity
		var bubbleMerged bool
		if ds.solarSystem != nil && ds.entity != nil {
			destMates, bubbleMerged = ds.solarSystem.AddToBubble(ds.entity.ID, landing)
			// On bubble merge, cross-notify all players in the merged bubble.
			if bubbleMerged {
				for _, p := range destMates {
					if !p.IsPlayer || p.Conn == nil || p.Conn.IsClosed() {
						continue
					}
					ensureBallsKnown(p, destMates...)
				}
			}
			if len(destMates) > 0 {
				// Late joiners only: witnesses present at warp start hold the mirror ball. A tunnel-time joiner is
				// shown the ship parked. See docs/destiny-arrival-lanes.md#warp-mirror-the-destination-grid.
				bd := destiny.BallDescForObserver(dm.BallDesc(), nil)
				bd.CorpID = uint32(ds.corpID)
				bd.Name = ds.ballName()
				slim := ds.entity.Slim
				var dmgState destiny.DamageState
				if ds.entity.HasHP() {
					dmgState = ds.entity.DamageStateFrac()
					dmgState.Timestamp = server.Win32TimeNow()
				} else {
					dmgState = ds.entity.Damage
				}
				dmgMap := map[uint32]destiny.DamageState{ds.entity.ID: dmgState}
				for _, mate := range destMates {
					// Claim through the known-ball set: it skips mirrored witnesses and racing broadcasts alike.
					if mate.Conn == nil || !mate.HasBallparkState() {
						continue
					}
					if len(mate.ClaimUnknownBalls(ds.entity.ID)) == 0 {
						continue
					}
					_ = mate.SendStamped(func(mateStamp uint32) error {
						return destiny.SendAddBallsFull(mate.Conn, globalNodeID, mate.AccountID, mateStamp,
							[]destiny.BallDesc{bd}, []destiny.SlimItem{slim}, dmgMap)
					})
				}
				log.Printf("[%s] warp ARRIVE: %d bubble-mates notified", addr, len(destMates))
			}
		}

		// Dungeon pre-render: spawn the arrival room now and deliver its balls during the decel, anchored on
		// the landing (dm.Pos is mid-warp). roomEnter triggers wait for the landing.
		if onArrive != nil {
			ds.dunPendingMode = true
			ds.dunPendingAnchor = &roomAnchor
			ds.dunPendingRoomEnter = false
			onArrive()
			ds.dunPendingAnchor = nil
			ds.dunPendingMode = false
			if len(ds.dunPendingBalls) > 0 {
				preBalls := ds.dunPendingBalls
				preSlims := ds.dunPendingSlims
				preDmg := ds.dunPendingDmg
				ds.dunPendingBalls, ds.dunPendingSlims, ds.dunPendingDmg = nil, nil, nil
				if ds.entity != nil {
					preIDs := make([]uint32, len(preBalls))
					for i, b := range preBalls {
						preIDs[i] = b.EntityID
					}
					ds.entity.ClaimUnknownBalls(preIDs...)
				}
				ds.syncStamp()
				if err := dm.SendStampedUrgent(func(preStamp uint32) error {
					return destiny.SendAddBallsFull(c, globalNodeID, uint64(accountID), preStamp,
						preBalls, preSlims, preDmg)
				}); err != nil {
					log.Printf("[%s] warp: pre-render room AddBalls FAILED: %v", addr, err)
				} else {
					log.Printf("[%s] warp: pre-rendered %d room balls during warp exit", addr, len(preBalls))
				}
			}
		}

		// Phase 3, exit buffer: wait for the dll's own deceleration curve before Stop; an early Stop ends
		// the warp short of the destination.
		inertia := dm.Mass() * dm.Agility() / 1e6
		exitSecs := math.Max(5, 3+inertia*0.3)
		// A cancel during the exit buffer ends the wait at once.
		if !tick.Sleep(warpCtx, time.Duration(exitSecs*float64(time.Second))) {
			warpLog.Info("warp CANCELED", "phase", "exit")
			ds.dunPendingRoomEnter = false // room spawned but landing never happens
			return
		}

		// Remove the departure grid's rigid dungeon balls here, after the pre-render: ds.dunRoomID vs
		// departRoomID is the only test that separates a same-room warp from a room change.
		if ds.solarSystem != nil && ds.entity != nil {
			// IDs come from the engage snapshot, never the live map, which may already hold the
			// pre-rendered room. The other sweeps never see these client-only balls. Server-side room
			// NPCs stay alive so a mission dungeon can be re-entered mid-fight.
			var dunRigidIDs []uint32
			for _, eid := range departDunIDs {
				if ds.dunObjOwns(eid) {
					dunRigidIDs = append(dunRigidIDs, eid)
				}
			}
			if len(dunRigidIDs) > 0 {
				// ClaimKnownBalls: the client logs "RemoveBall trying to remove non-existing ball" for IDs
				// it never held, and a blind ForgetBalls hides that divergence.
				if claimed := ds.entity.ClaimKnownBalls(dunRigidIDs...); len(claimed) > 0 {
					log.Printf("[%s] warp depart: removing %d dungeon balls from ballpark", addr, len(claimed))
					ds.syncStamp()
					_ = dm.SendStampedUrgent(func(stamp uint32) error {
						return destiny.SendRemoveBalls(c, globalNodeID, uint64(accountID), stamp, claimed)
					})
				}
				// Drop them from ds.dunObjects whatever the client held; a stale entry would re-send
				// RemoveBalls on the next room load.
				ds.dunObjMu.Lock()
				for _, eid := range dunRigidIDs {
					delete(ds.dunObjects, eid)
				}
				ds.dunObjMu.Unlock()
				ds.dunGateDir = nil
			}
			// Warping clear of the pocket lifts its phenomena, unless the pre-render loaded a
			// different room (an acceleration gate hop), which set its own flag.
			if ds.dunRoomID == departRoomID {
				ds.dunNoWarp = false
			}
		}

		// Requests under 1e10 m are a sub-warp GotoPoint in the dll (0x10017bb0), so only a short hop needs a
		// position snap; on a real warp it re-issues the landing as a GOTO. See SendWarpLandingBatch.
		warpDist := celestialPos.Sub(departPos).Len()
		shortHop := warpDist < destiny.MinWarpDistM

		ds.syncStamp()
		dm.SetPosition(landing)
		// Landing grace is set before Stop() so IsInWarp() and InLandingGrace() are never both
		// false. The client's decel slide outlasts the server exit buffer.
		if ds.entity != nil {
			// 12 s covers the client-side slide after the server leaves warp; a lock inside that
			// window reads as locked while still in warp.
			ds.entity.SetNoLockUntil(time.Now().Add(12 * time.Second))
		}
		dm.Stop()

		// Batch the arrival balls (and, on a short hop, the position snap) at one stamp so the client applies
		// them atomically; the claim below filters out the pre-rendered room.
		arrBalls, arrSlims, arrDmg := buildWarpArrivalBalls(ds)
		// Claim before sending so concurrent broadcasts see the balls as known. Send only the
		// claimed subset: pre-rendered and merge-notified balls must not be re-added.
		var claimedIDs []uint32
		if ds.entity != nil && len(arrBalls) > 0 {
			ids := make([]uint32, len(arrBalls))
			for i, b := range arrBalls {
				ids[i] = b.EntityID
			}
			claimedIDs = ds.entity.ClaimUnknownBalls(ids...)
			claimedSet := make(map[uint32]bool, len(claimedIDs))
			for _, id := range claimedIDs {
				claimedSet[id] = true
			}
			if len(claimedIDs) < len(arrBalls) {
				fBalls := arrBalls[:0]
				fSlims := arrSlims[:0]
				for i, b := range arrBalls {
					if claimedSet[b.EntityID] {
						fBalls = append(fBalls, b)
						fSlims = append(fSlims, arrSlims[i])
					} else {
						delete(arrDmg, b.EntityID)
					}
				}
				arrBalls = fBalls
				arrSlims = fSlims
			}
		}
		var arrFacings []marshal.PyRep
		if ds.solarSystem != nil {
			arrFacings = ds.solarSystem.StructureFacingActions(arrBalls, arrSlims)
		}
		if err := dm.SendStampedUrgent(func(stamp uint32) error {
			if err := destiny.SendWarpLandingBatch(c, globalNodeID, uint64(accountID), stamp,
				dm.EntityID, landing.X, landing.Y, landing.Z, shortHop,
				arrBalls, arrSlims, arrDmg); err != nil {
				return err
			}
			// Moon harvesters on the landing grid face their moon.
			if len(arrFacings) > 0 {
				return destiny.SendActionList(c, globalNodeID, uint64(accountID), stamp, arrFacings...)
			}
			return nil
		}); err != nil {
			log.Printf("[%s] warp: SendWarpLandingBatch FAILED: %v", addr, err)
			// Roll back on send failure so reconciliation can retry.
			if ds.entity != nil && len(claimedIDs) > 0 {
				ds.entity.ForgetBalls(claimedIDs...)
			}
		}

		// Engines out: the dll's decel curve carries no funcName the booster trigger watches, so
		// the BoosterShort from warp start would decay on its 30 s ramp on a parked ship. On a real
		// warp the pilot's own stop went out at decel start (engineOff); a second one here would cut
		// the engines of a pilot already flying off the landing.
		if realWarp {
			sendBoosterFXGrid(ds, dogma.FXGUIDBoosterStop)
		} else {
			sendBoosterFX(ds, c, uint64(accountID), dogma.FXGUIDBoosterStop)
		}

		// Backstop for the departure sweep, which works from per-category snapshots: anything the
		// client holds that does not belong to the landing grid is removed.
		// Queued, not urgent: these are other balls, and an urgent send here mints a second stamp
		// right behind the landing's. Each removal retires its ball when the flush sends it, and the
		// flush carries them all in one packet. See docs/destiny-stamps.md#budget-and-triage.
		if stale, _ := staleBallparkCandidates(ds); len(stale) > 0 && ds.entity != nil {
			var queued []uint32
			for _, id := range stale {
				if ds.entity.KnowsBall(id) {
					ds.entity.QueueRemoveBall("rm:"+strconv.FormatUint(uint64(id), 10), id)
					queued = append(queued, id)
				}
			}
			if len(queued) > 0 {
				log.Printf("[%s] warp landing: pruning %d stale balls from ballpark: %v", addr, len(queued), queued)
			}
		}

		// Broadcast warp-end to bubble mates so they see the ship
		// exit warp and snap to the landing position.
		if ds.solarSystem != nil && ds.entity != nil {
			for _, mate := range destMates {
				if mate.Conn == nil || !mate.KnowsBall(dm.EntityID) {
					continue
				}
				_ = mate.SendStamped(func(mateStamp uint32) error {
					return destiny.SendWarpEnd(mate.Conn, globalNodeID, mate.AccountID,
						mateStamp, dm.EntityID, landing.X, landing.Y, landing.Z)
				})
			}
		}

		// The warp landed, so the mirror stands: the deferred teardown above must not withdraw it.
		warpLanded = true

		// Deferred roomEnter from the pre-render: fire it with the landing, so transmissions
		// arrive with the ship and not mid-tunnel.
		if ds.dunPendingRoomEnter {
			ds.dunPendingRoomEnter = false
			if !fireDungeonTriggers(c, addr, accountID, ds, "roomEnter", 0) {
				autoReleaseDungeonWave(ds, globalNPCMgr, 0, addr)
			}
		}

		warpLog.Info("warp COMPLETE", "elapsedSecs", time.Since(warpStartTime).Seconds(), "expectedSecs", alignSecs+tunnelSecs+exitSecs)

		// Persist landing position (fire-and-forget).
		go func() {
			defer safego.Recover("startPlayerWarp")
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := evedb.UpdateEntityPosition(ctx, sqldb, ds.shipID, landing.X, landing.Y, landing.Z); err != nil {
				log.Printf("[%s] warp: persist position: %v", addr, err)
			}
		}()

		// Deferred ego position sync, short hops only: the dll flies those as a sub-warp GotoPoint, so the
		// position is re-sent once out of ModeWarp. A real warp lands on the sent XYZ.
		if shortHop {
			go func() {
				defer safego.Recover("startPlayerWarp")
				if !tick.Sleep(warpCtx, 12*time.Second) {
					return // superseded by another warp / canceled
				}
				if ds.entity == nil {
					return
				}
				// Sync to the server's position, not the landing point: the pilot may already be flying.
				pos := dm.Pos
				ds.syncStamp()
				if err := dm.SendStampedUrgent(func(stamp uint32) error {
					return destiny.SendWarpEnd(c, globalNodeID, uint64(accountID), stamp,
						dm.EntityID, pos.X, pos.Y, pos.Z)
				}); err != nil {
					log.Printf("[%s] warp: post-slide position sync: %v", addr, err)
				}
			}()
		}

		// Post-warp reconciliation: after a short delay, deliver NPC balls in the bubble that are
		// still unknown to this player (silent client AddBalls rejections).
		if ds.solarSystem != nil && ds.entity != nil {
			sweepSS := ds.solarSystem
			sweepEntity := ds.entity
			go func() {
				defer safego.Recover("startPlayerWarp")
				time.Sleep(3 * time.Second)
				npcs := sweepSS.GetBubbleNPCs(sweepEntity.ID)
				var missing []*destiny.Entity
				for _, n := range npcs {
					if !sweepEntity.KnowsBall(n.ID) {
						missing = append(missing, n)
					}
				}
				if len(missing) > 0 {
					log.Printf("[%s] post-warp sweep: delivering %d missing NPC balls", addr, len(missing))
					ensureBallsKnown(sweepEntity, missing...)
				}
			}()
		}
	}()
}
