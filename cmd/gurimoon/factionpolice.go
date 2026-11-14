package main

// Empire police and navy squads. In police space (rounded security 0.5 and up)
// the system's empire sends its police after a pilot whose security status is
// at or below the band's threshold (police.secThresholds), and its navy after a
// pilot whose standing toward that empire is at or below police.navyStandingMax.
// A pilot who qualifies for both gets the navy. The squad warps in on the
// pilot, follows them around the system, ignores capsules and stands down when
// the pilot docks, leaves, or no longer qualifies. Hulls and composition:
// docs/faction-police.md.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/safego"
	"gurimoon/seclaw"
	"gurimoon/server"
	"gurimoon/syncx"
	"gurimoon/tick"
)

type squadKind uint8

const (
	squadPolice squadKind = iota + 1
	squadNavy
)

func (k squadKind) String() string {
	if k == squadNavy {
		return "navy"
	}
	return "police"
}

// squadLine is one empire service's hulls: frigates and cruisers are drawn in
// turn, bs is the only tier carrying warp scramble and web dogma.
type squadLine struct {
	corpID   int64
	frigs    []int64
	cruisers []int64
	bs       int64
	warnKey  string // chat*PoliceWarning / chat*MilitaryWarning, "" for none
}

type empireResponders struct {
	police, navy squadLine
}

// empireSquads maps an empire factionID to its police (group 182) and navy
// (group 288) lines. Khanid and Ammatar navies have no battleship of their
// own and borrow the Amarr Navy's. CONCORD systems get SWAT and no navy.
var empireSquads = map[int64]empireResponders{
	500003: { // Amarr Empire
		police: squadLine{1000082, []int64{9959}, []int64{9962, 9960}, 11515, "chatAmarrEmpirePoliceWarning"},
		navy:   squadLine{1000084, []int64{9998, 9997, 10000, 9999}, []int64{10003, 10001}, 11137, "chatAmarrEmpireMilitaryWarning"},
	},
	500001: { // Caldari State
		police: squadLine{1000043, []int64{9970}, []int64{10660, 9971, 9965}, 11521, "chatCaldariStatePoliceWarning"},
		navy:   squadLine{1000035, []int64{10048, 10651, 10046, 10047}, []int64{10050}, 11138, "chatCaldariStateMilitaryWarning"},
	},
	500002: { // Minmatar Republic
		police: squadLine{1000054, []int64{9989}, []int64{9977, 9978}, 11520, "chatMinmatarRepublicPoliceWarning"},
		navy:   squadLine{1000051, []int64{10058, 10664, 10663, 10060}, []int64{10078, 10077}, 11140, "chatMinmatarRepublicMilitaryWarning"},
	},
	500004: { // Gallente Federation
		police: squadLine{1000121, []int64{9991}, []int64{9983, 9984}, 11519, "chatGallenteFederationPoliceWarning"},
		navy:   squadLine{1000120, []int64{10053, 10054, 10652, 10052}, []int64{10056, 10057}, 11139, "chatGallenteFederationMilitaryWarning"},
	},
	500008: { // Khanid Kingdom
		police: squadLine{1000156, []int64{9987}, []int64{9988, 10674}, 11518, "chatAmarrEmpirePoliceWarning"},
		navy:   squadLine{1000156, []int64{10076, 10079, 10080, 10670}, []int64{10082, 10083}, 11137, "chatAmarrEmpireMilitaryWarning"},
	},
	500007: { // Ammatar Mandate
		police: squadLine{1000126, []int64{10097}, []int64{10100, 10099}, 11517, "chatAmarrEmpirePoliceWarning"},
		navy:   squadLine{1000123, []int64{10084, 10085, 10086, 10650}, []int64{10089, 10669}, 11137, "chatAmarrEmpireMilitaryWarning"},
	},
	concordFactionID: {
		police: squadLine{concordCorpDED, []int64{10035}, []int64{10036}, 11126, ""},
	},
}

