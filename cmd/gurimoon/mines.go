package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"strconv"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/dogma"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/npc"
	"gurimoon/safego"
	"gurimoon/seclaw"
	"gurimoon/server"
	"gurimoon/syncx"
	"gurimoon/tick"
)

// Mines: group 92 charges laid from a missile launcher, triggered by
// proximity and judged by the law of the system they lie in. See docs/mines.md.

const (
	attrProximityRange = 154
	attrExplosionRange = 107
	attrLauncherSpeed  = 51 // rate of fire, ms
)

// mineLauncherGroups are the launchers EnsureMineLauncherCharges opens to group 92.
var mineLauncherGroups = map[int64]bool{507: true, 508: true, 509: true, 510: true, 511: true, 524: true}

func minesEnabled() bool { return evedb.CachedSettingFloat("mines.enabled", 1, 0, 1) > 0 }
func mineArmDelay() time.Duration {
	return evedb.CachedSettingDuration("mines.armSeconds", time.Second, 10, 0, 600)
}
func mineLifetime() time.Duration {
	return evedb.CachedSettingDuration("mines.lifetimeHours", time.Hour, 24, 1, 24*30)
}
func mineCapPerPilot() int {
	return int(evedb.CachedSettingFloat("mines.maxPerPilotPerSystem", 20, 1, 500))
}

// ── registry ────────────────────────────────────────────────────────────────

// liveMine is one mine in a loaded system.
type liveMine struct {
	entity *destiny.Entity
	row    evedb.SpaceMine
	name   string
	radius float64

	proximity, blast    float64
	em, exp, kin, therm float64
}

// mineSystem is the mines of one loaded SolarSystem. A system reloaded after
// teardown is a new *SolarSystem, and its mines are registered afresh.
type mineSystem struct {
	ss    *destiny.SolarSystem
	mu    sync.Mutex
	mines map[uint32]*liveMine
}

var mineSystems syncx.Map[int64, *mineSystem] // sysID → *mineSystem

func mineSystemFor(ss *destiny.SolarSystem) *mineSystem {
	if v, ok := mineSystems.Load(ss.ID); ok {
		if ms := v; ms.ss == ss {
			return ms
		}
	}
	ms := &mineSystem{ss: ss, mines: map[uint32]*liveMine{}}
	mineSystems.Store(ss.ID, ms)
	return ms
}

func registerMine(ss *destiny.SolarSystem, m *liveMine) {
	ms := mineSystemFor(ss)
	ms.mu.Lock()
	ms.mines[m.entity.ID] = m
	ms.mu.Unlock()
}

// forgetMine drops a mine from the registry and returns it, nil if absent.
func forgetMine(sysID int64, id uint32) *liveMine {
	v, ok := mineSystems.Load(sysID)
	if !ok {
		return nil
	}
	ms := v
	ms.mu.Lock()
	defer ms.mu.Unlock()
	m := ms.mines[id]
	delete(ms.mines, id)
	return m
}

func lookupMine(sysID int64, id uint32) *liveMine {
	v, ok := mineSystems.Load(sysID)
	if !ok {
		return nil
	}
	ms := v
	ms.mu.Lock()
	defer ms.mu.Unlock()
	return ms.mines[id]
}

func (ms *mineSystem) snapshot() []*liveMine {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	out := make([]*liveMine, 0, len(ms.mines))
	for _, m := range ms.mines {
		out = append(out, m)
	}
	return out
}

func minesOwnedIn(sysID, charID int64) int {
	v, ok := mineSystems.Load(sysID)
	if !ok {
		return 0
	}
	n := 0
	for _, m := range v.snapshot() {
		if m.row.OwnerCharID == charID {
			n++
		}
	}
	return n
}

// mineRemoved is the bookkeeping for a mine entity that left space by any
// path (npcDestroyEntity for gunfire and detonation): registry and rows.
func mineRemoved(sysID int64, id uint32) {
	if forgetMine(sysID, id) == nil || globalDB == nil {
		return
	}
	go func() {
		defer safego.Recover("mineRemoved")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := evedb.DeleteMine(ctx, globalDB, int64(id)); err != nil {
			log.Printf("[mines] delete %d: %v", id, err)
		}
	}()
}

