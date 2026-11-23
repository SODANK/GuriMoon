package main

import (
	"context"
	"database/sql"
	"log"
	"math"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/esp"
	"gurimoon/gang"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/npc"
	"gurimoon/server"
)

// Gang warp. The client's "Warp Gang" entries (menusvc.GangWarp, drawn for the
// leader) send the leader's own WarpToStuff with gang=True. The leader's warp
// resolves as usual and the resolved destination (never the request: bookmarks
// and scan results are the leader's own) fans out to every member on the
// leader's grid: a connected member on its own connection goroutine
// (ds.gangWarp), which owns every ds field a warp touches; a socketless member
// through gang.ModuleWarp. The leader's arrival hook is not shared (a mission
// bookmark spawns the leader's instance); shared sites load per member.

// gangWarpRange is how far from the leader a member may be and still be
// carried: the leader's grid, never across the system.
const gangWarpRange = destiny.BubbleRadius

// gangWarpReq is the leader's resolved warp, as handed to a member.
type gangWarpReq struct {
	leaderCharID  int64
	sysID         int64
	warpType      string
	targetID      int64
	celestial     destiny.Vec3
	targetRadius  float64
	landingBuffer float64
	// landing is the leader's static warp-in point (stars, planets, moons), nil
	// for an approach-vector landing, which each member computes from its own
	// position as startPlayerWarp does.
	landing *destiny.Vec3
}

// beaconWarpArrival is the arrival hook for a warp to a shared site (a
// custom-seeded dungeon entrance beacon or a spawner-placed open site): load a
// room on arrival and land at the pilot's requested range from the beacon (site=true).
// Shared by WarpToStuff and the gang warp.
func beaconWarpArrival(c *evenet.Conn, addr string, accountID int64, sqldb *sql.DB, ds *destinyConn,
	sessReg *esp.SessionRegistry, npcMgr *npc.NPCManager, warpType string, targetID int64,
	beaconPos destiny.Vec3) (onArrive func(), site bool) {
	if warpType != "item" && warpType != "scan" {
		return nil, false
	}
	// Custom-seeded dungeon entrance: a beacon in the custom celestial band with
	// a dungeon link spawns its room on arrival, like a mission deadspace warp.
	if targetID >= evedb.CustomCelestialIDFloor && targetID <= evedb.CustomCelestialIDCeil {
		ccCtx, ccCancel := context.WithTimeout(context.Background(), 3*time.Second)
		vid, roomIdx, hasDungeon, ccErr := evedb.GetCustomCelestialDungeon(ccCtx, sqldb, targetID)
		var fixed *openSite
		if ccErr == nil && hasDungeon {
			// One shared, never-despawning instance per entrance (fixedsites.go); the private
			// per-pilot room below is the fallback when it cannot be built.
			var fErr error
			if fixed, fErr = fixedSiteFor(ccCtx, sqldb, targetID, vid, roomIdx); fErr != nil {
				log.Printf("[%s] WarpToStuff: fixed dungeon %d: %v (loading a private room)", addr, targetID, fErr)
			}
		}
		if fixed != nil {
			siteID, roomID := fixed.row.SiteID, fixed.firstRoomID
			onArrive = func() {
				ds.dunSpawnShift = destiny.Vec3{}
				ds.dunPendingOpenSite = siteID
				if sErr := spawnDungeonRoom(c, addr, accountID, sqldb, ds, roomID, sessReg, npcMgr); sErr != nil {
					log.Printf("[%s] WarpToStuff: spawnDungeonRoom %d for fixed dungeon %d: %v", addr, roomID, siteID, sErr)
				}
			}
			site = true
			log.Printf("[%s] WarpToStuff: beacon %d is fixed dungeon %s (room %d)", addr, targetID, fixed.name, roomID)
		} else if ccErr == nil && hasDungeon {
			roomIDs, rErr := evedb.GetDungeonRoomIDs(ccCtx, sqldb, vid)
			if rErr == nil && len(roomIDs) > 0 {
				if roomIdx < 1 || roomIdx > len(roomIDs) {
					roomIdx = 1
				}
				roomID := roomIDs[roomIdx-1]
				onArrive = func() {
					// Stale gate travel from a previous dungeon must not
					// shift this room's anchor. The room's beacon stands on the entrance
					// beacon, so the pilot arrives at the range they asked for:
					// dunPendingAnchor is the pre-lane landing point and the shift is the
					// gap from it to the beacon.
					ds.dunSpawnShift = destiny.Vec3{}
					if ds.dunPendingAnchor != nil {
						ds.dunSpawnShift = beaconPos.Sub(*ds.dunPendingAnchor)
					}
					if sErr := spawnDungeonRoom(c, addr, accountID, sqldb, ds, roomID, sessReg, npcMgr); sErr != nil {
						log.Printf("[%s] WarpToStuff: spawnDungeonRoom %d for beacon %d: %v", addr, roomID, targetID, sErr)					}
				}
				// Same rule as a mission deadspace bookmark: the beacon is
				// the site anchor, so land on it.
				site = true
				log.Printf("[%s] WarpToStuff: beacon %d is a dungeon entrance (vid=%d room=%d)", addr, targetID, vid, roomID)
			} else if rErr != nil {
				log.Printf("[%s] WarpToStuff: dungeon beacon %d rooms lookup: %v", addr, targetID, rErr)
			}
		} else if ccErr != nil {
			log.Printf("[%s] WarpToStuff: custom celestial lookup %d: %v", addr, targetID, ccErr)
		}
		ccCancel()
	}
	// Spawner-placed open site (opensites.go): one shared instance anchored on
	// its beacon, so land on the beacon and load room 1 in its frame.
	if targetID > 0 && targetID <= math.MaxUint32 {
		if osite := openSiteByBall(uint32(targetID)); osite != nil && !osite.isGone() {
			siteID, roomID := osite.row.SiteID, osite.firstRoomID
			onArrive = func() {
				ds.dunSpawnShift = destiny.Vec3{}
				ds.dunPendingOpenSite = siteID
				if sErr := spawnDungeonRoom(c, addr, accountID, sqldb, ds, roomID, sessReg, npcMgr); sErr != nil {
					log.Printf("[%s] WarpToStuff: spawnDungeonRoom %d for open site %d: %v", addr, roomID, siteID, sErr)
				}
			}
			site = true
			log.Printf("[%s] WarpToStuff: beacon %d is open site %d (%s, room %d)", addr, targetID, siteID, osite.name, roomID)
		}
	}
	return onArrive, site
}

