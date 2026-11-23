package main

import (
	"context"
	"log"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/dogma"
	"gurimoon/marshal"
	_ "gurimoon/modules"
	"gurimoon/safego"
	"gurimoon/server"
	"gurimoon/server/lsc"
)

// handleDestinyBoundCall handles navigation bound calls (GotoDirection, Stop, etc.).
func handleDestinyBoundCall(cc *callCtx) error {
	c, addr, cs, pkt, accountID, sqldb, ds, sessReg, npcMgr := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.sqldb, cc.ds, cc.sessReg, cc.npcMgr
	if ds.dm == nil {
		log.Printf("[%s] → bound.%s (no destiny manager — returning None)", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	dm := ds.dm

	// Ship control is locked for the duration of the warp tunnel. The client keeps
	// sending whatever the pilot clicks, so the refusal is made here (see warpNavLocked).
	switch cs.Method {
	case "Stop", "GotoDirection", "GotoPoint", "GotoBookmark", "FollowBall", "Orbit", "SetSpeedFraction", "WarpToStuff", "StargateJump":
		if warpNavLocked(ds) {
			log.Printf("[%s] → destiny.%s ignored — ship is in warp", addr, cs.Method)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
	}

	// A ship running a cynosural field or holding a jump portal open (siegeModeWarpStatus
	// 852) cannot move. Stop stays allowed: the client sends it after every ballpark load.
	switch cs.Method {
	case "GotoDirection", "GotoPoint", "GotoBookmark", "FollowBall", "Orbit", "SetSpeedFraction", "WarpToStuff", "StargateJump":
		if pinned, mod := shipImmobilised(ds); pinned {
			log.Printf("[%s] → destiny.%s refused — pinned by %s", addr, cs.Method, mod)
			return server.SendCallException(c, pkt, accountID, immobilisedError(mod, "do that"))
		}
	}

	switch cs.Method {
	case "Stop":
		// Stop does not break the gate cloak (the client sends Stop after every ballpark
		// load). StopCoast decelerates the server ship on the client's inertia curve.
		dm.StopCoast()
		// Cancel any in-flight warp so alignment doesn't continue in the background.
		if ds.cancelWarp != nil {
			ds.cancelWarp()
			ds.cancelWarp = nil
		}
		if ds.entity != nil {
			ds.entity.SetWarpPending(false)
		}
		stopQueued := egoEchoQueued(ds, dm)
		if !stopQueued {
			ds.syncStamp()
		}
		log.Printf("[%s] → destiny.Stop entityID=%d%s", addr, dm.EntityID, queuedNote(stopQueued))
		if err := server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}); err != nil {
			return err
		}
		if stopQueued {
			ds.entity.QueueEgoStop()
		} else if err := dm.SendStampedUrgent(func(stamp uint32) error {
			return destiny.SendSimpleAction(c, globalNodeID, uint64(accountID), stamp,
				"Stop", marshal.PyInt{Value: uint64(dm.EntityID)})
		}); err != nil {
			return err
		}
		broadcastAction(ds, "Stop", marshal.PyInt{Value: uint64(dm.EntityID)})
		// Stop is not in michelle.scatterEvents, so the client raises no booster event for it.
		// The post-jump auto-Stop also lifts the cloak's -2.0 clamp (see breakGateCloak).
		sendBoosterFX(ds, c, uint64(accountID), dogma.FXGUIDBoosterStop)
		return nil

	case "GotoDirection":
		breakGateCloak(ds)
		abortPendingWarp(ds)
		// Client args: (x, y, z), a unit direction vector, converted to a far point along it from the
		// ship because the server sim and destiny.dll read GotoDirection params as a goto target.
		x, y, z := destinyExtractXYZ(cs.ArgTuple.Items, 0)
		dir := destiny.Vec3{X: x, Y: y, Z: z}
		if dir.Len() < 1e-9 {
			log.Printf("[%s] → destiny.GotoDirection entityID=%d: degenerate direction, ignored", addr, dm.EntityID)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		far := dm.Pos.Add(dir.Normalize().Scale(1e15))
		dm.GotoDirection(far)
		// No syncStamp on the queued path: it takes the wall second's stamp without sending and
		// holds the flush gate shut. See docs/destiny-stamps.md#nav-click-burst.
		egoQueued := ds.entity != nil && ds.entity.Manager == dm
		if !egoQueued {
			ds.syncStamp()
		}
		log.Printf("[%s] → destiny.GotoDirection entityID=%d dir=(%.3f,%.3f,%.3f) target=(%.0f,%.0f,%.0f)",
			addr, dm.EntityID, x, y, z, far.X, far.Y, far.Z)
		if err := server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}); err != nil {
			return err
		}
		// SetSpeedFraction goes in the same packet because the client holds speed 0 from the
		// previous Stop. See docs/destiny-stamps.md#nav-click-burst.
		gotoActions := []marshal.PyRep{
			destiny.BuildSimpleAction("SetSpeedFraction",
				marshal.PyInt{Value: uint64(dm.EntityID)},
				marshal.PyReal{Value: dm.SpeedFraction()}),
			destiny.BuildSimpleAction("GotoDirection",
				marshal.PyInt{Value: uint64(dm.EntityID)},
				marshal.PyReal{Value: far.X}, marshal.PyReal{Value: far.Y}, marshal.PyReal{Value: far.Z}),
		}
		// The echo rides the queue, latest click wins, and leaves at once when the wall second's
		// stamp is free: a drag-steer repeats the call several times a second.
		if egoQueued {
			ds.entity.QueueEgoGoto(far)
			ds.entity.FlushQueuedActions(globalNodeID, false)
		} else {
			if err := dm.SendStampedUrgent(func(stamp uint32) error {
				return destiny.SendActionList(c, globalNodeID, uint64(accountID), stamp, gotoActions...)
			}); err != nil {
				return err
			}
			ds.noteEgoSpeedEcho(dm.SpeedFraction())
		}
		broadcastActionList(ds, gotoActions...)
		return nil

	case "GotoBookmark":
		return handleGotoBookmark(c, addr, cs, pkt, accountID, sqldb, ds)

	case "ConeScan":
		return handleConeScan(c, addr, cs, pkt, accountID, sqldb, ds)

	// System Scanner (scanprobes.go)
	case "GetScans":
		return handleGetScans(c, addr, pkt, accountID, ds.charID, ds)
	case "CombineScans":
		return handleCombineScans(c, addr, cs, pkt, accountID, ds.charID, sqldb, ds)
	case "GiveProbe":
		return handleGiveProbe(c, addr, cs, pkt, accountID, ds.charID, ds)

	case "SetSpeedFraction":
		// Client args: (fraction)
		var f float64
		if len(cs.ArgTuple.Items) >= 1 {
			f = destinyExtractFloat(cs.ArgTuple.Items[0])
		}
		dm.SetSpeedFraction(f)
		speedQueued := egoEchoQueued(ds, dm)
		if !speedQueued {
			ds.syncStamp()
		}
		redundant := ds.egoSpeedEchoRedundant(f)
		repeatNote := ""
		if redundant {
			repeatNote = " (repeat, self echo skipped)"
		}
		log.Printf("[%s] → destiny.SetSpeedFraction entityID=%d f=%.2f%s%s", addr, dm.EntityID, f, repeatNote, queuedNote(speedQueued && !redundant))
		if err := server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}); err != nil {
			return err
		}
		if speedQueued && !redundant {
			ds.entity.QueueEgoSpeed()
		} else if !redundant {
			if err := dm.SendStampedUrgent(func(stamp uint32) error {
				return destiny.SendSimpleAction(c, globalNodeID, uint64(accountID), stamp,
					"SetSpeedFraction",
					marshal.PyInt{Value: uint64(dm.EntityID)},
					marshal.PyReal{Value: f})
			}); err != nil {
				return err
			}
			ds.noteEgoSpeedEcho(f)
		}
		broadcastAction(ds, "SetSpeedFraction",
			marshal.PyInt{Value: uint64(dm.EntityID)},
			marshal.PyReal{Value: f})
		return nil

	case "GetRelativity":
		// michelle.GetRelativity is the one call where the client reports its own ball
		// position and sim clock, so it measures server-vs-client drift. The result is logged
		// next to the stamp audit; the client only prints it to its dev console.
		//
		// Wire contract:
		//   call    (x, y, z, sTime, tTime, rTime)       the client's ball, stamp and clocks
		//   return  (diff, x, y, z, sTime, tTime, rTime)  the server's
		// sTime is a destiny stamp (lpark.currentTime). tTime and rTime are blue times in
		// 100-ns ticks (util.FmtSec divides their differences by 1e7).
		cliX, cliY, cliZ := destinyExtractXYZ(cs.ArgTuple.Items, 0)
		var cliStamp uint32
		if len(cs.ArgTuple.Items) >= 4 {
			if v, ok := cs.ArgTuple.Items[3].(marshal.PyInt); ok {
				cliStamp = uint32(v.Value)
			}
		}
		var cliRealTime uint64
		if len(cs.ArgTuple.Items) >= 6 {
			if v, ok := cs.ArgTuple.Items[5].(marshal.PyInt); ok {
				cliRealTime = v.Value
			}
		}

		srvPos := dm.Pos
		drift := destiny.Vec3{X: cliX, Y: cliY, Z: cliZ}.Sub(srvPos).Len()
		srvStamp := dm.Stamp
		now := server.Win32TimeNow()

		// Client to server leg only; the client measures the return leg itself.
		var upLegMs int64
		if cliRealTime != 0 && now > cliRealTime {
			upLegMs = int64((now - cliRealTime) / 10000)
		}
		lead := 0
		if ds.entity != nil {
			lead = ds.entity.StampLead()
		}
		log.Printf("[%s] GetRelativity: drift=%.1fm client=(%.0f,%.0f,%.0f) server=(%.0f,%.0f,%.0f) "+
			"stamp client=%d server=%d (delta=%d, lead=%d) uplink=%dms moving=%v",
			addr, drift, cliX, cliY, cliZ, srvPos.X, srvPos.Y, srvPos.Z,
			cliStamp, srvStamp, int64(srvStamp)-int64(cliStamp), lead, upLegMs, dm.IsMoving())

		return server.SendCallRsp(c, pkt, accountID, marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyReal{Value: drift},
			marshal.PyReal{Value: srvPos.X},
			marshal.PyReal{Value: srvPos.Y},
			marshal.PyReal{Value: srvPos.Z},
			marshal.PyInt{Value: uint64(srvStamp)},
			marshal.PyInt{Value: now},
			marshal.PyInt{Value: now},
		}})

	case "UpdateStateRequest":
		// The client sends this (michelle.RequestReset) after a stamp it could not match, and
		// ignores destiny updates until a fresh SetState arrives.
		ds.syncStamp()
		log.Printf("[%s] → destiny.UpdateStateRequest — resending full SetState", addr)
		if ds.entity != nil {
			// Recent stamp mints: a stamp reused across more than 1 s, or a jump of 3 or more
			// within one second, points at what desynced the client.
			log.Printf("[%s] stamp audit: %s", addr, ds.entity.StampAudit())
		}
		if err := server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}); err != nil {
			return err
		}
		resyncBallpark(c, addr, uint64(accountID), ds)
		return nil

	case "FollowBall":
		breakGateCloak(ds)
		abortPendingWarp(ds)
		// Client args: (targetID, distance)
		var targetID uint32
		var dist float64
		if len(cs.ArgTuple.Items) >= 1 {
			if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
				targetID = uint32(v.Value)
			}
		}
		if len(cs.ArgTuple.Items) >= 2 {
			dist = destinyExtractFloat(cs.ArgTuple.Items[1])
		}
		dm.Follow(targetID, navCentreRange(ds, targetID, dist))
		dm.SetNavWireRange(dist)
		followQueued := egoEchoQueued(ds, dm)
		if !followQueued {
			ds.syncStamp()
		}
		log.Printf("[%s] → destiny.Follow entityID=%d → targetID=%d dist=%.0f%s",
			addr, dm.EntityID, targetID, dist, queuedNote(followQueued))
		if err := server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}); err != nil {
			return err
		}
		// One packet, one stamp; see GotoDirection.
		followActions := []marshal.PyRep{
			destiny.BuildSimpleAction("SetSpeedFraction",
				marshal.PyInt{Value: uint64(dm.EntityID)},
				marshal.PyReal{Value: dm.SpeedFraction()}),
			destiny.BuildSimpleAction("FollowBall",
				marshal.PyInt{Value: uint64(dm.EntityID)},
				marshal.PyInt{Value: uint64(targetID)},
				marshal.PyReal{Value: dist}),
		}
		if followQueued {
			ds.entity.QueueEgoFollow(targetID, dist)
		} else {
			if err := dm.SendStampedUrgent(func(stamp uint32) error {
				return destiny.SendActionList(c, globalNodeID, uint64(accountID), stamp, followActions...)
			}); err != nil {
				return err
			}
			ds.noteEgoSpeedEcho(dm.SpeedFraction())
		}
		broadcastActionList(ds, followActions...)
		return nil

	case "Orbit":
		breakGateCloak(ds)
		abortPendingWarp(ds)
		// Client args: (targetID, distance)
		var targetID uint32
		var dist float64
		if len(cs.ArgTuple.Items) >= 1 {
			if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
				targetID = uint32(v.Value)
			}
		}
		if len(cs.ArgTuple.Items) >= 2 {
			dist = destinyExtractFloat(cs.ArgTuple.Items[1])
		}
		dm.Orbit(targetID, navCentreRange(ds, targetID, dist))
		dm.SetNavWireRange(dist)
		orbitQueued := egoEchoQueued(ds, dm)
		if !orbitQueued {
			ds.syncStamp()
		}
		log.Printf("[%s] → destiny.Orbit entityID=%d → targetID=%d dist=%.0f%s",
			addr, dm.EntityID, targetID, dist, queuedNote(orbitQueued))
		if err := server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}); err != nil {
			return err
		}
		// One packet, one stamp; see GotoDirection.
		orbitActions := []marshal.PyRep{
			destiny.BuildSimpleAction("SetSpeedFraction",
				marshal.PyInt{Value: uint64(dm.EntityID)},
				marshal.PyReal{Value: dm.SpeedFraction()}),
			destiny.BuildSimpleAction("Orbit",
				marshal.PyInt{Value: uint64(dm.EntityID)},
				marshal.PyInt{Value: uint64(targetID)},
				marshal.PyReal{Value: dist}),
		}
		if orbitQueued {
			ds.entity.QueueEgoOrbit(targetID, dist)
		} else {
			if err := dm.SendStampedUrgent(func(stamp uint32) error {
				return destiny.SendActionList(c, globalNodeID, uint64(accountID), stamp, orbitActions...)
			}); err != nil {
				return err
			}
			ds.noteEgoSpeedEcho(dm.SpeedFraction())
		}
		broadcastActionList(ds, orbitActions...)
		return nil

	case "WarpToStuff":
		breakGateCloak(ds)
		// Client sends WarpToStuff(type, id, minRange=X, gang=Y); type is "item",
		// "bookmark", "scan" or "char". Type is positional arg 0, the target ID arg 1.
		var warpType string
		var targetID int64
		var minRange float64
		// minRangeSet is false when the call carries no numeric minRange (autopilot, WarpDock).
		minRangeSet := false
		gangWarp := false
		if len(cs.ArgTuple.Items) >= 1 {
			if s, ok := cs.ArgTuple.Items[0].(marshal.PyString); ok {
				warpType = s.Value
			}
		}
		if len(cs.ArgTuple.Items) >= 2 {
			if v, ok := cs.ArgTuple.Items[1].(marshal.PyInt); ok {
				targetID = int64(v.Value)
			}
		}
		// Parse keyword args: minRange and item (fallback for targetID).
		if cs.ArgDict != nil {
			for _, e := range cs.ArgDict.Items {
				if k, ok := e.Key.(marshal.PyString); ok {
					switch k.Value {
					case "item":
						if targetID == 0 {
							if v, ok := e.Value.(marshal.PyInt); ok {
								targetID = int64(v.Value)
							}
						}
					case "minRange":
						switch e.Value.(type) {
						case marshal.PyReal, marshal.PyInt:
							minRange = destinyExtractFloat(e.Value)
							minRangeSet = true
						}
					case "gang":
						gangWarp = gangWarpKwarg(e.Value)
					}
				}
			}
		}
		if warpType == "" {
			warpType = "item" // default when type string missing
		}
		if targetID == 0 {
			log.Printf("[%s] → destiny.WarpToStuff: missing target ID", addr)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		// A destroyed ship cannot warp.
		if ds.entity != nil && ds.entity.IsDestroyed() {
			log.Printf("[%s] → destiny.WarpToStuff: BLOCKED — ship destroyed", addr)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		// Only the covert ops cloak warps.
		if cloakBlocksWarp(ds) {
			log.Printf("[%s] → destiny.WarpToStuff: BLOCKED — cloaked", addr)
			return clientMsgErr(c, pkt, accountID, "CantWarpWhileCloaked", nil)
		}
		// An active scrambler on this ship blocks warp unless its own warp core
		// stabilizers out-point it.
		if held, net := warpScrambleHold(ds, sqldb); held {
			log.Printf("[%s] → destiny.WarpToStuff: BLOCKED — warp scrambled (net %.0f)", addr, net)
			return server.SendCallException(c, pkt, accountID, warpScrambledError())
		}
		var destX, destY, destZ float64
		var targetRadius float64
		found := false
		// Non-nil for stars, planets and moons: their static warp-in point replaces the
		// approach-vector landing in startPlayerWarp.
		var warpInOverride *destiny.Vec3
		// Set when the destination is a deadspace site anchor, so the landing hugs the beacon.
		deadspaceLanding := false
		// Set for a station: a zero-range warp to one is a warp-to-dock and lands off the door.
		targetIsStation := false

		// resolveCelestial fills the destination from ds.celestials and, for bodies with a
		// static warp-in point, computes it extended outward by the requested warp-to range.
		resolveCelestial := func(id int64) bool {
			for i := range ds.celestials {
				cel := &ds.celestials[i]
				if cel.ItemID != id {
					continue
				}
				destX, destY, destZ = cel.X, cel.Y, cel.Z
				targetRadius = cel.Radius
				targetIsStation = cel.GroupID == 15
				if pt, outward, ok := celestialWarpInPoint(ds.celestials, cel); ok {
					p := pt.Add(outward.Scale(minRange))
					// A static warp-in point lies on the body's far side from its orbit parent; it is used only when
					// the line to it clears the body. See docs/main-s11-notes.md#static-warp-in-line.
					cpos := destiny.Vec3{X: cel.X, Y: cel.Y, Z: cel.Z}
					if warpPathBlocked(dm.Pos, p, cpos, cel.Radius) {
						// Same altitude on the near side, not the ordinary radius + range landing.
						log.Printf("[%s] WarpToStuff: static warp-in for %d crosses the body — using approach-side landing", addr, cel.ItemID)
						near := warpLandingPoint(dm.Pos, cpos, p.Sub(cpos).Len())
						warpInOverride = &near
					} else {
						warpInOverride = &p
					}
				}
				return true
			}
			return false
		}

		var onWarpArrive func()
		switch warpType {
		case "scan":
			// A System Scanner result: the point the last analysis reported, deviation included.
			// A cosmic signature lands on its site marker and beaconWarpArrival loads the site.
			if p, ok, refusal := warpTargetForScan(ds.charID, ds.sysID, targetID); ok {
				destX, destY, destZ = p.X, p.Y, p.Z
				targetRadius = 0
				found = true
			} else if refusal != "" {
				return server.SendCallException(c, pkt, accountID, customNotify(refusal))
			} else {
				log.Printf("[%s] → destiny.WarpToStuff scan: result %d unknown for char %d", addr, targetID, ds.charID)
				return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
			}
		case "bookmark":
			// IDs at or above BookmarkIDFloor are player bookmarks; lower IDs are the
			// mission-derived rows GetBookmarks adds.
			if targetID >= evedb.BookmarkIDFloor {
				bm, err := evedb.GetBookmark(context.Background(), sqldb, targetID)
				if err != nil {
					log.Printf("[%s] → destiny.WarpToStuff bookmark: DB error: %v", addr, err)
					return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
				}
				if bm == nil || bm.OwnerID != ds.charID {
					log.Printf("[%s] → destiny.WarpToStuff bookmark: %d not owned by char %d", addr, targetID, ds.charID)
					return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
				}
				if bm.LocationID != 0 && bm.LocationID != ds.sysID {
					log.Printf("[%s] → destiny.WarpToStuff bookmark: %d is in system %d, player in %d", addr, targetID, bm.LocationID, ds.sysID)
					return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
				}
				if bm.ItemID != 0 {
					// Object bookmark: warp to its celestial. A spot-in-space bookmark stores the solar
					// system as itemID, so no celestial resolves and the stored x/y/z are used.
					found = resolveCelestial(bm.ItemID)
					if !found && (bm.X != 0 || bm.Y != 0 || bm.Z != 0) {
						destX, destY, destZ = bm.X, bm.Y, bm.Z
						targetRadius = 0
						found = true
					}
					if !found {
						log.Printf("[%s] → destiny.WarpToStuff bookmark: object %d not in celestials and no coords", addr, bm.ItemID)
						return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
					}
				} else {
					destX, destY, destZ = bm.X, bm.Y, bm.Z
					targetRadius = 0
					found = true
				}
				break
			}

			// Mission bookmark: the coordinates come from the mission.
			m, err := evedb.GetMissionByID(context.Background(), sqldb, targetID)
			if err != nil {
				log.Printf("[%s] → destiny.WarpToStuff bookmark: DB error: %v", addr, err)
				return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
			}
			if m == nil {
				log.Printf("[%s] → destiny.WarpToStuff bookmark: mission %d not found", addr, targetID)
				return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
			}
			if m.CharID != ds.charID {
				log.Printf("[%s] → destiny.WarpToStuff bookmark: mission %d belongs to char %d, not %d", addr, targetID, m.CharID, ds.charID)
				return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
			}
			if m.BookmarkX != 0 || m.BookmarkY != 0 || m.BookmarkZ != 0 {
				// Coordinate bookmark; must be in the same system.
				if m.DestSystemID != 0 && m.DestSystemID != ds.sysID {
					log.Printf("[%s] → destiny.WarpToStuff bookmark: mission %d in system %d, player in %d", addr, targetID, m.DestSystemID, ds.sysID)
					return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
				}
				destX, destY, destZ = m.BookmarkX, m.BookmarkY, m.BookmarkZ
				targetRadius = 0
				found = true
				// Land on the site beacon, not at the client's generic warp-to range (see
				// deadspaceLandingBuffer).
				deadspaceLanding = true
				// Warping to a mission deadspace bookmark spawns the dungeon on arrival, as the
				// journal's "Warp to Location" does.
				bmCtx, bmCancel := context.WithTimeout(context.Background(), 5*time.Second)
				onWarpArrive = missionDungeonOnArrive(bmCtx, c, addr, accountID, sqldb, ds, sessReg, npcMgr, m)
				bmCancel()
			} else if m.DestStationID != 0 {
				// Station bookmark: resolve as a celestial.
				found = resolveCelestial(m.DestStationID)
				if !found {
					log.Printf("[%s] → destiny.WarpToStuff bookmark: station %d not in celestials", addr, m.DestStationID)
					return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
				}
			} else {
				log.Printf("[%s] → destiny.WarpToStuff bookmark: mission %d has no coordinates or station", addr, targetID)
				return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
			}

		case "char":
			// Warp to another player in the same system.
			if ds.solarSystem == nil {
				log.Printf("[%s] → destiny.WarpToStuff char: no solar system", addr)
				return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
			}
			te := ds.solarSystem.GetPlayerByCharID(targetID)
			if te == nil {
				log.Printf("[%s] → destiny.WarpToStuff char: charID %d not found in system", addr, targetID)
				return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
			}
			// The client offers this for gang members only. A cloaked or GM-hidden member
			// gives no position away.
			if !mayWarpToMember(ds.charID, targetID, te) {
				log.Printf("[%s] → destiny.WarpToStuff char: REFUSED — %d is not a warpable gang member of %d", addr, targetID, ds.charID)
				return clientMsgErr(c, pkt, accountID, "CantWarpAfterThat", nil)
			}
			pos := te.Pos()
			destX, destY, destZ = pos.X, pos.Y, pos.Z
			targetRadius = te.Manager.Radius()
			found = true

		default:
			// Items resolve from celestials, then starbases.
			found = resolveCelestial(targetID)
			// Starbases are player-anchored entities, not mapDenormalize rows, so the SolarSystem's
			// starbase ball list is the second source.
			if !found && ds.solarSystem != nil {
				for i := range ds.solarSystem.StarbaseBalls {
					if int64(ds.solarSystem.StarbaseBalls[i].EntityID) == targetID {
						bd := &ds.solarSystem.StarbaseBalls[i]
						destX, destY, destZ = bd.Pos.X, bd.Pos.Y, bd.Pos.Z
						targetRadius = bd.Radius
						found = true
						break
					}
				}
			}
			// Anything else with a ball in this system: a ship, an NPC, a drone, an asteroid, a
			// can or a corpse. The client offers Warp To on each once it is out of approach range.
			if !found {
				if pos, radius, ok := warpableBall(ds, uint32(targetID)); ok {
					destX, destY, destZ = pos.X, pos.Y, pos.Z
					targetRadius = radius
					found = true
				}
			}
		}

		// Deadspace natural phenomena, checked before the not-found bail so a target in the pocket gets
		// the phenomena message; client-only rigid dungeon balls never resolve to a position.
		if inDeadspacePocket(ds) {
			blocked := ds.dunObjOwns(uint32(targetID))
			if !blocked && found {
				blocked = dungeonWarpBlocked(ds, destiny.Vec3{X: destX, Y: destY, Z: destZ})
			}
			if blocked {
				log.Printf("[%s] → destiny.WarpToStuff: BLOCKED — deadspace natural phenomena (room %d, target %d)",
					addr, ds.dunRoomID, targetID)
				return server.SendCallException(c, pkt, accountID,
					server.BuildUserError("WarpingWithinGlobalDisruptors", nil))
			}
		}

		if !found {
			log.Printf("[%s] → destiny.WarpToStuff: unknown %s %d", addr, warpType, targetID)
			return clientMsgErr(c, pkt, accountID, "WarpDestinationGone", nil)
		}
		// The client offers a warp to a ball only beyond minWarpDistance and approaches anything
		// nearer. Bookmark and scan warps are left alone: a mission site loads on such an arrival.
		if (warpType == "item" || warpType == "char") && tooCloseToWarp(dm.Pos, destiny.Vec3{X: destX, Y: destY, Z: destZ}) {
			log.Printf("[%s] → destiny.WarpToStuff: REFUSED — %s %d is inside the minimum warp distance", addr, warpType, targetID)
			return clientMsgErr(c, pkt, accountID, "TooShortWarpDistance", map[string]marshal.PyRep{
				"mindist": marshal.PyInt{Value: minWarpDistanceM / 1000},
			})
		}

		// Dungeon entrance beacons and open sites load their room on arrival.
		if f, site := beaconWarpArrival(c, addr, accountID, sqldb, ds, sessReg, npcMgr, warpType, targetID,
			destiny.Vec3{X: destX, Y: destY, Z: destZ}); site {
			onWarpArrive = f
			deadspaceLanding = true
		}
		// The landing is offset from the celestial toward the ship by radius + minRange.
		// minRange comes from the client (15 km default warp-to distance); WarpDock sends 0.
		landingBuffer := minRange
		// An item warp with no range (the autopilot's warp to a gate) lands at the default warp-to
		// range, not at jump range. See docs/destiny-arrival-lanes.md#autopilot-warp-range.
		if !minRangeSet && warpType == "item" && !targetIsStation && !deadspaceLanding {
			landingBuffer = defaultWarpToRange
		}
		// Warp-to-dock sends no range and lands docking.warpInM off the station, as does a manual
		// "warp to 0 m". See docs/destiny-arrival-lanes.md#docking-clearance.
		if landingBuffer == 0 && targetIsStation && !deadspaceLanding {
			landingBuffer = destiny.DockClearanceSettings().WarpInM
		}
		// A dungeon beacon or deadspace bookmark honours the range the pilot asked for; only a warp
		// with no range (or zero) falls back to the fixed deadspace buffer. Acceleration gates are the
		// exception and never reach here: their landing is exact (ActivateAccelerationGate).
		if deadspaceLanding {
			landingBuffer = siteLandingBuffer(minRange, minRangeSet)
		} else if landingBuffer == 0 {
			landingBuffer = deadspaceLandingBuffer
		}

		// Warp phases (align, tunnel) are computed from the distance to the celestial itself;
		// startPlayerWarp derives the landing point.
		celestialPos := destiny.Vec3{X: destX, Y: destY, Z: destZ}

		distAU := celestialPos.Sub(dm.Pos).Len() / 149_597_870_700.0

		alignSecs, tunnelSecs, totalSecs := dm.WarpPhases(celestialPos)

		log.Printf("[%s] WarpToStuff: type=%s id=%d dist=%.3f AU align=%.1fs tunnel=%.1fs total=%.1fs mass=%.0f agility=%.3f maxVel=%.0f",
			addr, warpType, targetID, distAU, alignSecs, tunnelSecs, totalSecs, dm.Mass(), dm.Agility(), dm.MaxVelocity())
		if err := server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}); err != nil {
			return err
		}

		// Gang warp: fan the resolved destination out before the leader's own warp moves them,
		// so members are measured against where the order was given (gangwarp.go).
		if gangWarp {
			dispatchGangWarp(addr, ds, gangWarpReq{
				leaderCharID:  ds.charID,
				sysID:         ds.sysID,
				warpType:      warpType,
				targetID:      targetID,
				celestial:     celestialPos,
				targetRadius:  targetRadius,
				landingBuffer: landingBuffer,
				landing:       warpInOverride,
			})
		}

		startPlayerWarp(c, addr, accountID, sqldb, ds, dm, celestialPos, targetRadius, landingBuffer, warpInOverride, warpPaysCap, onWarpArrive)
		return nil

	case "StargateJump":
		cancelPendingDock(ds)
		// Client sends: StargateJump(fromGateID, toGateID)
		if len(cs.ArgTuple.Items) < 2 {
			log.Printf("[%s] → destiny.StargateJump: missing args", addr)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		fromGateArg, ok1 := cs.ArgTuple.Items[0].(marshal.PyInt)
		toGateArg, ok2 := cs.ArgTuple.Items[1].(marshal.PyInt)
		if !ok1 || !ok2 {
			log.Printf("[%s] → destiny.StargateJump: bad arg types", addr)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		toGateID := int64(toGateArg.Value)
		// Crimewatch 1.0: a PvP act locks the aggressor out of stargates for a minute
		// (pvptimers.go). A warp scrambler does not stop a gate jump.
		if aggressionLocked(ds.charID) {
			log.Printf("[%s] → destiny.StargateJump: BLOCKED — recent aggression", addr)
			return clientMsgErr(c, pkt, accountID, "CantJumpAfterAggression", nil)
		}
		if moduleCloakOn(ds) {
			log.Printf("[%s] → destiny.StargateJump: BLOCKED — cloaked", addr)
			return clientMsgErr(c, pkt, accountID, "CantJumpWhileCloaked", nil)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		// The client names both gates; mapJumps must pair them, or a crafted call could
		// land on any gate in the universe.
		paired, pairErr := evedb.StargatesPaired(ctx, sqldb, int64(fromGateArg.Value), toGateID)
		if pairErr != nil || !paired {
			cancel()
			log.Printf("[%s] → destiny.StargateJump: gate %d does not lead to %d (err=%v)", addr, fromGateArg.Value, toGateID, pairErr)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		destX, destY, destZ, destSysID, destConstID, destRegID, lookupErr := evedb.GetStargateDestination(ctx, sqldb, toGateID)
		cancel()
		if lookupErr != nil {
			log.Printf("[%s] → destiny.StargateJump: lookup toGate %d: %v", addr, toGateID, lookupErr)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		log.Printf("[%s] → destiny.StargateJump gate=%d→%d destSys=%d", addr, fromGateArg.Value, toGateID, destSysID)
		// Cancel the warp context unconditionally: the warp goroutine writes into state the jump replaces.
		// See docs/main-s11-notes.md#gate-cancels-warp-context.
		abortPendingWarp(ds)
		if ds.cancelWarp != nil {
			ds.cancelWarp()
			ds.cancelWarp = nil
		}
		if err := server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}); err != nil {
			return err
		}
		// Phase 1 (~200ms): play JumpOut flash on the source gate.
		time.Sleep(200 * time.Millisecond)
		ds.syncStamp()
		if err := dm.SendStampedUrgent(func(stamp uint32) error {
			return destiny.SendJumpOut(c, globalNodeID, uint64(accountID), stamp,
				dm.EntityID, uint32(fromGateArg.Value))
		}); err != nil {
			return err
		}
		// Same flash for everyone left on the source gate: JumpOut animates the target ball with
		// the ship as the effect source (jump.py), and mates still hold both balls.
		broadcastFXAction(ds, "OnSpecialFX",
			marshal.PyInt{Value: uint64(dm.EntityID)},
			marshal.PyNone{}, marshal.PyNone{},
			marshal.PyInt{Value: fromGateArg.Value},
			marshal.PyNone{},
			marshal.PyList{},
			marshal.PyString{Value: "effects.JumpOut"},
			marshal.PyInt{Value: 0}, marshal.PyInt{Value: 1}, marshal.PyInt{Value: 0},
		)

		// Remove from the source system and tell bubble-mates the ship disappeared.
		srcSS := ds.solarSystem
		if srcSS != nil && ds.entity != nil {
			// Drones cannot follow through a stargate; abandon them in place.
			if globalDroneMgr != nil && ds.shipID != 0 {
				globalDroneMgr.AbandonControl(ds.shipID)
			}
			// Carry combat damage across the jump; registerUndock reads it back in the destination.
			persistShipDamage(ds, sqldb)
			captureShipCharge(ds)
			remaining := srcSS.RemoveEntity(ds.entity.ID)
			for _, mate := range remaining {
				if mate.Conn == nil || !mate.KnowsBall(ds.entity.ID) {
					continue
				}
				_ = mate.SendStamped(func(mateStamp uint32) error {
					return destiny.SendRemoveBalls(mate.Conn, globalNodeID, mate.AccountID, mateStamp, []uint32{ds.entity.ID})
				})
				mate.ForgetBalls(ds.entity.ID)
			}
			releaseSystem(ds.sysMgr, srcSS, sqldb)
		}
		if ds.cancelReconcile != nil {
			ds.cancelReconcile()
			ds.cancelReconcile = nil
		}
		// See unregisterDock: a stale mate snapshot must not deliver into the ballpark about
		// to be replaced by the destination's.
		if ds.entity != nil {
			ds.entity.ClearBallparkState()
		}
		ds.solarSystem = nil
		ds.entity = nil

		// Build the destination while the flash plays, so the switch below does not wait on a
		// cold system (sentries, belts, NPC spawners).
		prewarmJumpDestination(ds.sysMgr, destSysID, sqldb, addr)

		// Phase 2: the session change tears down the client's ballpark, so it waits until the
		// JumpOut flash has played. The ship already left the source system above, so nothing
		// can shoot or tackle it during the wait.
		time.Sleep(gateJumpFlashDelay())
		oldSysID := ds.sysID
		oldConstID := ds.constellationID
		oldRegID := ds.regionID

		// Landing: GateArrivalRangeM from the destination gate on a radial scattered in a cone around the
		// jump line. See docs/destiny-arrival-lanes.md#gate-scatter.
		destGatePos := destiny.Vec3{X: destX, Y: destY, Z: destZ}
		var fromPos destiny.Vec3
		for _, cel := range ds.celestials {
			if cel.ItemID == int64(fromGateArg.Value) {
				fromPos = destiny.Vec3{X: cel.X, Y: cel.Y, Z: cel.Z}
				break
			}
		}
		// A gate built after this pilot entered the system is missing from the per-connection
		// snapshot; the system's shared slices have it (stargatelive.go).
		if fromPos.IsZero() && srcSS != nil {
			if p, ok := srcSS.CelestialPos(uint32(fromGateArg.Value)); ok {
				fromPos = p
			}
		}
		jumpDir := destGatePos.Sub(fromPos)
		if jumpDir.IsZero() {
			jumpDir = destiny.Vec3{X: 1}
		}
		// Arrival lane, the registry the warp landing also claims from; it runs outward along the
		// scattered radial. See docs/destiny-arrival-lanes.md#reservations-and-occupancy.
		landingBase, arrivalDir := destiny.GateArrivalPoint(destGatePos, jumpDir, nil)
		var destSS *destiny.SolarSystem
		if ds.sysMgr != nil {
			// Non-creating: loadSystemForConn loads the destination below, so an empty system has
			// no entities to be clear of and only the reservations apply.
			destSS = ds.sysMgr.Get(destSysID)
		}
		landingPos, laneClaim := destiny.ClaimArrivalPointEx(destSS, destSysID,
			landingBase, arrivalDir, dm.Radius(), dm.EntityID)
		// Logged in the same shape as warp engage so both arrival doors read alike.
		log.Printf("[%s] gate ARRIVE: landing=(%.0f,%.0f,%.0f) gate=(%.0f,%.0f,%.0f) "+
			"slot=%d overflow=%v rejected=%d res=%d occupied=%d moved=%.0fm sysID=%d",
			addr, landingPos.X, landingPos.Y, landingPos.Z,
			destGatePos.X, destGatePos.Y, destGatePos.Z,
			laneClaim.Slot, laneClaim.Overflow, laneClaim.Rejected,
			laneClaim.Reservations, laneClaim.Occupied,
			landingPos.Sub(landingBase).Len(), destSysID)

		dm.SetPosition(landingPos)
		dm.Stop()
		if err := loadSystemForConn(addr, sqldb, destSysID, ds); err != nil {
			log.Printf("[%s] → destiny.StargateJump loadSystem sysID=%d: %v", addr, destSysID, err)
		}
		ds.sysID = destSysID
		ds.constellationID = destConstID
		ds.regionID = destRegID

		// Register in destination system.
		registerUndock(c, addr, uint64(accountID), ds.charID, sqldb, ds)

		// Gate cloak: the pilot arrives invisible and untargetable until the first move, first
		// module activation or the expiry timer, whichever comes first.
		if ds.entity != nil {
			ds.entity.SetCloaked(true)
			armGateCloakExpiry(ds)
		}

		// Persist the location before the session change, so GetChannels cannot read the old
		// row and re-join the old system channel.
		locCtx, locCancel := context.WithTimeout(context.Background(), 3*time.Second)
		if err := evedb.UpdateCharacterLocation(locCtx, sqldb, ds.charID, destSysID, destConstID, destRegID); err != nil {
			log.Printf("[%s] StargateJump: persist location: %v", addr, err)
		}
		locCancel()

		// Record jump for starmap overlay.
		go func() {
			defer safego.Recover("handleDestinyBoundCall")
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = evedb.RecordMapEvent(ctx, sqldb, destSysID, evedb.StatJump, 1)
		}()

		// Swap local chat channel: leave old system, join new system.
		lsc.SwapSystemChannel(c, accountID, oldSysID, destSysID, globalLSCReg, sqldb)

		// The criminal timer follows the pilot; CONCORD meets them on the far side.
		carryCriminalFlag(oldSysID, destSysID, ds.charID)

		// Reset setStateSent so the client's GetFormations call triggers the SetState. Sending
		// it here races the client's beyonce bind ("No ballpark for update").
		ds.setStateSent = false
		ds.pendingGateEffect = true
		ds.pendingGateID = toGateID

		if err := sendJumpSessionChange(c, addr, uint64(accountID), oldSysID, destSysID, oldConstID, destConstID, oldRegID, destRegID); err != nil {
			return err
		}
		// Persist position (fire-and-forget).
		go func() {
			defer safego.Recover("handleDestinyBoundCall")
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := evedb.UpdateEntityPosition(ctx, sqldb, ds.shipID, landingPos.X, landingPos.Y, landingPos.Z); err != nil {
				log.Printf("[%s] StargateJump: persist position: %v", addr, err)
			}
		}()
		return nil

	default:
		log.Printf("[%s] → bound.%s (destiny — returning None)", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
}

// egoEchoQueued reports whether the pilot's own nav echo (Stop, Orbit, Follow, SetSpeedFraction)
// rides the queue instead of minting. Under the stamp ceiling it goes at once as before; over it,
// an urgent mint would run the client's sim clock ahead and it would fast-forward the grid, so the
// echo waits for the next stamped flush (the system tick drains it, normally within a second) and
// the latest click wins. Only for the pilot's own ship.
func egoEchoQueued(ds *destinyConn, dm *destiny.Manager) bool {
	return ds.entity != nil && ds.entity.Manager == dm && !ds.entity.UnderStampCeiling()
}

func queuedNote(queued bool) string {
	if queued {
		return " (over stamp ceiling, echo queued)"
	}
	return ""
}
