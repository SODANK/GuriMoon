package main

import (
	"context"
	"database/sql"
	"log"
	"time"

	evedb "gurimoon/db"
	"gurimoon/dogma"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
	"gurimoon/server/ram"
)

// Effective weapon attributes on the client (see db/moduleeffective.go). The client displays what
// the server sends, so ShipGetInfo and ItemGetInfo answer with folded values and pushWeaponAttrs
// re-sends them after ammo, fitting, onlining, tracking computer, skill or implant changes.

// trackingComputerGroup is invGroups 213: active, and counted by
// GetTurretTrackingMults only while running.
const trackingComputerGroup = 213

// weaponAttrIDs are every module attribute EffectiveWeaponAttrsFrom can move;
// a push sends each one the module carries, folded or not, so a value whose
// multiplier fell back to 1 (ammo unloaded) returns to base on the client.
var weaponAttrIDs = []int64{
	dogma.AttrMaxRange, dogma.AttrFalloff, dogma.AttrTrackingSpeed,
	dogma.AttrSpeed, dogma.AttrDamageMultiplier, dogma.AttrCapacitorNeed,
}

// needAttrIDs are a module's own CPU and powergrid need, which the fitting
// skills lower on every module (moduleneed.go).
var needAttrIDs = []int64{dogma.AttrCPUUsage, dogma.AttrPowerConsumption}

// chargeAttrIDs are the charge attributes it can move (missile range).
var chargeAttrIDs = []int64{dogma.AttrMaxVelocity, dogma.AttrExplosionDelay}

// activeEffectIDs is the set of the ship's running modules other than the persistent online
// effect; GetTurretTrackingMults uses it to decide whether a tracking computer counts.
// ds.activeModules belongs to the connection goroutine, so call this only from there.
func (ds *destinyConn) activeEffectIDs() map[int64]bool {
	out := map[int64]bool{}
	if ds == nil {
		return out
	}
	for id, m := range ds.activeModules {
		if m != nil && m.effectName != "online" {
			out[id] = true
		}
	}
	return out
}

// fittedWeaponOverrides answers ItemGetInfo for an item in a module slot: the
// folded attributes of the weapon itself, or of the charge loaded in one.
func fittedWeaponOverrides(ctx context.Context, sqldb *sql.DB, charID int64, ds *destinyConn, er *evedb.EntityRow) map[int64]float64 {
	if sqldb == nil || er == nil {
		return nil
	}
	ship, err := evedb.GetEntityRow(ctx, sqldb, er.LocationID)
	if err != nil || ship == nil {
		return nil
	}
	var active map[int64]bool
	if ds != nil && ds.shipID == er.LocationID {
		active = ds.activeEffectIDs()
	}
	if er.CategoryID == 8 {
		// A charge: fold against the module sharing its slot.
		mods, err := evedb.GetFittedModules(ctx, sqldb, er.LocationID)
		if err != nil {
			return nil
		}
		for _, m := range mods {
			if m.Flag == er.Flag {
				_, chargeOut := evedb.GetEffectiveWeaponAttrs(ctx, sqldb, charID, er.LocationID, ship.TypeID, m.TypeID, er.TypeID, active)
				return chargeOut
			}
		}
		return nil
	}
	chargeType, _ := evedb.GetChargeTypeID(ctx, sqldb, er.LocationID, er.Flag)
	modOut, _ := evedb.GetEffectiveWeaponAttrs(ctx, sqldb, charID, er.LocationID, ship.TypeID, er.TypeID, chargeType, active)
	if base, err := evedb.TypeAttrs(ctx, sqldb, er.TypeID); err == nil {
		need := map[int64]float64{}
		for _, a := range needAttrIDs {
			if v, ok := base[a]; ok {
				need[a] = v
			}
		}
		cm, pm := evedb.ModuleNeedMultsResolver(ctx, sqldb, ship.OwnerID, ship.TypeID)(er.TypeID)
		evedb.ApplyModuleNeedMults(need, cm, pm)
		for k, v := range need {
			modOut[k] = v
		}
	}
	return modOut
}

