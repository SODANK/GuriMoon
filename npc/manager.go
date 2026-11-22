package npc

import (
	"context"
	"database/sql"
	"gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/logx"
	"gurimoon/rates"
	"gurimoon/safego"
	"gurimoon/syncx"
	"log"
	"math"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// CombatDebug re-enables the per-hit NPC fire trace. Set once at boot from
// config (server.combatDebug), read without synchronisation. Off by default:
// a busy grid writes one line per shot, and the write is on the combat path.
var CombatDebug bool

// BroadcastFunc is called by the tick loop to broadcast an NPC's destiny
// action to nearby player connections.
type BroadcastFunc func(npcEntity *destiny.Entity, ss *destiny.SolarSystem, action Action)

// CombatBroadcastFunc is called when an NPC fires weapons at a target.
// hitQuality is the tracking modifier (0.0 to 3.0); on a miss, applied is 0.
// cycleTimeMs is the weapon cycle time for FX duration.
type CombatBroadcastFunc func(npcEntity *destiny.Entity, ss *destiny.SolarSystem,
	targetEntity *destiny.Entity, applied float64, destroyed bool, hitQuality float64, cycleTimeMs int64)

// LockBroadcastFunc is called when an NPC locks or unlocks a target.
// event is "add" (lock completed) or "lost" (unlock).
type LockBroadcastFunc func(npcEntity *destiny.Entity, ss *destiny.SolarSystem,
	targetEntity *destiny.Entity, event string)

// DeathBroadcastFunc is called when an NPC is destroyed.
// killerEntityID is the top-threat entity (0 if unknown), bounty is the ISK reward.
type DeathBroadcastFunc func(npcEntity *destiny.Entity, ss *destiny.SolarSystem, killerEntityID uint32, bounty float64)

// PosPersistFunc is called periodically for moving NPCs to persist their live
// position; targets holds the NPC's locked entities, for diagnostics. It must not
// push the position to clients. See docs/destiny-dll-spec.md#warp-kinematics-decoded.
type PosPersistFunc func(npcEntity *destiny.Entity, ss *destiny.SolarSystem, targets []*destiny.Entity)

// MissileLaunchFunc is called when a missile NPC fires. The callback spawns
// the missile ball and schedules delayed damage on impact.
type MissileLaunchFunc func(npcEntity *destiny.Entity, ss *destiny.SolarSystem,
	targetEntity *destiny.Entity, wp *db.NPCWeaponProfile, falloffFactor float64)

// SpawnNotifyFunc is called after an NPC is added to a solar system so that
// nearby players can refresh their ballpark and see the new NPC.
type SpawnNotifyFunc func(npcEntity *destiny.Entity, ss *destiny.SolarSystem)

// StopFireFunc is called when an NPC stops firing at a target (idle, target
// lost, death) so the client can clear the weapon FX and red-box state.
type StopFireFunc func(npcEntity *destiny.Entity, ss *destiny.SolarSystem, targetID uint32)

// DespawnBroadcastFunc removes an NPC ball from the grid without killing it:
// no explosion, no bounty, no wreck. It is the death broadcast's quiet twin,
// used when a rat leaves under its own power (a hauler that warped off).
type DespawnBroadcastFunc func(npcEntity *destiny.Entity, ss *destiny.SolarSystem)

// NPCRecord pairs an entity with its brain and combat state.
type NPCRecord struct {
	Entity      *destiny.Entity
	Brain       Brain
	System      *destiny.SolarSystem
	TypeID      int64
	TypeName    string
	Behavior    *Behavior // role + stance, resolved at spawn
	FactionID   int64  // NPC faction for standings-based hostility
	CelestialID int64  // belt/celestial this NPC belongs to
	SpawnerID   int64  // spawner that created this NPC
	MissionID   int64  // mission this NPC is tied to (0 = none)
	SpawnRole   string // spawn role: normal, hauler, commander, officer (belt rats)
	DunObjectID int64  // dungeon object ID (0 = belt NPC, >0 = dungeon NPC)
	OpenSiteID  int64  // open dungeon site this NPC belongs to (0 = none)

	mu             sync.Mutex
	WeaponProfile  *db.NPCWeaponProfile // loaded at spawn, nil = unarmed
	EwarProfile    *db.NPCEwarProfile   // electronic modules from type attrs, nil = none
	ewarActive     map[string]ewarActive // landed effects by kind
	ewarNextTry    map[string]time.Time  // next roll allowed, by kind
	Targets        []uint32             // locked targets (max 3)
	ewarReadyAt    map[uint32]time.Time // per locked target: no EWAR before this (the post-lock delay)
	LockingTarget  uint32               // currently locking (0 = none)
	LockEndTime    time.Time            // when lock completes
	JamUntil       time.Time            // ECM jam: locks broken, no re-lock until then
	WeaponCooldown time.Time            // next fire allowed
	threat         threatTable
	AggrRange      float64 // engagement range
	// UnarmedFlyRange is attr 416 for an NPC with no weapon profile (see WorldView).
	UnarmedFlyRange float64
	SpawnPos       destiny.Vec3
	// Leash anchor: when set, the NPC idles by circling LeashPos at
	// LeashOrbitDist instead of holding still at SpawnPos (dungeon
	// leashObjectID authoring).
	LeashPos         *destiny.Vec3
	LeashOrbitDist   float64
	Dead             bool
	Bounty           float64                 // ISK bounty paid on kill
	NextThinkTime    time.Time               // unused
	NextMovementTime time.Time               // next movement re-evaluation
	NextCombatTime   time.Time               // next combat brain decision
	pendingCombat    []CombatCmd             // queued combat commands, processed one per tick
	lastTickAt       time.Time               // when tickLoop last handed this record a turn
	ticking          atomic.Bool             // a turn is in flight; set and read by tickLoop, cleared by runTick
	lastAction       Action                  // for deduplication
	lastActionSync   time.Time               // last time lastAction was (re)broadcast
	lastMoveSwitch   time.Time               // last time lastAction.Type changed (dwell gate for orbit/follow)
	lastTargetPos    map[uint32]destiny.Vec3 // for angular velocity calc
	lastTargetPosT   map[uint32]time.Time    // when lastTargetPos was sampled
	lastSelfPos      map[uint32]destiny.Vec3 // NPC's own position at last fire (per target)
	lastSelfPosT     map[uint32]time.Time    // when lastSelfPos was sampled
	lastPosPersist   time.Time               // last time the live position was written to the DB
	lastFireTime     time.Time               // last weapon cycle fired (hit or miss)
	fireFXTarget     uint32                  // target of the last fire FX (0 = none active)
	lastCycleMs      int64                   // cycle time of the last fired weapon

	// Warp-in state.
	WarpingIn     bool
	WarpDest      destiny.Vec3
	WarpStartTime time.Time
	WarpDuration  time.Duration
	WarpInitiated bool // true once the warp command has been sent
	WarpDespawn   bool // the warp ends in a despawn, not a landing

	// Flee state. Set once, never cleared: a rat that has decided to run is
	// committed to it. See beginFlee.
	Fleeing    bool
	FleeWarpAt time.Time // when the align window closes and the warp starts
	FleeHeld   bool      // aligned and ready, but held down by a warp scrambler
}

// NPCManager holds all spawned NPCs and runs the tick loop.
type NPCManager struct {
	mu   sync.RWMutex
	npcs map[uint32]*NPCRecord // entityID -> record

	sqldb           *sql.DB         // for live entity persistence
	Standings       *StandingsMap   // faction-to-faction standings cache
	SpawnerMgr      *SpawnerManager // spawner system for death notifications
	broadcast       BroadcastFunc
	combatBroadcast CombatBroadcastFunc
	lockBroadcast   LockBroadcastFunc
	deathBroadcast  DeathBroadcastFunc
	despawnBroadcast DespawnBroadcastFunc
	missileLaunch   MissileLaunchFunc
	posPersist      PosPersistFunc
	spawnNotify     SpawnNotifyFunc
	stopFire        StopFireFunc
	ewarApply       EwarApplyFunc
	ewarClear       EwarClearFunc
	ewarRefresh     EwarRefreshFunc
	running         bool

	// dirty holds NPCs whose HP or position changed since the last FlushState
	// (persist.go), entityID -> dirtyHP|dirtyPos.
	dirtyMu sync.Mutex
	dirty   map[uint32]uint8

	// sigCache caches signature radius (attribute 552) by typeID to avoid
	// repeated DB lookups during hit chance calculations.
	sigCache syncx.Map[int64, float64] // map[int64]float64

	// ewarCache caches each NPC type's electronic loadout. Most types have
	// none, and a nil entry is cached just as eagerly as a populated one so a
	// belt full of unarmed rats doesn't re-query on every respawn.
	ewarCache syncx.Map[int64, *db.NPCEwarProfile] // map[int64]*db.NPCEwarProfile
}

// NewManager creates an NPCManager with the given broadcast callbacks.
func NewManager(sqldb *sql.DB, broadcast BroadcastFunc, combatBroadcast CombatBroadcastFunc, lockBroadcast LockBroadcastFunc, deathBroadcast DeathBroadcastFunc, missileLaunch MissileLaunchFunc, posPersist PosPersistFunc, spawnNotify SpawnNotifyFunc, stopFire StopFireFunc) *NPCManager {
	return &NPCManager{
		npcs:            make(map[uint32]*NPCRecord),
		sqldb:           sqldb,
		broadcast:       broadcast,
		combatBroadcast: combatBroadcast,
		lockBroadcast:   lockBroadcast,
		deathBroadcast:  deathBroadcast,
		missileLaunch:   missileLaunch,
		posPersist:      posPersist,
		spawnNotify:     spawnNotify,
		stopFire:        stopFire,
	}
}

// SetDespawnBroadcast installs the quiet-removal hook. Kept off NewManager's
// signature, which the rolebots module calls.
func (m *NPCManager) SetDespawnBroadcast(f DespawnBroadcastFunc) {
	m.despawnBroadcast = f
}

// SetEwarCallbacks installs the electronic-warfare hooks. Kept off
// NewManager's signature because the server layer builds them from the destiny
// connection plumbing, which is not available at manager construction time.
func (m *NPCManager) SetEwarCallbacks(apply EwarApplyFunc, clear EwarClearFunc, refresh EwarRefreshFunc) {
	m.ewarApply = apply
	m.ewarClear = clear
	m.ewarRefresh = refresh
}

// getTargetSigRadius returns the signature radius for the given typeID,
// using a cache to avoid repeated DB lookups. Falls back to 125m on error.
func (m *NPCManager) getTargetSigRadius(typeID int64) float64 {
	if typeID <= 0 {
		return 125.0
	}
	if v, ok := m.sigCache.Load(typeID); ok {
		return v
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	attrs, err := db.GetTypeAttributes(ctx, m.sqldb, typeID)
	if err != nil {
		return 125.0
	}
	sig := attrs[552] // signatureRadius
	if sig <= 0 {
		sig = 125.0
	}
	m.sigCache.Store(typeID, sig)
	return sig
}

// FactionUnknown (chrFactions "Unknown") marks an NPC authored factionless: no
// warFactionID on the wire, no standing loss on kill. The ID doubles as the
// SlimItem owner/corp so show-info reads "Unknown" instead of CONCORD.
const FactionUnknown = 500021

// OwnerNone is the ownerCorpID for a ball nobody owns: ownerID/corpID 0 on the
// wire, as asteroids carry. 0 itself means "no preference" and falls through
// to the CONCORD default.
const OwnerNone = -1

// How often a still-current movement command is re-broadcast. Only Goto is
// re-broadcast (applyAction); see docs/npc-behaviour.md#movement-commands.
const (
	combatActionResyncInterval = 2 * time.Second
	idleActionResyncInterval   = 30 * time.Second
)

// npcMovementInterval and npcThinkInterval are the one cadence every NPC runs
// on. Weapon timing is not tied to them: a gun fires when its own cooldown says
// it is ready, at the next think.
const (
	npcMovementInterval = 1 * time.Second
	npcThinkInterval    = 1 * time.Second
)

// posPersistInterval is how often a moving NPC's live position is written to
// the DB (maybePersistPosition). One async UPDATE; nothing goes to a client.
const posPersistInterval = 15 * time.Second

// NPCHPInit holds optional HP/resistance data for an NPC spawn.
type NPCHPInit struct {
	ShieldHP    float64
	ArmorHP     float64
	StructureHP float64
	ShieldTau   float64
	ShieldRes   [4]float64
	ArmorRes    [4]float64
	HullRes     [4]float64
}

// SpawnNPC creates an NPC entity in ss and registers it, returning the entity
// so the caller can send AddBalls. ownerCorpID sets the SlimItem OwnerID/CorpID
// (0 = the CONCORD default, OwnerNone = unowned). notify fires spawnNotify;
// callers that send their own AddBalls/SetState pass false.
func (m *NPCManager) SpawnNPC(ss *destiny.SolarSystem, typeID int64, typeName string,
	pos destiny.Vec3, mass, agility, maxVel, radius float64, stance Stance,
	hpInit *NPCHPInit, weaponProfile *db.NPCWeaponProfile, ownerCorpID int64,
	mayFlee bool, bounty float64, factionID int64, notify bool) *destiny.Entity {

	// Checked before either fallback: an explicitly unowned ball must not be
	// claimed for "Unknown" or for CONCORD on its way through.
	unowned := ownerCorpID == OwnerNone
	if unowned {
		ownerCorpID = 0
	}
	if factionID == FactionUnknown {
		factionID = 0
		if ownerCorpID <= 0 && !unowned {
			ownerCorpID = FactionUnknown
		}
	}
	if ownerCorpID <= 0 && !unowned {
		ownerCorpID = 1000125
	}

	entityID := ss.NextNPCID()
	dm := destiny.New(entityID, mass, agility, maxVel, radius)
	dm.Pos = pos
	dm.Stamp = 1

	// Wire up TargetLookup so Follow/Orbit can resolve entity positions.
	dm.TargetLookup = func(targetID uint32) (destiny.Vec3, bool) {
		if target := ss.GetEntity(targetID); target != nil {
			return target.Pos(), true
		}
		return destiny.Vec3{}, false
	}

	entity := &destiny.Entity{
		ID:       entityID,
		Manager:  dm,
		CharName: typeName,
		BubbleID: -1,
		IsPlayer: false,
		Slim: destiny.SlimItem{
			BallID:       int64(entityID),
			ItemID:       int64(entityID),
			TypeID:       typeID,
			GroupID:      25, // default; caller can override
			CategoryID:   6,  // Ship
			OwnerID:      ownerCorpID,
			CorpID:       ownerCorpID,
			WarFactionID: factionID,
		},
		Damage: destiny.DamageState{Shield: 1.0, Tau: 1e9, Armor: 1.0, Structure: 1.0},
	}
	dm.SetEntity(entity)

	// Attach synthetic turret modules so the client renders turret models on
	// NPC ship hardpoints and OnSpecialFX can reference a valid moduleID.
	if weaponProfile != nil && weaponProfile.TurretTypeID != 0 {
		entity.NPCTurretTypeID = weaponProfile.TurretTypeID
		mods := make([]destiny.ModuleRef, weaponProfile.TurretSlots)
		for i := range mods {
			mods[i] = destiny.ModuleRef{
				ItemID: int64(entityID)*100 + int64(i),
				TypeID: weaponProfile.TurretTypeID,
			}
		}
		entity.Slim.Modules = mods
	}

	if hpInit != nil {
		entity.HP = destiny.HPState{
			MaxShield: hpInit.ShieldHP, CurShield: hpInit.ShieldHP,
			MaxArmor: hpInit.ArmorHP, CurArmor: hpInit.ArmorHP,
			MaxStructure: hpInit.StructureHP, CurStructure: hpInit.StructureHP,
			ShieldTau: hpInit.ShieldTau,
		}
		entity.Resists = [3][4]float64{hpInit.ShieldRes, hpInit.ArmorRes, hpInit.HullRes}
	} else {
		// Default HP so HasHP() returns true and entity can take damage.
		entity.HP = destiny.HPState{
			MaxStructure: 100, CurStructure: 100,
		}
		// Resonance 1.0 = no resistance; the zero value would make the NPC
		// immune to all damage.
		entity.Resists = [3][4]float64{
			{1, 1, 1, 1}, {1, 1, 1, 1}, {1, 1, 1, 1},
		}
	}

	if _, _, ok := ss.AddEntity(entity); !ok {
		// System torn down concurrently: do not register a record no tick loop
		// will drive.
		return nil
	}

	// Aggro range: the larger of entityAttackRange and entityChaseMaxDistance. An NPC with no
	// weapon profile (unarmed, or electronic warfare only) reads them off its type directly.
	aggrRange := 80000.0 // 80km default when no profile data
	var unarmedFly float64
	if weaponProfile != nil {
		r := math.Max(weaponProfile.AttackRange, weaponProfile.ChaseMaxDist)
		if r > 0 {
			aggrRange = r
		}
	} else if m.sqldb != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		ai, err := db.GetNPCAIRanges(ctx, m.sqldb, typeID)
		cancel()
		if err == nil {
			if r := math.Max(ai.AttackRange, ai.ChaseMaxDist); r > 0 {
				aggrRange = r
			}
			unarmedFly = ai.FlyRange
		}
	}

	// Behaviour is derived from the type data the record carries, so no caller
	// can hand a hull a role it does not have. An electronic module counts as armed.
	ewarProfile := m.ewarProfileFor(typeID)
	armed := !IsUnarmed(weaponProfile) || (ewarProfile != nil && len(ewarProfile.Modules) > 0)
	behavior := NewBehavior(DeriveRole(armed, maxVel > 0, mayFlee), stance)

	// Stagger initial think times so NPCs from the same spawn don't all
	// act on the exact same tick, preventing synchronized broadcast bursts.
	now := time.Now()
	jitter := time.Duration(rand.Intn(1500)) * time.Millisecond

	m.mu.Lock()
	m.npcs[entityID] = &NPCRecord{
		Entity:           entity,
		Brain:            &StandardBrain{Behavior: behavior},
		System:           ss,
		TypeID:           typeID,
		TypeName:         typeName,
		Behavior:         behavior,
		FactionID:        factionID,
		WeaponProfile:    weaponProfile,
		EwarProfile:      ewarProfile,
		ewarActive:       make(map[string]ewarActive),
		ewarNextTry:      make(map[string]time.Time),
		threat:           newThreatTable(),
		AggrRange:        aggrRange,
		UnarmedFlyRange:  unarmedFly,
		SpawnPos:         pos,
		Bounty:           bounty,
		NextMovementTime: now.Add(jitter),
		NextCombatTime:   now.Add(jitter + 500*time.Millisecond),
	}
	if !m.running {
		m.running = true
		safego.Forever("npc.tickLoop", m.tickLoop)
	}
	m.mu.Unlock()

	log.Printf("[npc] spawned %s (typeID=%d) entityID=%d %s in system %d at (%.0f, %.0f, %.0f)",
		typeName, typeID, entityID, behavior, ss.ID, pos.X, pos.Y, pos.Z)

	// Notify nearby players so they refresh their ballpark.
	if notify && m.spawnNotify != nil {
		m.spawnNotify(entity, ss)
	}

	// Async persist to npcLiveEntities.
	if m.sqldb != nil {
		go func() {
			defer safego.Recover("SpawnNPC")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			// Read CelestialID/SpawnerID, which may be set shortly after spawn.
			m.mu.RLock()
			rec, ok := m.npcs[entityID]
			m.mu.RUnlock()
			var celestialID, spawnerID, recFactionID, corpID int64
			var recBounty float64
			var groupID, categoryID int64 = 25, 6
			if ok {
				rec.mu.Lock()
				celestialID = rec.CelestialID
				spawnerID = rec.SpawnerID
				recFactionID = rec.FactionID
				corpID = int64(entity.Slim.CorpID)
				recBounty = rec.Bounty
				rec.mu.Unlock()
				groupID = entity.Slim.GroupID
				categoryID = entity.Slim.CategoryID
			}
			if err := db.InsertLiveNPC(ctx, m.sqldb, entityID, typeID, typeName,
				int64(ss.ID), celestialID, spawnerID, pos.X, pos.Y, pos.Z,
				recFactionID, corpID, recBounty, groupID, categoryID); err != nil {
				log.Printf("[npc] InsertLiveNPC entityID=%d: %v", entityID, err)
			}
		}()
	}

	return entity
}

// SetGroupCategory sets the groupID and categoryID on a spawned NPC entity's SlimItem.
func SetGroupCategory(entity *destiny.Entity, groupID, categoryID int64) {
	entity.Slim.GroupID = groupID
	entity.Slim.CategoryID = categoryID
}

// NotifySpawn fires the spawn-notify callback for an NPC spawned with
// notify=false, once its SlimItem is final. The client picks its spaceObject
// class from the slim's groupID, so notifying before SetGroupCategory renders
// the NPC as a player ship.
func (m *NPCManager) NotifySpawn(entity *destiny.Entity, ss *destiny.SolarSystem) {
	if m.spawnNotify != nil {
		m.spawnNotify(entity, ss)
	}
}

// SetWeaponProfile updates the weapon profile on an existing NPC.
func (m *NPCManager) SetWeaponProfile(entityID uint32, profile *db.NPCWeaponProfile) {
	m.mu.RLock()
	rec, ok := m.npcs[entityID]
	m.mu.RUnlock()
	if !ok {
		return
	}
	rec.mu.Lock()
	rec.WeaponProfile = profile
	rec.mu.Unlock()
}

// SetWarpIn configures an NPC to warp to the given destination on its first tick.
func (m *NPCManager) SetWarpIn(entityID uint32, dest destiny.Vec3) {
	m.mu.RLock()
	rec, ok := m.npcs[entityID]
	m.mu.RUnlock()
	if !ok {
		return
	}
	rec.mu.Lock()
	rec.WarpingIn = true
	rec.WarpDest = dest
	rec.WarpStartTime = time.Now()
	// Compute warp duration from entity's destiny manager.
	dm := rec.Entity.Manager
	if dm != nil {
		_, _, total := dm.WarpPhases(dest)
		rec.WarpDuration = time.Duration(total * float64(time.Second))
	} else {
		rec.WarpDuration = 6 * time.Second
	}
	rec.mu.Unlock()
	log.Printf("[npc] entityID=%d set to warp-in (duration=%.1fs)", entityID, rec.WarpDuration.Seconds())
}

// SetAggrRange overrides the aggression range on an existing NPC.
// A value of 0 is treated as "use default" and is ignored, since 0
// effectively disables all targeting (pickTarget uses aggrRange+1 as
// the initial distance threshold).
func (m *NPCManager) SetAggrRange(entityID uint32, aggrRange float64) {
	if aggrRange <= 0 {
		return
	}
	m.mu.RLock()
	rec, ok := m.npcs[entityID]
	m.mu.RUnlock()
	if !ok {
		return
	}
	rec.mu.Lock()
	rec.AggrRange = aggrRange
	rec.mu.Unlock()
	log.Printf("[npc] entityID=%d aggression range set to %.0f", entityID, aggrRange)
}

// JamTargets applies an ECM jam: all locks drop and re-locking is blocked
// until the given time. Broadcasts "lost" for each dropped lock so victims
// see the NPC's targeting box clear.
func (m *NPCManager) JamTargets(npcEntityID uint32, until time.Time) {
	m.mu.RLock()
	rec, ok := m.npcs[npcEntityID]
	m.mu.RUnlock()
	if !ok {
		return
	}
	rec.mu.Lock()
	dropped := rec.Targets
	rec.Targets = nil
	rec.LockingTarget = 0
	rec.JamUntil = until
	entity := rec.Entity
	ss := rec.System
	rec.mu.Unlock()
	// Losing every lock also drops whatever this NPC was projecting.
	m.clearAllEwar(rec)
	log.Printf("[npc] entityID=%d JAMMED until %v (%d locks dropped)", npcEntityID, until, len(dropped))
	if m.lockBroadcast != nil && ss != nil {
		for _, tid := range dropped {
			if te := ss.GetEntity(tid); te != nil {
				m.lockBroadcast(entity, ss, te, "lost")
			}
		}
	}
}

// NotifyDamage is called from player weapon code when an NPC takes damage.
// It feeds the threat table and shares aggro with faction-mates on the grid.
func (m *NPCManager) NotifyDamage(npcEntityID, attackerID uint32, damage float64) {
	m.mu.RLock()
	rec, ok := m.npcs[npcEntityID]
	m.mu.RUnlock()
	if !ok {
		return
	}
	// A GM in /gm mode leaves no trace on the threat table: an entry the picker
	// refuses every tick keeps the NPC in combat forever and seeds its mates.
	rec.mu.Lock()
	sys := rec.System
	victim := rec.Entity
	rec.mu.Unlock()
	if sys != nil {
		att := sys.GetEntity(attackerID)
		// Bystanders on the grid get the hit in their combat log.
		destiny.ObserveCombat(sys, victim, att, damage, victim != nil && victim.IsDestroyed())
		if att != nil && att.IsGMMode() {
			return
		}
	}

	rec.mu.Lock()
	isNew := !rec.threat.Has(attackerID)
	rec.threat.Record(attackerID, damage)
	// Wake the combat brain immediately so the NPC reacts on the next
	// 500ms tick instead of waiting up to 2s for the scheduled Think.
	rec.NextCombatTime = time.Now()
	factionID := rec.FactionID
	bubbleID := rec.Entity.BubbleID
	var sysID int64
	if rec.System != nil {
		sysID = rec.System.ID
	}
	rec.mu.Unlock()

	// First hit from this attacker: share aggro with faction-mates on grid.
	if isNew && factionID != 0 {
		go m.shareFactionAggro(npcEntityID, factionID, bubbleID, sysID, attackerID)
	}
}

// SeedThreat records threat on an NPC without telling its faction-mates on
// grid and without a combat-log line: a dispatched police or customs pin. Police
// share an empire faction with the gate guns and customs, so NotifyDamage's
// shareFactionAggro would turn them on the same pilot.
func (m *NPCManager) SeedThreat(npcEntityID, targetID uint32, amount float64) {
	m.mu.RLock()
	rec, ok := m.npcs[npcEntityID]
	m.mu.RUnlock()
	if !ok {
		return
	}
	rec.mu.Lock()
	rec.threat.Record(targetID, amount)
	rec.NextCombatTime = time.Now()
	rec.mu.Unlock()
}

// ClearThreat drops one attacker from an NPC's threat table and releases any
// lock on it, broadcasting "lost". The inverse of a NotifyDamage threat seed,
// for a pin that has lapsed (a sentry's criminal timer ran out).
func (m *NPCManager) ClearThreat(npcEntityID, targetID uint32) {
	m.mu.RLock()
	rec, ok := m.npcs[npcEntityID]
	m.mu.RUnlock()
	if !ok {
		return
	}
	rec.mu.Lock()
	rec.threat.Clear(targetID)
	hadLock := rec.hasTarget(targetID)
	rec.removeTarget(targetID)
	if rec.LockingTarget == targetID {
		rec.LockingTarget = 0
	}
	entity := rec.Entity
	ss := rec.System
	rec.mu.Unlock()

	if hadLock && m.lockBroadcast != nil && ss != nil {
		if te := ss.GetEntity(targetID); te != nil {
			m.lockBroadcast(entity, ss, te, "lost")
		}
	}
}

// ForgetTarget drops targetID from every NPC's threat table, lock state and
// EWAR across the whole node, broadcasting "lost" for each released lock. For
// /gm on: the pickers only refuse the next lock, and a threat entry never
// expires on its own.
func (m *NPCManager) ForgetTarget(targetID uint32) int {
	return m.forgetTarget(targetID, nil)
}

// ForgetTargetInSystem is ForgetTarget narrowed to one solar system, for the
// death path: fireWeapons clears only the killer's own target, and another rat
// holding the victim scrammed would keep it until its own bubble prune.
func (m *NPCManager) ForgetTargetInSystem(ss *destiny.SolarSystem, targetID uint32) int {
	return m.forgetTarget(targetID, ss)
}

// forgetTarget is the shared sweep; onlySystem nil means the whole node. It
// collects under RLock and mutates each record outside it, so one command does
// not stall every NPC on the node. clearEwarOnTarget takes rec.mu itself and
// runs between one record's unlock and the next's lock.
func (m *NPCManager) forgetTarget(targetID uint32, onlySystem *destiny.SolarSystem) int {
	m.mu.RLock()
	recs := make([]*NPCRecord, 0, len(m.npcs))
	for _, rec := range m.npcs {
		if rec != nil {
			recs = append(recs, rec)
		}
	}
	m.mu.RUnlock()

	cleared := 0
	for _, rec := range recs {
		rec.mu.Lock()
		// The system filter is applied here: rec.System is written under rec.mu,
		// so reading it under m.mu alone would be a data race.
		if onlySystem != nil && rec.System != onlySystem {
			rec.mu.Unlock()
			continue
		}
		had := rec.threat.Has(targetID) || rec.hasTarget(targetID) || rec.LockingTarget == targetID
		rec.threat.Clear(targetID)
		hadLock := rec.hasTarget(targetID)
		rec.removeTarget(targetID)
		if rec.LockingTarget == targetID {
			rec.LockingTarget = 0
		}
		entity := rec.Entity
		ss := rec.System
		rec.mu.Unlock()

		if had {
			cleared++
		}
		// EWAR before the lock broadcast: on the death path this sweep races the
		// ball removal that makes ss.GetEntity answer nil.
		m.clearEwarOnTarget(rec, targetID)
		if hadLock && m.lockBroadcast != nil && ss != nil {
			if te := ss.GetEntity(targetID); te != nil {
				m.lockBroadcast(entity, ss, te, "lost")
			}
		}
	}
	return cleared
}

// ThreatAttackers returns the entity IDs that have dealt damage to an NPC. The
// threat table is the only record of who shot an NPC: Entity.RecordKillHit is
// called on the target.IsPlayer branch only, so KillHitsSnapshot on an NPC hull
// is always empty.
func (m *NPCManager) ThreatAttackers(npcEntityID uint32) []uint32 {
	m.mu.RLock()
	rec, ok := m.npcs[npcEntityID]
	m.mu.RUnlock()
	if !ok {
		return nil
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	entries := rec.threat.Entries()
	out := make([]uint32, 0, len(entries))
	for _, e := range entries {
		if e.TotalDamage > 0 {
			out = append(out, e.EntityID)
		}
	}
	return out
}

// shareGroupAggro seeds the threat table of every spawner-mate with the newly
// locked target so the spawn group engages together. Mates match on spawnerID
// and celestialID: belt spawners share spawnerID 0 and spawner rows are cloned
// per celestial. The seed damage is small so real attackers outrank it.
func (m *NPCManager) shareGroupAggro(sourceID uint32, spawnerID, celestialID int64, sysID int64, targetID uint32) {
	defer safego.Recover("shareGroupAggro")
	m.mu.RLock()
	mates := make([]*NPCRecord, 0, 8)
	for _, rec := range m.npcs {
		if rec == nil || rec.Entity == nil || rec.Entity.ID == sourceID {
			continue
		}
		if rec.SpawnerID != spawnerID || rec.CelestialID != celestialID ||
			rec.System == nil || rec.System.ID != sysID {
			continue
		}
		mates = append(mates, rec)
	}
	m.mu.RUnlock()
	for _, rec := range mates {
		rec.mu.Lock()
		if !rec.Dead && !rec.threat.Has(targetID) {
			rec.threat.Record(targetID, 1.0)
			rec.NextCombatTime = time.Now() // wake brain immediately
		}
		rec.mu.Unlock()
	}
}

// shareFactionAggro seeds threat in every NPC of the same faction on the same
// grid (bubble), so attacking one aggros the faction in visual range, neutral
// or friendly NPCs included.
func (m *NPCManager) shareFactionAggro(sourceID uint32, factionID int64, bubbleID int, sysID int64, targetID uint32) {
	defer safego.Recover("shareFactionAggro")
	m.mu.RLock()
	mates := make([]*NPCRecord, 0, 16)
	for _, rec := range m.npcs {
		if rec == nil || rec.Entity == nil || rec.Entity.ID == sourceID {
			continue
		}
		if rec.FactionID != factionID || rec.System == nil || rec.System.ID != sysID {
			continue
		}
		if rec.Entity.BubbleID != bubbleID {
			continue
		}
		mates = append(mates, rec)
	}
	m.mu.RUnlock()
	for _, rec := range mates {
		rec.mu.Lock()
		if !rec.Dead && !rec.threat.Has(targetID) {
			rec.threat.Record(targetID, 1.0)
			rec.NextCombatTime = time.Now() // wake brain immediately
		}
		rec.mu.Unlock()
	}
}

// SetMissionID tags an NPC with a mission ID for kill tracking.
func (m *NPCManager) SetMissionID(entityID uint32, missionID int64) {
	m.mu.RLock()
	rec := m.npcs[entityID]
	m.mu.RUnlock()
	if rec != nil {
		rec.mu.Lock()
		rec.MissionID = missionID
		rec.mu.Unlock()
	}
}

// GetMissionID returns the missionID associated with an NPC, or 0 if none.
func (m *NPCManager) GetMissionID(entityID uint32) int64 {
	m.mu.RLock()
	rec := m.npcs[entityID]
	m.mu.RUnlock()
	if rec != nil {
		return rec.MissionID
	}
	return 0
}

// GetSpawnRole returns the spawn role of an NPC ("normal", "hauler",
// "commander", "officer"), or "" if the entity is unknown. Used by the loot
// path to gate role-restricted drops (e.g. commander-only items).
func (m *NPCManager) GetSpawnRole(entityID uint32) string {
	m.mu.RLock()
	rec := m.npcs[entityID]
	m.mu.RUnlock()
	if rec != nil {
		return rec.SpawnRole
	}
	return ""
}

// SetLeash anchors an NPC's idle movement to a world position: with no
// combat target it circles pos at orbitDist instead of sitting still.
func (m *NPCManager) SetLeash(entityID uint32, pos destiny.Vec3, orbitDist float64) {
	m.mu.RLock()
	rec := m.npcs[entityID]
	m.mu.RUnlock()
	if rec == nil || orbitDist <= 0 {
		return
	}
	rec.mu.Lock()
	p := pos
	rec.LeashPos = &p
	rec.LeashOrbitDist = orbitDist
	rec.mu.Unlock()
}

// GetDunObjectID returns the dungeon objectID an NPC was spawned from,
// or 0 for non-dungeon NPCs / unknown entities.
func (m *NPCManager) GetDunObjectID(entityID uint32) int64 {
	m.mu.RLock()
	rec := m.npcs[entityID]
	m.mu.RUnlock()
	if rec == nil {
		return 0
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.DunObjectID
}

// SetDunObjectID tags an NPC with a dungeon object ID for table routing.
func (m *NPCManager) SetDunObjectID(entityID uint32, objectID int64) {
	m.mu.RLock()
	rec := m.npcs[entityID]
	m.mu.RUnlock()
	if rec != nil {
		rec.mu.Lock()
		rec.DunObjectID = objectID
		rec.mu.Unlock()
	}
}

// FindEntityByDunObject returns the entity ID of a live NPC spawned from the
// given dungeon objectID and missionID, or 0. Detects system-level dungeon NPCs
// already spawned before a player warps in.
func (m *NPCManager) FindEntityByDunObject(missionID, dunObjectID int64) uint32 {
	if missionID <= 0 || dunObjectID <= 0 {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for eid, rec := range m.npcs {
		if rec == nil {
			continue
		}
		rec.mu.Lock()
		match := rec.MissionID == missionID && rec.DunObjectID == dunObjectID
		rec.mu.Unlock()
		if match {
			return eid
		}
	}
	return 0
}

// DespawnMission quietly removes every live NPC tagged with missionID (no
// bounty, no wreck; see Despawn) and returns how many went. Used when a mission
// ends without the site being cleared.
func (m *NPCManager) DespawnMission(missionID int64, reason string) int {
	if missionID <= 0 {
		return 0
	}
	m.mu.RLock()
	var ids []uint32
	for eid, rec := range m.npcs {
		if rec == nil {
			continue
		}
		rec.mu.Lock()
		match := rec.MissionID == missionID && !rec.Dead
		rec.mu.Unlock()
		if match {
			ids = append(ids, eid)
		}
	}
	m.mu.RUnlock()
	n := 0
	for _, eid := range ids {
		if m.Despawn(eid, reason) {
			n++
		}
	}
	return n
}

// SetOpenSiteID tags an NPC as part of a spawner-placed open dungeon site.
// Kept apart from MissionID so no mission path ever sees a site NPC.
func (m *NPCManager) SetOpenSiteID(entityID uint32, siteID int64) {
	m.mu.RLock()
	rec := m.npcs[entityID]
	m.mu.RUnlock()
	if rec != nil {
		rec.mu.Lock()
		rec.OpenSiteID = siteID
		rec.mu.Unlock()
	}
}

// GetOpenSiteID returns the open site an NPC belongs to, or 0.
func (m *NPCManager) GetOpenSiteID(entityID uint32) int64 {
	m.mu.RLock()
	rec := m.npcs[entityID]
	m.mu.RUnlock()
	if rec == nil {
		return 0
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.OpenSiteID
}

// FindEntityByOpenSiteObject is FindEntityByDunObject for open sites: the live
// NPC an earlier visitor spawned from this object, so a later one adopts it
// instead of building a second copy of the site.
func (m *NPCManager) FindEntityByOpenSiteObject(siteID, dunObjectID int64) uint32 {
	if siteID <= 0 || dunObjectID <= 0 {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for eid, rec := range m.npcs {
		if rec == nil {
			continue
		}
		rec.mu.Lock()
		match := !rec.Dead && rec.OpenSiteID == siteID && rec.DunObjectID == dunObjectID
		rec.mu.Unlock()
		if match {
			return eid
		}
	}
	return 0
}

// OpenSiteEntities returns every live NPC of an open site.
func (m *NPCManager) OpenSiteEntities(siteID int64) []uint32 {
	if siteID <= 0 {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []uint32
	for eid, rec := range m.npcs {
		if rec == nil {
			continue
		}
		rec.mu.Lock()
		if !rec.Dead && rec.OpenSiteID == siteID {
			out = append(out, eid)
		}
		rec.mu.Unlock()
	}
	return out
}

// SyncHPToDB marks the NPC's HP for the next batched write (persist.go). It used
// to write the row on every hit, one goroutine and one commit each.
func (m *NPCManager) SyncHPToDB(entityID uint32) {
	m.markDirty(entityID, dirtyHP)
}

// Remove removes an NPC from the manager (e.g. after death).
func (m *NPCManager) Remove(entityID uint32) {
	m.mu.Lock()
	delete(m.npcs, entityID)
	m.mu.Unlock()
	m.forgetDirty(entityID)
}

// Kill runs the NPC death path: lock-lost broadcasts, the death broadcast
// (bounty, mission kill tracking, explosion and removal), spawner notification
// and live-row cleanup. killerID 0 falls back to the top-threat attacker.
// Returns false if entityID is not a managed NPC, true otherwise (already dead included).
func (m *NPCManager) Kill(entityID, killerID uint32) bool {
	m.mu.RLock()
	rec, ok := m.npcs[entityID]
	m.mu.RUnlock()
	if !ok {
		return false
	}

	rec.mu.Lock()
	if rec.Dead {
		rec.mu.Unlock()
		return true
	}
	rec.Dead = true
	if killerID == 0 {
		killerID = rec.threat.Top()
	}
	bounty := rec.Bounty
	entity := rec.Entity
	ss := rec.System
	lockedTargets := make([]uint32, len(rec.Targets))
	copy(lockedTargets, rec.Targets)
	rec.mu.Unlock()

	// Lift any electronic warfare this NPC had applied; nothing else would.
	m.clearAllEwar(rec)

	// Send "lost" to all targets this NPC had locked.
	if m.lockBroadcast != nil && ss != nil {
		for _, tid := range lockedTargets {
			if te := ss.GetEntity(tid); te != nil {
				m.lockBroadcast(entity, ss, te, "lost")
			}
		}
	}
	// Broadcast death and remove.
	if m.deathBroadcast != nil {
		m.deathBroadcast(entity, ss, killerID, bounty)
	}
	if m.SpawnerMgr != nil {
		m.SpawnerMgr.NotifyDeath(entityID)
	}
	m.Remove(entityID)
	// Async remove from npcLiveEntities.
	if m.sqldb != nil {
		go func(eid uint32) {
			defer safego.Recover("Kill")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := db.DeleteLiveNPC(ctx, m.sqldb, eid); err != nil {
				log.Printf("[npc] DeleteLiveNPC entityID=%d: %v", eid, err)
			}
		}(entityID)
	}
	return true
}

// Flee timings. The align window is the kill window: a despawn drops no loot
// and pays nothing. fleeWarpDistance must clear the dll's 1e10 m warp floor,
// under which a warp becomes a sub-warp GotoPoint. See docs/npc-behaviour.md#flee-and-despawn.
const (
	fleeWarpDuration = 5 * time.Second
	fleeWarpDistance = 2e11 // ~1.3 AU, comfortably past the dll's 1e10 m floor
)

// beginFlee commits an NPC to running: it drops every lock, turns away from
// awayFrom, and arms the warp-out that tickNPC fires once the align window
// closes. Fleeing is one-way; nothing clears it.
func (m *NPCManager) beginFlee(rec *NPCRecord, awayFrom uint32) {
	rec.mu.Lock()
	if rec.Dead || rec.Fleeing || rec.WarpingIn {
		rec.mu.Unlock()
		return
	}
	entity := rec.Entity
	ss := rec.System
	rec.Fleeing = true
	rec.FleeWarpAt = time.Now().Add(fleeAlignDelay())
	staleTargets := make([]uint32, len(rec.Targets))
	copy(staleTargets, rec.Targets)
	rec.Targets = nil
	rec.LockingTarget = 0

	// Direction: straight away from the threat, else out from the spawn point,
	// else a fixed axis.
	selfPos := entity.Pos()
	dir := destiny.Vec3{X: 1}
	if ss != nil {
		if threat := ss.GetEntity(awayFrom); threat != nil {
			if away := selfPos.Sub(threat.Pos()); !away.IsZero() {
				dir = away.Normalize()
			}
		} else if away := selfPos.Sub(rec.SpawnPos); !away.IsZero() {
			dir = away.Normalize()
		}
	}
	rec.WarpDest = selfPos.Add(dir.Scale(fleeWarpDistance))
	dest := rec.WarpDest
	rec.mu.Unlock()

	m.clearAllEwar(rec)

	// Whoever it had locked must lose the box now, not in nine seconds when
	// the ball disappears.
	if m.lockBroadcast != nil && ss != nil {
		for _, tid := range staleTargets {
			if te := ss.GetEntity(tid); te != nil {
				m.lockBroadcast(entity, ss, te, "lost")
			}
		}
	}
	for _, tid := range staleTargets {
		if m.stopFire != nil && ss != nil {
			m.stopFire(entity, ss, tid)
		}
	}

	// Align: a Goto at the warp destination, which applyAction turns into a
	// GotoDirection down the same vector.
	m.applyAction(rec, Action{Type: ActionGoto, Pos: dest})
	log.Printf("[npc] entityID=%d fleeing (aligning off %d, warp in %.0fs)",
		entity.ID, awayFrom, fleeAlignDelay().Seconds())
}

// Despawn removes an NPC from space quietly: no explosion, no bounty, no wreck,
// no killmail. The spawner is still told (NotifyDeath), so the slot frees, the
// npcBeltRats row is settled and the belt can reseed.
func (m *NPCManager) Despawn(entityID uint32, reason string) bool {
	m.mu.RLock()
	rec, ok := m.npcs[entityID]
	m.mu.RUnlock()
	if !ok {
		return false
	}

	rec.mu.Lock()
	if rec.Dead {
		rec.mu.Unlock()
		return true
	}
	rec.Dead = true
	entity := rec.Entity
	ss := rec.System
	lockedTargets := make([]uint32, len(rec.Targets))
	copy(lockedTargets, rec.Targets)
	rec.Targets = nil
	rec.LockingTarget = 0
	rec.mu.Unlock()

	m.clearAllEwar(rec)

	if m.lockBroadcast != nil && ss != nil {
		for _, tid := range lockedTargets {
			if te := ss.GetEntity(tid); te != nil {
				m.lockBroadcast(entity, ss, te, "lost")
			}
		}
	}
	if m.despawnBroadcast != nil {
		m.despawnBroadcast(entity, ss)
	}
	if m.SpawnerMgr != nil {
		m.SpawnerMgr.NotifyDeath(entityID)
	}
	m.Remove(entityID)
	if m.sqldb != nil {
		go func(eid uint32) {
			defer safego.Recover("Despawn")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := db.DeleteLiveNPC(ctx, m.sqldb, eid); err != nil {
				log.Printf("[npc] DeleteLiveNPC entityID=%d: %v", eid, err)
			}
		}(entityID)
	}
	log.Printf("[npc] entityID=%d despawned (%s)", entityID, reason)
	return true
}

// RemoveSystem despawns every NPC of a solar system when it is torn down, so
// nothing ticks against a dead system and the per-system ID counter can
// restart without collisions. Returns the number removed.
func (m *NPCManager) RemoveSystem(sysID int64) int {
	m.mu.Lock()
	removed := 0
	var doomed []*NPCRecord
	for id, rec := range m.npcs {
		if rec.System != nil && rec.System.ID == sysID {
			rec.mu.Lock()
			rec.Dead = true // in-flight tick goroutines bail on this flag
			rec.mu.Unlock()
			delete(m.npcs, id)
			doomed = append(doomed, rec)
			removed++
		}
	}
	m.mu.Unlock()

	// Lift EWAR outside the manager lock: the clear callback reaches back into
	// the server layer.
	for _, rec := range doomed {
		m.clearAllEwar(rec)
	}
	if removed > 0 {
		log.Printf("[npc] RemoveSystem sysID=%d: despawned %d NPCs", sysID, removed)
	}
	if m.sqldb != nil {
		go func() {
			defer safego.Recover("RemoveSystem")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := db.DeleteLiveNPCsBySystem(ctx, m.sqldb, sysID); err != nil {
				log.Printf("[npc] DeleteLiveNPCsBySystem sysID=%d: %v", sysID, err)
			}
		}()
	}
	return removed
}

// GetTargetersOf returns the entity IDs of all NPCs that have the given
// entityID in their locked targets list.
func (m *NPCManager) GetTargetersOf(entityID uint32) []uint32 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []uint32
	for _, rec := range m.npcs {
		rec.mu.Lock()
		for _, tid := range rec.Targets {
			if tid == entityID {
				result = append(result, rec.Entity.ID)
				break
			}
		}
		rec.mu.Unlock()
	}
	return result
}

// IsNPC returns true if the given entityID is managed by this NPCManager.
func (m *NPCManager) IsNPC(entityID uint32) bool {
	m.mu.RLock()
	_, ok := m.npcs[entityID]
	m.mu.RUnlock()
	return ok
}

// Count reports how many NPCs are spawned right now, for the ESP dashboard.
func (m *NPCManager) Count() int {
	m.mu.RLock()
	n := len(m.npcs)
	m.mu.RUnlock()
	return n
}

// Get returns the NPCRecord for a given entityID, or nil if not found.
func (m *NPCManager) Get(entityID uint32) *NPCRecord {
	m.mu.RLock()
	rec := m.npcs[entityID]
	m.mu.RUnlock()
	return rec
}

// npcTickJob is one record's turn, handed from tickLoop to a worker.
type npcTickJob struct {
	rec              *NPCRecord
	delta            time.Duration
	doMove, doCombat bool
}

// npcTickWorkers sizes the tick pool. A turn can block (a broadcast on a slow
// socket, a kill writing loot), so the pool is a multiple of the cores.
func npcTickWorkers() int {
	return max(8, 4*runtime.GOMAXPROCS(0))
}

// tickLoop runs every 250 ms and hands every NPC with something due to a fixed
// pool of workers; it never waits for them. Per record: pending combat commands
// every tick, movement every npcMovementInterval, the brain every npcThinkInterval.
// A record still busy with its last turn (ticking) is skipped with its timers untouched.
func (m *NPCManager) tickLoop() {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()

	jobs := make(chan npcTickJob, 1024)
	for i := 0; i < npcTickWorkers(); i++ {
		go func() {
			defer safego.Recover("tickLoop")
			for j := range jobs {
				m.runTick(j)
			}
		}()
	}

	for range ticker.C {
		now := time.Now()

		m.mu.RLock()
		records := make([]*NPCRecord, 0, len(m.npcs))
		for _, r := range m.npcs {
			records = append(records, r)
		}
		m.mu.RUnlock()

		for _, rec := range records {
			if rec.ticking.Load() {
				continue // last turn still running
			}
			rec.mu.Lock()
			if rec.Dead {
				rec.mu.Unlock()
				continue
			}

			// Determine which phases fire this tick.
			shouldMovement := !now.Before(rec.NextMovementTime)
			shouldCombat := !now.Before(rec.NextCombatTime)
			hasPending := len(rec.pendingCombat) > 0

			// Skip NPC entirely if nothing to do this tick.
			if !shouldMovement && !shouldCombat && !hasPending {
				rec.mu.Unlock()
				continue
			}

			if shouldMovement {
				rec.NextMovementTime = now.Add(npcMovementInterval)
			}
			if shouldCombat {
				rec.NextCombatTime = now.Add(npcThinkInterval)
			}
			delta := 250 * time.Millisecond
			if !rec.lastTickAt.IsZero() {
				delta = now.Sub(rec.lastTickAt)
			}
			rec.lastTickAt = now
			rec.mu.Unlock()

			// Only this loop sets the flag, so the Load above cannot go stale
			// before this Store.
			rec.ticking.Store(true)
			job := npcTickJob{rec: rec, delta: delta, doMove: shouldMovement, doCombat: shouldCombat}
			select {
			case jobs <- job:
			default:
				// Every worker is blocked and the queue is full: run this turn on its own
				// goroutine rather than stall the loop.
				go m.runTick(job)
			}
		}
	}
}

func (m *NPCManager) runTick(j npcTickJob) {
	defer j.rec.ticking.Store(false)
	defer safego.Recover("npc.runTick")
	m.tickNPC(j.rec, j.delta, j.doMove, j.doCombat)
}
func (m *NPCManager) tickNPC(rec *NPCRecord, delta time.Duration, doMovement, doCombat bool) {
	rec.mu.Lock()

	// Check dead flag.
	if rec.Dead {
		rec.mu.Unlock()
		return
	}

	entity := rec.Entity
	ss := rec.System

	// Check if destroyed (structure <= 0).
	if entity.IsDestroyed() {
		rec.mu.Unlock()
		m.Kill(entity.ID, 0)
		return
	}

	// Flee: the align window closes and the warp-out is armed on the warp-in
	// state machine; WarpDespawn turns the landing into a removal.
	now := time.Now()
	if rec.Fleeing && !rec.WarpingIn {
		if now.Before(rec.FleeWarpAt) {
			// Still aligning. The Goto issued by beginFlee is already flying;
			// running the brain again would only re-decide it.
			rec.mu.Unlock()
			return
		}
		// A point holds a rat as it holds a player. Checked here, not in beginFlee:
		// a held rat keeps its escape heading and leaves when the point drops.
		if entity.WarpScrambleTotal() > 0 {
			if !rec.FleeHeld {
				rec.FleeHeld = true
				log.Printf("[npc] entityID=%d flee HELD — warp scrambled", entity.ID)
			}
			rec.mu.Unlock()
			return
		}
		rec.FleeHeld = false
		rec.WarpingIn = true
		rec.WarpInitiated = false
		rec.WarpDespawn = true
		rec.WarpDuration = fleeWarpDuration
	}

	// Warp-in state machine: skip brain while warping.
	if rec.WarpingIn {
		if !rec.WarpInitiated {
			// First tick: initiate the warp.
			rec.WarpInitiated = true
			rec.WarpStartTime = now
			warpAction := Action{
				Type:     ActionWarp,
				WarpDest: rec.WarpDest,
			}
			rec.mu.Unlock()
			m.applyAction(rec, warpAction)
			return
		}
		if now.Sub(rec.WarpStartTime) >= rec.WarpDuration {
			if rec.WarpDespawn {
				// It was running, not travelling: the ball is dropped in the tunnel.
				// Despawn does the lock clears, grid removal and spawner settlement.
				rec.mu.Unlock()
				m.Despawn(entity.ID, "fled")
				return
			}
			// Warp complete: snap to destination and stop.
			rec.WarpingIn = false
			rec.WarpInitiated = false
			dm := entity.Manager
			if dm != nil {
				dm.Pos = rec.WarpDest
				dm.Stop()
			}
			rec.SpawnPos = rec.WarpDest

			// Clear all locks so nobody stays boxed by an NPC that warped away.
			staleTargets := make([]uint32, len(rec.Targets))
			copy(staleTargets, rec.Targets)
			rec.Targets = nil
			rec.LockingTarget = 0
			rec.threat = newThreatTable()

			// Transfer to correct bubble at destination.
			ss := rec.System
			rec.mu.Unlock()

			m.clearAllEwar(rec)

			// Broadcast "lost" to each previously locked player.
			if m.lockBroadcast != nil {
				for _, tid := range staleTargets {
					if te := ss.GetEntity(tid); te != nil {
						m.lockBroadcast(entity, ss, te, "lost")
					}
				}
			}
			if ss != nil {
				// FireBubbleTransition, not TransferBubble: the bare call returns the
				// departure grid's players for the caller to notify. Position is already at WarpDest.
				ss.FireBubbleTransition(entity)
			}
			// Broadcast warp-end so clients stop the warp effect and
			// snap the ball to the landing position.
			m.applyAction(rec, Action{Type: ActionWarpEnd, WarpDest: rec.WarpDest})
			return
		}
		// Still warping: skip the brain this tick.
		rec.mu.Unlock()
		return
	}

	// Process pending lock.
	if rec.LockingTarget != 0 && now.After(rec.LockEndTime) {
		lockedID := rec.LockingTarget
		// Re-check warp and landing grace before completing the lock; the lock may
		// have started in the window between the two transitions.
		targetInGrace := false
		if te := ss.GetEntity(lockedID); te == nil || te.IsInWarp() || te.IsUntargetable() {
			targetInGrace = true
		}
		if targetInGrace {
			rec.LockingTarget = 0
			log.Printf("[npc] entityID=%d cancelled lock on %d (target in warp/landing grace)", entity.ID, lockedID)
		} else {
			// Lock completed: add to targets list.
			rec.Targets = append(rec.Targets, lockedID)
			if len(rec.Targets) > 3 {
				rec.Targets = rec.Targets[len(rec.Targets)-3:]
			}
			rec.LockingTarget = 0
			log.Printf("[npc] entityID=%d locked targetID=%d", entity.ID, lockedID)

			// Aggro from one rat pulls the whole spawn group. Belt spawners have
			// SpawnerID 0 but carry a CelestialID, which scopes the pull to the belt.
			if rec.SpawnerID != 0 || rec.CelestialID != 0 {
				go m.shareGroupAggro(entity.ID, rec.SpawnerID, rec.CelestialID, ss.ID, lockedID)
			}

			// Delay between lock completion and first shot: the dogma attack delay
			// (attrs 475/476) when present, capped at 5 s. Cruiser+ hulls carry maxima
			// of 20 s+, which reads as a locked NPC that never fires.
			const maxPostLockDelay = 5 * time.Second
			postLockDelay := 1 * time.Second
			if wp := rec.WeaponProfile; wp != nil && wp.AttackDelayMaxMs > 0 {
				minMs := wp.AttackDelayMinMs
				maxMs := wp.AttackDelayMaxMs
				if maxMs < minMs {
					maxMs = minMs
				}
				d := minMs
				if maxMs > minMs {
					d += rand.Int63n(maxMs - minMs + 1)
				}
				postLockDelay = time.Duration(d) * time.Millisecond
			}
			if postLockDelay > maxPostLockDelay {
				postLockDelay = maxPostLockDelay
			}
			minCooldown := now.Add(postLockDelay)
			if rec.WeaponCooldown.Before(minCooldown) {
				rec.WeaponCooldown = minCooldown
			}
			// EWAR waits out the same delay. tickEwar runs later in this tick, so without it a
			// scram or painter landed with the lock, and the stamped EWAR FX could reach the
			// client ahead of the otheradd event (#22).
			if rec.ewarReadyAt == nil {
				rec.ewarReadyAt = make(map[uint32]time.Time)
			}
			rec.ewarReadyAt[lockedID] = minCooldown

			// Broadcast lock to the targeted player.
			if m.lockBroadcast != nil {
				if targetEntity := ss.GetEntity(lockedID); targetEntity != nil {
					m.lockBroadcast(entity, ss, targetEntity, "add")
				}
			}
		}
	}

	// --- Phase 1: Drain all pending combat commands (every tick) ---
drainLoop:
	for len(rec.pendingCombat) > 0 {
		cmd := rec.pendingCombat[0]
		rec.pendingCombat = rec.pendingCombat[1:]
		switch cmd.Type {
		case CmdLockTarget:
			if now.Before(rec.JamUntil) {
				break drainLoop // sensors jammed: no new locks
			}
			if rec.LockingTarget == 0 && !rec.hasTarget(cmd.TargetID) {
				// Sensor dampeners stretch the flat lock time, capped so a heavily damped
				// NPC still locks (npc/tunables.go).
				lockSecs := npcLockSeconds()
				if srm := entity.EwarTotals().ScanResolutionMult; srm > 0 && srm < 1 {
					lockSecs = math.Max(lockSecs, math.Min(lockSecs/srm, npcLockCapSeconds()))
				}
				rec.LockingTarget = cmd.TargetID
				rec.LockEndTime = now.Add(time.Duration(lockSecs * float64(time.Second)))
				log.Printf("[npc] entityID=%d locking targetID=%d (%.1fs)", entity.ID, cmd.TargetID, lockSecs)
			}
			break drainLoop // stop after a lock; fire would fail until it completes
		case CmdUnlockTarget:
			shouldBroadcast := m.lockBroadcast != nil && rec.hasTarget(cmd.TargetID)
			rec.removeTarget(cmd.TargetID)
			if rec.LockingTarget == cmd.TargetID {
				rec.LockingTarget = 0
			}
			rec.mu.Unlock()
			m.clearEwarOnTarget(rec, cmd.TargetID)
			if shouldBroadcast {
				if te := ss.GetEntity(cmd.TargetID); te != nil {
					m.lockBroadcast(entity, ss, te, "lost")
				}
			}
			rec.mu.Lock()
		case CmdFireWeapons:
			m.fireWeapons(rec, cmd.TargetID, ss, now)
		}
	}

	// Stop stale fire FX when the NPC has not cycled for a cycle plus grace, or
	// the client keeps the red box and looping turret FX forever.
	if rec.fireFXTarget != 0 && m.stopFire != nil {
		graceMs := rec.lastCycleMs
		if graceMs <= 0 {
			graceMs = 4000
		}
		if now.Sub(rec.lastFireTime) > time.Duration(graceMs)*time.Millisecond+2*time.Second {
			staleTarget := rec.fireFXTarget
			rec.fireFXTarget = 0
			rec.mu.Unlock()
			m.stopFire(entity, ss, staleTarget)
			rec.mu.Lock()
		}
	}

	// If neither movement nor combat brain runs this tick, just handle
	// position persistence and return.
	if !doMovement && !doCombat {
		m.maybePersistPosition(rec, entity, ss, now)
		return
	}

	// Build enriched WorldView (needed for both movement and combat brain).
	selfPos := entity.Pos()
	mates := ss.GetBubbleEntitiesExcept(entity.ID)

	selfFactionID := rec.FactionID

	nearby := make([]EntityState, 0, len(mates))
	for _, mate := range mates {
		// Skip entities in warp or in landing grace: NPCs must not target, approach
		// or attack them until they have landed.
		if mate.IsInWarp() || mate.IsUntargetable() {
			continue
		}
		// Classic-era belt rats never attack capsules.
		if mate.IsPlayer && mate.Slim.GroupID == 29 {
			continue
		}
		matePos := mate.Pos()
		dx := selfPos.X - matePos.X
		dy := selfPos.Y - matePos.Y
		dz := selfPos.Z - matePos.Z
		dist := math.Sqrt(dx*dx + dy*dy + dz*dz)

		es := EntityState{
			ID:       mate.ID,
			Pos:      matePos,
			IsPlayer: mate.IsPlayer,
			TypeID:   mate.Slim.TypeID,
			Distance: dist,
			CorpID:   mate.Slim.CorpID,
		}

		if mate.IsPlayer {
			es.IsHostile = m.isPlayerHostile(entity.Slim.CorpID, selfFactionID, mate.Slim.CorpID)
		} else if IsInertStructure(mate.Slim.GroupID) {
			// Unarmed scenery is never a target of choice: structures carry faction 0,
			// which IsHostile reads as hostile. Threat still overrides via pickTarget.
			es.IsHostile = false
		} else {
			// Look up faction for fellow NPCs.
			m.mu.RLock()
			if mateRec, ok := m.npcs[mate.ID]; ok {
				es.FactionID = mateRec.FactionID
			}
			m.mu.RUnlock()
			if m.Standings != nil {
				es.IsHostile = m.Standings.IsHostile(selfFactionID, es.FactionID)
			}
		}

		nearby = append(nearby, es)
	}

	// Prune stale targets: drop any locked entity not present in bubble.
	if len(rec.Targets) > 0 {
		nearbySet := make(map[uint32]struct{}, len(nearby))
		for _, n := range nearby {
			nearbySet[n.ID] = struct{}{}
		}
		var prunedEwar []uint32
		for i := len(rec.Targets) - 1; i >= 0; i-- {
			tid := rec.Targets[i]
			if _, ok := nearbySet[tid]; !ok {
				if m.lockBroadcast != nil {
					if te := ss.GetEntity(tid); te != nil {
						m.lockBroadcast(entity, ss, te, "lost")
					}
				}
				rec.removeTarget(tid)
				rec.threat.Clear(tid)
				prunedEwar = append(prunedEwar, tid)
				log.Printf("[npc] entityID=%d pruned stale target %d (left bubble)", entity.ID, tid)
			}
		}
		if len(prunedEwar) > 0 {
			rec.mu.Unlock()
			for _, tid := range prunedEwar {
				m.clearEwarOnTarget(rec, tid)
			}
			rec.mu.Lock()
		}
		if rec.LockingTarget != 0 {
			if _, ok := nearbySet[rec.LockingTarget]; !ok {
				log.Printf("[npc] entityID=%d cancelled lock on %d (left bubble)", entity.ID, rec.LockingTarget)
				rec.LockingTarget = 0
			}
		}
	}

	// HP view.
	hpView := HPView{ShieldFrac: 1.0, ArmorFrac: 1.0, StructureFrac: 1.0}
	if entity.HP.MaxShield > 0 {
		hpView.ShieldFrac = entity.HP.CurShield / entity.HP.MaxShield
	}
	if entity.HP.MaxArmor > 0 {
		hpView.ArmorFrac = entity.HP.CurArmor / entity.HP.MaxArmor
	}
	if entity.HP.MaxStructure > 0 {
		hpView.StructureFrac = entity.HP.CurStructure / entity.HP.MaxStructure
	}

	// Weapon view.
	var weaponView *WeaponView
	if rec.WeaponProfile != nil {
		wp := rec.WeaponProfile
		isMissile := wp.MissileTypeID != 0
		effectiveRange := wp.EffectiveRange()
		weaponView = &WeaponView{
			OptimalRange:   wp.OptimalRange,
			FalloffRange:   wp.Falloff,
			FlyRange:       wp.FlyRange,
			ChaseMaxDist:   wp.ChaseMaxDist,
			EffectiveRange: effectiveRange,
			IsReady:        now.After(rec.WeaponCooldown),
			DmgPerCycle:    (wp.Em + wp.Explosive + wp.Kinetic + wp.Thermal) * wp.Multiplier,
			IsMissile:      isMissile,
		}
	}

	// Copy targets and threat for the view.
	targets := make([]uint32, len(rec.Targets))
	copy(targets, rec.Targets)
	threatEntries := rec.threat.Entries()

	view := WorldView{
		Self: EntityState{
			ID:       entity.ID,
			Pos:      selfPos,
			IsPlayer: false,
			TypeID:   rec.TypeID,
		},
		Nearby:           nearby,
		TickDelta:        delta,
		SelfHP:           hpView,
		Weapons:          weaponView,
		Targets:          targets,
		Locking:          rec.LockingTarget,
		ThreatTable:      threatEntries,
		AggrRange:        rec.AggrRange,
		UnarmedFlyRange:  rec.UnarmedFlyRange,
		SpawnPos:         rec.SpawnPos,
		SelfFactionID:    selfFactionID,
		CurrentTargetID:  rec.lastAction.TargetID,
		LastMovementType: rec.lastAction.Type,
		MoveSwitchAge:    now.Sub(rec.lastMoveSwitch),
		LeashPos:         rec.LeashPos,
		LeashOrbitDist:   rec.LeashOrbitDist,
	}

	rec.mu.Unlock()

	// --- Phase 2 & 3: Brain decision (single call serves both phases) ---
	if doMovement || doCombat {
		result := rec.Brain.Think(view)
		if result.Movement.Type == ActionFlee {
			// A decision to leave, not a movement command. beginFlee owns the align, warp
			// and despawn; no combat commands from this tick are queued.
			m.beginFlee(rec, result.Movement.TargetID)
			rec.mu.Lock()
			m.maybePersistPosition(rec, entity, ss, now)
			return
		}
		if doMovement {
			m.applyAction(rec, result.Movement)
		}
		if doCombat && len(result.Combat) > 0 {
			rec.mu.Lock()
			// Stagger a first lock attempt with random jitter so several NPCs do not
			// alpha-strike together.
			if result.Combat[0].Type == CmdLockTarget {
				tid := result.Combat[0].TargetID
				if !rec.hasTarget(tid) && rec.LockingTarget != tid {
					jitter := time.Duration(rand.Intn(1000)) * time.Millisecond
					rec.NextCombatTime = time.Now().Add(jitter)
				}
			}
			rec.pendingCombat = append(rec.pendingCombat, result.Combat...)
			rec.mu.Unlock()
		}
		// Electronic modules run on the combat cadence, against whatever the
		// brain has locked.
		if doCombat {
			m.tickEwar(rec, now)
		}
	}

	rec.mu.Lock()
	m.maybePersistPosition(rec, entity, ss, now)
}

// maybePersistPosition writes the NPC's live position to the DB on a slow
// timer through posPersist. Must be called with rec.mu held; always releases it.
func (m *NPCManager) maybePersistPosition(rec *NPCRecord, entity *destiny.Entity, ss *destiny.SolarSystem, now time.Time) {
	if m.posPersist == nil || !isMovingAction(rec.lastAction.Type) ||
		now.Sub(rec.lastPosPersist) < posPersistInterval {
		rec.mu.Unlock()
		return
	}
	rec.lastPosPersist = now
	targetIDs := make([]uint32, len(rec.Targets))
	copy(targetIDs, rec.Targets)
	rec.mu.Unlock()

	// The row is written by the batched flush (persist.go); posPersist only logs.
	m.markDirty(entity.ID, dirtyPos)

	var targetEntities []*destiny.Entity
	for _, tid := range targetIDs {
		if te := ss.GetEntity(tid); te != nil {
			targetEntities = append(targetEntities, te)
		}
	}
	m.posPersist(entity, ss, targetEntities)
}

// nextWeaponCooldown schedules the next shot one cycle after the previous
// scheduled shot, not after now, so a gun polled on the think tick keeps its
// cycle time and matches the client's repeating fire FX. A cooldown more than
// a cycle stale (first shot, a pause, a new target) restarts from now.
func nextWeaponCooldown(prev, now time.Time, cycleMs int64) time.Time {
	cycle := time.Duration(cycleMs) * time.Millisecond
	if prev.IsZero() || now.Sub(prev) >= cycle {
		return now.Add(cycle)
	}
	return prev.Add(cycle)
}

// isMovingAction reports whether an action leaves the NPC under way.
func isMovingAction(t ActionType) bool {
	return t == ActionFollow || t == ActionOrbit || t == ActionGoto
}

// fireWeapons attempts to fire weapons at target. Must be called with rec.mu
// held. Temporarily releases rec.mu for network broadcasts, then reacquires.
func (m *NPCManager) fireWeapons(rec *NPCRecord, targetID uint32, ss *destiny.SolarSystem, now time.Time) {
	if rec.WeaponProfile == nil {
		return
	}
	if !rec.hasTarget(targetID) {
		return
	}
	if now.Before(rec.WeaponCooldown) {
		return
	}

	target := ss.GetEntity(targetID)
	if target == nil {
		rec.removeTarget(targetID)
		return
	}

	// A warping target is untouchable once the tunnel opens, and landing grace
	// covers the decel slide. The align is not: that ship is still on this grid.
	if target.IsInWarp() || target.IsUntargetable() {
		rec.removeTarget(targetID)
		if m.lockBroadcast != nil {
			m.lockBroadcast(rec.Entity, ss, target, "lost")
		}
		return
	}

	// Target must still share the NPC's bubble: a player entering warp leaves it
	// before the stale-target prune runs.
	if target.BubbleID < 0 || target.BubbleID != rec.Entity.BubbleID {
		rec.removeTarget(targetID)
		if m.lockBroadcast != nil {
			m.lockBroadcast(rec.Entity, ss, target, "lost")
		}
		return
	}

	// Range check.
	selfPos := rec.Entity.Pos()
	targetPos := target.Pos()
	dx := selfPos.X - targetPos.X
	dy := selfPos.Y - targetPos.Y
	dz := selfPos.Z - targetPos.Z
	dist := math.Sqrt(dx*dx + dy*dy + dz*dz)

	wp := rec.WeaponProfile
	// Turret range is entityAttackRange (attr 247); missile range is velocity x
	// flightTime. See NPCWeaponProfile.EffectiveRange for why a launcher replaces 247.
	maxRange := wp.EffectiveRange()
	if dist > maxRange {
		// Debug-gated: the cooldown is only armed by a shot that leaves, so an NPC
		// held out of range returns here every AI tick.
		if CombatDebug {
			log.Printf("[npc] entityID=%d target %d out of range (dist=%.0f max=%.0f)", rec.Entity.ID, targetID, dist, maxRange)
		}
		return // out of range
	}

	// Turret hit chance: tracking and falloff terms share one exponent (client
	// GetAccuracy, DOGMA_MATH.md section 6). Missiles do not track. EWAR applies below.
	selfEw := rec.Entity.EwarTotals()
	effOptimal := wp.OptimalRange * selfEw.TurretRangeMult
	effTracking := wp.TrackingSpeed * selfEw.TurretTrackingMult
	exponent := 0.0
	if wp.MissileTypeID == 0 {
		if dist > effOptimal && wp.Falloff > 0 {
			fterm := (dist - effOptimal) / wp.Falloff
			exponent += fterm * fterm
		}
		if effTracking > 0 {
			if rec.lastTargetPos == nil {
				rec.lastTargetPos = make(map[uint32]destiny.Vec3)
				rec.lastTargetPosT = make(map[uint32]time.Time)
				rec.lastSelfPos = make(map[uint32]destiny.Vec3)
				rec.lastSelfPosT = make(map[uint32]time.Time)
			}
			if lastTPos, ok := rec.lastTargetPos[targetID]; ok {
				// Angular velocity from the relative velocity (target minus NPC), as in the
				// player-side formula, so an orbiting NPC's own motion penalises its tracking.
				rdx := targetPos.X - selfPos.X
				rdy := targetPos.Y - selfPos.Y
				rdz := targetPos.Z - selfPos.Z
				rMag := math.Sqrt(rdx*rdx + rdy*rdy + rdz*rdz)
				// Position delta over elapsed time gives m/s; fire attempts are seconds apart.
				dt := now.Sub(rec.lastTargetPosT[targetID]).Seconds()
				if rMag > 0 && dt > 0 {
					lastSPos := rec.lastSelfPos[targetID]
					vx := (targetPos.X - lastTPos.X - (selfPos.X - lastSPos.X)) / dt
					vy := (targetPos.Y - lastTPos.Y - (selfPos.Y - lastSPos.Y)) / dt
					vz := (targetPos.Z - lastTPos.Z - (selfPos.Z - lastSPos.Z)) / dt
					// Radial direction (unit vector from NPC to target).
					ux, uy, uz := rdx/rMag, rdy/rMag, rdz/rMag
					// Cross product of the radial unit vector and v_rel for transverse speed.
					cx := uy*vz - uz*vy
					cy := uz*vx - ux*vz
					cz := ux*vy - uy*vx
					transverseSpeed := math.Sqrt(cx*cx + cy*cy + cz*cz)
					angular := transverseSpeed / rMag

					targetSig := m.getTargetSigRadius(target.Slim.TypeID) * target.SigRadiusMult()
					weaponSig := wp.WeaponSigRadius
					if weaponSig <= 0 {
						weaponSig = 400
					}
					tterm := angular / (effTracking * targetSig / weaponSig)
					exponent += tterm * tterm
				}
			}
			rec.lastTargetPos[targetID] = targetPos
			rec.lastTargetPosT[targetID] = now
			rec.lastSelfPos[targetID] = selfPos
			rec.lastSelfPosT[targetID] = now
			// Prune samples for targets not engaged recently, or long-lived NPCs keep
			// one entry per entity they ever fired on.
			if len(rec.lastTargetPosT) > 16 {
				stale := now.Add(-5 * time.Minute)
				for id, t := range rec.lastTargetPosT {
					if t.Before(stale) {
						delete(rec.lastTargetPos, id)
						delete(rec.lastTargetPosT, id)
						delete(rec.lastSelfPos, id)
						delete(rec.lastSelfPosT, id)
					}
				}
			}
		}
	}

	// One roll decides miss, hit and wrecking plus the damage modifier: roll <= chance
	// hits at roll+0.49; roll under 1% of chance wrecks at 3x. Missiles always hit.
	hitChance := math.Pow(0.5, exponent)
	hitQuality := 1.0
	dmgMod := 1.0
	if wp.MissileTypeID == 0 {
		roll := rand.Float64()
		switch {
		case roll < 0.01*hitChance:
			dmgMod = 3.0
		case roll <= hitChance:
			dmgMod = roll + 0.49
		default:
			dmgMod = 0
		}
		hitQuality = dmgMod
		if dmgMod == 0 {
			// Miss: pick a miss-tier message (IDs 0-2) by how close it was.
			hitQuality = math.Min(hitChance*0.49, 0.41)
			log.Printf("[npc] entityID=%d missed target %d (hitChance=%.2f)", rec.Entity.ID, targetID, hitChance)
			// Still set cooldown on miss.
			rec.WeaponCooldown = nextWeaponCooldown(rec.WeaponCooldown, now, wp.CycleTimeMs)
			// A miss still plays fire FX (active=1); track it so the idle check can
			// send a stop event later.
			rec.lastFireTime = now
			rec.fireFXTarget = targetID
			rec.lastCycleMs = wp.CycleTimeMs
			// Release lock for broadcast, then reacquire.
			entity := rec.Entity
			rec.mu.Unlock()
			if m.combatBroadcast != nil {
				m.combatBroadcast(entity, ss, target, 0, false, hitQuality, wp.CycleTimeMs)
			}
			rec.mu.Lock()
			return
		}
	}

	// Falloff damage reduction for missiles only; turrets fold falloff into
	// the hit roll above.
	falloffFactor := 1.0
	if wp.MissileTypeID != 0 && dist > wp.OptimalRange && wp.Falloff > 0 {
		falloffFactor = math.Pow(0.5, math.Pow((dist-wp.OptimalRange)/wp.Falloff, 2))
	}

	// Set cooldown.
	rec.WeaponCooldown = nextWeaponCooldown(rec.WeaponCooldown, now, wp.CycleTimeMs)

	// Missile path: delegate to callback for ball spawn + delayed impact.
	if wp.MissileTypeID != 0 {
		// Track the fire cycle so the idle check can send the stop-fire
		// event (clears the client's MissileDeployment red box).
		rec.lastFireTime = now
		rec.fireFXTarget = targetID
		rec.lastCycleMs = wp.CycleTimeMs
		entity := rec.Entity
		typeName := rec.TypeName
		rec.mu.Unlock()
		if m.missileLaunch != nil {
			if CombatDebug {
				log.Printf("[npc] %s (entityID=%d) launches missile at %d (missileType=%d falloff=%.2f)",
					typeName, entity.ID, targetID, wp.MissileTypeID, falloffFactor)
			}
			m.missileLaunch(entity, ss, target, wp, falloffFactor)
		}
		rec.mu.Lock()
		return
	}

	// Turret path: apply damage instantly, scaled by the roll-derived
	// modifier.
	applied, destroyed := target.ApplyDamage(
		wp.Em*wp.Multiplier*dmgMod*rates.NPCDamage,
		wp.Explosive*wp.Multiplier*dmgMod*rates.NPCDamage,
		wp.Kinetic*wp.Multiplier*dmgMod*rates.NPCDamage,
		wp.Thermal*wp.Multiplier*dmgMod*rates.NPCDamage,
	)

	if CombatDebug {
		log.Printf("[npc] %s (entityID=%d) fires at %d: dealt %.1f dmg (destroyed=%v)",
			rec.TypeName, rec.Entity.ID, targetID, applied, destroyed)
	}

	// Kill-report snapshot for the CONCORD mail on player deaths.
	if target.IsPlayer {
		weaponTID := rec.Entity.NPCTurretTypeID
		if weaponTID == 0 {
			weaponTID = rec.Entity.Slim.TypeID
		}
		target.RecordKillHit(rec.Entity, weaponTID, applied, destroyed)
	}

	// Track the fire cycle so the idle check can stop FX when firing ceases.
	rec.lastFireTime = now
	rec.fireFXTarget = targetID
	rec.lastCycleMs = wp.CycleTimeMs

	// Capture state, release lock for broadcasts.
	entity := rec.Entity
	rec.mu.Unlock()

	if m.combatBroadcast != nil {
		m.combatBroadcast(entity, ss, target, applied, destroyed, hitQuality, wp.CycleTimeMs)
	}

	// Notify target NPC's threat table so it can retaliate.
	if !target.IsPlayer && applied > 0 {
		safego.Go("NotifyDamage", func() { m.NotifyDamage(targetID, entity.ID, applied) })
		m.SyncHPToDB(targetID)
	}

	if destroyed {
		if m.lockBroadcast != nil {
			m.lockBroadcast(entity, ss, target, "lost")
		}
	}

	// Reacquire the lock; the caller expects it held.
	rec.mu.Lock()
	if destroyed {
		rec.removeTarget(targetID)
		rec.threat.Clear(targetID)
	}
}

func (rec *NPCRecord) hasTarget(id uint32) bool {
	for _, t := range rec.Targets {
		if t == id {
			return true
		}
	}
	return false
}

func (rec *NPCRecord) removeTarget(id uint32) {
	delete(rec.ewarReadyAt, id)
	for i, t := range rec.Targets {
		if t == id {
			rec.Targets = append(rec.Targets[:i], rec.Targets[i+1:]...)
			return
		}
	}
}

func (m *NPCManager) applyAction(rec *NPCRecord, action Action) {
	if action.Type == ActionIdle {
		return
	}

	now := time.Now()

	// Deduplicate: skip the broadcast if the action is unchanged. A steady Goto is
	// re-sent on a timer to bound drift; see docs/npc-behaviour.md#movement-commands.
	resyncEvery := idleActionResyncInterval
	rec.mu.Lock()
	if len(rec.Targets) > 0 {
		resyncEvery = combatActionResyncInterval
	}
	same := rec.lastAction.Type == action.Type &&
		rec.lastAction.TargetID == action.TargetID &&
		rec.lastAction.Distance == action.Distance &&
		rec.lastAction.Pos == action.Pos
	if rec.lastAction.Type != action.Type {
		rec.lastMoveSwitch = now
	}
	// Only Goto is re-broadcast: a Goto resend is idempotent, while an Orbit or
	// Follow resend restarts the client's integration and re-derives the orbit plane.
	resyncable := action.Type == ActionGoto
	resync := same && resyncable && now.Sub(rec.lastActionSync) >= resyncEvery
	rec.lastAction = action
	if !same || resync {
		rec.lastActionSync = now
	}
	rec.mu.Unlock()

	dm := rec.Entity.Manager
	if dm == nil {
		return
	}

	// Skip the redundant broadcast; the client keeps integrating the command.
	if same && !resync {
		return
	}

	if !same {
		// Debug, not Info: this fires per NPC per re-decision. The nil-broadcast line
		// below stays ungated.
		logx.Debugf("[npc] applyAction entityID=%d: type=%d targetID=%d dist=%.0f", rec.Entity.ID, action.Type, action.TargetID, action.Distance)
	}

	switch action.Type {
	case ActionFollow:
		dm.Follow(action.TargetID, action.Distance)
	case ActionOrbit:
		dm.Orbit(action.TargetID, action.Distance)
	case ActionGoto:
		dm.GotoDirection(action.Pos)
	case ActionStop:
		dm.Stop()
	case ActionWarp:
		effectStamp := rec.Entity.NextStamp()
		dm.WarpTo(action.WarpDest, effectStamp, rec.Entity.ID, 0)
	case ActionWarpEnd:
		dm.Stop()
	}

	// Broadcast the action to nearby players.
	if m.broadcast != nil {
		m.broadcast(rec.Entity, rec.System, action)
	} else {
		log.Printf("[npc] applyAction entityID=%d: broadcast callback is nil!", rec.Entity.ID)
	}
}

// isPlayerHostile reports whether an NPC treats a player as hostile: same
// corporation or same faction is friendly, everything else hostile.
func (m *NPCManager) isPlayerHostile(npcCorpID, npcFactionID, playerCorpID int64) bool {
	// Same corp: friendly.
	if npcCorpID != 0 && npcCorpID == playerCorpID {
		return false
	}

	// Same faction: friendly (via the player corp's faction).
	if npcFactionID != 0 && m.Standings != nil {
		playerFactionID := m.Standings.CorpFaction(playerCorpID)
		if playerFactionID != 0 && playerFactionID == npcFactionID {
			return false
		}
	}

	return true
}