// ── entity ──────────────────────────────────────────────────────────────────

// buildLiveMine reads the type's ranges, damage and HP and builds the mine's
// entity: rigid (no maxVelocity, so destiny.Manager emits a lock-safe rigid
// ball), shootable, owned by the pilot who laid it.
func buildLiveMine(ctx context.Context, sqldb *sql.DB, row evedb.SpaceMine) *liveMine {
	attrs, _ := evedb.GetTypeAttributes(ctx, sqldb, row.TypeID)
	ti, _ := evedb.GetTypeInfo(ctx, sqldb, row.TypeID)
	ehp, _ := evedb.GetEntityHPByType(ctx, sqldb, row.TypeID)
	name := typeNameOrID(ctx, sqldb, row.TypeID)

	radius := ti.Radius
	if radius <= 0 {
		radius = 300
	}
	m := &liveMine{
		row: row, name: name, radius: radius,
		proximity: attrs[attrProximityRange], blast: attrs[attrExplosionRange],
		em: attrs[dogma.AttrEmDamage], exp: attrs[dogma.AttrExplosiveDamage],
		kin: attrs[dogma.AttrKineticDamage], therm: attrs[dogma.AttrThermalDamage],
	}
	if m.proximity <= 0 {
		m.proximity = 500
	}
	if m.blast <= 0 {
		m.blast = m.proximity
	}

	id := uint32(row.MineID)
	dm := destiny.New(id, 1, 1, 0, radius)
	dm.Pos = destiny.Vec3{X: row.X, Y: row.Y, Z: row.Z}
	dm.Stamp = 1
	e := &destiny.Entity{
		ID:       id,
		Manager:  dm,
		CharName: name,
		BubbleID: -1,
		IsPlayer: false,
		Slim: destiny.SlimItem{
			BallID:     row.MineID,
			ItemID:     row.MineID,
			TypeID:     row.TypeID,
			GroupID:    evedb.GroupMine,
			CategoryID: 8,
			OwnerID:    row.OwnerCharID,
			CorpID:     row.OwnerCorpID,
			AllianceID: row.OwnerAllianceID,
		},
		Damage: destiny.DamageState{Shield: 1.0, Tau: 1e9, Armor: 1.0, Structure: 1.0},
	}
	if ehp != nil && ehp.StructureHP > 0 {
		e.HP = destiny.HPState{
			MaxShield: ehp.ShieldHP, CurShield: ehp.ShieldHP,
			MaxArmor: ehp.ArmorHP, CurArmor: ehp.ArmorHP,
			MaxStructure: ehp.StructureHP, CurStructure: ehp.StructureHP,
			ShieldTau: ehp.ShieldTau,
		}
		e.Resists = [3][4]float64{ehp.ShieldRes, ehp.ArmorRes, ehp.HullRes}
	} else {
		e.HP = destiny.HPState{MaxStructure: 100, CurStructure: 100}
		e.Resists = [3][4]float64{{1, 1, 1, 1}, {1, 1, 1, 1}, {1, 1, 1, 1}}
	}
	dm.SetEntity(e)
	m.entity = e
	return m
}

// loadMinesIntoSystem re-lays the persisted mines of a system being loaded.
// Called from ensureSystemLoaded before any player is in the system, so no
// ball is broadcast: arrivals receive mines with their bubble's SetState.
func loadMinesIntoSystem(addr string, sqldb *sql.DB, ss *destiny.SolarSystem, sysID int64) {
	if sqldb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := evedb.ListMinesForSystem(ctx, sqldb, sysID)
	if err != nil {
		log.Printf("[%s] [mines] load system %d: %v", addr, sysID, err)
		return
	}
	for _, row := range rows {
		m := buildLiveMine(ctx, sqldb, row)
		ss.AddEntity(m.entity)
		registerMine(ss, m)
	}
	if len(rows) > 0 {
		log.Printf("[%s] [mines] %d mine(s) restored in system %d", addr, len(rows), sysID)
	}
}

