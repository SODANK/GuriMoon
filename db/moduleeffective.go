package db

import (
	"context"
	"database/sql"
	"fmt"

	"gurimoon/dogma"
)

// Effective weapon attributes: the numbers a fitted gun, launcher or loaded charge fires with,
// for the client to display.
//
// The client's godma has no modifier engine, so Show Info, the fitting window and the tactical
// overlay's optimal/falloff rings (parklife/tactical.py UpdateTargetingRanges) print the
// attribute values the server sent. This file composes the folds the activation path uses
// (GetWeaponCombatBonuses, HullBonuses.WeaponMults, GetTurretSkillMults, GetTurretTrackingMults,
// MissileRangeMults) plus the charge's own range terms, and bound.Activate reads
// TurretRangeMults from here, so what is shown and what is fired cannot drift.

// Charge-side turret multipliers: weaponRangeMultiplier (Carbonized Lead 1.6, Titanium Sabot
// 0.875) and the Tech II ammo's tracking (244) and falloff (517) terms.
const (
	attrWeaponRangeMultiplier  int64 = 120
	attrChargeTrackingMultiply int64 = 244
	attrChargeFalloffMultiply  int64 = 517
)

// TurretRangeMults are a turret's optimal, falloff, tracking and cap-use multipliers from
// everything but the hull: skills, implants, tracking modules and the loaded charge. Identity is
// all ones.
type TurretRangeMults struct {
	Optimal, Falloff, Tracking, CapNeed float64
}

// chargeTurretMults reads the charge's own range terms (missing = 1).
func chargeTurretMults(charge map[int64]float64) (optimal, falloff, tracking float64) {
	optimal, falloff, tracking = 1, 1, 1
	if v := charge[attrWeaponRangeMultiplier]; v > 0 {
		optimal = v
	}
	if v := charge[attrChargeFalloffMultiply]; v > 0 {
		falloff = v
	}
	if v := charge[attrChargeTrackingMultiply]; v > 0 {
		tracking = v
	}
	return
}

// GetTurretRangeMults resolves TurretRangeMults for one gun. activeIDs are the
// ship's running modules (tracking computers count only while active);
// chargeTypeID 0 is an empty gun. Failures degrade each term to 1, like the
// folds it composes.
func GetTurretRangeMults(ctx context.Context, db *sql.DB, charID, shipID, chargeTypeID int64, activeIDs map[int64]bool) TurretRangeMults {
	ts := GetTurretSkillMults(ctx, db, charID)
	out := TurretRangeMults(ts)
	if db == nil {
		return out
	}
	if rm, tm, err := GetTurretTrackingMults(ctx, db, shipID, activeIDs); err == nil {
		out.Optimal *= rm
		out.Tracking *= tm
	}
	if chargeTypeID != 0 {
		if ca, err := TypeAttrs(ctx, db, chargeTypeID); err == nil {
			o, f, t := chargeTurretMults(ca)
			out.Optimal *= o
			out.Falloff *= f
			out.Tracking *= t
		}
	}
	return out
}

// WeaponFold is every multiplier that moves a weapon's numbers, resolved for
// one module (and charge). EffectiveWeaponAttrsFrom applies it to type
// attributes; GetEffectiveWeaponAttrs resolves it from the database.
type WeaponFold struct {
	IsTurret, IsLauncher bool
	Hull                 HullWeaponMults
	DmgMult, CycleMult   float64 // GetWeaponCombatBonuses (skills, damage mods, implants)
	Range                TurretRangeMults
	MissileVelocity      float64 // MissileRangeMults
	MissileFlightTime    float64
}

// IdentityWeaponFold moves nothing.
func IdentityWeaponFold() WeaponFold {
	return WeaponFold{
		Hull:              HullWeaponMults{Dmg: 1, PerType: [4]float64{1, 1, 1, 1}, Cycle: 1, Optimal: 1, Falloff: 1, Tracking: 1, CapNeed: 1},
		DmgMult:           1,
		CycleMult:         1,
		Range:             TurretRangeMults{Optimal: 1, Falloff: 1, Tracking: 1, CapNeed: 1},
		MissileVelocity:   1,
		MissileFlightTime: 1,
	}
}