// gangWarpKwarg reads WarpToStuff's gang keyword: True from menusvc, absent
// everywhere else.
func gangWarpKwarg(v marshal.PyRep) bool { return pyTruthy(v) }

// dispatchGangWarp fans the leader's warp out to the gang. Runs on the
// leader's connection goroutine, before the leader's own warp starts, so
// ds.dm.Pos is still where the leader stood when they gave the order.
func dispatchGangWarp(addr string, ds *destinyConn, req gangWarpReq) {
	if globalGangReg == nil || ds == nil || ds.solarSystem == nil || ds.dm == nil {
		return
	}
	gangID := globalGangReg.GetCharGang(ds.charID)
	if gangID == 0 {
		log.Printf("[%s] gang warp: char %d is in no gang — warping alone", addr, ds.charID)
		return
	}
	g := globalGangReg.GetGang(gangID)
	if g == nil {
		return
	}
	// The client only draws the entry for the leader, but the client is not
	// the gate: anyone could send the keyword.
	if g.LeaderID != ds.charID {
		log.Printf("[%s] gang warp: char %d is not the leader of gang %d (leader %d) — warping alone",
			addr, ds.charID, gangID, g.LeaderID)
		return
	}
	leaderPos := ds.dm.Pos
	var unconnected []int64
	queued, offGrid, full := 0, 0, 0
	for charID := range g.Members {
		if charID == ds.charID {
			continue
		}
		e := ds.solarSystem.GetPlayerByCharID(charID)
		if e != nil && e.Conn != nil {
			mds, ok := e.PlayerState.(*destinyConn)
			if !ok || mds == nil || mds.gangWarp == nil {
				continue
			}
			if e.Pos().Sub(leaderPos).Len() > gangWarpRange {
				offGrid++
				continue
			}
			select {
			case mds.gangWarp <- req:
				queued++
			default:
				// One gang warp already waiting on that pilot: they are about
				// to warp with this fleet anyway.
				full++
			}
			continue
		}
		unconnected = append(unconnected, charID)
	}
	if len(unconnected) > 0 && gang.ModuleWarp != nil {
		gang.ModuleWarp(gang.WarpOrder{
			GangID:   gangID,
			LeaderID: ds.charID,
			SystemID: ds.sysID,
			TargetID: req.targetID,
			X:        req.celestial.X, Y: req.celestial.Y, Z: req.celestial.Z,
			ExitDist: req.targetRadius + req.landingBuffer,
			LeaderX:  leaderPos.X, LeaderY: leaderPos.Y, LeaderZ: leaderPos.Z,
			Members:  unconnected,
		})
	}
	log.Printf("[%s] gang warp: gang %d leader %d → %s %d: %d pilot(s) queued, %d off grid, %d already queued, %d handed to module",
		addr, gangID, ds.charID, req.warpType, req.targetID, queued, offGrid, full, len(unconnected))
}

