// Package dogma holds the attribute, effect and skill IDs used by the dogma subsystem
// and the pure attribute math built on them (hull, gang and implant bonuses).
package dogma

// Attribute ID constants from dgmAttributeTypes.
const (
	AttrIsOnline              int64 = 2
	AttrDamage                int64 = 3
	AttrCapacitorNeed         int64 = 6 // cap consumed per activation cycle
	AttrPowerOutput           int64 = 11
	AttrLowSlots              int64 = 12
	AttrMedSlots              int64 = 13
	AttrHiSlots               int64 = 14
	AttrPowerLoad             int64 = 15
	AttrCharge                int64 = 18
	AttrPowerConsumption      int64 = 30
	AttrMaxVelocity           int64 = 37
	AttrCapacity              int64 = 38
	AttrCPUOutput             int64 = 48
	AttrCPULoad               int64 = 49
	AttrCPUUsage              int64 = 50
	AttrRechargeRate          int64 = 55 // capacitor recharge time (ms)
	AttrAgility               int64 = 70
	AttrDuration              int64 = 73
	AttrMiningAmount          int64 = 77
	AttrLauncherSlotsLeft     int64 = 101
	AttrTurretSlotsLeft       int64 = 102
	AttrHullKinResonance      int64 = 109
	AttrHullThermResonance    int64 = 110
	AttrHullExplResonance     int64 = 111
	AttrHullEmResonance       int64 = 113
	AttrLauncherGroup         int64 = 137
	AttrEmDamageBonus         int64 = 138
	AttrShieldCapacity        int64 = 263
	AttrShieldCharge          int64 = 264
	AttrDroneCapacity         int64 = 283 // drone bay volume (m3)
	AttrArmorDamage           int64 = 266
	AttrArmorEmResonance      int64 = 267
	AttrArmorExplResonance    int64 = 268
	AttrArmorKinResonance     int64 = 269
	AttrArmorThermResonance   int64 = 270
	AttrShieldEmResonance     int64 = 271
	AttrShieldExplResonance   int64 = 272
	AttrShieldKinResonance    int64 = 273
	AttrShieldThermResonance  int64 = 274
	AttrHP                    int64 = 9   // structure/hull hitpoints
	AttrEmDamage              int64 = 114 // EM damage (on charges/ammo)
	AttrExplosiveDamage       int64 = 116 // explosive damage
	AttrKineticDamage         int64 = 117 // kinetic damage
	AttrThermalDamage         int64 = 118 // thermal damage
	AttrDamageMultiplier      int64 = 64  // weapon damage multiplier (on turrets)
	AttrShieldBonus           int64 = 68  // shield booster: HP per cycle
	AttrCapacitorBonus        int64 = 67  // cap booster charge: GJ injected
	AttrArmorHP               int64 = 265 // armor hitpoints
	AttrStructureDamageAmount int64 = 83  // hull repairer: HP per cycle
	AttrArmorDamageAmount     int64 = 84  // armor repairer: HP per cycle
	AttrShieldRechargeRate    int64 = 479 // shield recharge time (ms)
	AttrSkillTimeConstant     int64 = 275
	AttrMaxRange              int64 = 54
	AttrCapacitorCapacity     int64 = 482
	AttrChargeSize            int64 = 128
	AttrChargeGroup1          int64 = 604
	AttrChargeGroup2          int64 = 605
	AttrChargeGroup3          int64 = 606
	AttrChargeGroup4          int64 = 609
	AttrChargeGroup5          int64 = 610
	AttrSignatureRadius       int64 = 552 // target's signature radius
	AttrOptimalSigRadius      int64 = 620 // turret optimal signature resolution
	AttrScanResolution        int64 = 564 // ship's scan resolution
	AttrExplosionDelay        int64 = 281 // missile max flight time (ms)
	AttrMaxFlightTimeBonus    int64 = 557 // Missile Bombardment: % flight time per level
	AttrSpeed                 int64 = 51  // rate of fire / cycle time (ms)
	AttrFalloff               int64 = 158 // weapon falloff range (metres)
	AttrTrackingSpeed         int64 = 160 // turret tracking speed (rad/s)
	AttrEntityMissileTypeID   int64 = 507 // NPC missile type to fire
	AttrEntityAttackRange     int64 = 247 // entityAttackRange: NPC opens fire within this
	AttrEntityFlyRange        int64 = 416 // entityFlyRange: NPC preferred orbit range
	AttrEntityChaseMaxDist    int64 = 665 // entityChaseMaxDistance: give-up distance
	AttrEntityAttackDelayMin  int64 = 475 // entityAttackDelayMin: first-shot delay (ms)
	AttrEntityAttackDelayMax  int64 = 476 // entityAttackDelayMax (ms)
	AttrMissileDmgMult        int64 = 212 // missileDamageMultiplier (entity)
	AttrDurationSkillBonus    int64 = 312 // repair skills: -% cycle time per level
	AttrShieldBoostMultiplier int64 = 548 // Shield Boost Amplifier: +% boost amount
	AttrShieldBoostCapBonus   int64 = 851 // Shield Compensation: -% booster cap need per level

	// Character attributes.
	AttrIntelligence int64 = 175
	AttrMemory       int64 = 176
	AttrCharisma     int64 = 177
	AttrWillpower    int64 = 178
	AttrPerception   int64 = 179

	// EWAR / propulsion module attributes.
	AttrSpeedFactor          int64 = 20  // web: % speed change (-75); prop mod: boost %
	AttrWarpScrambleStrength int64 = 105 // scramble points applied to target
	AttrSpeedBoostFactor     int64 = 567 // prop-mod thrust (N)
	AttrMassAddition         int64 = 796 // prop-mod / plate mass penalty (kg)

	// EWAR debuff magnitudes (RMR modules carry ready-made multipliers).
	AttrMaxTargetRangeMultiplier int64 = 237 // sensor damp: victim targeting range × (<1)
	AttrMaxRangeMultiplier       int64 = 243 // tracking disruptor: victim turret optimal × (<1)
	AttrTrackingSpeedMultiplier  int64 = 244 // tracking disruptor: victim turret tracking × (<1)
	AttrSignatureRadiusBonus     int64 = 554 // target painter: +% victim signature radius
	AttrScanResolutionMultiplier int64 = 565 // sensor damp: victim scan resolution × (<1)

	// Capacitor warfare and area-of-effect modules. Each carries its own amount/range
	// pair; the effect rows point at these, not at maxRange(54).
	AttrPowerTransferAmount         int64 = 90  // nosferatu: GJ stolen per cycle
	AttrPowerTransferRange          int64 = 91  // nosferatu range (m)
	AttrEnergyDestabilizationAmount int64 = 97  // neutralizer: GJ removed per cycle
	AttrEnergyDestabilizationRange  int64 = 98  // neutralizer range (m)
	AttrEmpFieldRange               int64 = 99  // smartbomb blast radius (m)
	AttrECMBurstRange               int64 = 142 // ECM burst radius (m)

	// Cloaking device attributes.
	AttrMaxVelocityBonus       int64 = 306 // x max velocity while cloaked (0.1 prototype, 1 covert ops)
	AttrCloakingTargetingDelay int64 = 560 // ms after a decloak before the ship may lock again

	// Superweapon (doomsday) attributes. The doomsdays carry no range attribute: their
	// effect is empWaveGrid, so the blast radius is destiny.BubbleRadius.
	AttrModuleReactivationDelay     int64 = 669  // ms of enforced idle after a cycle ends
	AttrConsumptionType             int64 = 713  // typeID burned per activation
	AttrConsumptionQuantity         int64 = 714  // units of AttrConsumptionType per activation
	AttrMaxGroupOnline              int64 = 978  // most of this invGroup that may be online at once
	AttrDisallowRepeatingActivation int64 = 1014 // 1 = one cycle only, never auto-repeat
	AttrCPUPenaltySuperWeapon       int64 = 1080 // effect 2435 multiplier applied to other superweapons
	AttrCPUBonusSuperWeapon         int64 = 1081 // effect 2436 multiplier applied to cpu(50) of SELF

	// Not enforced, though they sit beside the block above. maxGroupActive(763) is on 185
	// types, so honouring it changes every module list. moduleReactivationDelay(669) is
	// armed only by aoeEwarLoop; other types that carry it (cloaks, ECCM) never arm it, and
	// wiring one is an arming call and nothing else.

	// Capital mobility: cynosural fields, jump drives and jump portals.
	//
	// siegeModeWarpStatus(852) and disallowEarlyDeactivation(906) are set on exactly the
	// same seven types (three siege modules, both cyno generators, Clone Vat Bay, Jump
	// Portal Generator), so "852 > 0 means immobile" and "906 > 0 means no early
	// deactivation" are safe generic tests. maxGroupActive(763) is on 185 types and is not
	// honoured.
	AttrDeadspaceUnsafe           int64 = 801 // 1 = refuses to run inside a deadspace pocket
	AttrSiegeModeWarpStatus       int64 = 852 // >0 = ship is immobilised while this runs
	AttrDisallowEarlyDeactivation int64 = 906 // 1 = Deactivate is refused until the cycle ends

	// Jump drive, on the hull. canJump(861) is the boolean and jumpDriveRange(867) is the
	// light years; reading the range from 861 gives every capital 1.0 LY.
	// jumpDriveCapacitorNeed(898) is a fraction of capacitor capacity, not GJ.
	AttrCanJump                    int64 = 861 // >0 = hull has an onboard jump drive
	AttrJumpDriveConsumptionType   int64 = 866 // typeID of the racial isotope burned
	AttrJumpDriveRange             int64 = 867 // maximum jump, in light years
	AttrJumpDriveConsumptionAmount int64 = 868 // isotopes per light year
	AttrJumpDriveDuration          int64 = 869 // ms of enforced idle after a jump (300000)
	AttrJumpDriveCapacitorNeed     int64 = 898 // fraction of capacitor capacity, not GJ

	// Jump portal (titan bridge). The mass factor is on the generator; a bridged
	// subcapital carries no jump attributes, so the titan pays for every ship.
	AttrJumpPortalConsumptionMassFactor int64 = 1001 // strontium per kg of jumper mass
	AttrJumpPortalDuration              int64 = 1002 // declared by effect 2152, absent on type 23953

	// Fitting bonus attributes (modules).
	AttrCapacityBonus   int64 = 72  // shield extender: flat shield HP add
	AttrArmorHPBonusAdd int64 = 335 // armor plate: flat armor HP add
	AttrSpeedMultiplier int64 = 204 // damage mod: cycle-time multiplier (<1 = faster)

	// Ballistic Control Systems carry their damage bonus as a ready-made multiplier
	// (1.07 / 1.10) in this attribute, not in damageMultiplier(64).
	AttrMissileDamageMultiplierBonus int64 = 213

	// Skill bonus attributes (per-level percentages on skill types).
	AttrDamageMultiplierBonus int64 = 292 // +% weapon damage per level
	AttrRofBonus              int64 = 293 // % cycle time per level (negative = faster)
	AttrTurretSpeedBonus      int64 = 441 // Gunnery: % turret cycle time per level
	AttrRangeSkillBonus       int64 = 294 // Sharpshooter: +% turret optimal range per level
	AttrFalloffSkillBonus     int64 = 349 // Trajectory Analysis: +% turret falloff per level
	AttrCapNeedSkillBonus     int64 = 317 // Controlled Bursts / remote-rep emission skills: % capacitor need per level

	// Mining / survey attributes.
	AttrSurveyScanRange                       int64 = 197 // survey scanner: asteroid scan radius (m)
	AttrSpecialisationAsteroidGroup           int64 = 781 // mining crystal: asteroid groupID it boosts
	AttrSpecialisationAsteroidYieldMultiplier int64 = 782 // mining crystal: yield multiplier vs that group
	AttrCrystalVolatilityChance               int64 = 783 // per-cycle chance the crystal takes wear
	AttrCrystalVolatilityDamage               int64 = 784 // wear applied per failed volatility roll
	AttrCrystalsGetDamaged                    int64 = 786 // 1 = this charge wears out while mining
)

