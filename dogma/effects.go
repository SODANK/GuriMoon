package dogma

// Effect ID constants from dgmEffects.
const (
	EffectLoPower        int64 = 11
	EffectHiPower        int64 = 12
	EffectMedPower       int64 = 13
	EffectOnline         int64 = 16
	EffectLauncherFitted int64 = 40
	EffectTurretFitted   int64 = 42

	// Superweapon CPU effects. 2436 multiplies the doomsday's own cpu(50) by
	// cpuBonusSuperWeapon(1081) = 0.02, so a nominal 10,000 tf fits a real hull. 2435, a x50
	// penalty on other superweapons, never fires: maxGroupOnline(978) = 1 on group 588.
	EffectCPUPenaltyToSuperWeapons int64 = 2435
	EffectCPUBonusToSelf           int64 = 2436
)

// Effect name constants (effectName column in dgmEffects).
const (
	EffNameOnline                = "online"
	EffNameMiningLaser           = "miningLaser"
	EffNameSurveyScan            = "surveyScan"
	EffNameLaserFired            = "laserFired"
	EffNameProjectileFired       = "projectileFired"
	EffNameHybridFired           = "hybridFired"
	EffNameMissileLaunching      = "missileLaunching"
	EffNameUseMissiles           = "useMissiles"
	EffNameShieldBoosting        = "shieldBoosting"
	EffNameEnergyTransfer        = "energyTransfer"
	EffNameEnergyDestabilization = "energyDestabilization"
	EffNameEnergyVampire         = "energyVampire"
	EffNameShieldTransfer        = "shieldTransfer"
	EffNameWarpScramble          = "warpScramble"
	EffNameWarpScrambler         = "warpScrambler"
	EffNameDecreaseTargetSpeed   = "decreaseTargetSpeed"
	EffNameModifyTargetSpeed     = "modifyTargetSpeed"
	EffNameCloaking              = "cloaking"
	// cloakingWarpSafe is the covert ops cloak (effect 980): the same cloak, with isWarpSafe
	// set, so the ship may warp under it.
	EffNameCloakingWarpSafe = "cloakingWarpSafe"
	EffNameEmpWave          = "empWave"
	// empWaveGrid is the doomsday: effectCategory 1 like empWave (no target), with no
	// range attribute because the blast is the whole grid.
	EffNameEmpWaveGrid           = "empWaveGrid"
	// energyDestabilizationNew is a second neutralizer row carried by some RMR modules;
	// same attributes, same math.
	EffNameEnergyDestabNew = "energyDestabilizationNew"
	// energyNosferatu is the default effect of every player Nosferatu (group 68);
	// energyVampire sits only on the cap-drain drones. Both feed the attacker.
	EffNameEnergyNosferatu = "energyNosferatu"

	// Capital mobility. Both are effectCategory 1 (no target) and arrive through the
	// untargeted Activate path the smartbomb and doomsday use.
	//
	// Match on the effect, not the invGroup: Cynosural Field Generator I (21096) is in
	// group 330 "Cloaking Device".
	EffNameCynoField  = "cynosuralGeneration"  // effect 1729, no guid
	EffNameJumpPortal = "jumpPortalGeneration" // effect 2152, guid effects.JumpPortal

	// Targeted EWAR (chance-based, dgmEffects rangeChance=1).
	EffNameECMJam          = "ewTestEffectJam"                                     // ECM jammers, effect 1358
	EffNameECMBurst        = "ecmBurst"                                            // ECM Burst AoE, effect 53
	EffNameTrackingDisrupt = "turretWeaponRangeTrackingSpeedMultiplyTargetHostile" // effect 609
	EffNameSensorDamp      = "sensorBoostTargetedHostile"                          // effect 837
	EffNameTargetPaint     = "ewTargetPaint"                                       // effect 1549
)

