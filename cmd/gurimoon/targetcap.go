// Maximum locked targets: the lower of the hull's limit and the pilot's own.
package main

import (
	"context"
	"database/sql"

	evedb "gurimoon/db"
	"gurimoon/marshal"
)

const (
	attrMaxLockedTargets = 192 // maxLockedTargets, on hulls and on the character

	typeTargetingSkill    = 3429 // +1 locked target per level (attr 311)
	typeMultitaskingSkill = 3430 // +1 locked target per level (attr 311)

	// charBaseLockedTargets is attr 192 of every character type.
	charBaseLockedTargets = 2
)

// charMaxLockedTargets is the pilot's own limit: the character base plus one per level of
// Targeting and Multitasking.
func charMaxLockedTargets(levels func(skillTypeID int64) int) int {
	return charBaseLockedTargets + levels(typeTargetingSkill) + levels(typeMultitaskingSkill)
}

// maxLockedTargets combines the hull's attr 192 with the pilot's limit. A hull with no value
// leaves the pilot's limit alone.
func maxLockedTargets(shipMax float64, charMax int) int {
	if s := int(shipMax); s > 0 && s < charMax {
		return s
	}
	return charMax
}

// lockedTargetLimits reads both limits for a pilot in a hull. A failed skill read counts no
// skill levels.
func lockedTargetLimits(ctx context.Context, sqldb *sql.DB, charID, shipTypeID int64) (limit, charMax int) {
	levels, _ := evedb.GetCharacterSkillLevels(ctx, sqldb, charID)
	charMax = charMaxLockedTargets(func(s int64) int { return levels[s] })
	var shipMax float64
	if shipTypeID != 0 {
		if attrs, err := evedb.TypeAttrs(ctx, sqldb, shipTypeID); err == nil {
			shipMax = attrs[attrMaxLockedTargets]
		}
	}
	return maxLockedTargets(shipMax, charMax), charMax
}

// charLimitAttrs is the character's own skill-set limits as the attributes the client reads off
// the character item: maxActiveDrones for the drone window header and maxLockedTargets for
// auto-target-back.
func charLimitAttrs(ctx context.Context, sqldb *sql.DB, charID int64) map[int64]float64 {
	_, charMax := lockedTargetLimits(ctx, sqldb, charID, 0)
	return map[int64]float64{
		attrMaxActiveDrones:  float64(droneMaxActive(ctx, sqldb, charID)),
		attrMaxLockedTargets: float64(charMax),
	}
}

// pyTruthy reads a flag the client sends as a Python bool or an int. Anything else is false.
func pyTruthy(v marshal.PyRep) bool {
	switch b := v.(type) {
	case marshal.PyBool:
		return b.Value
	case marshal.PyInt:
		return b.Value != 0
	}
	return false
}
