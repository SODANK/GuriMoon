package destiny

import (
	"gurimoon/logx"
	"gurimoon/marshal"
	"gurimoon/safego"
	"log"
	"math"
	"math/rand"
	"sort"
	"sync"
	"time"
)

// SolarSystem holds the shared state for one solar system. ss.mu guards it.
type SolarSystem struct {
	ID       int64
	Security float64 // system security status (negative = nullsec)

	// FactionID is the sovereign NPC faction (mapSolarSystems.factionID, 0
	// when unclaimed). Set before GenerateAsteroids: it gates which lowsec
	// ore and which faction ice the system's belts carry.
	FactionID int64

	mu       sync.RWMutex
	entities map[uint32]*Entity
	bubbles []*Bubble
	// bubblesByID indexes ss.bubbles by ID for getBubble; keep it in sync at every
	// site that mutates the slice. The slice stays the source of truth because
	// findOrCreateBubble and findOverlappingBubble search by position.
	bubblesByID map[int]*Bubble
	nextBub  int // next bubble ID

	// Celestial data, loaded once when the system is created and read-only after.
	CelBalls  []BallDesc
	CelSlims  []SlimItem
	CelDamage map[uint32]DamageState
	SolItem   marshal.PyRep

	// StationEntities holds conquerable stations registered as entities so they can
	// be targeted and damaged. The station stays in CelBalls/CelSlims for drawing.
	// Keyed by stationID (= entityID).
	StationEntities map[uint32]*Entity

	// StructureEntities holds category-23 structures registered as entities so they
	// lock and take damage. Kept out of bubbles and ss.entities (StarbaseBalls owns
	// the ball) and out of StationEntities (a death there runs station conquest).
	StructureEntities map[uint32]*Entity

	// Asteroid belt data, generated when the system is first created.
	AsteroidBalls      []BallDesc
	AsteroidSlims      []SlimItem
	AsteroidDamage     map[uint32]DamageState
	AsteroidQty        map[uint32]int64     // entityID → ore units remaining
	AsteroidMaxQty     map[uint32]int64     // entityID → initial quantity for respawn
	AsteroidDepletedAt map[uint32]time.Time // entityID → time of depletion

	// AsteroidBelt maps a rock to the mapDenormalize itemID of the groupID-9
	// celestial it was generated into. Kept off BallDesc/SlimItem, which are
	// wire-shaped and marshalled as they are.
	AsteroidBelt map[uint32]int64

	// asteroidDirty is the set of rocks whose quantity has moved since the last
	// flush; the write-back drains it on a timer. Guarded by mu.
	asteroidDirty map[uint32]struct{}

	// asteroidRespawn is how long a mined-out rock stays gone. Zero (the default)
	// disables respawn: the field's regeneration cycle is what refills the pool.
	// Set from the asteroids.respawnMinutes setting at load.
	asteroidRespawn time.Duration

	// onAsteroidRespawn is called from the respawn goroutine, outside ss.mu, for
	// each rock that comes back so main.go can push AddBalls to the grid. Guarded
	// by mu; set through SetOnAsteroidRespawn on every player registration.
	onAsteroidRespawn func(ball BallDesc, slim SlimItem, dmg DamageState)

	// Starbase data, loaded from the DB when the system is first created.
	StarbaseBalls  []BallDesc
	StarbaseSlims  []SlimItem
	StarbaseDamage map[uint32]DamageState

	// Billboard data: Interbus Billboards near stargates in high and low sec,
	// generated once at system init.
	BillboardBalls  []BallDesc
	BillboardSlims  []SlimItem
	BillboardDamage map[uint32]DamageState

	// Loot containers (typeID 23) at ship-death sites. Ephemeral: cleared from the
	// DB at startup, despawned on a timer or when emptied. Ball ID = DB itemID.
	LootBalls   []BallDesc
	LootSlims   []SlimItem
	LootDamage  map[uint32]DamageState
	LootSpawned map[uint32]time.Time // entityID → spawn time, drives TTL expiry

	respawnOnce     sync.Once
	shieldRegenOnce sync.Once
	tickOnce        sync.Once

	// stopCh is closed by StopTickers to signal all background goroutines
	// (tick loop, shield regen, asteroid respawn) to exit.
	stopCh chan struct{}

	// dead is set under mu by SystemManager.RemoveIfEmpty when the last player
	// leaves. Tickers never restart (sync.Once), so AddEntity refuses and the
	// caller re-acquires a fresh instance from the SystemManager.
	dead bool

	// shieldRegenBroadcast is set by main.go and called when a shield regen tick
	// changes an entity's HP. Guarded by mu.
	shieldRegenBroadcast func(ss *SolarSystem, entityID uint32, ds DamageState)

	// bubbleTransitionBroadcast is set by main.go and called by the tick loop when
	// an entity has moved outside its bubble; it sends AddBalls/RemoveBalls.
	// Guarded by mu.
	bubbleTransitionBroadcast func(ss *SolarSystem, entity *Entity, oldBubbleID int)
}

// SetOnAsteroidRespawn installs the asteroid-respawn callback.
func (ss *SolarSystem) SetOnAsteroidRespawn(f func(ball BallDesc, slim SlimItem, dmg DamageState)) {
	ss.mu.Lock()
	ss.onAsteroidRespawn = f
	ss.mu.Unlock()
}

// SetShieldRegenBroadcast installs the shield-regen broadcast callback.
func (ss *SolarSystem) SetShieldRegenBroadcast(f func(ss *SolarSystem, entityID uint32, ds DamageState)) {
	ss.mu.Lock()
	ss.shieldRegenBroadcast = f
	ss.mu.Unlock()
}

// SetBubbleTransitionBroadcast installs the bubble-transition callback.
func (ss *SolarSystem) SetBubbleTransitionBroadcast(f func(ss *SolarSystem, entity *Entity, oldBubbleID int)) {
	ss.mu.Lock()
	ss.bubbleTransitionBroadcast = f
	ss.mu.Unlock()
}

// FireBubbleTransition moves e into the bubble covering its current position
// and notifies both grids: RemoveBalls to the one it left, AddBalls to the one
// it joined. The caller has already placed the entity. With no callback
// installed it falls back to TransferBubble, which notifies nobody.
func (ss *SolarSystem) FireBubbleTransition(e *Entity) {
	if e == nil {
		return
	}
	ss.mu.RLock()
	cb := ss.bubbleTransitionBroadcast
	ss.mu.RUnlock()
	if cb == nil {
		ss.TransferBubble(e.ID, e.Pos())
		return
	}
	e.mu.Lock()
	oldBubbleID := e.BubbleID
	e.mu.Unlock()
	cb(ss, e, oldBubbleID)
}

// NewSolarSystem creates a new empty solar system.
func NewSolarSystem(id int64) *SolarSystem {
	return &SolarSystem{
		ID:             id,
		entities:       make(map[uint32]*Entity),
		CelDamage:      make(map[uint32]DamageState),
		StarbaseDamage: make(map[uint32]DamageState),
		stopCh:         make(chan struct{}),
	}
}

// Done returns a channel closed when the system's background tickers stop
// (server shutdown of this system). Lets per-object timers exit cleanly.
func (ss *SolarSystem) Done() <-chan struct{} {
	return ss.stopCh
}

// AddLootContainer registers a ship-death loot container so it appears in
// subsequent SetState payloads and can be broadcast to players already in
// space. Safe for concurrent use. Records the spawn time as time.Now().
func (ss *SolarSystem) AddLootContainer(bd BallDesc, slim SlimItem) {
	ss.AddLootContainerAt(bd, slim, time.Now())
}

// AddLootContainerAt is like AddLootContainer but records a caller-supplied
// spawn time. Used by the system loader to restore containers with their
// original age so the expiry sweep honours the real TTL.
func (ss *SolarSystem) AddLootContainerAt(bd BallDesc, slim SlimItem, spawnedAt time.Time) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.LootBalls = append(ss.LootBalls, bd)
	ss.LootSlims = append(ss.LootSlims, slim)
	if ss.LootDamage == nil {
		ss.LootDamage = make(map[uint32]DamageState)
	}
	ss.LootDamage[bd.EntityID] = DamageState{Shield: 1.0, Tau: 1e9, Armor: 1.0, Structure: 1.0}
	if ss.LootSpawned == nil {
		ss.LootSpawned = make(map[uint32]time.Time)
	}
	ss.LootSpawned[bd.EntityID] = spawnedAt
}

// LootContainersOlderThan returns the itemIDs of loot containers spawned
// before cutoff, for the expiry sweep.
func (ss *SolarSystem) LootContainersOlderThan(cutoff time.Time) []int64 {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	var ids []int64
	for id, t := range ss.LootSpawned {
		if t.Before(cutoff) {
			ids = append(ids, int64(id))
		}
	}
	return ids
}

// RemoveLootContainer drops a loot container from the system's shared slices.
// Returns true if the entity was found and removed.
func (ss *SolarSystem) RemoveLootContainer(entityID uint32) bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	for i, bd := range ss.LootBalls {
		if bd.EntityID == entityID {
			ss.LootBalls = append(ss.LootBalls[:i], ss.LootBalls[i+1:]...)
			ss.LootSlims = append(ss.LootSlims[:i], ss.LootSlims[i+1:]...)
			delete(ss.LootDamage, entityID)
			delete(ss.LootSpawned, entityID)
			return true
		}
	}
	return false
}

// GetLootContainers returns copies of the loot ball/slim slices plus the
// damage map for SetState builders. Safe for concurrent use.
func (ss *SolarSystem) GetLootContainers() ([]BallDesc, []SlimItem, map[uint32]DamageState) {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	balls := make([]BallDesc, len(ss.LootBalls))
	copy(balls, ss.LootBalls)
	slims := make([]SlimItem, len(ss.LootSlims))
	copy(slims, ss.LootSlims)
	dmg := make(map[uint32]DamageState, len(ss.LootDamage))
	for k, v := range ss.LootDamage {
		dmg[k] = v
	}
	return balls, slims, dmg
}

// NearbyLootContainers returns loot containers within BubbleRadius of
// playerPos, the grid-local filter NearbyAsteroids and NearbyBillboards apply.
// Full SetState builds use this, not GetLootContainers, so a client holds only
// the cans on its own grid.
func (ss *SolarSystem) NearbyLootContainers(playerPos Vec3) ([]BallDesc, []SlimItem, map[uint32]DamageState) {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	rSq := BubbleRadius * BubbleRadius
	var balls []BallDesc
	var slims []SlimItem
	dmg := make(map[uint32]DamageState)
	for i, bd := range ss.LootBalls {
		if playerPos.Sub(bd.Pos).LenSq() <= rSq {
			balls = append(balls, bd)
			slims = append(slims, ss.LootSlims[i])
			if d, ok := ss.LootDamage[bd.EntityID]; ok {
				dmg[bd.EntityID] = d
			}
		}
	}
	return balls, slims, dmg
}

// lootGroupBiomass is invGroups 14: a podded pilot's corpse, which shares the
// loot slices (same clock, same reload) but is not a container.
const lootGroupBiomass = 14

// GetLootContainerPosition returns a loot container's position. Every in-space
// inventory gate uses it as the "openable container" test, so a corpse answers
// false: it rides the loot slices but must never be bound as an inventory.
// GetLootBall sees both. Safe for concurrent use.
func (ss *SolarSystem) GetLootContainerPosition(entityID uint32) (Vec3, bool) {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	for i, bd := range ss.LootBalls {
		if bd.EntityID == entityID {
			if ss.LootSlims[i].GroupID == lootGroupBiomass || ss.LootSlims[i].CategoryID == lootCategoryShip {
				return Vec3{}, false
			}
			return bd.Pos, true
		}
	}
	return Vec3{}, false
}

// lootCategoryShip is invCategories 6: an abandoned hull, which rides the loot
// slices for SetState and grid delivery but is neither a container nor on the
// loot expiry clock.
const lootCategoryShip = 6

// AddAbandonedShip registers an unpiloted hull in the loot slices with its
// damage. It gets no spawn time, so the loot expiry sweep never takes it.
// Safe for concurrent use.
func (ss *SolarSystem) AddAbandonedShip(bd BallDesc, slim SlimItem, dmg DamageState) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.LootBalls = append(ss.LootBalls, bd)
	ss.LootSlims = append(ss.LootSlims, slim)
	if ss.LootDamage == nil {
		ss.LootDamage = make(map[uint32]DamageState)
	}
	ss.LootDamage[bd.EntityID] = dmg
}

// GetAbandonedShip returns an unpiloted hull's ball, slim and damage. Safe for
// concurrent use.
func (ss *SolarSystem) GetAbandonedShip(entityID uint32) (BallDesc, SlimItem, DamageState, bool) {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	for i, bd := range ss.LootBalls {
		if bd.EntityID == entityID {
			if ss.LootSlims[i].CategoryID != lootCategoryShip {
				break
			}
			return bd, ss.LootSlims[i], ss.LootDamage[entityID], true
		}
	}
	return BallDesc{}, SlimItem{}, DamageState{}, false
}

// GetLootBall returns any ball in the loot slices, container or corpse, with
// its slim. Safe for concurrent use.
func (ss *SolarSystem) GetLootBall(entityID uint32) (BallDesc, SlimItem, bool) {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	for i, bd := range ss.LootBalls {
		if bd.EntityID == entityID {
			return bd, ss.LootSlims[i], true
		}
	}
	return BallDesc{}, SlimItem{}, false
}

// SliceBall finds a ball in the loot or starbase slices (cans, wrecks, corpses,
// probes, towers, open-site and cyno beacons): the balls with no entity behind
// them, which names a scan result that has no table row. Safe for concurrent use.
func (ss *SolarSystem) SliceBall(entityID uint32) (BallDesc, SlimItem, bool) {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	for i, bd := range ss.LootBalls {
		if bd.EntityID == entityID && i < len(ss.LootSlims) {
			return bd, ss.LootSlims[i], true
		}
	}
	for i, bd := range ss.StarbaseBalls {
		if bd.EntityID == entityID && i < len(ss.StarbaseSlims) {
			return bd, ss.StarbaseSlims[i], true
		}
	}
	return BallDesc{}, SlimItem{}, false
}