// ── laying ──────────────────────────────────────────────────────────────────

var mineLaunchAt syncx.Map[int64, time.Time] // launcher itemID → time of its last mine

// launchMine lays one mine when the launcher holds a group-92 charge. Same
// contract as launchScanProbe: handled=false means "not a mine, carry on"; a
// refusal is the pilot-facing reason and nothing was consumed. The client
// fires a mine without a target: mineLaying, the charge's default effect, is
// an activation effect (shipmodulebutton.ActivateEffect).
func launchMine(c *evenet.Conn, addr string, accountID, charID int64, ds *destinyConn,
	modItemID, modTypeID, modFlag int64, sqldb *sql.DB) (handled bool, refusal string) {

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if ds == nil || ds.shipID == 0 {
		return false, ""
	}
	lc, err := evedb.GetLoadedCharge(ctx, sqldb, ds.shipID, modFlag)
	if err != nil || lc == nil {
		return false, ""
	}
	if g, _, gerr := evedb.GetTypeGroupCategory(ctx, sqldb, lc.TypeID); gerr != nil || g != evedb.GroupMine {
		return false, ""
	}
	mineName := typeNameOrID(ctx, sqldb, lc.TypeID)

	ss := ds.solarSystem
	switch {
	case ss == nil || ds.entity == nil:
		return true, "Mines can only be laid in space."
	case !minesEnabled():
		return true, "Mines are disabled on this server."
	case ss.Security >= concordMinSec:
		return true, "CONCORD forbids laying mines in high-security space."
	case ds.entity.IsInWarp() || ds.entity.IsWarpPending():
		return true, "You cannot lay a mine while warping."
	case ds.entity.IsCloaked():
		return true, "You cannot lay a mine while cloaked."
	case minesOwnedIn(ds.sysID, charID) >= mineCapPerPilot():
		return true, fmt.Sprintf("You already have %d mines in this system.", mineCapPerPilot())
	}
	if missing := missingFitSkills(ctx, sqldb, charID, lc.TypeID); missing != "" {
		return true, fmt.Sprintf("You need %s to lay a %s.", missing, mineName)
	}
	modAttrs, _ := evedb.GetTypeAttributes(ctx, sqldb, modTypeID)
	cycle := time.Duration(modAttrs[attrLauncherSpeed]) * time.Millisecond
	if v, ok := mineLaunchAt.Load(modItemID); ok && time.Since(v) < cycle {
		return true, "The launcher is still reloading."
	}

	// Spend the mine (one unit of the loaded stack).
	if lc.Quantity <= 1 {
		_ = evedb.DeleteEntity(ctx, sqldb, lc.ItemID)
		gone := &evedb.EntityRow{ItemID: lc.ItemID, TypeID: lc.TypeID, OwnerID: charID,
			LocationID: ds.shipID, Flag: modFlag, Quantity: lc.Quantity}
		_ = server.SendNotification(c, accountID, "OnItemChange", "charid", itemDeletedNotifArgs(gone))
	} else {
		_ = evedb.UpdateCargoItemQty(ctx, sqldb, lc.ItemID, lc.Quantity-1)
		if upd, _ := evedb.GetEntityRow(ctx, sqldb, lc.ItemID); upd != nil {
			_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
				evedb.BuildEntityRow(upd),
				marshal.PyDict{Items: []marshal.DictEntry{
					{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(lc.Quantity)}},
				}},
			}})
		}
	}
	mineLaunchAt.Store(modItemID, time.Now())

	// One mine is one cycle: light the module, then put it out.
	sendGodmaShipEffect(c, accountID, modItemID, modTypeID, ds.shipID, charID, 0, "useMissiles", true, 1000, 1)
	time.AfterFunc(time.Second, func() {
		if !c.IsClosed() {
			sendGodmaShipEffect(c, accountID, modItemID, modTypeID, ds.shipID, charID, 0, "useMissiles", false, 0, 0)
		}
	})

	ti, _ := evedb.GetTypeInfo(ctx, sqldb, lc.TypeID)
	now := time.Now()
	pos := mineDropPoint(ds.entity.BallDesc(), ti.Radius)
	row := evedb.SpaceMine{
		TypeID: lc.TypeID, SolarSystemID: ds.sysID,
		OwnerCharID: charID, OwnerCorpID: ds.corpID, OwnerAllianceID: ds.allianceID,
		X: pos.X, Y: pos.Y, Z: pos.Z,
		LaidAt: now.Unix(), ArmAt: now.Add(mineArmDelay()).Unix(), ExpiresAt: now.Add(mineLifetime()).Unix(),
	}
	id, err := evedb.CreateMine(ctx, sqldb, row, mineName)
	if err != nil {
		// The charge is spent; the pilot sees the launcher cycle and no mine.
		log.Printf("[%s] [mines] create for char %d: %v", addr, charID, err)
		return true, ""
	}
	row.MineID = id
	m := buildLiveMine(ctx, sqldb, row)
	ss.AddEntity(m.entity)
	registerMine(ss, m)

	bd := m.entity.BallDesc()
	bd.CorpID = uint32(ds.corpID)
	bd.Name = mineName
	broadcastAddBalls(ds, []destiny.BallDesc{bd}, []destiny.SlimItem{m.entity.Slim},
		map[uint32]destiny.DamageState{m.entity.ID: m.entity.DamageStateFrac()})

	log.Printf("[%s] [mines] char %d laid %s id=%d in system %d sec=%.2f, arms in %s",
		addr, charID, mineName, id, ds.sysID, ss.Security, mineArmDelay())
	return true, ""
}

