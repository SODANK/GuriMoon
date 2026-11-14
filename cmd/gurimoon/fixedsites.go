package main

// Fixed dungeons: a Dungeon Entrance placed from ESP Solar System (a custom celestial with a
// dungeon link, db/customcelestialdb.go). Each runs as one shared open-site instance keyed by the
// celestial's itemID, built on the first warp-in and kept for the life of the process. It never
// clears or despawns. A killed object comes back fixedRespawnDelay after its death; a wave group
// resets as a whole once its last rat has been dead that long. See docs/open-sites.md#fixed-dungeons.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	evenet "gurimoon/net"
	"gurimoon/npc"
)

// fixedRespawnDelay is how long a fixed dungeon's killed object stays dead
// (server setting dungeon.fixedRespawnMinutes, default 10, 1 to 1440).
func fixedRespawnDelay() time.Duration {
	return evedb.CachedSettingDuration("dungeon.fixedRespawnMinutes", time.Minute, 10, 1, 24*60)
}

// fixedRespawnBook is a fixed site's pending respawns. Objects outside any wave come back one at a
// time; a wave group comes back as a whole. The owner's mu guards it.
type fixedRespawnBook struct {
	objDue  map[int64]time.Time    // dunObjects.objectID → due
	waveDue map[[2]int64]time.Time // {roomID, wave} → due
}

// noteKill books the respawn for one kill. wave 0 books the object itself. A wave member books its
// group only when groupDead says every rat of the group is now dead; until then nothing is booked.
func (b *fixedRespawnBook) noteKill(objectID, roomID, wave int64, groupDead bool, at time.Time, delay time.Duration) {
	due := at.Add(delay)
	if wave <= 0 {
		if b.objDue == nil {
			b.objDue = map[int64]time.Time{}
		}
		b.objDue[objectID] = due
		return
	}
	if !groupDead {
		return
	}
	if b.waveDue == nil {
		b.waveDue = map[[2]int64]time.Time{}
	}
	b.waveDue[[2]int64{roomID, wave}] = due
}

// next is the earliest booked respawn; ok is false when nothing is booked.
func (b *fixedRespawnBook) next() (at time.Time, ok bool) {
	for _, t := range b.objDue {
		if !ok || t.Before(at) {
			at, ok = t, true
		}
	}
	for _, t := range b.waveDue {
		if !ok || t.Before(at) {
			at, ok = t, true
		}
	}
	return at, ok
}

// takeDue removes and returns every booking due at now, sorted so the result is stable.
func (b *fixedRespawnBook) takeDue(now time.Time) (objs []int64, waves [][2]int64) {
	for oid, t := range b.objDue {
		if !t.After(now) {
			objs = append(objs, oid)
			delete(b.objDue, oid)
		}
	}
	for g, t := range b.waveDue {
		if !t.After(now) {
			waves = append(waves, g)
			delete(b.waveDue, g)
		}
	}
	sort.Slice(objs, func(i, j int) bool { return objs[i] < objs[j] })
	sort.Slice(waves, func(i, j int) bool {
		if waves[i][0] != waves[j][0] {
			return waves[i][0] < waves[j][0]
		}
		return waves[i][1] < waves[j][1]
	})
	return objs, waves
}

var fixedSiteBuildMu sync.Mutex

// fixedSiteFor returns the live instance of a custom-celestial dungeon, building and registering
// it on first use. dungeonVID and roomIdx are the celestial's link (GetCustomCelestialDungeon);
// roomIdx picks the entry room, which is anchored on the celestial.
func fixedSiteFor(ctx context.Context, sqldb *sql.DB, itemID, dungeonVID int64, roomIdx int) (*openSite, error) {
	get := func() (*openSite, error) {
		s := openSiteByID(itemID)
		if s == nil {
			return nil, nil
		}
		if !s.fixed {
			return nil, fmt.Errorf("site ID %d is already a spawner site", itemID)
		}
		return s, nil
	}
	if s, err := get(); s != nil || err != nil {
		return s, err
	}
	fixedSiteBuildMu.Lock()
	defer fixedSiteBuildMu.Unlock()
	if s, err := get(); s != nil || err != nil {
		return s, err
	}

	sysID, x, y, z, ok, err := evedb.GetCustomCelestialPosition(ctx, sqldb, itemID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("custom celestial %d has no mapDenormalize row", itemID)
	}
	rooms, err := evedb.GetDungeonRoomIDs(ctx, sqldb, dungeonVID)
	if err != nil {
		return nil, err
	}
	if len(rooms) == 0 {
		return nil, fmt.Errorf("dungeon %d has no rooms", dungeonVID)
	}
	if roomIdx < 1 || roomIdx > len(rooms) {
		roomIdx = 1
	}
	s, err := newOpenSite(ctx, sqldb, evedb.OpenSite{
		SiteID: itemID, DungeonVID: dungeonVID, SolarSystemID: sysID,
		X: x, Y: y, Z: z, CreatedAt: time.Now().Unix(), BeaconVisible: true,
	})
	if err != nil {
		return nil, err
	}
	objs := map[int64][2]int64{}
	for _, roomID := range rooms {
		rows, err := evedb.GetRoomObjects(ctx, sqldb, roomID)
		if err != nil {
			return nil, err
		}
		for _, o := range rows {
			var wave int64
			if o.SpawnWave.Valid && o.SpawnWave.Int64 > 0 {
				wave = o.SpawnWave.Int64
			}
			objs[o.ObjectID] = [2]int64{roomID, wave}
		}
	}
	s.fixed = true
	s.ballID = 0
	s.firstRoomID = rooms[roomIdx-1]
	s.fixedObjs = objs
	registerOpenSite(s)
	log.Printf("[fixedsite] %d (%s) live in sys=%d, entry room %d, %d object(s)",
		itemID, s.name, sysID, s.firstRoomID, len(objs))
	return s, nil
}

