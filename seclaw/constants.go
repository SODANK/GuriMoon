// Package seclaw is the arithmetic of security status: what a kill costs, what a rat
// is worth, and whether a shot was a crime.
//
// Everything here is pure: no database, no destiny or package main import, no state.
// The player death path, an embodied rolebot's guns and the rolebots abstract tier all
// share it. The exemption ladder in verdict.go is the one CONCORD runs.
package seclaw

// The security status ceiling and floor, the standing range. The db package's
// security writes take them as bound parameters.
const (
	SecMax = 10.0
	SecMin = -10.0
)

// The shipped penalties, as percent of the distance to SecMin (LossFromGap). The
// client renders every CONCORD transaction as modification*100 percent, so the record
// moves by fractions, not points. Transcribed from sql/eveconstants.sql, which stores
// signed values unsigned-wrapped (concordPodKillPenalty is an int8 254 = -2): there is
// no general loader. These are defaults; loadSecuritySettings loads the live values.
const (
	// DefaultAggressionPenaltyPct is the first shot on an innocent. The shipped row
	// ('concordAggressionPenalty',0) is zero, yet the client ships
	// SecurityStatusAggressionChange and the old two-step penalty charged the shot; 0.5
	// is the figure players measured.
	DefaultAggressionPenaltyPct = 0.5

	// DefaultShipKillPenaltyPct has no shipped row; 2.0 is the measured figure.
	DefaultShipKillPenaltyPct = 2.0

	// DefaultPodKillPenaltyPct. Raw row: ('concordPodKillPenalty',254), int8 -2.
	DefaultPodKillPenaltyPct = 2.0

	// DefaultPoliceAggressionPenaltyPct is charged once per engagement against the law.
	// Raw row: ('concordPoliceAggressionPenalty',254), int8 -2.
	DefaultPoliceAggressionPenaltyPct = 2.0

	// DefaultPoliceKillPenaltyPct. Raw row: ('concordPoliceKillPenalty',253), int8 -3.
	DefaultPoliceKillPenaltyPct = 3.0

	// DefaultAidPenaltyPct is the cost of repairing a criminal or an outlaw in empire
	// space (the client's AidGlobalCriminalEmpire2 and AidOutlawEmpire2 promise one).
	DefaultAidPenaltyPct = 0.5

	// DefaultDecayPerHour is the negative record forgotten per hour. Raw row:
	// ('standingBadSecurityMilliPointsForgotten',100), i.e. 0.1.
	DefaultDecayPerHour = 0.1

	// DefaultRatGainFallback is the flat gain for a type with no
	// entitySecurityStatusKillBonus attribute. An explicit zero is authored data and stays
	// zero; the fallback never replaces it.
	DefaultRatGainFallback = 0.01
)

// OutlawStatus is the record at which a pilot is Criminal to every client. Not
// tunable: the client hardcodes -5.0 (tactical.CheckCriminal on the slimItem's
// securityStatus, destiny/notify.go). Raw row: ('outlawSecurityStatus',251), int8 -5.
const OutlawStatus = -5.0
