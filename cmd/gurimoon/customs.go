package main

// Customs Officer Patrol System: contraband enforcement at empire stargates,
// run to the client's own config.InvContrabandTypes rows (invcontrabandtypes).
// Officers spawn at each gate of a system whose rounded security reaches the
// faction's lowest confiscateMinSec (0.3 for some goods), randomly select
// pilots in range for a search (customs.scanChance) once per clearance window
// (customs.clearMinutes), and per illegal stack
// confiscate, fine and cost standing with the faction; jettisoned contraband
// near them is detonated. attackMinSec is 1.1 on every shipped row, so they do
// not open fire. Rules and evidence: docs/faction-police.md#customs.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math"
	"math/rand"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/npc"
	"gurimoon/safego"
	"gurimoon/seclaw"
	"gurimoon/server"
	"gurimoon/syncx"
	"gurimoon/tick"
)

// ── Faction → customs corp + ship type mapping ──────────────────────────────

type customsFaction struct {
	name      string
	corpID    int64
	factionID int64
	typeIDs   []int64 // NPC entity typeIDs (invTypes with NPC stats)
}

var customsFactions = map[int64]customsFaction{
	500001: {name: "Caldari", corpID: 1000043, factionID: 500001, typeIDs: []int64{19385, 19367}},
	500002: {name: "Minmatar", corpID: 1000054, factionID: 500002, typeIDs: []int64{19370, 19388}},
	500003: {name: "Amarr", corpID: 1000079, factionID: 500003, typeIDs: []int64{17286, 19371, 19372}},
	500004: {name: "Gallente", corpID: 1000122, factionID: 500004, typeIDs: []int64{19369, 19383}},
	500006: {name: "CONCORD", corpID: 1000132, factionID: 500006, typeIDs: []int64{19563, 19564}},
	500007: {name: "Ammatar", corpID: 1000126, factionID: 500007, typeIDs: []int64{19366, 19387}},
	500008: {name: "Khanid", corpID: 1000156, factionID: 500008, typeIDs: []int64{19368, 19386}},
}

// ── Tuning constants ────────────────────────────────────────────────────────

const (
	customsScanRange    = 15_000.0          // 15km proximity before scan triggers
	customsScanDelay    = 5 * time.Second   // search length: warning, scan FX, verdict
	customsOrbitDist    = 5_000.0           // orbit distance from gate
	customsPatrolLinger = 120 * time.Second // time at each gate before roaming
	customsOfficerCount = 2                 // officers per gate (gate-anchored)
	customsPatrolCount  = 2                 // roaming patrol size

	// refContrabandFine is the journal ref type for contraband fines (73, CONCORD fine).
	refContrabandFine = 73

	// customsCargoScannerType is Cargo Scanner I, the module type the search FX names.
	customsCargoScannerType = 442
)

// ── Per-system state ────────────────────────────────────────────────────────

type gateInfo struct {
	entityID uint32
	pos      destiny.Vec3
}

type customsSystem struct {
	mu          sync.Mutex
	sysID       int64
	factionID   int64
	faction     customsFaction
	officers    []uint32   // all gate-anchored customs entity IDs
	patrol      []uint32   // roaming patrol squad entity IDs
	gates       []gateInfo // gate positions for patrol routing
	patrolIdx   int        // current gate in patrol cycle
	patrolState string     // "orbiting" | "warping"
	patrolSince time.Time  // when current state started
}

// customsActive maps sysID → *customsSystem for all systems with active customs.
var customsActive syncx.Map[int64, *customsSystem]

// customsClearance maps customsClearKey → time.Time of the pilot's last
// customs decision with that faction. A pilot the roll passes over, or one
// searched (clean or fined), is left alone by every officer of the faction for
// customsClearWindow. A search in flight holds the same claim and drops it if
// the pilot slips away before the verdict.
var customsClearance syncx.Map[customsClearKey, time.Time]

type customsClearKey struct{ charID, factionID int64 }

// ── Initialization ──────────────────────────────────────────────────────────

