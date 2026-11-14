// Capital jump drives. A hull with canJump(861) jumps to a cynosural field a fleet member has
// lit, up to jumpDriveRange(867) light years away, burning racial isotopes by the light year and
// most of its capacitor. It may not arrive in empire space.
//
// The client's entry is "Jump to member" on a fleet member's menu, wrapped in
// sessionMgr.PerformSessionChange('jump', bp.BeaconJump, ...), so it waits for the session
// change crossSystemJump sends. The client hides the stargate entry for canJump hulls
// (menusvc checkCanUseGate); CantJumpWithJumpDrive is unused. See docs/jump-notes.md#jump-drive.
package main

import (
	"context"
	"database/sql"
	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/dogma"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
	"fmt"
	"log"
	"math"
	"math/rand"
	"time"
)

// Arrival geometry, from eveConstants: a jump lands 12–13 km off the beacon.
const (
	jumpInDistanceMin = 12000.0
	jumpInDistanceMax = 13000.0
	// maxStargateJumpingDistance: how close a subcapital must be to a titan to take its bridge.
	// The client checks it in gang.CanJumpThrough; the server is the gate.
	jumpThroughRange = 2500.0
)

// cynoShipFromBallID recovers the ship that lit a field from its beacon ball ID: the live field
// whose cynoBeaconID it is, in sysID when known, else anywhere. The ID is folded into a band
// (ephemeralball.go) and cannot be subtracted back. 0 when no live field has that ball.
func cynoShipFromBallID(ctx context.Context, sqldb *sql.DB, ballID, sysID int64) int64 {
	if ballID < cynoBeaconBallBand || ballID >= cynoBeaconBallBand+ephemeralBallSpan || sqldb == nil {
		return 0
	}
	var recs []evedb.CynoBeacon
	var err error
	if sysID != 0 {
		recs, err = evedb.ListCynoBeaconsForSystem(ctx, sqldb, sysID, server.Win32TimeNow())
	} else {
		recs, err = evedb.ListLiveCynoBeacons(ctx, sqldb, server.Win32TimeNow())
	}
	if err != nil {
		return 0
	}
	for _, r := range recs {
		if int64(cynoBeaconID(r.ShipID)) == ballID {
			return r.ShipID
		}
	}
	return 0
}

// jumpDriveError raises one of the client's own shipped jump dialogs.
func jumpDriveError(key string, params map[string]marshal.PyRep) marshal.PyRep {
	return server.BuildUserError(key, params)
}

// resolveJumpBeacon looks up a live field by the ball ID the client sent and checks the caller
// may use it. The gang is read from the stored row, not the live registry: a field is lit for
// the fleet that existed then.
func resolveJumpBeacon(ctx context.Context, sqldb *sql.DB, beaconBallID, wantSysID, callerCharID int64) (*evedb.CynoBeacon, string) {
	shipID := cynoShipFromBallID(ctx, sqldb, beaconBallID, wantSysID)
	if shipID == 0 {
		return nil, "JumpDestinationInvalid"
	}
	rec, err := evedb.GetCynoBeaconByShip(ctx, sqldb, shipID)
	if err != nil || rec == nil {
		return nil, "JumpDestinationInvalid"
	}
	if rec.ExpiresAt <= server.Win32TimeNow() {
		return nil, "JumpDestinationInvalid"
	}
	if wantSysID != 0 && rec.SolarSystemID != wantSysID {
		return nil, "JumpDestinationInvalid"
	}
	if rec.GangID != 0 && globalGangReg != nil {
		if globalGangReg.GetCharGang(callerCharID) != rec.GangID {
			return nil, "JumpDestinationInvalid"
		}
	}
	return rec, ""
}

// jumpRangeLY returns the distance between two solar systems in light years.
//
// Both positions come from the preloaded celestial cache: this runs on an
// activation path and must not query.
func jumpRangeLY(fromSysID, toSysID int64) (float64, bool) {
	if globalCelestialCache == nil {
		return 0, false
	}
	a, ok1 := globalCelestialCache.GetPosition(fromSysID)
	b, ok2 := globalCelestialCache.GetPosition(toSysID)
	if !ok1 || !ok2 {
		return 0, false
	}
	return destiny.Vec3{X: a.X, Y: a.Y, Z: a.Z}.
		LightYearsTo(destiny.Vec3{X: b.X, Y: b.Y, Z: b.Z}), true
}