// AddStarbase registers a new in-space starbase into the system's shared
// ball/slim slices so it appears in subsequent SetState payloads and bubble
// broadcasts. Safe to call after the system has been populated; useful when
// /posplant fires on a system that already has players in it.
func (ss *SolarSystem) AddStarbase(bd BallDesc, slim SlimItem, dmg DamageState) {
	ss.mu.Lock()
	ss.StarbaseBalls = append(ss.StarbaseBalls, bd)
	ss.StarbaseSlims = append(ss.StarbaseSlims, slim)
	if ss.StarbaseDamage == nil {
		ss.StarbaseDamage = make(map[uint32]DamageState)
	}
	ss.StarbaseDamage[bd.EntityID] = dmg
	factory := StructureEntityFactory
	ss.mu.Unlock()

	// The factory reads HP from the database, so it runs outside the lock; nil
	// means the ball stays scenery.
	if factory != nil && slim.CategoryID == 23 {
		if e := factory(ss, bd, slim); e != nil {
			ss.mu.Lock()
			if ss.StructureEntities == nil {
				ss.StructureEntities = make(map[uint32]*Entity)
			}
			// Only if the ball is still there: a remove racing the factory
			// must not leave an entity with no ball behind it.
			for _, b := range ss.StarbaseBalls {
				if b.EntityID == bd.EntityID {
					ss.StructureEntities[bd.EntityID] = e
					if d := e.DamageStateFrac(); d.Structure > 0 {
						ss.StarbaseDamage[bd.EntityID] = d
					}
					break
				}
			}
			ss.mu.Unlock()
		}
	}
}

// StructureEntityFactory builds the combat entity for a category-23 ball added
// to a system. Set by the server entry point (posentity.go), which owns the
// database; nil leaves structures as scenery. Called by AddStarbase.
var StructureEntityFactory func(ss *SolarSystem, bd BallDesc, slim SlimItem) *Entity

// StructureEntity returns the registered structure entity, or nil.
func (ss *SolarSystem) StructureEntity(entityID uint32) *Entity {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	return ss.StructureEntities[entityID]
}

// IsStructureEntity reports whether entityID is a registered structure.
func (ss *SolarSystem) IsStructureEntity(entityID uint32) bool {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	_, ok := ss.StructureEntities[entityID]
	return ok
}

// SnapshotStructureEntities returns the registered structures.
func (ss *SolarSystem) SnapshotStructureEntities() []*Entity {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	out := make([]*Entity, 0, len(ss.StructureEntities))
	for _, e := range ss.StructureEntities {
		out = append(out, e)
	}
	return out
}

// SetStarbaseDamage stores a structure's damage state for later SetState
// payloads. StarbaseDamage is written from combat and regen goroutines while
// connections read it; always go through this and SnapshotStarbaseDamage.
func (ss *SolarSystem) SetStarbaseDamage(entityID uint32, d DamageState) {
	ss.mu.Lock()
	if ss.StarbaseDamage == nil {
		ss.StarbaseDamage = make(map[uint32]DamageState)
	}
	if _, ok := ss.StarbaseDamage[entityID]; ok {
		ss.StarbaseDamage[entityID] = d
	}
	ss.mu.Unlock()
}

// SnapshotStarbaseDamage returns a copy of the starbase damage map.
func (ss *SolarSystem) SnapshotStarbaseDamage() map[uint32]DamageState {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	out := make(map[uint32]DamageState, len(ss.StarbaseDamage))
	for k, v := range ss.StarbaseDamage {
		out[k] = v
	}
	return out
}

// UpdateStarbaseSlim replaces the stored SlimItem for a starbase (state
// transitions: anchored→online etc.) so new arrivals see the current state.
// Returns true if the entity was found.
func (ss *SolarSystem) UpdateStarbaseSlim(entityID uint32, slim SlimItem) bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	for i, bd := range ss.StarbaseBalls {
		if bd.EntityID == entityID {
			ss.StarbaseSlims[i] = slim
			return true
		}
	}
	return false
}

// RemoveStarbase drops a starbase from the system's shared slices. Returns
// true if the entity was found and removed.
func (ss *SolarSystem) RemoveStarbase(entityID uint32) bool {
	ss.mu.Lock()
	var removed *Entity
	found := false
	for i, bd := range ss.StarbaseBalls {
		if bd.EntityID == entityID {
			ss.StarbaseBalls = append(ss.StarbaseBalls[:i], ss.StarbaseBalls[i+1:]...)
			ss.StarbaseSlims = append(ss.StarbaseSlims[:i], ss.StarbaseSlims[i+1:]...)
			delete(ss.StarbaseDamage, entityID)
			removed = ss.StructureEntities[entityID]
			delete(ss.StructureEntities, entityID)
			found = true
			break
		}
	}
	hook := StructureEntityRemoved
	ss.mu.Unlock()
	// The ball is removed on every anchor swap as well as on death; the hook lets
	// the database owner carry the entity's HP over to the replacement.
	if removed != nil && hook != nil {
		hook(ss, removed)
	}
	return found
}

// StructureEntityRemoved is set by the server entry point (posentity.go) and
// called by RemoveStarbase, outside the lock, for every structure entity dropped.
var StructureEntityRemoved func(ss *SolarSystem, e *Entity)

// GetStarbaseSlim returns a copy of the stored SlimItem for a starbase-slice
// entity (towers, launched structures, force fields). ok=false when unknown.
func (ss *SolarSystem) GetStarbaseSlim(entityID uint32) (SlimItem, bool) {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	for i, bd := range ss.StarbaseBalls {
		if bd.EntityID == entityID {
			return ss.StarbaseSlims[i], true
		}
	}
	return SlimItem{}, false
}

// SetStarbaseBallName renames a starbase-slice ball in place and returns its
// updated ball, slim and damage for a re-broadcast (a structure renamed with
// Set Name: the name rides AddBalls, which every later visitor gets).
// Thread-safe.
func (ss *SolarSystem) SetStarbaseBallName(entityID uint32, name string) (BallDesc, SlimItem, DamageState, bool) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	for i := range ss.StarbaseBalls {
		if ss.StarbaseBalls[i].EntityID == entityID {
			ss.StarbaseBalls[i].Name = name
			return ss.StarbaseBalls[i], ss.StarbaseSlims[i], ss.StarbaseDamage[entityID], true
		}
	}
	return BallDesc{}, SlimItem{}, DamageState{}, false
}

// GetStarbaseBall returns the ball descriptor of a starbase entity (position
// and radius). Thread-safe.
func (ss *SolarSystem) GetStarbaseBall(entityID uint32) (BallDesc, bool) {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	for _, bd := range ss.StarbaseBalls {
		if bd.EntityID == entityID {
			return bd, true
		}
	}
	return BallDesc{}, false
}

// GetStarbasePosition returns the position of a starbase entity. Thread-safe.
func (ss *SolarSystem) GetStarbasePosition(entityID uint32) (Vec3, bool) {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	for _, bd := range ss.StarbaseBalls {
		if bd.EntityID == entityID {
			return bd.Pos, true
		}
	}
	return Vec3{}, false
}

// Restated invGroups/invCategories IDs (destiny does not import db).
const (
	starbaseGroupControlTower = 365 // db.GroupControlTower
	starbaseGroupForceField   = 411 // a tower's shield sphere (category 2)
	starbaseCategoryStructure = 23
)

// starbaseDeployableGroup is db.IsDeployableGroup restated: secure cans,
// audit log cans, mobile sentries and mobile warp disruptors ride the
// starbase slices once launched and are grid information like Mobile Storage.
func starbaseDeployableGroup(groupID int64) bool {
	switch groupID {
	case 340, 448, 336, 361:
		return true
	}
	return false
}

// StarbaseSlimIsSystemWide reports whether a starbase-slice ball is shown to
// every pilot in the system. Control towers and non-structure riders (cyno
// beacons, outpost platforms) are; what is anchored around a tower, the force
// field, deployables and cosmic signature markers are grid-local. See docs/destiny-space.md#visibility.
func StarbaseSlimIsSystemWide(slim SlimItem) bool {
	if slim.GroupID == starbaseGroupForceField || starbaseDeployableGroup(slim.GroupID) ||
		slim.GroupID == GroupCosmicSignature {
		return false
	}
	return slim.CategoryID != starbaseCategoryStructure || slim.GroupID == starbaseGroupControlTower
}

// StarbaseBallVisibleFrom is the one rule for whether a client at pos holds a
// starbase-slice ball: system-wide balls always, the rest within BubbleRadius,
// the grid radius asteroids, billboards and loot use.
func StarbaseBallVisibleFrom(bd BallDesc, slim SlimItem, pos Vec3) bool {
	if StarbaseSlimIsSystemWide(slim) {
		return true
	}
	return pos.Sub(bd.Pos).LenSq() <= BubbleRadius*BubbleRadius
}

// VisibleStarbases returns the starbase-slice balls a client at pos should
// hold: every system-wide ball plus the grid-local ones within BubbleRadius.
// Full SetState builds use this. Safe for concurrent use.
func (ss *SolarSystem) VisibleStarbases(pos Vec3) ([]BallDesc, []SlimItem) {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	var balls []BallDesc
	var slims []SlimItem
	for i, bd := range ss.StarbaseBalls {
		if StarbaseBallVisibleFrom(bd, ss.StarbaseSlims[i], pos) {
			balls = append(balls, bd)
			slims = append(slims, ss.StarbaseSlims[i])
		}
	}
	return balls, slims
}

// NearbyGridStarbases returns the grid-local starbase-slice balls within
// BubbleRadius of pos: the set a warp arrival adds and a departure removes.
// System-wide balls are excluded; the client keeps those across every
// intra-system move. Safe for concurrent use.
func (ss *SolarSystem) NearbyGridStarbases(pos Vec3) ([]BallDesc, []SlimItem) {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	rSq := BubbleRadius * BubbleRadius
	var balls []BallDesc
	var slims []SlimItem
	for i, bd := range ss.StarbaseBalls {
		if StarbaseSlimIsSystemWide(ss.StarbaseSlims[i]) {
			continue
		}
		if pos.Sub(bd.Pos).LenSq() <= rSq {
			balls = append(balls, bd)
			slims = append(slims, ss.StarbaseSlims[i])
		}
	}
	return balls, slims
}

// GetPlayerByCharID finds a player entity by character ID. Returns nil if not found.
func (ss *SolarSystem) GetPlayerByCharID(charID int64) *Entity {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	for _, e := range ss.entities {
		if e.IsPlayer && e.Slim.CharID == charID {
			return e
		}
	}
	return nil
}

// AddEntity registers an entity, assigns it to a bubble and returns the other
// entities in that bubble. merged is true when the position overlapped a second
// bubble; others then holds every entity of the merged bubble. ok is false when
// the system is dead: nothing was added and the caller re-acquires the system.
func (ss *SolarSystem) AddEntity(e *Entity) (others []*Entity, merged bool, ok bool) {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	if ss.dead {
		return nil, false, false
	}

	ss.entities[e.ID] = e
	e.sys.Store(ss)
	pos := e.Pos()
	bub := ss.findOrCreateBubble(pos)
	bub.Entities[e.ID] = e
	e.BubbleID = bub.ID

	if overlap := ss.findOverlappingBubble(bub.ID, pos); overlap != nil {
		ss.mergeBubbles(bub, overlap)
		return bub.AllEntities(), true, true
	}
	return bub.PlayerEntitiesExcept(e.ID), false, true
}

// EntityRemoved is set by the server entry point and called by RemoveEntity, outside the
// lock, for every entity that left the system (death, dock, jump, logoff, bot exit).
var EntityRemoved func(ss *SolarSystem, entityID uint32)

// RemoveEntity removes an entity from the system and its bubble, returning
// the remaining player entities in that bubble (for sending RemoveBalls).
func (ss *SolarSystem) RemoveEntity(entityID uint32) []*Entity {
	remaining, removed := ss.removeEntity(entityID)
	if removed {
		if hook := EntityRemoved; hook != nil {
			hook(ss, entityID)
		}
	}
	return remaining
}

func (ss *SolarSystem) removeEntity(entityID uint32) ([]*Entity, bool) {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	e, ok := ss.entities[entityID]
	if !ok {
		return nil, false
	}
	delete(ss.entities, entityID)
	// A jump can re-add the entity to its next system before this removal;
	// only clear the pointer if it still names this one.
	e.sys.CompareAndSwap(ss, nil)

	if bub := ss.getBubble(e.BubbleID); bub != nil {
		delete(bub.Entities, entityID)
		remaining := bub.PlayerEntities()
		ss.pruneEmptyBubble(bub)
		return remaining, true
	}
	// Not in a bubble (mid-warp or already removed): return every connected player
	// still holding the ball so the caller can send RemoveBalls.
	var observers []*Entity
	for _, other := range ss.entities {
		if other.IsPlayer && other.Conn != nil && other.KnowsBall(entityID) {
			observers = append(observers, other)
		}
	}
	return observers, true
}

// EnsureSameBubble moves entityID into the same bubble as refID.
// Used for dungeon spawns where all NPCs must share the player's grid
// regardless of position offsets that might cross a bubble boundary.
func (ss *SolarSystem) EnsureSameBubble(entityID, refID uint32) {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	e, ok := ss.entities[entityID]
	if !ok {
		return
	}
	ref, ok := ss.entities[refID]
	if !ok {
		return
	}
	if e.BubbleID == ref.BubbleID {
		return
	}
	// Remove from old bubble.
	if old := ss.getBubble(e.BubbleID); old != nil {
		delete(old.Entities, entityID)
	}
	// Pin the entity so the tick loop's containment check does not eject it: its
	// position may lie outside the bubble's radius, but it must share the grid.
	if target := ss.getBubble(ref.BubbleID); target != nil {
		target.Entities[entityID] = e
		e.BubbleID = ref.BubbleID
		e.BubblePinned = true
	}
}