// fixedSiteKilled books the respawn of an object of a fixed site that just died. The caller has
// already marked it killed.
func fixedSiteKilled(s *openSite, objectID int64) {
	info, ok := s.fixedObjs[objectID]
	if !ok {
		return
	}
	roomID, wave := info[0], info[1]
	groupDead := true
	if wave > 0 {
		for _, oid := range s.census[roomID][wave] {
			if !s.state.IsKilled(oid) {
				groupDead = false
				break
			}
		}
	}
	s.mu.Lock()
	s.fixedRespawn.noteKill(objectID, roomID, wave, groupDead, time.Now(), fixedRespawnDelay())
	s.mu.Unlock()
	armFixedRespawn(s)
}

// armFixedRespawn keeps one one-shot armed at the site's earliest booked respawn.
func armFixedRespawn(s *openSite) {
	if globalTickMgr == nil {
		return
	}
	s.respawnArmMu.Lock()
	defer s.respawnArmMu.Unlock()
	s.mu.Lock()
	at, ok := s.fixedRespawn.next()
	old := s.respawnTimer
	s.respawnTimer = 0
	gone := s.gone
	s.mu.Unlock()
	if old != 0 {
		globalTickMgr.CancelOnce(old)
	}
	if !ok || gone {
		return
	}
	siteID := s.row.SiteID
	id := globalTickMgr.ScheduleOnce(fmt.Sprintf("fixedsite-respawn-%d", siteID), at, func() {
		go runFixedRespawn(siteID)
	})
	s.mu.Lock()
	s.respawnTimer = id
	s.mu.Unlock()
}

// dunRespawnReq asks a connection to bring back a fixed site's respawned objects in the room it
// holds. Reload rebuilds the whole room instead, which re-arms its triggers after a wave reset.
type dunRespawnReq struct {
	SiteID    int64
	RoomID    int64
	ObjectIDs []int64
	Reload    bool
}

// runFixedRespawn puts every due object and wave group back on the site and tells the pilots in
// the rooms concerned. A room nobody is in builds them on the next warp-in.
func runFixedRespawn(siteID int64) {
	s := openSiteByID(siteID)
	if s == nil || !s.fixed || s.isGone() {
		return
	}
	s.mu.Lock()
	objs, waves := s.fixedRespawn.takeDue(time.Now())
	s.mu.Unlock()

	perRoom := map[int64][]int64{}
	for _, oid := range objs {
		s.state.Unkill(oid)
		room := s.fixedObjs[oid][0]
		perRoom[room] = append(perRoom[room], oid)
	}
	reload := map[int64]bool{}
	for _, g := range waves {
		s.state.UnreleaseWave(g[0], g[1])
		for oid, info := range s.fixedObjs {
			if info == g {
				s.state.Unkill(oid)
			}
		}
		reload[g[0]] = true
	}
	if len(objs) > 0 || len(waves) > 0 {
		log.Printf("[fixedsite] %d (%s): %d object(s) and %d wave group(s) respawned",
			siteID, s.name, len(objs), len(waves))
	}

	var reqs []dunRespawnReq
	for room := range reload {
		reqs = append(reqs, dunRespawnReq{SiteID: siteID, RoomID: room, Reload: true})
	}
	for room, oids := range perRoom {
		if !reload[room] {
			reqs = append(reqs, dunRespawnReq{SiteID: siteID, RoomID: room, ObjectIDs: oids})
		}
	}
	if len(reqs) > 0 && globalSysMgr != nil {
		if ss := globalSysMgr.Get(s.row.SolarSystemID); ss != nil {
			for _, p := range ss.GetAllPlayersInSystem() {
				pds, ok := p.PlayerState.(*destinyConn)
				if !ok || pds == nil {
					continue
				}
				for _, req := range reqs {
					select {
					case pds.dunRespawn <- req:
					default:
						// Queue full or no channel: the room catches up on its next load.
					}
				}
			}
		}
	}
	armFixedRespawn(s)
}

