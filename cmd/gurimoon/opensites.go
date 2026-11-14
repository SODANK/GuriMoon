package main

// Open dungeon sites: spawner-placed deadspace pockets any pilot can warp to (spawner config in
// db/dunspawnerdb.go). A site is one shared instance of a dungeon template: one beacon ball in the
// starbase slices, NPCs built once on the first warp-in and adopted afterwards, one rigid ball ID
// per scenery object, rooms anchored in the site's own frame, and a clear rule that arms a despawn
// timer. Kill state is memory-only; the row, position and pending despawn survive a restart.
// See docs/cmd-misc-notes.md#open-sites.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math/rand"
	"runtime/debug"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/esp"
	"gurimoon/safego"
	"gurimoon/tick"
)

const (
	openSiteBeaconRadius = 2500
	// openSiteSpawnsPerPass caps how many sites one spawner places per pass,
	// so a fresh rule with a big target fills over a few minutes instead of
	// in one burst of queries.
	openSiteSpawnsPerPass = 3
	// openSiteSystemTries is how many candidate systems a placement tries
	// before giving up for this pass.
	openSiteSystemTries = 10
)

func openSiteBallID(siteID int64) uint32 {
	off := siteID % openSiteBallSpan
	if off < 0 {
		off += openSiteBallSpan
	}
	return uint32(openSiteBallBand + off)
}

// openSite is one live site's runtime state.
type openSite struct {
	row         evedb.OpenSite
	name        string
	firstRoomID int64
	census      evedb.DungeonRatCensus
	ballID      uint32

	// roomWaves and roomWaveTrigs are each room's held wave groups and spawnWave triggers, read
	// once at load for the clear rule's auto-chain test (openSite.wavePending).
	roomWaves     map[int64][]int64
	roomWaveTrigs map[int64][]evedb.TriggerRow

	// completed is set by a completeDungeon trigger and counts as a clear.
	completed bool

	// evalMu serialises reevaluateOpenSite, so the last evaluation reads every kill and release
	// made before it.
	evalMu sync.Mutex

	// state is the killed/released-wave record, the mission site type with
	// missionID 0 so nothing of it is persisted.
	state *dunSiteState

	// spawnMu serialises building the site, so two pilots landing together
	// cannot each spawn a copy of the rats before either is registered.
	spawnMu sync.Mutex

	mu          sync.Mutex
	roomOffsets map[int64]destiny.Vec3
	rigidIDs    map[int64]uint32 // dunObjects.objectID → shared rigid ball ID
	rigidSet    map[uint32]bool
	clearedAt   int64 // unix seconds, 0 = not cleared
	timer       uint64
	gone        bool
	// clearEmitted is set once the OpenSiteCleared world event went out; a reopened site
	// that clears again does not send a second one.
	clearEmitted bool
	// inflight is the waves queued to a connection and not yet spawned ([roomID, wave] -> give-up
	// time). The clear rule waits on them.
	inflight map[[2]int64]time.Time

	// fixed marks an ESP-placed dungeon entrance (fixedsites.go): no dunOpenSites row, no beacon
	// ball of its own, never cleared or despawned. Its killed objects respawn on fixedRespawn's
	// timers instead. fixedObjs maps every dunObjects.objectID of the dungeon to {roomID, wave}.
	fixed        bool
	fixedObjs    map[int64][2]int64
	fixedRespawn fixedRespawnBook // guarded by mu
	respawnTimer uint64           // guarded by mu
	respawnArmMu sync.Mutex
}

// openSiteInflightGrace is how long a queued wave may take to spawn before the clear rule stops
// waiting for it (a dropped request, a pilot who logged off).
const openSiteInflightGrace = 30 * time.Second

var openSiteReg = struct {
	sync.RWMutex
	byID   map[int64]*openSite
	byBall map[uint32]*openSite
}{byID: map[int64]*openSite{}, byBall: map[uint32]*openSite{}}

