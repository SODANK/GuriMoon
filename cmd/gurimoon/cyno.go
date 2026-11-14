// Cynosural fields: the beacon a fleet's capitals jump to. Activating a Cynosural Field Generator
// (21096, effect 1729 cynosuralGeneration) takes 500 Liquid Ozone, creates a beacon ball where
// the ship stands, tells every fleet member and pins the ship for the ten minutes the field
// burns. The module cannot be switched off early. The beacon is not an entity and has no HP;
// the counterplay is the pinned ship. See docs/jump-notes.md#cyno.
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
	"sync"
	"time"
)

// Beacon ball constants. The field renders from the shipped Cynosural Field I (21094), group 310
// Beacon, category 2 Celestial, as a rigid structure ball the server can persist and reload.
const (
	cynoBeaconTypeID  = 21094
	cynoBeaconGroupID = 310 // const.groupBeacon
	cynoBeaconCatID   = 2   // Celestial
	cynoBeaconRadius  = 500

	// Nothing may be lit or jumped to in empire space.
	highsecFloor = 0.45
)

// cynoBeaconID returns the ephemeral ball ID of a ship's cynosural field.
// Beacon balls never get an entity row, so the ID is derived from the ship
// that lit the field (one ship holds at most one), in its own band under 2^31
// (ephemeralball.go).
func cynoBeaconID(shipID int64) uint32 { return ephemeralBallID(cynoBeaconBallBand, shipID) }

// ─────────────────────────────────────────────────────────────────────────────
// The pin (siegeModeWarpStatus 852)
// ─────────────────────────────────────────────────────────────────────────────

// shipImmobilised reports whether a module pinning the ship in place is running, and names it
// for the refusal. It keys on attribute 852 (with disallowEarlyDeactivation 906), which sit on
// the same seven types: the cyno, the bridging titan and the siege modules.
func shipImmobilised(ds *destinyConn) (bool, string) {
	if ds == nil || len(ds.immobilisers) == 0 {
		return false, ""
	}
	for _, name := range ds.immobilisers {
		return true, name
	}
	return false, ""
}

// immobilisedError is the refusal every pinned-departure path answers with, worded for the
// action refused. It uses CustomNotify, which renders into the log rather than a modal, so
// repeated clicks do not stack dialogs.
func immobilisedError(module, action string) marshal.PyRep {
	return server.BuildUserError("CustomNotify", map[string]marshal.PyRep{
		"notify": marshal.PyString{Value: "You cannot " + action +
			" while the " + module + " is running."},
	})
}

// customNotify builds a plain informational refusal (CustomNotify) to raise as an exception,
// for paths that still have a CallReq to answer.
func customNotify(text string) marshal.PyRep {
	return server.BuildUserError("CustomNotify", map[string]marshal.PyRep{
		"notify": marshal.PyString{Value: text},
	})
}

// cynoNotify pushes a plain informational line into the pilot's log as OnRemoteMessage, since
// the CallRsp for the activation has already gone out.
func cynoNotify(c *evenet.Conn, accountID int64, text string) {
	if c == nil {
		return
	}
	args := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: "CustomNotify"},
		marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "notify"}, Value: marshal.PyString{Value: text}},
		}},
	}}
	_ = server.SendNotification(c, accountID, "OnRemoteMessage", "charid", args)
}

// markImmobiliser records a running module that pins the ship, if it is one. Each pinning
// module's activation branch calls it after succeeding, never the generic Activate path, so a
// refused activation cannot leave the pilot unable to move. The test is on the attribute.
func markImmobiliser(ds *destinyConn, modID int64, modAttrs map[int64]float64, name string) {
	if ds == nil || modAttrs[dogma.AttrSiegeModeWarpStatus] <= 0 {
		return
	}
	if ds.immobilisers == nil {
		ds.immobilisers = make(map[int64]string)
	}
	ds.immobilisers[modID] = name
}