// runGangWarp is a member's half: the leader's warp, started on this pilot's
// own connection goroutine. Everything the member's own WarpToStuff would
// refuse is refused here too; a fleet warp does not carry a scrambled ship.
func runGangWarp(c *evenet.Conn, addr string, accountID int64, sqldb *sql.DB, ds *destinyConn,
	sessReg *esp.SessionRegistry, npcMgr *npc.NPCManager, req gangWarpReq) {
	dm := ds.dm
	if dm == nil || ds.solarSystem == nil || ds.entity == nil || ds.sysID != req.sysID {
		return // docked, jumped or reloaded between the order and now
	}
	if globalGangReg == nil {
		return
	}
	// Still in the leader's gang: a member who left while the order sat in the
	// queue is not warped by it.
	if gid := globalGangReg.GetCharGang(ds.charID); gid == 0 || gid != globalGangReg.GetCharGang(req.leaderCharID) {
		return
	}
	if ds.entity.IsDestroyed() {
		return
	}
	if warpNavLocked(ds) {
		log.Printf("[%s] gang warp: already in warp — ignored", addr)
		return
	}
	if pinned, mod := shipImmobilised(ds); pinned {
		log.Printf("[%s] gang warp: refused — pinned by %s", addr, mod)
		return
	}
	if held, net := warpScrambleHold(ds, sqldb); held {
		log.Printf("[%s] gang warp: refused — warp scrambled (net %.0f)", addr, net)
		pushRemoteMessage(c, accountID, "WarpScrambled", nil)
		return
	}
	if dungeonWarpBlocked(ds, req.celestial) {
		log.Printf("[%s] gang warp: refused — deadspace natural phenomena (room %d)", addr, ds.dunRoomID)
		pushRemoteMessage(c, accountID, "WarpingWithinGlobalDisruptors", nil)
		return
	}
	breakGateCloak(ds)

	// The leader's resolved buffer already carries the site range they asked for.
	landingBuffer := req.landingBuffer
	onArrive, _ := beaconWarpArrival(c, addr, accountID, sqldb, ds, sessReg, npcMgr, req.warpType, req.targetID, req.celestial)
	log.Printf("[%s] gang warp: following leader %d to %s %d", addr, req.leaderCharID, req.warpType, req.targetID)
	startPlayerWarp(c, addr, accountID, sqldb, ds, dm, req.celestial, req.targetRadius, landingBuffer, req.landing, warpPaysCap, onArrive)
}

// handleGangRegroup is beyonce.GangRegroup(): the leader's Regroup entry pulls
// every member in the system (not only the grid) to the leader's position,
// landed like a warp to a fleet member through runGangWarp, each told who they
// regroup to (GangRegroup). Socketless members go to gang.ModuleWarp.
func handleGangRegroup(c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID int64, ds *destinyConn) error {
	none := marshal.PyNone{}
	if globalGangReg == nil || ds == nil || ds.solarSystem == nil || ds.dm == nil || ds.entity == nil {
		return server.SendCallRsp(c, pkt, accountID, none)
	}
	gangID := globalGangReg.GetCharGang(ds.charID)
	g := globalGangReg.GetGang(gangID)
	if gangID == 0 || g == nil || g.LeaderID != ds.charID {
		log.Printf("[%s] gang regroup: char %d is not a gang leader", addr, ds.charID)
		return server.SendCallRsp(c, pkt, accountID, none)
	}
	leaderPos := ds.dm.Pos
	req := gangWarpReq{
		leaderCharID:  ds.charID,
		sysID:         ds.sysID,
		warpType:      "char",
		targetID:      ds.charID,
		celestial:     leaderPos,
		targetRadius:  ds.entity.Manager.Radius(),
		landingBuffer: deadspaceLandingBuffer,
	}
	leaderName := ds.charName
	var unconnected []int64
	queued := 0
	for charID := range g.Members {
		if charID == ds.charID {
			continue
		}
		e := ds.solarSystem.GetPlayerByCharID(charID)
		if e != nil && e.Conn != nil {
			mds, ok := e.PlayerState.(*destinyConn)
			if !ok || mds == nil || mds.gangWarp == nil {
				continue
			}
			pushRemoteMessage(e.Conn, int64(e.AccountID), "GangRegroup", map[string]marshal.PyRep{
				"leader": marshal.PyString{Value: leaderName},
			})
			select {
			case mds.gangWarp <- req:
				queued++
			default:
			}
			continue
		}
		unconnected = append(unconnected, charID)
	}
	if len(unconnected) > 0 && gang.ModuleWarp != nil {
		gang.ModuleWarp(gang.WarpOrder{
			GangID:   gangID,
			LeaderID: ds.charID,
			SystemID: ds.sysID,
			TargetID: ds.charID,
			X:        leaderPos.X, Y: leaderPos.Y, Z: leaderPos.Z,
			ExitDist: req.targetRadius + req.landingBuffer,
			LeaderX:  leaderPos.X, LeaderY: leaderPos.Y, LeaderZ: leaderPos.Z,
			Members:  unconnected,
		})
	}
	log.Printf("[%s] gang regroup: gang %d leader %d: %d pilot(s) queued, %d handed to module",
		addr, gangID, ds.charID, queued, len(unconnected))
	return server.SendCallRsp(c, pkt, accountID, none)
}