func openSiteByID(siteID int64) *openSite {
	if siteID <= 0 {
		return nil
	}
	openSiteReg.RLock()
	defer openSiteReg.RUnlock()
	return openSiteReg.byID[siteID]
}

func openSiteByBall(ballID uint32) *openSite {
	if ballID < openSiteBallBand || ballID >= openSiteBallBand+openSiteBallSpan {
		return nil
	}
	openSiteReg.RLock()
	defer openSiteReg.RUnlock()
	return openSiteReg.byBall[ballID]
}

func openSitesSnapshot() []*openSite {
	openSiteReg.RLock()
	defer openSiteReg.RUnlock()
	out := make([]*openSite, 0, len(openSiteReg.byID))
	for _, s := range openSiteReg.byID {
		// Spawner sites only: beacons, the spawner pass, ESP Live sites, probe signatures and the
		// rolebots bridge all read this list.
		if s.fixed {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].row.SiteID < out[j].row.SiteID })
	return out
}

func registerOpenSite(s *openSite) {
	openSiteReg.Lock()
	openSiteReg.byID[s.row.SiteID] = s
	if !s.fixed {
		openSiteReg.byBall[s.ballID] = s
	}
	openSiteReg.Unlock()
}

func unregisterOpenSite(s *openSite) {
	openSiteReg.Lock()
	if openSiteReg.byID[s.row.SiteID] == s {
		delete(openSiteReg.byID, s.row.SiteID)
	}
	if openSiteReg.byBall[s.ballID] == s {
		delete(openSiteReg.byBall, s.ballID)
	}
	openSiteReg.Unlock()
}

// newOpenSite builds the runtime half of a stored site row.
func newOpenSite(ctx context.Context, sqldb *sql.DB, row evedb.OpenSite) (*openSite, error) {
	rooms, err := evedb.GetDungeonRoomIDs(ctx, sqldb, row.DungeonVID)
	if err != nil {
		return nil, err
	}
	if len(rooms) == 0 {
		return nil, fmt.Errorf("dungeon %d has no rooms", row.DungeonVID)
	}
	census, err := evedb.LoadDungeonRatCensus(ctx, sqldb, row.DungeonVID)
	if err != nil {
		return nil, err
	}
	var name string
	if err := sqldb.QueryRowContext(ctx,
		`SELECT dungeonVName FROM dunDungeons WHERE dungeonVID = ?`, row.DungeonVID).Scan(&name); err != nil {
		return nil, fmt.Errorf("dungeon %d: %w", row.DungeonVID, err)
	}
	roomWaves := map[int64][]int64{}
	roomWaveTrigs := map[int64][]evedb.TriggerRow{}
	for _, roomID := range rooms {
		objs, err := evedb.GetRoomObjects(ctx, sqldb, roomID)
		if err != nil {
			return nil, err
		}
		if groups := evedb.RoomWaveGroups(objs); len(groups) > 0 {
			roomWaves[roomID] = groups
		}
		trigs, err := evedb.GetRoomTriggers(ctx, sqldb, roomID)
		if err != nil {
			return nil, err
		}
		for _, t := range trigs {
			if t.Action == "spawnWave" {
				roomWaveTrigs[roomID] = append(roomWaveTrigs[roomID], t)
			}
		}
	}
	return &openSite{
		row:           row,
		name:          name,
		firstRoomID:   rooms[0],
		census:        census,
		roomWaves:     roomWaves,
		roomWaveTrigs: roomWaveTrigs,
		inflight:      map[[2]int64]time.Time{},
		ballID:        openSiteBallID(row.SiteID),
		state: &dunSiteState{
			lastUsed: time.Now(),
			killed:   make(map[int64]bool),
			waves:    make(map[int64]map[int64]bool),
		},
		roomOffsets: map[int64]destiny.Vec3{},
		rigidIDs:    map[int64]uint32{},
		rigidSet:    map[uint32]bool{},
		clearedAt:   row.ClearedAt,
	}, nil
}