// respondersFor resolves a system's faction to its squads, falling back to the
// empire of the same race (sentryRaceForFaction) for a minor faction.
func respondersFor(factionID int64) (int64, empireResponders, bool) {
	if r, ok := empireSquads[factionID]; ok {
		return factionID, r, true
	}
	byRace := map[int64]int64{1: 500001, 2: 500002, 4: 500003, 8: 500004}
	if f, ok := byRace[sentryRaceForFaction(factionID)]; ok {
		return f, empireSquads[f], true
	}
	return 0, empireResponders{}, false
}

// ── the rules (pure) ────────────────────────────────────────────────────────

// Band order of the per-band dials: 1.0, 0.9, 0.8, 0.7, 0.6, 0.5.
var (
	policeThresholdDefault = []float64{-2.0, -2.5, -3.0, -3.5, -4.0, -4.5}
	policeDelayDefault     = []float64{6, 8, 10, 12, 14, 15}
	squadLowDefault        = []float64{3, 2, 0} // 0.5-0.6: frigates, cruisers, battleships
	squadMidDefault        = []float64{3, 2, 1} // 0.7-0.8
	squadHighDefault       = []float64{2, 2, 2} // 0.9-1.0
)

// policeBandIndex is the index into a per-band dial, or ok=false below police
// space (the same 0.45 line as CONCORD).
func policeBandIndex(sec float64) (int, bool) {
	if sec < concordMinSec {
		return 0, false
	}
	i := int(math.Round((1.0 - seclaw.SecBand(sec)) * 10))
	if i < 0 {
		i = 0
	}
	if i > 5 {
		i = 5
	}
	return i, true
}

// policeHunts reports whether a pilot of this security status is hunted in a
// system of this security, given the per-band thresholds.
func policeHunts(sec, status float64, thresholds []float64) bool {
	i, ok := policeBandIndex(sec)
	return ok && i < len(thresholds) && status <= thresholds[i]
}

// navyHunts reports whether a pilot with this standing toward the system's
// empire is hunted by its navy.
func navyHunts(sec, standing, max float64) bool {
	_, ok := policeBandIndex(sec)
	return ok && standing <= max
}

// squadForBand is the {frigates, cruisers, battleships} sent in a system of
// this security.
func squadForBand(sec float64, low, mid, high []float64) [3]int {
	src := low
	switch b := seclaw.SecBand(sec); {
	case b >= 0.9:
		src = high
	case b >= 0.7:
		src = mid
	}
	var out [3]int
	for i := 0; i < 3 && i < len(src); i++ {
		out[i] = int(src[i])
	}
	return out
}

// policeVerdict decides which squad, if any, hunts a pilot: the navy first.
func policeVerdict(sec, status, standing float64, hasStanding bool,
	thresholds []float64, navyMax float64) squadKind {

	if hasStanding && navyHunts(sec, standing, navyMax) {
		return squadNavy
	}
	if policeHunts(sec, status, thresholds) {
		return squadPolice
	}
	return 0
}

// ── live dials ──────────────────────────────────────────────────────────────

func policeThresholds() []float64 {
	return evedb.CachedSettingFloats("police.secThresholds", policeThresholdDefault, -10, 10)
}
func policeNavyStandingMax() float64 {
	return evedb.CachedSettingFloat("police.navyStandingMax", -5, -10, 10)
}
func policeAttackPods() bool {
	return evedb.CachedSettingFloat("police.attackPods", 0, 0, 1) != 0
}
func policeLinger() time.Duration {
	return evedb.CachedSettingDuration("police.lingerSec", time.Second, 60, 0, 3600)
}
func policeRewarpDelay() time.Duration {
	return evedb.CachedSettingDuration("police.rewarpDelaySec", time.Second, 10, 0, 300)
}
func policeDelay(sec float64) time.Duration {
	d := evedb.CachedSettingFloats("police.responseDelaySec", policeDelayDefault, 0, 600)
	i, _ := policeBandIndex(sec)
	return time.Duration(d[i] * float64(time.Second))
}
func policeSquad(sec float64) [3]int {
	return squadForBand(sec,
		evedb.CachedSettingFloats("police.squadLow", squadLowDefault, 0, 10),
		evedb.CachedSettingFloats("police.squadMid", squadMidDefault, 0, 10),
		evedb.CachedSettingFloats("police.squadHigh", squadHighDefault, 0, 10))
}

