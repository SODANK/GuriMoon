package dogma

import "sort"

// Implants: what a plugged-in cyberimplant (group 300) does to the pilot's ship, authored per
// effect name like HullBonusSpecs because the data has no dgmExpressions
// (TestImplantSpecsCoverEveryEffect holds the table to every effect in the dump).
//
// Three kinds, told apart by effect. Attribute implants carry a *LocationChar effect and +N in
// attrs 175-179 (folded by db.GetEffectiveCharAttr). Hardwirings carry one effect naming the
// skill bonus they borrow; the percent is read from that attribute, unscaled by skill level.
// Set pieces carry a setBonus* effect (the companion effect restating the stat is ignored)
// and combine as bonus_i x product of every plugged piece's multiplier. Alpha..Omega are slot
// positions; ImplantGrade comes from the multiplier. See docs/IMPLANTS.md#core.

// ImplantChannel names the stat an implant modifies.
type ImplantChannel uint8

const (
	ICNone               ImplantChannel = iota
	ICAttribute                         // +N to a character attribute (175-179); which one is ImplantTag.AttrID
	ICTurretDamage                      // 292, % turret damage; ReqSkill 0 = every turret, else one family
	ICTurretCycle                       // 441, % turret cycle time (negative = faster)
	ICTurretTracking                    // 244, % tracking
	ICTurretOptimal                     // 294, % optimal range
	ICTurretFalloff                     // 349, % falloff
	ICTurretCapNeed                     // 317, % turret capacitor need (negative = cheaper)
	ICLauncherCycle                     // 293, % launcher cycle time
	ICMissileVelocity                   // 20, % missile velocity
	ICMissileFlightTime                 // 557, % missile flight time
	ICMissileAoEVelocity                // 847 (tag only)
	ICMissileAoECloud                   // 848 (tag only)
	ICArmorHP                           // 335 / 1083
	ICShieldHP                          // 337
	ICHullHP                            // 327
	ICShieldRecharge                    // 338, % shield recharge time
	ICShieldBoost                       // 548, % shield booster amount
	ICArmorRepAmount                    // 806, % armour repairer amount
	ICRepairCycle                       // 312, % repairer cycle time
	ICCapCapacity                       // 67, % capacitor capacity
	ICCapRecharge                       // 314, % capacitor recharge time
	ICVelocity                          // 315 / 1076 / 1084, % max velocity
	ICAgility                           // 151, % inertia (negative = nimbler)
	ICSigRadius                         // 554, % signature radius (negative = smaller)
	ICTargetRange                       // 309, % max targeting range
	ICMiningYield                       // 434 on the Astrogeology effect, % laser yield
	ICMiningDroneYield                  // 434 on the Mining Drone Operation effect
	ICIceCycle                          // 780, % ice harvester cycle time
	ICNosDuration                       // 66 on the Talisman set, % nosferatu/neutralizer cycle
	ICRemoteRepCapNeed                  // 317 on an emission skill; ReqSkill names which
	ICPropDuration                      // 66 afterburner cycle (tag only)
	ICPropCapNeed                       // 317 AB / 216 MWD capacitor need (tag only)
	ICPropBoost                         // 318, % prop-mod speed factor (tag only)
	ICEwarDuration                      // 66 on scram / damp / TD effects (tag only)
	ICSmartbombDuration                 // 73 (tag only)
	ICFittingCPU                        // 310, % module CPU need; ReqSkill names the modules
	ICFittingPower                      // 323, % module power need; ReqSkill names the modules
	ICCPUOutput                         // 424, % ship CPU output
	ICPowerOutput                       // 313, % ship powergrid output
	ICWarpCapNeed                       // 319 (tag only)
	ICWarpSpeed                         // 601 (tag only)
	ICSensorStrength                    // 1027-1030 (tag only)
	ICResists                           // 994-997, the Aurora pair (tag only)
	ICMindlink                          // gang mindlinks: dogma/gangbonus.go owns them
	numImplantChannels
)