func (s *openSite) isGone() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gone
}

// roomFrame returns the offset a room of this site spawns at. Room 1 sits on
// the site's beacon; a deeper room takes whatever its first visitor computed
// (the gate landing), and every later visitor reuses it, so the site has one
// frame no matter who arrives or from where.
func (s *openSite) roomFrame(roomID int64, computed, beaconLocal destiny.Vec3, hasBeacon bool) destiny.Vec3 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if off, ok := s.roomOffsets[roomID]; ok {
		return off
	}
	off := computed
	if roomID == s.firstRoomID {
		off = destiny.Vec3{X: s.row.X, Y: s.row.Y, Z: s.row.Z}
		if hasBeacon {
			off = off.Sub(beaconLocal)
		}
	}
	s.roomOffsets[roomID] = off
	return off
}

// rigidID is the shared rigid ball ID of one scenery object of the site.
func (s *openSite) rigidID(objectID int64) uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.rigidIDs[objectID]; ok {
		return id
	}
	id := nextDunRigidID()
	s.rigidIDs[objectID] = id
	s.rigidSet[id] = true
	return id
}

// shares reports whether a ball of a pilot's room belongs to the site rather
// than to the pilot: its NPCs and its shared scenery. Those stay in space, and
// on other pilots' grids, when one visitor's room is torn down.
func (s *openSite) shares(eid uint32) bool {
	s.mu.Lock()
	rigid := s.rigidSet[eid]
	s.mu.Unlock()
	if rigid {
		return true
	}
	return globalNPCMgr != nil && globalNPCMgr.GetOpenSiteID(eid) == s.row.SiteID
}

func (s *openSite) rigidBallIDs() []uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]uint32, 0, len(s.rigidSet))
	for id := range s.rigidSet {
		out = append(out, id)
	}
	return out
}

// openSiteLocalBalls drops a site's shared balls from ids, leaving the ones
// that are only this pilot's to tear down.
func openSiteLocalBalls(siteID int64, ids []uint32) []uint32 {
	s := openSiteByID(siteID)
	if s == nil {
		return ids
	}
	out := ids[:0:0]
	for _, id := range ids {
		if !s.shares(id) {
			out = append(out, id)
		}
	}
	return out
}

// dunSiteStateFor is the kill/wave record a connection's loaded room reads:
// its open site's, else its mission's (nil for neither).
func dunSiteStateFor(ds *destinyConn, create bool) *dunSiteState {
	if s := openSiteByID(ds.dunOpenSiteID); s != nil {
		return s.state
	}
	return dunSiteFor(ds.dunMissionID, create && ds.dunMissionID > 0)
}

// ─────────────────────────────────────────────────────────────────────────────
// Beacon
// ─────────────────────────────────────────────────────────────────────────────

func openSiteBallSlim(s *openSite) (destiny.BallDesc, destiny.SlimItem, destiny.DamageState) {
	bd := destiny.BallDesc{
		EntityID: s.ballID,
		Mode:     destiny.ModeRigid,
		SubType:  6,
		Pos:      destiny.Vec3{X: s.row.X, Y: s.row.Y, Z: s.row.Z},
		Radius:   openSiteBeaconRadius,
		Name:     s.name,
	}
	slim := destiny.SlimItem{
		BallID:         int64(s.ballID),
		ItemID:         int64(s.ballID),
		TypeID:         evedb.DungeonBeaconTypeID,
		GroupID:        cynoBeaconGroupID, // const.groupBeacon
		CategoryID:     cynoBeaconCatID,
		OwnerID:        1,
		DunDescription: s.name,
	}
	if !s.row.BeaconVisible {
		// Probe-only site: the marker is a cosmic signature (group 502, type 19728), grid-local and left
		// out of ConeScan, so only a probe Analyze reports it. It stays in the starbase slices.
		slim.TypeID = signatureTypeID
		slim.GroupID = destiny.GroupCosmicSignature
	}
	return bd, slim, fullDamageState()
}