// clearImmobiliser lifts one module's pin.
func clearImmobiliser(ds *destinyConn, modID int64) {
	if ds != nil {
		delete(ds.immobilisers, modID)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Lighting a field
// ─────────────────────────────────────────────────────────────────────────────

// lightCynoField runs a Cynosural Field Generator activation. Returns false when the field was
// refused, having taken nothing; the CallReq is already answered, so a refusal travels as
// OnRemoteMessage. Every gate fails closed, and the fuel is charged after all gates pass but
// before the beacon exists.
func lightCynoField(c *evenet.Conn, addr string, accountID, charID int64,
	ds *destinyConn, modID, modTypeID int64, modAttrs map[int64]float64,
	durationMs int64, sqldb *sql.DB) bool {

	if ds == nil || ds.solarSystem == nil || ds.entity == nil || sqldb == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	modName := typeNameOrID(ctx, sqldb, modTypeID)

	// Empire space. In memory, no query: this is an activation path.
	if globalCelestialCache != nil {
		if sec, ok := globalCelestialCache.GetSecurity(ds.sysID); ok && sec >= highsecFloor {
			log.Printf("[%s] cyno: refused — system %d is highsec (%.2f)", addr, ds.sysID, sec)
			cynoNotify(c, accountID, "A cynosural field cannot be lit in empire space.")
			return false
		}
	}

	// deadspaceUnsafe(801) is set on this module; inDeadspacePocket already serves mission pockets.
	if modAttrs[dogma.AttrDeadspaceUnsafe] > 0 && inDeadspacePocket(ds) {
		cynoNotify(c, accountID, "A cynosural field cannot be lit inside a deadspace pocket.")
		return false
	}

	// One field per ship. The primary key is the backstop; this check gives the pilot the refusal
	// and stops a held button, since the cyno does not carry disallowRepeatingActivation(1014).
	if existing, err := evedb.GetCynoBeaconByShip(ctx, sqldb, ds.shipID); err != nil {
		log.Printf("[%s] cyno: GetCynoBeaconByShip: %v", addr, err)
		return false
	} else if existing != nil {
		cynoNotify(c, accountID, "Your cynosural field is already lit.")
		return false
	}

	// consumeModuleFuel reads consumptionType(713) and consumptionQuantity(714) off the module (500
	// Liquid Ozone), pushes the item changes and sends ModuleRequiresFuel on a short hold.
	if !consumeModuleFuel(ctx, c, addr, accountID, charID, ds, modID, modTypeID,
		dogma.EffNameCynoField, modAttrs, sqldb) {
		return false
	}

	now := server.Win32TimeNow()
	if durationMs <= 0 {
		durationMs = 600000
	}
	pos := ds.entity.Pos()
	rec := evedb.CynoBeacon{
		ShipID:        ds.shipID,
		CharID:        charID,
		ModuleID:      modID,
		ModuleTypeID:  modTypeID,
		SolarSystemID: ds.sysID,
		CorpID:        ds.corpID,
		AllianceID:    ds.allianceID,
		X:             pos.X, Y: pos.Y, Z: pos.Z,
		LitAt:     now,
		ExpiresAt: now + uint64(durationMs)*10000, // ms → FILETIME 100ns ticks
	}
	if globalGangReg != nil {
		rec.GangID = globalGangReg.GetCharGang(charID)
	}
	if err := evedb.LightCynoBeacon(ctx, sqldb, rec); err != nil {
		log.Printf("[%s] cyno: LightCynoBeacon: %v", addr, err)
		return false
	}

	bd, slim, dmg := cynoBeaconBallSlim(rec)
	ds.solarSystem.AddStarbase(bd, slim, dmg)
	broadcastBallAdd(ds.solarSystem, bd, slim, dmg)

	markImmobiliser(ds, modID, modAttrs, modName)
	armCynoExpiry(ds, rec)
	broadcastJumpBeaconChange(rec, true)

	log.Printf("[%s] cyno lit: ship=%d char=%d sys=%d ball=%d expires in %ds",
		addr, rec.ShipID, charID, rec.SolarSystemID, bd.EntityID, durationMs/1000)
	return true
}

// cynoBeaconBallSlim builds the ball/slim/damage triple for one field. SubType 6, not 66: the
// client silently rejects a SubType-66 ball whose slim is not a station out of SetState.
func cynoBeaconBallSlim(r evedb.CynoBeacon) (destiny.BallDesc, destiny.SlimItem, destiny.DamageState) {
	ballID := cynoBeaconID(r.ShipID)
	bd := destiny.BallDesc{
		EntityID: ballID,
		Mode:     destiny.ModeRigid,
		SubType:  6,
		Pos:      destiny.Vec3{X: r.X, Y: r.Y, Z: r.Z},
		Radius:   cynoBeaconRadius,
		Name:     "Cynosural Field",
		CorpID:   uint32(r.CorpID),
	}
	slim := destiny.SlimItem{
		BallID:     int64(ballID),
		ItemID:     int64(ballID),
		TypeID:     cynoBeaconTypeID,
		GroupID:    cynoBeaconGroupID,
		CategoryID: cynoBeaconCatID,
		OwnerID:    r.CharID,
		CorpID:     r.CorpID,
		AllianceID: r.AllianceID,
	}
	return bd, slim, fullDamageState()
}

// ─────────────────────────────────────────────────────────────────────────────
// Expiry and collapse
// ─────────────────────────────────────────────────────────────────────────────

// armCynoExpiry schedules the field's own collapse and remembers the tick id. CancelOnce takes
// the numeric id, and a one-shot left armed would fire against a reissued shipID.
func armCynoExpiry(ds *destinyConn, rec evedb.CynoBeacon) {
	if globalTickMgr == nil {
		return
	}
	remaining := time.Duration(int64(rec.ExpiresAt-server.Win32TimeNow())*100) * time.Nanosecond
	if remaining < 0 {
		remaining = 0
	}
	shipID := rec.ShipID
	id := globalTickMgr.ScheduleOnce(fmt.Sprintf("cyno-expire-%d", shipID),
		time.Now().Add(remaining), func() {
			collapseCynoBeacon(shipID, "expired")
		})
	if ds != nil {
		ds.cynoExpiryTimer = id
	}
}

// cynoCollapseMu serialises collapse so the four callers cannot interleave.
var cynoCollapseMu sync.Mutex

// collapseCynoBeacon puts a field out, whatever ended it (expiry, death, disconnect, GM
// teleport). Idempotent. It takes only a shipID so it works from the tick goroutine and from
// connection teardown, which may have no live destinyConn.
func collapseCynoBeacon(shipID int64, reason string) {
	if shipID == 0 || globalDB == nil {
		return
	}
	cynoCollapseMu.Lock()
	defer cynoCollapseMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rec, err := evedb.GetCynoBeaconByShip(ctx, globalDB, shipID)
	if err != nil {
		log.Printf("[cyno] collapse ship=%d lookup: %v", shipID, err)
		return
	}
	if rec == nil {
		return // already gone
	}
	if err := evedb.DeleteCynoBeacon(ctx, globalDB, shipID); err != nil {
		log.Printf("[cyno] collapse ship=%d delete: %v", shipID, err)
	}

	ballID := cynoBeaconID(shipID)
	if globalSysMgr != nil {
		if ss := globalSysMgr.Get(rec.SolarSystemID); ss != nil {
			ss.RemoveStarbase(ballID)
			broadcastBallRemove(ss, ballID)
		}
	}

	// Lift the pin and drop the module's effect on the pilot's client, if still connected.
	if conn, acct := charConnLookup(rec.CharID); conn != nil {
		if ds := cynoOwnerState(*rec); ds != nil {
			clearImmobiliser(ds, rec.ModuleID)
			if globalTickMgr != nil && ds.cynoExpiryTimer != 0 {
				globalTickMgr.CancelOnce(ds.cynoExpiryTimer)
				ds.cynoExpiryTimer = 0
			}
			stopWeaponFX(conn, "cyno", acct, rec.CharID, ds,
				rec.ModuleID, rec.ModuleTypeID, 0, dogma.EffNameCynoField, globalDB)
		}
	}

	broadcastJumpBeaconChange(*rec, false)
	log.Printf("[cyno] collapsed: ship=%d char=%d sys=%d (%s)",
		shipID, rec.CharID, rec.SolarSystemID, reason)
}

// cynoOwnerState finds the live connection state of the pilot holding a field, through the
// solar system: the entity registry carries PlayerState and no charID map exists.
func cynoOwnerState(rec evedb.CynoBeacon) *destinyConn {
	if globalSysMgr == nil {
		return nil
	}
	ss := globalSysMgr.Get(rec.SolarSystemID)
	if ss == nil {
		return nil
	}
	e := ss.GetPlayerByCharID(rec.CharID)
	if e == nil {
		return nil
	}
	ds, _ := e.PlayerState.(*destinyConn)
	return ds
}

// collapseCynoBeaconForChar is the disconnect path, which has a charID and may lack a
// trustworthy shipID. Logging off collapses the field. See docs/jump-notes.md#cyno.
func collapseCynoBeaconForChar(charID int64, reason string) {
	if charID == 0 || globalDB == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	rec, err := evedb.GetCynoBeaconByChar(ctx, globalDB, charID)
	cancel()
	if err != nil || rec == nil {
		return
	}
	collapseCynoBeacon(rec.ShipID, reason)
}

// ─────────────────────────────────────────────────────────────────────────────
// Fleet notification
// ─────────────────────────────────────────────────────────────────────────────

// broadcastJumpBeaconChange tells the fleet a field went up or out. svc.gang builds activeBeacon
// only from these pushes, so a member who misses one has no "Jump to member" entry. The
// lighting pilot is included, so a gangless pilot sees their own field.
func broadcastJumpBeaconChange(rec evedb.CynoBeacon, active bool) {
	activeVal := uint64(0)
	if active {
		activeVal = 1
	}
	args := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(rec.CharID)},
		marshal.PyInt{Value: uint64(rec.SolarSystemID)},
		marshal.PyInt{Value: uint64(cynoBeaconID(rec.ShipID))},
		marshal.PyInt{Value: activeVal},
	}}

	sent := 0
	send := func(charID int64) {
		if conn, acct := charConnLookup(charID); conn != nil {
			if err := server.SendNotification(conn, acct, "OnJumpBeaconChange", "charid", args); err == nil {
				sent++
			}
		}
	}
	if rec.GangID != 0 && globalGangReg != nil {
		if g := globalGangReg.GetGang(rec.GangID); g != nil {
			for memberID := range g.Members {
				send(memberID)
			}
			log.Printf("[cyno] OnJumpBeaconChange active=%v gang=%d → %d member(s)",
				active, rec.GangID, sent)
			return
		}
	}
	send(rec.CharID)
}