// initCustomsPatrol spawns customs officers at stargates in a highsec empire
// system. Called from registerUndock when a system is first loaded.
func initCustomsPatrol(ss *destiny.SolarSystem, sqldb *sql.DB, npcMgr *npc.NPCManager) {
	if ss == nil || sqldb == nil || npcMgr == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	factionID, err := evedb.GetSystemFactionID(ctx, sqldb, int64(ss.ID))
	var rules map[int64]evedb.ContrabandEntry
	if err == nil && factionID != 0 {
		rules, err = evedb.GetContrabandForFaction(ctx, sqldb, factionID)
	}
	cancel()
	if err != nil || factionID == 0 {
		return
	}
	faction, ok := customsFactions[factionID]
	if !ok {
		return
	}
	minSec, present := customsPresenceMin(rules,
		evedb.CachedSettingFloat("customs.minSec", -1, -1, 1.1))
	if !present || seclaw.SecBand(ss.Security) < minSec {
		return
	}

	var gates []gateInfo
	for i, slim := range ss.CelSlims {
		if slim.GroupID == 10 && i < len(ss.CelBalls) {
			gates = append(gates, gateInfo{
				entityID: ss.CelBalls[i].EntityID,
				pos:      ss.CelBalls[i].Pos,
			})
		}
	}
	if len(gates) == 0 {
		return
	}

	cs := &customsSystem{
		sysID:     int64(ss.ID),
		factionID: factionID,
		faction:   faction,
		gates:     gates,
	}

	spawnCtx, spawnCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer spawnCancel()

	for _, gate := range gates {
		for i := 0; i < customsOfficerCount; i++ {
			typeID := faction.typeIDs[rand.Intn(len(faction.typeIDs))]
			entity := spawnCustomsOfficer(spawnCtx, ss, sqldb, npcMgr, typeID, gate.pos, faction)
			if entity != nil {
				cs.officers = append(cs.officers, entity.ID)
				npcMgr.SetLeash(entity.ID, gate.pos, customsOrbitDist)
			}
		}
	}

	for i := 0; i < customsPatrolCount; i++ {
		typeID := faction.typeIDs[rand.Intn(len(faction.typeIDs))]
		entity := spawnCustomsOfficer(spawnCtx, ss, sqldb, npcMgr, typeID, gates[0].pos, faction)
		if entity != nil {
			cs.patrol = append(cs.patrol, entity.ID)
			npcMgr.SetLeash(entity.ID, gates[0].pos, customsOrbitDist)
		}
	}
	cs.patrolState = "orbiting"
	cs.patrolSince = time.Now()

	customsActive.Store(int64(ss.ID), cs)

	totalShips := len(cs.officers) + len(cs.patrol)
	log.Printf("[customs] spawned %d officers in system %d (%s, %d gates)",
		totalShips, ss.ID, faction.name, len(gates))
}

// spawnCustomsOfficer spawns a single customs officer near pos.
func spawnCustomsOfficer(ctx context.Context, ss *destiny.SolarSystem, sqldb *sql.DB,
	npcMgr *npc.NPCManager, typeID int64, pos destiny.Vec3, faction customsFaction) *destiny.Entity {

	stats, err := evedb.GetNPCStats(ctx, sqldb, typeID)
	if err != nil {
		log.Printf("[customs] GetNPCStats typeID=%d: %v", typeID, err)
		return nil
	}

	var npcHP *npc.NPCHPInit
	if ehp, err := evedb.GetEntityHPByType(ctx, sqldb, typeID); err == nil {
		npcHP = &npc.NPCHPInit{
			ShieldHP: ehp.ShieldHP, ArmorHP: ehp.ArmorHP,
			StructureHP: ehp.StructureHP, ShieldTau: ehp.ShieldTau,
			ShieldRes: ehp.ShieldRes, ArmorRes: ehp.ArmorRes, HullRes: ehp.HullRes,
		}
	}

	weaponProfile, err := evedb.GetNPCWeaponProfile(ctx, sqldb, typeID)
	if err != nil {
		weaponProfile = nil
	}

	theta := rand.Float64() * 2 * math.Pi
	phi := rand.Float64() * math.Pi
	r := 2_000.0 + rand.Float64()*3_000.0
	spawnPos := destiny.Vec3{
		X: pos.X + r*math.Sin(phi)*math.Cos(theta),
		Y: pos.Y + r*math.Sin(phi)*math.Sin(theta),
		Z: pos.Z + r*math.Cos(phi),
	}

	entity := npcMgr.SpawnNPC(ss, typeID, stats.TypeName,
		spawnPos, stats.Mass, stats.Agility, stats.MaxVelocity, stats.Radius,
		npc.StanceDefensive, npcHP, weaponProfile, faction.corpID, false, 0, faction.factionID, false)
	if entity == nil {
		return nil
	}
	npc.SetGroupCategory(entity, stats.GroupID, stats.CategoryID)

	// Deliver to any players already on grid.
	for _, p := range ss.GetBubbleConnectedPlayersExcept(entity.ID) {
		ensureBallsKnown(p, entity)
	}

	return entity
}

