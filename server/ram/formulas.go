package ram

import "math"

// CalcProductionTime calculates manufacturing time using pre-Crius formula.
// baseTime * (1 - (prodMod/baseTime) * (PE/(1+PE))) * slotMult * (1 - 0.04*skill) * implant
// productivityModifier comes from invBlueprintTypes; the time waste proportion
// is productivityModifier/productionTime (varies per blueprint, commonly 0.2).
func CalcProductionTime(baseTime, productivityModifier, peLevel int64, industrySkill int) float64 {
	bt := float64(baseTime)
	pe := float64(peLevel)

	// Time waste proportion: productivityModifier / productionTime.
	// Falls back to 0 when baseTime is 0 (shouldn't happen for valid blueprints).
	var timeWasteProportion float64
	if baseTime > 0 {
		timeWasteProportion = float64(productivityModifier) / bt
	}

	// PE modifier: reduces time. Negative PE increases time.
	var peMod float64
	if peLevel >= 0 {
		peMod = 1.0 - (pe/(1.0+pe))*timeWasteProportion
	} else {
		peMod = 1.0 - pe*timeWasteProportion
	}

	// Industry skill: 4% reduction per level.
	skillMod := 1.0 - 0.04*float64(industrySkill)

	result := bt * peMod * skillMod
	if result < 60 {
		result = 60 // minimum 60 seconds
	}
	return result
}

// Industry skills, as InstallJob reads them.
const (
	SkillIndustry   int64 = 3380
	SkillScience    int64 = 3402
	SkillResearch   int64 = 3403
	SkillMetallurgy int64 = 3409
)

// IndustryCharAttrs are the character attributes the blueprint Show Info window
// multiplies into its "(You)" lines (infosvc: 219 manufactureTimeMultiplier, 398
// mineralNeedResearchSpeed, 385 manufacturingTimeResearchSpeed, 387 copySpeedPercent).
// They are the skill terms of the formulas in this file.
func IndustryCharAttrs(level func(skillID int64) int) map[int64]float64 {
	return map[int64]float64{
		219: 1.0 - 0.04*float64(level(SkillIndustry)),
		398: 1.0 - 0.05*float64(level(SkillMetallurgy)),
		385: 1.0 - 0.05*float64(level(SkillResearch)),
		387: 1.0 - 0.05*float64(level(SkillScience)),
	}
}

// CalcMEResearchTime calculates ME research time:
// baseTime * runs * (1 - 0.05*metallurgySkill). Every level takes the same time, so
// currentME is accepted only to keep one signature.
// See docs/db-10-notes.md#blueprint-research-has-no-level-cap.
func CalcMEResearchTime(baseTime int64, currentME int64, runs int, metallurgySkill int) float64 {
	_ = currentME
	total := float64(baseTime) * float64(runs)
	skillMod := 1.0 - 0.05*float64(metallurgySkill)
	return math.Max(total*skillMod, 60)
}

// CalcPEResearchTime calculates PE (productivity/TE) research time.
// baseTime * runs * (1 - 0.05*researchSkill); constant per level, see
// CalcMEResearchTime.
func CalcPEResearchTime(baseTime int64, currentPE int64, runs int, researchSkill int) float64 {
	_ = currentPE
	total := float64(baseTime) * float64(runs)
	skillMod := 1.0 - 0.05*float64(researchSkill)
	return math.Max(total*skillMod, 60)
}

// CalcCopyTime calculates blueprint copying time. The DB baseResearchCopyTime
// covers maxProductionLimit licensed runs, so fewer licensed runs per copy scale it:
//
//	total = baseTime * (licensedRuns / maxLimit) * copies * (1 - 0.05*scienceSkill)
func CalcCopyTime(baseResearchCopyTime int64, copies int, licensedRuns, maxProductionLimit int64, scienceSkill int) float64 {
	bt := float64(baseResearchCopyTime)
	skillMod := 1.0 - 0.05*float64(scienceSkill)

	// Scale by licensed runs requested vs max possible.
	if maxProductionLimit > 0 && licensedRuns > 0 {
		bt = bt * float64(licensedRuns) / float64(maxProductionLimit)
	}

	return math.Max(bt*float64(copies)*skillMod, 60)
}