// replayJumpBeaconsToChar re-sends every live field in the character's fleet to one pilot:
// activeBeacon is built only from OnJumpBeaconChange, so a latecomer would have none.
func replayJumpBeaconsToChar(charID int64) {
	if charID == 0 || globalDB == nil || globalGangReg == nil {
		return
	}
	gangID := globalGangReg.GetCharGang(charID)
	if gangID == 0 {
		return
	}
	g := globalGangReg.GetGang(gangID)
	if g == nil {
		return
	}
	conn, acct := charConnLookup(charID)
	if conn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for memberID := range g.Members {
		rec, err := evedb.GetCynoBeaconByChar(ctx, globalDB, memberID)
		if err != nil || rec == nil {
			continue
		}
		args := marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(rec.CharID)},
			marshal.PyInt{Value: uint64(rec.SolarSystemID)},
			marshal.PyInt{Value: uint64(cynoBeaconID(rec.ShipID))},
			marshal.PyInt{Value: 1},
		}}
		_ = server.SendNotification(conn, acct, "OnJumpBeaconChange", "charid", args)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// System load and boot
// ─────────────────────────────────────────────────────────────────────────────

// loadCynoBeaconsIntoSystem rebuilds the fields burning in a freshly loaded system and re-arms
// their expiry. A field whose time ran out is deleted. Failure is logged and never blocks the
// undock, since this runs on the first-player-in path of every system.
func loadCynoBeaconsIntoSystem(addr string, sqldb *sql.DB, ss *destiny.SolarSystem, sysID int64) {
	if sqldb == nil || ss == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	now := server.Win32TimeNow()
	recs, err := evedb.ListCynoBeaconsForSystem(ctx, sqldb, sysID, now)
	if err != nil {
		log.Printf("[%s] loadCynoBeaconsIntoSystem sysID=%d: %v", addr, sysID, err)
		return
	}
	for _, r := range recs {
		bd, slim, dmg := cynoBeaconBallSlim(r)
		ss.AddStarbase(bd, slim, dmg)
		// The pilot who lit it may not be connected (restart), so the one-shot is armed without a destinyConn.
		armCynoExpiry(nil, r)
		log.Printf("[%s] cyno beacon loaded: ship=%d char=%d sys=%d %.0fs left",
			addr, r.ShipID, r.CharID, sysID, float64(r.ExpiresAt-now)/1e7)
	}
}