var implantChannelNames = [numImplantChannels]string{
	"none", "attribute", "turret damage", "turret cycle", "turret tracking",
	"turret optimal", "turret falloff", "turret cap", "launcher cycle",
	"missile velocity", "missile flight time", "missile aoe velocity",
	"missile aoe cloud", "armor hp", "shield hp", "hull hp", "shield recharge",
	"shield boost", "armor rep", "repair cycle", "cap capacity", "cap recharge",
	"velocity", "agility", "signature", "target range", "mining yield",
	"mining drone yield", "ice cycle", "nos duration", "remote rep cap",
	"prop duration", "prop cap", "prop boost", "ewar duration",
	"smartbomb duration", "fitting cpu", "fitting power", "cpu output",
	"power output", "warp cap", "warp speed", "sensor strength", "resists",
	"mindlink",
}

func (c ImplantChannel) String() string {
	if int(c) < len(implantChannelNames) {
		return implantChannelNames[c]
	}
	return "?"
}

// Folded reports whether this server applies the channel to ship math. Tag-only channels
// are listed in docs/IMPLANTS.md#core.
func (c ImplantChannel) Folded() bool {
	switch c {
	case ICTurretDamage, ICTurretCycle, ICTurretTracking, ICTurretOptimal, ICTurretFalloff,
		ICTurretCapNeed, ICLauncherCycle, ICMissileVelocity, ICMissileFlightTime,
		ICArmorHP, ICShieldHP, ICHullHP, ICShieldRecharge, ICShieldBoost, ICArmorRepAmount,
		ICRepairCycle, ICCapCapacity, ICCapRecharge, ICVelocity, ICAgility, ICSigRadius,
		ICTargetRange, ICMiningYield, ICMiningDroneYield, ICIceCycle, ICNosDuration,
		ICRemoteRepCapNeed, ICFittingCPU, ICFittingPower, ICCPUOutput, ICPowerOutput:
		return true
	}
	return false
}

// ImplantSet is one of the five pirate implant sets.
type ImplantSet uint8

const (
	ISNone     ImplantSet = iota
	ISSlave               // Sansha: armour HP
	ISSnake               // Serpentis: velocity
	ISCrystal             // Guristas: shield boost amount
	ISHalo                // Angel: signature radius
	ISTalisman            // Blood Raider: nos / neut cycle
	numImplantSets
)

var implantSetNames = [numImplantSets]string{"", "Slave", "Snake", "Crystal", "Halo", "Talisman"}

func (s ImplantSet) String() string {
	if int(s) < len(implantSetNames) {
		return implantSetNames[s]
	}
	return "?"
}

// ImplantSetPieces is how many pieces make a full set (Alpha..Omega).
const ImplantSetPieces = 6

// ImplantGrade is the set's grade, read off the multiplier.
type ImplantGrade uint8

const (
	IGNone ImplantGrade = iota
	IGLow               // "Low-grade": 1.10 per piece, 1.25 Omega
	IGHigh              // plain: 1.15 per piece, 1.50 Omega
)

func (g ImplantGrade) String() string {
	switch g {
	case IGLow:
		return "low"
	case IGHigh:
		return "high"
	}
	return ""
}

// ImplantGradeOf classifies a set multiplier.
func ImplantGradeOf(mult float64) ImplantGrade {
	switch {
	case mult <= 0:
		return IGNone
	case mult <= 1.1 || (mult > 1.2 && mult <= 1.25):
		return IGLow
	}
	return IGHigh
}

// Character attribute bonus attrs on an implant, in attribute order
// (charisma 164, intelligence 165, memory 166, perception 167, willpower
// 168 -> bonus attr = base + 11).
const (
	AttrCharismaBonus     int64 = 175
	AttrIntelligenceBonus int64 = 176
	AttrMemoryBonus       int64 = 177
	AttrPerceptionBonus   int64 = 178
	AttrWillpowerBonus    int64 = 179
	AttrImplantness       int64 = 331
)

// ImplantAttrBonusIDs are the five bonus attrs, charisma first.
var ImplantAttrBonusIDs = [5]int64{AttrCharismaBonus, AttrIntelligenceBonus, AttrMemoryBonus, AttrPerceptionBonus, AttrWillpowerBonus}

