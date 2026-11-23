// Warp capacitor cost.
//
// The RMR client holds no warp-cap formula: WarpToStuff is a bare remote call, and the cost,
// the drain and the shortened warp are all the server's. The inputs are static data:
// warpCapacitorNeed (attr 153) is GJ per kg of ship mass per AU, scaled by Warp Drive
// Operation and hardwirings (attr 319, db.GetWarpCapNeedMultiplier).
//
//	cost (GJ) = mass (kg) × warpCapacitorNeed × skill/implant multiplier × distance (AU)
//
// mass is the hull plus its online armor plates (attr 796). A running prop mod's mass is not
// counted: entering warp switches it off.
//
// A capacitor that cannot cover the whole warp takes the ship as far as it can, which is
// the client's own WarpingWithAvailablePower message. One that cannot reach the minimum
// warp distance does not warp at all (NotEnoughEnergy).
package main

import (
	"context"
	"database/sql"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/dogma"
)

// warpCapMode says whether a startPlayerWarp call is paid from the ship's capacitor.
type warpCapMode bool

const (
	// warpPaysCap is every warp the pilot's own drive makes: warp-to, bookmarks, gang and
	// journal warps.
	warpPaysCap warpCapMode = true
	// warpCapFree is a warp something else powers: the acceleration gate.
	warpCapFree warpCapMode = false
)

// warpCapEffectName is the effect NotEnoughEnergy names on a refused warp.
const warpCapEffectName = "Warp Drive"

// warpCapPerMeter is the capacitor (GJ) one metre of warp costs a ship of massKg whose
// effective warpCapacitorNeed is needPerKgAU.
func warpCapPerMeter(massKg, needPerKgAU float64) float64 {
	if massKg <= 0 || needPerKgAU <= 0 {
		return 0
	}
	return massKg * needPerKgAU / metersPerAU
}

// warpCapPlan is what a warp of a given length does to the capacitor.
type warpCapPlan struct {
	// cost is the charge drained when the ship enters warp.
	cost float64
	// short: the capacitor covers only reach metres, so the landing is pulled in.
	short bool
	reach float64
	// refuse: the capacitor cannot reach even minWarpDistanceM. need is the cost of that
	// minimum, got the charge there is.
	refuse    bool
	need, got float64
}

// planWarpCap sizes a warp of distM metres against a capacitor holding cur GJ, at perMeter
// GJ per metre. A free warp (perMeter or distM zero) costs nothing.
func planWarpCap(perMeter, cur, distM float64) warpCapPlan {
	if perMeter <= 0 || distM <= 0 {
		return warpCapPlan{}
	}
	cost := perMeter * distM
	if cost <= cur {
		return warpCapPlan{cost: cost}
	}
	reach := cur / perMeter
	if reach < minWarpDistanceM {
		return warpCapPlan{refuse: true, need: perMeter * minWarpDistanceM, got: cur}
	}
	return warpCapPlan{cost: cur, short: true, reach: reach}
}

// planPlayerWarpCap sizes the pilot's warp from dm.Pos to landing against the ship's
// capacitor now. A ship with no capacitor or no warpCapacitorNeed warps free.
func planPlayerWarpCap(ds *destinyConn, sqldb *sql.DB, dm *destiny.Manager, landing destiny.Vec3) warpCapPlan {
	if ds == nil || ds.entity == nil || dm == nil || !ds.entity.HasCap() {
		return warpCapPlan{}
	}
	perMeter := warpCapPerMeter(warpCapMass(ds, sqldb, dm.Mass()), shipWarpCapNeed(ds, sqldb))
	cur, _ := ds.entity.CurrentCap()
	return planWarpCap(perMeter, cur, landing.Sub(dm.Pos).Len())
}

// warpCapMass is the mass the warp drive pays for: the hull (hullMass, the destiny manager's
// invTypes mass) plus its online armor plates. A failed lookup charges the hull alone.
func warpCapMass(ds *destinyConn, sqldb *sql.DB, hullMass float64) float64 {
	if sqldb == nil || ds.shipID == 0 {
		return hullMass
	}
	plates, err := evedb.GetShipPlateMass(context.Background(), sqldb, ds.shipID)
	if err != nil {
		return hullMass
	}
	return hullMass + plates
}

// shipWarpCapNeed is the pilot's ship's warpCapacitorNeed with skill and implants applied.
func shipWarpCapNeed(ds *destinyConn, sqldb *sql.DB) float64 {
	if sqldb == nil || ds.shipTypeID == 0 {
		return 0
	}
	ctx := context.Background()
	attrs, err := evedb.TypeAttrs(ctx, sqldb, ds.shipTypeID)
	if err != nil {
		return 0
	}
	base := attrs[dogma.AttrWarpCapacitorNeed]
	if base <= 0 {
		return 0
	}
	return base * evedb.GetWarpCapNeedMultiplier(ctx, sqldb, ds.charID)
}
