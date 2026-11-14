package db

import (
	"context"
	"database/sql"
	"fmt"

	"gurimoon/dogma"
)

// Module fitting need: the CPU and powergrid a module costs once the pilot's fitting
// skills are folded in. Each rule is one skill's dgmEffect ("...NeedBonusPostPercent...
// ModulesRequiring<Skill>"): the per-level percentage is read off the skill type (310
// cpuNeedBonus, 323 powerNeedBonus) and applies to a module that directly requires one
// of the listed skills; walking the prerequisite chain would not change who matches,
// because most turrets list Gunnery themselves. The Advanced Energy Grid, Shield and
// Sensor Upgrades skills ship with a 0 bonus and are left out. The pilot's fitting
// hardwirings (ImplantMults.FittingNeed) select modules the same way.

const (
	skillEnergyPulseWeapons     int64 = 3421
	skillWeaponUpgrades         int64 = 3318
	skillAdvancedWeaponUpgrades int64 = 11207
	skillEnergyGridUpgrades     int64 = 3424
	skillShieldUpgrades         int64 = 3425
	skillElectronicsUpgrades    int64 = 3432

	attrCPUNeedBonus   int64 = 310
	attrPowerNeedBonus int64 = 323
)

// moduleNeedRule is one fitting skill: the attribute carrying its per-level bonus, the
// need it lowers, and the required skills that select a module.
type moduleNeedRule struct {
	skill, bonusAttr int64
	cpu              bool // false = powergrid
	requires         []int64
}

var moduleNeedRules = []moduleNeedRule{
	{skillWeaponUpgrades, attrCPUNeedBonus, true, []int64{dogma.SkillGunnery, skillEnergyPulseWeapons, dogma.SkillMissileLauncherOp}},
	{skillAdvancedWeaponUpgrades, attrPowerNeedBonus, false, []int64{dogma.SkillGunnery, dogma.SkillMissileLauncherOp}},
	{skillEnergyGridUpgrades, attrCPUNeedBonus, true, []int64{skillEnergyGridUpgrades}},
	{skillElectronicsUpgrades, attrCPUNeedBonus, true, []int64{skillElectronicsUpgrades}},
	{skillShieldUpgrades, attrPowerNeedBonus, false, []int64{skillShieldUpgrades}},
}

// requiredSkillAttrs are requiredSkill1..3 (182-184); 1285/1289/1290 are requiredSkill4..6
// and harmless when absent.
var requiredSkillAttrs = []int64{182, 183, 184, 1285, 1289, 1290}

// directRequiredSkills reads a type's requiredSkill1..6 from its attributes.
func directRequiredSkills(attrs map[int64]float64) map[int64]bool {
	out := map[int64]bool{}
	for _, a := range requiredSkillAttrs {
		if s := int64(attrs[a]); s > 0 {
			out[s] = true
		}
	}
	return out
}

