package main

// Station and stargate sentry guns, the empire-space defence battery. Every
// station in sec >= 0.1 gets four guns (above, below, +X, -X) 50 km out, of the
// station's race and owned by the station's corporation; every stargate gets
// two (above, below), of the system's sovereign faction's navy (gateSentryType).
// The guns are immobile, defensive and threat-seeded onto a pilot carrying a
// global criminal flag only for a crime committed on their own grid
// (sentryNoteCrime), released when the flag lapses or the pilot leaves the
// grid, docks, jumps or dies. A pilot who leaves is forgotten. A destroyed gun
// respawns after sentryRespawnDelay. State lives as long as the SolarSystem does.

import (
	"context"
	"database/sql"
	"log"
	"math"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/npc"
	"gurimoon/syncx"
	"gurimoon/tick"
)

const (
	// Sentry gun entity types by station race.
	sentryTypeCaldari  = 3740
	sentryTypeMinmatar = 3743
	sentryTypeAmarr    = 1194
	sentryTypeGallente = 3742

	// sentryStandoff is how far from the station centre the guns sit. Floored
	// to the station model's own radius plus a margin so the guns never end up
	// inside the hull (some station models are >40 km in radius).
	sentryStandoff       = 50_000.0
	sentryHullClearance  = 20_000.0
	sentryStationGroupID = 15 // invGroups "Station"
	sentryGateGroupID    = 10 // invGroups "Stargate"

	// sentryConcordCorpID owns a gate gun in a system whose faction has no
	// navy of its own.
	sentryConcordCorpID    = 1000125
	sentryConcordFactionID = 500006

	// sentryEngageRange is how far a sentry looks for criminals. Matched to the
	// bubble radius: beyond it the criminal isn't in the gun's world view at
	// all, so a threat seed would do nothing.
	sentryEngageRange = destiny.BubbleRadius

	// sentryThreatSeed pins the gun onto the criminal above any incidental
	// threat, but below the CONCORD dispatch seed.
	sentryThreatSeed = 500_000.0

	// sentryMinSecurity is the rounded system security at or above which station
	// guns exist: low-sec and high-sec, never null-sec (conquerable stations).
	sentryMinSecurity = 0.1
)

// sentryRespawnDelay is how long a destroyed gun stays down. server_settings
// sentry.respawnMinutes, default 15.
func sentryRespawnDelay() time.Duration {
	return evedb.CachedSettingDuration("sentry.respawnMinutes", time.Minute, 15, 0, 24*60)
}

// sentryPost is one gun emplacement: a fixed position plus the spawn recipe to
// (re)create the gun that belongs there.
type sentryPost struct {
	// kind is "station" or "gate"; stationID/stationName name the anchor
	// either way (a gate's itemID and name, for a gate post).
	kind        string
	stationID   int64
	stationName string
	bearing     string // "above" / "below" / "right" / "left", logging only
	pos         destiny.Vec3

	typeID    int64
	typeName  string
	corpID    int64
	factionID int64
	stats     *evedb.NPCStats
	hp        *npc.NPCHPInit
	weapons   *evedb.NPCWeaponProfile

	// entityID is the live gun, 0 while it is down. deadSince is when the
	// watch noticed it destroyed and starts the respawn clock.
	entityID  uint32
	deadSince time.Time

	// engaged are the criminal ship entity IDs this gun currently has a
	// sentry threat seed against.
	engaged map[uint32]bool
}

// sentryGroup holds every gun emplacement in one solar system.
type sentryGroup struct {
	mu    sync.Mutex
	posts []*sentryPost
	// wanted is each anchor's grid memory: anchor stationID -> charID -> unix
	// second the pilot's crime there was last noted. Guns fire only on a criminal
	// whose crime they saw; leaving the grid wipes the anchor's memory of them.
	wanted map[int64]map[int64]int64
}