// GetEntity looks up an entity by ID (players, NPCs, or station entities).
// Returns nil if not found.
func (ss *SolarSystem) GetEntity(entityID uint32) *Entity {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	if e, ok := ss.entities[entityID]; ok {
		return e
	}
	// Check station entities.
	if e, ok := ss.StationEntities[entityID]; ok {
		return e
	}
	if e, ok := ss.StructureEntities[entityID]; ok {
		return e
	}
	return nil
}

// AddCelestial appends a celestial to the shared slices after the system is
// populated (outpost construction). Mutates under the lock, like AddStarbase.
func (ss *SolarSystem) AddCelestial(bd BallDesc, slim SlimItem, dmg DamageState) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.CelBalls = append(ss.CelBalls, bd)
	ss.CelSlims = append(ss.CelSlims, slim)
	if ss.CelDamage == nil {
		ss.CelDamage = make(map[uint32]DamageState)
	}
	ss.CelDamage[bd.EntityID] = dmg
}

// RemoveCelestial drops a celestial from the shared slices (an admin-deleted stargate) and
// reports whether it was there. Its anchored bubble stays until the system unloads.
func (ss *SolarSystem) RemoveCelestial(entityID uint32) bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	for i, bd := range ss.CelBalls {
		if bd.EntityID != entityID {
			continue
		}
		// Fresh slices, not in-place shifts: readers range CelBalls without the lock.
		ss.CelBalls = append(append([]BallDesc(nil), ss.CelBalls[:i]...), ss.CelBalls[i+1:]...)
		ss.CelSlims = append(append([]SlimItem(nil), ss.CelSlims[:i]...), ss.CelSlims[i+1:]...)
		delete(ss.CelDamage, entityID)
		return true
	}
	return false
}

// CelestialPos returns the position of a celestial in the shared slices.
func (ss *SolarSystem) CelestialPos(entityID uint32) (Vec3, bool) {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	for _, bd := range ss.CelBalls {
		if bd.EntityID == entityID {
			return bd.Pos, true
		}
	}
	return Vec3{}, false
}

// GetStationEntity returns the registered conquerable-station entity, or nil.
func (ss *SolarSystem) GetStationEntity(entityID uint32) *Entity {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	return ss.StationEntities[entityID]
}

// RegisterStationEntity registers a conquerable station as a targetable
// entity. The station stays in CelBalls/CelSlims for drawing; it is not placed
// in a bubble.
func (ss *SolarSystem) RegisterStationEntity(e *Entity) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.StationEntities == nil {
		ss.StationEntities = make(map[uint32]*Entity)
	}
	ss.StationEntities[e.ID] = e
}

// IsStationEntity reports whether entityID is a registered conquerable station.
func (ss *SolarSystem) IsStationEntity(entityID uint32) bool {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	_, ok := ss.StationEntities[entityID]
	return ok
}

// SetCelDamage stores a damage state for a celestial or station ball so new
// arrivals' SetState payloads carry current HP. CelDamage is written from
// combat and regen goroutines while connections range it; always go through
// this and SnapshotCelDamage.
func (ss *SolarSystem) SetCelDamage(entityID uint32, d DamageState) {
	ss.mu.Lock()
	ss.CelDamage[entityID] = d
	ss.mu.Unlock()
}

// SnapshotCelDamage returns a copy of the celestial damage map.
func (ss *SolarSystem) SnapshotCelDamage() map[uint32]DamageState {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	out := make(map[uint32]DamageState, len(ss.CelDamage))
	for k, v := range ss.CelDamage {
		out[k] = v
	}
	return out
}

// GetAllPlayersInSystem returns all player entities across all bubbles.
// Used for system-wide broadcasts (e.g. station ownership changes).
func (ss *SolarSystem) GetAllPlayersInSystem() []*Entity {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	var out []*Entity
	for _, e := range ss.entities {
		if e.IsPlayer && e.Conn != nil {
			out = append(out, e)
		}
	}
	return out
}

// GetAllPlayerEntities returns every entity with IsPlayer set, regardless of
// whether it has a live connection. This includes bot/headless entities that
// GetAllPlayersInSystem filters out.
func (ss *SolarSystem) GetAllPlayerEntities() []*Entity {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	var out []*Entity
	for _, e := range ss.entities {
		if e.IsPlayer {
			out = append(out, e)
		}
	}
	return out
}

// GetBubblePlayersExcept returns all player entities in the same bubble as
// entityID, excluding that entity itself.
func (ss *SolarSystem) GetBubblePlayersExcept(entityID uint32) []*Entity {
	ss.mu.RLock()
	defer ss.mu.RUnlock()

	e, ok := ss.entities[entityID]
	if !ok {
		return nil
	}
	if bub := ss.getBubble(e.BubbleID); bub != nil {
		return bub.PlayerEntitiesExcept(entityID)
	}
	return nil
}

// GetBubbleConnectedPlayersExcept returns the players in entityID's bubble
// that have a live connection, excluding entityID: the recipient list for a
// packet. GetBubblePlayersExcept is the content list and includes headless
// entities (Conn == nil); a content builder using this one leaves bots off the grid.
func (ss *SolarSystem) GetBubbleConnectedPlayersExcept(entityID uint32) []*Entity {
	ss.mu.RLock()
	defer ss.mu.RUnlock()

	e, ok := ss.entities[entityID]
	if !ok {
		return nil
	}
	if bub := ss.getBubble(e.BubbleID); bub != nil {
		return bub.ConnectedPlayerEntitiesExcept(entityID)
	}
	return nil
}

// GetBubbleNPCs returns all non-player entities in the same bubble as entityID.
// Used by sendSetState to include NPC ships in a player's ballpark.
func (ss *SolarSystem) GetBubbleNPCs(entityID uint32) []*Entity {
	ss.mu.RLock()
	defer ss.mu.RUnlock()

	e, ok := ss.entities[entityID]
	if !ok {
		return nil
	}
	if bub := ss.getBubble(e.BubbleID); bub != nil {
		return bub.NPCEntities()
	}
	return nil
}

// AllEntities returns a snapshot of every entity in the system (used by the
// NPC tick loop to iterate bots).
func (ss *SolarSystem) AllEntities() []*Entity {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	out := make([]*Entity, 0, len(ss.entities))
	for _, e := range ss.entities {
		out = append(out, e)
	}
	return out
}

// FreeBallFootprints returns the collision footprints of every free ball
// within rng metres of center, excluding the given ID (0 for none). An arrival
// lane checks a candidate landing against these as well as its reservations,
// since a ship that landed earlier and parked holds no reservation.
func (ss *SolarSystem) FreeBallFootprints(center Vec3, rng float64, exclude uint32) []BallFootprint {
	rngSq := rng * rng
	var out []BallFootprint
	for _, e := range ss.AllEntities() {
		if e.ID == exclude {
			continue
		}
		fp, ok := e.Footprint()
		if !ok {
			continue
		}
		if fp.Pos.Sub(center).LenSq() > rngSq {
			continue
		}
		out = append(out, fp)
	}
	return out
}

// NextNPCID returns the next available NPC entity ID. IDs are process-global
// so records in the process-wide NPC registry never collide across systems.
func (ss *SolarSystem) NextNPCID() uint32 {
	return npcIDs.next()
}

// NextMissileID returns the next available missile entity ID (process-global).
func (ss *SolarSystem) NextMissileID() uint32 {
	return missileIDs.next()
}
// GetBubbleEntitiesExcept returns every entity (players and NPCs) in the same
// bubble as entityID, excluding that entity. Builds the SetState/AddBalls
// payloads that include NPC ships.
// payloads that include NPC ships.
func (ss *SolarSystem) GetBubbleEntitiesExcept(entityID uint32) []*Entity {
	ss.mu.RLock()
	defer ss.mu.RUnlock()

	e, ok := ss.entities[entityID]
	if !ok {
		return nil
	}
	bub := ss.getBubble(e.BubbleID)
	if bub == nil {
		return nil
	}
	var out []*Entity
	for _, ent := range bub.Entities {
		if ent.ID != entityID {
			out = append(out, ent)
		}
	}
	return out
}

// RemoveFromBubble removes an entity from its current bubble (e.g. entering
// warp tunnel) but keeps it in the system. Returns the remaining player
// entities in the old bubble.
func (ss *SolarSystem) RemoveFromBubble(entityID uint32) []*Entity {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	e, ok := ss.entities[entityID]
	if !ok {
		return nil
	}
	var remaining []*Entity
	if bub := ss.getBubble(e.BubbleID); bub != nil {
		delete(bub.Entities, entityID)
		remaining = bub.PlayerEntities()
		ss.pruneEmptyBubble(bub)
	}
	e.BubbleID = -1
	return remaining
}

// AddToBubble places an entity into the bubble for newPos (arriving from
// warp), removing it from any stale bubble first. Returns the other entities
// in the destination bubble; merged is true when a bubble merge occurred, and
// others then holds every entity of the merged bubble.
func (ss *SolarSystem) AddToBubble(entityID uint32, newPos Vec3) (others []*Entity, merged bool) {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	e, ok := ss.entities[entityID]
	if !ok {
		return nil, false
	}
	// Clear stale membership: the entity should carry BubbleID -1 here, but a race
	// with the tick loop's bubble transition can leave it in a bubble.
	if oldBub := ss.getBubble(e.BubbleID); oldBub != nil {
		delete(oldBub.Entities, entityID)
		ss.pruneEmptyBubble(oldBub)
	}
	bub := ss.findOrCreateBubble(newPos)
	bub.Entities[e.ID] = e
	e.BubbleID = bub.ID

	if overlap := ss.findOverlappingBubble(bub.ID, newPos); overlap != nil {
		ss.mergeBubbles(bub, overlap)
		return bub.AllEntities(), true
	}
	return bub.PlayerEntitiesExcept(entityID), false
}

// TransferBubble moves an entity to the bubble covering newPos. Returns the
// players left behind in the old bubble and the entities in the new one; when
// a bubble merge occurred, merged is true and joined holds every entity of the
// merged bubble.
func (ss *SolarSystem) TransferBubble(entityID uint32, newPos Vec3) (left, joined []*Entity, merged bool) {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	e, ok := ss.entities[entityID]
	if !ok {
		return nil, nil, false
	}

	// Remove from old bubble.
	if oldBub := ss.getBubble(e.BubbleID); oldBub != nil {
		delete(oldBub.Entities, entityID)
		left = oldBub.PlayerEntities()
		ss.pruneEmptyBubble(oldBub)
	}

	// Add to new bubble. Clear any force-pin since the entity is now
	// placed at its real position.
	newBub := ss.findOrCreateBubble(newPos)
	newBub.Entities[e.ID] = e
	e.BubblePinned = false
	e.BubbleID = newBub.ID

	if overlap := ss.findOverlappingBubble(newBub.ID, newPos); overlap != nil {
		ss.mergeBubbles(newBub, overlap)
		return left, newBub.AllEntities(), true
	}
	joined = newBub.PlayerEntitiesExcept(entityID)
	return left, joined, false
}

// PlayerCount returns the number of player entities in the system.
func (ss *SolarSystem) PlayerCount() int {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	return ss.playerCountLocked()
}

// playerCountLocked counts player entities. Caller must hold ss.mu.
func (ss *SolarSystem) playerCountLocked() int {
	n := 0
	for _, e := range ss.entities {
		if e.IsPlayer {
			n++
		}
	}
	return n
}

// SeedCelestialBubbles pre-creates anchored bubbles at stargates (groupID 10),
// stations (15) and asteroid belts (9) so players near a celestial share one
// grid. Call during system init, before GenerateAsteroids, GenerateBillboards
// and NPC spawning, so those entities join the right bubbles.
func (ss *SolarSystem) SeedCelestialBubbles(celBalls []BallDesc, celSlims []SlimItem) {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	for i, slim := range celSlims {
		switch slim.GroupID {
		case 10, 15, 9: // stargate, station, asteroid belt
		default:
			continue
		}
		pos := celBalls[i].Pos
		// Two celestials < 250 km apart share one bubble (e.g. station
		// orbiting planet near a gate). Skip if already covered.
		covered := false
		for _, bub := range ss.bubbles {
			if bub.Contains(pos) {
				covered = true
				break
			}
		}
		if covered {
			continue
		}
		bub := NewBubble(ss.nextBub, pos)
		bub.AnchorID = slim.ItemID
		ss.nextBub++
		ss.addBubbleLocked(bub)
	}
	if len(ss.bubbles) > 0 {
		log.Printf("[system %d] seeded %d anchored bubbles (gates/stations/belts)", ss.ID, len(ss.bubbles))
	}
}

// findOrCreateBubble returns an existing bubble that contains pos, or creates
// a new one centered at pos. Prefers anchored bubbles (celestial grids) over
// dynamic ones to avoid grid fragmentation. Caller must hold ss.mu for writing.
func (ss *SolarSystem) findOrCreateBubble(pos Vec3) *Bubble {
	// First pass: prefer anchored (celestial) bubbles.
	for _, bub := range ss.bubbles {
		if bub.AnchorID != 0 && bub.Contains(pos) {
			return bub
		}
	}
	// Second pass: any bubble that contains the position.
	for _, bub := range ss.bubbles {
		if bub.Contains(pos) {
			return bub
		}
	}
	bub := NewBubble(ss.nextBub, pos)
	ss.nextBub++
	ss.addBubbleLocked(bub)
	return bub
}

// addBubbleLocked appends a bubble to the slice and the ID index together.
// Caller must hold ss.mu for writing.
func (ss *SolarSystem) addBubbleLocked(bub *Bubble) {
	ss.bubbles = append(ss.bubbles, bub)
	if ss.bubblesByID == nil {
		ss.bubblesByID = make(map[int]*Bubble, len(ss.bubbles))
	}
	ss.bubblesByID[bub.ID] = bub
}

// getBubble returns the bubble with the given ID, or nil. Caller must hold ss.mu.
func (ss *SolarSystem) getBubble(id int) *Bubble {
	if id < 0 {
		return nil
	}
	return ss.bubblesByID[id]
}

