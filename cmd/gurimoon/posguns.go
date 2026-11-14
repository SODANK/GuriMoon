package main

// POS sentry guns and electronic-warfare batteries. Every second the loop
// walks the structure entities of every loaded system; each online gun or
// battery under an online or reinforced tower picks a pilot the Defense tab
// (db.SentryShouldEngage) says to engage, locks them, and fires on its own
// rate of fire. Guns are not NPC-manager entities; damage is the loaded
// ammunition times the gun's damageMultiplier, one round per shot (laser
// crystals excepted), and an empty bay does not fire. Missile batteries
// launch a real missile ball through queueMissileLaunch, as an NPC does.
// See docs/starbase-pos-investigation.md#19-sentries-and-ew-batteries.

import (
	"context"
	"database/sql"
	"log"
	"math"
	"math/rand"
	"strconv"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/dogma"
	"gurimoon/marshal"
	"gurimoon/rates"
	"gurimoon/safego"
	"gurimoon/server"
	"gurimoon/syncx"
	"gurimoon/tick"
)

// POS battery groups.
const (
	groupECMBattery       = 439
	groupDampBattery      = 440
	groupWebBattery       = 441
	groupScramBattery     = 443
	posGunTickSeconds     = 1
	posEngageCacheTTL     = 15 * time.Second
	posPolicyCacheTTL     = 30 * time.Second
	posDefaultSigRadius   = 100.0
)

// posGunKind classifies a structure group for the loop.
func posGunKind(groupID int64) string {
	switch groupID {
	case evedb.GroupMobileProjectileSentry, evedb.GroupMobileLaserSentry, evedb.GroupMobileHybridSentry:
		return "turret"
	case evedb.GroupMobileMissileSentry:
		return "missile"
	case groupECMBattery:
		return "jam"
	case groupDampBattery:
		return "sensorDamp"
	case groupWebBattery:
		return "web"
	case groupScramBattery:
		return "scram"
	}
	return ""
}

// posGun is one gun's state between ticks.
type posGun struct {
	mu       sync.Mutex
	nextFire time.Time
	target   uint32
	lastPos  destiny.Vec3
	lastSeen time.Time
	ewarOn   bool
}

var posGuns syncx.Map[int64, *posGun] // entityID (int64) → *posGun

// stopPOSGun forgets a gun (it died or was removed).
func stopPOSGun(entityID int64) { posGuns.Delete(entityID) }

// posTypeAttrs caches GetTypeAttributes for guns, charges and target hulls.
var posTypeAttrs syncx.Map[int64, map[int]float64] // typeID → map[int]float64

func posAttrs(sqldb *sql.DB, typeID int64) map[int]float64 {
	if v, ok := posTypeAttrs.Load(typeID); ok {
		return v
	}
	ctx, cancel := context.WithTimeout(context.Background(), starbaseCtxTimeout)
	defer cancel()
	raw, err := evedb.GetTypeAttributes(ctx, sqldb, typeID)
	if err != nil {
		return map[int]float64{}
	}
	m := make(map[int]float64, len(raw))
	for k, v := range raw {
		m[int(k)] = v
	}
	posTypeAttrs.Store(typeID, m)
	return m
}

// posTowerInfo is the cached per-tower half of an engagement decision.
type posTowerInfo struct {
	at     time.Time
	rec    evedb.StarbaseRecord
	policy evedb.StarbasePolicy
}

var posTowerCache syncx.Map[int64, posTowerInfo] // towerID → posTowerInfo

func posTower(sqldb *sql.DB, towerID int64) (posTowerInfo, bool) {
	if v, ok := posTowerCache.Load(towerID); ok {
		if ti := v; time.Since(ti.at) < posPolicyCacheTTL {
			return ti, true
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), starbaseCtxTimeout)
	defer cancel()
	rec, found, err := evedb.GetStarbase(ctx, sqldb, towerID)
	if err != nil || !found {
		return posTowerInfo{}, false
	}
	pol, err := evedb.GetStarbasePolicy(ctx, sqldb, rec)
	if err != nil {
		return posTowerInfo{}, false
	}
	ti := posTowerInfo{at: time.Now(), rec: rec, policy: pol}
	posTowerCache.Store(towerID, ti)
	return ti, true
}