// mineDropPoint is behind the ship, clear of its hull: against the velocity
// when moving, any direction when stopped.
func mineDropPoint(ship destiny.BallDesc, mineRadius float64) destiny.Vec3 {
	dir := ship.Vel.Scale(-1)
	if dir.Len() < 1 {
		dir = destiny.Vec3{X: rand.Float64() - 0.5, Y: rand.Float64() - 0.5, Z: rand.Float64() - 0.5}
	}
	if dir.Len() == 0 {
		dir = destiny.Vec3{X: 1}
	}
	return ship.Pos.Add(dir.Normalize().Scale(ship.Radius + mineRadius + 100))
}

// ── friend or foe ───────────────────────────────────────────────────────────

// mineParty is who a mine is weighing: its owner, or whoever flew into it.
type mineParty struct {
	CharID, CorpID, AllianceID, GangID int64
}

// mineSpares reports whether the mine treats v as a friend: its owner, the
// owner's corp, alliance or gang, or anyone positive returns true for (the
// standings lookup, run only when the cheap tests fail). Everyone else,
// neutral and worse, sets it off.
func mineSpares(owner, v mineParty, positive func() bool) bool {
	switch {
	case v.CharID != 0 && v.CharID == owner.CharID:
		return true
	case v.CorpID != 0 && v.CorpID == owner.CorpID:
		return true
	case v.AllianceID != 0 && v.AllianceID == owner.AllianceID:
		return true
	case v.GangID != 0 && v.GangID == owner.GangID:
		return true
	}
	return positive != nil && positive()
}

type mineFoFEntry struct {
	at      time.Time
	hostile bool
}

var mineFoFCache syncx.Map[string, mineFoFEntry] // "mine:entity" → mineFoFEntry

const mineFoFTTL = 10 * time.Second

func gangOf(charID int64) int64 {
	if globalGangReg == nil || charID <= 0 {
		return 0
	}
	return globalGangReg.GetCharGang(charID)
}

