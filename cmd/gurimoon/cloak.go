// Cloaking devices. See docs/combat-notes.md#cloaking-devices.
//
// A cloaking device holds the same cloaked flag the gate cloak uses, so hiding from other
// ballparks, the target pickers and the scanners all work as they do there. The differences
// are in what ends it: movement does not, the module stopping does (deactivation, going
// offline, an object within cloakProximityM), and breakGateCloak leaves it alone.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/dogma"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/safego"
)

const (
	// cloakProximityM is how close another object's surface may come before a cloak fails or
	// refuses to start. The client's CantCloakProximity message prints it.
	cloakProximityM = 2000.0
	// cloakWatchInterval is how often a running cloak looks for an object that close.
	cloakWatchInterval = time.Second
	// cloakFadeDelay lets the client's effects.Cloak fade (1.5 s) play before the ball is
	// withdrawn from the other pilots' ballparks.
	cloakFadeDelay = 1500 * time.Millisecond
	// skillCloaking is required by every cloaking device and by nothing else in group 330.
	skillCloaking int64 = 11579
)

// isCloakEffect reports whether an effect is a cloaking device's.
func isCloakEffect(effectName string) bool {
	return effectName == dogma.EffNameCloaking || effectName == dogma.EffNameCloakingWarpSafe
}

// isCloakModuleType reports whether a fitted module type is a cloaking device.
func isCloakModuleType(attrs map[int64]float64) bool {
	for _, a := range []int64{182, 183, 184} {
		if int64(attrs[a]) == skillCloaking {
			return true
		}
	}
	return false
}

// cloakFXArgs is the OnSpecialFX argument list of a cloak effect on a ship.
func cloakFXArgs(ballID uint32, guid string) []marshal.PyRep {
	return []marshal.PyRep{
		marshal.PyInt{Value: uint64(ballID)},
		marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{},
		marshal.PyList{},
		marshal.PyString{Value: guid},
		marshal.PyInt{Value: 0}, marshal.PyInt{Value: 1}, marshal.PyInt{Value: 0},
	}
}

// cloakRefusal returns the client message key and dict that refuse a cloaking device's start,
// or "" when it may start. Every check runs before anything changes. Police pursuit is refused
// by the caller; a docked ship (no entity) is never refused here.
func cloakRefusal(ctx context.Context, sqldb *sql.DB, ds *destinyConn, modID int64) (string, map[string]marshal.PyRep) {
	e := ds.entity
	if e == nil || ds.solarSystem == nil {
		return "", nil
	}
	switch {
	case e.IsGMCloak():
		return "ShipCloaked", nil
	case e.ModuleCloak() != 0:
		return "AlreadyCloaking", nil
	}
	if sqldb != nil && ds.shipID != 0 {
		if mods, err := evedb.GetFittedModules(ctx, sqldb, ds.shipID); err == nil {
			devices := 0
			for _, m := range mods {
				if attrs, aerr := evedb.TypeAttrs(ctx, sqldb, m.TypeID); aerr == nil && isCloakModuleType(attrs) {
					devices++
				}
			}
			if devices > 1 {
				return "CantCloakTooManyDevices", nil
			}
		}
	}
	if len(lockersOf(ds)) > 0 {
		return "CantCloakTargeted", nil
	}
	ds.targetMu.Lock()
	holding := len(ds.targets) + len(ds.lockingTargets)
	ds.targetMu.Unlock()
	if holding > 0 {
		return "CantCloakTargeting", nil
	}
	if near, ok := ds.solarSystem.NearestBallWithin(e.Pos(), e.BallDesc().Radius, cloakProximityM, e.ID); ok {
		return "CantCloakProximity", map[string]marshal.PyRep{
			"dist": marshal.PyInt{Value: uint64(cloakProximityM)},
			"item": marshal.PyString{Value: nearBallName(ctx, sqldb, near)},
		}
	}
	return "", nil
}

// nearBallName names a ball for a message: its type name, or "object" when it has none.
func nearBallName(ctx context.Context, sqldb *sql.DB, b destiny.NearBall) string {
	if sqldb == nil || b.TypeID == 0 {
		return "object"
	}
	return typeNameOrID(ctx, sqldb, b.TypeID)
}