// posEngageCache remembers "should tower T engage pilot C" for a few seconds
// so the loop does not run standings queries every tick for every pilot.
type posEngageEntry struct {
	at     time.Time
	engage bool
}

var posEngageCache syncx.Map[string, posEngageEntry] // "tower:char" → posEngageEntry

func posShouldEngage(sqldb *sql.DB, ss *destiny.SolarSystem, ti posTowerInfo, p *destiny.Entity) bool {
	charID := p.Slim.CharID
	if charID == 0 {
		return false
	}
	key := strconv.FormatInt(ti.rec.EntityID, 10) + ":" + strconv.FormatInt(charID, 10)
	if v, ok := posEngageCache.Load(key); ok {
		if e := v; time.Since(e.at) < posEngageCacheTTL {
			return e.engage
		}
	}
	owner, ownerAlly := ti.rec.OwnerCorpID, ti.rec.OwnerAllianceID
	t := evedb.SentryTarget{
		SameCorp:     p.Slim.CorpID != 0 && p.Slim.CorpID == owner,
		SameAlliance: ownerAlly != 0 && p.Slim.AllianceID == ownerAlly,
		SecStatus:    p.Slim.SecurityStatus,
		Aggressed:    posAggressed(ti.rec.EntityID, charID) || isGlobalCriminal(ss.ID, charID),
		AtWar:        WarShotIsLegal(sqldb, owner, ownerAlly, p.Slim.CorpID, p.Slim.AllianceID),
	}
	if !t.SameCorp && !t.SameAlliance && ti.policy.Sentry.Standing != nil {
		ctx, cancel := context.WithTimeout(context.Background(), starbaseCtxTimeout)
		t.Standing = evedb.OwnerStandingToward(ctx, sqldb, owner, charID, p.Slim.CorpID, p.Slim.AllianceID)
		cancel()
	}
	engage := evedb.SentryShouldEngage(ti.policy.Sentry, t)
	posEngageCache.Store(key, posEngageEntry{at: time.Now(), engage: engage})
	return engage
}

// startPOSGuns registers the gun loop. server_settings starbase.gunsEnabled
// (default on) stops the guns only; the support arrays (posarrays.go) are
// applied by the same pass either way.
func startPOSGuns(tickMgr *tick.Manager, sqldb *sql.DB) {
	if tickMgr == nil || sqldb == nil {
		return
	}
	var enabled = true
	var checkedAt time.Time
	var running sync.Mutex
	tickMgr.RegisterPeriodic("starbase-guns", posGunTickSeconds, func(uint64) bool {
		if !running.TryLock() {
			return true // previous pass still running
		}
		go func() {
			defer running.Unlock()
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[pos-guns] panic: %v", r)
				}
			}()
			if time.Since(checkedAt) > time.Minute {
				ctx, cancel := context.WithTimeout(context.Background(), starbaseCtxTimeout)
				if v, ok, err := evedb.GetServerSetting(ctx, sqldb, "starbase.gunsEnabled"); err == nil && ok {
					enabled = v != "0" && v != "false"
				}
				cancel()
				checkedAt = time.Now()
			}
			if globalSysMgr == nil {
				return
			}
			for _, ss := range globalSysMgr.All() {
				applyPOSArrays(ss, sqldb)
				if enabled {
					runPOSGunsInSystem(ss, sqldb)
				}
			}
		}()
		return true
	})
}

// posPosState is the current posState of a structure ball, -1 when unknown.
func posPosState(ss *destiny.SolarSystem, id uint32) int64 {
	slim, ok := ss.GetStarbaseSlim(id)
	if !ok || slim.PosState == nil {
		return -1
	}
	return *slim.PosState
}