// pruneEmptyBubble removes a dynamic (non-anchored) bubble that has no
// entities. Anchored bubbles (celestial grids) are never pruned. Caller must
// hold ss.mu for writing.
func (ss *SolarSystem) pruneEmptyBubble(bub *Bubble) {
	if bub == nil || len(bub.Entities) != 0 || bub.AnchorID != 0 {
		return
	}
	for i, b := range ss.bubbles {
		if b.ID == bub.ID {
			ss.bubbles = append(ss.bubbles[:i], ss.bubbles[i+1:]...)
			delete(ss.bubblesByID, bub.ID)
			logx.Debugf("[system %d] pruned empty dynamic bubble %d", ss.ID, bub.ID)
			return
		}
	}
}

// findOverlappingBubble returns another bubble (not bubID) whose radius
// contains pos. Returns nil if no overlap. Caller must hold ss.mu.
func (ss *SolarSystem) findOverlappingBubble(bubID int, pos Vec3) *Bubble {
	for _, bub := range ss.bubbles {
		if bub.ID != bubID && bub.Contains(pos) {
			return bub
		}
	}
	return nil
}

// mergeBubbles moves all entities from absorbBub into keepBub, updates their
// BubbleID, and prunes the empty absorbed bubble. Caller must hold ss.mu.
func (ss *SolarSystem) mergeBubbles(keepBub, absorbBub *Bubble) {
	for id, e := range absorbBub.Entities {
		keepBub.Entities[id] = e
		e.BubbleID = keepBub.ID
	}
	absorbBub.Entities = make(map[uint32]*Entity)
	ss.pruneEmptyBubble(absorbBub)
	log.Printf("[system %d] merged bubble %d into bubble %d", ss.ID, absorbBub.ID, keepBub.ID)
}

// ── Billboard generation ──────────────────────────────────────────────────────

// GenerateBillboards spawns Interbus Billboard entities (typeID 11136) 7km
// from each stargate in high-sec and low-sec systems. Entity IDs start at
// BillboardIDBase (ballids.go). Populates ss.BillboardBalls, BillboardSlims,
// BillboardDamage.
func (ss *SolarSystem) GenerateBillboards(celBalls []BallDesc, celSlims []SlimItem) {
	// Only high-sec (>= 0.5) and low-sec (0.1–0.4).
	if ss.Security < 0.1 {
		return
	}

	ss.BillboardDamage = make(map[uint32]DamageState)
	nextID := BillboardIDBase
	const offsetDist = 7_000.0 // 7km from gate

	for i, slim := range celSlims {
		if slim.GroupID != 10 { // stargate
			continue
		}
		gatePos := celBalls[i].Pos

		// Compute perpendicular offset in XZ plane from origin→gate direction.
		dx := gatePos.X
		dz := gatePos.Z
		lenXZ := math.Sqrt(dx*dx + dz*dz)
		var offX, offZ float64
		if lenXZ > 1.0 {
			// Rotate 90° in XZ: (dx, dz) → (-dz, dx) normalized.
			offX = -dz / lenXZ * offsetDist
			offZ = dx / lenXZ * offsetDist
		} else {
			// Gate near origin: use a fixed X offset.
			offX = offsetDist
		}

		entityID := nextID
		nextID++

		pos := Vec3{
			X: gatePos.X + offX,
			Y: gatePos.Y,
			Z: gatePos.Z + offZ,
		}

		ss.BillboardBalls = append(ss.BillboardBalls, BallDesc{
			EntityID: entityID,
			Mode:     ModeRigid,
			SubType:  6, // generic celestial (not dockable)
			Pos:      pos,
			Radius:   1000.0,
		})
		ss.BillboardSlims = append(ss.BillboardSlims, SlimItem{
			BallID:     int64(entityID),
			ItemID:     int64(entityID),
			TypeID:     11136, // Interbus Billboard
			GroupID:    323,   // Billboard
			CategoryID: 2,    // Celestial
			OwnerID:    1000125, // CONCORD
		})
		ss.BillboardDamage[entityID] = DamageState{
			Shield: 1.0, Tau: 1e9, Armor: 1.0, Structure: 1.0,
		}
	}
}

// NearbyBillboards returns billboards within BubbleRadius of playerPos.
func (ss *SolarSystem) NearbyBillboards(playerPos Vec3) ([]BallDesc, []SlimItem, map[uint32]DamageState) {
	rSq := BubbleRadius * BubbleRadius
	var balls []BallDesc
	var slims []SlimItem
	dmg := make(map[uint32]DamageState)
	for i, bd := range ss.BillboardBalls {
		if playerPos.Sub(bd.Pos).LenSq() <= rSq {
			balls = append(balls, bd)
			slims = append(slims, ss.BillboardSlims[i])
			if d, ok := ss.BillboardDamage[bd.EntityID]; ok {
				dmg[bd.EntityID] = d
			}
		}
	}
	return balls, slims, dmg
}

// ── Asteroid generation ───────────────────────────────────────────────────────

// OreVariant holds a single ore variant (base, concentrated, or dense).
type OreVariant struct {
	TypeID  int64
	GroupID int64
}

// OreFamily describes an ore with its 3 variants and its cargo volume.
type OreFamily struct {
	Name         string
	Base         OreVariant
	Concentrated OreVariant
	Dense        OreVariant

	// UnitVolume is m³ per ore unit (invTypes.volume). Rock sizes are budgeted in
	// m³ and converted to units through this, so rocks of one visual size hold
	// one cargo volume whatever the ore.
	UnitVolume float64
}

// OreFamilies maps family name → OreFamily. TypeIDs, groupIDs and volumes all
// match invTypes/invGroups; the variant names are the classic-era ones
// (Concentrated/Dense Veldspar, Vivid/Radiant Hemorphite, …).
var OreFamilies = map[string]OreFamily{
	"Veldspar":    {Name: "Veldspar", Base: OreVariant{1230, 462}, Concentrated: OreVariant{17470, 462}, Dense: OreVariant{17471, 462}, UnitVolume: 0.1},
	"Scordite":    {Name: "Scordite", Base: OreVariant{1228, 460}, Concentrated: OreVariant{17463, 460}, Dense: OreVariant{17464, 460}, UnitVolume: 0.15},
	"Pyroxeres":   {Name: "Pyroxeres", Base: OreVariant{1224, 459}, Concentrated: OreVariant{17459, 459}, Dense: OreVariant{17460, 459}, UnitVolume: 0.3},
	"Plagioclase": {Name: "Plagioclase", Base: OreVariant{18, 458}, Concentrated: OreVariant{17455, 458}, Dense: OreVariant{17456, 458}, UnitVolume: 0.35},
	"Omber":       {Name: "Omber", Base: OreVariant{1227, 469}, Concentrated: OreVariant{17867, 469}, Dense: OreVariant{17868, 469}, UnitVolume: 0.6},
	"Kernite":     {Name: "Kernite", Base: OreVariant{20, 457}, Concentrated: OreVariant{17452, 457}, Dense: OreVariant{17453, 457}, UnitVolume: 1.2},
	"Jaspet":      {Name: "Jaspet", Base: OreVariant{1226, 456}, Concentrated: OreVariant{17448, 456}, Dense: OreVariant{17449, 456}, UnitVolume: 2},
	"Hemorphite":  {Name: "Hemorphite", Base: OreVariant{1231, 455}, Concentrated: OreVariant{17444, 455}, Dense: OreVariant{17445, 455}, UnitVolume: 3},
	"Hedbergite":  {Name: "Hedbergite", Base: OreVariant{21, 454}, Concentrated: OreVariant{17440, 454}, Dense: OreVariant{17441, 454}, UnitVolume: 3},
	"Gneiss":      {Name: "Gneiss", Base: OreVariant{1229, 467}, Concentrated: OreVariant{17865, 467}, Dense: OreVariant{17866, 467}, UnitVolume: 5},
	"Dark Ochre":  {Name: "Dark Ochre", Base: OreVariant{1232, 453}, Concentrated: OreVariant{17436, 453}, Dense: OreVariant{17437, 453}, UnitVolume: 8},
	"Crokite":     {Name: "Crokite", Base: OreVariant{1225, 452}, Concentrated: OreVariant{17432, 452}, Dense: OreVariant{17433, 452}, UnitVolume: 16},
	"Spodumain":   {Name: "Spodumain", Base: OreVariant{19, 461}, Concentrated: OreVariant{17466, 461}, Dense: OreVariant{17467, 461}, UnitVolume: 16},
	"Bistot":      {Name: "Bistot", Base: OreVariant{1223, 451}, Concentrated: OreVariant{17428, 451}, Dense: OreVariant{17429, 451}, UnitVolume: 16},
	"Arkonor":     {Name: "Arkonor", Base: OreVariant{22, 450}, Concentrated: OreVariant{17425, 450}, Dense: OreVariant{17426, 450}, UnitVolume: 16},
	"Mercoxit":    {Name: "Mercoxit", Base: OreVariant{11396, 468}, Concentrated: OreVariant{17869, 468}, Dense: OreVariant{17870, 468}, UnitVolume: 40},
}

// Asteroid group IDs with special mining rules. Real invGroups IDs: mining
// crystals match a rock by specialisationAsteroidGroup against its groupID, so
// an invented value breaks every crystal for that ore.
const (
	GroupIce         int64 = 465 // ice asteroids, mined with an ice harvester
	GroupMercoxit    int64 = 468 // Mercoxit, mined with a deep core miner
	CategoryAsteroid int64 = 25
)

// ── Empire blocs ──────────────────────────────────────────────────────────────

// OreBloc is the empire grouping that gates lowsec ore and faction ice: Jaspet
// and Hemorphite spawn in Amarr and Gallente lowsec, Hedbergite in Caldari and
// Minmatar lowsec, and each empire's space carries one faction ice.
type OreBloc int

const (
	BlocNone OreBloc = iota // unclaimed space, most of 0.0
	BlocAmarr
	BlocCaldari
	BlocGallente
	BlocMinmatar
)

func (b OreBloc) String() string {
	switch b {
	case BlocAmarr:
		return "Amarr"
	case BlocCaldari:
		return "Caldari"
	case BlocGallente:
		return "Gallente"
	case BlocMinmatar:
		return "Minmatar"
	default:
		return "Unclaimed"
	}
}

// BlocForFaction maps a system's mapSolarSystems.factionID to its empire bloc.
// The Amarr client states (Ammatar Mandate, Khanid Kingdom) count as Amarr;
// every other faction (pirate holdings, Syndicate, CONCORD) is unclaimed.
func BlocForFaction(factionID int64) OreBloc {
	switch factionID {
	case 500003, 500007, 500008: // Amarr Empire, Ammatar Mandate, Khanid Kingdom
		return BlocAmarr
	case 500001: // Caldari State
		return BlocCaldari
	case 500004: // Gallente Federation
		return BlocGallente
	case 500002: // Minmatar Republic
		return BlocMinmatar
	default:
		return BlocNone
	}
}

// WeightedOre pairs an ore family name with a relative spawn weight.
type WeightedOre struct {
	Name   string
	Weight int
}

// OreDistribution returns the weighted ore table for a system from its
// security status and sovereign faction. Lowsec specials follow the empire
// bloc; space with no sovereign empire carries both at half weight.
// See docs/destiny-space.md#ore-bands.
func OreDistribution(security float64, factionID int64) []WeightedOre {
	bloc := BlocForFaction(factionID)
	switch {
	case security >= 0.95: // 1.0
		return []WeightedOre{{"Veldspar", 55}, {"Scordite", 30}, {"Pyroxeres", 10}, {"Plagioclase", 5}}
	case security >= 0.85: // 0.9
		return []WeightedOre{{"Veldspar", 45}, {"Scordite", 28}, {"Pyroxeres", 15}, {"Plagioclase", 12}}
	case security >= 0.65: // 0.7–0.8
		return []WeightedOre{{"Veldspar", 34}, {"Scordite", 24}, {"Pyroxeres", 18}, {"Plagioclase", 16},
			{"Omber", 4}, {"Kernite", 4}}
	case security >= 0.45: // 0.5-0.6: Omber and Kernite arrive in force
		return []WeightedOre{{"Veldspar", 24}, {"Scordite", 20}, {"Pyroxeres", 15}, {"Plagioclase", 14},
			{"Omber", 14}, {"Kernite", 13}}
	case security >= 0.35: // 0.4: the only band that carries Jaspet
		r := []WeightedOre{{"Veldspar", 20}, {"Scordite", 17}, {"Pyroxeres", 13}, {"Plagioclase", 12},
			{"Omber", 16}, {"Kernite", 15}}
		switch bloc {
		case BlocAmarr, BlocGallente:
			r = append(r, WeightedOre{"Jaspet", 14})
		case BlocNone:
			r = append(r, WeightedOre{"Jaspet", 7})
		}
		return r
	case security > 0.0: // 0.3–0.1
		r := []WeightedOre{{"Veldspar", 18}, {"Scordite", 15}, {"Pyroxeres", 12}, {"Plagioclase", 11},
			{"Omber", 17}, {"Kernite", 16}}
		switch bloc {
		case BlocAmarr, BlocGallente:
			r = append(r, WeightedOre{"Hemorphite", 18})
		case BlocCaldari, BlocMinmatar:
			r = append(r, WeightedOre{"Hedbergite", 18})
		default:
			r = append(r, WeightedOre{"Hemorphite", 9}, WeightedOre{"Hedbergite", 9})
		}
		return r
	}
	return nullOreDistribution(security)
}