// sentryNoteCrime tells the guns of every anchor whose grid holds pos that
// charID just committed a crime there (actOnCrime, an aid crime, a mine hit).
// The pilot's global criminal flag is set separately; the guns of other grids
// ignore it.
func sentryNoteCrime(ss *destiny.SolarSystem, charID int64, pos destiny.Vec3) {
	if ss == nil || charID <= 0 {
		return
	}
	g, ok := sentryGroups.Load(ss.ID)
	if !ok {
		return
	}
	group := g
	now := time.Now().Unix()
	group.mu.Lock()
	defer group.mu.Unlock()
	for _, post := range group.posts {
		if pos.Sub(post.pos).LenSq() > sentryEngageRange*sentryEngageRange {
			continue
		}
		if group.wanted == nil {
			group.wanted = map[int64]map[int64]int64{}
		}
		w := group.wanted[post.stationID]
		if w == nil {
			w = map[int64]int64{}
			group.wanted[post.stationID] = w
		}
		w[charID] = now
	}
}

// sentryGroups maps solarSystemID to *sentryGroup for systems that are loaded
// and have station guns.
var sentryGroups syncx.Map[int64, *sentryGroup]

// sentryTypeCache caches the per-type spawn data (stats, HP, weapon profile) of
// the four gun types so repeated system loads don't re-query dogma.
var sentryTypeCache syncx.Map[int64, *sentryTypeData] // typeID to *sentryTypeData

type sentryTypeData struct {
	stats   *evedb.NPCStats
	hp      *npc.NPCHPInit
	weapons *evedb.NPCWeaponProfile
}

// sentryTypeForRace maps invTypes.raceID of the station to its gun type.
func sentryTypeForRace(raceID int64) int64 {
	switch raceID {
	case 1:
		return sentryTypeCaldari
	case 2:
		return sentryTypeMinmatar
	case 4:
		return sentryTypeAmarr
	case 8:
		return sentryTypeGallente
	default:
		return 0
	}
}

// sentryRaceForFaction maps an empire factionID back to a raceID, for stations
// whose invTypes row carries no raceID.
func sentryRaceForFaction(factionID int64) int64 {
	switch factionID {
	case 500001:
		return 1 // Caldari State
	case 500002:
		return 2 // Minmatar Republic
	case 500003:
		return 4 // Amarr Empire
	case 500004:
		return 8 // Gallente Federation
	case 500007:
		return 4 // Ammatar Mandate: Amarr guns
	case 500008:
		return 4 // Khanid Kingdom: Amarr guns
	case 500011:
		return 1 // Mordu's Legion: Caldari guns
	default:
		return 0
	}
}

// gateSentryType is the gun a stargate gets under a sovereign faction: the
// faction's own race's gun flying for its navy, or, for a faction with no navy
// gun (CONCORD, the Syndicate, the pirates), a CONCORD-owned Caldari gun. ok
// is false under no faction. Pure, for the test.
func gateSentryType(factionID int64) (typeID int64, concordOwned, ok bool) {
	if factionID == 0 {
		return 0, false, false
	}
	if race := sentryRaceForFaction(factionID); race != 0 {
		return sentryTypeForRace(race), false, true
	}
	return sentryTypeCaldari, true, true
}

// sentryFactionForRace is the inverse: the empire faction a gun flies for when
// its owner corp is not in crpNPCCorporations.
func sentryFactionForRace(raceID int64) int64 {
	switch raceID {
	case 1:
		return 500001
	case 2:
		return 500002
	case 4:
		return 500003
	case 8:
		return 500004
	default:
		return 0
	}
}

// sentrySecurityQualifies reports whether a system's security band gets station
// guns (low-sec and high-sec; null-sec has conquerable stations instead).
func sentrySecurityQualifies(sec float64) bool {
	return math.Round(sec*10)/10 >= sentryMinSecurity
}

// loadSentryTypeData fetches (and caches) the spawn data for a gun type.
func loadSentryTypeData(ctx context.Context, sqldb *sql.DB, typeID int64) *sentryTypeData {
	if v, ok := sentryTypeCache.Load(typeID); ok {
		return v
	}
	stats, err := evedb.GetNPCStats(ctx, sqldb, typeID)
	if err != nil {
		log.Printf("[sentry] GetNPCStats typeID=%d: %v", typeID, err)
		return nil
	}
	td := &sentryTypeData{stats: stats}
	if ehp, err := evedb.GetEntityHPByType(ctx, sqldb, typeID); err == nil {
		td.hp = &npc.NPCHPInit{
			ShieldHP: ehp.ShieldHP, ArmorHP: ehp.ArmorHP,
			StructureHP: ehp.StructureHP, ShieldTau: ehp.ShieldTau,
			ShieldRes: ehp.ShieldRes, ArmorRes: ehp.ArmorRes, HullRes: ehp.HullRes,
		}
	} else {
		log.Printf("[sentry] GetEntityHPByType typeID=%d: %v", typeID, err)
	}
	if wp, err := evedb.GetNPCWeaponProfile(ctx, sqldb, typeID); err == nil {
		td.weapons = wp
	} else {
		log.Printf("[sentry] GetNPCWeaponProfile typeID=%d: %v", typeID, err)
	}
	sentryTypeCache.Store(typeID, td)
	return td
}

