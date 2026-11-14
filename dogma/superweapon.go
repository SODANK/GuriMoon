package dogma

// Superweapon (doomsday) fitting and cycle rules. The shipped doomsdays and two dev types are
// invGroups 588 "Super Weapon" with no canFitShipType/canFitShipGroup attribute. The only fitting
// gates are 100,000 MW powergrid, the CPU rule below, and two skills (Doomsday Operation plus the
// racial Titan skill), so a doomsday fits any hull that carries the grid.

// SuperWeaponCPUUsage returns the CPU a module actually draws, applying dgmEffects 2436
// cpuBonusToSelf. It gates on attribute 1081, not a dgmTypeEffects lookup: the two sets are the
// same six group-588 types and the attribute is already in every caller's map. The raw cpu(50) is
// 10,000 tf, unfittable on any hull; scaled by 0.02 it is 200 tf.
func SuperWeaponCPUUsage(attrs map[int64]float64) float64 {
	cpu := attrs[AttrCPUUsage]
	if bonus := attrs[AttrCPUBonusSuperWeapon]; bonus > 0 {
		return cpu * bonus
	}
	return cpu
}

// IsSuperWeaponEffect reports whether effectName is the doomsday effect.
func IsSuperWeaponEffect(effectName string) bool {
	return effectName == EffNameEmpWaveGrid
}