// signatureTypeID is invTypes 19728 "Deadspace Signature".
const signatureTypeID = 19728

// loadOpenSitesIntoSystem puts a freshly loaded system's site beacons back
// into its starbase slices. Their NPCs come back on the next warp-in.
func loadOpenSitesIntoSystem(addr string, ss *destiny.SolarSystem, sysID int64) {
	if ss == nil {
		return
	}
	n := 0
	for _, s := range openSitesSnapshot() {
		if s.row.SolarSystemID != sysID || s.isGone() {
			continue
		}
		bd, slim, dmg := openSiteBallSlim(s)
		ss.AddStarbase(bd, slim, dmg)
		n++
	}
	if n > 0 {
		log.Printf("[%s] open sites loaded: %d in sys=%d", addr, n, sysID)
	}
}

// presentOpenSite adds a new site's beacon to its system if that system is
// loaded right now. An unloaded system picks it up in loadOpenSitesIntoSystem.
func presentOpenSite(s *openSite) {
	if globalSysMgr == nil {
		return
	}
	ss := globalSysMgr.Get(s.row.SolarSystemID)
	if ss == nil {
		return
	}
	bd, slim, dmg := openSiteBallSlim(s)
	ss.AddStarbase(bd, slim, dmg)
	broadcastBallAdd(ss, bd, slim, dmg)
}

// ─────────────────────────────────────────────────────────────────────────────
// Clear and despawn
// ─────────────────────────────────────────────────────────────────────────────

var openSiteDB *sql.DB