// ── Periodic scan tick ──────────────────────────────────────────────────────

// startCustomsWatch arms the periodic that scans players near customs officers.
func startCustomsWatch(tickMgr *tick.Manager, sqldb *sql.DB) {
	// Own lane, for the same reason as sentry-watch. Officer and cooldown
	// state is under cs.mu.
	tickMgr.RegisterPeriodicLane("customs-watch", "customs-watch", 5, func(uint64) bool {
		npcMgr := globalNPCMgr
		if npcMgr == nil || globalSysMgr == nil {
			return true
		}
		customsActive.Range(func(k int64, v *customsSystem) bool {
			customsWatchSystem(k, v, sqldb, npcMgr)
			return true
		})
		customsPruneClearance(time.Now(), customsClearWindow())
		return true
	})
}

// customsWatchSystem runs one system's customs scan + patrol logic.
func customsWatchSystem(sysID int64, cs *customsSystem, sqldb *sql.DB, npcMgr *npc.NPCManager) {
	ss := globalSysMgr.Get(sysID)
	if ss == nil {
		return
	}

	cs.mu.Lock()
	allOfficers := append(append([]uint32(nil), cs.officers...), cs.patrol...)
	cs.mu.Unlock()

	// Prune dead officers.
	alive := allOfficers[:0]
	for _, id := range allOfficers {
		if ss.GetEntity(id) != nil {
			alive = append(alive, id)
		}
	}

	now := time.Now()
	window := customsClearWindow()
	scanRangeSq := customsScanRange * customsScanRange

	for _, officerID := range alive {
		officer := ss.GetEntity(officerID)
		if officer == nil {
			continue
		}
		officerPos := officer.Pos()

		for _, p := range ss.GetAllPlayersInSystem() {
			if !p.IsPlayer || p.Slim.CharID <= 0 || p.Conn == nil {
				continue
			}
			distSq := officerPos.Sub(p.Pos()).LenSq()
			if distSq > scanRangeSq || !customsEligible(p) {
				continue
			}
			key := customsClearKey{charID: p.Slim.CharID, factionID: cs.factionID}
			if !customsClaim(key, now, window) {
				continue
			}
			// One roll per clearance window: a pilot passed over is waved through.
			if !customsSelected(rand.Float64(), customsScanChance()) {
				continue
			}
			go performCustomsScan(ss, sqldb, npcMgr, cs, officer, p, false)
		}
	}

	customsAdvancePatrol(ss, cs, sqldb, npcMgr)
}