// runCynoBootReconcile ensures the schema and clears fields that burned out
// while the server was down.
func runCynoBootReconcile(sqldb *sql.DB) {
	if sqldb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := evedb.EnsureCynoSchema(ctx, sqldb); err != nil {
		log.Printf("[cyno] EnsureCynoSchema: %v", err)
		return
	}
	if n, err := evedb.PurgeExpiredCynoBeacons(ctx, sqldb, server.Win32TimeNow()); err != nil {
		log.Printf("[cyno] PurgeExpiredCynoBeacons: %v", err)
	} else if n > 0 {
		log.Printf("[cyno] boot: cleared %d expired beacon(s)", n)
	}
}

// startCynoExpirySweep collapses any field whose one-shot was lost; the one-shot is the
// mechanism and this is the backstop.
func startCynoExpirySweep(sqldb *sql.DB) {
	if globalTickMgr == nil || sqldb == nil {
		return
	}
	globalTickMgr.RegisterPeriodic("cyno-expiry-sweep", 60, func(uint64) bool {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		recs, err := evedb.ListExpiredCynoBeacons(ctx, sqldb, server.Win32TimeNow())
		cancel()
		if err != nil {
			log.Printf("[cyno] expiry sweep: %v", err)
			return true
		}
		for _, r := range recs {
			collapseCynoBeacon(r.ShipID, "expiry sweep")
		}
		return true
	})
}