// reevaluateOpenSite runs the clear rule after a kill or a wave release. A site clears when every
// required rat is dead and no wave is still to come (wavePending); it then arms its despawn. A
// cleared site that gains rats again (a trigger released a wave into it) is reopened.
func reevaluateOpenSite(siteID int64) {
	s := openSiteByID(siteID)
	if s == nil || s.fixed || openSiteDB == nil {
		return
	}
	s.evalMu.Lock()
	defer s.evalMu.Unlock()
	dead, required, cleared := s.census.Cleared(s.state.IsKilled, s.state.WaveReleased)
	if cleared {
		if room, wave, ok := s.wavePending(); ok {
			cleared = false
			log.Printf("[opensite] site %d (%s): %d/%d rats dead, holding clear for wave %d of room %d",
				siteID, s.name, dead, required, wave, room)
		}
	}
	s.mu.Lock()
	if s.gone {
		s.mu.Unlock()
		return
	}
	cleared = cleared || s.completed
	emit := false
	if cleared && s.clearedAt == 0 && !s.clearEmitted {
		s.clearEmitted = true
		emit = true
	}
	switch {
	case cleared && s.clearedAt == 0:
		s.clearedAt = time.Now().Unix()
		at := s.clearedAt
		s.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		minutes := evedb.OpenSiteClearMinutes(ctx, openSiteDB)
		if err := evedb.SetOpenSiteCleared(ctx, openSiteDB, siteID, at); err != nil {
			log.Printf("[opensite] site %d: stamp cleared: %v", siteID, err)
		}
		cancel()
		armOpenSiteDespawn(s, time.Unix(at, 0).Add(time.Duration(minutes)*time.Minute))
		log.Printf("[opensite] site %d (%s) cleared: %d/%d rats dead, despawn in %d min",
			siteID, s.name, dead, required, minutes)
		if emit && evedb.WorldEventsWanted() {
			// World event for modules (db/worldevents.go). Who cleared it
			// rides the NPCKilled events tagged with this site.
			ev := evedb.OpenSiteClearedEvent{At: time.Unix(at, 0), SiteID: siteID,
				DungeonVID: s.row.DungeonVID, SolarSystemID: s.row.SolarSystemID}
			fctx, fcancel := context.WithTimeout(context.Background(), 3*time.Second)
			ev.DungeonFactionID, ev.DEDLevel, _ = evedb.GetDungeonFactionDED(fctx, openSiteDB, s.row.DungeonVID)
			fcancel()
			evedb.EmitWorldEvent(ev)
		}
	case !cleared && s.clearedAt != 0:
		s.clearedAt = 0
		timer := s.timer
		s.timer = 0
		s.mu.Unlock()
		if timer != 0 && globalTickMgr != nil {
			globalTickMgr.CancelOnce(timer)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := evedb.SetOpenSiteCleared(ctx, openSiteDB, siteID, 0); err != nil {
			log.Printf("[opensite] site %d: lift cleared: %v", siteID, err)
		}
		cancel()
		log.Printf("[opensite] site %d (%s) reopened: %d/%d rats dead", siteID, s.name, dead, required)
	default:
		s.mu.Unlock()
	}
}

// wavePending reports a wave the site is still waiting on: one queued to a connection and not yet
// spawned, or rats left in a room's auto chain (db.AutoWaveRatsPending). Waves behind other
// triggers (gate, item placed, damage) never hold the site open. A room nobody is in keeps its
// auto chain pending, so the site waits for a visitor as it does for unkilled base rats.
func (s *openSite) wavePending() (roomID, wave int64, ok bool) {
	now := time.Now()
	s.mu.Lock()
	for k, until := range s.inflight {
		if now.Before(until) {
			s.mu.Unlock()
			return k[0], k[1], true
		}
		delete(s.inflight, k)
	}
	s.mu.Unlock()
	for room, groups := range s.roomWaves {
		released := func(w int64) bool { return s.state.WaveReleased(room, w) }
		hasRats := func(w int64) bool { return s.census.HasRats(room, w) }
		if evedb.AutoWaveRatsPending(groups, s.roomWaveTrigs[room], released, hasRats) {
			return room, evedb.NextAutoWave(groups, s.roomWaveTrigs[room], released), true
		}
	}
	return 0, 0, false
}

// openSiteWaveQueued records a wave queued to a connection for an open site, so the clear rule
// waits for it. delay is the trigger's own delay before it queues. A wave that never spawns stops
// counting after openSiteInflightGrace, and the site is re-checked then.
func openSiteWaveQueued(siteID, roomID, wave int64, delay time.Duration) {
	s := openSiteByID(siteID)
	if s == nil || wave <= 0 {
		return
	}
	wait := delay + openSiteInflightGrace
	s.mu.Lock()
	s.inflight[[2]int64{roomID, wave}] = time.Now().Add(wait)
	s.mu.Unlock()
	time.AfterFunc(wait+time.Second, func() { reevaluateOpenSite(siteID) })
}

// openSiteWaveLanded ends a wave's in-flight wait once spawnDungeonWave has marked it released,
// then re-checks the site.
func openSiteWaveLanded(siteID, roomID, wave int64) {
	s := openSiteByID(siteID)
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.inflight, [2]int64{roomID, wave})
	s.mu.Unlock()
	reevaluateOpenSite(siteID)
}

// completeOpenSite clears a site whatever its rats are doing (a completeDungeon trigger). The mark
// is held in memory, so a restart puts the site back on the census.
func completeOpenSite(s *openSite) {
	if s.fixed {
		return
	}
	s.mu.Lock()
	s.completed = true
	s.mu.Unlock()
	reevaluateOpenSite(s.row.SiteID)
}

func armOpenSiteDespawn(s *openSite, at time.Time) {
	if globalTickMgr == nil {
		return
	}
	siteID := s.row.SiteID
	id := globalTickMgr.ScheduleOnce(fmt.Sprintf("opensite-despawn-%d", siteID), at, func() {
		safego.Go("despawnOpenSite", func() { _ = despawnOpenSite(siteID, "cleared") })
	})
	s.mu.Lock()
	old := s.timer
	s.timer = id
	s.mu.Unlock()
	if old != 0 {
		globalTickMgr.CancelOnce(old)
	}
}

