package seclaw

import "math"

// SecBand rounds a system's security to one decimal, as concordDelayForSecurity and
// sentrySecurityQualifies do, so all three agree on the band.
func SecBand(sec float64) float64 { return math.Round(sec*10) / 10 }

// LossFromGap is what a penalty of pct percent costs a pilot at rating current: pct of
// the distance down to SecMin, returned negative. A clean pilot pays more points than
// one already near the floor. At 2%: 0.0 costs -0.20, +10 costs -0.40, -8 costs -0.04.
// db.PenalizeSecurityRatingFraction runs the same arithmetic inside its UPDATE.
func LossFromGap(current, pct float64) float64 {
	if pct <= 0 || current <= SecMin {
		return 0
	}
	return -(current - SecMin) * pct / 100
}

// GainFromGap is pct percent of the distance up to SecMax.
func GainFromGap(current, pct float64) float64 {
	if pct <= 0 || current >= SecMax {
		return 0
	}
	return (SecMax - current) * pct / 100
}

// RatGain turns an NPC type's entitySecurityStatusKillBonus (dgmattribute 252) into
// the status a kill is worth to a pilot at rating current; mul is the operator's dial.
// A positive bonus is a fraction of the remaining gap to SecMax; a negative one is
// applied flat, or shooting a protected NPC would be free at the ceiling. This reference form
// is pinned by tests; the live award is the same arithmetic inside
// db.AwardSecurityRatingFraction's UPDATE, fed by RatFraction. The two must agree.
func RatGain(bonus, current, mul float64) float64 {
	if bonus == 0 || mul <= 0 {
		return 0
	}
	if bonus < 0 {
		return bonus * mul
	}
	if current >= SecMax {
		return 0
	}
	return (SecMax - current) * bonus * mul
}

// RatFraction is RatGain without the gap scaling, as the SQL wants it: bonus * mul,
// sign preserved. db.AwardSecurityRatingFraction switches to a flat UPDATE on a
// negative fraction.
func RatFraction(bonus, mul float64) float64 {
	if bonus == 0 || mul <= 0 {
		return 0
	}
	return bonus * mul
}