// mineIsHostile decides whether victim sets off (and is hurt by) mine m.
// Things that cannot move (structures, sentries, other mines) never do; NPCs
// are neutral and always do; a pilot or a pilot's drone is weighed by
// mineSpares against the owner's personal, corporation and alliance standings.
func mineIsHostile(sqldb *sql.DB, m *liveMine, victim *destiny.Entity) bool {
	if victim == nil || victim.ID == m.entity.ID || victim.Slim.GroupID == evedb.GroupMine {
		return false
	}
	if bd := victim.BallDesc(); bd.Mode == destiny.ModeRigid || bd.MaxSpeed <= 0 {
		return false
	}
	var v mineParty
	switch {
	case victim.IsPlayer:
		v = mineParty{CharID: victim.Slim.CharID, CorpID: victim.Slim.CorpID, AllianceID: victim.Slim.AllianceID}
	case victim.Slim.CategoryID == 18: // a drone answers for its owner
		v = mineParty{CharID: victim.Slim.OwnerID, CorpID: victim.Slim.CorpID}
	default:
		return true
	}
	key := strconv.FormatUint(uint64(m.entity.ID), 10) + ":" + strconv.FormatUint(uint64(victim.ID), 10)
	if c, ok := mineFoFCache.Load(key); ok {
		if e := c; time.Since(e.at) < mineFoFTTL {
			return e.hostile
		}
	}
	v.GangID = gangOf(v.CharID)
	owner := mineParty{CharID: m.row.OwnerCharID, CorpID: m.row.OwnerCorpID,
		AllianceID: m.row.OwnerAllianceID, GangID: gangOf(m.row.OwnerCharID)}
	hostile := !mineSpares(owner, v, func() bool { return mineStandingPositive(sqldb, owner, v) })
	mineFoFCache.Store(key, mineFoFEntry{at: time.Now(), hostile: hostile})
	return hostile
}

// mineStandingPositive reports whether the owner, the owner's corporation or
// the owner's alliance rates v above neutral.
func mineStandingPositive(sqldb *sql.DB, owner, v mineParty) bool {
	if sqldb == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, to := range []int64{v.CharID, v.CorpID, v.AllianceID} {
		if to > 0 && evedb.GetPersonalStanding(ctx, sqldb, owner.CharID, to) > 0 {
			return true
		}
	}
	if owner.CorpID > 0 && evedb.OwnerStandingToward(ctx, sqldb, owner.CorpID, v.CharID, v.CorpID, v.AllianceID) > 0 {
		return true
	}
	switch evedb.AllianceRelationshipToward(ctx, sqldb, owner.AllianceID, v.CorpID, v.AllianceID) {
	case evedb.AllianceRelationshipNAP, evedb.AllianceRelationshipFriend:
		return true
	}
	return false
}

// ── watch and detonation ────────────────────────────────────────────────────

// startMineWatch arms the one-second proximity sweep on its own lane, and
// once a minute expires mines past their lifetime, unloaded systems included.
func startMineWatch(tickMgr *tick.Manager, sqldb *sql.DB) {
	var pass uint64
	tickMgr.RegisterPeriodicLane("mine-watch", "mine-watch", 1, func(uint64) bool {
		pass++
		now := time.Now()
		npcMgr := globalNPCMgr
		mineSystems.Range(func(k int64, v *mineSystem) bool {
			ms := v
			if globalSysMgr == nil || globalSysMgr.Get(ms.ss.ID) != ms.ss {
				mineSystems.CompareAndDelete(k, v) // torn down; reloads re-register
				return true
			}
			for _, m := range ms.snapshot() {
				mineWatchOne(ms.ss, m, now, sqldb, npcMgr)
			}
			return true
		})
		if pass%60 == 0 {
			expireMines(sqldb, now)
		}
		return true
	})
}

// mineWatchOne expires, ignores (unarmed) or detonates one mine.
func mineWatchOne(ss *destiny.SolarSystem, m *liveMine, now time.Time, sqldb *sql.DB, npcMgr *npc.NPCManager) {
	if ss.GetEntity(m.entity.ID) != m.entity {
		forgetMine(ss.ID, m.entity.ID) // gone by a path that did not tell us
		return
	}
	if now.Unix() >= m.row.ExpiresAt {
		removeMineQuietly(ss, m, sqldb)
		return
	}
	if now.Unix() < m.row.ArmAt {
		return
	}
	origin := m.entity.Pos()
	for _, e := range aoeEwarVictims(ss, origin, m.radius, m.entity.ID, m.proximity, false) {
		if mineIsHostile(sqldb, m, e) {
			detonateMine(ss, m, sqldb, npcMgr)
			return
		}
	}
}