var openSiteDespawnMu sync.Mutex

// despawnOpenSite takes a site out of the world: its row, beacon, shared scenery on every grid that
// holds it, and its remaining NPCs (quietly: no wreck, no bounty). Wrecks and cans already dropped
// keep their own expiry. Idempotent.
func despawnOpenSite(siteID int64, reason string) error {
	openSiteDespawnMu.Lock()
	defer openSiteDespawnMu.Unlock()
	s := openSiteByID(siteID)
	if s == nil {
		return fmt.Errorf("site %d is not live", siteID)
	}
	if s.fixed {
		return fmt.Errorf("site %d is a fixed dungeon; delete its entrance in ESP Solar System", siteID)
	}
	s.mu.Lock()
	s.gone = true
	timer := s.timer
	s.timer = 0
	s.mu.Unlock()
	unregisterOpenSite(s)
	if timer != 0 && globalTickMgr != nil {
		globalTickMgr.CancelOnce(timer)
	}

	if openSiteDB != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if _, err := evedb.DeleteOpenSite(ctx, openSiteDB, siteID); err != nil {
			log.Printf("[opensite] site %d: delete row: %v", siteID, err)
		}
		if err := evedb.StampSpawnerDespawn(ctx, openSiteDB, s.row.SpawnerID, time.Now().Unix()); err != nil {
			log.Printf("[opensite] spawner %d: stamp despawn: %v", s.row.SpawnerID, err)
		}
		cancel()
	}

	npcs := 0
	if globalNPCMgr != nil {
		for _, eid := range globalNPCMgr.OpenSiteEntities(siteID) {
			if globalNPCMgr.Despawn(eid, "open site despawned") {
				npcs++
			}
		}
	}
	if globalSysMgr != nil {
		if ss := globalSysMgr.Get(s.row.SolarSystemID); ss != nil {
			ss.RemoveStarbase(s.ballID)
			broadcastBallRemove(ss, append(s.rigidBallIDs(), s.ballID)...)
		}
	}
	log.Printf("[opensite] site %d (%s) despawned in sys=%d (%s), %d NPC(s) removed",
		siteID, s.name, s.row.SolarSystemID, reason, npcs)
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Boot, the spawner pass, and placement
// ─────────────────────────────────────────────────────────────────────────────

// initOpenSites creates the tables, loads every live site (despawning the ones
// whose post-clear time ran out while the server was down, re-arming the
// rest), and starts the spawner pass.
func initOpenSites(sqldb *sql.DB, tickMgr *tick.Manager) {
	openSiteDB = sqldb
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := evedb.EnsureDungeonSpawnerTables(ctx, sqldb); err != nil {
		log.Printf("[opensite] EnsureDungeonSpawnerTables: %v", err)
		return
	}
	rows, err := evedb.ListOpenSites(ctx, sqldb)
	if err != nil {
		log.Printf("[opensite] ListOpenSites: %v", err)
		return
	}
	minutes := evedb.OpenSiteClearMinutes(ctx, sqldb)
	now := time.Now()
	loaded := 0
	for _, row := range rows {
		s, err := newOpenSite(ctx, sqldb, row)
		if err != nil {
			log.Printf("[opensite] boot: site %d dropped: %v", row.SiteID, err)
			_, _ = evedb.DeleteOpenSite(ctx, sqldb, row.SiteID)
			continue
		}
		if row.ClearedAt > 0 {
			due := time.Unix(row.ClearedAt, 0).Add(time.Duration(minutes) * time.Minute)
			if !due.After(now) {
				_, _ = evedb.DeleteOpenSite(ctx, sqldb, row.SiteID)
				_ = evedb.StampSpawnerDespawn(ctx, sqldb, row.SpawnerID, now.Unix())
				continue
			}
			registerOpenSite(s)
			armOpenSiteDespawn(s, due)
		} else {
			registerOpenSite(s)
		}
		loaded++
	}
	if loaded > 0 {
		log.Printf("[opensite] boot: %d live site(s)", loaded)
	}

	tickMgr.RegisterPeriodic("dungeon-spawner", 60, func(uint64) bool {
		if !openSitePassRunning.CompareAndSwap(false, true) {
			return true
		}
		go func() {
			defer openSitePassRunning.Store(false)
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[opensite] spawner pass panic: %v\n%s", r, debug.Stack())
				}
			}()
			runDungeonSpawnerPass(sqldb)
		}()
		return true
	})
}