// destinationIsHighsec reports whether a jump may not land there.
func destinationIsHighsec(sysID int64) bool {
	if globalCelestialCache == nil {
		return false
	}
	sec, ok := globalCelestialCache.GetSecurity(sysID)
	return ok && sec >= highsecFloor
}

// jumpArrivalPoint places an arriving ship off a beacon. A random point on the 12-13 km sphere
// is blind to ships already there (the cyno pilot sits at its centre), so ClaimArrivalPoint
// checks live footprints and in-flight reservations. It tolerates a nil destination system
// (restart recovery).
func jumpArrivalPoint(destSS *destiny.SolarSystem, destSysID int64, beaconPos destiny.Vec3,
	hullRadius float64, exclude uint32) destiny.Vec3 {

	dir := destiny.Vec3{
		X: rand.Float64()*2 - 1,
		Y: rand.Float64()*2 - 1,
		Z: rand.Float64()*2 - 1,
	}
	if dir.IsZero() {
		dir = destiny.Vec3{X: 1}
	}
	dir = dir.Normalize()
	dist := jumpInDistanceMin + rand.Float64()*(jumpInDistanceMax-jumpInDistanceMin)
	base := beaconPos.Add(dir.Scale(dist))
	return destiny.ClaimArrivalPoint(destSS, destSysID, base, dir, hullRadius, exclude)
}

// spendJumpResources charges the capacitor and the isotopes for one jump. Returns a client
// message key on refusal, having taken nothing. jumpDriveCapacitorNeed(898) is a fraction of
// capacitor capacity, not GJ like every other capacitor attribute here.
func spendJumpResources(ctx context.Context, c *evenet.Conn, addr string, accountID, charID int64,
	ds *destinyConn, hullAttrs map[int64]float64, distLY float64, sqldb *sql.DB) (string, map[string]marshal.PyRep) {

	cur, max := ds.entity.CurrentCap()
	need := hullAttrs[dogma.AttrJumpDriveCapacitorNeed] * max
	if cur < need {
		return "JumpNotEnoughCap", map[string]marshal.PyRep{
			"need": marshal.PyReal{Value: need},
			"got":  marshal.PyReal{Value: cur},
		}
	}

	fuelType := int64(hullAttrs[dogma.AttrJumpDriveConsumptionType])
	perLY := hullAttrs[dogma.AttrJumpDriveConsumptionAmount]
	fuelQty := int64(math.Ceil(perLY * distLY))
	if fuelType != 0 && fuelQty > 0 {
		have, _ := evedb.CountCargoItemsByType(ctx, sqldb, fuelType, charID, ds.shipID)
		ok, deltas, err := evedb.ConsumeCargoFuel(ctx, sqldb, fuelType, charID, ds.shipID, fuelQty)
		if err != nil {
			log.Printf("[%s] jump: ConsumeCargoFuel: %v", addr, err)
			ok = false
		}
		if !ok {
			return "JumpNotEnoughCharge", map[string]marshal.PyRep{
				"required":  marshal.PyInt{Value: uint64(fuelQty)},
				"typeName":  marshal.PyString{Value: typeNameOrID(ctx, sqldb, fuelType)},
				"available": marshal.PyInt{Value: uint64(maxInt64(have, 0))},
			}
		}
		pushCargoDeltas(c, accountID, deltas)
		log.Printf("[%s] jump: burned %d of type %d for %.2f LY", addr, fuelQty, fuelType, distLY)
	}

	// Capacitor last: the fuel take is the one that can fail on a race, and a failed jump must not
	// cost capacitor.
	if newVal, ok := ds.entity.ConsumeCap(need); ok {
		sendModuleAttributeChange(c, accountID, charID, ds.shipID,
			dogma.AttrCharge, newVal, newVal+need)
	}
	return "", nil
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// pushCargoDeltas mirrors the stack changes of a fuel take to the client, so an
// open cargo hold updates instead of keeping its old quantities.
func pushCargoDeltas(c *evenet.Conn, accountID int64, deltas []evedb.ItemDelta) {
	for _, d := range deltas {
		if d.Row == nil {
			continue
		}
		if d.Kind == evedb.ItemDeltaRemoved {
			_ = server.SendNotification(c, accountID, "OnItemChange", "charid", itemDeletedNotifArgs(d.Row))
			continue
		}
		_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
			evedb.BuildEntityRow(d.Row),
			marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(d.OldQty)}},
			}},
		}})
	}
}

