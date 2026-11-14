package db

import (
	"context"
	"database/sql"
	"fmt"

	"gurimoon/dogma"
)

// ResolvedHullBonus is one hull bonus with its total percentage resolved
// against the pilot's trained skill level (Pct = per-level % × level, or the
// flat role percentage when the spec has no skill).
type ResolvedHullBonus struct {
	Spec dogma.HullBonusSpec
	Pct  float64
}

// HullBonuses is the full set of resolved hull bonuses for one pilot in one
// hull. The helper methods fold them into multiplicative factors; hull
// bonuses are exempt from stacking penalties, matching EVE.
type HullBonuses []ResolvedHullBonus

// GetShipHullBonuses resolves the hull bonuses of shipTypeID for charID: dgmTypeEffects rows
// select specs from dogma.HullBonusSpecs, each scaled by the pilot's trained skill level. charID
// 0 resolves flat role bonuses only. Returns nil when the hull has no recognized bonus effect.
func GetShipHullBonuses(ctx context.Context, db *sql.DB, charID, shipTypeID int64) (HullBonuses, error) {
	if shipTypeID == 0 {
		return nil, nil
	}
	effects, err := GetTypeEffects(ctx, db, shipTypeID)
	if err != nil {
		return nil, fmt.Errorf("GetShipHullBonuses effects: %w", err)
	}
	var attrs map[int64]float64
	names := make([]string, 0, len(effects))
	for _, te := range effects {
		if _, ok := dogma.HullBonusSpecs[te.EffectName]; !ok {
			continue
		}
		if attrs == nil {
			// Read-only: the hull's own attributes are only sampled here.
			attrs, err = TypeAttrs(ctx, db, shipTypeID)
			if err != nil {
				return nil, fmt.Errorf("GetShipHullBonuses attrs: %w", err)
			}
		}
		names = append(names, te.EffectName)
	}
	if len(names) == 0 {
		return nil, nil
	}
	levels := map[int64]int{} // skillID → trained level cache
	level := func(skillID int64) int {
		if charID == 0 {
			return 0
		}
		lvl, cached := levels[skillID]
		if !cached {
			lvl, _ = GetCharacterSkillLevel(ctx, db, charID, skillID)
			levels[skillID] = lvl
		}
		return lvl
	}
	return ResolveHullBonuses(names, attrs, level), nil
}

// ResolveHullBonuses is the pure half of GetShipHullBonuses: the hull's effect names select
// specs, each reads the first of its source attributes present on the hull, and
// dogma.HullBonusPct turns that into a percentage (per level using level, flat, or a Factor).
// level is consulted only for per-level specs.
func ResolveHullBonuses(effectNames []string, attrs map[int64]float64, level func(skillID int64) int) HullBonuses {
	var out HullBonuses
	for _, name := range effectNames {
		for _, sp := range dogma.HullBonusSpecs[name] {
			var val float64
			for _, a := range sp.Attrs {
				if v, ok := attrs[a]; ok && v != 0 {
					val = v
					break
				}
			}
			lvl := 0
			if sp.SkillID != 0 && !sp.Factor && val != 0 {
				lvl = level(sp.SkillID)
			}
			pct, ok := dogma.HullBonusPct(sp, val, lvl)
			if !ok {
				continue
			}
			out = append(out, ResolvedHullBonus{Spec: sp, Pct: pct})
		}
	}
	return out
}

// mult folds every bonus matching pred into one multiplicative factor.
func (hb HullBonuses) mult(pred func(dogma.HullBonusSpec) bool) float64 {
	m := 1.0
	for _, b := range hb {
		if pred(b.Spec) {
			m *= 1.0 + b.Pct/100.0
		}
	}
	return m
}

func containsInt64(list []int64, v int64) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// HullWeaponMults aggregates every hull bonus that applies to one specific
// weapon. All fields default to 1.0.
type HullWeaponMults struct {
	Dmg      float64    // turret damage (all types)
	PerType  [4]float64 // missile damage per type [em, expl, kin, therm]
	Cycle    float64    // cycle time (turret RoF + launcher RoF)
	Optimal  float64
	Falloff  float64
	Tracking float64
	CapNeed  float64
}