var openSitePassRunning atomic.Bool

// runDungeonSpawnerPass is one minute's work: the backstop for cleared sites
// whose one-shot was lost, then a top-up of every enabled spawner.
func runDungeonSpawnerPass(sqldb *sql.DB) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	minutes := evedb.OpenSiteClearMinutes(ctx, sqldb)
	now := time.Now()
	for _, s := range openSitesSnapshot() {
		s.mu.Lock()
		cleared := s.clearedAt
		s.mu.Unlock()
		if cleared > 0 && !time.Unix(cleared, 0).Add(time.Duration(minutes)*time.Minute).After(now) {
			_ = despawnOpenSite(s.row.SiteID, "despawn backstop")
		}
	}

	spawners, err := evedb.ListDungeonSpawners(ctx, sqldb)
	if err != nil {
		log.Printf("[opensite] ListDungeonSpawners: %v", err)
		return
	}
	for _, sp := range spawners {
		if !sp.Enabled || sp.TargetActive <= 0 {
			continue
		}
		live := 0
		for _, s := range openSitesSnapshot() {
			if s.row.SpawnerID == sp.SpawnerID {
				live++
			}
		}
		if live >= sp.TargetActive {
			continue
		}
		if sp.LastDespawnAt > 0 && now.Unix()-sp.LastDespawnAt < int64(sp.RespawnDelayMinutes)*60 {
			continue
		}
		for i := 0; i < openSiteSpawnsPerPass && live < sp.TargetActive; i++ {
			msg, err := spawnOpenSite(ctx, sqldb, sp)
			if err != nil {
				log.Printf("[opensite] spawner %d (%s): %v", sp.SpawnerID, sp.Name, err)
				break
			}
			log.Printf("[opensite] spawner %d (%s): %s", sp.SpawnerID, sp.Name, msg)
			live++
		}
	}
}

