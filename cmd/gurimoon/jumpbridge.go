// Titan jump bridges (jumpPortalGeneration, effect 2152). A titan runs a Jump Portal Generator,
// points it at a fleet member's cynosural field, and subcapitals next to it jump through. The
// titan pays each ship's strontium by the ship's mass.
//
// The client ships all three halves: "Bridge to member" (titan-gated), "Jump through to
// <system>" on the titan's menu, and svc.gang's activeBridge dict fed by OnBridgeModeChange.
// See docs/jump-notes.md#jump-bridge.
package main

import (
	"context"
	"database/sql"
	evedb "gurimoon/db"
	"gurimoon/dogma"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
	"fmt"
	"gurimoon/syncx"
	"log"
	"math"
	"time"
)

// attrMass is the hull's mass in kg, which the portal's fuel bill scales with.
const attrMass int64 = 4

// jumpBridge is one titan holding a portal open. It lives in memory: the window is sixty
// seconds and its state belongs to one live connection, as with the GM-mode registry.
type jumpBridge struct {
	shipID       int64
	charID       int64
	moduleID     int64
	moduleTypeID int64
	sysID        int64
	gangID       int64
	// Destination, set by BridgeToMember. Until then the portal points nowhere and the fleet has
	// been told nothing: OnBridgeModeChange's payload is the destination.
	destBeaconBall int64
	destSysID      int64
	expiresAt      time.Time
	timerID        uint64
}

// jumpBridges maps a titan's ship itemID to its open portal. Keyed on shipID because the
// client's activeBridge dict and a subcapital's JumpThrough both use it.
var jumpBridges syncx.Map[int64, *jumpBridge]

func getJumpBridge(shipID int64) *jumpBridge {
	if b, ok := jumpBridges.Load(shipID); ok {
		return b
	}
	return nil
}

