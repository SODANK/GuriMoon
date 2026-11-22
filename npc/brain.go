package npc

import (
	"gurimoon/destiny"
	"time"
)

// Brain is the pluggable AI interface for NPCs. Each NPC entity has a Brain
// that is called every tick to decide what the NPC should do next.
type Brain interface {
	// Think is called every tick. Returns a CombatAction with movement and combat commands.
	Think(view WorldView) CombatAction
}

// CombatAction is the combined result of Brain.Think: movement and combat
// commands.
type CombatAction struct {
	Movement Action      // movement command (Follow, Orbit, Goto, Stop, Idle)
	Combat   []CombatCmd // lock/unlock/fire commands
}

// CombatCmdType enumerates combat commands an NPC can issue.
type CombatCmdType int

const (
	CmdLockTarget   CombatCmdType = iota // start locking a target
	CmdUnlockTarget                      // unlock a target
	CmdFireWeapons                       // fire weapons at current target
	// CmdDisengage drops the lock (or lock in progress) and threat on a target the NPC has
	// given up at its leash, and ignores that target for npc.leashForgetSec.
	CmdDisengage
)

// CombatCmd is a single combat command from a Brain.
type CombatCmd struct {
	Type     CombatCmdType
	TargetID uint32
}

// WorldView is the snapshot of the world passed to Brain.Think each tick.
type WorldView struct {
	Self          EntityState   // NPC's own position, velocity, nav state
	Nearby        []EntityState // Other entities in the bubble
	TickDelta     time.Duration // Time since last tick
	SelfHP        HPView        // own HP fractions
	Weapons       *WeaponView   // weapon state (range, ready, cycle)
	Targets       []uint32      // currently locked targets
	Locking       uint32        // target being locked (0 = none)
	ThreatTable   []ThreatEntry // who's attacking us
	AggrRange       float64       // aggression range from config
	SpawnPos        destiny.Vec3  // original spawn position for leash logic
	SelfFactionID   int64         // NPC's own faction (0 = no faction)
	CurrentTargetID uint32        // movement target from last tick (0 = none)
	LastMovementType ActionType   // last applied movement action type (for hysteresis)
	MoveSwitchAge    time.Duration // time since LastMovementType last changed (dwell gate)
	LeashPos         *destiny.Vec3 // idle orbit anchor (nil = idle in place)
	LeashOrbitDist   float64       // idle orbit radius around LeashPos (metres)
	// UnarmedFlyRange is the authored orbit range (attr 416) of an NPC with no weapon profile,
	// which has no WeaponView to carry it. 0 = the 5 km default.
	UnarmedFlyRange float64
}

// HPView holds fractional HP values for the NPC.
type HPView struct {
	ShieldFrac    float64
	ArmorFrac     float64
	StructureFrac float64
}

// WeaponView holds weapon state information for Brain decision-making.
type WeaponView struct {
	OptimalRange   float64 // optimal engagement range (metres)
	FalloffRange   float64 // falloff range (metres)
	FlyRange       float64 // preferred orbit range (attr 416 entityFlyRange)
	ChaseMaxDist   float64 // give-up distance (attr 665 entityChaseMaxDistance)
	EffectiveRange float64 // max firing range: turrets = entityAttackRange (attr 247), missiles = vel*flightTime
	IsReady        bool    // cooldown elapsed, can fire
	DmgPerCycle    float64 // total damage per cycle
	IsMissile      bool    // true if this NPC uses missiles (MissileTypeID != 0)
}

// ThreatEntry tracks cumulative damage from one attacker.
type ThreatEntry struct {
	EntityID    uint32
	TotalDamage float64
}

// EntityState describes a single entity visible to the NPC.
type EntityState struct {
	ID        uint32
	Pos       destiny.Vec3
	IsPlayer  bool
	TypeID    int64
	Distance  float64 // precomputed distance from NPC (metres)
	CorpID    int64   // owning corp
	FactionID int64   // owning faction (0 = no faction)
	IsHostile bool    // pre-computed: should this NPC consider this entity hostile?
}

// ActionType enumerates what an NPC can do for movement.
type ActionType int

const (
	ActionIdle   ActionType = iota // do nothing
	ActionFollow                   // chase TargetID at Distance
	ActionOrbit                    // orbit TargetID at Distance
	ActionGoto                     // fly to Pos
	ActionStop                     // decelerate to rest
	ActionWarp                     // warp to Pos (WarpDest)
	ActionWarpEnd // warp complete: snap to WarpDest and stop
	ActionFlee                     // run away: align off TargetID, warp out, despawn
)

// Action is a movement command for the NPC manager.
type Action struct {
	Type     ActionType
	TargetID uint32       // for Follow/Orbit
	Distance float64      // for Follow/Orbit range (metres)
	Pos      destiny.Vec3 // for Goto
	WarpDest destiny.Vec3 // for ActionWarp
	// Idle marks a leash-patrol waypoint (leashIdleAction): movement with no target
	// behind it, which the broadcaster may drop for a witness whose stream is ahead
	// of the wall. Not compared by applyAction's dedup.
	Idle bool
}