// performJumpTo moves a ship to a beacon and lands it in the arrival lane. crossSystemJump is
// reached through a slashTeleporter used as a call-context bag, as handleDeadspace does.
func performJumpTo(ctx context.Context, c *evenet.Conn, addr string, accountID, charID int64,
	ds *destinyConn, rec *evedb.CynoBeacon, sqldb *sql.DB) error {

	// Drop what the warp goroutine still holds for the departure system: a completed warp keeps a
	// deferred position sync armed for 12 s that would re-issue its landing as a GOTO.
	abortPendingWarp(ds)
	if ds.cancelWarp != nil {
		ds.cancelWarp()
		ds.cancelWarp = nil
	}

	target, err := evedb.ClassifyTeleportTarget(ctx, sqldb, rec.SolarSystemID)
	if err != nil || target == nil {
		log.Printf("[%s] jump: ClassifyTeleportTarget sys=%d: %v", addr, rec.SolarSystemID, err)
		return nil
	}

	var oldSysID int64
	if ds.solarSystem != nil {
		oldSysID = ds.solarSystem.ID
	}
	beaconPos := destiny.Vec3{X: rec.X, Y: rec.Y, Z: rec.Z}
	var destSS *destiny.SolarSystem
	if globalSysMgr != nil {
		destSS = globalSysMgr.Get(rec.SolarSystemID)
	}
	var hullRadius float64
	var excludeID uint32
	if ds.entity != nil {
		excludeID = ds.entity.ID
		if fp, ok := ds.entity.Footprint(); ok {
			hullRadius = fp.Radius
		}
	}
	landing := jumpArrivalPoint(destSS, rec.SolarSystemID, beaconPos, hullRadius, excludeID)

	tp := &slashTeleporter{c: c, addr: addr, accountID: accountID,
		charID: charID, sqldb: sqldb, ds: ds}
	if _, err := tp.crossSystemJump(ctx, target.SolarSystemID, target.ConstellationID,
		target.RegionID, landing); err != nil {
		log.Printf("[%s] jump: crossSystemJump: %v", addr, err)
		return nil
	}
	// The criminal timer follows the pilot, as through a stargate.
	carryCriminalFlag(oldSysID, target.SolarSystemID, charID)

	// A jump-drive arrival gets no gate cloak, but its engines are shut down so the flare does
	// not coast its decay ramp on a parked hull.
	sendBoosterFX(ds, c, uint64(accountID), dogma.FXGUIDBoosterStop)
	ds.lastJumpAt = time.Now()

	log.Printf("[%s] jump complete: char=%d → sys=%d beacon=%d landing=(%.0f,%.0f,%.0f)",
		addr, charID, rec.SolarSystemID, cynoBeaconID(rec.ShipID), landing.X, landing.Y, landing.Z)
	return nil
}

