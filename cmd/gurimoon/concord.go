package main

// CONCORD response, the highsec police function. An unlawful shot on a player
// in a system of security >= concordMinSec spawns a response fleet (DED corp
// 1000137, CONCORD faction 500006) on the aggressor after a security-dependent
// delay; the fleet lingers after the criminal is gone, then despawns silently.
// Legality is seclaw.ShotIsCrime.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/npc"
	"gurimoon/seclaw"
	"gurimoon/server"
	"gurimoon/syncx"
	"gurimoon/tick"
)

const (
	concordCorpDED   = 1000137 // DED, Directive Enforcement Department
	concordFactionID = 500006  // CONCORD Assembly
	concordMinSec    = 0.45    // no CONCORD response below this (rounds to 0.5)
	// Threat seeds pin CONCORD onto criminals: the pilot the fleet was
	// dispatched for outranks any other criminal that wanders on-grid.
	concordThreatSeed          = 1_000_000.0
	concordSecondaryThreatSeed = 100_000.0
)

// concordFleet is the response composition: the group-301 (Concord Drone) NPC
// entity types, which carry full entity/weapon dogma. The 1896/1904/1912
// Concord Police hulls are bare ship types with no NPC attributes.
var concordFleet = []struct {
	typeID int64
	count  int
}{
	{3863, 3},  // CONCORD Police Officer (frigate)
	{3885, 2},  // CONCORD Police Captain (cruiser)
	{11125, 1}, // CONCORD Police Commander (battleship)
}

// concordResponse tracks one active (scheduled or spawned) response fleet.
type concordResponse struct {
	mu        sync.Mutex
	sysID     int64
	spawned   bool
	entityIDs []uint32
	// seeded records which criminal SHIP entities the fleet already has
	// threat against, so a pilot is only seeded once per hull (a fresh
	// capsule after a ship kill counts as a new entity and gets re-seeded).
	seeded map[uint32]bool
	// goneSince is when the dispatch target stopped being a live flagged
	// criminal in this system; zero while they are still present.
	goneSince time.Time
}

// concordActive maps criminal charID to *concordResponse, from crime detection
// until the fleet despawns. It doubles as the CONCORD-flagged marker that keeps
// retaliation legal and prevents duplicate fleets.
var concordActive syncx.Map[int64, *concordResponse]

// concordDelayDefault is the response delay per rounded security band,
// [1.0/0.9, 0.8, 0.7, 0.6, 0.5], in seconds. server_settings
// concord.responseDelaySec overrides it as five comma-separated numbers.
var concordDelayDefault = []float64{5, 8, 10, 12, 16}

// concordDelayForSecurity returns the response delay for a system security
// level, or ok=false when the system is below CONCORD jurisdiction.
func concordDelayForSecurity(sec float64) (time.Duration, bool) {
	if sec < concordMinSec {
		return 0, false
	}
	d := evedb.CachedSettingFloats("concord.responseDelaySec", concordDelayDefault, 0, 600)
	var s float64
	switch r := math.Round(sec*10) / 10; {
	case r >= 0.9:
		s = d[0]
	case r >= 0.8:
		s = d[1]
	case r >= 0.7:
		s = d[2]
	case r >= 0.6:
		s = d[3]
	default:
		s = d[4]
	}
	return time.Duration(s * float64(time.Second)), true
}

// concordLinger is how long the fleet stays on scene after its criminal is
// gone (dead, docked, jumped out, or flag lapsed). While the criminal is still
// alive in system the fleet never leaves. server_settings concord.lingerSec.
func concordLinger() time.Duration {
	return evedb.CachedSettingDuration("concord.lingerSec", time.Second, 90, 0, 3600)
}

// concordIsFlagged reports whether the character has an active (scheduled or
// spawned) CONCORD response; shooting them is legal.
func concordIsFlagged(charID int64) bool {
	_, ok := concordActive.Load(charID)
	return ok
}