// spawnStationSentries builds the gun emplacements for every station in a
// freshly loaded low-sec/high-sec system and spawns them. Called from
// registerUndock's first-player-in-system path, after the celestial bubbles are
// seeded so the guns join the station's grid.
func spawnStationSentries(ss *destiny.SolarSystem, celestials []evedb.CelestialEntity,
	sqldb *sql.DB, npcMgr *npc.NPCManager, addr string) {

	if ss == nil || sqldb == nil || npcMgr == nil {
		return
	}
	if !sentrySecurityQualifies(ss.Security) {
		return
	}
	if _, loaded := sentryGroups.Load(ss.ID); loaded {
		return // already emplaced for this system instance
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	group := &sentryGroup{}
	stations := 0
	for _, cel := range celestials {
		if cel.GroupID != sentryStationGroupID {
			continue
		}

		corpID := cel.OwnerID
		raceID := evedb.GetTypeRaceID(ctx, sqldb, cel.TypeID)

		// Faction: prefer the owning corp's own faction, fall back to the
		// station race's empire.
		var factionID int64
		if npcMgr.Standings != nil && corpID > 0 {
			factionID = npcMgr.Standings.CorpFaction(corpID)
		}
		if raceID == 0 {
			raceID = sentryRaceForFaction(factionID)
		}
		if factionID == 0 {
			factionID = sentryFactionForRace(raceID)
		}
		typeID := sentryTypeForRace(raceID)
		if typeID == 0 {
			log.Printf("[%s] [sentry] station %q (ID=%d typeID=%d) has no resolvable race — no guns",
				addr, cel.Name, cel.ItemID, cel.TypeID)
			continue
		}
		// Player-owned stations don't exist in empire space, but be defensive:
		// without an owner the guns fly for the station race's faction navy.
		if corpID <= 0 {
			corpID = evedb.GetFactionCorpID(ctx, sqldb, factionID)
		}

		td := loadSentryTypeData(ctx, sqldb, typeID)
		if td == nil {
			continue
		}

		// Stand off 50 km, or clear of the hull for the big station models.
		dist := sentryStandoff
		if d := cel.Radius + sentryHullClearance; d > dist {
			dist = d
		}
		center := destiny.Vec3{X: cel.X, Y: cel.Y, Z: cel.Z}
		// Bearings around the station model: above (+Y), below (-Y), right (+X) and
		// left (-X); Y is up in world space.
		bearings := []struct {
			name   string
			offset destiny.Vec3
		}{
			{"above", destiny.Vec3{Y: dist}},
			{"below", destiny.Vec3{Y: -dist}},
			{"right", destiny.Vec3{X: dist}},
			{"left", destiny.Vec3{X: -dist}},
		}
		for _, b := range bearings {
			post := &sentryPost{
				kind:        "station",
				stationID:   cel.ItemID,
				stationName: cel.Name,
				bearing:     b.name,
				pos: destiny.Vec3{
					X: center.X + b.offset.X,
					Y: center.Y + b.offset.Y,
					Z: center.Z + b.offset.Z,
				},
				typeID:    typeID,
				typeName:  td.stats.TypeName,
				corpID:    corpID,
				factionID: factionID,
				stats:     td.stats,
				hp:        td.hp,
				weapons:   td.weapons,
				engaged:   map[uint32]bool{},
			}
			group.posts = append(group.posts, post)
		}
		stations++
	}

	// Stargates: two posts each, the recipe resolved once per system from
	// the sovereign faction (a gate carries neither race nor owner).
	gates := 0
	var gateTD *sentryTypeData
	var gateTypeID, gateCorpID, gateFactionID int64
	gateResolved := false
	for _, cel := range celestials {
		if cel.GroupID != sentryGateGroupID {
			continue
		}
		if !gateResolved {
			gateResolved = true
			factionID, err := evedb.GetSystemFactionID(ctx, sqldb, int64(ss.ID))
			if err != nil {
				log.Printf("[%s] [sentry] system %d: faction lookup failed, no gate guns: %v", addr, ss.ID, err)
			}
			typeID, concordOwned, ok := gateSentryType(factionID)
			if !ok {
				log.Printf("[%s] [sentry] system %d has no sovereign faction — no gate guns", addr, ss.ID)
			} else {
				gateTypeID, gateFactionID = typeID, factionID
				if concordOwned {
					gateCorpID, gateFactionID = sentryConcordCorpID, sentryConcordFactionID
				} else if gateCorpID = evedb.GetFactionCorpID(ctx, sqldb, factionID); gateCorpID <= 0 {
					gateCorpID = sentryConcordCorpID
				}
				gateTD = loadSentryTypeData(ctx, sqldb, gateTypeID)
			}
		}
		if gateTD == nil {
			continue
		}
		dist := sentryStandoff
		if d := cel.Radius + sentryHullClearance; d > dist {
			dist = d
		}
		// Above and below only: two posts bracketing the gate vertically cover a
		// jump-in from either side.
		for _, b := range []struct {
			name   string
			offset destiny.Vec3
		}{{"above", destiny.Vec3{Y: dist}}, {"below", destiny.Vec3{Y: -dist}}} {
			group.posts = append(group.posts, &sentryPost{
				kind:        "gate",
				stationID:   cel.ItemID,
				stationName: cel.Name,
				bearing:     b.name,
				pos:         destiny.Vec3{X: cel.X + b.offset.X, Y: cel.Y + b.offset.Y, Z: cel.Z + b.offset.Z},
				typeID:      gateTypeID,
				typeName:    gateTD.stats.TypeName,
				corpID:      gateCorpID,
				factionID:   gateFactionID,
				stats:       gateTD.stats,
				hp:          gateTD.hp,
				weapons:     gateTD.weapons,
				engaged:     map[uint32]bool{},
			})
		}
		gates++
	}

	if len(group.posts) == 0 {
		return
	}
	if _, loaded := sentryGroups.LoadOrStore(ss.ID, group); loaded {
		return // lost the race with another loader
	}

	spawned := 0
	group.mu.Lock()
	for _, post := range group.posts {
		if spawnSentryPost(ss, npcMgr, post) {
			spawned++
		}
	}
	group.mu.Unlock()

	log.Printf("[%s] [sentry] system %d (sec=%.2f): %d guns up around %d stations, %d gates",
		addr, ss.ID, ss.Security, spawned, stations, gates)
}

// spawnSentryPost puts a gun into space at its emplacement. Caller holds the
// group lock. Returns true when the gun is live.
func spawnSentryPost(ss *destiny.SolarSystem, npcMgr *npc.NPCManager, post *sentryPost) bool {
	// maxVelocity 0 (a turret, never handed a movement command) and defensive:
	// blind until threat is seeded, then highest threat first, with its type's EWAR.
	entity := npcMgr.SpawnNPC(ss, post.typeID, post.typeName, post.pos,
		post.stats.Mass, post.stats.Agility, 0, post.stats.Radius,
		npc.StanceDefensive, post.hp, post.weapons, post.corpID, false, 0, post.factionID, false)
	if entity == nil {
		return false
	}
	npc.SetGroupCategory(entity, post.stats.GroupID, post.stats.CategoryID)
	npcMgr.NotifySpawn(entity, ss)

	post.entityID = entity.ID
	post.deadSince = time.Time{}
	post.engaged = map[uint32]bool{}
	log.Printf("[sentry] %s gun up %s of %q (%s %d) in system %d — entity %d corp %d faction %d",
		post.typeName, post.bearing, post.stationName, post.kind, post.stationID, ss.ID,
		entity.ID, post.corpID, post.factionID)
	return true
}

// sentryForgetSystem drops all gun state for a torn-down system. The NPC
// records themselves are removed by NPCManager.RemoveSystem.
func sentryForgetSystem(sysID int64) {
	sentryGroups.Delete(sysID)
}

// startSentryWatch arms the periodic that drives station guns: seeding threat
// onto criminals in range, releasing them when their timer lapses, and
// respawning destroyed guns.
func startSentryWatch(tickMgr *tick.Manager) {
	// Own lane, so gun engagement keeps its 5 s cadence while the default
	// lane works through a slow sweep. Gun state is under group.mu.
	tickMgr.RegisterPeriodicLane("sentry-watch", "sentry-watch", 5, func(uint64) bool {
		npcMgr := globalNPCMgr
		if npcMgr == nil || globalSysMgr == nil {
			return true
		}
		sentryGroups.Range(func(k int64, v *sentryGroup) bool {
			sentryWatchSystem(k, v, npcMgr)
			return true
		})
		return true
	})
}

// sentryWatchSystem runs one system's gun upkeep.
func sentryWatchSystem(sysID int64, group *sentryGroup, npcMgr *npc.NPCManager) {
	ss := globalSysMgr.Get(sysID)
	if ss == nil {
		sentryGroups.Delete(sysID) // system torn down
		return
	}

	// Live criminal ships: pilots with an unexpired global criminal flag here.
	// Docking, jumping, dying or a lapsed timer drops them, disengaging the guns.
	// Each anchor's guns then narrow this to the pilots whose crime they saw.
	criminals := globalCriminalsInSystem(sysID)
	type criminalShip struct {
		entityID uint32
		charID   int64
		pos      destiny.Vec3
	}
	var targets []criminalShip
	if len(criminals) > 0 {
		for _, p := range ss.GetAllPlayersInSystem() {
			charID := p.Slim.CharID
			if charID <= 0 || !criminals[charID] || p.IsDestroyed() {
				continue
			}
			targets = append(targets, criminalShip{entityID: p.ID, charID: charID, pos: p.Pos()})
		}
	}

	now := time.Now()
	group.mu.Lock()
	defer group.mu.Unlock()

	// Grid memory: an anchor forgets a pilot who is no longer a criminal, or whose
	// ship is out of range of every gun at that anchor.
	inAnchorRange := map[int64]map[int64]bool{}
	for _, post := range group.posts {
		for _, t := range targets {
			if t.pos.Sub(post.pos).LenSq() <= sentryEngageRange*sentryEngageRange {
				if inAnchorRange[post.stationID] == nil {
					inAnchorRange[post.stationID] = map[int64]bool{}
				}
				inAnchorRange[post.stationID][t.charID] = true
			}
		}
	}
	for anchor, w := range group.wanted {
		for charID := range w {
			if !criminals[charID] || !inAnchorRange[anchor][charID] {
				delete(w, charID)
			}
		}
		if len(w) == 0 {
			delete(group.wanted, anchor)
		}
	}

	for _, post := range group.posts {
		// Gun destroyed? Start (or continue) the respawn clock.
		if post.entityID != 0 && ss.GetEntity(post.entityID) == nil {
			log.Printf("[sentry] gun %s of %q (system %d) destroyed — respawn in %s",
				post.bearing, post.stationName, sysID, sentryRespawnDelay())
			post.entityID = 0
			post.deadSince = now
			post.engaged = map[uint32]bool{}
		}
		if post.entityID == 0 {
			if post.deadSince.IsZero() {
				post.deadSince = now
			} else if now.Sub(post.deadSince) >= sentryRespawnDelay() {
				spawnSentryPost(ss, npcMgr, post)
			}
			continue
		}

		// Seed threat on every criminal in range whose crime this grid saw.
		inRange := make(map[uint32]bool, len(targets))
		for _, t := range targets {
			if t.pos.Sub(post.pos).LenSq() > sentryEngageRange*sentryEngageRange {
				continue
			}
			if _, seen := group.wanted[post.stationID][t.charID]; !seen {
				continue
			}
			inRange[t.entityID] = true
			if post.engaged[t.entityID] {
				continue
			}
			post.engaged[t.entityID] = true
			npcMgr.NotifyDamage(post.entityID, t.entityID, sentryThreatSeed)
			log.Printf("[sentry] %q gun %s (entity %d) engaging criminal charID=%d (entity %d)",
				post.stationName, post.bearing, post.entityID, t.charID, t.entityID)
		}

		// Release anyone who is no longer a criminal in range.
		for id := range post.engaged {
			if inRange[id] {
				continue
			}
			delete(post.engaged, id)
			npcMgr.ClearThreat(post.entityID, id)
			log.Printf("[sentry] %q gun %s (entity %d) disengaging entity %d",
				post.stationName, post.bearing, post.entityID, id)
		}
	}
}
