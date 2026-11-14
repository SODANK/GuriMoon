package dogma

import "math"

// POS weapon arithmetic for the sentry guns and EW batteries a control tower fields. Pure
// functions so the gun loop's rules test without a grid; the formulas restate the player turret
// loop's (client GetAccuracy contract), which keeps its per-shot state inline.

// TurretHitChance is P = 0.5^[(ω/(T·sigT/sigRes))² + (max(0,d−optimal)/falloff)²].
// angular is the target's angular velocity relative to the gun (rad/s);
// tracking the gun's trackingSpeed; targetSig the target's signature radius;
// sigRes the gun's signature resolution.
func TurretHitChance(dist, optimal, falloff, angular, tracking, targetSig, sigRes float64) float64 {
	exponent := 0.0
	if optimal > 0 && dist > optimal && falloff > 0 {
		f := (dist - optimal) / falloff
		exponent += f * f
	}
	if tracking > 0 && targetSig > 0 && sigRes > 0 && angular > 0 {
		t := angular / (tracking * targetSig / sigRes)
		exponent += t * t
	}
	return math.Pow(0.5, exponent)
}

// TurretDamageMod turns one uniform roll into the damage modifier: 0 is a miss, a roll under 1%
// of the chance is a wrecking shot (x3), any other hit is roll+0.49, so a low-chance shot lands
// only a weak hit.
func TurretDamageMod(roll, hitChance float64) float64 {
	switch {
	case roll < 0.01*hitChance:
		return 3.0
	case roll <= hitChance:
		return roll + 0.49
	}
	return 0
}

// SentrySigResolution maps a POS gun's chargeSize to its signature
// resolution: small, medium, large, extra-large. POS guns ship without an
// optimalSigRadius of their own.
func SentrySigResolution(chargeSize float64) float64 {
	switch {
	case chargeSize <= 1:
		return 40
	case chargeSize <= 2:
		return 125
	case chargeSize <= 3:
		return 400
	}
	return 1000
}

// Sensor strength order used by ECMJamChance: radar, ladar, magnetometric, gravimetric.
// Attributes 208-211 on the target, 241/239/240/238 (the *StrengthBonus jammer strengths) on the jammer.
const (
	SensorRadar = iota
	SensorLadar
	SensorMagnetometric
	SensorGravimetric
)

// ECMJamChance is the chance one ECM cycle jams a target: the jammer's
// strength against the target's own sensor type (its strongest of the four,
// which is how a hull's racial sensor is identified) over the target's
// strength, capped at 1. A target with no sensor strength at all is jammed.
func ECMJamChance(jammer, sensor [4]float64) float64 {
	best, idx := 0.0, -1
	for i, s := range sensor {
		if s > best {
			best, idx = s, i
		}
	}
	if idx < 0 {
		return 1
	}
	c := jammer[idx] / best
	switch {
	case c < 0:
		return 0
	case c > 1:
		return 1
	}
	return c
}
