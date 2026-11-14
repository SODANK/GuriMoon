package npc

import (
	"strings"

	"gurimoon/db"
)

// NPC behaviour is two inputs resolved once at spawn: Role, read from the hull's
// type (weapons, electronic modules, mobility), and Stance, the one per-ball
// choice an author makes. Hostility is the standings layer, ranges come from
// the weapon attributes (deriveRanges), and electronic warfare is whatever the
// type carries. See docs/npc-behaviour.md#role-and-stance.

// Stance is how willing an NPC is to fight. It is the only behaviour an author
// sets per ball (dunObjects.stance); spawners, sentries and CONCORD pick theirs
// in code.
type Stance string

const (
	// StanceAggressive engages any hostile it sees. The default.
	StanceAggressive Stance = "aggressive"
	// StanceDefensive engages only what is on its threat table: whoever shot it,
	// or whoever group aggro seeded onto it. Station guns and CONCORD.
	StanceDefensive Stance = "defensive"
	// StancePassive never engages, even when shot: scenery, scripted ships,
	// escorts that exist to be looked at or killed.
	StancePassive Stance = "passive"
)

// Stances lists every stance in display order, default first.
func Stances() []Stance {
	return []Stance{StanceAggressive, StanceDefensive, StancePassive}
}

// ParseStance reads a stored or submitted stance. Empty and unknown values read
// as aggressive.
func ParseStance(s string) Stance {
	switch Stance(strings.ToLower(strings.TrimSpace(s))) {
	case StanceDefensive:
		return StanceDefensive
	case StancePassive:
		return StancePassive
	}
	return StanceAggressive
}

// ValidStance reports whether s names a stance exactly (case-insensitive). The
// ESP write paths use it to refuse a typo rather than store one that would
// silently read back as aggressive.
func ValidStance(s string) bool {
	switch Stance(strings.ToLower(strings.TrimSpace(s))) {
	case StanceAggressive, StanceDefensive, StancePassive:
		return true
	}
	return false
}

// Role is what a hull can do. Derived at spawn (SpawnNPC), never stored.
type Role int

const (
	// RoleFighter can hurt and can move: closes to weapon range, orbits, fires.
	RoleFighter Role = iota
	// RoleTurret can hurt and cannot move: holds and fires at what comes in
	// range. Sentry guns and armed dungeon structures.
	RoleTurret
	// RoleRunner cannot hurt, can move, and its spawner lets it leave: runs
	// from whatever threatens it and warps off the grid.
	RoleRunner
	// RoleInert cannot hurt: holds (or flies its leash patrol) and never locks.
	RoleInert
)

func (r Role) String() string {
	switch r {
	case RoleFighter:
		return "fighter"
	case RoleTurret:
		return "turret"
	case RoleRunner:
		return "runner"
	}
	return "inert"
}

// DeriveRole classifies a hull. armed counts electronic modules as well as
// guns. mayFlee is the spawner's call, not the hull's: an unarmed dungeon ship
// must hold its ground or a killAll objective could leave the grid.
func DeriveRole(armed, mobile, mayFlee bool) Role {
	switch {
	case armed && mobile:
		return RoleFighter
	case armed:
		return RoleTurret
	case mobile && mayFlee:
		return RoleRunner
	}
	return RoleInert
}

// IsUnarmed reports whether a weapon profile can hurt anything: a nil profile,
// or no missile type and zero damage on every channel. GetNPCWeaponProfile
// fills in cycle time and ranges for any type, so a nil check alone is not
// enough. See docs/npc-behaviour.md#role-and-stance.
func IsUnarmed(wp *db.NPCWeaponProfile) bool {
	if wp == nil {
		return true
	}
	if wp.MissileTypeID != 0 {
		return false
	}
	return wp.Em+wp.Explosive+wp.Kinetic+wp.Thermal <= 0
}

// Behavior is the resolved stance of one NPC: its role and stance, plus the
// four settings the brain reads off them. Built only by NewBehavior.
type Behavior struct {
	Role   Role
	Stance Stance

	MovementMode     string // "orbit", "hold", "flee"
	AutoAggro        bool   // notices hostiles without being shot first
	TargetPreference string // "nearestPlayer", "highestThreat", "none"
	MaxTargets       int    // 0 = never locks
}

// NewBehavior resolves a role and stance into what the brain reads. Passive
// wins over every role. An inert hull locks nothing whatever its stance. A
// runner notices hostiles (AutoAggro) so it has something to run from, and
// never locks.
func NewBehavior(role Role, stance Stance) *Behavior {
	b := &Behavior{Role: role, Stance: stance}
	switch {
	case stance == StancePassive || role == RoleInert:
		b.MovementMode, b.TargetPreference = "hold", "none"
	case role == RoleRunner:
		b.MovementMode, b.AutoAggro, b.TargetPreference = "flee", true, "nearestPlayer"
	default:
		b.MovementMode, b.MaxTargets = "orbit", 1
		if role == RoleTurret {
			b.MovementMode = "hold"
		}
		if stance == StanceDefensive {
			b.TargetPreference = "highestThreat"
		} else {
			b.AutoAggro, b.TargetPreference = true, "nearestPlayer"
		}
	}
	return b
}

// String names the behaviour for logs: "fighter/aggressive".
func (b *Behavior) String() string {
	if b == nil {
		return "<nil>"
	}
	return b.Role.String() + "/" + string(b.Stance)
}