// Turret family, emission and fitting skills, for the scoped hardwirings.
const (
	SkillEnergyGridUpgrades     int64 = 3424
	SkillShieldUpgrades         int64 = 3425
	SkillElectronicsUpgrades    int64 = 3432
	SkillSmallHybridTurret      int64 = 3301
	SkillSmallProjectileTurret  int64 = 3302
	SkillSmallEnergyTurret      int64 = 3303
	SkillMediumHybridTurret     int64 = 3304
	SkillMediumProjectileTurret int64 = 3305
	SkillMediumEnergyTurret     int64 = 3306
	SkillLargeHybridTurret      int64 = 3307
	SkillLargeProjectileTurret  int64 = 3308
	SkillLargeEnergyTurret      int64 = 3309
	SkillShieldEmissionSystems  int64 = 3422
	SkillRemoteArmorRepair      int64 = 16069
	SkillEnergyEmissionSystems  int64 = 3423
)

// ImplantSpec is what one effect name means on an implant.
type ImplantSpec struct {
	Channel ImplantChannel
	// Attrs lists the attributes the percent may be stored in, first present
	// wins (velocity implants spell it three ways across the catalogue).
	Attrs []int64
	// ReqSkill scopes a turret damage spec to one family, or a remote-rep
	// cap spec to one emission skill. 0 = every weapon of the kind.
	ReqSkill int64
}

func ispec(c ImplantChannel, attrs ...int64) ImplantSpec {
	return ImplantSpec{Channel: c, Attrs: attrs}
}
func ispecSkill(c ImplantChannel, skill int64, attrs ...int64) ImplantSpec {
	return ImplantSpec{Channel: c, Attrs: attrs, ReqSkill: skill}
}