func runPOSGunsInSystem(ss *destiny.SolarSystem, sqldb *sql.DB) {
	structures := ss.SnapshotStructureEntities()
	if len(structures) == 0 {
		return
	}
	// GetAllPlayerEntities keeps rolebot projections (no Conn); posShouldEngage decides.
	players := ss.GetAllPlayerEntities()
	now := time.Now()
	for _, gun := range structures {
		kind := posGunKind(gun.Slim.GroupID)
		if kind == "" || gun.IsDestroyed() {
			continue
		}
		v, _ := posGuns.LoadOrStore(int64(gun.ID), &posGun{})
		st := v
		st.mu.Lock()
		runPOSGun(ss, sqldb, gun, kind, st, players, now)
		st.mu.Unlock()
	}
}

// runPOSGun is one gun's tick. Called with st.mu held.
func runPOSGun(ss *destiny.SolarSystem, sqldb *sql.DB, gun *destiny.Entity, kind string,
	st *posGun, players []*destiny.Entity, now time.Time) {

	towerID := gun.Slim.ControlTowerID
	ts := posPosState(ss, uint32(towerID))
	online := posPosState(ss, gun.ID) == 4 && (ts == 4 || ts == 3)
	if !online || towerID == 0 {
		posDropTarget(ss, gun, kind, st)
		return
	}
	ti, ok := posTower(sqldb, towerID)
	if !ok {
		posDropTarget(ss, gun, kind, st)
		return
	}
	attrs := posAttrs(sqldb, gun.Slim.TypeID)
	if kind == "turret" {
		attrs = withTrackingArrays(attrs, posTowerBonus(towerID))
	}
	reach := posGunReach(kind, attrs)
	gunPos := gun.Pos()

	// Keep the current target while it is still a valid one; otherwise the
	// nearest pilot the Defense tab says to engage.
	var target *destiny.Entity
	if st.target != 0 {
		if t := ss.GetEntity(st.target); posTargetable(t, gunPos, reach) && posShouldEngage(sqldb, ss, ti, t) {
			target = t
		}
	}
	if target == nil {
		posDropTarget(ss, gun, kind, st)
		best := math.MaxFloat64
		for _, p := range players {
			if !posTargetable(p, gunPos, reach) {
				continue
			}
			if d := p.Pos().Sub(gunPos).Len(); d < best && posShouldEngage(sqldb, ss, ti, p) {
				best, target = d, p
			}
		}
		if target == nil {
			return
		}
		st.target = target.ID
		st.lastSeen = time.Time{}
		posLockNotify(gun, target, true)
		// A fresh lock takes the gun's own lock time before the first shot.
		st.nextFire = now.Add(time.Duration(math.Max(attrs[73], attrs[51])/2) * time.Millisecond)
	}
	if now.Before(st.nextFire) {
		return
	}

	switch kind {
	case "turret", "missile":
		cycle := attrs[51] // speed (rate of fire)
		if kind == "missile" && attrs[506] > 0 {
			cycle = attrs[506] // missileLaunchDuration
		}
		if cycle <= 0 {
			cycle = 5000
		}
		st.nextFire = now.Add(time.Duration(cycle) * time.Millisecond)
		posFire(ss, sqldb, gun, kind, st, target, attrs, int64(cycle))
	default:
		dur := attrs[73]
		if dur <= 0 {
			dur = 10000
		}
		st.nextFire = now.Add(time.Duration(dur) * time.Millisecond)
		posEwar(ss, sqldb, gun, kind, st, target, attrs, int64(dur))
	}
}

// withTrackingArrays is a turret sentry's attributes with its tower's Tracking
// Arrays applied to optimal range (54) and tracking speed (160). The cached
// type map is shared, so a boosted gun gets a copy. Pure.
func withTrackingArrays(attrs map[int]float64, b posArrayBonus) map[int]float64 {
	if b.turretRange == 1 && b.turretTrack == 1 {
		return attrs
	}
	out := make(map[int]float64, len(attrs))
	for k, v := range attrs {
		out[k] = v
	}
	out[54] *= b.turretRange
	out[160] *= b.turretTrack
	return out
}