// detonateMine hits every hostile inside the blast and removes the mine with
// an explosion.
func detonateMine(ss *destiny.SolarSystem, m *liveMine, sqldb *sql.DB, npcMgr *npc.NPCManager) {
	if forgetMine(ss.ID, m.entity.ID) == nil {
		return // another pass, a scoop or gunfire got there first
	}
	origin := m.entity.Pos()
	ownerOnline := globalSessReg != nil && globalSessReg.FindByCharID(m.row.OwnerCharID) != nil
	ownerShip := ss.GetPlayerByCharID(m.row.OwnerCharID)

	var hit int
	for _, v := range aoeEwarVictims(ss, origin, m.radius, m.entity.ID, m.blast, false) {
		if !mineIsHostile(sqldb, m, v) {
			continue
		}
		mineHit(ss, m, v, ownerOnline, ownerShip, sqldb, npcMgr)
		hit++
	}
	log.Printf("[mines] %s id=%d (owner %d) detonated in system %d sec=%.2f: %d victim(s)",
		m.name, m.entity.ID, m.row.OwnerCharID, ss.ID, ss.Security, hit)

	// Explosion and removal (TerminalExplosion); mineRemoved finds the
	// registry already empty, so the rows go here.
	npcDestroyEntity(ss, m.entity, 0)
	deleteMineRows(sqldb, int64(m.entity.ID))
}

func deleteMineRows(sqldb *sql.DB, id int64) {
	if sqldb == nil {
		return
	}
	go func() {
		defer safego.Recover("deleteMineRows")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := evedb.DeleteMine(ctx, sqldb, id); err != nil {
			log.Printf("[mines] delete %d: %v", id, err)
		}
	}()
}

// mineHit applies one mine's damage to one victim, then the law and the
// kill, in the smartbombVictim order.
func mineHit(ss *destiny.SolarSystem, m *liveMine, victim *destiny.Entity, ownerOnline bool,
	ownerShip *destiny.Entity, sqldb *sql.DB, npcMgr *npc.NPCManager) {

	applied, destroyed := victim.ApplyDamage(m.em, m.exp, m.kin, m.therm)
	if applied <= 0 && !destroyed {
		return
	}
	targetID := victim.ID
	killerID := m.entity.ID
	if ownerShip != nil {
		killerID = ownerShip.ID
	}

	// The killmail names the owner; an offline owner's mine is ownerless.
	hitRec := destiny.KillHit{
		Name: m.name, IsNPC: true, ShipTypeID: m.row.TypeID, WeaponTypeID: m.row.TypeID,
		Damage: applied, FinalBlow: destroyed, Time: time.Now().Unix(),
	}
	if ownerOnline {
		hitRec.Name, hitRec.IsNPC = "", false
		hitRec.CharID, hitRec.CorpID, hitRec.AllianceID = m.row.OwnerCharID, m.row.OwnerCorpID, m.row.OwnerAllianceID
	}
	victim.RecordKillHitRaw(int64(m.entity.ID), hitRec)

	if victim.IsPlayer {
		if victim.Conn != nil {
			sendCombatMessage(victim.Conn, int64(victim.AccountID), attackMsgTaken[hitQualityID(1.0)], marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyString{Value: "source"}, Value: marshal.PyInt{Value: uint64(m.entity.ID)}},
				{Key: marshal.PyString{Value: "weapon"}, Value: marshal.PyInt{Value: uint64(m.row.TypeID)}},
				{Key: marshal.PyString{Value: "target"}, Value: marshal.PyInt{Value: uint64(targetID)}},
				{Key: marshal.PyString{Value: "damage"}, Value: marshal.PyReal{Value: applied}},
			}})
		}
		if ownerShip != nil {
			victim.AddAggressor(ownerShip.ID, time.Now().Unix())
		}
		if ownerOnline {
			noteMineAggression(ss, m, victim, ownerShip, sqldb)
		}
		sendPlayerHPUpdate(victim)
	} else if npcMgr != nil {
		npcMgr.NotifyDamage(targetID, killerID, applied)
		npcMgr.SyncHPToDB(targetID)
	}

	dmgState := victim.DamageStateFrac()
	dmgState.Timestamp = server.Win32TimeNow()
	broadcastDamageStateTo(ss, targetID, dmgState)
	if !destroyed {
		return
	}
	switch {
	case victim.IsPlayer && victim.Conn != nil && victim.PlayerState != nil:
		queuePlayerDeath(victim.PlayerState.(*destinyConn), victim.Slim.TypeID == 670)
	case globalDroneMgr != nil && globalDroneMgr.HandleDestroyed(targetID, killerID):
	case npcMgr != nil && npcMgr.Kill(targetID, killerID):
	default:
		npcDestroyEntity(ss, victim, killerID)
	}
}

