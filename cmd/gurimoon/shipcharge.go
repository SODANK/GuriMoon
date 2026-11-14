// Capacitor and shield a ship keeps across a system change.
//
// registerUndock builds a fresh entity with a full capacitor and a full shield. That is right
// for an undock. A gate jump or a cross-system teleport re-registers the same ship, so the
// departing entity's charge is captured first and laid over the new one.
package main

import (
	"gurimoon/destiny"
	"gurimoon/dogma"
	evenet "gurimoon/net"
)

// shipCharge is a ship's capacitor and shield as fractions of their maximum, so a pool whose
// size differs in the new entity keeps its fill level.
type shipCharge struct {
	shipID      int64
	cap, shield float64
}

// captureShipCharge records the charge of the pilot's current entity for the next
// registerUndock of the same ship. Call it before the entity leaves its system.
func captureShipCharge(ds *destinyConn) {
	if ds == nil || ds.entity == nil || ds.shipID == 0 {
		return
	}
	sc := &shipCharge{shipID: ds.shipID, cap: 1, shield: ds.entity.DamageStateFrac().Shield}
	if cur, max := ds.entity.CurrentCap(); max > 0 {
		sc.cap = cur / max
	}
	ds.carriedCharge = sc
}

// applyCarriedCharge lays a captured charge over a freshly built entity that is not yet in a
// system, and clears it. A capture from another ship is dropped unused.
func applyCarriedCharge(ds *destinyConn, entity *destiny.Entity) {
	sc := ds.carriedCharge
	ds.carriedCharge = nil
	if sc == nil || entity == nil || sc.shipID != ds.shipID {
		return
	}
	entity.SetCapFraction(sc.cap)
	if entity.HP.MaxShield > 0 {
		entity.HP.CurShield = entity.HP.MaxShield * clamp01(sc.shield)
	}
}

// pushCapacitorCharge sends the ship's present capacitor charge (attr 18). The client keeps
// extrapolating the last charge it was told, so every fresh entity is announced.
func pushCapacitorCharge(c *evenet.Conn, accountID, charID int64, ds *destinyConn, entity *destiny.Entity) {
	if c == nil || entity == nil || !entity.HasCap() {
		return
	}
	cur, max := entity.CurrentCap()
	sendModuleAttributeChange(c, accountID, charID, ds.shipID, dogma.AttrCharge, cur, max)
}