// posGunReach is how far a gun will engage: optimal plus two falloffs for a
// turret, its range for everything else.
func posGunReach(kind string, attrs map[int]float64) float64 {
	switch kind {
	case "turret":
		return attrs[54] + 2*attrs[158]
	case "scram":
		if attrs[103] > 0 {
			return attrs[103]
		}
	}
	if attrs[54] > 0 {
		return attrs[54]
	}
	return 150000
}

// posTargetable is the gun's view of a candidate: a pilot in space here,
// connected or a rolebot projection, visible, not warping, within reach.
func posTargetable(p *destiny.Entity, gunPos destiny.Vec3, reach float64) bool {
	if p == nil || !p.IsPlayer || p.IsDestroyed() {
		return false
	}
	if p.IsUntargetable() || p.IsInWarp() {
		return false
	}
	return p.Pos().Sub(gunPos).Len() <= reach
}

// posDropTarget releases a gun's lock, its fire FX and any held EWAR.
func posDropTarget(ss *destiny.SolarSystem, gun *destiny.Entity, kind string, st *posGun) {
	if st.target == 0 {
		return
	}
	t := ss.GetEntity(st.target)
	if st.ewarOn {
		clearNPCEwar(gun, ss, t, kind)
		st.ewarOn = false
	} else {
		posFireFX(ss, gun, st.target, "", 0, 0, false)
	}
	if t != nil {
		t.ClearAggressor(gun.ID)
		posLockNotify(gun, t, false)
	}
	st.target = 0
}

// posLockNotify drives the victim's "locked by" indicator (OnTarget
// otheradd / otherlost), the same event an NPC lock sends.
func posLockNotify(gun, target *destiny.Entity, add bool) {
	if target == nil || !target.IsPlayer || target.Conn == nil {
		return
	}
	ev := "otheradd"
	if !add {
		ev = "otherlost"
	}
	args := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyList{Items: []marshal.PyRep{marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyString{Value: "OnTarget"},
			marshal.PyString{Value: ev},
			marshal.PyInt{Value: uint64(gun.ID)},
			marshal.PyNone{},
		}}}},
	}}
	_ = server.SendNotification(target.Conn, int64(target.AccountID), "OnMultiEvent", "clientID", args)
}

// posFireFXGUID is the turret effect a POS gun group draws.
func posFireFXGUID(groupID int64) string {
	switch groupID {
	case evedb.GroupMobileProjectileSentry:
		return dogma.FXGUIDProjectileFired
	case evedb.GroupMobileHybridSentry:
		return dogma.FXGUIDHybridFired
	case evedb.GroupMobileMissileSentry:
		return dogma.FXGUIDMissileDeployment
	}
	return dogma.FXGUIDLaser
}