// ModuleNeedMultsFrom is the pure fold: the multipliers on one module's CPU and
// powergrid need. levels reports a trained skill level, skillAttrs returns a skill
// type's attributes read-only (nil to skip), reqs are the module's direct required skills.
func ModuleNeedMultsFrom(levels func(skillID int64) int, skillAttrs func(typeID int64) map[int64]float64, reqs map[int64]bool) (cpu, pg float64) {
	cpu, pg = 1, 1
	for _, r := range moduleNeedRules {
		hit := false
		for _, s := range r.requires {
			if reqs[s] {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		lvl := levels(r.skill)
		if lvl <= 0 {
			continue
		}
		a := skillAttrs(r.skill)
		if a == nil {
			continue
		}
		b := a[r.bonusAttr]
		if b == 0 {
			continue
		}
		if r.cpu {
			cpu *= 1 + b/100*float64(lvl)
		} else {
			pg *= 1 + b/100*float64(lvl)
		}
	}
	return cpu, pg
}

// ModuleFittingNeedFrom is a module's effective CPU and powergrid need: its raw
// attributes (superweapon CPU factor included, dogma.SuperWeaponCPUUsage) times
// ModuleNeedMultsFrom and the pilot's fitting implants, and for CPU times the hull's role
// bonus on a module of groupID. A nil hull is a module priced off any ship; a zero im is
// a pilot with no implants.
func ModuleFittingNeedFrom(modAttrs map[int64]float64, groupID int64, levels func(int64) int, skillAttrs func(int64) map[int64]float64, hull *dogma.HullCPUNeed, im ImplantMults) (cpu, pg float64) {
	reqs := directRequiredSkills(modAttrs)
	cm, pm := ModuleNeedMultsFrom(levels, skillAttrs, reqs)
	ic, ip := im.FittingNeed(reqs)
	return dogma.SuperWeaponCPUUsage(modAttrs) * cm * ic * hull.Mult(reqs, groupID), modAttrs[dogma.AttrPowerConsumption] * pm * ip
}

// needSkillReaders returns the two lookups the folds take for one pilot. A pilot ID of
// 0 or a failed read leaves every level at 0, the raw need.
func needSkillReaders(ctx context.Context, db *sql.DB, charID int64) (func(int64) int, func(int64) map[int64]float64) {
	var lv map[int64]int
	if charID != 0 {
		lv, _ = GetCharacterSkillLevels(ctx, db, charID)
	}
	levels := func(s int64) int { return lv[s] }
	attrs := func(t int64) map[int64]float64 {
		a, err := TypeAttrs(ctx, db, t)
		if err != nil {
			return nil
		}
		return a
	}
	return levels, attrs
}

// GetModuleFittingNeed is ModuleFittingNeedFrom for one module type and pilot on a hull of
// shipTypeID (0 for no hull).
func GetModuleFittingNeed(ctx context.Context, db *sql.DB, charID, shipTypeID, modTypeID int64) (cpu, pg float64, err error) {
	modAttrs, err := TypeAttrs(ctx, db, modTypeID)
	if err != nil {
		return 0, 0, fmt.Errorf("GetModuleFittingNeed: %w", err)
	}
	levels, attrs := needSkillReaders(ctx, db, charID)
	hull := GetHullCPUNeed(ctx, db, charID, shipTypeID)
	var groupID int64
	if hull != nil {
		groupID = typeGroupID(ctx, db, modTypeID)
	}
	cpu, pg = ModuleFittingNeedFrom(modAttrs, groupID, levels, attrs, hull, GetCharImplantMults(ctx, db, charID))
	return cpu, pg, nil
}

// ModuleNeedMultsResolver reads one pilot's skills, implants and the role bonuses of the hull
// of shipTypeID once, and returns a lookup of the CPU and powergrid multipliers for any module
// type, to show a module's own need (attrs 50/30) beside the GetShipCPUPowerLoad total. The
// superweapon CPU factor is not included: the client shows the cpu attribute as authored and
// only the load carries it.
func ModuleNeedMultsResolver(ctx context.Context, db *sql.DB, charID, shipTypeID int64) func(modTypeID int64) (cpu, pg float64) {
	levels, skillAttrs := needSkillReaders(ctx, db, charID)
	hull := GetHullCPUNeed(ctx, db, charID, shipTypeID)
	im := GetCharImplantMults(ctx, db, charID)
	return func(modTypeID int64) (float64, float64) {
		a, err := TypeAttrs(ctx, db, modTypeID)
		if err != nil {
			return 1, 1
		}
		reqs := directRequiredSkills(a)
		cm, pm := ModuleNeedMultsFrom(levels, skillAttrs, reqs)
		ic, ip := im.FittingNeed(reqs)
		cm *= ic
		pm *= ip
		if hull != nil {
			cm *= hull.Mult(reqs, typeGroupID(ctx, db, modTypeID))
		}
		return cm, pm
	}
}

// ApplyModuleNeedMults scales a module attribute map's cpu (50) and power (30) in
// place, leaving absent attributes absent.
func ApplyModuleNeedMults(attrs map[int64]float64, cpuMult, pgMult float64) {
	if v, ok := attrs[dogma.AttrCPUUsage]; ok && cpuMult != 1 {
		attrs[dogma.AttrCPUUsage] = v * cpuMult
	}
	if v, ok := attrs[dogma.AttrPowerConsumption]; ok && pgMult != 1 {
		attrs[dogma.AttrPowerConsumption] = v * pgMult
	}
}