// killFactsFromEntities lifts the plain values the law reasons about out of
// two in-space entities. seclaw knows nothing of destiny, so the module's
// abstract combat tier (no grid, no entities) prices a kill through the same
// function.
func killFactsFromEntities(attacker, target *destiny.Entity,
	ss *destiny.SolarSystem, pod bool) seclaw.KillFacts {

	return seclaw.KillFacts{
		KillerCharID:     attacker.Slim.CharID,
		KillerCorpID:     attacker.Slim.CorpID,
		KillerAllianceID: attacker.Slim.AllianceID,
		KillerShipTypeID: attacker.Slim.TypeID,
		VictimCharID:     target.Slim.CharID,
		VictimCorpID:     target.Slim.CorpID,
		VictimAllianceID: target.Slim.AllianceID,
		SystemID:         ss.ID,
		Security:         ss.Security,
		// The victim's own record, read off the slimItem the client paints Criminal
		// from, so server and overview agree about who is shootable.
		VictimOutlaw: target.Slim.SecurityStatus <= seclaw.OutlawStatus,
		Pod:          pod,
	}
}

// seclawDeps supplies the live lookups the exemption ladder needs.
// WarShotIsLegal mirrors the client's IsWarInHostileState arithmetic
// (db.War.Hostile) so CONCORD and the overview agree. The flagged check covers
// a CONCORD response inbound on the victim and a criminal timer of their own.
func seclawDeps(sqldb *sql.DB) seclaw.Deps {
	return seclaw.Deps{
		WarShotIsLegal: func(atkCorp, atkAlliance, vicCorp, vicAlliance int64) bool {
			return WarShotIsLegal(sqldb, atkCorp, atkAlliance, vicCorp, vicAlliance)
		},
		VictimIsLegalTgt: func(sysID, victimCharID int64) bool {
			return concordIsFlagged(victimCharID) || isGlobalCriminal(sysID, victimCharID)
		},
		KillRightHeld: killRightHeldCached,
		// The per-victim half of the crimewatch table: a thief at a can has aggressed
		// the owner and the owner's corp (recordLootTheft), so shooting them is lawful.
		VictimAggressedKiller: victimEngaged,
		SentryQualifies:       sentrySecurityQualifies,
	}
}

// maybeTriggerConcordResponse is called from the player->player damage paths
// (turret, missile, drone, ewar). attacker is the aggressor's in-space entity,
// target the damaged player. The verdict is seclaw.ShotIsCrime, the same
// ladder that prices the kill; this is the only site that acts on it.
func maybeTriggerConcordResponse(attacker *destiny.Entity, target *destiny.Entity,
	ss *destiny.SolarSystem, sqldb *sql.DB, npcMgr *npc.NPCManager) {

	if attacker == nil || target == nil || ss == nil || !target.IsPlayer {
		return
	}
	// The facts are held: the aggression charge below prices the same shot the
	// police react to, so both read one copy.
	facts := killFactsFromEntities(attacker, target, ss, false)
	crime, _ := seclaw.ShotIsCrime(facts, seclawDeps(sqldb))
	if !crime {
		return
	}
	criminalCharID := attacker.Slim.CharID

	// Opening fire on a neutral is the crime (noteAggressionCrime), charged ahead
	// of actOnCrime's jurisdiction test: low-sec has no police but keeps the record.
	noteAggressionCrime(attacker, facts)
	actOnCrime(attacker, criminalCharID, ss, sqldb, npcMgr)
}

// maybeTriggerStructureConcord is maybeTriggerConcordResponse for a shot on a
// player-owned structure: the victim is the owning corporation, identified
// by the structure's slim, and there is no pilot whose own timer could make
// the shot legal. Same ladder, same response.
func maybeTriggerStructureConcord(attacker *destiny.Entity, target *destiny.Entity,
	ss *destiny.SolarSystem, sqldb *sql.DB, npcMgr *npc.NPCManager) {

	if attacker == nil || target == nil || ss == nil || !attacker.IsPlayer {
		return
	}
	// Shooting a player structure is a PvP act whatever the verdict.
	stampPvPEntities(attacker, nil)
	facts := killFactsFromEntities(attacker, target, ss, false)
	facts.VictimCharID = 0
	facts.VictimIsStructure = true
	crime, _ := seclaw.ShotIsCrime(facts, seclawDeps(sqldb))
	if !crime {
		return
	}
	actOnCrime(attacker, attacker.Slim.CharID, ss, sqldb, npcMgr)
}