// ── the law ─────────────────────────────────────────────────────────────────

// mineCrimeFacts are the facts of a mine hit: the owner as the shooter, and
// the system the MINE lies in as the scene. Where the owner is plays no part;
// judging the owner's own system is what put CONCORD on null-sec miners.
func mineCrimeFacts(sysID int64, sec float64, m *liveMine, victim *destiny.Entity) seclaw.KillFacts {
	return seclaw.KillFacts{
		KillerCharID:     m.row.OwnerCharID,
		KillerCorpID:     m.row.OwnerCorpID,
		KillerAllianceID: m.row.OwnerAllianceID,
		KillerShipTypeID: m.row.TypeID,
		VictimCharID:     victim.Slim.CharID,
		VictimCorpID:     victim.Slim.CorpID,
		VictimAllianceID: victim.Slim.AllianceID,
		SystemID:         sysID,
		Security:         sec,
		VictimOutlaw:     victim.Slim.SecurityStatus <= seclaw.OutlawStatus,
	}
}

// noteMineAggression rules on one mine hit on a pilot. A crime (low-sec, an
// innocent victim) makes the owner a global criminal IN THE MINE'S SYSTEM,
// paints the owner on the victim's overview there, and takes the aggression
// charge. Null-sec is lawless and the ladder says so. CONCORD is never
// called: high-sec mines cannot be laid.
func noteMineAggression(ss *destiny.SolarSystem, m *liveMine, victim, ownerShip *destiny.Entity, sqldb *sql.DB) {
	facts := mineCrimeFacts(ss.ID, ss.Security, m, victim)
	crime, why := seclaw.ShotIsCrime(facts, seclawDeps(sqldb))
	if !crime {
		log.Printf("[mines] hit on char %d by owner %d in system %d: lawful (%s)",
			facts.VictimCharID, facts.KillerCharID, ss.ID, why)
		return
	}
	applyMineCrime(ss.ID, ss, m.row.OwnerCharID, facts.VictimCharID, facts.VictimCorpID)
	if victim != nil {
		sentryNoteCrime(ss, m.row.OwnerCharID, victim.Pos())
	}
	noteAggressionCrime(ownerShip, facts)
}

// applyMineCrime writes the criminal flag and the owner→victim entries into
// sysID's table only, and pushes it to that system when it is loaded.
func applyMineCrime(sysID int64, ss *destiny.SolarSystem, ownerCharID, victimCharID, victimCorpID int64) {
	t := aggroTableFor(sysID)
	now := time.Now().Unix()
	t.set(ownerCharID, 0, now)
	t.set(ownerCharID, victimCharID, now)
	if victimCorpID > 0 {
		t.set(ownerCharID, victimCorpID, now)
	}
	log.Printf("[aggro] charID=%d GLOBAL CRIMINAL in system %d (mine)", ownerCharID, sysID)
	if ss != nil {
		broadcastAggression(ss)
	}
}

// ── expiry and scoop ────────────────────────────────────────────────────────

// removeMineQuietly takes a mine out of space with no explosion (expiry).
func removeMineQuietly(ss *destiny.SolarSystem, m *liveMine, sqldb *sql.DB) {
	if forgetMine(ss.ID, m.entity.ID) == nil {
		return
	}
	ss.RemoveEntity(m.entity.ID)
	broadcastBallRemove(ss, m.entity.ID)
	deleteMineRows(sqldb, int64(m.entity.ID))
	log.Printf("[mines] %s id=%d expired in system %d", m.name, m.entity.ID, ss.ID)
}