// openJumpBridge runs a Jump Portal Generator activation.
func openJumpBridge(c *evenet.Conn, addr string, accountID, charID int64,
	ds *destinyConn, modID, modTypeID int64, modAttrs map[int64]float64,
	durationMs int64, sqldb *sql.DB) bool {

	if ds == nil || ds.solarSystem == nil || ds.entity == nil || sqldb == nil || ds.shipID == 0 {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// A bridge is a jump for everyone who takes it, so the same rule applies.
	if destinationIsHighsec(ds.sysID) {
		cynoNotify(c, accountID, "A jump portal cannot be opened in empire space.")
		return false
	}
	if getJumpBridge(ds.shipID) != nil {
		cynoNotify(c, accountID, "Your jump portal is already open.")
		return false
	}
	// 500 Strontium Clathrates through the generic path the cyno's ozone uses; nothing names a fuel type.
	if !consumeModuleFuel(ctx, c, addr, accountID, charID, ds, modID, modTypeID,
		dogma.EffNameJumpPortal, modAttrs, sqldb) {
		return false
	}

	// durationMs is attr 73, not the effect's declared durationAttributeID: effect 2152 names
	// jumpPortalDuration(1002), which this module does not carry (a lookup there gives zero).
	if durationMs <= 0 {
		durationMs = 60000
	}
	b := &jumpBridge{
		shipID: ds.shipID, charID: charID,
		moduleID: modID, moduleTypeID: modTypeID,
		sysID:     ds.sysID,
		expiresAt: time.Now().Add(time.Duration(durationMs) * time.Millisecond),
	}
	if globalGangReg != nil {
		b.gangID = globalGangReg.GetCharGang(charID)
	}
	jumpBridges.Store(ds.shipID, b)

	markImmobiliser(ds, modID, modAttrs, typeNameOrID(ctx, sqldb, modTypeID))

	shipID := ds.shipID
	if globalTickMgr != nil {
		b.timerID = globalTickMgr.ScheduleOnce(fmt.Sprintf("bridge-close-%d", shipID),
			b.expiresAt, func() { closeJumpBridge(shipID, "expired") })
	}
	log.Printf("[%s] jump portal open: ship=%d char=%d sys=%d for %ds",
		addr, shipID, charID, ds.sysID, durationMs/1000)
	return true
}

// closeJumpBridge shuts a portal, whatever ended it. Idempotent.
func closeJumpBridge(shipID int64, reason string) {
	b, ok := jumpBridges.LoadAndDelete(shipID)
	if !ok || b == nil {
		return
	}
	if globalTickMgr != nil && b.timerID != 0 {
		globalTickMgr.CancelOnce(b.timerID)
	}
	// Tell the fleet only if they were told it was open (at BridgeToMember, not at activation).
	if b.destBeaconBall != 0 {
		broadcastBridgeModeChange(b, false)
	}
	if conn, acct := charConnLookup(b.charID); conn != nil {
		if ds := bridgeOwnerState(b); ds != nil {
			clearImmobiliser(ds, b.moduleID)
			stopWeaponFX(conn, "bridge", acct, b.charID, ds,
				b.moduleID, b.moduleTypeID, 0, dogma.EffNameJumpPortal, globalDB)
		}
	}
	log.Printf("[bridge] closed: ship=%d char=%d (%s)", shipID, b.charID, reason)
}

// closeJumpBridgeForChar is the death and disconnect path, which has a charID
// and may no longer have a trustworthy shipID.
func closeJumpBridgeForChar(charID int64, reason string) {
	var found int64
	jumpBridges.Range(func(_ int64, b *jumpBridge) bool {
		if b != nil && b.charID == charID {
			found = b.shipID
			return false
		}
		return true
	})
	if found != 0 {
		closeJumpBridge(found, reason)
	}
}

// bridgeOwnerState finds the titan pilot's live connection state.
func bridgeOwnerState(b *jumpBridge) *destinyConn {
	if globalSysMgr == nil {
		return nil
	}
	ss := globalSysMgr.Get(b.sysID)
	if ss == nil {
		return nil
	}
	e := ss.GetPlayerByCharID(b.charID)
	if e == nil {
		return nil
	}
	ds, _ := e.PlayerState.(*destinyConn)
	return ds
}

// broadcastBridgeModeChange tells the fleet a portal opened onto a destination, or closed.
// The payload is (titanShipID, destination solarsystemID, destination beacon ball, active);
// svc.gang stores it as activeBridge[shipID] = (solarsystemID, itemID) and menusvc reads the
// pair back. The system is the destination's, not the titan's.
func broadcastBridgeModeChange(b *jumpBridge, active bool) {
	activeVal := uint64(0)
	if active {
		activeVal = 1
	}
	args := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(b.shipID)},
		marshal.PyInt{Value: uint64(b.destSysID)},
		marshal.PyInt{Value: uint64(b.destBeaconBall)},
		marshal.PyInt{Value: activeVal},
	}}
	sent := 0
	send := func(charID int64) {
		if conn, acct := charConnLookup(charID); conn != nil {
			if err := server.SendNotification(conn, acct, "OnBridgeModeChange", "charid", args); err == nil {
				sent++
			}
		}
	}
	if b.gangID != 0 && globalGangReg != nil {
		if g := globalGangReg.GetGang(b.gangID); g != nil {
			for memberID := range g.Members {
				send(memberID)
			}
			log.Printf("[bridge] OnBridgeModeChange active=%v ship=%d → %d member(s)",
				active, b.shipID, sent)
			return
		}
	}
	send(b.charID)
}