const (
	// policeThreatSeed pins the squad on its target above incidental threat.
	policeThreatSeed = 500_000.0
	// policeRewarpRange is how far the target may get from the squad before it
	// warps after them: past it the pilot has left the grid.
	policeRewarpRange = 150_000.0
)

// ── state ───────────────────────────────────────────────────────────────────

// policeResponse is one squad hunting one pilot.
type policeResponse struct {
	mu          sync.Mutex
	kind        squadKind
	factionID   int64
	sysID       int64
	spawned     bool
	entityIDs   []uint32
	goneSince   time.Time
	rewarpSince time.Time
}

// policeActive maps charID to *policeResponse from dispatch until despawn.
var policeActive syncx.Map[int64, *policeResponse]

// policeMembers maps a squad ship's entityID to its squadKind, so the law can
// tell the police from the navy (isPoliceEntity).
var policeMembers syncx.Map[uint32, squadKind]

// policePursuing reports whether a squad is on this pilot (cloak jam).
func policePursuing(charID int64) bool {
	v, ok := policeActive.Load(charID)
	if !ok {
		return false
	}
	r := v
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.spawned
}

// isPoliceSquadMember reports whether an entity is a police (not navy) squad ship.
func isPoliceSquadMember(entityID uint32) bool {
	v, ok := policeMembers.Load(entityID)
	return ok && v == squadPolice
}

// systemFaction caches mapSolarSystems/constellation/region faction per system.
var systemFaction syncx.Map[int64, int64] // sysID -> int64

func systemFactionID(sysID int64) int64 {
	if v, ok := systemFaction.Load(sysID); ok {
		return v
	}
	if globalDB == nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	f, err := evedb.GetSystemSovFactionID(ctx, globalDB, sysID)
	if err != nil {
		log.Printf("[police] system %d faction: %v", sysID, err)
		return 0
	}
	systemFaction.Store(sysID, f)
	return f
}

// huntedBy reports which squad, if any, hunts this pilot in this system.
func huntedBy(ss *destiny.SolarSystem, e *destiny.Entity) (squadKind, int64) {
	if ss == nil || e == nil || !e.IsPlayer || e.Slim.CharID <= 0 || e.IsDestroyed() || e.IsGMMode() {
		return 0, 0
	}
	if _, ok := policeBandIndex(ss.Security); !ok {
		return 0, 0
	}
	if e.Slim.GroupID == groupCapsule && !policeAttackPods() {
		return 0, 0
	}
	faction, _, ok := respondersFor(systemFactionID(ss.ID))
	if !ok {
		return 0, 0
	}
	standing, has := e.GetNPCStanding(faction)
	kind := policeVerdict(ss.Security, e.Slim.SecurityStatus, standing, has,
		policeThresholds(), policeNavyStandingMax())
	if kind == squadNavy && empireSquads[faction].navy.corpID == 0 {
		kind = squadPolice // no navy here (CONCORD space): the police take it
		if !policeHunts(ss.Security, e.Slim.SecurityStatus, policeThresholds()) {
			kind = 0
		}
	}
	return kind, faction
}

// ── dispatch ────────────────────────────────────────────────────────────────