// ImplantEffectSpecs maps every non-set, non-attribute effect an implant can
// carry to its channel.
var ImplantEffectSpecs = map[string]ImplantSpec{
	// turrets
	"surgicalStrikeDamageMultiplierBonusPostPercentDamageMultiplierLocationShipModulesRequiringGunnery":                        ispec(ICTurretDamage, 292),
	"smallEnergyTurretDamageMultiplierBonusPostPercentDamageMultiplierLocationShipModulesRequiringSmallEnergyTurret":           ispecSkill(ICTurretDamage, SkillSmallEnergyTurret, 292),
	"mediumEnergyTurretDamageMultiplierBonusPostPercentDamageMultiplierLocationShipModulesRequiringMediumEnergyTurret":         ispecSkill(ICTurretDamage, SkillMediumEnergyTurret, 292),
	"largeEnergyTurretDamageMultiplierBonusPostPercentDamageMultiplierLocationShipModulesRequiringLargeEnergyTurret":           ispecSkill(ICTurretDamage, SkillLargeEnergyTurret, 292),
	"smallHybridTurretDamageMultiplierBonusPostPercentDamageMultiplierLocationShipModulesRequiringSmallHybridTurret":           ispecSkill(ICTurretDamage, SkillSmallHybridTurret, 292),
	"mediumHybridTurretDamageMultiplierBonusPostPercentDamageMultiplierLocationShipModulesRequiringMediumHybridTurret":         ispecSkill(ICTurretDamage, SkillMediumHybridTurret, 292),
	"largeHybridTurretDamageMultiplierBonusPostPercentDamageMultiplierLocationShipModulesRequiringLargeHybridTurret":           ispecSkill(ICTurretDamage, SkillLargeHybridTurret, 292),
	"smallProjectileTurretDamageMultiplierBonusPostPercentDamageMultiplierLocationShipModulesRequiringSmallProjectileTurret":   ispecSkill(ICTurretDamage, SkillSmallProjectileTurret, 292),
	"mediumProjectileTurretDamageMultiplierBonusPostPercentDamageMultiplierLocationShipModulesRequiringMediumProjectileTurret": ispecSkill(ICTurretDamage, SkillMediumProjectileTurret, 292),
	"largeProjectileTurretDamageMultiplierBonusPostPercentDamageMultiplierLocationShipModulesRequiringLargeProjectileTurret":   ispecSkill(ICTurretDamage, SkillLargeProjectileTurret, 292),
	"gunneryTurretSpeeBonusPostPercentSpeedLocationShipModulesRequiringGunnery":                                                ispec(ICTurretCycle, 441),
	"motionPredictionTrackingSpeedMultiplierPostPercentTrackingSpeedLocationShipModulesRequiringGunnery":                       ispec(ICTurretTracking, 244),
	"sharpshooterRangeSkillBonusPostPercentMaxRangeLocationShipModulesRequiringGunnery":                                        ispec(ICTurretOptimal, 294),
	"surgicalStrikeFalloffBonusPostPercentFalloffLocationShipModulesRequiringGunnery":                                          ispec(ICTurretFalloff, 349),
	"controlledBurstsCapNeedBonusPostPercentCapacitorNeedLocationShipModulesRequiringGunnery":                                  ispec(ICTurretCapNeed, 317),
	// missiles
	"missileSkillRapidLauncherRoF":               ispec(ICLauncherCycle, 293),
	"missileSkillMissileProjectileVelocityBonus": ispec(ICMissileVelocity, 20),
	"missileBombardmentMaxFlightTimeBonusPostPercentExplosionDelayOwnerCharModulesRequiringMissileLauncherOperation": ispec(ICMissileFlightTime, 557),
	"missileSkillAoeVelocityBonus":  ispec(ICMissileAoEVelocity, 847),
	"missileSkillAoeCloudSizeBonus": ispec(ICMissileAoECloud, 848),
	// tank
	"hullUpgradesArmorHpBonusPostPercentHpLocationShip":                                      ispec(ICArmorHP, 335),
	"implantArmorHpBonus2":                                                                   ispec(ICArmorHP, 1083),
	"shieldManagementShieldCapacityBonusPostPercentCapacityLocationShipGroupShield":          ispec(ICShieldHP, 337),
	"mechanicHullHpBonusPostPercentHpShip":                                                   ispec(ICHullHP, 327),
	"shieldOperationRechargeratebonusPostPercentRechargeRateLocationShipGroupShield":         ispec(ICShieldRecharge, 338),
	"shieldBoostAmplifierPassive":                                                            ispec(ICShieldBoost, 548),
	"structuralAnalysisEffect":                                                               ispec(ICArmorRepAmount, 806),
	"repairSystemsDurationBonusPostPercentDurationLocationShipModulesRequiringRepairSystems": ispec(ICRepairCycle, 312),
	"modifyArmorResonancePassivePostPercentPassive":                                          ispec(ICResists, 994, 995, 996, 997),
	"modifyShieldResonancePassivePostPercentPassive":                                         ispec(ICResists, 994, 995, 996, 997),
	// capacitor
	"energyManagementCapacitorBonusPostPercentCapacityLocationShipGroupCapacitor":                                ispec(ICCapCapacity, 67),
	"energysystemsoperationCapRechargeBonusPostPercentRechargeRateLocationShipGroupCapacitor":                    ispec(ICCapRecharge, 314),
	"remoteArmorSystemsCapNeedBonusPostPercentCapacitorNeedLocationShipModulesRequiringRemoteArmorSystems":       ispecSkill(ICRemoteRepCapNeed, SkillRemoteArmorRepair, 317),
	"shieldEmmisionSystemsCapNeedBonusPostPercentCapacitorNeedLocationShipModulesRequiringShieldEmmisionSystems": ispecSkill(ICRemoteRepCapNeed, SkillShieldEmissionSystems, 317),
	"energyEmmisionSystemsCapNeedBonusPostPercentCapacitorNeedLocationShipModulesRequiringEnergyEmmisionSystems": ispecSkill(ICRemoteRepCapNeed, SkillEnergyEmissionSystems, 317),
	// navigation
	"navigationVelocityBonusPostPercentMaxVelocityShip":                                                                 ispec(ICVelocity, 315, 1076, 1084),
	"navigationVelocityBonusPostPercentMaxVelocityLocationShip":                                                         ispec(ICVelocity, 315, 1076, 1084),
	"implantVelocityBonus":                                                                                              ispec(ICVelocity, 1076, 315, 1084),
	"implantVelocityBonus2":                                                                                             ispec(ICVelocity, 1084, 1076, 315),
	"evasiveManeuveringAgilityBonusPostPercentAgilityShip":                                                              ispec(ICAgility, 151),
	"structureStealthEmitterArraySigDecrease":                                                                           ispec(ICSigRadius, 554),
	"afterburnerDurationBonusPostPercentDurationLocationShipModulesRequiringAfterburner":                                ispec(ICPropDuration, 66),
	"accerationControlCapNeedBonusPostPercentCapacitorNeedLocationShipGroupAfterburner":                                 ispec(ICPropCapNeed, 317),
	"highSpeedManuveringCapacitorNeedMultiplierPostPercentCapacitorNeedLocationShipModulesRequiringHighSpeedManuvering": ispec(ICPropCapNeed, 216),
	"accerationControlSpeedFBonusPostPercentSpeedFactorLocationShipGroupAfterburner":                                    ispec(ICPropBoost, 318),
	"accerationControlSkillAb&MwdSpeedBoost":                                                                            ispec(ICPropBoost, 318),
	"warpdriveoperationWarpCapacitorNeedBonusPostPercentWarpCapacitorNeedLocationShip":                                  ispec(ICWarpCapNeed, 319),
	"warpSkillSpeed": ispec(ICWarpSpeed, 601),
	// targeting / sensors
	"longRangeTargetingMaxTargetRangeBonusPostPercentMaxTargetRangeLocationShipGroupElectronic": ispec(ICTargetRange, 309),
	"signatureAnalysisMaxTargetRangeBonusPostPercentMaxTargetRangeLocationShip":                 ispec(ICTargetRange, 309),
	"scanStrengthBonusPercentPassive": ispec(ICSensorStrength, 1027, 1028, 1029, 1030),
	// mining
	"astrogeologyMiningAmountBonusPostPercentMiningAmountLocationShipModulesRequiringMining": ispec(ICMiningYield, 434),
	"miningDroneOperationMiningAmountBonusPostPercentMiningDroneAmountPercentChar":           ispec(ICMiningDroneYield, 434),
	"iceHarvestCycleTimeModulesRequiringIceHarvesting":                                       ispec(ICIceCycle, 780),
	// ewar / smartbombs (tag only)
	"propulsionJammingDurationBonusPostPercentDurationLocationShipModulesRequiringPropulsionJamming": ispec(ICEwarDuration, 66),
	"sensorLinkingDurationBonusPostPercentDurationLocationShipModulesRequiringSensorLinking":         ispec(ICEwarDuration, 66),
	"weaponDisruptionDurationBonusPostPercentDurationLocationShipModulesRequiringWeaponDisruption":   ispec(ICEwarDuration, 66),
	// the one "Electronic Warfare I" row stores durationBonus, whatever the effect name says
	"implantEWcpuneed": ispec(ICEwarDuration, 66),
	"energyPulseWeaponsDurationBonusPostPercentDurationLocationShipModulesRequiringEnergyPulseWeapons": ispec(ICSmartbombDuration, 73),
	// fitting: the need lines hit modules that directly require ReqSkill ("sensorUpgrades" is
	// the effect name of Electronics Upgrades)
	"weaponUpgradesCpuNeedBonusPostPercentCpuLocationShipModulesRequiringMissileLauncherOperation": ispecSkill(ICFittingCPU, SkillMissileLauncherOp, 310),
	"weaponUpgradesCpuNeedBonusPostPercentCpuLocationShipModulesRequiringGunnery":                  ispecSkill(ICFittingCPU, SkillGunnery, 310),
	"sensorUpgradesCpuNeedBonusPostPercentCpuLocationShipModulesRequiringSensorUpgrades":           ispecSkill(ICFittingCPU, SkillElectronicsUpgrades, 310),
	"energyGridUpgradesCpuNeedBonusPostPercentCpuLocationShipModulesRequiringEnergyGridUpgrades":   ispecSkill(ICFittingCPU, SkillEnergyGridUpgrades, 310),
	"shieldUpgradesPowerNeedBonusPostPercentPowerLocationShipModulesRequiringShieldUpgrades":       ispecSkill(ICFittingPower, SkillShieldUpgrades, 323),
	"electronicsCpuOutputBonusPostPercentCpuOutputLocationShipGroupComputer":                       ispec(ICCPUOutput, 424),
	"engineeringPowerEngineeringOutputBonusPostPercentPowerOutputLocationShipGroupPowerCore":       ispec(ICPowerOutput, 313),
}

