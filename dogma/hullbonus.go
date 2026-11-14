package dogma

// Ship hull bonuses ("Special Ability" traits): per-skill-level and flat role bonuses granted
// by the hull itself, e.g. "Rifter: 5% bonus to Small Projectile Turret damage per Minmatar
// Frigate level".
//
// The data carries no dgmExpressions, so HullBonusSpecs encodes per effect name which stat an
// effect modifies, from which shipBonus* attribute, scaled by which skill. A spec reads its
// percentage from the first Attrs entry present on the hull and scales it by the pilot's
// trained level of SkillID; SkillID 0 is a flat role bonus. Factor specs store a multiplier
// (destroyerROFpenality 727 = 1.25) and read as (value - 1) x 100. HullBonusPct is the single
// fold for both shapes; GetShipHullBonuses and the rolebots fit-time twins use it.

// Damage type indices, matching the resonance attribute order used by
// entity.Resists and GetShipResistanceMultipliers: [em, expl, kin, therm].
const (
	DTAll       int8 = -1
	DTEm        int8 = 0
	DTExplosive int8 = 1
	DTKinetic   int8 = 2
	DTThermal   int8 = 3
)

// HullBonusTarget names the stat a hull bonus modifies.
type HullBonusTarget uint8

const (
	HBTurretDamage HullBonusTarget = iota
	HBTurretROF                    // % cycle time (negative = faster)
	HBTurretTracking
	HBTurretOptimal
	HBTurretFalloff
	HBTurretCapNeed
	HBMissileDamage     // Dmg selects the damage type (DTAll = every type)
	HBLauncherROF       // % launcher cycle time
	HBMissileVelocity   // % charge maxVelocity (attr 37)
	HBMissileFlightTime // % charge explosionDelay (attr 281)
	HBShieldResist  // % resonance change (negative = more resistance)
	HBArmorResist
	HBShieldHP
	HBArmorHP
	HBCapCapacity
	HBCapRecharge // % capacitor recharge time (negative = faster)
	HBCargo
	HBVelocity
	HBTargetRange
	HBShieldBoostAmount
	HBShieldBoostCapNeed
	HBArmorRepairAmount
	HBArmorRepairCapNeed
	HBArmorRepairDuration
	HBMiningYield
)

// Ship command skill typeIDs (Blood 3913 data).
const (
	SkillAmarrFrigate      int64 = 3331
	SkillCaldariFrigate    int64 = 3330
	SkillGallenteFrigate   int64 = 3328
	SkillMinmatarFrigate   int64 = 3329
	SkillAmarrCruiser      int64 = 3335
	SkillCaldariCruiser    int64 = 3334
	SkillGallenteCruiser   int64 = 3332
	SkillMinmatarCruiser   int64 = 3333
	SkillAmarrBattleship   int64 = 3339
	SkillCaldariBattleship int64 = 3338
	SkillGallenteBattleship int64 = 3336
	SkillMinmatarBattleship int64 = 3337
	SkillAmarrIndustrial    int64 = 3343
	SkillCaldariIndustrial  int64 = 3342
	SkillGallenteIndustrial int64 = 3340
	SkillMinmatarIndustrial int64 = 3341
	SkillDestroyers         int64 = 12097
	SkillBattlecruisers     int64 = 12099
	SkillMiningBarge        int64 = 17940
	SkillExhumers           int64 = 22551
	SkillInterceptors       int64 = 12092
	SkillCovertOps          int64 = 12093
	SkillAssaultShips       int64 = 12095
	SkillLogistics          int64 = 12096
	SkillHeavyAssaultShips  int64 = 16591
	SkillInterdictors       int64 = 12098
	SkillTransportShips     int64 = 19719
	SkillCommandShips       int64 = 23950
	SkillReconShips         int64 = 22761
	SkillMiningFrigate      int64 = 11395 // custom skill on specialty mining frigates
)

// Hull bonus attribute IDs (shipBonus* / eliteBonus* in dgmAttributeTypes).
const (
	abMF   int64 = 460 // shipBonusMF
	abGF   int64 = 462
	abCF   int64 = 463
	abAF   int64 = 464
	abAC   int64 = 478
	ab2AF  int64 = 485 // shipBonus2AF
	abGC   int64 = 486
	abCC   int64 = 487
	abMC   int64 = 489
	abMB   int64 = 490
	abCB   int64 = 491
	abAB   int64 = 492
	abMI   int64 = 493
	abAI   int64 = 494
	abCI   int64 = 495
	abGI   int64 = 496
	abGI2  int64 = 813
	abGB   int64 = 500
	ab2CB  int64 = 501 // shipBonus2CB
	abMB2  int64 = 518
	abGB2  int64 = 561
	abAB2  int64 = 585
	abGF2  int64 = 586
	abMF2  int64 = 587
	abCF2  int64 = 588
	abCB3  int64 = 598
	abAC2  int64 = 656
	abCC2  int64 = 657
	abGC2  int64 = 658
	abMC2  int64 = 659
	abMD1  int64 = 729
	abD1   int64 = 732
	abD2   int64 = 733
	abCD1  int64 = 734
	abCD2  int64 = 735
	abGD1  int64 = 738
	abGD2  int64 = 739
	abMD2  int64 = 740
	abBC1  int64 = 741
	abBC2  int64 = 742
	abCBC1 int64 = 743
	abCBC2 int64 = 745
	abGBC2 int64 = 746
	abGBC1 int64 = 747
	abMBC1 int64 = 748
	abMBC2 int64 = 749
	abDF1  int64 = 758
	abDF2  int64 = 759
	abORE1 int64 = 773
	abORE2 int64 = 774
	abORE3 int64 = 926
	abPirate int64 = 793 // shipBonusPirateFaction (flat role bonuses)
	abABC1   int64 = 795
	// The destroyer pair: a raw cycle-time multiplier (1.25 on every T1 destroyer) and the +50%
	// small-turret optimal that offsets it, both flat. Interdictors carry attr 727 without the
	// effect and take no penalty.
	abDestroyerROF  int64 = 727 // destroyerROFpenality (multiplier, Factor)
	abMaxRangeBonus int64 = 351 // maxRangeBonus (%)

	abEliteInterceptor2  int64 = 804
	abEliteCoverOps1     int64 = 569
	abEliteCoverOps2     int64 = 839
	abEliteGunship1      int64 = 673
	abEliteGunship2      int64 = 675
	abEliteHeavyGunship1 int64 = 692
	abEliteHeavyGunship2 int64 = 693
	abEliteInterdictors1 int64 = 1012
	abEliteIndustrial1   int64 = 807
	abEliteIndustrial2   int64 = 808
	abEliteBarge1        int64 = 924
	abEliteReconShip1    int64 = 962
	abEliteCommandShips1 int64 = 1000
	abEliteCommandShips2 int64 = 999
)

