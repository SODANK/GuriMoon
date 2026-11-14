package db

// Research Facility slots: the pure rules, apart from the SQL so they test without a
// database. See researchslotdb.go and docs/research-facility.md.

import "math"

const (
	// ResearchSlotFirst..ResearchSlotLast are the client's research facility slot flags
	// (const.flagResearchFacilitySlotFirst/Last, eveFlags 200-255).
	ResearchSlotFirst int64 = 200
	ResearchSlotLast  int64 = 255

	// StationServiceLaboratory is staServices 16384; the lobby shows the research
	// facility button only where the station's serviceMask has it.
	StationServiceLaboratory int64 = 16384

	ActivityDuplicating        int64 = 6
	ActivityReverseEngineering int64 = 7

	SkillScience            int64 = 3402
	SkillDuplicating        int64 = 10298
	SkillReverseEngineering int64 = 3408

	// Journal refTypes in market_reftypes: 61 Duplicating, 62 Reverse Engineering
	// (RefFactorySlotRentalFee, 14, is in billsdb.go).
	RefDuplicating        = 61
	RefReverseEngineering = 62

	ResearchJobRunning   = 0
	ResearchJobDelivered = 1
	ResearchJobEjected   = 2

	// researchMaxRuns caps one job; the client's runs field goes to 2^31.
	researchMaxRuns int64 = 1000

	// researchFallbackRunSeconds is the per-run time for an item with no blueprint to
	// borrow a time from (faction modules, most implants).
	researchFallbackRunSeconds int64 = 3600

	win32TicksPerSec int64 = 10000000
)

// IsResearchSlotFlag reports whether flag is a research facility slot flag.
func IsResearchSlotFlag(flag int64) bool {
	return flag >= ResearchSlotFirst && flag <= ResearchSlotLast
}

// ResearchActivityName is the activity's wording in mail and refusals.
func ResearchActivityName(activityID int64) string {
	switch activityID {
	case ActivityDuplicating:
		return "Duplicating"
	case ActivityReverseEngineering:
		return "Reverse Engineering"
	}
	return "Unknown"
}

// ResearchActivitySkill is the skill whose level multiplies the base chance, the
// one the client reads for its confirmation prompt (facility.py AskAcceptProductionJobRisk).
func ResearchActivitySkill(activityID int64) int64 {
	if activityID == ActivityDuplicating {
		return SkillDuplicating
	}
	return SkillReverseEngineering
}

// ResearchSuccessChance is base * skill level, clamped to [0, 1]. It must match the
// client's arithmetic: the pilot accepted the job against the percentage it printed.
func ResearchSuccessChance(base float64, skillLevel int) float64 {
	c := base * float64(skillLevel)
	if c < 0 || math.IsNaN(c) {
		return 0
	}
	if c > 1 {
		return 1
	}
	return c
}

// ResearchRunSeconds is one run's duration: the borrowed blueprint time (or the
// fallback) scaled by research.timeMult and divided by the industry and research speed
// rates, as every RAM research job is. Never below one second.
func ResearchRunSeconds(baseSeconds int64, timeMult, industrySpeed, researchSpeed float64) int64 {
	if baseSeconds <= 0 {
		baseSeconds = researchFallbackRunSeconds
	}
	if timeMult <= 0 {
		timeMult = 1
	}
	if industrySpeed <= 0 {
		industrySpeed = 1
	}
	if researchSpeed <= 0 {
		researchSpeed = 1
	}
	s := math.Ceil(float64(baseSeconds) * timeMult / industrySpeed / researchSpeed)
	if s < 1 {
		return 1
	}
	return int64(s)
}

// ClampResearchRuns bounds a requested run count to [1, researchMaxRuns].
func ClampResearchRuns(runs int64) int64 {
	if runs < 1 {
		return 1
	}
	if runs > researchMaxRuns {
		return researchMaxRuns
	}
	return runs
}

// ResearchRollSuccesses rolls runs independent attempts at chance; rnd returns a
// float in [0, 1).
func ResearchRollSuccesses(runs int64, chance float64, rnd func() float64) int64 {
	var n int64
	for i := int64(0); i < runs; i++ {
		if rnd() < chance {
			n++
		}
	}
	return n
}

// ResearchItemCategoryAllowed mirrors the install dialog's item list (facility.py:
// Ship, Module, Charge, Implant, Drone); the dialog is the only client-side filter.
func ResearchItemCategoryAllowed(categoryID int64) bool {
	switch categoryID {
	case 6, 7, 8, 18, 20:
		return true
	}
	return false
}

// ResearchMaterialNeed splits one tl2 material row into what a job of runs consumes
// and what it needs present. As in manufacturing, damagePerJob >= 1 is consumed per
// run; anything lower is a tool, required but not eaten.
func ResearchMaterialNeed(quantity int64, damagePerJob float64, runs int64) (consume, present int64) {
	if quantity <= 0 {
		return 0, 0
	}
	if damagePerJob >= 1 {
		return quantity * runs, 0
	}
	return 0, quantity
}

// ResearchIterationsCompleted is how many runs have elapsed, the progress figure the
// slot window prints. Rolls happen at the end, all at once.
func ResearchIterationsCompleted(runs, perRunSeconds, elapsedSeconds int64) int64 {
	if perRunSeconds <= 0 || elapsedSeconds <= 0 {
		return 0
	}
	n := elapsedSeconds / perRunSeconds
	if n > runs {
		return runs
	}
	return n
}

// ResearchSlotView is how one slot reads to one pilot.
type ResearchSlotView struct {
	Rented        bool // somebody holds the rental
	CanUse        bool // the caller (or the caller's corp) holds it
	AccessAllowed bool // the caller may see what is installed
	ForCorp       bool // the rental is a corporation's
	Public        bool // free to rent (PUBLICLY AVAILABLE)
}

// ResearchSlotViewFor answers the slot row's access flags. A corporation rental is
// usable by its members (the install re-checks hangar roles), a personal one by its
// renter. An unrented slot reads as publicly available.
func ResearchSlotViewFor(rented bool, renterID int64, forCorp bool, charID, corpID int64) ResearchSlotView {
	if !rented {
		return ResearchSlotView{Public: true}
	}
	v := ResearchSlotView{Rented: true, ForCorp: forCorp}
	if forCorp {
		v.CanUse = corpID != 0 && renterID == corpID
	} else {
		v.CanUse = renterID == charID
	}
	v.AccessAllowed = v.CanUse
	return v
}
