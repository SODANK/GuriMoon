package db

import (
	"context"
	"database/sql"
	"fmt"
	"math"
)

// Starbase defenses and the force field.
//
// A tower's force field blocks targeting in both directions, so sentries and EW batteries must
// stand outside it. The client checks nothing (playerOwned.py CheckAnchoringPosition only finds
// a tower in range), so this is the gate for posMgr.AnchorStructure, AnchorSlaveFor and the
// template stamp. Only anchoring is gated.

// DefaultTowerShieldRadius is the force-field radius used when a tower type
// carries no shieldRadius attribute (30 km, every large tower in this data).
const DefaultTowerShieldRadius = 30000.0

// IsStarbaseDefenseGroup reports whether a structure group is a starbase defense (sentry gun
// or electronic warfare battery).
func IsStarbaseDefenseGroup(groupID int64) bool {
	switch groupID {
	case GroupMobileMissileSentry, GroupMobileProjectileSentry,
		GroupMobileLaserSentry, GroupMobileHybridSentry,
		439, // Electronic Warfare Battery
		440, // Sensor Dampening Battery
		441, // Stasis Webification Battery
		443: // Warp Scrambling Battery
		return true
	}
	return false
}

// TowerShieldRadius is the force-field radius of a tower type, in metres:
// its shieldRadius attribute, the same number the force-field ball is sized
// from.
func TowerShieldRadius(ctx context.Context, db *sql.DB, towerTypeID int64) float64 {
	if v, ok, err := GetTypeAttributeFloat(ctx, db, towerTypeID, "shieldRadius"); err == nil && ok && v > 0 {
		return v
	}
	return DefaultTowerShieldRadius
}

// defenseInsideForceField reports whether a structure of radius structRadius
// whose centre sits dist metres from the tower would overlap a force field of
// shieldRadius. The whole hull must be clear of the field, not just its centre.
func defenseInsideForceField(dist, shieldRadius, structRadius float64) bool {
	return dist < shieldRadius+math.Max(structRadius, 0)
}

// ForceFieldAnchorRefusal is "" when a structure of structTypeID may be
// anchored at offset (dx, dy, dz) from a tower of towerTypeID, else the
// sentence the pilot is told. Only defenses are checked; every other
// structure may sit inside the field.
func ForceFieldAnchorRefusal(ctx context.Context, db *sql.DB, towerTypeID, structTypeID int64, dx, dy, dz float64) string {
	var groupID int64
	var radius sql.NullFloat64
	var name string
	if err := db.QueryRowContext(ctx, `
		SELECT groupID, radius, typeName FROM invTypes WHERE typeID = ?`, structTypeID,
	).Scan(&groupID, &radius, &name); err != nil {
		return ""
	}
	if !IsStarbaseDefenseGroup(groupID) {
		return ""
	}
	shield := TowerShieldRadius(ctx, db, towerTypeID)
	dist := math.Sqrt(dx*dx + dy*dy + dz*dz)
	if !defenseInsideForceField(dist, shield, radius.Float64) {
		return ""
	}
	need := shield + math.Max(radius.Float64, 0)
	return fmt.Sprintf("%s cannot be anchored inside the control tower's force field: nothing inside the field can "+
		"target or be targeted. It is %.1f km from the tower and must be at least %.1f km out.",
		name, dist/1000, need/1000)
}
