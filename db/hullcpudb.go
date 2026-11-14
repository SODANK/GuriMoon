package db

import (
	"context"
	"database/sql"

	"gurimoon/dogma"
)

// GetHullCPUNeed resolves the CPU-need role bonuses of shipTypeID for charID
// (dogma.HullCPUNeedSpecs). It returns nil, which changes no need, for a hull with no such
// effect, for shipTypeID 0 and when a read fails. charID 0 counts every skill level as 0, which
// leaves the flat percentages whole and the cloak multiplier at its base.
func GetHullCPUNeed(ctx context.Context, db *sql.DB, charID, shipTypeID int64) *dogma.HullCPUNeed {
	if shipTypeID == 0 {
		return nil
	}
	effects, err := GetTypeEffects(ctx, db, shipTypeID)
	if err != nil {
		return nil
	}
	var names []string
	for _, te := range effects {
		if _, ok := dogma.HullCPUNeedSpecs[te.EffectName]; ok {
			names = append(names, te.EffectName)
		}
	}
	if len(names) == 0 {
		return nil
	}
	attrs, err := TypeAttrs(ctx, db, shipTypeID)
	if err != nil {
		return nil
	}
	levels := map[int64]int{}
	return &dogma.HullCPUNeed{
		Effects: names,
		Attrs:   attrs,
		Level: func(skillID int64) int {
			if charID == 0 {
				return 0
			}
			lvl, cached := levels[skillID]
			if !cached {
				lvl, _ = GetCharacterSkillLevel(ctx, db, charID, skillID)
				levels[skillID] = lvl
			}
			return lvl
		},
	}
}

// typeGroupID is a type's invGroup, 0 when the read fails.
func typeGroupID(ctx context.Context, db *sql.DB, typeID int64) int64 {
	groupID, _, err := GetTypeGroupCategory(ctx, db, typeID)
	if err != nil {
		return 0
	}
	return groupID
}
