// Package rates holds global gameplay rate multipliers, loaded once at
// startup from the "rates" block in config.json and read from hot paths
// all over the server (skill training, bounties, mining, loot, damage,
// industry).  Every rate defaults to 1.0 = stock behaviour; 2.0 doubles
// the effect.  "Bigger = more/faster" for every knob: a speed rate of 2
// halves the time, a yield rate of 2 doubles the output.
//
// The values are plain package variables written exactly once from main()
// before any connection is accepted, and only read afterwards, so no
// locking is needed.
package rates

var (
	// SkillTraining multiplies the SP/minute training rate.
	// 2.0 = skills train twice as fast.
	SkillTraining = 1.0

	// NPCBounty multiplies the ISK bounty paid for NPC kills.
	// 0 disables bounties entirely.
	NPCBounty = 1.0

	// MiningYield multiplies the m3 mined per module cycle.
	MiningYield = 1.0

	// LootDrop multiplies every loot drop chance (player wreck item
	// survival and authored NPC loot table rolls). Chances are capped
	// at 100% naturally; 0 disables drops.
	LootDrop = 1.0

	// PlayerDamage multiplies all damage dealt by player turrets and
	// missiles.
	PlayerDamage = 1.0

	// NPCDamage multiplies all damage dealt by NPC turrets and missiles.
	// 0 makes NPCs harmless.
	NPCDamage = 1.0

	// IndustrySpeed divides manufacturing/research/copy job durations.
	// 2.0 = jobs finish in half the time.
	IndustrySpeed = 1.0

	// ProductionSpeed divides manufacturing (activity 1) job durations,
	// on top of IndustrySpeed.
	ProductionSpeed = 1.0

	// ResearchSpeed divides ME research, PE research and copy
	// (activities 4, 3, 5) job durations, on top of IndustrySpeed.
	ResearchSpeed = 1.0

	// ResearchPoints multiplies the RP/day accrued with R&D agents.
	// 0 disables RP accrual.
	ResearchPoints = 1.0

	// MissionReward multiplies a completed mission's ISK reward and its
	// time bonus.
	MissionReward = 1.0

	// LoyaltyPoints multiplies the LP a completed mission pays. 0 disables LP.
	LoyaltyPoints = 1.0

	// StandingGain multiplies every standing change a mission authors: completion gains and
	// decline/quit/expiry losses alike.
	StandingGain = 1.0

	// RatStandingLoss multiplies the faction standing lost per NPC kill.
	// 0 disables the hit.
	RatStandingLoss = 1.0
)