// pushWeaponAttrs re-sends every fitted module's CPU/PG need and every weapon's and loaded
// charge's folded attributes as one OnMultiEvent of OnModuleAttributeChange tuples. active is the
// running-module set (ds.activeEffectIDs on the connection goroutine, nil elsewhere). Errors are
// logged; a missed push leaves the old numbers until the next ShipGetInfo.
func pushWeaponAttrs(c *evenet.Conn, accountID, charID, shipID int64, sqldb *sql.DB, active map[int64]bool) {
	if c == nil || sqldb == nil || charID == 0 || shipID == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ship, err := evedb.GetEntityRow(ctx, sqldb, shipID)
	if err != nil || ship == nil || ship.CategoryID != 6 {
		return
	}
	mods, err := evedb.GetFittedModules(ctx, sqldb, shipID)
	if err != nil {
		log.Printf("pushWeaponAttrs: ship %d: %v", shipID, err)
		return
	}
	ts := server.Win32TimeNowRep()
	var events []marshal.PyRep
	needMults := evedb.ModuleNeedMultsResolver(ctx, sqldb, ship.OwnerID, ship.TypeID)
	add := func(itemID, attrID int64, v float64) {
		events = append(events, marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyString{Value: "OnModuleAttributeChange"},
			marshal.PyInt{Value: uint64(charID)},
			marshal.PyInt{Value: uint64(itemID)},
			marshal.PyInt{Value: uint64(attrID)},
			ts,
			marshal.PyReal{Value: v},
			marshal.PyReal{Value: v},
		}})
	}
	for _, m := range mods {
		// Every module: its CPU/PG need under the owner's fitting skills.
		if base, err := evedb.TypeAttrs(ctx, sqldb, m.TypeID); err == nil {
			need := map[int64]float64{}
			for _, a := range needAttrIDs {
				if v, ok := base[a]; ok {
					need[a] = v
				}
			}
			cm, pm := needMults(m.TypeID)
			evedb.ApplyModuleNeedMults(need, cm, pm)
			for _, a := range needAttrIDs {
				if v, ok := need[a]; ok {
					add(m.ItemID, a, v)
				}
			}
		}
		chargeItem, chargeType, _ := evedb.GetChargeItem(ctx, sqldb, shipID, m.Flag)
		f, ok, ferr := evedb.GetWeaponFold(ctx, sqldb, charID, shipID, ship.TypeID, m.TypeID, chargeType, active)
		if ferr != nil || !ok {
			continue
		}
		modBase, err := evedb.TypeAttrs(ctx, sqldb, m.TypeID)
		if err != nil {
			continue
		}
		var chargeBase map[int64]float64
		if chargeType != 0 {
			chargeBase, _ = evedb.TypeAttrs(ctx, sqldb, chargeType)
		}
		modOut, chargeOut := evedb.EffectiveWeaponAttrsFrom(modBase, chargeBase, f)
		for _, a := range weaponAttrIDs {
			base, has := modBase[a]
			if !has {
				continue
			}
			if v, ok := modOut[a]; ok {
				base = v
			}
			add(m.ItemID, a, base)
		}
		if chargeItem != 0 && !f.IsTurret {
			for _, a := range chargeAttrIDs {
				base, has := chargeBase[a]
				if !has {
					continue
				}
				if v, ok := chargeOut[a]; ok {
					base = v
				}
				add(chargeItem, a, base)
			}
		}
	}
	if len(events) == 0 {
		return
	}
	args := marshal.PyTuple{Items: []marshal.PyRep{marshal.PyList{Items: events}}}
	if err := server.SendNotification(c, accountID, "OnMultiEvent", "charid", args); err != nil {
		log.Printf("pushWeaponAttrs: %v", err)
	}
}

// pushWeaponAttrsForChar is pushWeaponAttrs for callers that hold only the pilot (skill
// completion, implant change), off the connection goroutine. Running tracking computers are not
// known there; their own start/stop pushes carry them.
func pushWeaponAttrsForChar(c *evenet.Conn, accountID, charID int64, sqldb *sql.DB) {
	if c == nil || sqldb == nil || charID == 0 {
		return
	}
	// The character's own industry multipliers move with the same skills.
	ictx, icancel := context.WithTimeout(context.Background(), 3*time.Second)
	for attrID, v := range industryCharAttrs(ictx, sqldb, charID) {
		sendModuleAttributeChange(c, accountID, charID, charID, attrID, v, v)
	}
	// So do the drone and locked-target limits.
	for attrID, v := range charLimitAttrs(ictx, sqldb, charID) {
		sendModuleAttributeChange(c, accountID, charID, charID, attrID, v, v)
	}
	icancel()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	shipID, err := evedb.GetCurrentShipID(ctx, sqldb, charID)
	cancel()
	if err != nil || shipID == 0 {
		return
	}
	// A fitting skill also moves the ship's CPU/PG load.
	sendShipFittingStats(c, accountID, charID, shipID, sqldb)
	pushWeaponAttrs(c, accountID, charID, shipID, sqldb, nil)
}

// industryCharAttrs is ram.IndustryCharAttrs for one pilot: the skill terms
// the S&I formulas apply, as the character attributes the blueprint window
// reads.
func industryCharAttrs(ctx context.Context, sqldb *sql.DB, charID int64) map[int64]float64 {
	levels, err := evedb.GetCharacterSkillLevels(ctx, sqldb, charID)
	if err != nil {
		return nil
	}
	return ram.IndustryCharAttrs(func(s int64) int { return levels[s] })
}