// handleBridgeToMember implements bp.BridgeToMember(charID, beaconID, solarsystemID): the
// titan pointing its open portal at a fleet member's field. Nothing moves; the destination is
// armed and the fleet is told, which puts "Jump through to <system>" on every menu.
func handleBridgeToMember(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket,
	accountID, charID int64, ds *destinyConn, sqldb *sql.DB) error {

	if len(cs.ArgTuple.Items) < 3 || ds == nil || sqldb == nil || ds.shipID == 0 {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	beaconID := argInt64(cs.ArgTuple.Items[1])
	destSysID := argInt64(cs.ArgTuple.Items[2])
	log.Printf("[%s] → bound.BridgeToMember beacon=%d sys=%d", addr, beaconID, destSysID)

	b := getJumpBridge(ds.shipID)
	if b == nil {
		return server.SendCallException(c, pkt, accountID, jumpDriveError("JumpNoPortal", nil))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rec, msgKey := resolveJumpBeacon(ctx, sqldb, beaconID, destSysID, charID)
	if msgKey != "" {
		return server.SendCallException(c, pkt, accountID, jumpDriveError(msgKey, nil))
	}
	if destinationIsHighsec(rec.SolarSystemID) {
		return server.SendCallException(c, pkt, accountID, customNotify(
			"A jump portal cannot be opened into empire space."))
	}

	// Range is the generator's own jumpDriveRange(867), 3.5 LY, separate from the hull's.
	if modAttrs, err := evedb.GetTypeAttributes(ctx, sqldb, b.moduleTypeID); err == nil {
		if maxLY := modAttrs[dogma.AttrJumpDriveRange]; maxLY > 0 {
			distLY, ok := jumpRangeLY(ds.sysID, rec.SolarSystemID)
			if !ok {
				return server.SendCallException(c, pkt, accountID,
					jumpDriveError("JumpDestinationInvalid", nil))
			}
			if distLY > maxLY {
				return server.SendCallException(c, pkt, accountID, jumpDriveError("JumpTooFar",
					map[string]marshal.PyRep{
						"dist":  marshal.PyString{Value: formatLY(distLY)},
						"range": marshal.PyString{Value: formatLY(maxLY)},
					}))
			}
		}
	}

	b.destBeaconBall = int64(cynoBeaconID(rec.ShipID))
	b.destSysID = rec.SolarSystemID
	broadcastBridgeModeChange(b, true)
	log.Printf("[%s] bridge armed: ship=%d → sys=%d beacon=%d",
		addr, b.shipID, b.destSysID, b.destBeaconBall)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// handleJumpThrough implements bp.JumpThrough(otherCharID, otherShipID, beaconID,
// solarsystemID), a subcapital taking a titan's bridge. It arrives on the subcapital's
// connection; every ID in the arguments belongs to the titan.
func handleJumpThrough(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket,
	accountID, charID int64, ds *destinyConn, sqldb *sql.DB) error {

	if len(cs.ArgTuple.Items) < 4 || ds == nil || ds.entity == nil || sqldb == nil {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	titanShipID := argInt64(cs.ArgTuple.Items[1])
	beaconID := argInt64(cs.ArgTuple.Items[2])
	destSysID := argInt64(cs.ArgTuple.Items[3])
	log.Printf("[%s] → bound.JumpThrough titan=%d beacon=%d sys=%d", addr, titanShipID, beaconID, destSysID)

	if pinned, mod := shipImmobilised(ds); pinned {
		return server.SendCallException(c, pkt, accountID, immobilisedError(mod, "jump"))
	}
	// A tackled ship does not get a free ride through a portal.
	if held, net := warpScrambleHold(ds, sqldb); held {
		log.Printf("[%s] JumpThrough: BLOCKED — warp scrambled (net %.0f)", addr, net)
		return server.SendCallException(c, pkt, accountID, warpScrambledError())
	}
	if aggressionLocked(charID) {
		log.Printf("[%s] JumpThrough: BLOCKED — recent aggression", addr)
		return clientMsgErr(c, pkt, accountID, "CantJumpAfterAggression", nil)
	}

	b := getJumpBridge(titanShipID)
	if b == nil || b.destBeaconBall == 0 || b.destBeaconBall != beaconID || b.sysID != ds.sysID {
		return server.SendCallException(c, pkt, accountID, jumpDriveError("JumpNoBridge", nil))
	}
	if b.gangID != 0 && globalGangReg != nil && globalGangReg.GetCharGang(charID) != b.gangID {
		return server.SendCallException(c, pkt, accountID, jumpDriveError("JumpDestinationInvalid", nil))
	}

	// maxStargateJumpingDistance: gang.CanJumpThrough gates the menu client-side; this is the real gate.
	if ds.solarSystem != nil {
		titan := ds.solarSystem.GetEntity(uint32(titanShipID))
		if titan == nil {
			return server.SendCallException(c, pkt, accountID, jumpDriveError("JumpNoBridge", nil))
		}
		if d := ds.entity.Pos().Sub(titan.Pos()).Len(); d > jumpThroughRange {
			return server.SendCallException(c, pkt, accountID, jumpDriveError("NotCloseEnoughToJump",
				map[string]marshal.PyRep{
					"maxdist": marshal.PyInt{Value: uint64(jumpThroughRange)},
				}))
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	rec, msgKey := resolveJumpBeacon(ctx, sqldb, beaconID, destSysID, charID)
	if msgKey != "" {
		return server.SendCallException(c, pkt, accountID, jumpDriveError(msgKey, nil))
	}

	if key, params := chargeBridgeFuel(ctx, addr, b, ds, sqldb); key != "" {
		return server.SendCallException(c, pkt, accountID, jumpDriveError(key, params))
	}

	if err := server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}); err != nil {
		return err
	}
	return performJumpTo(ctx, c, addr, accountID, charID, ds, rec, sqldb)
}

// chargeBridgeFuel takes the strontium a bridging ship costs out of the titan's hold; returns
// a client message key on refusal, having taken nothing. The bill is mass x 1e-9 x the
// generator's consumptionQuantity (jumpPortalConsumptionMassFactor 1001). A subcapital carries
// no jump attributes and pays no capacitor.
func chargeBridgeFuel(ctx context.Context, addr string, b *jumpBridge,
	ds *destinyConn, sqldb *sql.DB) (string, map[string]marshal.PyRep) {

	modAttrs, err := evedb.GetTypeAttributes(ctx, sqldb, b.moduleTypeID)
	if err != nil {
		return "", nil
	}
	fuelType := int64(modAttrs[dogma.AttrConsumptionType])
	massFactor := modAttrs[dogma.AttrJumpPortalConsumptionMassFactor]
	perUnit := modAttrs[dogma.AttrConsumptionQuantity]
	if fuelType == 0 || massFactor <= 0 || perUnit <= 0 {
		return "", nil
	}

	hullAttrs, hErr := evedb.GetTypeAttributes(ctx, sqldb, ds.shipTypeID)
	if hErr != nil {
		return "", nil
	}
	mass := hullAttrs[attrMass]
	qty := int64(math.Ceil(mass * massFactor * perUnit))
	if qty <= 0 {
		return "", nil
	}

	ok, deltas, cErr := evedb.ConsumeCargoFuel(ctx, sqldb, fuelType, b.charID, b.shipID, qty)
	if cErr != nil {
		log.Printf("[%s] bridge: ConsumeCargoFuel: %v", addr, cErr)
		ok = false
	}
	if !ok {
		// Both halves are told: the passenger did not move, and only the titan pilot can act on it.
		if conn, acct := charConnLookup(b.charID); conn != nil {
			cynoNotify(conn, acct, "Your jump portal is out of fuel.")
		}
		return "JumpNotEnoughCharge", map[string]marshal.PyRep{
			"required":  marshal.PyInt{Value: uint64(qty)},
			"typeName":  marshal.PyString{Value: typeNameOrID(ctx, sqldb, fuelType)},
			"available": marshal.PyInt{Value: 0},
		}
	}
	// The stacks that changed are in the TITAN's hold, so the pushes go to the
	// titan's client and not to the ship that is jumping.
	if conn, acct := charConnLookup(b.charID); conn != nil {
		pushCargoDeltas(conn, acct, deltas)
	}
	log.Printf("[%s] bridge: titan %d burned %d of type %d for a %.0f kg hull",
		addr, b.shipID, qty, fuelType, mass)
	return "", nil
}
