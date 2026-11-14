package ram

import evedb "gurimoon/db"

// Skill typeIDs governing concurrent industry job limits (RMR-era IDs).
const (
	SkillMassProduction         = 3387
	SkillAdvancedMassProduction = 24625
	SkillLaboratoryOperation    = 3406
	SkillAdvancedLabOperation   = 24624
)

// MaxConcurrentJobs returns the job cap for one activity class:
// 1 base job plus one per level of the basic and advanced skill.
func MaxConcurrentJobs(skillLevel, advancedSkillLevel int) int {
	return 1 + skillLevel + advancedSkillLevel
}

// AdjustCostForStanding applies the assembly line's standing-based discount or
// surcharge (both expressed in percent per standing point) to a cost.
// Positive standing discounts, negative standing surcharges; the result is
// never negative.
func AdjustCostForStanding(cost, standing, discountPerGoodPoint, surchargePerBadPoint float64) float64 {
	return evedb.AdjustCostForStanding(cost, standing, discountPerGoodPoint, surchargePerBadPoint)
}

// Assembly line restrictionMask bits (eveConstants ramRestrictBy*).
const (
	RestrictBySecurity = 1
	RestrictByStanding = 2
	RestrictByCorp     = 4
	RestrictByAlliance = 8
)