// nullOreDistribution is the 0.0 table. Every ore is present at every depth;
// the weights swing with truesec.
func nullOreDistribution(security float64) []WeightedOre {
	switch {
	case security > -0.25: // shallow 0.0: Gneiss country
		return []WeightedOre{{"Veldspar", 15}, {"Scordite", 13}, {"Pyroxeres", 9}, {"Plagioclase", 9},
			{"Omber", 10}, {"Kernite", 10}, {"Jaspet", 6}, {"Hemorphite", 5}, {"Hedbergite", 5},
			{"Gneiss", 12}, {"Dark Ochre", 3}, {"Crokite", 2}, {"Spodumain", 1}, {"Bistot", 1}, {"Arkonor", 1}}
	case security > -0.5: // mid 0.0: Crokite, Dark Ochre, Spodumain
		return []WeightedOre{{"Veldspar", 10}, {"Scordite", 9}, {"Pyroxeres", 6}, {"Plagioclase", 6},
			{"Omber", 7}, {"Kernite", 7}, {"Jaspet", 4}, {"Hemorphite", 4}, {"Hedbergite", 4},
			{"Gneiss", 12}, {"Dark Ochre", 10}, {"Crokite", 9}, {"Spodumain", 6}, {"Bistot", 3}, {"Arkonor", 3}}
	case security > -0.75: // deep 0.0: Bistot and Arkonor
		return []WeightedOre{{"Veldspar", 7}, {"Scordite", 6}, {"Pyroxeres", 4}, {"Plagioclase", 4},
			{"Omber", 5}, {"Kernite", 5}, {"Jaspet", 3}, {"Hemorphite", 3}, {"Hedbergite", 3},
			{"Gneiss", 9}, {"Dark Ochre", 9}, {"Crokite", 11}, {"Spodumain", 9}, {"Bistot", 11}, {"Arkonor", 11}}
	default: // deepest 0.0: the only band with loose Mercoxit rocks
		return []WeightedOre{{"Veldspar", 5}, {"Scordite", 5}, {"Pyroxeres", 3}, {"Plagioclase", 3},
			{"Omber", 3}, {"Kernite", 3}, {"Jaspet", 2}, {"Hemorphite", 2}, {"Hedbergite", 2},
			{"Gneiss", 7}, {"Dark Ochre", 8}, {"Crokite", 12}, {"Spodumain", 9}, {"Bistot", 15},
			{"Arkonor", 15}, {"Mercoxit", 6}}
	}
}

// ── Ore deposits (0.0 clusters) ───────────────────────────────────────────────

// OreDeposit is a classic 0.0 deposit site: a cluster of 2–3 special ores
// spawned together around one anchor instead of one ore per rock.
type OreDeposit struct {
	Name   string
	Ores   []string
	MaxSec float64 // requires system security <= this
	Weight int     // relative frequency among the eligible deposits
}

// OreDeposits are the era's deposit types. A 0.0 belt rolls some of its anchor
// groups as deposits, so nullsec ore clusters by site instead of scattering
// evenly. Mercoxit deposits stay rare where the truesec allows them.
var OreDeposits = []OreDeposit{
	{"Crokite, Dark Ochre and Gneiss Deposit", []string{"Crokite", "Dark Ochre", "Gneiss"}, -0.25, 40},
	{"Spodumain, Crokite and Dark Ochre Deposit", []string{"Spodumain", "Crokite", "Dark Ochre"}, -0.4, 30},
	{"Arkonor and Bistot Deposit", []string{"Arkonor", "Bistot"}, -0.55, 25},
	{"Mercoxit Mineral Deposit", []string{"Mercoxit"}, -0.75, 6},
}

// EligibleDeposits returns the deposit types a system's truesec allows.
func EligibleDeposits(security float64) []OreDeposit {
	var out []OreDeposit
	for _, d := range OreDeposits {
		if security <= d.MaxSec {
			out = append(out, d)
		}
	}
	return out
}

// pickDeposit rolls one deposit from a weighted eligible list.
func pickDeposit(rng *rand.Rand, deposits []OreDeposit) OreDeposit {
	total := 0
	for _, d := range deposits {
		total += d.Weight
	}
	if total <= 0 {
		return deposits[0]
	}
	roll := rng.Intn(total)
	for _, d := range deposits {
		roll -= d.Weight
		if roll < 0 {
			return d
		}
	}
	return deposits[len(deposits)-1]
}

// depositChancePct is how often a 0.0 anchor group is a deposit cluster
// instead of a plain weighted pick.
const depositChancePct = 30

// ── Ice ───────────────────────────────────────────────────────────────────────

// IceType is one ice variant that can spawn in a system, with the m³ budget
// one formation is worth. Ice is 1000 m³ per unit and a harvester pulls one
// unit per cycle, so budgets stay small in unit terms.
type IceType struct {
	TypeID int64
	Name   string
	Weight int
	VolMin float64 // m³; ice is 1000 m³/unit, so units × 1000
	VolMax float64
}

// Faction ice typeIDs, one per empire, per the classic split.
var factionIce = map[OreBloc][2]IceType{
	BlocAmarr:    {{TypeID: 16262, Name: "Clear Icicle"}, {TypeID: 17978, Name: "Enriched Clear Icicle"}},
	BlocCaldari:  {{TypeID: 16265, Name: "White Glaze"}, {TypeID: 17976, Name: "Pristine White Glaze"}},
	BlocGallente: {{TypeID: 16264, Name: "Blue Ice"}, {TypeID: 17975, Name: "Thick Blue Ice"}},
	BlocMinmatar: {{TypeID: 16263, Name: "Glacial Mass"}, {TypeID: 17977, Name: "Smooth Glacial Mass"}},
}

// IceTypesFor returns the weighted ice an Ice Field celestial in this system
// holds. Only an Ice Field celestial (IceFieldTypeID) rolls ice; this decides
// which. Empire space, high and low, carries its empire's base ice only; 0.0
// adds the richer variant and the unaligned deep ices. See docs/destiny-space.md#ice.
func IceTypesFor(security float64, factionID int64) []IceType {
	bloc := BlocForFaction(factionID)
	var out []IceType

	addFaction := func(b OreBloc, baseW, richW int) {
		pair, ok := factionIce[b]
		if !ok {
			return
		}
		base := pair[0]
		base.Weight, base.VolMin, base.VolMax = baseW, 25_000, 75_000
		out = append(out, base)
		if richW > 0 {
			rich := pair[1]
			rich.Weight, rich.VolMin, rich.VolMax = richW, 20_000, 60_000
			out = append(out, rich)
		}
	}

	if security > 0.0 { // empire space: base faction ice only
		if bloc == BlocNone {
			for _, b := range []OreBloc{BlocAmarr, BlocCaldari, BlocGallente, BlocMinmatar} {
				addFaction(b, 25, 0)
			}
		} else {
			addFaction(bloc, 100, 0)
		}
		return out
	}

	// 0.0: faction ice plus its richer variant where an empire holds sov.
	if bloc == BlocNone {
		for _, b := range []OreBloc{BlocAmarr, BlocCaldari, BlocGallente, BlocMinmatar} {
			addFaction(b, 8, 4)
		}
	} else {
		addFaction(bloc, 30, 15)
	}

	// Unaligned deep-space ices, gated by depth.
	deep := []struct {
		t     IceType
		maxSec float64
	}{
		{IceType{TypeID: 16266, Name: "Glare Crust", Weight: 20, VolMin: 15_000, VolMax: 40_000}, -0.1},
		{IceType{TypeID: 16267, Name: "Dark Glitter", Weight: 15, VolMin: 12_000, VolMax: 35_000}, -0.35},
		{IceType{TypeID: 16268, Name: "Gelidus", Weight: 10, VolMin: 10_000, VolMax: 30_000}, -0.6},
		{IceType{TypeID: 16269, Name: "Krystallos", Weight: 6, VolMin: 8_000, VolMax: 25_000}, -0.8},
	}
	for _, d := range deep {
		if security <= d.maxSec {
			out = append(out, d.t)
		}
	}
	return out
}

// pickIceType rolls one ice type from a weighted table.
func pickIceType(rng *rand.Rand, types []IceType) IceType {
	total := 0
	for _, t := range types {
		total += t.Weight
	}
	if total <= 0 {
		return types[0]
	}
	roll := rng.Intn(total)
	for _, t := range types {
		roll -= t.Weight
		if roll < 0 {
			return t
		}
	}
	return types[len(types)-1]
}

// SecurityClass is the A-H security classification. OreDistribution and
// IceTypesFor pick the ore; this is the density knob: how many rocks and how
// big, not which ore they hold.
type SecurityClass string

const (
	SecClassA SecurityClass = "A" // 1.0
	SecClassB SecurityClass = "B" // 0.9
	SecClassC SecurityClass = "C" // 0.7-0.8
	SecClassD SecurityClass = "D" // 0.5-0.6
	SecClassE SecurityClass = "E" // 0.3-0.4
	SecClassF SecurityClass = "F" // 0.1-0.2
	SecClassG SecurityClass = "G" // -0.0 to -0.4
	SecClassH SecurityClass = "H" // -0.5 to -1.0
)

// SecurityClassFromStatus maps a system security value to a SecurityClass.
func SecurityClassFromStatus(sec float64) SecurityClass {
	switch {
	case sec >= 0.95:
		return SecClassA
	case sec >= 0.85:
		return SecClassB
	case sec >= 0.65:
		return SecClassC
	case sec >= 0.45:
		return SecClassD
	case sec >= 0.25:
		return SecClassE
	case sec > 0.0:
		return SecClassF
	case sec > -0.5:
		return SecClassG
	default:
		return SecClassH
	}
}

// pickOreFamily selects a random ore family weighted by a distribution table.
func pickOreFamily(rng *rand.Rand, dist []WeightedOre) OreFamily {
	totalWeight := 0
	for _, wo := range dist {
		totalWeight += wo.Weight
	}
	if totalWeight <= 0 {
		return OreFamilies["Veldspar"]
	}
	roll := rng.Intn(totalWeight)
	for _, wo := range dist {
		roll -= wo.Weight
		if roll < 0 {
			return OreFamilies[wo.Name]
		}
	}
	return OreFamilies[dist[0].Name]
}

// pickVariant selects a variant: 70% base, 20% concentrated, 10% dense.
func pickVariant(rng *rand.Rand, family OreFamily) OreVariant {
	roll := rng.Intn(100)
	switch {
	case roll < 70:
		return family.Base
	case roll < 90:
		return family.Concentrated
	default:
		return family.Dense
	}
}

// UnitVolume returns m³ per unit for an asteroid typeID (every ore variant,
// plus ice at 1000 m³). Unknown types fall back to 1 m³.
func UnitVolume(typeID int64) float64 {
	if v, ok := unitVolumes[typeID]; ok {
		return v
	}
	return 1.0
}

// maxRockRadius keeps the biggest ice formations from swallowing a belt.
const maxRockRadius = 3_500.0

// RadiusFromVolume computes a rock's visual radius from the m³ of ore it
// holds, logarithmically (30,000 m³ renders ~2400 m, 1,000 m³ ~950 m). Sizing
// by volume keeps a 16 m³/unit Arkonor rock from drawing as a pebble beside a
// Veldspar boulder holding a tenth the cargo.
func RadiusFromVolume(m3 float64) float64 {
	if m3 <= 0 {
		return 150.0
	}
	r := 150.0 + 450.0*math.Log1p(m3*5e-03)
	if r > maxRockRadius {
		return maxRockRadius
	}
	return r
}

// radiusForType is RadiusFromVolume for a rock of typeID holding qty units.
func radiusForType(typeID, qty int64) float64 {
	return RadiusFromVolume(float64(qty) * UnitVolume(typeID))
}

// unitsForVolume converts an m³ rock budget into ore units of a given type.
func unitsForVolume(typeID int64, m3 float64) int64 {
	v := UnitVolume(typeID)
	if v <= 0 {
		v = 1
	}
	q := int64(m3 / v)
	if q < 1 {
		q = 1
	}
	return q
}

// addAsteroid appends one asteroid to all tracking structures on a SolarSystem.
func (ss *SolarSystem) addAsteroid(entityID uint32, beltID int64, pos Vec3, qty int64, variant OreVariant) {
	radius := radiusForType(variant.TypeID, qty)
	ss.AsteroidBalls = append(ss.AsteroidBalls, BallDesc{
		EntityID: entityID, Mode: ModeRigid, SubType: 2,
		Pos: pos, Radius: radius,
	})
	ss.AsteroidSlims = append(ss.AsteroidSlims, SlimItem{
		BallID: int64(entityID), ItemID: int64(entityID),
		TypeID: variant.TypeID, GroupID: variant.GroupID, CategoryID: CategoryAsteroid,
	})
	ss.AsteroidDamage[entityID] = DamageState{
		Shield: 1.0, Tau: 1e9, Armor: 1.0, Structure: 1.0,
	}
	ss.AsteroidQty[entityID] = qty
	ss.AsteroidMaxQty[entityID] = qty
	ss.AsteroidBelt[entityID] = beltID
}

// pickOreVariant selects a variant with 60% chance of dominant family,
// 40% random from the system's distribution.
func pickOreVariant(rng *rand.Rand, dominant OreFamily, dist []WeightedOre) OreVariant {
	return pickOreVariantChance(rng, dominant, dist, 60)
}

// pickOreVariantChance is pickOreVariant with an explicit percentage chance of
// staying in the dominant family. Cluster satellites use a high chance so a big
// rock is visibly surrounded by its own kind.
func pickOreVariantChance(rng *rand.Rand, dominant OreFamily, dist []WeightedOre, chance int) OreVariant {
	var family OreFamily
	if rng.Intn(100) < chance {
		family = dominant
	} else {
		family = pickOreFamily(rng, dist)
	}
	return pickVariant(rng, family)
}

// scatterXZ returns a position at distance dist from center in the XZ plane
// with slight Y jitter to keep the belt flat.
func scatterXZ(rng *rand.Rand, center Vec3, dist, ySpread float64) Vec3 {
	theta := rng.Float64() * 2 * math.Pi
	return Vec3{
		X: center.X + dist*math.Cos(theta),
		Y: center.Y + (rng.Float64()-0.5)*ySpread,
		Z: center.Z + dist*math.Sin(theta),
	}
}

// placedRock tracks a placed asteroid's position and radius for overlap checks.
type placedRock struct {
	pos    Vec3
	radius float64
}

// rockMargin is the extra clearance between two rock surfaces, as a multiple
// of the sum of their radii; it is what keeps a dense field from clipping.
const rockMargin = 1.15