// policeCheck dispatches a squad on a pilot who qualifies in this system.
// Called on every entry into space and on a security status change. A squad
// already on them here is re-aimed; one left in another system is withdrawn.
func policeCheck(ss *destiny.SolarSystem, e *destiny.Entity) {
	kind, faction := huntedBy(ss, e)
	if kind == 0 || globalTickMgr == nil || globalNPCMgr == nil {
		return
	}
	charID := e.Slim.CharID
	if v, ok := policeActive.Load(charID); ok {
		r := v
		r.mu.Lock()
		here := r.sysID == ss.ID
		if here {
			r.goneSince = time.Time{}
		}
		r.mu.Unlock()
		if here {
			return
		}
		endPoliceResponse(charID, r)
	}
	r := &policeResponse{kind: kind, factionID: faction, sysID: ss.ID}
	if _, loaded := policeActive.LoadOrStore(charID, r); loaded {
		return
	}
	delay := policeDelay(ss.Security)
	log.Printf("[police] charID=%d (sec %.2f) in system %d sec=%.2f: %s of faction %d in %s",
		charID, e.Slim.SecurityStatus, ss.ID, ss.Security, kind, faction, delay)
	globalTickMgr.ScheduleOnce(fmt.Sprintf("police-spawn-%d", charID), time.Now().Add(delay), func() {
		spawnPoliceSquad(ss, charID, r, true)
	})
}

// spawnPoliceSquad warps the squad in on the pilot, if they are still here and
// (when check is set) still hunted. /police spawns unchecked; the watch then
// stands an unqualified target's squad down after the linger.
func spawnPoliceSquad(ss *destiny.SolarSystem, charID int64, r *policeResponse, check bool) {
	if v, ok := policeActive.Load(charID); !ok || v != r {
		return
	}
	target := findPlayerEntityByChar(ss, charID)
	if target == nil {
		policeActive.Delete(charID)
		return
	}
	if kind, _ := huntedBy(ss, target); check && kind == 0 {
		policeActive.Delete(charID)
		return
	}
	line := empireSquads[r.factionID].police
	if r.kind == squadNavy {
		line = empireSquads[r.factionID].navy
	}
	counts := policeSquad(ss.Security)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Spawn off grid and warp in, as a squad answering a call.
	tpos := target.Pos()
	dir := randomUnit()
	origin := tpos.Add(dir.Scale(350_000 + rand.Float64()*100_000))
	var ids []uint32
	add := func(typeID int64, n int) {
		ids = append(ids, spawnNPCWave(ctx, ss, globalDB, globalNPCMgr, typeID, n, origin,
			500, 3_000, line.corpID, r.factionID)...)
	}
	for i := 0; i < counts[0] && len(line.frigs) > 0; i++ {
		add(line.frigs[i%len(line.frigs)], 1)
	}
	for i := 0; i < counts[1] && len(line.cruisers) > 0; i++ {
		add(line.cruisers[i%len(line.cruisers)], 1)
	}
	if line.bs > 0 {
		add(line.bs, counts[2])
	}
	if len(ids) == 0 {
		policeActive.Delete(charID)
		return
	}
	for _, id := range ids {
		policeMembers.Store(id, r.kind)
		globalNPCMgr.SetWarpIn(id, landingNear(tpos))
	}
	deliverNPCFleet(ss, ids)

	r.mu.Lock()
	r.entityIDs = ids
	r.spawned = true
	r.mu.Unlock()

	if target.Conn != nil && line.warnKey != "" {
		pushRemoteMessage(target.Conn, int64(target.AccountID), line.warnKey,
			map[string]marshal.PyRep{"owner": marshal.PyString{Value: target.CharName}})
	}
	breakCloakForPursuit(target)
	log.Printf("[police] %s squad (%d ships: %v) warping to charID=%d in system %d",
		r.kind, len(ids), counts, charID, ss.ID)
}

// landingNear is a warp-in point 2-6 km from a pilot.
func landingNear(p destiny.Vec3) destiny.Vec3 {
	return p.Add(randomUnit().Scale(2_000 + rand.Float64()*4_000))
}