// posFireFX starts (or stops) the gun's fire effect for every pilot who holds
// both balls. moduleID is the gun's own ball ID: StructureSentryGun keys its
// turret by it, as EntityShip does.
func posFireFX(ss *destiny.SolarSystem, gun *destiny.Entity, targetID uint32, guid string,
	chargeTypeID, durationMs int64, start bool) {

	var action marshal.PyRep
	if start {
		ae := destiny.ActiveEffect{
			EntityID: gun.ID, ModuleID: int64(gun.ID), ModuleType: gun.Slim.TypeID,
			TargetID: targetID, OtherType: chargeTypeID, GUID: guid,
			IsOffensive: true, DurationMs: durationMs, StartTime: server.Win32TimeNow(),
		}
		gun.SetActiveEffect(int64(gun.ID), ae)
		action = destiny.BuildEffectAction(ae)
	} else {
		var ae destiny.ActiveEffect
		for _, a := range gun.GetActiveEffects() {
			if a.ModuleID == int64(gun.ID) {
				ae = a
				break
			}
		}
		if ae.GUID == "" {
			return
		}
		gun.ClearActiveEffect(int64(gun.ID))
		var other marshal.PyRep = marshal.PyNone{}
		if ae.OtherType != 0 {
			other = marshal.PyInt{Value: uint64(ae.OtherType)}
		}
		action = destiny.BuildSimpleAction("OnSpecialFX",
			marshal.PyInt{Value: uint64(gun.ID)}, marshal.PyInt{Value: uint64(gun.ID)},
			marshal.PyInt{Value: uint64(gun.Slim.TypeID)}, marshal.PyInt{Value: uint64(targetID)},
			other, marshal.PyList{}, marshal.PyString{Value: ae.GUID},
			marshal.PyInt{Value: 1}, marshal.PyInt{Value: 0}, marshal.PyInt{Value: 0})
	}
	for _, p := range ss.GetAllPlayersInSystem() {
		if p.Conn == nil || !p.KnowsBall(gun.ID) || (start && !p.KnowsBall(targetID)) {
			continue
		}
		p.SendFXStamped(func(stamp uint32) error {
			return destiny.SendActionList(p.Conn, globalNodeID, p.AccountID, stamp, action)
		})
	}
}

// posLoadedCharge is the round in a gun's ammunition bay (flag 27).
type posLoadedCharge struct {
	itemID, typeID, qty int64
}

func posCharge(sqldb *sql.DB, gunID int64) (posLoadedCharge, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), starbaseCtxTimeout)
	defer cancel()
	var c posLoadedCharge
	err := sqldb.QueryRowContext(ctx, `
		SELECT itemID, typeID, quantity FROM entity
		 WHERE locationID = ? AND flag = ? AND quantity > 0
		 ORDER BY itemID LIMIT 1`, gunID, evedb.FlagSentryAmmo).Scan(&c.itemID, &c.typeID, &c.qty)
	return c, err == nil
}

// posConsumesAmmo is the ammunition rule: every round is spent except laser
// crystals, which the RMR data carries as reusable charges.
func posConsumesAmmo(groupID int64) bool {
	return groupID != evedb.GroupMobileLaserSentry
}

func posSpendRound(sqldb *sql.DB, c posLoadedCharge) {
	ctx, cancel := context.WithTimeout(context.Background(), starbaseCtxTimeout)
	defer cancel()
	if c.qty <= 1 {
		_ = evedb.DeleteEntityDeep(ctx, sqldb, c.itemID)
		return
	}
	_, _ = sqldb.ExecContext(ctx, `UPDATE entity SET quantity = quantity - 1 WHERE itemID = ?`, c.itemID)
}