// worstOverlap returns the already-placed rock that a new rock of newRadius at
// pos penetrates the deepest, or ok=false when nothing is clipped.
func worstOverlap(pos Vec3, newRadius float64, placed []placedRock) (placedRock, bool) {
	var worst placedRock
	deepest := 0.0
	found := false
	for _, pr := range placed {
		dx := pos.X - pr.pos.X
		dy := pos.Y - pr.pos.Y
		dz := pos.Z - pr.pos.Z
		dist := math.Sqrt(dx*dx + dy*dy + dz*dz)
		minDist := (newRadius + pr.radius) * rockMargin
		if pen := minDist - dist; pen > deepest {
			worst, deepest, found = pr, pen, true
		}
	}
	return worst, found
}

// pushOff slides pos directly away from pr until the two rocks clear each other
// by rockMargin.
func pushOff(rng *rand.Rand, pos Vec3, pr placedRock, newRadius float64) Vec3 {
	dx := pos.X - pr.pos.X
	dy := pos.Y - pr.pos.Y
	dz := pos.Z - pr.pos.Z
	dist := math.Sqrt(dx*dx + dy*dy + dz*dz)
	if dist < 1.0 {
		// Concentric: pick a direction so the push is well-defined.
		theta := rng.Float64() * 2 * math.Pi
		dx, dy, dz, dist = math.Cos(theta), 0, math.Sin(theta), 1.0
	}
	scale := (newRadius + pr.radius) * rockMargin * 1.001 / dist
	return Vec3{
		X: pr.pos.X + dx*scale,
		Y: pr.pos.Y + dy*scale,
		Z: pr.pos.Z + dz*scale,
	}
}

// pushOutOfClearing keeps a rock outside the sphere of radius minDist centred on
// the belt's warp-in point. The push is XZ-only: shoving along the full 3D
// radial would fling rocks that sit near the belt axis straight up and spike the
// field vertically. Reports whether it had to move.
func pushOutOfClearing(rng *rand.Rand, pos, center Vec3, minDist float64) (Vec3, bool) {
	dy := pos.Y - center.Y
	if math.Abs(dy) >= minDist {
		return pos, false // already above/below the clearing
	}
	needXZ := math.Sqrt(minDist*minDist - dy*dy)
	dx, dz := pos.X-center.X, pos.Z-center.Z
	dist := math.Hypot(dx, dz)
	if dist >= needXZ {
		return pos, false
	}
	if dist < 1.0 {
		theta := rng.Float64() * 2 * math.Pi
		dx, dz, dist = math.Cos(theta), math.Sin(theta), 1.0
	}
	scale := needXZ * 1.001 / dist
	pos.X = center.X + dx*scale
	pos.Z = center.Z + dz*scale
	return pos, true
}

// stepOutward moves pos further from center in XZ by step metres, keeping its
// height. Because distance from the centre only ever grows, a rock that already
// clears the warp-in bubble stays clear.
func stepOutward(rng *rand.Rand, pos, center Vec3, step float64) Vec3 {
	dx, dz := pos.X-center.X, pos.Z-center.Z
	dist := math.Hypot(dx, dz)
	if dist < 1.0 {
		theta := rng.Float64() * 2 * math.Pi
		dx, dz, dist = math.Cos(theta), math.Sin(theta), 1.0
	}
	scale := (dist + step) / dist
	pos.X = center.X + dx*scale
	pos.Z = center.Z + dz*scale
	return pos
}

// settleRock resolves a new rock against every rock already placed in the belt
// and against the warp-in clearing: a relax phase alternating the two pushes,
// then an outward walk. The walk is the guaranteed exit: distance from the
// centre only grows, so the clearing holds and the rock reaches empty space.
func settleRock(rng *rand.Rand, pos Vec3, radius float64, center Vec3, placed []placedRock) Vec3 {
	const relaxPasses = 12
	for i := 0; i < relaxPasses; i++ {
		moved := false
		if pr, ok := worstOverlap(pos, radius, placed); ok {
			pos = pushOff(rng, pos, pr, radius)
			moved = true
		}
		if p, pushed := pushOutOfClearing(rng, pos, center, beltClearing+radius); pushed {
			pos, moved = p, true
		}
		if !moved {
			return pos
		}
	}
	pos, _ = pushOutOfClearing(rng, pos, center, beltClearing+radius)
	for {
		pr, ok := worstOverlap(pos, radius, placed)
		if !ok {
			return pos
		}
		gap := (radius + pr.radius) * rockMargin
		pen := gap - dist3(pos, pr.pos)
		pos = stepOutward(rng, pos, center, pen+1)
	}
}

// dist3 is the straight-line distance between two points.
func dist3(a, b Vec3) float64 {
	dx, dy, dz := a.X-b.X, a.Y-b.Y, a.Z-b.Z
	return math.Sqrt(dx*dx + dy*dy + dz*dz)
}

// oreTypeMap maps a variant typeID to its base ore typeID, variantFamily to
// its family name (respawn re-rolls a variant within the family), and
// unitVolumes maps every asteroid typeID, ore and ice, to m³ per unit. Built at init.
var (
	oreTypeMap    map[int64]int64
	variantFamily map[int64]string
	unitVolumes   map[int64]float64
)

func init() {
	oreTypeMap = make(map[int64]int64)
	variantFamily = make(map[int64]string)
	unitVolumes = make(map[int64]float64)
	for name, fam := range OreFamilies {
		baseID := fam.Base.TypeID
		for _, v := range []OreVariant{fam.Base, fam.Concentrated, fam.Dense} {
			oreTypeMap[v.TypeID] = baseID
			variantFamily[v.TypeID] = name
			unitVolumes[v.TypeID] = fam.UnitVolume
		}
	}
	// Every ice type is 1000 m³ per unit.
	for _, tid := range []int64{16262, 16263, 16264, 16265, 16266, 16267, 16268, 16269,
		17975, 17976, 17977, 17978} {
		unitVolumes[tid] = 1000
	}
}

// oreTier classifies an ore family into the size tiers that set a rock's m³
// budget: 0 common (Veldspar, Scordite; the largest rocks), 1 uncommon
// (Pyroxeres, Plagioclase, Omber, Kernite), 2 rare (everything else; smallest).
func oreTier(familyName string) int {
	switch familyName {
	case "Veldspar", "Scordite":
		return 0
	case "Pyroxeres", "Plagioclase", "Omber", "Kernite":
		return 1
	default:
		return 2
	}
}

// beltClearing is the radius of the empty sphere around a belt's warp-in point.
// Rocks are pushed out so their surface, not just their centre, stays clear.
const beltClearing = 10_000.0

// beltScaleParams holds the density knobs for one security class. A belt is a
// packed annulus from beltClearing out to fieldOuter; lowsec and nullsec carry
// more groups, bigger common rocks and tighter clusters.
type beltScaleParams struct {
	anchorBase, anchorRand int     // anchor group count range
	countMult              float64 // satellite count multiplier
	commonSizeMult         float64 // common ore quantity multiplier
	clusterMult            float64 // satellite gap multiplier (<1 = hugs the anchor)
	strayBase, strayRand   int     // stray rock count range
	fieldOuter             float64 // outer edge of the packed field
}

func beltScale(secClass SecurityClass) beltScaleParams {
	switch secClass {
	case SecClassE, SecClassF: // lowsec
		return beltScaleParams{6, 4, 1.8, 1.4, 0.85, 16, 12, 40_000}
	case SecClassG, SecClassH: // nullsec
		return beltScaleParams{8, 5, 2.1, 1.8, 0.7, 22, 14, 46_000}
	default: // hisec
		return beltScaleParams{4, 3, 1.4, 1.0, 1.0, 12, 10, 34_000}
	}
}

// BeltFieldRadius returns how far from a belt's warp-in point its rocks can
// lie: the outer edge of the annulus GenerateAsteroids fills. Anything that
// must reach a miner at work (belt-rat aggro) covers it; two ships in one belt
// can be two radii apart.
func BeltFieldRadius(security float64) float64 {
	return beltScale(SecurityClassFromStatus(security)).fieldOuter
}

// ---------------------------------------------------------------------------
// Belt shapes: each belt gets a random formation instead of a uniform ring.
// ---------------------------------------------------------------------------

// beltShapeKind identifies one of the shape generators that distribute anchor
// positions through the annulus.
type beltShapeKind int

const (
	shapeArc      beltShapeKind = iota // ~120° wedge, a concentrated arc
	shapeCrescent                      // ~180° half-ring
	shapeCluster                       // tight offset group
	shapeSpiral                        // two logarithmic spiral arms
	shapeScatter                       // original uniform ring, just sparser
)

// rollBeltShape picks a random formation for one belt.
func rollBeltShape(rng *rand.Rand) beltShapeKind {
	switch rng.Intn(10) {
	case 0, 1, 2: // 30%
		return shapeArc
	case 3, 4: // 20%
		return shapeCrescent
	case 5, 6: // 20%
		return shapeCluster
	case 7, 8: // 20%
		return shapeSpiral
	default: // 10%
		return shapeScatter
	}
}

// distributeAnchors returns anchor offsets (relative to belt centre, XZ plane)
// for numAnchors groups spread across the annulus according to shape.
func distributeAnchors(rng *rand.Rand, shape beltShapeKind, numAnchors int, inner, outer float64) []Vec3 {
	out := make([]Vec3, 0, numAnchors)

	switch shape {
	case shapeArc:
		// ~120° wedge centred on a random bearing.
		wedge := (2.0 / 3.0) * math.Pi
		base := rng.Float64() * 2 * math.Pi
		for a := 0; a < numAnchors; a++ {
			frac := (float64(a) + rng.Float64()) / float64(numAnchors)
			angle := base - wedge/2 + frac*wedge
			r := annulusRadius(rng, inner, outer)
			out = append(out, Vec3{
				X: r * math.Cos(angle),
				Y: (rng.Float64() - 0.5) * 7_000,
				Z: r * math.Sin(angle),
			})
		}

	case shapeCrescent:
		// ~180° half-ring.
		wedge := math.Pi
		base := rng.Float64() * 2 * math.Pi
		for a := 0; a < numAnchors; a++ {
			frac := (float64(a) + rng.Float64()) / float64(numAnchors)
			angle := base - wedge/2 + frac*wedge
			r := annulusRadius(rng, inner, outer)
			out = append(out, Vec3{
				X: r * math.Cos(angle),
				Y: (rng.Float64() - 0.5) * 7_000,
				Z: r * math.Sin(angle),
			})
		}

	case shapeCluster:
		// Tight group offset from centre. All anchors in a ~12km blob,
		// centred at a random point in the mid-annulus.
		bearing := rng.Float64() * 2 * math.Pi
		cDist := (inner + outer) * 0.45
		cx := cDist * math.Cos(bearing)
		cz := cDist * math.Sin(bearing)
		spread := 12_000.0
		for a := 0; a < numAnchors; a++ {
			theta := rng.Float64() * 2 * math.Pi
			r := rng.Float64() * spread
			out = append(out, Vec3{
				X: cx + r*math.Cos(theta),
				Y: (rng.Float64() - 0.5) * 5_000,
				Z: cz + r*math.Sin(theta),
			})
		}

	case shapeSpiral:
		// Two logarithmic spiral arms winding ~270° from inner to outer.
		armOffset := math.Pi // 180° between the two arms
		totalWind := 1.5 * math.Pi
		base := rng.Float64() * 2 * math.Pi
		for a := 0; a < numAnchors; a++ {
			arm := float64(a % 2)
			t := float64(a) / float64(numAnchors)
			angle := base + arm*armOffset + t*totalWind
			r := inner + t*(outer-inner) + (rng.Float64()-0.5)*4_000
			if r < inner {
				r = inner
			}
			out = append(out, Vec3{
				X: r * math.Cos(angle),
				Y: (rng.Float64() - 0.5) * 7_000,
				Z: r * math.Sin(angle),
			})
		}

	default: // shapeScatter: uniform ring with jitter
		step := (2 * math.Pi) / float64(numAnchors)
		for a := 0; a < numAnchors; a++ {
			angle := float64(a)*step + (rng.Float64()-0.5)*step*0.8
			r := annulusRadius(rng, inner, outer)
			out = append(out, Vec3{
				X: r * math.Cos(angle),
				Y: (rng.Float64() - 0.5) * 7_000,
				Z: r * math.Sin(angle),
			})
		}
	}
	return out
}

// scaledCount returns a scaled loop count: int(base*mult) + rand(int(randN*mult)+1).
func scaledCount(rng *rand.Rand, base, randN int, mult float64) int {
	b := int(float64(base) * mult)
	r := int(float64(randN)*mult) + 1
	return b + rng.Intn(r)
}

// annulusRadius picks a radius in [inner, outer] weighted so rocks spread
// evenly by area instead of bunching against the inner edge.
func annulusRadius(rng *rand.Rand, inner, outer float64) float64 {
	u := rng.Float64()
	return math.Sqrt(inner*inner + u*(outer*outer-inner*inner))
}

// rockVolume returns a random m³ budget in [min,max), scaled by mult.
func rockVolume(rng *rand.Rand, min, max, mult float64) float64 {
	return (min + rng.Float64()*(max-min)) * mult
}

// AsteroidGenVersion is bumped whenever the generator changes shape; a stored
// field rolled under an older version is regenerated. Version 2: ice follows
// the Ice Field celestial type.
const AsteroidGenVersion = 2

const (
	// AsteroidIDBase is where a system's asteroid entity IDs start. Every system
	// reuses the range, so an asteroid entityID is meaningful only paired with
	// its solarSystemID.
	AsteroidIDBase = uint32(900_000_000)

	// AsteroidIDsPerBelt is the block of IDs each belt owns. Rocks are numbered
	// inside their belt's block, so a belt added or removed renumbers no other
	// belt's rocks and persisted depletion survives. See docs/destiny-space.md#belt-layout.
	AsteroidIDsPerBelt = uint32(4096)
)

// IceFieldTypeID is the "Ice Field" celestial type. mapDenormalize carries 861
// of them beside 55,108 plain Asteroid Belts (typeID 15), both groupID 9; the
// typeID is the only thing that says a belt is ice.
const IceFieldTypeID = 17774