// Weapon-class filters. Turret bonuses match on the weapon's required turret
// skill; launcher bonuses match on the launcher's groupID.
var (
	wcEnergySmall  = []int64{3303}
	wcEnergyMed    = []int64{3306}
	wcEnergyLarge  = []int64{3309}
	wcEnergyAll    = []int64{3303, 3306, 3309}
	wcHybridSmall  = []int64{3301}
	wcHybridMed    = []int64{3304}
	wcHybridLarge  = []int64{3307}
	wcHybridAll    = []int64{3301, 3304, 3307}
	wcProjSmall    = []int64{3302}
	wcProjMed     = []int64{3305}
	wcProjLarge   = []int64{3308}
	wcProjAll     = []int64{3302, 3305, 3308}
	wcTurretAll   = []int64{3301, 3302, 3303, 3304, 3305, 3306, 3307, 3308, 3309}
)

// Missile launcher groupIDs (invGroups).
const (
	GroupLauncherCruise   int64 = 506
	GroupLauncherRocket   int64 = 507
	GroupLauncherSiege    int64 = 508
	GroupLauncherStandard int64 = 509
	GroupLauncherHeavy    int64 = 510
	GroupLauncherAssault  int64 = 511
)

var (
	lgCruise      = []int64{GroupLauncherCruise}
	lgSiege       = []int64{GroupLauncherSiege}
	lgCruiseSiege = []int64{GroupLauncherCruise, GroupLauncherSiege}
	lgStandard    = []int64{GroupLauncherStandard, GroupLauncherRocket, GroupLauncherAssault}
	lgHeavy       = []int64{GroupLauncherHeavy}
	lgAssault     = []int64{GroupLauncherAssault}
	// A "light missile" bonus covers the Standard and Assault launchers, never the rocket
	// launcher (groups 509/511 accept charge groups 384/394, 507 accepts 387 alone). The Condor
	// carries separate rocket and light bonuses, so lgStandard would apply both to every launcher.
	lgLight  = []int64{GroupLauncherStandard, GroupLauncherAssault}
	lgRocket = []int64{GroupLauncherRocket}
	lgSmall  = []int64{GroupLauncherRocket, GroupLauncherStandard, GroupLauncherAssault}
)

// HullBonusSpec describes one hull bonus carried by a ship effect.
type HullBonusSpec struct {
	Target HullBonusTarget
	Dmg    int8 // damage type for HBMissileDamage / HB*Resist, else DTAll
	// Turret bonuses: weapon must require one of these skills. Missile
	// bonuses: launcher groupID must be in Groups (nil = every launcher).
	WeaponSkills []int64
	Groups       []int64
	// Hull attribute holding the percentage; first ID present wins.
	Attrs []int64
	// Skill whose trained level scales the percentage; 0 = flat role bonus.
	SkillID int64
	// Invert flips the stored sign (for data that stores reductions as
	// positive percentages, e.g. cap recharge "bonuses").
	Invert bool
	// Factor marks an attribute that stores a multiplier, not a percentage
	// (1.25 = +25%). Always a flat role bonus: the level is ignored.
	Factor bool
}

// HullBonusPct is the fold from a spec's source attribute value to the percentage every
// HullBonuses reader multiplies by: per-level percentage × trained level, a flat percentage,
// or a Factor's (value - 1) × 100, with Invert applied last. ok is false when the bonus does
// not apply (no value, or a per-level bonus with no trained level).
func HullBonusPct(sp HullBonusSpec, val float64, level int) (pct float64, ok bool) {
	if val == 0 {
		return 0, false
	}
	switch {
	case sp.Factor:
		pct = (val - 1) * 100
	case sp.SkillID != 0:
		if level <= 0 {
			return 0, false
		}
		pct = val * float64(level)
	default:
		pct = val
	}
	if sp.Invert {
		pct = -pct
	}
	return pct, true
}

func tur(t HullBonusTarget, wc []int64, attrs []int64, skill int64) HullBonusSpec {
	return HullBonusSpec{Target: t, Dmg: DTAll, WeaponSkills: wc, Attrs: attrs, SkillID: skill}
}

// turFactor is a flat turret bonus whose attribute is a raw multiplier.
func turFactor(t HullBonusTarget, wc []int64, attr int64) HullBonusSpec {
	return HullBonusSpec{Target: t, Dmg: DTAll, WeaponSkills: wc, Attrs: []int64{attr}, Factor: true}
}

func mis(dmg int8, groups []int64, attrs []int64, skill int64) HullBonusSpec {
	return HullBonusSpec{Target: HBMissileDamage, Dmg: dmg, Groups: groups, Attrs: attrs, SkillID: skill}
}

func lrof(groups []int64, attrs []int64, skill int64) HullBonusSpec {
	return HullBonusSpec{Target: HBLauncherROF, Dmg: DTAll, Groups: groups, Attrs: attrs, SkillID: skill}
}

// mrng builds a missile range bonus (velocity or flight time). Range is the product of the
// two, so a hull carrying both compounds; they are separate targets for that reason.
func mrng(t HullBonusTarget, groups []int64, attrs []int64, skill int64) HullBonusSpec {
	return HullBonusSpec{Target: t, Dmg: DTAll, Groups: groups, Attrs: attrs, SkillID: skill}
}