// engageModuleCloak cloaks the ship under a cloaking device that has just started (the caller
// has sent the module's start event). Every other running module is switched off, the ship is
// hidden from the grid after the fade, NPCs forget it, and the device's speed limit applies.
// A ship still under its gate cloak is hidden already and only changes hands. Connection
// goroutine only.
func engageModuleCloak(c *evenet.Conn, addr string, accountID, charID int64, ds *destinyConn,
	modID, modTypeID int64, effectName string, modAttrs map[int64]float64, sqldb *sql.DB) {
	e := ds.entity
	if e == nil || ds.solarSystem == nil || ds.dm == nil {
		return
	}
	// Nothing else runs under a cloak (DeniedActivateCloaked refuses new activations).
	stopped := 0
	for id, mod := range ds.activeModules {
		if id == modID || mod == nil {
			continue
		}
		if stopModuleNow(c, addr, accountID, charID, ds, id, sqldb) {
			stopped++
		}
	}
	if stopped > 0 {
		pushRemoteMessage(c, accountID, "EffectDeactivationCloaking", nil)
	}

	alreadyHidden := e.IsCloaked()
	if !alreadyHidden {
		// The fade goes to the grid while the ball is still known there: broadcastFXAction
		// skips a cloaked ship.
		broadcastFXAction(ds, "OnSpecialFX", cloakFXArgs(e.ID, "effects.Cloak")...)
	}
	e.SetModuleCloak(modID)
	if globalNPCMgr != nil {
		globalNPCMgr.ForgetTargetInSystem(ds.solarSystem, e.ID)
	}
	// Locks still being acquired on the ship are cancelled; none is complete (cloakRefusal).
	dropLocksOnEntity(ds.solarSystem, e.ID)

	if !alreadyHidden {
		// CloakBall sets ball.isCloaked on the pilot's own client, which the gate cloak gets
		// from the SetState cloak byte; mode 1 is the ordinary cloak.
		idRep := marshal.PyInt{Value: uint64(e.ID)}
		ds.sendSelfStamped(func(stamp uint32) error {
			return destiny.SendActionList(c, globalNodeID, e.AccountID, stamp,
				destiny.BuildSimpleAction("CloakBall", idRep, marshal.PyInt{Value: 1}),
				destiny.BuildSimpleAction("OnSpecialFX", cloakFXArgs(e.ID, "effects.Cloak")...),
			)
		})
	}

	if v := modAttrs[dogma.AttrMaxVelocityBonus]; v > 0 && v != 1 {
		newSpeed := e.SetSpeedModifier(modID, v)
		broadcastSetMaxSpeed(ds, e, newSpeed)
	}

	if !alreadyHidden {
		hide := func() {
			if ds.entity == e && e.ModuleCloak() == modID {
				hideFromMates(ds)
			}
		}
		if globalTickMgr != nil {
			globalTickMgr.ScheduleOnce(fmt.Sprintf("module-cloak-%d", e.ID), time.Now().Add(cloakFadeDelay), hide)
		} else {
			hide()
		}
	}

	// The watch runs under the module's cancel func, so Deactivate, a forced stop and a dock
	// all end it. A device with a capacitor cost already has a cycle loop there; both stop.
	watchCtx, watchCancel := context.WithCancel(context.Background())
	if ds.cancelWeapon == nil {
		ds.cancelWeapon = make(map[int64]context.CancelFunc)
	}
	if prev, ok := ds.cancelWeapon[modID]; ok {
		ds.cancelWeapon[modID] = func() { prev(); watchCancel() }
	} else {
		ds.cancelWeapon[modID] = watchCancel
	}
	go cloakWatchLoop(watchCtx, c, addr, accountID, charID, ds, e, modID, modTypeID, effectName, sqldb)
	log.Printf("[%s] cloak ON: mod=%d (%s) ship=%d, %d other module(s) stopped", addr, modID, effectName, e.ID, stopped)
}