// beltCelestial is one groupID-9 celestial: the anchor of one asteroid field.
type beltCelestial struct {
	id  int64
	pos Vec3
	ice bool // an Ice Field celestial, not an Asteroid Belt
}

// beltCelestials extracts the system's asteroid belts sorted by itemID. The
// order decides each belt's ID block, so it must not depend on how the
// celestial rows came back from the database.
func beltCelestials(celBalls []BallDesc, celSlims []SlimItem) []beltCelestial {
	var out []beltCelestial
	for i, slim := range celSlims {
		if slim.GroupID != 9 || i >= len(celBalls) {
			continue
		}
		out = append(out, beltCelestial{id: slim.ItemID, pos: celBalls[i].Pos,
			ice: slim.TypeID == IceFieldTypeID})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].id < out[b].id })
	return out
}

// beltRNG derives one belt's random stream from the system seed and the belt's
// itemID. The multiply by the 64-bit golden-ratio constant spreads the
// clustered belt IDs across the word; XORing raw IDs would give neighbouring
// belts streams that differ in two bits.
func beltRNG(seed, beltID int64) *rand.Rand {
	return rand.New(rand.NewSource(seed ^ int64(uint64(beltID)*0x9E3779B97F4A7C15)))
}

// GenerateAsteroids creates a packed asteroid field around each groupID-9
// celestial under a fresh seed. Ore comes from OreDistribution and
// OreDeposits, ice from IceTypesFor on Ice Field celestials. Layout rules:
// docs/destiny-space.md#belt-layout.
func (ss *SolarSystem) GenerateAsteroids(celBalls []BallDesc, celSlims []SlimItem) {
	ss.GenerateAsteroidsSeeded(celBalls, celSlims, RandomAsteroidSeed())
}

// RandomAsteroidSeed mints a seed for a field nobody has stored one for.
func RandomAsteroidSeed() int64 {
	return rand.New(rand.NewSource(time.Now().UnixNano())).Int63()
}

// GenerateAsteroidsSeeded is GenerateAsteroids with an explicit random stream:
// the same seed over the same belts, security and sovereignty rebuilds the
// identical field, so a field is stored as one number. Each belt draws from its
// own sub-stream (beltRNG) and ID block, so one belt's change leaves the rest intact.
func (ss *SolarSystem) GenerateAsteroidsSeeded(celBalls []BallDesc, celSlims []SlimItem, seed int64) {
	// Assign, never append: reseeding in place depends on a second call replacing
	// the field rather than doubling it.
	ss.AsteroidBalls = nil
	ss.AsteroidSlims = nil
	ss.AsteroidDamage = make(map[uint32]DamageState)
	ss.AsteroidQty = make(map[uint32]int64)
	ss.AsteroidMaxQty = make(map[uint32]int64)
	ss.AsteroidBelt = make(map[uint32]int64)
	// Allocated here, not lazily in MarkAsteroidDepleted: a hydrated system needs
	// it populated before any miner touches it.
	ss.AsteroidDepletedAt = make(map[uint32]time.Time)

	secClass := SecurityClassFromStatus(ss.Security)
	bp := beltScale(secClass)
	dist := OreDistribution(ss.Security, ss.FactionID)
	iceTypes := IceTypesFor(ss.Security, ss.FactionID)
	deposits := EligibleDeposits(ss.Security)

	belts := beltCelestials(celBalls, celSlims)
	for k, belt := range belts {
		beltPos := belt.pos
		beltID := belt.id
		rng := beltRNG(seed, beltID)
		nextID := AsteroidIDBase + uint32(k)*AsteroidIDsPerBelt
		blockEnd := nextID + AsteroidIDsPerBelt

		// An Ice Field celestial is an ice belt; an Asteroid Belt is ore.
		if belt.ice && len(iceTypes) > 0 {
			ss.generateIceBelt(rng, beltID, beltPos, iceTypes, bp, nextID, blockEnd)
			continue
		}

		numAnchors := bp.anchorBase + rng.Intn(bp.anchorRand)

		// Anchors occupy the inner two-thirds of the field so their satellite
		// rings still land inside fieldOuter.
		anchorInner := beltClearing + 3_000
		anchorOuter := beltClearing + (bp.fieldOuter-beltClearing)*0.65

		// Pick a shape for this belt and distribute anchor offsets.
		shape := rollBeltShape(rng)
		anchors := distributeAnchors(rng, shape, numAnchors, anchorInner, anchorOuter)

		// One collision list for the whole belt: with the field this tight,
		// neighbouring clusters overlap each other, not just themselves.
		var placed []placedRock

		// addRock settles a rock of the given m³ budget against the belt and
		// records it.
		addRock := func(pos Vec3, m3 float64, variant OreVariant) {
			q := unitsForVolume(variant.TypeID, m3)
			r := radiusForType(variant.TypeID, q)
			if nextID >= blockEnd {
				return // belt is out of ID block; see AsteroidIDsPerBelt
			}
			pos = settleRock(rng, pos, r, beltPos, placed)
			placed = append(placed, placedRock{pos: pos, radius: r})
			ss.addAsteroid(nextID, beltID, pos, q, variant)
			nextID++
		}

		for a := 0; a < numAnchors; a++ {
			anchorPos := Vec3{
				X: beltPos.X + anchors[a].X,
				Y: beltPos.Y + anchors[a].Y,
				Z: beltPos.Z + anchors[a].Z,
			}

			// A 0.0 anchor group may be a deposit cluster: 2–3 special ores spawned
			// together, drawn from instead of the system-wide table.
			var deposit *OreDeposit
			if len(deposits) > 0 && rng.Intn(100) < depositChancePct {
				d := pickDeposit(rng, deposits)
				deposit = &d
			}

			// pickInCluster returns the ore family a rock in this group takes:
			// a deposit draws uniformly from its own ore list, a plain group
			// overwhelmingly repeats its anchor's family.
			var dominant OreFamily
			pickInCluster := func(chance int) OreVariant {
				if deposit != nil {
					return pickVariant(rng, OreFamilies[deposit.Ores[rng.Intn(len(deposit.Ores))]])
				}
				return pickOreVariantChance(rng, dominant, dist, chance)
			}
			if deposit != nil {
				dominant = OreFamilies[deposit.Ores[rng.Intn(len(deposit.Ores))]]
			} else {
				dominant = pickOreFamily(rng, dist)
			}
			tier := oreTier(dominant.Name)

			// Anchor rock: m³ budget depends on tier, scaled by security.
			var anchorVol float64
			switch {
			case deposit != nil: // deposit anchors are the big prize in 0.0
				anchorVol = rockVolume(rng, 25_000, 60_000, 1)
			case tier == 0: // common: scales with commonSizeMult
				anchorVol = rockVolume(rng, 20_000, 50_000, bp.commonSizeMult)
			case tier == 1: // uncommon
				anchorVol = rockVolume(rng, 15_000, 35_000, 1)
			default: // rare
				anchorVol = rockVolume(rng, 10_000, 25_000, 1)
			}

			anchorVariant := pickInCluster(60)
			anchorQty := unitsForVolume(anchorVariant.TypeID, anchorVol)
			anchorR := radiusForType(anchorVariant.TypeID, anchorQty)
			if nextID >= blockEnd {
				break
			}
			anchorPos = settleRock(rng, anchorPos, anchorR, beltPos, placed)
			placed = append(placed, placedRock{pos: anchorPos, radius: anchorR})
			ss.addAsteroid(nextID, beltID, anchorPos, anchorQty, anchorVariant)
			nextID++

			// placeSatellite drops a rock in a shell hugging the anchor's surface (gap
			// measured from contact, so a huge anchor keeps its escort close), mostly of the anchor's family.
			placeSatellite := func(m3, gapMin, gapRand, ySpread float64) {
				variant := pickInCluster(85)
				q := unitsForVolume(variant.TypeID, m3)
				r := radiusForType(variant.TypeID, q)
				gap := (gapMin + rng.Float64()*gapRand) * bp.clusterMult
				d := (anchorR+r)*rockMargin + gap
				addRock(scatterXZ(rng, anchorPos, d, ySpread), m3, variant)
			}

			// Satellites: count scaled by countMult, gaps by clusterMult.
			switch {
			case deposit != nil: // deposits: a handful of substantial rocks
				for l := 0; l < scaledCount(rng, 2, 2, bp.countMult); l++ {
					placeSatellite(rockVolume(rng, 5_000, 15_000, 1), 400, 1_600, 3_000)
				}
				for m := 0; m < scaledCount(rng, 3, 3, bp.countMult); m++ {
					placeSatellite(rockVolume(rng, 1_500, 6_000, 1), 300, 3_000, 4_000)
				}
				for s := 0; s < scaledCount(rng, 3, 4, bp.countMult); s++ {
					placeSatellite(rockVolume(rng, 300, 1_500, 1), 200, 5_000, 5_000)
				}

			case tier == 0: // common: large + medium + small
				for l := 0; l < scaledCount(rng, 2, 2, bp.countMult); l++ {
					placeSatellite(rockVolume(rng, 8_000, 25_000, bp.commonSizeMult), 400, 1_600, 3_000)
				}
				for m := 0; m < scaledCount(rng, 3, 3, bp.countMult); m++ {
					placeSatellite(rockVolume(rng, 2_500, 8_000, bp.commonSizeMult), 300, 3_000, 4_000)
				}
				for s := 0; s < scaledCount(rng, 5, 4, bp.countMult); s++ {
					placeSatellite(rockVolume(rng, 200, 1_500, 1), 200, 5_000, 5_000)
				}

			case tier == 1: // uncommon: large + medium + small
				for l := 0; l < scaledCount(rng, 2, 2, bp.countMult); l++ {
					placeSatellite(rockVolume(rng, 6_000, 18_000, 1), 400, 1_600, 3_000)
				}
				for m := 0; m < scaledCount(rng, 2, 3, bp.countMult); m++ {
					placeSatellite(rockVolume(rng, 2_000, 6_000, 1), 300, 3_000, 4_000)
				}
				for s := 0; s < scaledCount(rng, 4, 4, bp.countMult); s++ {
					placeSatellite(rockVolume(rng, 200, 1_200, 1), 200, 5_000, 5_000)
				}

			default: // rare: medium + small, no large
				for m := 0; m < scaledCount(rng, 2, 3, bp.countMult); m++ {
					placeSatellite(rockVolume(rng, 1_500, 5_000, 1), 300, 3_000, 4_000)
				}
				for s := 0; s < scaledCount(rng, 3, 4, bp.countMult); s++ {
					placeSatellite(rockVolume(rng, 300, 1_500, 1), 200, 5_000, 5_000)
				}
			}
		}

		// Stray rocks filling the gaps, shaped to match the belt.
		strayCount := bp.strayBase + rng.Intn(bp.strayRand)
		strayOffsets := distributeAnchors(rng, shape, strayCount,
			beltClearing+1_500, bp.fieldOuter)
		for s := 0; s < strayCount; s++ {
			pos := Vec3{
				X: beltPos.X + strayOffsets[s].X,
				Y: beltPos.Y + strayOffsets[s].Y + (rng.Float64()-0.5)*4_000,
				Z: beltPos.Z + strayOffsets[s].Z,
			}
			family := pickOreFamily(rng, dist)
			addRock(pos, rockVolume(rng, 200, 3_000, 1), pickOreVariant(rng, family, dist))
		}
	}
}

// generateIceBelt packs ice formations into the same annulus an ore belt uses.
// Ice is 1000 m³ per unit, so the rocks are huge and the most likely to clip;
// each is settled against its neighbours and the clearing. Reached only for an
// Ice Field celestial where IceTypesFor returned something.
func (ss *SolarSystem) generateIceBelt(rng *rand.Rand, beltID int64, beltPos Vec3,
	types []IceType, bp beltScaleParams, nextID, blockEnd uint32) {
	count := int(float64(5+rng.Intn(5)) * bp.countMult)
	if count < 4 {
		count = 4
	}
	baseStep := (2 * math.Pi) / float64(count)
	var placed []placedRock
	for a := 0; a < count; a++ {
		angle := float64(a)*baseStep + (rng.Float64()-0.5)*baseStep*0.8
		d := annulusRadius(rng, beltClearing+4_000, bp.fieldOuter)
		pos := Vec3{
			X: beltPos.X + d*math.Cos(angle),
			Y: beltPos.Y + (rng.Float64()-0.5)*8_000,
			Z: beltPos.Z + d*math.Sin(angle),
		}
		ice := pickIceType(rng, types)
		qty := unitsForVolume(ice.TypeID, rockVolume(rng, ice.VolMin, ice.VolMax, 1))
		r := radiusForType(ice.TypeID, qty)
		if nextID >= blockEnd {
			return
		}
		pos = settleRock(rng, pos, r, beltPos, placed)
		placed = append(placed, placedRock{pos: pos, radius: r})
		ss.addAsteroid(nextID, beltID, pos, qty, OreVariant{TypeID: ice.TypeID, GroupID: GroupIce})
		nextID++
	}
}

// GetAsteroidQty returns the remaining ore quantity for an asteroid. Thread-safe.
func (ss *SolarSystem) GetAsteroidQty(entityID uint32) int64 {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	return ss.AsteroidQty[entityID]
}

// DecrementAsteroidQty subtracts amount from an asteroid's remaining ore.
// Returns the actual amount decremented (capped at remaining). Thread-safe.
func (ss *SolarSystem) DecrementAsteroidQty(entityID uint32, amount int64) int64 {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	remaining := ss.AsteroidQty[entityID]
	if remaining <= 0 {
		return 0
	}
	if amount > remaining {
		amount = remaining
	}
	ss.AsteroidQty[entityID] = remaining - amount
	ss.markAsteroidDirtyLocked(entityID)
	return amount
}