func randomUnit() destiny.Vec3 {
	theta := rand.Float64() * 2 * math.Pi
	z := rand.Float64()*2 - 1
	s := math.Sqrt(1 - z*z)
	return destiny.Vec3{X: s * math.Cos(theta), Y: s * math.Sin(theta), Z: z}
}

// endPoliceResponse removes a squad and forgets the response.
func endPoliceResponse(charID int64, r *policeResponse) {
	if v, ok := policeActive.Load(charID); ok && v == r {
		policeActive.Delete(charID)
	}
	r.mu.Lock()
	ids := r.entityIDs
	r.entityIDs = nil
	sysID := r.sysID
	r.mu.Unlock()
	for _, id := range ids {
		policeMembers.Delete(id)
	}
	if len(ids) == 0 || globalSysMgr == nil || globalNPCMgr == nil {
		return
	}
	if ss := globalSysMgr.Get(sysID); ss != nil {
		despawnNPCFleet(ss, globalDB, globalNPCMgr, ids)
	}
	log.Printf("[police] squad on charID=%d stood down (%d ships)", charID, len(ids))
}

// ── upkeep ──────────────────────────────────────────────────────────────────

// startPoliceWatch arms the squads' upkeep on its own lane: re-aim, pursue,
// charge the law, stand down. Every response carries its own lock.
func startPoliceWatch(tickMgr *tick.Manager, sqldb *sql.DB) {
	tickMgr.RegisterPeriodicLane("police-watch", "police-watch", 5, func(uint64) bool {
		if globalNPCMgr == nil || globalSysMgr == nil {
			return true
		}
		policeActive.Range(func(k int64, v *policeResponse) bool {
			policeWatchOne(k, v, sqldb)
			return true
		})
		return true
	})
}

func policeWatchOne(charID int64, r *policeResponse, sqldb *sql.DB) {
	r.mu.Lock()
	spawned, sysID := r.spawned, r.sysID
	r.mu.Unlock()
	if !spawned {
		return
	}
	ss := globalSysMgr.Get(sysID)
	if ss == nil {
		endPoliceResponse(charID, r)
		return
	}

	// Prune the dead; a squad wiped out ends.
	r.mu.Lock()
	alive := r.entityIDs[:0]
	for _, id := range r.entityIDs {
		if e := ss.GetEntity(id); e != nil && !e.IsDestroyed() {
			alive = append(alive, id)
		} else {
			policeMembers.Delete(id)
		}
	}
	r.entityIDs = alive
	kind := r.kind
	r.mu.Unlock()
	if len(alive) == 0 {
		policeActive.Delete(charID)
		log.Printf("[police] squad on charID=%d destroyed", charID)
		return
	}
	if kind == squadPolice {
		chargePoliceAggressors(sqldb, ss, globalNPCMgr, alive)
	}

	target := findPlayerEntityByChar(ss, charID)
	if k, _ := huntedBy(ss, target); k == 0 {
		r.mu.Lock()
		if r.goneSince.IsZero() {
			r.goneSince = time.Now()
		}
		leave := time.Since(r.goneSince) >= policeLinger()
		r.mu.Unlock()
		if leave {
			endPoliceResponse(charID, r)
		}
		return
	}

	// The pin: re-seeded every pass, because a warp-in landing resets an NPC's
	// threat table and a new hull is a new entity.
	var centre destiny.Vec3
	for _, id := range alive {
		globalNPCMgr.SeedThreat(id, target.ID, policeThreatSeed)
		if e := ss.GetEntity(id); e != nil {
			centre = centre.Add(e.Pos())
		}
	}
	centre = centre.Scale(1 / float64(len(alive)))

	// Pursuit: a pilot who left the grid is followed after the rewarp delay.
	tpos := target.Pos()
	r.mu.Lock()
	r.goneSince = time.Time{}
	far := tpos.Sub(centre).LenSq() > policeRewarpRange*policeRewarpRange
	rewarp := false
	switch {
	case !far:
		r.rewarpSince = time.Time{}
	case r.rewarpSince.IsZero():
		r.rewarpSince = time.Now()
	case time.Since(r.rewarpSince) >= policeRewarpDelay():
		r.rewarpSince = time.Time{}
		rewarp = true
	}
	r.mu.Unlock()
	if rewarp {
		for _, id := range alive {
			globalNPCMgr.SetWarpIn(id, landingNear(tpos))
		}
		log.Printf("[police] squad on charID=%d warping after them in system %d", charID, ss.ID)
	}
}