// fixedRespawnGridRange is how close to its room a pilot must be for a respawn to reach them. A
// pilot further out (warped off with the room still loaded) gets it on the next warp-in.
const fixedRespawnGridRange = destiny.BubbleRadius

// handleDunRespawn runs a dunRespawnReq on the connection goroutine. It does nothing unless the
// pilot is in that site's room, in space, out of warp and on its grid.
func handleDunRespawn(c *evenet.Conn, addr string, accountID int64, sqldb *sql.DB, ds *destinyConn, npcMgr *npc.NPCManager, req dunRespawnReq) {
	if ds.dunOpenSiteID != req.SiteID || ds.dunRoomID != req.RoomID || ds.solarSystem == nil || ds.dm == nil || npcMgr == nil {
		return
	}
	if ds.dunPendingMode || warpNavLocked(ds) {
		return
	}
	s := openSiteByID(req.SiteID)
	if s == nil || !s.fixed {
		return
	}
	if ds.dm.Pos.Sub(ds.dunOffset.Add(ds.dunRoomAnchor)).Len() > fixedRespawnGridRange {
		return
	}
	if req.Reload {
		log.Printf("[%s] fixed site %d: wave reset, reloading room %d", addr, req.SiteID, req.RoomID)
		ds.dunPendingOpenSite = req.SiteID
		if err := spawnDungeonRoom(c, addr, accountID, sqldb, ds, req.RoomID, globalSessReg, npcMgr, ds.dunMissionID); err != nil {
			log.Printf("[%s] fixed site %d: reload room %d: %v", addr, req.SiteID, req.RoomID, err)
		}
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	objs, err := evedb.GetRoomObjects(ctx, sqldb, req.RoomID)
	if err != nil {
		log.Printf("[%s] fixed site %d: GetRoomObjects %d: %v", addr, req.SiteID, req.RoomID, err)
		return
	}
	want := make(map[int64]bool, len(req.ObjectIDs))
	for _, oid := range req.ObjectIDs {
		want[oid] = true
	}
	// Drop the dead NPCs' stale mappings and note which wanted objects still stand.
	standing := map[int64]bool{}
	ds.dunObjMu.Lock()
	for eid, oid := range ds.dunObjects {
		if !want[oid] {
			continue
		}
		if npcMgr.IsNPC(eid) && ds.solarSystem.GetEntity(eid) != nil {
			standing[oid] = true
		} else {
			delete(ds.dunObjects, eid)
		}
	}
	ds.dunObjMu.Unlock()
	var back []evedb.ObjectRow
	for _, o := range objs {
		if !want[o.ObjectID] || standing[o.ObjectID] || s.state.IsKilled(o.ObjectID) {
			continue
		}
		if o.SpawnWave.Valid && o.SpawnWave.Int64 > 0 && !s.state.WaveReleased(req.RoomID, o.SpawnWave.Int64) {
			continue
		}
		back = append(back, o)
	}
	if len(back) == 0 {
		return
	}
	log.Printf("[%s] fixed site %d: respawning %d object(s) in room %d", addr, req.SiteID, len(back), req.RoomID)
	s.spawnMu.Lock()
	defer s.spawnMu.Unlock()
	if err := spawnDungeonObjects(c, addr, accountID, sqldb, ds, npcMgr, back, objs); err != nil {
		log.Printf("[%s] fixed site %d: respawn: %v", addr, req.SiteID, err)
	}
}

// dropFixedSite takes a fixed dungeon out of the world after its entrance is deleted in ESP: its
// NPCs (quietly: no wreck, no bounty), its shared scenery on every grid, and its timers.
func dropFixedSite(itemID int64) {
	s := openSiteByID(itemID)
	if s == nil || !s.fixed {
		return
	}
	s.respawnArmMu.Lock()
	s.mu.Lock()
	s.gone = true
	timer := s.respawnTimer
	s.respawnTimer = 0
	s.mu.Unlock()
	s.respawnArmMu.Unlock()
	unregisterOpenSite(s)
	if timer != 0 && globalTickMgr != nil {
		globalTickMgr.CancelOnce(timer)
	}
	npcs := 0
	if globalNPCMgr != nil {
		for _, eid := range globalNPCMgr.OpenSiteEntities(itemID) {
			if globalNPCMgr.Despawn(eid, "fixed dungeon removed") {
				npcs++
			}
		}
	}
	if globalSysMgr != nil {
		if ss := globalSysMgr.Get(s.row.SolarSystemID); ss != nil {
			if ids := s.rigidBallIDs(); len(ids) > 0 {
				broadcastBallRemove(ss, ids...)
			}
		}
	}
	log.Printf("[fixedsite] %d (%s) removed from sys=%d, %d NPC(s) despawned", itemID, s.name, s.row.SolarSystemID, npcs)
}