// WeaponMults resolves the hull bonuses applying to a weapon. Turret bonuses
// match when the weapon requires one of the spec's turret skills
// (weaponReqSkills = required-skill typeIDs of the weapon); missile and
// launcher bonuses match on the launcher's groupID (spec Groups nil = any).
func (hb HullBonuses) WeaponMults(weaponReqSkills map[int64]bool, weaponGroupID int64, isMissile bool) HullWeaponMults {
	m := HullWeaponMults{Dmg: 1, PerType: [4]float64{1, 1, 1, 1}, Cycle: 1, Optimal: 1, Falloff: 1, Tracking: 1, CapNeed: 1}
	for _, b := range hb {
		sp := b.Spec
		f := 1.0 + b.Pct/100.0
		switch sp.Target {
		case dogma.HBTurretDamage, dogma.HBTurretROF, dogma.HBTurretTracking,
			dogma.HBTurretOptimal, dogma.HBTurretFalloff, dogma.HBTurretCapNeed:
			if isMissile {
				continue
			}
			matched := false
			for _, s := range sp.WeaponSkills {
				if weaponReqSkills[s] {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
			switch sp.Target {
			case dogma.HBTurretDamage:
				m.Dmg *= f
			case dogma.HBTurretROF:
				m.Cycle *= f
			case dogma.HBTurretTracking:
				m.Tracking *= f
			case dogma.HBTurretOptimal:
				m.Optimal *= f
			case dogma.HBTurretFalloff:
				m.Falloff *= f
			case dogma.HBTurretCapNeed:
				m.CapNeed *= f
			}
		case dogma.HBMissileDamage:
			if !isMissile {
				continue
			}
			if sp.Groups != nil && !containsInt64(sp.Groups, weaponGroupID) {
				continue
			}
			if sp.Dmg == dogma.DTAll {
				for d := 0; d < 4; d++ {
					m.PerType[d] *= f
				}
			} else {
				m.PerType[sp.Dmg] *= f
			}
		case dogma.HBLauncherROF:
			if !isMissile {
				continue
			}
			if sp.Groups != nil && !containsInt64(sp.Groups, weaponGroupID) {
				continue
			}
			m.Cycle *= f
		}
	}
	return m
}

// MissileRangeMults returns the hull's multipliers on a missile's flight velocity and flight
// time for a launcher in launcherGroupID. Range is their product; they stay separate because the
// server chases at velocity and expires on time. Not part of HullWeaponMults, which a module
// copies through a keyed literal. See docs/db-11-notes.md#hull-missile-multipliers.
func (hb HullBonuses) MissileRangeMults(launcherGroupID int64) (velocity, flightTime float64) {
	velocity, flightTime = 1.0, 1.0
	for _, b := range hb {
		sp := b.Spec
		if sp.Target != dogma.HBMissileVelocity && sp.Target != dogma.HBMissileFlightTime {
			continue
		}
		if sp.Groups != nil && !containsInt64(sp.Groups, launcherGroupID) {
			continue
		}
		f := 1.0 + b.Pct/100.0
		if sp.Target == dogma.HBMissileVelocity {
			velocity *= f
		} else {
			flightTime *= f
		}
	}
	return velocity, flightTime
}

// ResistMults returns the resonance multipliers granted by hull resist
// bonuses per layer (0 shield, 1 armor) and damage type [em, expl, kin,
// therm]. Bonus percentages are stored negative, so factors below 1.0 mean
// more resistance.
func (hb HullBonuses) ResistMults() [2][4]float64 {
	out := [2][4]float64{{1, 1, 1, 1}, {1, 1, 1, 1}}
	for _, b := range hb {
		sp := b.Spec
		var layer int
		switch sp.Target {
		case dogma.HBShieldResist:
			layer = 0
		case dogma.HBArmorResist:
			layer = 1
		default:
			continue
		}
		f := 1.0 + b.Pct/100.0
		if sp.Dmg == dogma.DTAll {
			for d := 0; d < 4; d++ {
				out[layer][d] *= f
			}
		} else {
			out[layer][sp.Dmg] *= f
		}
	}
	return out
}

// HullStatMults aggregates the ship-wide stat bonuses. All fields default to
// 1.0; recharge/cap-need fields scale times and costs (below 1.0 = better).
type HullStatMults struct {
	ShieldHP, ArmorHP        float64
	CapCapacity, CapRecharge float64
	Cargo, Velocity          float64
	TargetRange              float64
}

// StatMults folds the ship-wide hull stat bonuses.
func (hb HullBonuses) StatMults() HullStatMults {
	is := func(t dogma.HullBonusTarget) func(dogma.HullBonusSpec) bool {
		return func(s dogma.HullBonusSpec) bool { return s.Target == t }
	}
	return HullStatMults{
		ShieldHP:    hb.mult(is(dogma.HBShieldHP)),
		ArmorHP:     hb.mult(is(dogma.HBArmorHP)),
		CapCapacity: hb.mult(is(dogma.HBCapCapacity)),
		CapRecharge: hb.mult(is(dogma.HBCapRecharge)),
		Cargo:       hb.mult(is(dogma.HBCargo)),
		Velocity:    hb.mult(is(dogma.HBVelocity)),
		TargetRange: hb.mult(is(dogma.HBTargetRange)),
	}
}

// RepairMults returns the multipliers for local repair modules: shield-boost
// amount and cap need, armor-repair amount, cap need and cycle duration.
func (hb HullBonuses) RepairMults() (boostAmount, boostCapNeed, repairAmount, repairCapNeed, repairDuration float64) {
	is := func(t dogma.HullBonusTarget) func(dogma.HullBonusSpec) bool {
		return func(s dogma.HullBonusSpec) bool { return s.Target == t }
	}
	return hb.mult(is(dogma.HBShieldBoostAmount)),
		hb.mult(is(dogma.HBShieldBoostCapNeed)),
		hb.mult(is(dogma.HBArmorRepairAmount)),
		hb.mult(is(dogma.HBArmorRepairCapNeed)),
		hb.mult(is(dogma.HBArmorRepairDuration))
}

// MiningMult returns the hull mining-yield multiplier.
func (hb HullBonuses) MiningMult() float64 {
	return hb.mult(func(s dogma.HullBonusSpec) bool { return s.Target == dogma.HBMiningYield })
}
