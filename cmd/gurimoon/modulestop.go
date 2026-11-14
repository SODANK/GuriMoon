// Forced module stops and the warp rule for running modules.
//
// dgmEffects.isWarpSafe decides which effects a warp leaves alone: hardeners, damage controls,
// repairers, capacitor and sensor boosters, tracking computers and the covert ops cloak keep
// cycling. Every other running effect interferes with the warp drive. It is switched off as
// the warp starts (EffectDeactivationWarping) and cannot be started until the warp ends
// (DeniedActivateInWarp). Propulsion modules are the one exception to the timing: they run
// through the alignment and go off as the tunnel opens (stopPropModsForWarp).
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
)

// sensorBoosterGroup is invGroups 212: active, and folded into targeting range and scan
// resolution only while running.
const sensorBoosterGroup = 212

// effectIsOffensive reports whether an effect acts on a target as a weapon or hostile module:
// the dgmEffects flag, or one of the weapon effects the dump leaves unflagged.
func effectIsOffensive(effectName string) bool {
	if ei, ok := effectMap[effectName]; ok && ei.IsOffensive {
		return true
	}
	switch effectName {
	case "laserFired", "projectileFired", "hybridFired", "missileLaunching", "useMissiles",
		"targetAttack", "smallProjectileTurret", "mediumProjectileTurret", "largeProjectileTurret",
		"smallHybridTurret", "mediumHybridTurret", "largeHybridTurret",
		"smallEnergyTurret", "mediumEnergyTurret", "largeEnergyTurret":
		return true
	}
	return false
}

// warpStopsEffect reports whether a running effect is switched off as its ship starts a warp:
// every effect aimed at a target, and every other effect dgmEffects does not mark warp safe.
// "online" is fitting state and propulsion modules stop later, when the tunnel opens. An
// effect the table does not know stops only if it is an area module or a survey scan.
func warpStopsEffect(effectName string, targetID uint32) bool {
	if effectName == dogma.EffNameOnline || isPropModEffectName(effectName) {
		return false
	}
	if targetID != 0 {
		return true
	}
	if ei, ok := effectMap[effectName]; ok {
		return !ei.WarpSafe
	}
	return isAoEEwarEffect(effectName) || effectName == dogma.EffNameSurveyScan
}

// warpDeniesActivation reports whether an effect cannot be started by a ship that is aligning
// for a warp (aligning) or inside the tunnel (inTunnel): every effect a warp start switches
// off and, in the tunnel, the propulsion modules.
func warpDeniesActivation(effectName string, targetID uint32, aligning, inTunnel bool) bool {
	if !aligning && !inTunnel {
		return false
	}
	if warpStopsEffect(effectName, targetID) {
		return true
	}
	return inTunnel && isPropModEffectName(effectName)
}

// targetIsLocked reports whether the pilot holds a completed lock on targetID.
func targetIsLocked(ds *destinyConn, targetID uint32) bool {
	ds.targetMu.Lock()
	defer ds.targetMu.Unlock()
	for _, t := range ds.targets {
		if t == targetID {
			return true
		}
	}
	return false
}

// stopModuleNow force-stops the effect a module is running: its cycle loop is cancelled, the
// client gets the stop event and FX, and everything the running effect moved is put back
// (speed boost, resistances, turret and sensor stats). It reports whether the module was
// running anything. Connection goroutine only: it writes the connection's module maps.
func stopModuleNow(c *evenet.Conn, addr string, accountID, charID int64, ds *destinyConn, modID int64, sqldb *sql.DB) bool {
	mod := ds.activeModules[modID]
	if mod == nil || mod.effectName == dogma.EffNameOnline {
		return false
	}
	if sess, ok := ds.miningSessions[modID]; ok {
		sess.cancel()
		delete(ds.miningSessions, modID)
	}
	if cancelFn, ok := ds.cancelWeapon[modID]; ok {
		cancelFn()
	}
	// Deletes the activeModules and cancelWeapon entries.
	stopWeaponFX(c, addr, accountID, charID, ds, modID, mod.typeID, mod.targetID, mod.effectName, sqldb)
	if ds.entity != nil {
		if newSpeed, removed := ds.entity.RemoveSpeedModifier(modID); removed {
			broadcastSetMaxSpeed(ds, ds.entity, newSpeed)
			clearMWDBloom(ds.entity)
		}
	}
	if isResistEffectName(mod.effectName) {
		recalcShipResists(ds, sqldb)
	}
	pushStatsAfterModuleChange(c, accountID, charID, ds, mod.typeID, sqldb)
	// A cloaking device forced off reveals the ship. No-op for any other module.
	disengageModuleCloak(ds, ds.entity, modID, mod.typeID, sqldb)
	return true
}

// pushStatsAfterModuleChange re-sends the ship stats a module moves only while it runs, after
// it started or stopped: turret reach for a tracking computer, targeting range and scan
// resolution for a sensor booster. The client folds neither itself.
func pushStatsAfterModuleChange(c *evenet.Conn, accountID, charID int64, ds *destinyConn, modTypeID int64, sqldb *sql.DB) {
	if sqldb == nil || modTypeID == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	groupID, _, _ := evedb.GetTypeGroupCategory(ctx, sqldb, modTypeID)
	cancel()
	switch groupID {
	case trackingComputerGroup:
		pushWeaponAttrs(c, accountID, charID, ds.shipID, sqldb, ds.activeEffectIDs())
	case sensorBoosterGroup:
		pushEwarVictimStats(ds.entity, sqldb)
	}
}

// stopModulesForWarp switches off every running module a warp interferes with
// (warpStopsEffect) and tells the pilot when any went dark. Mining and survey sessions end
// with them. A cycle loop left with no module entry is cancelled too. Connection goroutine only.
func stopModulesForWarp(c *evenet.Conn, addr string, accountID, charID int64, ds *destinyConn, sqldb *sql.DB) {
	stopped := 0
	for modID, mod := range ds.activeModules {
		if mod == nil || !warpStopsEffect(mod.effectName, mod.targetID) {
			continue
		}
		if stopModuleNow(c, addr, accountID, charID, ds, modID, sqldb) {
			stopped++
		}
	}
	for modID, sess := range ds.miningSessions {
		sess.cancel()
		delete(ds.miningSessions, modID)
	}
	for modID, cancelFn := range ds.cancelWeapon {
		if ds.activeModules[modID] == nil {
			cancelFn()
			delete(ds.cancelWeapon, modID)
		}
	}
	if stopped > 0 {
		log.Printf("[%s] warp: %d module(s) switched off", addr, stopped)
		pushRemoteMessage(c, accountID, "EffectDeactivationWarping", nil)
	}
}

// moduleNameDict is the one-entry message dict naming a module type under key.
func moduleNameDict(sqldb *sql.DB, key string, typeID int64) map[string]marshal.PyRep {
	name := ""
	if sqldb != nil && typeID != 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		name = typeNameOrID(ctx, sqldb, typeID)
		cancel()
	}
	return map[string]marshal.PyRep{key: marshal.PyString{Value: name}}
}