// actOnCrime is the half of a CONCORD response that follows a guilty verdict:
// the global criminal flag, then (in police space) one response fleet per
// criminal.
func actOnCrime(attacker *destiny.Entity, criminalCharID int64, ss *destiny.SolarSystem, sqldb *sql.DB, npcMgr *npc.NPCManager) {

	// Global criminal flag: Neocom countdown, overview flashing, legal target for
	// all, and the station guns open up.
	markGlobalCriminal(ss, criminalCharID)
	if attacker != nil {
		sentryNoteCrime(ss, criminalCharID, attacker.Pos())
	}
	dispatchConcord(attacker, criminalCharID, ss, sqldb, npcMgr)
}

// concordShouldFollow is concordFollow's rule: dispatch unless a fleet for
// this pilot is already active in this system (fleetSysID 0 = no fleet).
func concordShouldFollow(fleetSysID, hereSysID int64) bool {
	return fleetSysID != hereSysID
}

// concordFollow meets a pilot who arrives in space (undock, gate, login, ship
// swap) still carrying a global criminal flag: CONCORD destroys any new ship a
// criminal undocks. A fleet left in another system is withdrawn first; a fleet
// still here is re-aimed at the arriving hull. Never refreshes the flag.
func concordFollow(ss *destiny.SolarSystem, e *destiny.Entity) {
	if ss == nil || e == nil || !e.IsPlayer || globalSysMgr == nil || globalNPCMgr == nil {
		return
	}
	charID := e.Slim.CharID
	if !isGlobalCriminal(ss.ID, charID) {
		return
	}
	var fleetSys int64
	if v, ok := concordActive.Load(charID); ok {
		resp := v
		fleetSys = resp.sysID
		if !concordShouldFollow(fleetSys, ss.ID) {
			// The same ship ID comes back after a dock; the watch seeds a hull once.
			resp.mu.Lock()
			delete(resp.seeded, e.ID)
			resp.mu.Unlock()
			return
		}
	}
	if fleetSys != 0 {
		if old := globalSysMgr.Get(fleetSys); old != nil {
			despawnConcordResponse(old, globalDB, globalNPCMgr, charID)
		} else {
			concordActive.Delete(charID)
		}
	}
	dispatchConcord(e, charID, ss, globalDB, globalNPCMgr)
}

// dispatchConcord schedules one response fleet on a flagged criminal in police
// space. The first dispatch wins; later calls are no-ops while it is active.
func dispatchConcord(attacker *destiny.Entity, criminalCharID int64, ss *destiny.SolarSystem, sqldb *sql.DB, npcMgr *npc.NPCManager) {
	delay, ok := concordDelayForSecurity(ss.Security)
	if !ok {
		return // low-sec: flagged, but no CONCORD response
	}
	// One fleet per criminal: first crime wins, later shots are no-ops.
	if _, loaded := concordActive.LoadOrStore(criminalCharID, &concordResponse{sysID: ss.ID}); loaded {
		return
	}

	crimePos := attacker.Pos()
	criminalEntityID := attacker.ID
	sysID := ss.ID
	log.Printf("[concord] criminal act: charID=%d (entity %d) in system %d sec=%.2f — response in %s",
		criminalCharID, criminalEntityID, sysID, ss.Security, delay)

	if globalTickMgr == nil {
		concordActive.Delete(criminalCharID)
		return
	}
	globalTickMgr.ScheduleOnce(fmt.Sprintf("concord-spawn-%d", criminalCharID),
		time.Now().Add(delay), func() {
			spawnConcordResponse(ss, sqldb, npcMgr, criminalCharID, criminalEntityID, crimePos)
		})
}