// ── GM test ─────────────────────────────────────────────────────────────────

// slashFields splits a slash line into lower-cased words without the command.
func slashFields(cs *server.CallStream) []string {
	if len(cs.ArgTuple.Items) == 0 {
		return nil
	}
	raw, ok := cs.ArgTuple.Items[0].(marshal.PyString)
	if !ok {
		return nil
	}
	parts := strings.Fields(strings.TrimLeft(strings.TrimSpace(raw.Value), "/#"))
	if len(parts) <= 1 {
		return nil
	}
	return parts[1:]
}

// handlePoliceTest processes "/police [navy]": a squad on the caller now,
// whatever their record, for testing the squad itself.
func handlePoliceTest(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket,
	accountID, charID int64, ds *destinyConn) error {

	reply := func(msg string) error {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyString{Value: msg})
	}
	if ds.solarSystem == nil || ds.entity == nil {
		return reply("police: you must be in space (undocked).")
	}
	ss := ds.solarSystem
	faction, resp, ok := respondersFor(systemFactionID(ss.ID))
	if !ok {
		return reply(fmt.Sprintf("police: no empire owns system %d.", ss.ID))
	}
	kind := squadPolice
	if args := slashFields(cs); len(args) > 0 && args[0] == "navy" {
		kind = squadNavy
		if resp.navy.corpID == 0 {
			return reply("police: no navy in this system.")
		}
	}
	if v, ok := policeActive.Load(charID); ok {
		endPoliceResponse(charID, v)
	}
	r := &policeResponse{kind: kind, factionID: faction, sysID: ss.ID}
	policeActive.Store(charID, r)
	log.Printf("[%s] slash: /police %s charID=%d system %d", addr, kind, charID, ss.ID)
	safego.Go("spawnPoliceSquad", func() { spawnPoliceSquad(ss, charID, r, false) })
	return reply(fmt.Sprintf("%s squad of faction %d dispatched: %v (frig, cruiser, bs).",
		kind, faction, policeSquad(ss.Security)))
}

// handleSecStatusSet processes "/secstatus <value>": sets the caller's own
// security status through the ordinary write path.
func handleSecStatusSet(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket,
	accountID, charID int64, sqldb *sql.DB, ds *destinyConn) error {

	reply := func(msg string) error {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyString{Value: msg})
	}
	args := slashFields(cs)
	if len(args) == 0 {
		return reply("usage: /secstatus <value -10..10>")
	}
	want, err := strconv.ParseFloat(args[0], 64)
	if err != nil || want < seclaw.SecMin || want > seclaw.SecMax {
		return reply("secstatus: value must be a number from -10 to 10.")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cur, err := evedb.GetCharSecurityRating(ctx, sqldb, charID)
	if err != nil {
		return reply("secstatus: " + err.Error())
	}
	applySecStatusChange(ctx, sqldb, ds.entity, charID, want-cur,
		evedb.EventStandingSlashSet, charID, ds.sysID, 0)
	log.Printf("[%s] slash: /secstatus charID=%d %.2f -> %.2f", addr, charID, cur, want)
	return reply(fmt.Sprintf("Security status %.2f -> %.2f.", cur, want))
}
