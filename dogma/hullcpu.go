package dogma

// Hull role bonuses on module CPU need: the reductions that let a hull online a module no
// other ship can power (a command ship's Warfare Link, a barge's strip miner, a covert ops
// cloak). HullCPUNeedSpecs encodes per ship effect which hull attribute holds the bonus and
// which modules it reaches. Two shapes exist:
//
//   - a percentage on the hull (cpuNeedBonus 310 = -99, shipBonusORE1 773 = -99.5);
//   - the cloak multiplier: attr 649 is the base (0.05), lowered by the spec's attribute
//     (0.01) for each level of LevelSkill and floored at 0.
//
// See docs/fitting-notes.md#hull-cpu-role-bonuses.

// Skills and groups the CPU specs select modules by.
const (
	skillLeadership             int64 = 3348
	skillCloaking               int64 = 11579
	skillTacticalWeaponReconfig int64 = 22043 // Siege Module
	skillAdvDroneInterfacing    int64 = 24613 // Drone Control Unit
	skillCloningFacilityOp      int64 = 24606 // Clone Vat Bay
	groupStripMiner             int64 = 464
	groupSiegeModule            int64 = 515
	groupInterdictionSphere     int64 = 589
	groupJumpPortalGenerator    int64 = 590
	attrHullCPUNeedBonus        int64 = 310 // cpuNeedBonus on a hull, percent
	attrCloakingCPUNeedBonus    int64 = 649 // cloakingCpuNeedBonus, a multiplier
)

// HullCPUNeedSpec is one hull bonus on module CPU need, carried by a ship effect.
type HullCPUNeedSpec struct {
	// Attr is the hull attribute read: a percentage, or the per-level step when LevelSkill is set.
	Attr int64
	// The module must require one of Skills and sit in one of Groups; nil matches any.
	Skills []int64
	Groups []int64
	// LevelSkill marks a cloak step: Attr times its trained level comes off attr 649.
	LevelSkill int64
}

// matches reports whether the spec reaches a module with these direct required skills and group.
func (sp HullCPUNeedSpec) matches(reqs map[int64]bool, groupID int64) bool {
	if sp.Skills != nil {
		hit := false
		for _, s := range sp.Skills {
			if reqs[s] {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	if sp.Groups != nil {
		hit := false
		for _, g := range sp.Groups {
			if g == groupID {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	return true
}

// HullCPUNeedSpecs maps dgmEffects.effectName to the CPU-need bonus it grants. Left out: the
// titan superweapon effects (the weapon pays through cpuBonusSuperWeapon, SuperWeaponCPUUsage)
// and the two escort-frigate effects, whose module filter the data does not give.
var HullCPUNeedSpecs = map[string][]HullCPUNeedSpec{
	// Warfare Links are the only modules that require Leadership.
	"leadershipCpuBonus": {{Attr: attrHullCPUNeedBonus, Skills: []int64{skillLeadership}}},
	// Mining alone would reach every mining laser, so the strip miner group narrows it.
	"shipMinerCPUNeed":                       {{Attr: abORE1, Skills: []int64{SkillMining}, Groups: []int64{groupStripMiner}}},
	"shipIceHarvesterCPUNeed":                {{Attr: abORE1, Skills: []int64{SkillIceHarvesting}}},
	"dreadnoughtSiegeModeCpuNeedBonus":       {{Attr: attrHullCPUNeedBonus, Skills: []int64{skillTacticalWeaponReconfig}, Groups: []int64{groupSiegeModule}}},
	"droneControlUnitCpuNeedBonus":           {{Attr: attrHullCPUNeedBonus, Skills: []int64{skillAdvDroneInterfacing}}},
	"titanJumpPortalCpuNeedBonus":            {{Attr: attrHullCPUNeedBonus, Groups: []int64{groupJumpPortalGenerator}}},
	"cloneVatCpuNeedBonus":                   {{Attr: attrHullCPUNeedBonus, Skills: []int64{skillCloningFacilityOp}}},
	"interdictorBubbleProbeLauncherCpuBonus": {{Attr: attrHullCPUNeedBonus, Groups: []int64{groupInterdictionSphere}}},
	// Cloaking selects the cloaks out of group 330, which also holds the Clone Vat Bay and the
	// Cynosural Field Generator.
	"covertOpsCpuBonus1":      {{Attr: abEliteCoverOps1, Skills: []int64{skillCloaking}, LevelSkill: SkillCovertOps}},
	"reconShipCpuMultiplier1": {{Attr: abEliteReconShip1, Skills: []int64{skillCloaking}, LevelSkill: SkillReconShips}},
}

// HullCPUNeed is one hull's CPU-need bonuses for one pilot. A nil *HullCPUNeed changes nothing.
type HullCPUNeed struct {
	// Effects are the hull's effect names; names without a spec are ignored.
	Effects []string
	// Attrs are the hull's own attributes, read-only.
	Attrs map[int64]float64
	// Level reports the pilot's trained level of a skill; nil counts every level as 0.
	Level func(skillID int64) int
}

// Mult is the multiplier on the CPU need of a module with these direct required skills and
// group when fitted to this hull. It is 1 for a module no bonus reaches.
func (h *HullCPUNeed) Mult(reqs map[int64]bool, groupID int64) float64 {
	if h == nil {
		return 1
	}
	m := 1.0
	cloak, cloakStep := false, 0.0
	for _, name := range h.Effects {
		for _, sp := range HullCPUNeedSpecs[name] {
			if !sp.matches(reqs, groupID) {
				continue
			}
			v := h.Attrs[sp.Attr]
			if sp.LevelSkill != 0 {
				cloak = true
				if h.Level != nil {
					cloakStep += v * float64(h.Level(sp.LevelSkill))
				}
				continue
			}
			m *= 1 + v/100
		}
	}
	if base := h.Attrs[attrCloakingCPUNeedBonus]; cloak && base > 0 {
		c := base - cloakStep
		if c < 0 {
			c = 0
		}
		m *= c
	}
	if m < 0 {
		m = 0
	}
	return m
}