// handleBeaconJump implements bp.BeaconJump(charID, beaconID, solarsystemID), "Jump to member"
// on a fleet member carrying a lit cynosural field.
func handleBeaconJump(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket,
	accountID, charID int64, ds *destinyConn, sqldb *sql.DB) error {

	if len(cs.ArgTuple.Items) < 3 || ds == nil || ds.entity == nil || sqldb == nil {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	beaconID := argInt64(cs.ArgTuple.Items[1])
	destSysID := argInt64(cs.ArgTuple.Items[2])
	log.Printf("[%s] → bound.BeaconJump beacon=%d sys=%d", addr, beaconID, destSysID)

	// A pinned ship cannot jump, including the cyno pilot jumping to their own field.
	if pinned, mod := shipImmobilised(ds); pinned {
		return server.SendCallException(c, pkt, accountID, immobilisedError(mod, "jump"))
	}
	// A jump is a departure like any other; skipping the scramble check would make tackle optional.
	if held, net := warpScrambleHold(ds, sqldb); held {
		log.Printf("[%s] BeaconJump: BLOCKED — warp scrambled (net %.0f)", addr, net)
		return server.SendCallException(c, pkt, accountID, warpScrambledError())
	}
	if aggressionLocked(charID) {
		log.Printf("[%s] BeaconJump: BLOCKED — recent aggression", addr)
		return clientMsgErr(c, pkt, accountID, "CantJumpAfterAggression", nil)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	hullAttrs, err := evedb.GetTypeAttributes(ctx, sqldb, ds.shipTypeID)
	if err != nil || hullAttrs[dogma.AttrCanJump] <= 0 {
		return server.SendCallException(c, pkt, accountID, jumpDriveError("JumpNoDrive", nil))
	}

	// jumpDriveDuration(869) is 5 minutes on every jump hull; without it the range attribute is moot.
	if wait := jumpCooldownRemaining(ds, hullAttrs); wait > 0 {
		return server.SendCallException(c, pkt, accountID, customNotify(
			"Your jump drive is still recharging. Ready in " +
				formatDurationShort(wait) + "."))
	}

	rec, msgKey := resolveJumpBeacon(ctx, sqldb, beaconID, destSysID, charID)
	if msgKey != "" {
		return server.SendCallException(c, pkt, accountID, jumpDriveError(msgKey, nil))
	}
	if destinationIsHighsec(rec.SolarSystemID) {
		return server.SendCallException(c, pkt, accountID, customNotify(
			"A jump drive cannot be engaged into empire space."))
	}

	distLY, ok := jumpRangeLY(ds.sysID, rec.SolarSystemID)
	if !ok {
		return server.SendCallException(c, pkt, accountID, jumpDriveError("JumpDestinationInvalid", nil))
	}
	maxLY := hullAttrs[dogma.AttrJumpDriveRange]
	if distLY > maxLY {
		return server.SendCallException(c, pkt, accountID, jumpDriveError("JumpTooFar",
			map[string]marshal.PyRep{
				"dist":  marshal.PyString{Value: formatLY(distLY)},
				"range": marshal.PyString{Value: formatLY(maxLY)},
			}))
	}

	// Everything above refuses for free. From here the jump is atomic.
	if key, params := spendJumpResources(ctx, c, addr, accountID, charID, ds, hullAttrs, distLY, sqldb); key != "" {
		return server.SendCallException(c, pkt, accountID, jumpDriveError(key, params))
	}

	if err := server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}); err != nil {
		return err
	}
	return performJumpTo(ctx, c, addr, accountID, charID, ds, rec, sqldb)
}

// jumpCooldownRemaining answers how long is left on jumpDriveDuration(869).
func jumpCooldownRemaining(ds *destinyConn, hullAttrs map[int64]float64) time.Duration {
	delayMs := hullAttrs[dogma.AttrJumpDriveDuration]
	if delayMs <= 0 || ds.lastJumpAt.IsZero() {
		return 0
	}
	ready := ds.lastJumpAt.Add(time.Duration(delayMs) * time.Millisecond)
	if wait := time.Until(ready); wait > 0 {
		return wait
	}
	return 0
}

// formatLY renders a light-year figure the way the client's own JumpTooFar
// dialog reads it ("%(dist)s lightyears").
func formatLY(v float64) string {
	return fmt.Sprintf("%.2f", v)
}

// formatDurationShort renders a cooldown as "3m 12s" / "45s".
func formatDurationShort(d time.Duration) string {
	total := int(d.Round(time.Second).Seconds())
	if total < 60 {
		return fmt.Sprintf("%ds", total)
	}
	return fmt.Sprintf("%dm %ds", total/60, total%60)
}

// argInt64 pulls an integer out of a call argument, tolerating the PyReal the
// client occasionally sends for an ID.
func argInt64(r marshal.PyRep) int64 {
	switch v := r.(type) {
	case marshal.PyInt:
		return int64(v.Value)
	case marshal.PyReal:
		return int64(v.Value)
	}
	return 0
}