// posFire is one shot (or one missile) from a turret or missile battery.
func posFire(ss *destiny.SolarSystem, sqldb *sql.DB, gun *destiny.Entity, kind string,
	st *posGun, target *destiny.Entity, attrs map[int]float64, cycleMs int64) {

	charge, ok := posCharge(sqldb, int64(gun.ID))
	if !ok {
		// Out of ammunition: keep the lock, stop the fire effect.
		posFireFX(ss, gun, target.ID, "", 0, 0, false)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), starbaseCtxTimeout)
	prof, err := evedb.GetWeaponDamageProfile(ctx, sqldb, gun.Slim.TypeID, charge.typeID)
	cancel()
	if err != nil || prof == nil {
		return
	}
	var flight posMissileFlight
	if kind == "missile" {
		// Out of the charge's own range: hold fire (and the round) rather
		// than launch a missile that can only fizzle.
		flight = posMissileFlightFor(sqldb, charge.typeID)
		if d := target.Pos().Sub(gun.Pos()).Len(); d > flight.velocity*flight.maxFlight {
			return
		}
	}
	if posConsumesAmmo(gun.Slim.GroupID) {
		posSpendRound(sqldb, charge)
	}
	target.AddAggressor(gun.ID, time.Now().Unix())
	needFX := true
	for _, ae := range gun.GetActiveEffects() {
		if ae.ModuleID == int64(gun.ID) && ae.TargetID == target.ID {
			needFX = false
		}
	}
	if needFX {
		posFireFX(ss, gun, target.ID, posFireFXGUID(gun.Slim.GroupID), charge.typeID, cycleMs, true)
	}

	dist := target.Pos().Sub(gun.Pos()).Len()
	mod := 1.0
	if kind == "turret" {
		angular := 0.0
		now := time.Now()
		tp := target.Pos()
		if !st.lastSeen.IsZero() && dist > 0 {
			if dt := now.Sub(st.lastSeen).Seconds(); dt > 0 {
				v := tp.Sub(st.lastPos)
				r := tp.Sub(gun.Pos())
				// |r̂ × v| / |r| = angular velocity about the gun.
				cx := r.Y*v.Z - r.Z*v.Y
				cy := r.Z*v.X - r.X*v.Z
				cz := r.X*v.Y - r.Y*v.X
				angular = math.Sqrt(cx*cx+cy*cy+cz*cz) / (dist * dist) / dt
			}
		}
		st.lastPos, st.lastSeen = tp, now
		sig := posAttrs(sqldb, target.Slim.TypeID)[552]
		if sig <= 0 {
			sig = posDefaultSigRadius
		}
		sig *= target.SigRadiusMult()
		chance := dogma.TurretHitChance(dist, attrs[54], attrs[158], angular, attrs[160], sig,
			dogma.SentrySigResolution(attrs[128]))
		mod = dogma.TurretDamageMod(rand.Float64(), chance)
		if mod == 0 {
			posCombatMessage(gun, target, min(hitQualityID(chance*0.49), 2), 0)
			return
		}
		posApply(ss, sqldb, gun, target, prof, mod)
		return
	}
	posLaunchMissile(ss, sqldb, gun, target, charge.typeID, flight, prof)
}

// posMissileFlight is a charge's flight: m/s and seconds of fuel.
type posMissileFlight struct {
	velocity, maxFlight float64
	groupID, categoryID int64
}

// posMissileFlightFor reads a missile's flight parameters, with the NPC
// path's defaults for a charge that carries none.
func posMissileFlightFor(sqldb *sql.DB, chargeTypeID int64) posMissileFlight {
	f := posMissileFlight{velocity: 3000, maxFlight: 15}
	ctx, cancel := context.WithTimeout(context.Background(), starbaseCtxTimeout)
	defer cancel()
	if mfp, err := evedb.GetMissileFlightParams(ctx, sqldb, chargeTypeID); err == nil && mfp != nil {
		if mfp.Velocity > 0 {
			f.velocity = mfp.Velocity
		}
		if mfp.MaxFlightTime > 0 {
			f.maxFlight = mfp.MaxFlightTime
		}
	}
	f.groupID, f.categoryID, _ = evedb.GetTypeGroupCategory(ctx, sqldb, chargeTypeID)
	return f
}

// posLaunchMissile puts one missile in flight from a battery and lands its
// damage on arrival. Recipients are the pilots who already hold both balls;
// a structure is in no bubble, and the battery must never pass through
// ensureBallsKnown, whose first-sight ball is not the anchored POS ball.
func posLaunchMissile(ss *destiny.SolarSystem, sqldb *sql.DB, gun, target *destiny.Entity,
	chargeTypeID int64, f posMissileFlight, prof *evedb.WeaponDamageProfile) {

	spawn, ballFlight, fizzle := launchMissileBallFor(ss, gun, target, chargeTypeID, f)
	if fizzle {
		return
	}
	launchedAt := time.Now()
	targetID := target.ID
	go func() {
		defer safego.Recover("posLaunchMissile")
		tgt := waitForMissileImpact(ss, spawn, f.velocity, f.maxFlight, targetID)
		if tgt == nil {
			return
		}
		if wait := time.Duration(ballFlight*float64(time.Second)) - time.Since(launchedAt); wait > 0 {
			time.Sleep(wait)
		}
		if ss.GetEntity(targetID) != tgt || tgt.IsDestroyed() || gun.IsDestroyed() {
			return
		}
		posApply(ss, sqldb, gun, tgt, prof, 1.0)
	}()
}