// EffectiveWeaponAttrsFrom applies f to a weapon's and its charge's type attributes and returns
// only the attributes it changed (attrID to value), as bound.Activate and weaponCycleLoop fire:
//
//	turret   54 optimal, 158 falloff, 160 tracking = base × Range.X × Hull.X
//	         64 damage = base × DmgMult × Hull.Dmg; 6 cap need = base × Range.CapNeed × Hull.CapNeed
//	both     51 cycle = base × CycleMult × Hull.Cycle
//	launcher 6 cap need = base × Hull.CapNeed
//	missile  37 velocity = base × MissileVelocity; 281 fuse = base × MissileFlightTime
//
// An attribute the type does not carry is never added.
func EffectiveWeaponAttrsFrom(mod, charge map[int64]float64, f WeaponFold) (modOut, chargeOut map[int64]float64) {
	modOut = map[int64]float64{}
	chargeOut = map[int64]float64{}
	set := func(dst, src map[int64]float64, attrID int64, mult float64) {
		if v, ok := src[attrID]; ok && mult > 0 && mult != 1 {
			dst[attrID] = v * mult
		}
	}
	if !f.IsTurret && !f.IsLauncher {
		return
	}
	set(modOut, mod, dogma.AttrSpeed, f.CycleMult*f.Hull.Cycle)
	if f.IsTurret {
		set(modOut, mod, dogma.AttrMaxRange, f.Range.Optimal*f.Hull.Optimal)
		set(modOut, mod, dogma.AttrFalloff, f.Range.Falloff*f.Hull.Falloff)
		set(modOut, mod, dogma.AttrTrackingSpeed, f.Range.Tracking*f.Hull.Tracking)
		set(modOut, mod, dogma.AttrDamageMultiplier, f.DmgMult*f.Hull.Dmg)
		set(modOut, mod, dogma.AttrCapacitorNeed, f.Range.CapNeed*f.Hull.CapNeed)
		return
	}
	set(modOut, mod, dogma.AttrCapacitorNeed, f.Hull.CapNeed)
	set(chargeOut, charge, dogma.AttrMaxVelocity, f.MissileVelocity)
	set(chargeOut, charge, dogma.AttrExplosionDelay, f.MissileFlightTime)
	return
}

// weaponKind reports whether typeID is a turret (effect 42) or a launcher
// (effect 40).
func weaponKind(ctx context.Context, db *sql.DB, typeID int64) (turret, launcher bool, err error) {
	rows, err := db.QueryContext(ctx,
		`SELECT effectID FROM dgmTypeEffects WHERE typeID = ? AND effectID IN (?, ?)`,
		typeID, dogma.EffectTurretFitted, dogma.EffectLauncherFitted)
	if err != nil {
		return false, false, fmt.Errorf("weaponKind: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var e int64
		if rows.Scan(&e) == nil {
			turret = turret || e == dogma.EffectTurretFitted
			launcher = launcher || e == dogma.EffectLauncherFitted
		}
	}
	return turret, launcher, rows.Err()
}

// GetWeaponFold resolves the fold for one fitted weapon. ok=false for a module that is neither a
// turret nor a launcher.
func GetWeaponFold(ctx context.Context, db *sql.DB, charID, shipID, shipTypeID, modTypeID, chargeTypeID int64, activeIDs map[int64]bool) (f WeaponFold, ok bool, err error) {
	f = IdentityWeaponFold()
	if db == nil || modTypeID == 0 {
		return f, false, nil
	}
	f.IsTurret, f.IsLauncher, err = weaponKind(ctx, db, modTypeID)
	if err != nil || (!f.IsTurret && !f.IsLauncher) {
		return f, false, err
	}
	groupID, _, _ := GetTypeGroupCategory(ctx, db, modTypeID)
	if charID != 0 {
		if d, c, berr := GetWeaponCombatBonuses(ctx, db, charID, shipID, modTypeID, groupID, chargeTypeID, f.IsTurret); berr == nil {
			f.DmgMult, f.CycleMult = d, c
		}
	}
	if shipTypeID != 0 {
		if hb, herr := GetShipHullBonuses(ctx, db, charID, shipTypeID); herr == nil && len(hb) > 0 {
			req := map[int64]bool{}
			if reqs, rerr := GetRequiredSkills(ctx, db, modTypeID); rerr == nil {
				for _, r := range reqs {
					req[r.SkillID] = true
				}
			}
			f.Hull = hb.WeaponMults(req, groupID, f.IsLauncher)
		}
	}
	if f.IsTurret {
		f.Range = GetTurretRangeMults(ctx, db, charID, shipID, chargeTypeID, activeIDs)
	} else {
		f.MissileVelocity, f.MissileFlightTime = MissileRangeMults(ctx, db, charID, shipTypeID, groupID)
	}
	return f, true, nil
}

// GetEffectiveWeaponAttrs resolves the changed attributes of one fitted weapon and its loaded
// charge (see EffectiveWeaponAttrsFrom). Both maps are empty for a non-weapon or on any failure,
// and the raw type values stand.
func GetEffectiveWeaponAttrs(ctx context.Context, db *sql.DB, charID, shipID, shipTypeID, modTypeID, chargeTypeID int64, activeIDs map[int64]bool) (modOut, chargeOut map[int64]float64) {
	f, ok, err := GetWeaponFold(ctx, db, charID, shipID, shipTypeID, modTypeID, chargeTypeID, activeIDs)
	if err != nil || !ok {
		return map[int64]float64{}, map[int64]float64{}
	}
	mod, err := TypeAttrs(ctx, db, modTypeID)
	if err != nil {
		return map[int64]float64{}, map[int64]float64{}
	}
	var charge map[int64]float64
	if chargeTypeID != 0 {
		charge, _ = TypeAttrs(ctx, db, chargeTypeID)
	}
	return EffectiveWeaponAttrsFrom(mod, charge, f)
}