// Skill typeIDs applied server-side to combat math (Blood 3913 data).
const (
	SkillGunnery            int64 = 3300
	SkillRapidFiring        int64 = 3310
	SkillSurgicalStrike     int64 = 3315
	// Turret range skills, named outright because no gun or crystal requires them.
	// Per-level percentages: Sharpshooter +5% optimal (294), Trajectory Analysis +5%
	// falloff (349), Motion Prediction +5% tracking (244; a multiplier on a disruptor),
	// Controlled Bursts -5% cap use (317).
	SkillSharpshooter       int64 = 3311
	SkillTrajectoryAnalysis int64 = 3317
	SkillMotionPrediction   int64 = 3312
	SkillControlledBursts   int64 = 3316
	SkillMissileLauncherOp  int64 = 3319
	SkillWarheadUpgrades    int64 = 20315 // +2% missile damage per level (attr 292)
	SkillRapidLaunch        int64 = 21071 // -3% launcher cycle time per level (attr 293)
	// Missile range skills. No launcher or charge requires them, so they are named
	// outright like Warhead Upgrades and Rapid Launch.
	SkillMissileBombardment int64 = 12441 // +10% missile flight time per level (attr 557)
	SkillMissileProjection  int64 = 12442 // +10% missile velocity per level (attr 20)
	SkillSignatureAnalysis  int64 = 3431
	SkillShieldCompensation int64 = 21059 // -2% shield booster cap need per level (attr 851)
	SkillShieldManagement   int64 = 3419  // +5% shield capacity per level (attr 337)
	SkillShieldOperation    int64 = 3416  // -5% shield recharge time per level (attr 338)
	SkillHullUpgrades       int64 = 3394  // +5% armor HP per level (attr 335)
	SkillMechanic           int64 = 3392  // +5% hull HP per level (attr 327)
	SkillEnergyManagement   int64 = 3418  // +5% capacitor capacity per level (attr 1079)
	SkillEnergySystemsOp    int64 = 3417  // -5% capacitor recharge time per level (attr 314)

	// Mining skills.
	SkillMining         int64 = 3386  // +5% mining yield per level
	SkillAstrogeology   int64 = 3410  // +5% mining yield per level
	SkillDeepCoreMining int64 = 11395 // required to operate deep core miners (Mercoxit)
	SkillIceHarvesting  int64 = 16281 // required to operate ice harvesters

	AttrScanResolutionBonus int64 = 566 // Signature Analysis: +% scan resolution per level

	AttrMaxTargetRange      int64 = 76  // ship maximum targeting range (metres)
	AttrMaxTargetRangeBonus int64 = 309 // Long Range Targeting: +% targeting range per level
	SkillLongRangeTargeting int64 = 3428
)