// spawnConcordResponse spawns the response fleet at the criminal's current
// position (or the crime scene if they already left), aims it at the
// criminal, and arms the linger/despawn timer.
func spawnConcordResponse(ss *destiny.SolarSystem, sqldb *sql.DB, npcMgr *npc.NPCManager,
	criminalCharID int64, criminalEntityID uint32, crimePos destiny.Vec3) {

	resp, ok := concordActive.Load(criminalCharID)
	if !ok {
		return
	}
	response := resp

	// System already empty and torn down: nothing to police. Player entities, not
	// connections: a rolebot projection is a player ship with no Conn.
	if len(ss.GetAllPlayerEntities()) == 0 {
		concordActive.Delete(criminalCharID)
		return
	}

	// Land on the criminal if they're still in space here, else on the scene.
	spawnCenter := crimePos
	criminal := ss.GetEntity(criminalEntityID)
	if criminal != nil && criminal.IsPlayer && criminal.Slim.CharID == criminalCharID {
		spawnCenter = criminal.Pos()
	} else {
		criminal = nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var spawned []uint32
	for _, wave := range concordFleet {
		// Land in brawling range: far enough not to bump the criminal, close
		// enough that they can shoot back with short-range guns.
		ids := spawnNPCWave(ctx, ss, sqldb, npcMgr, wave.typeID, wave.count, spawnCenter,
			2_000, 6_000, concordCorpDED, concordFactionID)
		for _, id := range ids {
			if criminal != nil {
				npcMgr.NotifyDamage(id, criminalEntityID, concordThreatSeed)
			}
		}
		spawned = append(spawned, ids...)
	}

	// The police jam the criminal's cloak on arrival.
	breakCloakForPursuit(criminal)

	if len(spawned) == 0 {
		concordActive.Delete(criminalCharID)
		return
	}
	response.mu.Lock()
	response.entityIDs = spawned
	response.spawned = true
	response.seeded = map[uint32]bool{}
	if criminal != nil {
		response.seeded[criminalEntityID] = true
	}
	response.mu.Unlock()

	// Deliver the fleet to everyone on grid.
	deliverNPCFleet(ss, spawned)

	log.Printf("[concord] response spawned in system %d: %d ships on charID=%d (onGrid=%v)",
		ss.ID, len(spawned), criminalCharID, criminal != nil)
	// Departure is decided by concordWatchTick, not a fixed timer: the fleet
	// stays as long as its criminal is alive and flagged in system.
}

// startConcordWatch arms the periodic that keeps active response fleets
// honest: re-aiming them at every criminal on grid, and holding them on
// scene until their dispatch target is dead, gone, or no longer flagged.
func startConcordWatch(tickMgr *tick.Manager, sqldb *sql.DB) {
	// Own lane: a response fleet re-aims every 5 s and must not wait behind an
	// hourly sweep. Every response carries its own lock; nothing is shared.
	tickMgr.RegisterPeriodicLane("concord-watch", "concord-watch", 5, func(uint64) bool {
		npcMgr := globalNPCMgr
		if npcMgr == nil || globalSysMgr == nil {
			return true
		}
		concordActive.Range(func(k int64, v *concordResponse) bool {
			concordWatchOne(k, v, sqldb, npcMgr)
			return true
		})
		return true
	})
}

// concordWatchOne runs one response fleet's upkeep.
func concordWatchOne(criminalCharID int64, response *concordResponse, sqldb *sql.DB, npcMgr *npc.NPCManager) {
	response.mu.Lock()
	spawned := response.spawned
	sysID := response.sysID
	fleet := append([]uint32(nil), response.entityIDs...)
	response.mu.Unlock()

	if !spawned {
		return // still counting down to the spawn
	}
	ss := globalSysMgr.Get(sysID)
	if ss == nil {
		concordActive.Delete(criminalCharID) // system torn down
		return
	}

	// Prune fleet members the criminal (or anyone) destroyed.
	alive := fleet[:0]
	for _, id := range fleet {
		if ss.GetEntity(id) != nil {
			alive = append(alive, id)
		}
	}
	if len(alive) == 0 {
		log.Printf("[concord] fleet dispatched for charID=%d wiped out — response over", criminalCharID)
		concordActive.Delete(criminalCharID)
		return
	}
	response.mu.Lock()
	response.entityIDs = append([]uint32(nil), alive...)
	response.mu.Unlock()

	// Shooting back at CONCORD costs the record once per pilot per response, read
	// here (5 s cadence) rather than per shot, which would floor a pilot in a second.
	chargePoliceAggressors(sqldb, ss, npcMgr, alive)

	// Every globally-flagged criminal in the system is a valid CONCORD
	// target, not just the pilot the fleet was dispatched for.
	criminals := globalCriminalsInSystem(sysID)
	primaryPresent := false
	for _, p := range ss.GetAllPlayersInSystem() {
		charID := p.Slim.CharID
		if charID <= 0 || !criminals[charID] || p.IsDestroyed() {
			continue
		}
		if charID == criminalCharID {
			primaryPresent = true
		}
		response.mu.Lock()
		fresh := !response.seeded[p.ID]
		if fresh {
			response.seeded[p.ID] = true
		}
		response.mu.Unlock()
		if !fresh {
			continue
		}
		seed := concordSecondaryThreatSeed
		if charID == criminalCharID {
			seed = concordThreatSeed
		}
		for _, id := range alive {
			npcMgr.NotifyDamage(id, p.ID, seed)
		}
		log.Printf("[concord] fleet for charID=%d engaging criminal charID=%d (entity %d, threat %.0f)",
			criminalCharID, charID, p.ID, seed)
	}

	// Hold station while the dispatch target is still a live criminal here;
	// only start the departure clock once they are gone.
	response.mu.Lock()
	if primaryPresent {
		response.goneSince = time.Time{}
		response.mu.Unlock()
		return
	}
	if response.goneSince.IsZero() {
		response.goneSince = time.Now()
		response.mu.Unlock()
		return
	}
	leave := time.Since(response.goneSince) >= concordLinger()
	response.mu.Unlock()
	if leave {
		despawnConcordResponse(ss, sqldb, npcMgr, criminalCharID)
	}
}

// handleConcordTest processes "/concord": a GM test command that summons a
// CONCORD response on the caller with the real security-based delay.
func handleConcordTest(c *evenet.Conn, addr string, pkt *evenet.PyPacket,
	accountID, charID int64, sqldb *sql.DB, ds *destinyConn, npcMgr *npc.NPCManager) error {

	if ds.solarSystem == nil || ds.entity == nil {
		return server.SendCallRsp(c, pkt, accountID,
			marshal.PyString{Value: "concord: you must be in space (undocked)."})
	}
	ss := ds.solarSystem
	delay, ok := concordDelayForSecurity(ss.Security)
	if !ok {
		return server.SendCallRsp(c, pkt, accountID,
			marshal.PyString{Value: fmt.Sprintf("concord: no jurisdiction here (sec %.2f).", ss.Security)})
	}
	if _, loaded := concordActive.LoadOrStore(charID, &concordResponse{sysID: ss.ID}); loaded {
		return server.SendCallRsp(c, pkt, accountID,
			marshal.PyString{Value: "concord: a response is already active on you."})
	}
	crimePos := ds.entity.Pos()
	criminalEntityID := ds.entity.ID
	markGlobalCriminal(ss, charID)
	globalTickMgr.ScheduleOnce(fmt.Sprintf("concord-spawn-%d", charID),
		time.Now().Add(delay), func() {
			spawnConcordResponse(ss, sqldb, npcMgr, charID, criminalEntityID, crimePos)
		})
	msg := fmt.Sprintf("CONCORD dispatched: sec %.2f → %s response. They stay while you live, then leave %s after you're gone.",
		ss.Security, delay, concordLinger())
	log.Printf("[%s] slash: /concord charID=%d %s", addr, charID, msg)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyString{Value: msg})
}

// despawnConcordResponse silently removes the response fleet (no explosion:
// they "warp off") and clears the criminal's flag.
func despawnConcordResponse(ss *destiny.SolarSystem, sqldb *sql.DB, npcMgr *npc.NPCManager, criminalCharID int64) {
	resp, ok := concordActive.LoadAndDelete(criminalCharID)
	if !ok {
		return
	}
	response := resp
	despawnNPCFleet(ss, sqldb, npcMgr, response.entityIDs)
	log.Printf("[concord] response for charID=%d despawned (%d ships)", criminalCharID, len(response.entityIDs))
}