// expireMines deletes expired mines, loaded systems through removeMineQuietly
// and unloaded ones straight from the table.
func expireMines(sqldb *sql.DB, now time.Time) {
	if sqldb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	expired, err := evedb.ListExpiredMines(ctx, sqldb, now.Unix())
	if err != nil {
		log.Printf("[mines] expiry: %v", err)
		return
	}
	for _, x := range expired {
		if globalSysMgr != nil {
			if ss := globalSysMgr.Get(x.SolarSystemID); ss != nil {
				if m := lookupMine(x.SolarSystemID, uint32(x.MineID)); m != nil {
					removeMineQuietly(ss, m, sqldb)
					continue
				}
			}
		}
		if err := evedb.DeleteMine(ctx, sqldb, x.MineID); err != nil {
			log.Printf("[mines] expiry delete %d: %v", x.MineID, err)
		}
	}
}

// scoopMineFromCall is ship.Scoop for a mine: the owner, within transfer
// range, takes it back into cargo. handled=false means the ID is not a mine.
func scoopMineFromCall(ds *destinyConn, addr string, charID, itemID int64, sqldb *sql.DB) (handled bool, err error) {
	if ds.solarSystem == nil {
		return false, nil
	}
	ss := ds.solarSystem
	m := lookupMine(ss.ID, uint32(itemID))
	if m == nil {
		return false, nil
	}
	if ds.entity == nil || ds.shipID == 0 || sqldb == nil {
		return true, fmt.Errorf("you must be in space to scoop that")
	}
	if m.row.OwnerCharID != charID {
		return true, fmt.Errorf("that mine is not yours")
	}
	if !shipWithin(ds, m.entity.Pos(), itemTransferRange+m.radius) {
		return true, fmt.Errorf("you are too far away to scoop that")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if shipER, _ := evedb.GetEntityRow(ctx, sqldb, ds.shipID); shipER != nil {
		cargoCap, _ := evedb.GetShipCargoCapacityFitted(ctx, sqldb, shipER.TypeID, ds.shipID)
		usedVol, _ := evedb.GetCargoUsedVolume(ctx, sqldb, charID, ds.shipID)
		need := evedb.ItemVolume(ctx, sqldb, itemID, m.row.TypeID)
		if free := cargoCap - usedVol; need > free {
			return true, fmt.Errorf("not enough cargo space: the mine needs %.2f m3 and your hold has %.2f m3 free",
				need, max(free, 0))
		}
	}
	// Taking it out of the registry is the claim against a detonation.
	if forgetMine(ss.ID, m.entity.ID) == nil {
		return true, fmt.Errorf("there is nothing here to scoop")
	}
	if err := evedb.ScoopMineToCargo(ctx, sqldb, itemID, ds.sysID, ds.shipID); err != nil {
		if !errors.Is(err, evedb.ErrMineGone) {
			registerMine(ss, m) // still in space; put it back
			return true, err
		}
		// The rows are gone; the ball must not outlive them.
		ss.RemoveEntity(m.entity.ID)
		broadcastBallRemove(ss, m.entity.ID)
		return true, err
	}
	ss.RemoveEntity(m.entity.ID)
	broadcastBallRemove(ss, m.entity.ID)
	if newER, err := evedb.GetEntityRow(ctx, sqldb, itemID); err == nil && newER != nil {
		changeDict := marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: uint64(ds.sysID)}},
			{Key: marshal.PyInt{Value: 4}, Value: marshal.PyInt{Value: 0}},
		}}
		_ = server.SendNotification(ds.entity.Conn, int64(ds.entity.AccountID), "OnItemChange", "charid",
			marshal.PyTuple{Items: []marshal.PyRep{evedb.BuildEntityRow(newER), changeDict}})
	}
	log.Printf("[%s] [mines] %s id=%d scooped by char %d", addr, m.name, itemID, charID)
	return true, nil
}