// ImplantSetSpec is what a setBonus* effect means.
type ImplantSetSpec struct {
	Set       ImplantSet
	Channel   ImplantChannel
	BonusAttr int64 // the per-piece stat percent (0 on the Omega)
	MultAttr  int64 // the per-piece set multiplier
}

// ImplantSetSpecs maps the set effects. Two sets carry a second effect row
// naming the same set; both map to the one spec.
var ImplantSetSpecs = map[string]ImplantSetSpec{
	"setBonusSansha":               {ISSlave, ICArmorHP, 335, 864},
	"setBonusSerpentis":            {ISSnake, ICVelocity, 315, 802},
	"setBonusSerpentis2":           {ISSnake, ICVelocity, 315, 802},
	"setBonusGuristas":             {ISCrystal, ICShieldBoost, 548, 838},
	"angelsetbonus":                {ISHalo, ICSigRadius, 554, 863},
	"setBonusBloodraider":          {ISTalisman, ICNosDuration, 66, 799},
	"setBonusBloodraiderNosferatu": {ISTalisman, ICNosDuration, 66, 799},
}

// ImplantAttrEffects are the five character-attribute effects; the value is
// the bonus attr they add.
var ImplantAttrEffects = map[string]int64{
	"empathyCharismaBonusModAddCharismaLocationChar":                AttrCharismaBonus,
	"analyticalMindIntelligenceBonusModAddIntelligenceLocationChar": AttrIntelligenceBonus,
	"instantRecallMemoryBonusModAddMemoryLocationChar":              AttrMemoryBonus,
	"spatialAwarenessPerceptionBonusModAddPerceptionLocationChar":   AttrPerceptionBonus,
	"ironWillWillpowerBonusModAddWillpowerLocationChar":             AttrWillpowerBonus,
}