// performCustomsScan searches a selected pilot: chatDefaultCustomsWarning and
// the scan FX, customsScanDelay, then chatDefaultCustomsPass or enforcement.
// A pilot who warps, cloaks, leaves range or the system in the meantime is let
// go with no verdict and the clearance claim dropped. force (/customs) skips
// the eligibility tests.
func performCustomsScan(ss *destiny.SolarSystem, sqldb *sql.DB, npcMgr *npc.NPCManager,
	cs *customsSystem, officer, player *destiny.Entity, force bool) {
	defer safego.Recover("performCustomsScan")

	if player == nil || player.Conn == nil || officer == nil {
		return
	}
	pds, ok := player.PlayerState.(*destinyConn)
	if !ok || pds == nil {
		return
	}

	charID := player.Slim.CharID
	shipID := pds.shipID
	if charID <= 0 || shipID <= 0 {
		return
	}

	key := customsClearKey{charID: charID, factionID: cs.factionID}

	// An empty hold has nothing to search; the claim stands as a clearance.
	qctx, qcancel := context.WithTimeout(context.Background(), 5*time.Second)
	cargo, err := evedb.GetCargoTypeIDs(qctx, sqldb, charID, shipID)
	qcancel()
	if err != nil {
		log.Printf("[customs] cargo query charID=%d: %v", charID, err)
		customsClearance.Delete(key)
		return
	}
	if len(cargo) == 0 {
		return
	}

	owner := map[string]marshal.PyRep{"owner": marshal.PyString{Value: pds.charName}}
	pushRemoteMessage(player.Conn, int64(player.AccountID), "chatDefaultCustomsWarning", owner)
	sendCustomsScanFX(ss, officer, player, true)
	time.Sleep(customsScanDelay)
	sendCustomsScanFX(ss, officer, player, false)

	if !force && !customsStillHeld(ss, officer, player) {
		customsClearance.Delete(key)
		return
	}
	customsClearance.Store(key, time.Now())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Read again: the pilot may have jettisoned during the search.
	cargo, err = evedb.GetCargoTypeIDs(ctx, sqldb, charID, shipID)
	if err != nil {
		log.Printf("[customs] cargo query charID=%d: %v", charID, err)
		return
	}

	contraband, err := evedb.GetContrabandForFaction(ctx, sqldb, cs.factionID)
	if err != nil {
		log.Printf("[customs] contraband query faction=%d: %v", cs.factionID, err)
		return
	}
	if len(contraband) == 0 {
		return
	}

	type found struct {
		item evedb.CargoItem
		rule evedb.ContrabandEntry
	}
	var hits []found
	for _, item := range cargo {
		if rule, illegal := contraband[item.TypeID]; illegal {
			hits = append(hits, found{item, rule})
		}
	}
	if len(hits) == 0 {
		pushRemoteMessage(player.Conn, int64(player.AccountID), "chatDefaultCustomsPass", owner)
		return
	}

	log.Printf("[customs] charID=%d in system %d: %d contraband items found", charID, ss.ID, len(hits))

	for _, h := range hits {
		customsEnforce(ctx, ss, sqldb, npcMgr, cs, officer, player, pds, h.item, h.rule)
	}
}

// sendCustomsScanFX starts or stops the cargo-scan beam from officer onto
// pilot, the effect a fitted Cargo Scanner plays, for every pilot on grid. The
// target must be the pilot's ball: fxSequencer drops a trigger whose target it
// does not hold.
func sendCustomsScanFX(ss *destiny.SolarSystem, officer, pilot *destiny.Entity, start bool) {
	guid := moduleFXGUID("cargoScan")
	if guid == "" {
		guid = "effects.CargoScan"
	}
	var action marshal.PyRep
	if start {
		action = destiny.BuildEffectAction(destiny.ActiveEffect{
			EntityID:   officer.ID,
			ModuleID:   int64(officer.ID),
			ModuleType: customsCargoScannerType,
			TargetID:   pilot.ID,
			GUID:       guid,
			DurationMs: customsScanDelay.Milliseconds(),
			StartTime:  server.Win32TimeNow(),
		})
	} else {
		action = destiny.BuildSimpleAction("OnSpecialFX",
			marshal.PyInt{Value: uint64(officer.ID)},      // entityID
			marshal.PyInt{Value: uint64(officer.ID)},      // moduleID
			marshal.PyInt{Value: customsCargoScannerType}, // moduleTypeID
			marshal.PyInt{Value: uint64(pilot.ID)},        // targetID
			marshal.PyNone{},                              // otherTypeID
			marshal.PyList{},                              // area
			marshal.PyString{Value: guid},                 // effect_type
			marshal.PyInt{Value: 0},                       // isOffensive
			marshal.PyInt{Value: 0},                       // start
			marshal.PyInt{Value: 0},                       // active
		)
	}
	for _, p := range ss.GetBubbleConnectedPlayersExcept(officer.ID) {
		if p.Conn == nil || !ensureBallsKnown(p, officer, pilot) {
			continue
		}
		if err := p.SendFXStamped(func(stamp uint32) error {
			return destiny.SendActionListEvents(p.Conn, globalNodeID, p.AccountID, stamp, nil, action)
		}); err != nil {
			log.Printf("[customs] scan FX to %d: %v", p.ID, err)
		}
	}
}