// launchMissileBallFor is the visible half of a missile shot for a shooter
// with no client connection (a POS battery, a module ship): the missile ball
// on every client holding both balls, plus its scheduled removal. Returns the
// spawn point, the seconds to wait before landing damage, and fizzle (cannot reach).
func launchMissileBallFor(ss *destiny.SolarSystem, shooter, target *destiny.Entity,
	chargeTypeID int64, f posMissileFlight) (spawn destiny.Vec3, ballFlight float64, fizzle bool) {

	shooterPos, targetPos := shooter.Pos(), target.Pos()
	spawn = shooterPos.Add(missileHardpointOffset(shooterPos, targetPos, shooter.BallDesc().Radius, 0))
	delta := targetPos.Sub(spawn)
	dist := delta.Len()
	var ballSpeed float64
	var removeAfter time.Duration
	ballSpeed, ballFlight, removeAfter, fizzle = missileTiming(dist, f.velocity, f.maxFlight)
	var vel destiny.Vec3
	if dist > 0 {
		vel = delta.Scale(ballSpeed / dist)
	}
	missileID := ss.NextMissileID()
	ball := destiny.BallDesc{
		EntityID:      missileID,
		Mode:          destiny.ModeMissile,
		SubType:       0x01,
		Pos:           spawn,
		Radius:        20,
		Mass:          1.0,
		CorpID:        uint32(shooter.Slim.CorpID),
		MaxSpeed:      ballSpeed,
		Vel:           vel,
		Agility:       1.0,
		SpeedFraction: 1.0,
		FollowID:      target.ID,
	}
	slim := destiny.SlimItem{
		BallID: int64(missileID), ItemID: int64(missileID), TypeID: chargeTypeID,
		OwnerID: shooter.Slim.OwnerID, CorpID: shooter.Slim.CorpID, AllianceID: shooter.Slim.AllianceID,
		GroupID: f.groupID, CategoryID: f.categoryID,
	}
	dmg := map[uint32]destiny.DamageState{missileID: {Shield: 1.0, Tau: 1e9, Armor: 1.0, Structure: 1.0}}

	var players []*destiny.Entity
	for _, p := range ss.GetAllPlayersInSystem() {
		if p.Conn == nil || !p.KnowsBall(shooter.ID) || (p.ID != target.ID && !p.KnowsBall(target.ID)) {
			continue
		}
		players = append(players, p)
	}
	if len(players) > 0 {
		queueMissileLaunch(players, shooter, target, target.ID, ball, slim, dmg, nil)
		scheduleMissileRemove(players, missileID, true, removeAfter)
	}
	return spawn, ballFlight, fizzle
}

// posApply lands damage on a pilot from a POS gun: HP, HUD, combat log,
// kill credit, and the death path when it is lethal.
func posApply(ss *destiny.SolarSystem, sqldb *sql.DB, gun, target *destiny.Entity,
	prof *evedb.WeaponDamageProfile, mod float64) {

	// Structures are a PvP weapon: the NPC damage rate does not apply.
	m := prof.Multiplier * mod * rates.PlayerDamage
	applied, destroyed := target.ApplyDamage(prof.Em*m, prof.Explosive*m, prof.Kinetic*m, prof.Thermal*m)
	target.RecordKillHit(gun, gun.Slim.TypeID, applied, destroyed)
	posCombatMessage(gun, target, hitQualityID(mod), applied)
	d := target.DamageStateFrac()
	d.Timestamp = server.Win32TimeNow()
	broadcastDamageStateTo(ss, target.ID, d)
	if !destroyed || !target.IsPlayer {
		return
	}
	log.Printf("[pos-guns] structure %d killed %d", gun.ID, target.ID)
	if target.Conn != nil && target.PlayerState != nil {
		queuePlayerDeath(target.PlayerState.(*destinyConn), target.Slim.TypeID == 670)
	} else if target.Conn == nil {
		// A rolebot projection: the module's OnEntityRemoved runs the loss.
		npcDestroyEntity(ss, target, gun.ID)
	}
}