// ImplantIgnoredEffects are effects an implant may carry that mean nothing
// to the ship math here: flavour, the gang mindlinks (gangbonus.go), and the
// Aurora "no offence" flag.
var ImplantIgnoredEffects = map[string]bool{
	"smugglingModifier":        true,
	"disallowOffensiveActChar": true,
	"miningYieldGangBonus":     true,
	"miningForemanMindlink":    true,
	"armoredWarfareMindlink":   true,
	"armorTankingGang2":        true,
	"covertWarfareMindlink":    true,
	"reconOperationsMaxTargetRangeBonusPostPercentMaxTargetRangeGangShips":           true,
	"shieldDefensiveOperationsShieldCapacityBonusPostPercentShieldCapacityGangShips": true,
	"siegeWarfareMindlink":          true,
	"shirmishWarfareMindlink":       true,
	"skirmishWarfareVelocityBonus2": true,
}

// mindlinkEffects are the ones that mark a type as a gang mindlink.
var mindlinkEffects = map[string]bool{
	"miningForemanMindlink": true, "armoredWarfareMindlink": true, "covertWarfareMindlink": true,
	"siegeWarfareMindlink": true, "shirmishWarfareMindlink": true,
}

// ImplantEffect is one folded hardwiring line on a type: the channel, its
// scope and its signed percent.
type ImplantEffect struct {
	Channel  ImplantChannel
	ReqSkill int64
	Pct      float64
}

// ImplantTag is everything the server and the rolebots module know about
// one implant TYPE, derived from its effects and attributes alone.
type ImplantTag struct {
	Slot int64 // implantness (331), 1..20
	// AttrBonus is the +N on each character attribute, charisma first
	// (ImplantAttrBonusIDs order). An attribute implant has exactly one
	// non-zero entry; set pieces carry one too.
	AttrBonus [5]float64
	// Effects are the hardwiring lines, strongest first. A set piece has
	// exactly one, from its set spec.
	Effects []ImplantEffect
	// Set data. Omega pieces carry no stat bonus of their own.
	Set     ImplantSet
	Grade   ImplantGrade
	SetMult float64
	Omega   bool
	// Mindlink marks a gang mindlink (gangbonus.go applies it).
	Mindlink bool
	// Dummy is an implant with no effect beyond its slot (nine shipped "Hardwiring - X I" rows
	// carry no bonus attribute).
	Dummy bool
}