// UpdateAsteroidRadius recalculates an asteroid's visual radius from its
// quantity and updates AsteroidBalls. changed reports a move past
// asteroidRadiusEpsilon: on-grid clients learn of it only through a SetBallRadius
// push costing a stamp per viewer, so sub-percent shrinks are not pushed. Thread-safe.
func (ss *SolarSystem) UpdateAsteroidRadius(entityID uint32) (radius float64, changed bool) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	qty := ss.AsteroidQty[entityID]
	typeID := int64(0)
	for i := range ss.AsteroidSlims {
		if ss.AsteroidSlims[i].ItemID == int64(entityID) {
			typeID = ss.AsteroidSlims[i].TypeID
			break
		}
	}
	newRadius := radiusForType(typeID, qty)
	for i := range ss.AsteroidBalls {
		if ss.AsteroidBalls[i].EntityID == entityID {
			old := ss.AsteroidBalls[i].Radius
			ss.AsteroidBalls[i].Radius = newRadius
			changed = old > 0 && math.Abs(newRadius-old) > old*asteroidRadiusEpsilon
			break
		}
	}
	return newRadius, changed
}

// asteroidRadiusEpsilon is the relative radius change below which a shrinking
// rock is not re-pushed to clients.
const asteroidRadiusEpsilon = 0.01

// GetAsteroidTypeID returns the typeID for an asteroid entity. Returns 0 if not found. Thread-safe.
func (ss *SolarSystem) GetAsteroidTypeID(entityID uint32) int64 {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	for _, slim := range ss.AsteroidSlims {
		if slim.ItemID == int64(entityID) {
			return slim.TypeID
		}
	}
	return 0
}

// GetAsteroidGroupID returns the groupID for an asteroid entity (462… ore
// groups, 465 ice, 530 Mercoxit). Returns 0 if not found. Thread-safe.
func (ss *SolarSystem) GetAsteroidGroupID(entityID uint32) int64 {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	for _, slim := range ss.AsteroidSlims {
		if slim.ItemID == int64(entityID) {
			return slim.GroupID
		}
	}
	return 0
}

// AsteroidScanEntry is one survey-scanner result row.
type AsteroidScanEntry struct {
	EntityID uint32
	TypeID   int64
	Qty      int64
}

// AsteroidsInRange returns the non-depleted asteroids within rng of pos with
// their typeID and remaining quantity: the survey scanner result set.
// Thread-safe.
func (ss *SolarSystem) AsteroidsInRange(pos Vec3, rng float64) []AsteroidScanEntry {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	rSq := rng * rng
	var out []AsteroidScanEntry
	for i, bd := range ss.AsteroidBalls {
		qty := ss.AsteroidQty[bd.EntityID]
		if qty <= 0 {
			continue
		}
		if pos.Sub(bd.Pos).LenSq() <= rSq {
			out = append(out, AsteroidScanEntry{
				EntityID: bd.EntityID,
				TypeID:   ss.AsteroidSlims[i].TypeID,
				Qty:      qty,
			})
		}
	}
	return out
}

// GetAsteroidPosition returns the position of an asteroid entity. Thread-safe.
func (ss *SolarSystem) GetAsteroidPosition(entityID uint32) (Vec3, bool) {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	for _, bd := range ss.AsteroidBalls {
		if bd.EntityID == entityID {
			return bd.Pos, true
		}
	}
	return Vec3{}, false
}

// NearbyAsteroids returns the subset of system asteroids within BubbleRadius
// of playerPos. Depleted rocks (qty 0, awaiting respawn) are excluded so a
// returning client never sees a ghost. Returns parallel slices of balls/slims
// and a damage map containing only the nearby rocks. Thread-safe.
func (ss *SolarSystem) NearbyAsteroids(playerPos Vec3) ([]BallDesc, []SlimItem, map[uint32]DamageState) {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	rSq := BubbleRadius * BubbleRadius
	var balls []BallDesc
	var slims []SlimItem
	dmg := make(map[uint32]DamageState)
	for i, bd := range ss.AsteroidBalls {
		if ss.AsteroidQty[bd.EntityID] <= 0 {
			continue
		}
		if playerPos.Sub(bd.Pos).LenSq() <= rSq {
			balls = append(balls, bd)
			slims = append(slims, ss.AsteroidSlims[i])
			if d, ok := ss.AsteroidDamage[bd.EntityID]; ok {
				dmg[bd.EntityID] = d
			}
		}
	}
	return balls, slims, dmg
}

// BaseOreType returns the base ore typeID of an asteroid typeID, folding the
// Concentrated/Dense variants onto their base. It classifies a rock by family;
// what lands in a miner's hold is MinedOreType.
func BaseOreType(asteroidTypeID int64) int64 {
	if baseID, ok := oreTypeMap[asteroidTypeID]; ok {
		return baseID
	}
	return asteroidTypeID
}

// MinedOreType returns the typeID that goes into the cargo hold when a rock of
// the given type is mined: the rock's own type, variant included. Each grade is
// its own invTypes row with its own price and invorereprocessing yield.
// See docs/destiny-space.md#mined-ore-variants.
func MinedOreType(asteroidTypeID int64) int64 {
	return asteroidTypeID
}

// MarkAsteroidDepleted records the depletion time for an asteroid. Thread-safe.
func (ss *SolarSystem) MarkAsteroidDepleted(entityID uint32) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.markAsteroidDirtyLocked(entityID)
	if ss.AsteroidDepletedAt == nil {
		ss.AsteroidDepletedAt = make(map[uint32]time.Time)
	}
	ss.AsteroidDepletedAt[entityID] = time.Now()
}

// StartAsteroidRespawnTicker starts the goroutine that checks every 60 s for
// depleted asteroids and respawns them after asteroidRespawn. Only the first
// call starts it.
func (ss *SolarSystem) StartAsteroidRespawnTicker() {
	ss.respawnOnce.Do(func() {
		safego.Forever("destiny.asteroidRespawnLoop", ss.asteroidRespawnLoop)
	})
}

// DefaultAsteroidRespawnDelay is the documented value of the
// asteroids.respawnMinutes knob, not its default: respawn ships off (see
// SolarSystem.asteroidRespawn).
const DefaultAsteroidRespawnDelay = 5 * time.Minute

func (ss *SolarSystem) asteroidRespawnLoop() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	// Respawn is a live mutation on top of the deterministic field, so it draws
	// from its own stream rather than a belt's.
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	for {
		select {
		case <-ticker.C:
			type respawned struct {
				ball BallDesc
				slim SlimItem
				dmg  DamageState
			}
			var events []respawned
			ss.mu.Lock()
			delay := ss.asteroidRespawn
			if delay <= 0 {
				// Respawn disabled: the belt is a resource pool that drains,
				// and its regeneration cycle is what refills it.
				ss.mu.Unlock()
				continue
			}
			now := time.Now()
			for entityID, depletedAt := range ss.AsteroidDepletedAt {
				if now.Sub(depletedAt) < delay {
					continue
				}
				// Restore original quantity.
				qty := ss.AsteroidMaxQty[entityID]
				if qty <= 0 {
					qty = 10_000
				}
				ss.AsteroidQty[entityID] = qty
				delete(ss.AsteroidDepletedAt, entityID)
				ss.markAsteroidDirtyLocked(entityID)

				// Keep the rock's ore family: re-roll only the variant. Ice and unknown types
				// respawn unchanged.
				slimIdx := -1
				for i := range ss.AsteroidSlims {
					if ss.AsteroidSlims[i].ItemID == int64(entityID) {
						slimIdx = i
						break
					}
				}
				if slimIdx < 0 {
					continue
				}
				if famName, ok := variantFamily[ss.AsteroidSlims[slimIdx].TypeID]; ok {
					variant := pickVariant(rng, OreFamilies[famName])
					ss.AsteroidSlims[slimIdx].TypeID = variant.TypeID
					ss.AsteroidSlims[slimIdx].GroupID = variant.GroupID
				}

				// Recalculate radius.
				newRadius := radiusForType(ss.AsteroidSlims[slimIdx].TypeID, qty)
				for i := range ss.AsteroidBalls {
					if ss.AsteroidBalls[i].EntityID == entityID {
						ss.AsteroidBalls[i].Radius = newRadius
						events = append(events, respawned{
							ball: ss.AsteroidBalls[i],
							slim: ss.AsteroidSlims[slimIdx],
							dmg:  ss.AsteroidDamage[entityID],
						})
						break
					}
				}

				log.Printf("[respawn] asteroid %d respawned with typeID=%d qty=%d radius=%.0f in system %d",
					entityID, ss.AsteroidSlims[slimIdx].TypeID, qty, newRadius, ss.ID)
			}
			ss.mu.Unlock()
			// Push AddBalls to on-grid clients outside the lock.
			ss.mu.RLock()
			respawnCB := ss.onAsteroidRespawn
			ss.mu.RUnlock()
			if respawnCB != nil {
				for _, ev := range events {
					respawnCB(ev.ball, ev.slim, ev.dmg)
				}
			}
		case <-ss.stopCh:
			return
		}
	}
}

// ── Shield regeneration ───────────────────────────────────────────────────────

const shieldRegenInterval = 5 * time.Second

// StartShieldRegenTicker starts the goroutine that applies shield regeneration
// to every entity with mutable HP each shieldRegenInterval. Only the first
// call starts it.
func (ss *SolarSystem) StartShieldRegenTicker() {
	ss.shieldRegenOnce.Do(func() {
		safego.Forever("destiny.shieldRegenLoop", ss.shieldRegenLoop)
	})
}

func (ss *SolarSystem) shieldRegenLoop() {
	ticker := time.NewTicker(shieldRegenInterval)
	defer ticker.Stop()
	dt := shieldRegenInterval.Seconds()

	for {
		select {
		case <-ticker.C:
			// Collect entities that need regen. We do the regen outside the
			// system lock since Entity has its own mutex.
			ss.mu.RLock()
			allEntities := make([]*Entity, 0, len(ss.entities)+len(ss.StationEntities)+len(ss.StructureEntities))
			for _, e := range ss.entities {
				allEntities = append(allEntities, e)
			}
			for _, e := range ss.StationEntities {
				allEntities = append(allEntities, e)
			}
			for _, e := range ss.StructureEntities {
				allEntities = append(allEntities, e)
			}
			regenCB := ss.shieldRegenBroadcast
			ss.mu.RUnlock()

			for _, e := range allEntities {
				if e.RegenShield(dt) {
					// Shield changed: broadcast to players.
					if regenCB != nil {
						ds := e.DamageStateFrac()
						regenCB(ss, e.ID, ds)
					}
				}
			}
		case <-ss.stopCh:
			return
		}
	}
}

// ── Server-side tick loop ─────────────────────────────────────────────────────

const tickInterval = 200 * time.Millisecond // 5 Hz

// FlushNodeID is the server's macho node ID, used when the destiny package
// originates packets itself (queued-action flushes from the tick loop).
// Set once at startup by the server entry point before any system ticks.
var FlushNodeID uint64

// StartTickLoop starts a background goroutine that advances server-side
// physics for all moving entities at 5 Hz. Safe to call multiple times.
func (ss *SolarSystem) StartTickLoop() {
	ss.tickOnce.Do(func() {
		safego.Forever("destiny.tickLoop", ss.tickLoop)
		log.Printf("[tick] started tick loop for system %d (%.0fms interval)", ss.ID, tickInterval.Seconds()*1000)
	})
}

func (ss *SolarSystem) tickLoop() {
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	dt := tickInterval.Seconds()

	for {
		select {
		case <-ticker.C:
			// Collect entities with a non-nil Manager under RLock.
			ss.mu.RLock()
			candidates := make([]*Entity, 0, len(ss.entities))
			flushable := make([]*Entity, 0, len(ss.entities))
			for _, e := range ss.entities {
				if e.Manager != nil {
					candidates = append(candidates, e)
				}
				if e.IsPlayer && e.Conn != nil {
					flushable = append(flushable, e)
				}
			}
			ss.mu.RUnlock()

			// Drain queued actions for every connected player, Manager or not: this loop
			// is the only path that delivers them. See docs/destiny-space.md#tick-loop.
			for _, e := range flushable {
				e.FlushQueuedActions(FlushNodeID, false)
			}

			for _, e := range candidates {
				e.mu.Lock()
				if e.Manager == nil || !e.Manager.IsMoving() {
					e.mu.Unlock()
					continue
				}
				e.Manager.Step(dt)
				pos := e.Manager.Pos
				bubID := e.BubbleID
				pinned := e.BubblePinned
				inWarp := e.Manager.CurrentBallMode() == ModeWarp
				e.mu.Unlock()

				// No bubble, mid-warp or pinned (EnsureSameBubble): no transition. A mid-warp
				// one re-inserts the ship into a transit bubble; see docs/destiny-space.md#tick-loop.
				if bubID < 0 || inWarp || pinned {
					continue
				}
				ss.mu.RLock()
				bub := ss.getBubble(bubID)
				stillContained := bub != nil && bub.Contains(pos)
				transitionCB := ss.bubbleTransitionBroadcast
				ss.mu.RUnlock()

				if !stillContained && transitionCB != nil {
					transitionCB(ss, e, bubID)
				}
			}
		case <-ss.stopCh:
			return
		}
	}
}

// StopTickers signals all background goroutines (tick loop, shield regen,
// asteroid respawn) to exit. Safe to call multiple times.
func (ss *SolarSystem) StopTickers() {
	select {
	case <-ss.stopCh:
		// already closed
	default:
		close(ss.stopCh)
		log.Printf("[tick] stopped tickers for system %d", ss.ID)
	}
}

// PlayersAtPoint returns the connected players on the grid at pos without
// joining or minting a bubble: the read-only half of AddToBubble, for a warp
// destination's witnesses while the ship is still in the tunnel. Search order is
// findOrCreateBubble's, so the audience matches the arrival. See docs/destiny-space.md#players-at-point.
func (ss *SolarSystem) PlayersAtPoint(pos Vec3, excludeID uint32) []*Entity {
	ss.mu.RLock()
	defer ss.mu.RUnlock()

	for _, bub := range ss.bubbles {
		if bub.AnchorID != 0 && bub.Contains(pos) {
			return bub.ConnectedPlayerEntitiesExcept(excludeID)
		}
	}
	for _, bub := range ss.bubbles {
		if bub.Contains(pos) {
			return bub.ConnectedPlayerEntitiesExcept(excludeID)
		}
	}
	return nil
}