// spawnOpenSite places one site for a spawner: a random candidate system
// still under the rule's per-system cap, then a spot beside one of its planets
// or, for a deadspace rule, out in open space between their orbits.
func spawnOpenSite(ctx context.Context, sqldb *sql.DB, sp evedb.DungeonSpawner) (string, error) {
	systems, err := evedb.SpawnerCandidateSystems(ctx, sqldb, sp.RegionIDs, sp.SecMin, sp.SecMax)
	if err != nil {
		return "", err
	}
	perSystem := map[int64]int{}
	sitesIn := map[int64][]evedb.SitePoint{}
	for _, s := range openSitesSnapshot() {
		if s.row.SpawnerID == sp.SpawnerID {
			perSystem[s.row.SolarSystemID]++
		}
		sitesIn[s.row.SolarSystemID] = append(sitesIn[s.row.SolarSystemID],
			evedb.SitePoint{X: s.row.X, Y: s.row.Y, Z: s.row.Z})
	}
	open := systems[:0:0]
	for _, sys := range systems {
		if perSystem[sys.SolarSystemID] < sp.MaxPerSystem {
			open = append(open, sys)
		}
	}
	if len(open) == 0 {
		return "", fmt.Errorf("no system has room: %d match the filters, all at the cap of %d", len(systems), sp.MaxPerSystem)
	}
	global, err := evedb.GlobalBeltPlanetSamples(ctx, sqldb)
	if err != nil {
		return "", err
	}
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	rng.Shuffle(len(open), func(i, j int) { open[i], open[j] = open[j], open[i] })
	if len(open) > openSiteSystemTries {
		open = open[:openSiteSystemTries]
	}
	for _, sys := range open {
		cels, err := evedb.CachedSystemCelestials(ctx, sqldb, sys.SolarSystemID)
		if err != nil {
			continue
		}
		var pos evedb.SitePoint
		var planetID int64
		var ok bool
		if sp.IsDeadspace() {
			pos, ok = evedb.PlaceSiteInDeadspace(cels, sitesIn[sys.SolarSystemID], rng)
		} else {
			pos, planetID, ok = evedb.PlaceSiteNearPlanet(cels, sitesIn[sys.SolarSystemID], global, rng)
		}
		if !ok {
			continue
		}
		row := evedb.OpenSite{
			SpawnerID: sp.SpawnerID, DungeonVID: sp.DungeonVID,
			SolarSystemID: sys.SolarSystemID, PlanetID: planetID,
			X: pos.X, Y: pos.Y, Z: pos.Z, CreatedAt: time.Now().Unix(),
			BeaconVisible: sp.BeaconVisible,
		}
		id, err := evedb.InsertOpenSite(ctx, sqldb, row)
		if err != nil {
			return "", err
		}
		row.SiteID = id
		s, err := newOpenSite(ctx, sqldb, row)
		if err != nil {
			_, _ = evedb.DeleteOpenSite(ctx, sqldb, id)
			return "", err
		}
		registerOpenSite(s)
		presentOpenSite(s)
		where := fmt.Sprintf("beside planet %d", planetID)
		if planetID == 0 {
			where = "in deadspace"
		}
		return fmt.Sprintf("site %d (%s) placed in sys=%d (sec %.2f) %s",
			id, s.name, sys.SolarSystemID, sys.Security, where), nil
	}
	return "", fmt.Errorf("no spot found in %d candidate system(s)", len(open))
}

// ─────────────────────────────────────────────────────────────────────────────
// ESP
// ─────────────────────────────────────────────────────────────────────────────

func installOpenSiteESPHooks(sqldb *sql.DB) {
	esp.OpenSiteDespawner = func(siteID int64) error {
		return despawnOpenSite(siteID, "despawned from ESP")
	}
	esp.CustomCelestialRemoved = dropFixedSite
	esp.DungeonSpawnerSpawnNow = func(spawnerID int64) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		sp, err := evedb.GetDungeonSpawner(ctx, sqldb, spawnerID)
		if err != nil {
			return "", err
		}
		if sp == nil {
			return "", fmt.Errorf("spawner %d not found", spawnerID)
		}
		return spawnOpenSite(ctx, sqldb, *sp)
	}
	esp.OpenSiteLiveState = func() map[int64]esp.OpenSiteLive {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		minutes := evedb.OpenSiteClearMinutes(ctx, sqldb)
		cancel()
		out := map[int64]esp.OpenSiteLive{}
		for _, s := range openSitesSnapshot() {
			dead, required, _ := s.census.Cleared(s.state.IsKilled, s.state.WaveReleased)
			live := esp.OpenSiteLive{Dead: dead, Required: required, Total: s.census.Count()}
			if globalNPCMgr != nil {
				live.LiveNPCs = len(globalNPCMgr.OpenSiteEntities(s.row.SiteID))
			}
			if globalSysMgr != nil {
				live.Loaded = globalSysMgr.Get(s.row.SolarSystemID) != nil
			}
			s.mu.Lock()
			if s.clearedAt > 0 {
				live.DespawnAt = s.clearedAt + int64(minutes)*60
			}
			s.mu.Unlock()
			out[s.row.SiteID] = live
		}
		return out
	}
}