// Channel is the implant's headline channel: the first effect's, or
// ICAttribute for a pure attribute implant, ICMindlink for a mindlink.
func (t ImplantTag) Channel() ImplantChannel {
	switch {
	case len(t.Effects) > 0:
		return t.Effects[0].Channel
	case t.Mindlink:
		return ICMindlink
	case t.HasAttrBonus():
		return ICAttribute
	}
	return ICNone
}

// Pct is the headline effect's percent (0 for an attribute implant).
func (t ImplantTag) Pct() float64 {
	if len(t.Effects) > 0 {
		return t.Effects[0].Pct
	}
	return 0
}

// HasAttrBonus reports a non-zero character attribute bonus.
func (t ImplantTag) HasAttrBonus() bool {
	for _, b := range t.AttrBonus {
		if b != 0 {
			return true
		}
	}
	return false
}

// AttrID is the character attribute (164-168) the implant raises, with the
// bonus; (0, 0) when none.
func (t ImplantTag) AttrID() (attrID int64, bonus float64) {
	for i, b := range t.AttrBonus {
		if b != 0 {
			return ImplantAttrBonusIDs[i] - 11, b
		}
	}
	return 0, 0
}

// TagImplant classifies one implant type from its effect names and type attributes, in
// order: a setBonus* effect makes it a set piece (its companion effects are skipped); a
// mindlink effect makes it a mindlink; otherwise each spec'd effect whose attribute is
// present becomes a hardwiring line.
func TagImplant(effects []string, attrs map[int64]float64) ImplantTag {
	t := ImplantTag{Slot: int64(attrs[AttrImplantness])}
	for i, id := range ImplantAttrBonusIDs {
		t.AttrBonus[i] = attrs[id]
	}
	for _, e := range effects {
		if sp, ok := ImplantSetSpecs[e]; ok {
			t.Set = sp.Set
			t.SetMult = attrs[sp.MultAttr]
			t.Grade = ImplantGradeOf(t.SetMult)
			if b := attrs[sp.BonusAttr]; b != 0 {
				t.Effects = []ImplantEffect{{Channel: sp.Channel, Pct: b}}
			} else {
				t.Omega = true
			}
			return t
		}
	}
	for _, e := range effects {
		if mindlinkEffects[e] {
			t.Mindlink = true
			return t
		}
	}
	seen := map[ImplantChannel]bool{}
	for _, e := range effects {
		sp, ok := ImplantEffectSpecs[e]
		if !ok {
			continue
		}
		for _, a := range sp.Attrs {
			if v := attrs[a]; v != 0 {
				// A type carrying three gunnery effects (the II/III rows) holds one attribute; the other
				// effects find nothing to read, so no channel is charged twice.
				if !seen[sp.Channel] {
					seen[sp.Channel] = true
					t.Effects = append(t.Effects, ImplantEffect{Channel: sp.Channel, ReqSkill: sp.ReqSkill, Pct: v})
				}
				break
			}
		}
	}
	sort.SliceStable(t.Effects, func(i, j int) bool {
		ai, aj := t.Effects[i].Pct, t.Effects[j].Pct
		if ai < 0 {
			ai = -ai
		}
		if aj < 0 {
			aj = -aj
		}
		return ai > aj
	})
	t.Dummy = len(t.Effects) == 0 && !t.HasAttrBonus()
	return t
}

// ImplantSetFactor is the classic set rule: the product of every plugged
// piece's multiplier, applied to each piece's own bonus. A single piece is
// its own multiplier; the Omega contributes only to the product.
func ImplantSetFactor(mults []float64) float64 {
	f := 1.0
	for _, m := range mults {
		if m > 0 {
			f *= m
		}
	}
	return f
}