func posCombatMessage(gun, target *destiny.Entity, dmgID int, applied float64) {
	if target.Conn == nil || target.IsInWarp() || target.InLandingGrace() {
		return
	}
	if dmgID < 0 || dmgID >= len(attackMsgTaken) {
		dmgID = 0
	}
	sendCombatMessage(target.Conn, int64(target.AccountID), attackMsgTaken[dmgID], marshal.PyDict{Items: []marshal.DictEntry{
		{Key: marshal.PyString{Value: "source"}, Value: marshal.PyInt{Value: uint64(gun.ID)}},
		{Key: marshal.PyString{Value: "weapon"}, Value: marshal.PyInt{Value: uint64(gun.Slim.TypeID)}},
		{Key: marshal.PyString{Value: "target"}, Value: marshal.PyInt{Value: uint64(target.ID)}},
		{Key: marshal.PyString{Value: "damage"}, Value: marshal.PyReal{Value: applied}},
	}})
}

// posEwarModule maps an EW battery's attributes onto the NPC EWAR module the
// shared applier understands. Pure apart from the attribute map it is given.
func posEwarModule(kind string, attrs map[int]float64, durMs int64) evedb.NPCEwarModule {
	mod := evedb.NPCEwarModule{Kind: kind, Chance: 1, DurationMs: durMs, MaxRange: attrs[54], Falloff: attrs[158]}
	switch kind {
	case "web":
		mod.Multiplier = attrs[20] // speedFactor, negative percent
	case "scram":
		mod.MaxRange = attrs[103]
		mod.Strength = attrs[105]
		if c := attrs[504]; c > 0 {
			mod.Chance = c
		}
	case "sensorDamp":
		switch m := attrs[237]; {
		case m > 0 && m < 1:
			mod.Multiplier = m
		case attrs[309] < 0:
			mod.Multiplier = 1 + attrs[309]/100
		}
	}
	return mod
}

// posEwar is one EW battery cycle.
func posEwar(ss *destiny.SolarSystem, sqldb *sql.DB, gun *destiny.Entity, kind string,
	st *posGun, target *destiny.Entity, attrs map[int]float64, durMs int64) {

	mod := posEwarModule(kind, attrs, durMs)
	target.AddAggressor(gun.ID, time.Now().Unix())
	if kind == "jam" {
		ta := posAttrs(sqldb, target.Slim.TypeID)
		chance := dogma.ECMJamChance(
			[4]float64{attrs[241], attrs[239], attrs[240], attrs[238]},
			[4]float64{ta[208], ta[209], ta[210], ta[211]})
		if rand.Float64() >= chance {
			return
		}
	} else if mod.Chance < 1 && rand.Float64() >= mod.Chance {
		return
	}
	applyNPCEwar(globalNPCMgr, gun, ss, target, mod)
	st.ewarOn = kind != "jam"
	if st.ewarOn {
		// Web / scram / damp are held while the battery keeps cycling; lift
		// them if this was the last cycle (the next tick re-applies).
		tid := target.ID
		time.AfterFunc(time.Duration(durMs+1500)*time.Millisecond, func() {
			v, ok := posGuns.Load(int64(gun.ID))
			if !ok {
				clearNPCEwar(gun, ss, ss.GetEntity(tid), kind)
				return
			}
			g := v
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.target != tid || time.Now().After(g.nextFire.Add(time.Second)) {
				clearNPCEwar(gun, ss, ss.GetEntity(tid), kind)
				if g.target == tid {
					g.ewarOn = false
				}
			}
		})
	}
}