// customsEnforce applies a single contraband item's penalties.
func customsEnforce(ctx context.Context, ss *destiny.SolarSystem, sqldb *sql.DB,
	npcMgr *npc.NPCManager, cs *customsSystem, officer, player *destiny.Entity,
	pds *destinyConn, item evedb.CargoItem, rule evedb.ContrabandEntry) {

	charID := player.Slim.CharID
	// Rounded as the client rounds it, so the thresholds match the presence test.
	sysSec := seclaw.SecBand(ss.Security)

	if rule.ConfiscateMinSec > 0 && sysSec >= rule.ConfiscateMinSec {
		if err := evedb.DeleteCargoItems(ctx, sqldb, []int64{item.ItemID}); err != nil {
			log.Printf("[customs] confiscate itemID=%d: %v", item.ItemID, err)
		} else {
			log.Printf("[customs] confiscated itemID=%d typeID=%d qty=%d from charID=%d",
				item.ItemID, item.TypeID, item.Quantity, charID)
			// Notify client of item removal.
			removeArgs := marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyString{Value: "OnItemChange"},
				marshal.PyTuple{Items: []marshal.PyRep{
					marshal.PyInt{Value: uint64(item.ItemID)},
				}},
				marshal.PyDict{Items: []marshal.DictEntry{
					{Key: marshal.PyString{Value: "locationID"}, Value: marshal.PyNone{}},
				}},
			}}
			_ = server.SendNotification(player.Conn, int64(player.AccountID),
				"OnItemChange", "charid", removeArgs)
		}
	}

	if rule.FineByValue > 0 {
		basePrice, err := evedb.GetItemBasePrice(ctx, sqldb, item.TypeID)
		if err == nil && basePrice > 0 {
			fineAmount := float64(item.Quantity) * basePrice * rule.FineByValue
			if fineAmount > 0 {
				reason := evedb.JournalReason(fmt.Sprintf("Contraband fine: %s", rule.TypeName))
				newBal, err := evedb.DeductWalletFine(ctx, sqldb, charID, fineAmount,
					refContrabandFine, reason)
				if err != nil {
					log.Printf("[customs] fine charID=%d: %v", charID, err)
				} else {
					log.Printf("[customs] fined charID=%d %.2f ISK (balance: %.2f)", charID, fineAmount, newBal)
					// Notify client of balance change.
					balArgs := marshal.PyTuple{Items: []marshal.PyRep{
						marshal.PyString{Value: "OnAccountChange"},
						marshal.PyString{Value: "cash"},
						marshal.PyInt{Value: uint64(player.AccountID)},
						marshal.PyReal{Value: newBal},
					}}
					_ = server.SendNotification(player.Conn, int64(player.AccountID),
						"OnAccountChange", "charid", balArgs)
				}
			}
		}
	}

	// Standing with the faction whose law was broken (event 126). The live
	// copy moves too, so a pilot pushed to -5 meets the navy on their next entry.
	if rule.StandingLoss > 0 {
		newStanding, err := evedb.UpsertNPCStanding(ctx, sqldb, charID, cs.factionID, -rule.StandingLoss)
		if err != nil {
			log.Printf("[customs] standing loss charID=%d: %v", charID, err)
		} else {
			player.SetNPCStanding(cs.factionID, newStanding)
			if err := evedb.InsertStandingTransaction(ctx, sqldb, evedb.EventStandingContrabandTrafficking,
				cs.factionID, charID, -rule.StandingLoss, item.TypeID, ss.ID, 0, ""); err != nil {
				log.Printf("[customs] standing transaction charID=%d: %v", charID, err)
			}
		}
	}

	// Attack: seed threat so the officers engage. Unreachable with shipped rows
	// (attackMinSec 1.1); SeedThreat keeps an operator's edit off the gate guns.
	if rule.AttackMinSec > 0 && sysSec >= rule.AttackMinSec {
		cs.mu.Lock()
		allOfficers := append(append([]uint32(nil), cs.officers...), cs.patrol...)
		cs.mu.Unlock()

		for _, oid := range allOfficers {
			if ss.GetEntity(oid) != nil {
				npcMgr.SeedThreat(oid, player.ID, concordThreatSeed)
			}
		}
		pushRemoteMessage(player.Conn, int64(player.AccountID), "chatDefaultCustomsThreat",
			map[string]marshal.PyRep{"owner": marshal.PyString{Value: pds.charName}})
		log.Printf("[customs] officers engaging charID=%d in system %d", charID, ss.ID)
	}
}

// ── Patrol routing ──────────────────────────────────────────────────────────