// cloakWatchLoop ends a running cloak when another object comes within cloakProximityM. It
// returns silently when cancelled: whoever cancelled it sends the stop and the reveal.
func cloakWatchLoop(ctx context.Context, c *evenet.Conn, addr string, accountID, charID int64, ds *destinyConn,
	e *destiny.Entity, modID, modTypeID int64, effectName string, sqldb *sql.DB) {
	defer safego.Recover("cloakWatchLoop")
	ticker := time.NewTicker(cloakWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if ds.entity != e || e.ModuleCloak() != modID {
			return
		}
		ss := ds.solarSystem
		// A covert ops cloak crosses grids in warp; nothing it passes is near it.
		if ss == nil || e.IsInWarp() {
			continue
		}
		near, ok := ss.NearestBallWithin(e.Pos(), e.BallDesc().Radius, cloakProximityM, e.ID)
		if !ok {
			continue
		}
		log.Printf("[%s] cloak OFF: mod=%d decloaked by ball %d at %.0f m", addr, modID, near.ID, near.Dist)
		// The same forced stop a cap-out runs from its loop.
		stopWeaponFX(c, addr, accountID, charID, ds, modID, modTypeID, 0, effectName, sqldb)
		disengageModuleCloak(ds, e, modID, modTypeID, sqldb)
		return
	}
}

// disengageModuleCloak reveals a ship whose cloaking device modID has stopped and starts the
// device's two delays: no locking for cloakingTargetingDelay, no reactivation for
// moduleReactivationDelay. The caller has sent, or is sending, the module's stop event. It
// does nothing when modID does not hold the cloak, so every stop path may call it. Safe from
// any goroutine.
func disengageModuleCloak(ds *destinyConn, e *destiny.Entity, modID, modTypeID int64, sqldb *sql.DB) {
	if e == nil || !e.EndModuleCloak(modID) {
		return
	}
	if ds.entity == e {
		// revealCloakedShip clears the cloaked flag as it tells the clients.
		revealCloakedShip(ds)
		if newSpeed, removed := e.RemoveSpeedModifier(modID); removed {
			broadcastSetMaxSpeed(ds, e, newSpeed)
		}
	} else {
		e.SetCloaked(false)
	}
	var attrs map[int64]float64
	if sqldb != nil && modTypeID != 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		attrs, _ = evedb.TypeAttrs(ctx, sqldb, modTypeID)
		cancel()
	}
	ds.setCloakRecalibration(time.Duration(attrs[dogma.AttrCloakingTargetingDelay]) * time.Millisecond)
	ds.armModuleReactivation(modID, attrs[dogma.AttrModuleReactivationDelay])
}

// forceModuleCloakOff switches a running cloaking device off from outside the pilot's
// connection goroutine (police landing on a pursued pilot): the forced stop the proximity
// watch runs, then the reveal.
func forceModuleCloakOff(ds *destinyConn, e *destiny.Entity, modID int64) {
	if ds == nil || e == nil || e.Conn == nil || e.ModuleCloak() != modID {
		return
	}
	var modTypeID int64
	effectName := dogma.EffNameCloaking
	if mod := ds.activeModules[modID]; mod != nil {
		modTypeID, effectName = mod.typeID, mod.effectName
	}
	if cancelFn, ok := ds.cancelWeapon[modID]; ok {
		cancelFn()
	}
	stopWeaponFX(e.Conn, e.Addr, int64(e.AccountID), ds.charID, ds, modID, modTypeID, 0, effectName, globalDB)
	disengageModuleCloak(ds, e, modID, modTypeID, globalDB)
}

// setCloakRecalibration blocks new target locks for d after a decloak.
func (ds *destinyConn) setCloakRecalibration(d time.Duration) {
	if ds == nil || d <= 0 {
		return
	}
	ds.targetMu.Lock()
	ds.cloakRecalUntil = time.Now().Add(d)
	ds.targetMu.Unlock()
}

// cloakRecalibrating reports whether the ship's sensors are still recalibrating after a cloak.
func (ds *destinyConn) cloakRecalibrating() bool {
	ds.targetMu.Lock()
	defer ds.targetMu.Unlock()
	return time.Now().Before(ds.cloakRecalUntil)
}

// moduleCloakOn reports whether the ship is cloaked by a cloaking device.
func moduleCloakOn(ds *destinyConn) bool {
	return ds != nil && ds.entity != nil && ds.entity.ModuleCloak() != 0
}

// cloakBlocksWarp reports whether the ship is under a cloaking device that cannot warp: every
// device but the covert ops cloak. Connection goroutine only.
func cloakBlocksWarp(ds *destinyConn) bool {
	if !moduleCloakOn(ds) {
		return false
	}
	mod := ds.activeModules[ds.entity.ModuleCloak()]
	return mod == nil || mod.effectName != dogma.EffNameCloakingWarpSafe
}