// FX GUID constants (client-side effect visual identifiers).
const (
	// effects.Mining is the drone mining beam (client droneMining.py, MiningDrone objects
	// only). Ship lasers and strip miners render through effects.Laser (dgmEffects 67).
	FXGUIDMining            = "effects.Mining"
	FXGUIDLaser             = "effects.Laser"
	FXGUIDSurveyScan        = "effects.SurveyScan"
	FXGUIDProjectileFired   = "effects.ProjectileFired"
	FXGUIDHybridFired       = "effects.HybridFired"
	FXGUIDMissileDeployment = "effects.MissileDeployment"
	FXGUIDShieldBoosting    = "effects.ShieldBoosting"
	FXGUIDEnergyTransfer    = "effects.EnergyTransfer"
	FXGUIDEnergyVampire     = "effects.EnergyVampire"
	FXGUIDShieldTransfer    = "effects.ShieldTransfer"
	FXGUIDWarpScramble      = "effects.WarpScramble"
	FXGUIDModifyTargetSpeed = "effects.ModifyTargetSpeed"
	FXGUIDCloaking          = "effects.Cloaking"
	FXGUIDEMPWave           = "effects.EMPWave"
	// The client builds the EMPWaveGrid model path from the module's graphicID, which is a
	// bare stem (IonStorm, MissileStrike, Singularity, Sunburst) on the four doomsday rows,
	// so the effect is per race with nothing extra on the wire.
	FXGUIDEMPWaveGrid       = "effects.EMPWaveGrid"
	FXGUIDGenericEffect     = "effects.GenericEffect"
	// ECM jam beam. tactical.py red-boxes this GUID, and it triggers the client's
	// "TargetJammedBy" message.
	FXGUIDECMJam         = "effects.ScanStrengthBonusTarget"
	FXGUIDEwarTargetBeam = "effects.ElectronicAttributeModifyTarget" // TD / sensor damp beam
	FXGUIDTargetPaint    = "effects.TargetPaint"
	// Self-centred ECM flare (client ECM.py). There is no effects.ECMBurst class, so the
	// burst borrows it.
	FXGUIDECMActivate = "effects.ElectronicAttributeModifyActivate"

	// Thruster flare and engine audio burst (client effects2/boosters.py); speedBoost
	// normalizes onto it. FitBoosters leaves engines silent, so this is the only thing that
	// makes one audible outside a manual nav command.
	FXGUIDBoosterShort = "effects.BoosterShort"
	// The inverse: flare and engine audio down to nothing in a second. It also undoes the
	// -2.0 clamp effects.CloakRegardless leaves on the curve, which effects.Uncloak does
	// not restore (gainCurve).
	FXGUIDBoosterStop = "effects.BoosterStop"
)

// NormalizeClientGUID maps dgmEffects GUIDs with no class in the client's effects2
// registry onto the nearest visual it has; other GUIDs pass through. An unknown GUID
// logs "Effect GUID <x> not found." and drops the FX, as every afterburner/MWD would.
func NormalizeClientGUID(guid string) string {
	switch guid {
	case "effects.SpeedBoost":
		return FXGUIDBoosterShort // the prop-mod visual
	case "effects.TorpedoDeployment":
		return FXGUIDMissileDeployment
	case "effects.Barrage":
		return FXGUIDProjectileFired
	case "effects.ArmorHardening", "effects.DecloakWave":
		return FXGUIDGenericEffect
	case "effects.ECMBurst":
		// No ECMBurst class; the ECM flare is centred on the ship, as a burst is.
		return FXGUIDECMActivate
	default:
		return guid
	}
}

// FallbackGUID maps an effect name to a client FX GUID and offensive flag
// when the database has no GUID entry for the effect.
func FallbackGUID(effectName string) (guid string, isOffensive bool) {
	switch effectName {
	case EffNameMiningLaser:
		// Ship-fitted miners use the turret laser; effects.Mining is drone-only.
		return FXGUIDLaser, false
	case EffNameSurveyScan:
		return FXGUIDSurveyScan, false
	case EffNameLaserFired:
		return FXGUIDLaser, true
	case EffNameProjectileFired:
		return FXGUIDProjectileFired, true
	case EffNameHybridFired:
		return FXGUIDHybridFired, true
	case EffNameMissileLaunching, EffNameUseMissiles:
		return FXGUIDMissileDeployment, true
	case EffNameShieldBoosting:
		return FXGUIDShieldBoosting, false
	case EffNameEnergyTransfer:
		return FXGUIDEnergyTransfer, false
	case EffNameEnergyDestabilization, EffNameEnergyDestabNew, EffNameEnergyVampire, EffNameEnergyNosferatu:
		return FXGUIDEnergyVampire, true
	case EffNameShieldTransfer:
		return FXGUIDShieldTransfer, false
	case EffNameWarpScramble, EffNameWarpScrambler:
		return FXGUIDWarpScramble, true
	case EffNameDecreaseTargetSpeed, EffNameModifyTargetSpeed:
		return FXGUIDModifyTargetSpeed, true
	case EffNameECMJam:
		// dgmEffects row 1358 ships with an empty guid; without this fallback ECM renders as
		// GenericEffect and never red-boxes.
		return FXGUIDECMJam, true
	case EffNameTrackingDisrupt, EffNameSensorDamp:
		return FXGUIDEwarTargetBeam, true
	case EffNameTargetPaint:
		return FXGUIDTargetPaint, true
	case EffNameCloaking, EffNameCloakingWarpSafe:
		return FXGUIDCloaking, false
	case EffNameEmpWave:
		return FXGUIDEMPWave, true
	case EffNameEmpWaveGrid:
		return FXGUIDEMPWaveGrid, true
	default:
		return FXGUIDGenericEffect, false
	}
}