// customsAdvancePatrol moves the roaming patrol squad between gates by
// despawning at the old gate and respawning at the new one.
func customsAdvancePatrol(ss *destiny.SolarSystem, cs *customsSystem, sqldb *sql.DB, npcMgr *npc.NPCManager) {
	cs.mu.Lock()
	if len(cs.gates) < 2 || len(cs.patrol) == 0 {
		cs.mu.Unlock()
		return
	}
	if cs.patrolState != "orbiting" || time.Since(cs.patrolSince) < customsPatrolLinger {
		cs.mu.Unlock()
		return
	}
	cs.patrolIdx = (cs.patrolIdx + 1) % len(cs.gates)
	nextGate := cs.gates[cs.patrolIdx]
	oldPatrol := append([]uint32(nil), cs.patrol...)
	cs.patrol = nil
	cs.patrolState = "warping"
	cs.mu.Unlock()

	// The whole patrol leaves in one RemoveBalls; a packet per ball would cost
	// every witness a sim-second per patrol ship.
	live := make([]uint32, 0, len(oldPatrol))
	for _, eid := range oldPatrol {
		if ss.GetEntity(eid) == nil {
			continue
		}
		npcMgr.Remove(eid)
		live = append(live, eid)
		go func(id uint32) {
			defer safego.Recover("customsAdvancePatrol")
			dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer dcancel()
			_ = evedb.DeleteLiveNPC(dctx, sqldb, id)
		}(eid)
	}
	removeBallsBatched(ss, live)

	spawnCtx, spawnCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer spawnCancel()

	cs.mu.Lock()
	for i := 0; i < customsPatrolCount; i++ {
		typeID := cs.faction.typeIDs[rand.Intn(len(cs.faction.typeIDs))]
		entity := spawnCustomsOfficer(spawnCtx, ss, sqldb, npcMgr, typeID, nextGate.pos, cs.faction)
		if entity != nil {
			cs.patrol = append(cs.patrol, entity.ID)
			npcMgr.SetLeash(entity.ID, nextGate.pos, customsOrbitDist)
		}
	}
	cs.patrolState = "orbiting"
	cs.patrolSince = time.Now()
	cs.mu.Unlock()

	log.Printf("[customs] patrol moved to gate %d in system %d", nextGate.entityID, ss.ID)
}

// ── Despawn ─────────────────────────────────────────────────────────────────

// customsDespawn clears a system's customs state so the next load respawns.
// Called from teardownSystemNPCs; entity removal and DB cleanup are
// globalNPCMgr.RemoveSystem's.
func customsDespawn(sysID int64) {
	_, ok := customsActive.LoadAndDelete(sysID)
	if !ok {
		return
	}
	log.Printf("[customs] cleared customs state for system %d", sysID)
}

// ── GM test command ─────────────────────────────────────────────────────────

// handleCustomsTest processes /customs: a GM command that spawns customs
// officers and force-triggers a scan on the caller.
func handleCustomsTest(c *evenet.Conn, addr string, pkt *evenet.PyPacket,
	accountID, charID int64, sqldb *sql.DB, ds *destinyConn, npcMgr *npc.NPCManager) error {

	if ds.solarSystem == nil || ds.entity == nil {
		return server.SendCallRsp(c, pkt, accountID,
			marshal.PyString{Value: "customs: you must be in space (undocked)."})
	}
	ss := ds.solarSystem

	if val, ok := customsActive.Load(int64(ss.ID)); ok {
		// Force a scan on self.
		cs := val
		cs.mu.Lock()
		var officerID uint32
		if len(cs.officers) > 0 {
			officerID = cs.officers[0]
		} else if len(cs.patrol) > 0 {
			officerID = cs.patrol[0]
		}
		cs.mu.Unlock()

		officer := ss.GetEntity(officerID)
		if officer != nil {
			go performCustomsScan(ss, sqldb, npcMgr, cs, officer, ds.entity, true)
			msg := "Customs scan triggered on yourself."
			log.Printf("[%s] slash: /customs charID=%d scan triggered", addr, charID)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyString{Value: msg})
		}
	}

	// No customs yet: spawn them.
	initCustomsPatrol(ss, sqldb, npcMgr)
	if _, ok := customsActive.Load(int64(ss.ID)); !ok {
		return server.SendCallRsp(c, pkt, accountID,
			marshal.PyString{Value: "customs: could not spawn (wrong faction or sec status?)."})
	}

	msg := fmt.Sprintf("Customs officers spawned in system %d (sec %.2f). Approach a gate within 15km to be scanned.",
		ss.ID, ss.Security)
	log.Printf("[%s] slash: /customs charID=%d %s", addr, charID, msg)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyString{Value: msg})
}

// ── Era rules (pure) ────────────────────────────────────────────────────────