func res(t HullBonusTarget, dmg int8, attrs []int64, skill int64) HullBonusSpec {
	return HullBonusSpec{Target: t, Dmg: dmg, Attrs: attrs, SkillID: skill}
}

func stat(t HullBonusTarget, attrs []int64, skill int64) HullBonusSpec {
	return HullBonusSpec{Target: t, Dmg: DTAll, Attrs: attrs, SkillID: skill}
}

// HullBonusSpecs maps dgmEffects.effectName → the hull bonuses it grants.
// Only effects whose stat the server actually simulates are listed; unknown
// ship effects are ignored (and can be logged by callers for coverage).
var HullBonusSpecs = map[string][]HullBonusSpec{
	// ── turret damage ────────────────────────────────────────────────────
	"shipSETDmgBonusAF":        {tur(HBTurretDamage, wcEnergySmall, []int64{abAF}, SkillAmarrFrigate)},
	"shipETDamageAF":           {tur(HBTurretDamage, wcEnergyAll, []int64{abAF}, SkillAmarrFrigate)},
	"shipSHTDmgBonusGF":        {tur(HBTurretDamage, wcHybridSmall, []int64{abGF}, SkillGallenteFrigate)},
	"shipHybridDamageBonusCF":  {tur(HBTurretDamage, wcHybridSmall, []int64{abCF}, SkillCaldariFrigate)},
	"shipPDmgBonusMF":          {tur(HBTurretDamage, wcProjSmall, []int64{abMF}, SkillMinmatarFrigate)},
	"shipProjectileDamageDF1":  {tur(HBTurretDamage, wcProjSmall, []int64{abDF1}, SkillDestroyers)},
	"shipHTDmgBonusfixedGC":    {tur(HBTurretDamage, wcHybridMed, []int64{abGC}, SkillGallenteCruiser)},
	"shipProjectileDmgMC2":     {tur(HBTurretDamage, wcProjMed, []int64{abMC2}, SkillMinmatarCruiser)},
	"shipLaserRofAC2":          {tur(HBTurretROF, wcEnergyMed, []int64{abAC2}, SkillAmarrCruiser)},
	"shipHTDmgBonusGB":         {tur(HBTurretDamage, wcHybridLarge, []int64{abGB}, SkillGallenteBattleship)},
	"shipPTDmgBonusMB":         {tur(HBTurretDamage, wcProjLarge, []int64{abMB}, SkillMinmatarBattleship)},
	"shipHybridDmgGBC1":        {tur(HBTurretDamage, wcHybridMed, []int64{abBC1}, SkillBattlecruisers)},
	"shipLaserDamageBonusBC2":  {tur(HBTurretDamage, wcEnergyMed, []int64{abBC2}, SkillBattlecruisers)},
	"eliteBonusGunshipLaserDamage1":      {tur(HBTurretDamage, wcEnergySmall, []int64{abEliteGunship1}, SkillAssaultShips)},
	"eliteBonusGunshipLaserDamage2":      {tur(HBTurretDamage, wcEnergySmall, []int64{abEliteGunship2}, SkillAssaultShips)},
	"eliteBonusGunshipHybridDmg2":        {tur(HBTurretDamage, wcHybridSmall, []int64{abEliteGunship2}, SkillAssaultShips)},
	"eliteBonusGunshipProjectileDamage1": {tur(HBTurretDamage, wcProjSmall, []int64{abEliteGunship1}, SkillAssaultShips)},
	"eliteBonusGunshipProjectileDamage2": {tur(HBTurretDamage, wcProjSmall, []int64{abEliteGunship2}, SkillAssaultShips)},
	"eliteBonusHeavyGunshipHybridDmg2":     {tur(HBTurretDamage, wcHybridMed, []int64{abEliteHeavyGunship2}, SkillHeavyAssaultShips)},
	"eliteBonusHeavyGunshipLaserDmg2":      {tur(HBTurretDamage, wcEnergyMed, []int64{abEliteHeavyGunship2}, SkillHeavyAssaultShips)},
	"eliteBonusHeavyGunshipProjectileDmg2": {tur(HBTurretDamage, wcProjMed, []int64{abEliteHeavyGunship2}, SkillHeavyAssaultShips)},
	"eliteBonusCommandShipHybridDamageCS1":     {tur(HBTurretDamage, wcHybridMed, []int64{abEliteCommandShips1}, SkillCommandShips)},
	"eliteBonusCommandShipLaserDamageCS1":      {tur(HBTurretDamage, wcEnergyMed, []int64{abEliteCommandShips1}, SkillCommandShips)},
	"eliteBonusCommandShipProjectileDamageCS1": {tur(HBTurretDamage, wcProjMed, []int64{abEliteCommandShips1}, SkillCommandShips)},
	// Pirate-faction flat role bonuses.
	"shipBonusPirateFrigateProjDamage": {tur(HBTurretDamage, wcProjSmall, []int64{abPirate}, 0)},
	"shipBonusPirateSmallHybridDmg":    {tur(HBTurretDamage, wcHybridSmall, []int64{abPirate}, 0)},
	"shipHybridDmgPirateCruiser":       {tur(HBTurretDamage, wcHybridMed, []int64{abPirate}, 0)},
	"shipHybridDmgPirateBattleship":    {tur(HBTurretDamage, wcHybridLarge, []int64{abPirate}, 0)},

	// ── turret rate of fire ──────────────────────────────────────────────
	"shipPTspeedBonusMB2":       {tur(HBTurretROF, wcProjLarge, []int64{abMB2}, SkillMinmatarBattleship)},
	"shipETspeedBonusAB2":       {tur(HBTurretROF, wcEnergyLarge, []int64{abAB2}, SkillAmarrBattleship)},
	"shipPTurretSpeedBonusMC":   {tur(HBTurretROF, wcProjMed, []int64{abMC}, SkillMinmatarCruiser)},
	"shipPTurretSpeedBonusGB":   {tur(HBTurretROF, wcProjLarge, []int64{abGB}, SkillGallenteBattleship)},
	"shipProjectileRofMBC2":     {tur(HBTurretROF, wcProjMed, []int64{abBC2}, SkillBattlecruisers)},
	"eliteBonusCommandShipLaserROFCS2": {tur(HBTurretROF, wcEnergyMed, []int64{abEliteCommandShips2}, SkillCommandShips)},
	"shipProjectileRofPirateCruiser":    {tur(HBTurretROF, wcProjMed, []int64{abPirate}, 0)},
	"shipProjectileRofPirateBattleship": {tur(HBTurretROF, wcProjLarge, []int64{abPirate}, 0)},
	// The destroyer role penalty: every turret cycles x1.25 (Coercer, Cormorant, Catalyst,
	// Thrasher), as the client's fitting window shows.
	"destroyerPenalityTurretROF": {turFactor(HBTurretROF, wcTurretAll, abDestroyerROF)},

	// ── turret optimal range ─────────────────────────────────────────────
	"shipHybridRangeBonusCF":  {tur(HBTurretOptimal, wcHybridSmall, []int64{abCF}, SkillCaldariFrigate)},
	"shipHybridRangeBonusCF2": {tur(HBTurretOptimal, wcHybridSmall, []int64{abCF2}, SkillCaldariFrigate)},
	"shipHRangeBonusCC":       {tur(HBTurretOptimal, wcHybridMed, []int64{abCC}, SkillCaldariCruiser)},
	"shipBonusHybridOptimalCB": {tur(HBTurretOptimal, wcHybridLarge, []int64{abCB}, SkillCaldariBattleship)},
	"shipHybridOptimalCBC1":   {tur(HBTurretOptimal, wcHybridMed, []int64{abBC1}, SkillBattlecruisers)},
	"shipHybridRangeCD1":      {tur(HBTurretOptimal, wcHybridSmall, []int64{abDF1}, SkillDestroyers)},
	"shipLaserOptimalBonuseAF": {tur(HBTurretOptimal, wcEnergySmall, []int64{abAF}, SkillAmarrFrigate)},
	"shipProjectileOptimalBonuseMF2": {tur(HBTurretOptimal, wcProjSmall, []int64{abMF2}, SkillMinmatarFrigate)},
	"shipPTRangeBonusMB":      {tur(HBTurretOptimal, wcProjLarge, []int64{abMB}, SkillMinmatarBattleship)},
	"eliteBonusGunshipHybridOptimal1":     {tur(HBTurretOptimal, wcHybridSmall, []int64{abEliteGunship1}, SkillAssaultShips)},
	"eliteBonusGunshipLaserOptimal1":      {tur(HBTurretOptimal, wcEnergySmall, []int64{abEliteGunship1}, SkillAssaultShips)},
	"eliteBonusGunshipProjectileOptimal1": {tur(HBTurretOptimal, wcProjSmall, []int64{abEliteGunship1}, SkillAssaultShips)},
	"eliteBonusHeavyGunshipHybridOptimal1":     {tur(HBTurretOptimal, wcHybridMed, []int64{abEliteHeavyGunship1}, SkillHeavyAssaultShips)},
	"eliteBonusHeavyGunshipLaserOptimal1":      {tur(HBTurretOptimal, wcEnergyMed, []int64{abEliteHeavyGunship1}, SkillHeavyAssaultShips)},
	"eliteBonusHeavyGunshipProjectileOptimal1": {tur(HBTurretOptimal, wcProjMed, []int64{abEliteHeavyGunship1}, SkillHeavyAssaultShips)},
	"eliteBonusCommandShipHybridOptimalCS1":    {tur(HBTurretOptimal, wcHybridMed, []int64{abEliteCommandShips1}, SkillCommandShips)},
	"eliteBonusCommandShipLaserOptimalCS1":     {tur(HBTurretOptimal, wcEnergyMed, []int64{abEliteCommandShips1}, SkillCommandShips)},
	"eliteBonusCommandShipLaserOptimalCS1FIXED": {tur(HBTurretOptimal, wcEnergyMed, []int64{abEliteCommandShips1}, SkillCommandShips)},
	// The destroyer role range bonus: +50% small-turret optimal, flat, one effect per weapon
	// family (hybrid Cormorant/Catalyst, energy Coercer, projectile Thrasher); offsets the penalty above.
	"smallHybridMaxRangeBonus":     {tur(HBTurretOptimal, wcHybridSmall, []int64{abMaxRangeBonus}, 0)},
	"smallEnergyMaxRangeBonus":     {tur(HBTurretOptimal, wcEnergySmall, []int64{abMaxRangeBonus}, 0)},
	"smallProjectileMaxRangeBonus": {tur(HBTurretOptimal, wcProjSmall, []int64{abMaxRangeBonus}, 0)},

	// ── turret falloff ───────────────────────────────────────────────────
	"shipFalloffBonusGF":     {tur(HBTurretFalloff, wcHybridSmall, []int64{abGF2}, SkillGallenteFrigate)},
	"shipHybridFallOffGD1":   {tur(HBTurretFalloff, wcHybridSmall, []int64{abDF1}, SkillDestroyers)},
	"interceptor2ProjectileFallOff": {tur(HBTurretFalloff, wcProjSmall, []int64{abEliteInterceptor2}, SkillInterceptors)},
	"eliteBonusGunshipProjectileFalloff2":       {tur(HBTurretFalloff, wcProjSmall, []int64{abEliteGunship2}, SkillAssaultShips)},
	"eliteBonusHeavyGunshipHybridFallOff1":      {tur(HBTurretFalloff, wcHybridMed, []int64{abEliteHeavyGunship1}, SkillHeavyAssaultShips)},
	"eliteBonusHeavyGunshipProjectileFallOff1":  {tur(HBTurretFalloff, wcProjMed, []int64{abEliteHeavyGunship1}, SkillHeavyAssaultShips)},
	"eliteBonusInterdictorsProjectileFalloff1":  {tur(HBTurretFalloff, wcProjSmall, []int64{abEliteInterdictors1}, SkillInterdictors)},
	"eliteBonusCommandShipHybridFalloffCS2":     {tur(HBTurretFalloff, wcHybridMed, []int64{abEliteCommandShips2}, SkillCommandShips)},
	"eliteBonusCommandShipProjectileFalloffCS2": {tur(HBTurretFalloff, wcProjMed, []int64{abEliteCommandShips2}, SkillCommandShips)},

	// ── turret tracking ──────────────────────────────────────────────────
	"shipProjectileTrackingMF2":  {tur(HBTurretTracking, wcProjSmall, []int64{abMF2}, SkillMinmatarFrigate)},
	"shipBonusHybridTrackingGF2": {tur(HBTurretTracking, wcHybridSmall, []int64{abGF2}, SkillGallenteFrigate)},
	"shipHybridTrackingBonusGf":  {tur(HBTurretTracking, wcHybridSmall, []int64{abGF}, SkillGallenteFrigate)},
	"shipProjectileTrackingGF":   {tur(HBTurretTracking, wcProjSmall, []int64{abGF}, SkillGallenteFrigate)},
	"shipHybridTrackingGC2":      {tur(HBTurretTracking, wcHybridMed, []int64{abGC2}, SkillGallenteCruiser)},
	"shipProjectileTrackingGC2":  {tur(HBTurretTracking, wcProjMed, []int64{abGC2}, SkillGallenteCruiser)},
	"shipHybridTrackingGD2":      {tur(HBTurretTracking, wcHybridSmall, []int64{abDF2}, SkillDestroyers)},
	"shipProjectileTrackingMD2":  {tur(HBTurretTracking, wcProjSmall, []int64{abDF2}, SkillDestroyers)},
	"shipLaserTrackingAD2":       {tur(HBTurretTracking, wcEnergySmall, []int64{abDF2}, SkillDestroyers)},
	"shipHTTrackingBonusGB":      {tur(HBTurretTracking, wcHybridLarge, []int64{abGB}, SkillGallenteBattleship)},
	"shipHTTrackingBonusGB2":     {tur(HBTurretTracking, wcHybridLarge, []int64{abGB2}, SkillGallenteBattleship)},
	"interceptor2HybridTracking":     {tur(HBTurretTracking, wcHybridSmall, []int64{abEliteInterceptor2}, SkillInterceptors)},
	"interceptor2LaserTracking":      {tur(HBTurretTracking, wcEnergySmall, []int64{abEliteInterceptor2}, SkillInterceptors)},
	"interceptor2ProjectileTracking": {tur(HBTurretTracking, wcProjSmall, []int64{abEliteInterceptor2}, SkillInterceptors)},
	"eliteBonusGunshipHybridTracking2":          {tur(HBTurretTracking, wcHybridSmall, []int64{abEliteGunship2}, SkillAssaultShips)},
	"eliteBonusHeavyGunshipProjectileTracking2": {tur(HBTurretTracking, wcProjMed, []int64{abEliteHeavyGunship2}, SkillHeavyAssaultShips)},
	"eliteBonusCommandShipProjectileTrackingCS1": {tur(HBTurretTracking, wcProjMed, []int64{abEliteCommandShips1}, SkillCommandShips)},

	// ── turret capacitor need ────────────────────────────────────────────
	"shipEnergyTCapNeedBonusAF": {tur(HBTurretCapNeed, wcEnergyAll, []int64{ab2AF}, SkillAmarrFrigate)},
	"shipTCapNeedBonusAC":       {tur(HBTurretCapNeed, wcEnergyAll, []int64{abAC}, SkillAmarrCruiser)},
	"shipCapNeedBonusAB":        {tur(HBTurretCapNeed, wcEnergyAll, []int64{abAB}, SkillAmarrBattleship)},
	"shipLaserCapABC2":          {tur(HBTurretCapNeed, wcEnergyAll, []int64{abBC2}, SkillBattlecruisers)},
	"shipLaserCapNeedAD1":       {tur(HBTurretCapNeed, wcEnergyAll, []int64{abDF1}, SkillDestroyers)},
	"shipBonusPirateSanshaLaserCap": {tur(HBTurretCapNeed, wcEnergyAll, []int64{abPirate}, 0)},
	"shipLaserCapPirateCruiser":     {tur(HBTurretCapNeed, wcEnergyAll, []int64{abPirate}, 0)},
	"shipLaserCapPirateBattleship":  {tur(HBTurretCapNeed, wcEnergyAll, []int64{abPirate}, 0)},

	// ── missile damage ───────────────────────────────────────────────────
	// Kestrel/Condor family: kinetic from the primary attr, the other three from the secondary.
	"shipMissileKineticDamageCF":   {mis(DTKinetic, nil, []int64{abCF}, SkillCaldariFrigate)},
	"shipMissileEMDamageCF":        {mis(DTEm, nil, []int64{abCF2}, SkillCaldariFrigate)},
	"shipMissileExplosiveDamageCF": {mis(DTExplosive, nil, []int64{abCF2}, SkillCaldariFrigate)},
	"shipMissileThermalDamageCF":   {mis(DTThermal, nil, []int64{abCF2}, SkillCaldariFrigate)},
	"shipMissileKineticDamageCC":   {mis(DTKinetic, nil, []int64{abCC}, SkillCaldariCruiser)},
	// Breacher: explosive from the primary attr, EM/kin/therm secondary.
	"shipMissileExplosiveDamageMF1": {mis(DTExplosive, nil, []int64{abMF}, SkillMinmatarFrigate)},
	"shipMissileEMDamageMF2":        {mis(DTEm, nil, []int64{abMF2}, SkillMinmatarFrigate)},
	"shipMissileKineticDamageMF2":   {mis(DTKinetic, nil, []int64{abMF2}, SkillMinmatarFrigate)},
	"shipMissileThermalDamageMF2":   {mis(DTThermal, nil, []int64{abMF2}, SkillMinmatarFrigate)},
	// Inquisitor: EM from the secondary attr, expl/kin/therm primary.
	"shipMissileEMDamage2AF":        {mis(DTEm, nil, []int64{ab2AF}, SkillAmarrFrigate)},
	"shipMissileExplosiveDamageAF1": {mis(DTExplosive, nil, []int64{abAF}, SkillAmarrFrigate)},
	"shipMissileKineticDamageAF1":   {mis(DTKinetic, nil, []int64{abAF}, SkillAmarrFrigate)},
	"shipMissileThermalDamageAF1":   {mis(DTThermal, nil, []int64{abAF}, SkillAmarrFrigate)},
	"shipMissileCruiseKineticDmgCF2": {mis(DTKinetic, lgCruise, []int64{abCF2}, SkillCaldariFrigate)},
	"interceptor2MissileEmDamage":      {mis(DTEm, nil, []int64{abEliteInterceptor2}, SkillInterceptors)},
	"interceptor2MissileKineticDamage": {mis(DTKinetic, nil, []int64{abEliteInterceptor2}, SkillInterceptors)},
	"shipMissileThermDamageGallenteInterceptor2": {mis(DTThermal, nil, []int64{abEliteInterceptor2}, SkillInterceptors)},
	"eliteBonusCoverOpsCruiseEmDmg1":        {mis(DTEm, lgCruise, []int64{abEliteCoverOps1}, SkillCovertOps)},
	"eliteBonusCoverOpsCruiseExplosiveDmg1": {mis(DTExplosive, lgCruise, []int64{abEliteCoverOps1}, SkillCovertOps)},
	"eliteBonusCoverOpsCruiseThermalDmg1":   {mis(DTThermal, lgCruise, []int64{abEliteCoverOps1}, SkillCovertOps)},
	"eliteBonusInterdictorsMissileEMDamage1":      {mis(DTEm, nil, []int64{abEliteInterdictors1}, SkillInterdictors)},
	"eliteBonusInterdictorsMissileKineticDamage1": {mis(DTKinetic, nil, []int64{abEliteInterdictors1}, SkillInterdictors)},
	"eliteBonusInterdictorsMissileThermalDamage1": {mis(DTThermal, nil, []int64{abEliteInterdictors1}, SkillInterdictors)},
	"eliteReconLightMissileKineticDamage1": {mis(DTKinetic, lgStandard, []int64{abEliteReconShip1}, SkillReconShips)},
	"eliteReconHeavyMissileKineticDamage1": {mis(DTKinetic, lgHeavy, []int64{abEliteReconShip1}, SkillReconShips)},
	"eliteBonusCommandShipMissileKineticDamageCS1": {mis(DTKinetic, nil, []int64{abEliteCommandShips1}, SkillCommandShips)},

	// ── launcher rate of fire ────────────────────────────────────────────
	"shipCruiseLauncherROFBonus2CB": {lrof(lgCruise, []int64{ab2CB}, SkillCaldariBattleship)},
	"shipSiegeLauncherROFBonus2CB":  {lrof(lgSiege, []int64{ab2CB}, SkillCaldariBattleship)},
	"shipLauncherspeedCB2":          {lrof(lgCruiseSiege, []int64{ab2CB}, SkillCaldariBattleship)},
	"shipLauncherRofBonusMB1":       {lrof(nil, []int64{abMB}, SkillMinmatarBattleship)},
	"shipMissileLauncherRofCC2":     {lrof(nil, []int64{abCC2}, SkillCaldariCruiser)},
	"shipAssaultLauncherROFBC1":     {lrof(lgAssault, []int64{abBC1}, SkillBattlecruisers)},
	"shipHeavyLauncherROFBC1":       {lrof(lgHeavy, []int64{abBC1}, SkillBattlecruisers)},
	"eliteBonusHeavyGunshipLauncherRof2": {lrof(nil, []int64{abEliteHeavyGunship2}, SkillHeavyAssaultShips)},
	"eliteReconBonusAssaultLauncherROF1": {lrof(lgAssault, []int64{abEliteReconShip1}, SkillReconShips)},

	// ── missile range (velocity × flight time) ─────────────────────────────
	// Range decides whether a shot connects at all. Effects with no ship carrying them in this
	// data (shipMissileSpeedBonusAF, shipMissile*VelocityBonusCC1) are left out.
	"shipMissileLightVelocityBonusCC2": {mrng(HBMissileVelocity, lgLight, []int64{abCC2}, SkillCaldariCruiser)},
	"shipMissileHeavyVelocityBonusCC2": {mrng(HBMissileVelocity, lgHeavy, []int64{abCC2}, SkillCaldariCruiser)},
	"shipMissileLightVelocityBonusCF2": {mrng(HBMissileVelocity, lgLight, []int64{abCF2}, SkillCaldariFrigate)},
	"shipMissileRocketVelocityBonusCF2": {mrng(HBMissileVelocity, lgRocket, []int64{abCF2}, SkillCaldariFrigate)},
	"shipCruiseMissileVelocityBonusCB3": {mrng(HBMissileVelocity, lgCruise, []int64{abCB3}, SkillCaldariBattleship)},
	"shipTorpedosVelocityBonusCB3":      {mrng(HBMissileVelocity, lgSiege, []int64{abCB3}, SkillCaldariBattleship)},
	"shipMissileSpeedBonusCF":           {mrng(HBMissileVelocity, nil, []int64{abCF}, SkillCaldariFrigate)},
	"shipMissileVelocityDF1":            {mrng(HBMissileVelocity, nil, []int64{abDF1}, SkillDestroyers)},
	"interceptor2MissileVelocity":       {mrng(HBMissileVelocity, nil, []int64{abEliteInterceptor2}, SkillInterceptors)},
	"eliteBonusAssaultShipMissileVelocity1": {mrng(HBMissileVelocity, nil, []int64{abEliteGunship1}, SkillAssaultShips)},
	"eliteBonusHeavyGunshipLightMissileFlightTime1": {mrng(HBMissileFlightTime, lgLight, []int64{abEliteHeavyGunship1}, SkillHeavyAssaultShips)},
	"eliteBonusHeavyGunshipHeavyMissileFlightTime1": {mrng(HBMissileFlightTime, lgHeavy, []int64{abEliteHeavyGunship1}, SkillHeavyAssaultShips)},
	// Pirate-faction flat role bonuses (+50%, no skill).
	"shipMissileVelocityPirateFactionFrigate": {mrng(HBMissileVelocity, lgSmall, []int64{abPirate}, 0)},
	"shipMissileVelocityPirateFactionLight":   {mrng(HBMissileVelocity, lgLight, []int64{abPirate}, 0)},
	"shipMissileVelocityPirateFactionHeavy":   {mrng(HBMissileVelocity, lgHeavy, []int64{abPirate}, 0)},
	"shipMissileVelocityPirateFactionCruise":  {mrng(HBMissileVelocity, lgCruise, []int64{abPirate}, 0)},
	"shipMissileVelocityPirateFactionTorp":    {mrng(HBMissileVelocity, lgSiege, []int64{abPirate}, 0)},
	"eliteReconBonusHeavyLauncherROF1":   {lrof(lgHeavy, []int64{abEliteReconShip1}, SkillReconShips)},

	// ── shield resistances ───────────────────────────────────────────────
	"shipShieldEMResistanceCF2":        {res(HBShieldResist, DTEm, []int64{abCF}, SkillCaldariFrigate)},
	"shipShieldExplosiveResistanceCF2": {res(HBShieldResist, DTExplosive, []int64{abCF}, SkillCaldariFrigate)},
	"shipShieldKineticResistanceCF2":   {res(HBShieldResist, DTKinetic, []int64{abCF}, SkillCaldariFrigate)},
	"shipShieldThermalResistanceCF2":   {res(HBShieldResist, DTThermal, []int64{abCF}, SkillCaldariFrigate)},
	"shipShieldEMResistanceCC2":        {res(HBShieldResist, DTEm, []int64{abCC2}, SkillCaldariCruiser)},
	"shipShieldExplosiveResistanceCC2": {res(HBShieldResist, DTExplosive, []int64{abCC2}, SkillCaldariCruiser)},
	"shipShieldKineticResistanceCC2":   {res(HBShieldResist, DTKinetic, []int64{abCC2}, SkillCaldariCruiser)},
	"shipShieldThermalResistanceCC2":   {res(HBShieldResist, DTThermal, []int64{abCC2}, SkillCaldariCruiser)},
	"shipShieldEmResistanceCBC2":        {res(HBShieldResist, DTEm, []int64{abBC2}, SkillBattlecruisers)},
	"shipShieldExplosiveResistanceCBC2": {res(HBShieldResist, DTExplosive, []int64{abBC2}, SkillBattlecruisers)},
	"shipShieldKineticResistanceCBC2":   {res(HBShieldResist, DTKinetic, []int64{abBC2}, SkillBattlecruisers)},
	"shipShieldThermalResistanceCBC2":   {res(HBShieldResist, DTThermal, []int64{abBC2}, SkillBattlecruisers)},
	"shipBonusEmShieldResistanceCB2":        {res(HBShieldResist, DTEm, []int64{ab2CB}, SkillCaldariBattleship)},
	"shipBonusExplosiveShieldResistanceCB2": {res(HBShieldResist, DTExplosive, []int64{ab2CB}, SkillCaldariBattleship)},
	"shipBonusKineticShieldResistanceCB2":   {res(HBShieldResist, DTKinetic, []int64{ab2CB}, SkillCaldariBattleship)},
	"shipBonusThermicShieldResistanceCB2":   {res(HBShieldResist, DTThermal, []int64{ab2CB}, SkillCaldariBattleship)},

	// ── armor resistances ────────────────────────────────────────────────
	"shipArmorEMResistanceAF1": {res(HBArmorResist, DTEm, []int64{abAF}, SkillAmarrFrigate)},
	"shipArmorEXResistanceAF1": {res(HBArmorResist, DTExplosive, []int64{abAF}, SkillAmarrFrigate)},
	"shipArmorKNResistanceAF1": {res(HBArmorResist, DTKinetic, []int64{abAF}, SkillAmarrFrigate)},
	"shipArmorTHResistanceAF1": {res(HBArmorResist, DTThermal, []int64{abAF}, SkillAmarrFrigate)},
	"shipArmorEmResistanceAC2":        {res(HBArmorResist, DTEm, []int64{abAC2}, SkillAmarrCruiser)},
	"shipArmorExplosiveResistanceAC2": {res(HBArmorResist, DTExplosive, []int64{abAC2}, SkillAmarrCruiser)},
	"shipArmorKineticResistanceAC2":   {res(HBArmorResist, DTKinetic, []int64{abAC2}, SkillAmarrCruiser)},
	"shipArmorThermalResistanceAC2":   {res(HBArmorResist, DTThermal, []int64{abAC2}, SkillAmarrCruiser)},
	"shipArmorEMResistanceABC1":        {res(HBArmorResist, DTEm, []int64{abBC1}, SkillBattlecruisers)},
	"shipArmorExplosiveResistanceABC1": {res(HBArmorResist, DTExplosive, []int64{abBC1}, SkillBattlecruisers)},
	"shipArmorKineticResistanceABC1":   {res(HBArmorResist, DTKinetic, []int64{abBC1}, SkillBattlecruisers)},
	"shipArmorThermResistanceABC1":     {res(HBArmorResist, DTThermal, []int64{abBC1}, SkillBattlecruisers)},
	"shipBonusArmorResistAB": {
		res(HBArmorResist, DTEm, []int64{abAB}, SkillAmarrBattleship),
		res(HBArmorResist, DTExplosive, []int64{abAB}, SkillAmarrBattleship),
		res(HBArmorResist, DTKinetic, []int64{abAB}, SkillAmarrBattleship),
		res(HBArmorResist, DTThermal, []int64{abAB}, SkillAmarrBattleship),
	},

	// ── hull stats ───────────────────────────────────────────────────────
	"shipArmorHpAC2":      {stat(HBArmorHP, []int64{abAC2}, SkillAmarrCruiser)},
	"shipShieldCapBonusCF": {stat(HBShieldHP, []int64{abCF}, SkillCaldariFrigate)},
	"shipBonusCapCapAB":   {stat(HBCapCapacity, []int64{abAB2}, SkillAmarrBattleship)},
	"eliteBonusGunshipCapRecharge2": {{Target: HBCapRecharge, Dmg: DTAll, Attrs: []int64{abEliteGunship2}, SkillID: SkillAssaultShips, Invert: true}},
	"shipCargoBonusAF":    {stat(HBCargo, []int64{abAF}, SkillAmarrFrigate)},
	"shipCargoBonusCF":    {stat(HBCargo, []int64{abCF}, SkillCaldariFrigate)},
	"shipCargoBonusGF":    {stat(HBCargo, []int64{abGF2}, SkillGallenteFrigate)},
	"shipCargoBonusMF":    {stat(HBCargo, []int64{abMF}, SkillMinmatarFrigate)},
	"shipCargoBonusMF2":   {stat(HBCargo, []int64{abMF2}, SkillMinmatarFrigate)},
	"shipCargoBonusAI":    {stat(HBCargo, []int64{abAI}, SkillAmarrIndustrial)},
	"shipBonusCargoCI":    {stat(HBCargo, []int64{abCI}, SkillCaldariIndustrial)},
	"shipBonusCargo2GI":   {stat(HBCargo, []int64{abGI, abGI2}, SkillGallenteIndustrial)},
	"shipBonusCargoMI":    {stat(HBCargo, []int64{abMI}, SkillMinmatarIndustrial)},
	"shipCargoCapacityGC2": {stat(HBCargo, []int64{abGC2}, SkillGallenteCruiser)},
	"shipVelocityBonusAI":  {stat(HBVelocity, []int64{abAI}, SkillAmarrIndustrial)},
	"shipBonusVelocityCI":  {stat(HBVelocity, []int64{abCI}, SkillCaldariIndustrial)},
	"shipBonusVelocityGI":  {stat(HBVelocity, []int64{abGI, abGI2}, SkillGallenteIndustrial)},
	"shipVelocityBonusMI":  {stat(HBVelocity, []int64{abMI}, SkillMinmatarIndustrial)},
	"shipBonusVelocityMI":  {stat(HBVelocity, []int64{abMI}, SkillMinmatarIndustrial)},
	"shipVelocityBonusMF":  {stat(HBVelocity, []int64{abMF}, SkillMinmatarFrigate)},
	"shipVelocityBonusMC":  {stat(HBVelocity, []int64{abMC}, SkillMinmatarCruiser)},
	"shipMaxVelocityMC2":   {stat(HBVelocity, []int64{abMC2}, SkillMinmatarCruiser)},
	"shipTargetRangeBonusCF":    {stat(HBTargetRange, []int64{abCF}, SkillCaldariFrigate)},
	"shipTargetRangeBonusGF":    {stat(HBTargetRange, []int64{abGF}, SkillGallenteFrigate)},
	"shipTargetRangeBonusMF":    {stat(HBTargetRange, []int64{abMF}, SkillMinmatarFrigate)},
	"shipTargetingRangeBonusAF": {stat(HBTargetRange, []int64{abAF}, SkillAmarrFrigate)},

	// ── local repair amounts ─────────────────────────────────────────────
	"shipShieldBoostCC2":            {stat(HBShieldBoostAmount, []int64{abCC2}, SkillCaldariCruiser)},
	"shipShieldBoostMBC1":           {stat(HBShieldBoostAmount, []int64{abBC1}, SkillBattlecruisers)},
	"shipBonusShieldBonusCB":        {stat(HBShieldBoostAmount, []int64{abCB}, SkillCaldariBattleship)},
	"eliteBonusGunshipShieldBoost2": {stat(HBShieldBoostAmount, []int64{abEliteGunship2}, SkillAssaultShips)},
	"eliteIndustrialShieldBoostAmountElite1":  {stat(HBShieldBoostAmount, []int64{abEliteIndustrial1}, SkillTransportShips)},
	"eliteIndustrialShieldBoostCapNeedElite2": {stat(HBShieldBoostCapNeed, []int64{abEliteIndustrial2}, SkillTransportShips)},
	"shipArmorRepairingBC1":  {stat(HBArmorRepairAmount, []int64{abBC1}, SkillBattlecruisers)},
	"shipArmorRepairingGBC2": {stat(HBArmorRepairAmount, []int64{abBC2}, SkillBattlecruisers)},
	"eliteIndustrialArmorRepairAmountElite1":   {stat(HBArmorRepairAmount, []int64{abEliteIndustrial1}, SkillTransportShips)},
	"eliteIndustrialArmorRepairCapNeedElite1":  {stat(HBArmorRepairCapNeed, []int64{abEliteIndustrial1}, SkillTransportShips)},
	"eliteIndustrialArmorRepairDurationElite2": {stat(HBArmorRepairDuration, []int64{abEliteIndustrial2}, SkillTransportShips)},

	// ── mining yield ─────────────────────────────────────────────────────
	"shipMiningBonusAF":  {stat(HBMiningYield, []int64{ab2AF}, SkillAmarrFrigate)},
	"shipMiningBonusCF":  {stat(HBMiningYield, []int64{abCF2}, SkillCaldariFrigate)},
	"shipMiningBonusGF":  {stat(HBMiningYield, []int64{abGF}, SkillGallenteFrigate)},
	"shipMiningBonusMF":  {stat(HBMiningYield, []int64{abMF}, SkillMinmatarFrigate)},
	"shipMiningYieldCC2": {stat(HBMiningYield, []int64{abCC2}, SkillCaldariCruiser)},
	"shipMiningYieldMC2": {stat(HBMiningYield, []int64{abMC2}, SkillMinmatarCruiser)},
	"shipMiningBonusORE2":        {stat(HBMiningYield, []int64{abORE2}, SkillMiningBarge)},
	"eliteBonusBargeMiningYield": {stat(HBMiningYield, []int64{abEliteBarge1}, SkillExhumers)},
	"shipSpecialityMining":       {stat(HBMiningYield, []int64{abGF, abCF, abMF, abAF}, SkillMiningFrigate)},
}