// Weapon groupIDs to the dgmEffects IDs of their damage-mod effects (damage multiply,
// speed multiply): gyrostabilizers 92/89, heat sinks 91/95, magnetic field
// stabilizers 93/96.
var WeaponDamageModEffects = map[int64][2]int64{
	55: {92, 89}, // Projectile Weapon ← Gyrostabilizer
	53: {91, 95}, // Energy Weapon ← Heat Sink
	74: {93, 96}, // Hybrid Weapon ← Magnetic Field Stabilizer
	// Missile launcher groups to the Ballistic Control System effects (763 missileDMGBonus,
	// 889 missileLauncherSpeedMultiplier). BCS damage is the raw multiplier in
	// missileDamageMultiplierBonus(213); see GetWeaponCombatBonuses.
	56:  {763, 889}, // Missile Launcher (generic)
	506: {763, 889}, // Missile Launcher Cruise
	507: {763, 889}, // Missile Launcher Rocket
	508: {763, 889}, // Missile Launcher Siege
	509: {763, 889}, // Missile Launcher Standard
	510: {763, 889}, // Missile Launcher Heavy
	511: {763, 889}, // Missile Launcher Assault
	524: {763, 889}, // Missile Launcher Citadel
}

// ChargeGroupAttrs returns the attribute IDs that define which charge groups
// a module accepts.
func ChargeGroupAttrs() []int64 {
	return []int64{AttrChargeGroup1, AttrChargeGroup2, AttrChargeGroup3, AttrChargeGroup4, AttrChargeGroup5}
}