// customsPresenceMin is the lowest rounded security at which a faction keeps
// customs at its gates: its lowest reachable confiscateMinSec. override >= 0
// (customs.minSec) replaces it. ok is false for a faction that seizes nothing.
func customsPresenceMin(rules map[int64]evedb.ContrabandEntry, override float64) (float64, bool) {
	if override >= 0 {
		return override, true
	}
	min, ok := 0.0, false
	for _, r := range rules {
		if !r.Confiscates() {
			continue
		}
		if !ok || r.ConfiscateMinSec < min {
			min, ok = r.ConfiscateMinSec, true
		}
	}
	return min, ok
}

// customsScanChance is the chance a pilot in range is selected for a search.
func customsScanChance() float64 {
	return evedb.CachedSettingFloat("customs.scanChance", 0.5, 0, 1)
}

// customsSelected is the selection roll: roll in [0, 1).
func customsSelected(roll, chance float64) bool { return roll < chance }

// customsClearWindow is how long a customs decision stands (customs.clearMinutes).
func customsClearWindow() time.Duration {
	return time.Duration(evedb.CachedSettingFloat("customs.clearMinutes", 60, 0, 10080) * float64(time.Minute))
}

// customsClaim records a decision for key at now unless one younger than
// window stands. It reports whether the caller took the claim.
func customsClaim(key customsClearKey, now time.Time, window time.Duration) bool {
	if v, ok := customsClearance.Load(key); ok && now.Sub(v) < window {
		return false
	}
	customsClearance.Store(key, now)
	return true
}

// customsPruneClearance drops decisions older than window.
func customsPruneClearance(now time.Time, window time.Duration) {
	customsClearance.Range(func(k customsClearKey, v time.Time) bool {
		if now.Sub(v) >= window {
			customsClearance.Delete(k)
		}
		return true
	})
}

// customsEligible reports whether officers may pick a pilot: not cloaked, in
// GM mode or landing grace, and not aligning to or in warp.
func customsEligible(p *destiny.Entity) bool {
	return !p.IsUntargetable() && !p.IsInWarp() && !p.IsWarpPending()
}

// customsStillHeld reports whether a searched pilot is still eligible, in the
// system and within twice the scan range of the officer at the verdict.
func customsStillHeld(ss *destiny.SolarSystem, officer, pilot *destiny.Entity) bool {
	if ss.GetEntity(pilot.ID) == nil || ss.GetEntity(officer.ID) == nil || !customsEligible(pilot) {
		return false
	}
	reach := 2 * customsScanRange
	return officer.Pos().Sub(pilot.Pos()).LenSq() <= reach*reach
}

// customsNoteJettison detonates a can of contraband jettisoned within an
// officer's scan range (chatDefaultCustomsDiscard to its owner). Called from
// the Jettison handler after the can is in space.
func customsNoteJettison(ss *destiny.SolarSystem, ownerCharID, containerID, typeID int64, pos destiny.Vec3) {
	if ss == nil || globalDB == nil {
		return
	}
	v, ok := customsActive.Load(int64(ss.ID))
	if !ok {
		return
	}
	cs := v
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	rules, err := evedb.GetContrabandForFaction(ctx, globalDB, cs.factionID)
	cancel()
	if err != nil {
		return
	}
	rule, illegal := rules[typeID]
	if !illegal || !rule.Confiscates() || seclaw.SecBand(ss.Security) < rule.ConfiscateMinSec {
		return
	}
	cs.mu.Lock()
	officers := append(append([]uint32(nil), cs.officers...), cs.patrol...)
	cs.mu.Unlock()
	seen := false
	for _, id := range officers {
		if o := ss.GetEntity(id); o != nil && o.Pos().Sub(pos).LenSq() <= customsScanRange*customsScanRange {
			seen = true
			break
		}
	}
	if !seen {
		return
	}
	factionName := cs.faction.name
	go func() {
		defer safego.Recover("customsNoteJettison")
		time.Sleep(customsScanDelay)
		despawnLootContainer(ss, containerID, globalDB)
		log.Printf("[customs] detonated jettisoned contraband container %d (type %d) of charID=%d in system %d",
			containerID, typeID, ownerCharID, ss.ID)
		if c, acct := charConnLookup(ownerCharID); c != nil {
			pushRemoteMessage(c, acct, "chatDefaultCustomsDiscard",
				map[string]marshal.PyRep{"faction": marshal.PyString{Value: factionName}})
		}
	}()
}