// ShieldResonanceAttrs returns the four shield damage resonance attribute IDs.
func ShieldResonanceAttrs() []int64 {
	return []int64{
		AttrShieldEmResonance, AttrShieldExplResonance,
		AttrShieldKinResonance, AttrShieldThermResonance,
	}
}

// ArmorResonanceAttrs returns the four armor damage resonance attribute IDs.
func ArmorResonanceAttrs() []int64 {
	return []int64{
		AttrArmorEmResonance, AttrArmorExplResonance,
		AttrArmorKinResonance, AttrArmorThermResonance,
	}
}

// HullResonanceAttrs returns the four hull/structure damage resonance attribute IDs.
func HullResonanceAttrs() []int64 {
	return []int64{
		AttrHullEmResonance, AttrHullExplResonance,
		AttrHullKinResonance, AttrHullThermResonance,
	}
}

// AllResonanceAttrs returns all twelve damage resonance attribute IDs
// (shield, armor, hull) in one slice.
func AllResonanceAttrs() []int64 {
	return []int64{
		AttrShieldEmResonance, AttrShieldExplResonance,
		AttrShieldKinResonance, AttrShieldThermResonance,
		AttrArmorEmResonance, AttrArmorExplResonance,
		AttrArmorKinResonance, AttrArmorThermResonance,
		AttrHullEmResonance, AttrHullExplResonance,
		AttrHullKinResonance, AttrHullThermResonance,
	}
}
