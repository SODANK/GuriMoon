// Outpost construction: the client flow that turns an Outpost Construction
// Platform into a station.
//
//	manufacture platform (the BPO's component mix decides the station's services)
//	  → ship.Drop            "Launch for corp"    (shared with the POS path)
//	  → dogmaIM.Activate 649 "Anchor"             anchorPlatformForPlayer
//	  → GetInventoryFromId(id).List()/Add()       the staging hold
//	  → GetInventoryFromId(id).Build()            buildOutpostForPlayer
//
// Design notes: db/outpostdb.go.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"gurimoon/corp"
	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/esp"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
	"gurimoon/server/mapsvc"
)

// skillOutpostConstruction is invTypes 3400, requiredSkill1 on all four
// platform types at level 1.  The client greys its buttons on skills but never
// enforces them, so the server is the gate.
const skillOutpostConstruction = 3400

// maxAnchorPlanetDist is how far from a planet a platform may anchor, measured
// from the surface (NearestPlanet), so it is clearance above the body.
const maxAnchorPlanetDist = 100_000_000.0 // 100,000 km

// ─── identification ────────────────────────────────────────────────────────

// isPlatformBall reports whether itemID is a construction platform in the
// caller's current system, anchored or free. Platforms live in the starbase
// ball slices (they launch through ship.Drop), so the group is the discriminator.
func isPlatformBall(ds *destinyConn, itemID int64) bool {
	if ds == nil || ds.solarSystem == nil || itemID <= 0 {
		return false
	}
	slim, ok := ds.solarSystem.GetStarbaseSlim(uint32(itemID))
	return ok && slim.GroupID == evedb.GroupConstructionPlatform
}

// isStructureBay reports whether itemID is an in-space container the owner can
// open: a POS structure bay (category 23) or a platform's staging hold (group
// 307). Both store contents at locationID = structure, flag 0. isStarbaseBay
// stays for the places that mean towers specifically.
func isStructureBay(ds *destinyConn, itemID int64) bool {
	return isStarbaseBay(ds, itemID) || isPlatformBall(ds, itemID)
}

// ─── anchor / unanchor (dogmaIM generic anchor effects) ────────────────────

// handlePlatformEffect processes anchorDrop/anchorLift against a construction
// platform.  Returns true when the call was consumed.
func handlePlatformEffect(ds *destinyConn, addr string, charID, itemID, effectID int64, activate bool, sqldb *sql.DB) bool {
	if !activate || !isPlatformBall(ds, itemID) {
		return false
	}
	switch effectID {
	case effectAnchorDrop:
		if err := anchorPlatformForPlayer(ds, addr, charID, itemID, sqldb); err != nil {
			log.Printf("[%s] platform %d anchor refused: %v", addr, itemID, err)
			platformNotify(ds, "Cannot anchor: "+err.Error())
		}
		return true
	case effectAnchorLift:
		if err := unanchorPlatformForPlayer(ds, addr, charID, itemID, sqldb); err != nil {
			log.Printf("[%s] platform %d unanchor refused: %v", addr, itemID, err)
			platformNotify(ds, "Cannot unanchor: "+err.Error())
		}
		return true
	}
	return false
}

// platformNotify puts a refusal in the caller's message window through
// OnRemoteMessage: dogmaIM.Activate's response has already gone out, so no
// exception is available.
func platformNotify(ds *destinyConn, text string) {
	if ds == nil || ds.entity == nil || ds.entity.Conn == nil {
		return
	}
	args := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: "CustomNotify"},
		marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "notify"}, Value: marshal.PyString{Value: text}},
		}},
	}}
	_ = server.SendNotification(ds.entity.Conn, int64(ds.entity.AccountID),
		"OnRemoteMessage", "charid", args)
}

// anchorPlatformForPlayer implements the Anchor menu entry for a platform.
// The client's generic AnchorObject sends no coordinates: the platform anchors
// wherever its ball already is.
func anchorPlatformForPlayer(ds *destinyConn, addr string, charID, itemID int64, sqldb *sql.DB) error {
	if ds.solarSystem == nil || sqldb == nil {
		return fmt.Errorf("not in space")
	}
	ss := ds.solarSystem
	slim, ok := ss.GetStarbaseSlim(uint32(itemID))
	if !ok || slim.GroupID != evedb.GroupConstructionPlatform {
		return fmt.Errorf("item %d is not a construction platform here", itemID)
	}
	if slim.PosState == nil || *slim.PosState != 0 {
		return fmt.Errorf("platform %d is already anchored", itemID)
	}
	pos, posOK := ss.GetStarbasePosition(uint32(itemID))
	if !posOK {
		return fmt.Errorf("platform %d has no ball", itemID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_, corpID, allianceID, err := evedb.GetCharSovContext(ctx, sqldb, charID)
	if err != nil {
		return err
	}
	if slim.CorpID != corpID {
		return fmt.Errorf("that platform belongs to another corporation")
	}
	if !mayFoundOutpost(ctx, sqldb, corpID, charID) {
		return fmt.Errorf("only the CEO or a Director may anchor an outpost platform")
	}
	if lvl, err := evedb.GetCharSkillLevel(ctx, sqldb, charID, skillOutpostConstruction); err == nil && lvl < 1 {
		return fmt.Errorf("Outpost Construction level 1 is required")
	}

	// Nullsec only, and the caller's alliance must already hold the system. An
	// outpost never auto-claims sovereignty: dropping one must not take a system.
	sec, secOK, err := evedb.GetSystemSecurity(ctx, sqldb, ds.sysID)
	if err != nil {
		return err
	}
	if secOK && sec > 0.0 {
		return fmt.Errorf("outposts may only be anchored in 0.0 space (this system is %.1f)", sec)
	}
	if allianceID == 0 {
		return fmt.Errorf("outposts require an alliance holding sovereignty in this system")
	}
	sov, held, err := evedb.GetSov(ctx, sqldb, ds.sysID)
	if err != nil {
		return err
	}
	if !held {
		return fmt.Errorf("no alliance holds sovereignty in this system")
	}
	if sov.AllianceID != allianceID {
		return fmt.Errorf("this system is held by another alliance")
	}

	planetID, planetName, dist, foundPlanet, err := evedb.NearestPlanet(ctx, sqldb, ds.sysID, pos.X, pos.Y, pos.Z)
	if err != nil {
		return err
	}
	if !foundPlanet || dist > maxAnchorPlanetDist {
		return fmt.Errorf("an outpost must be anchored within %.0f km of a planet (nearest: %.0f km)",
			maxAnchorPlanetDist/1000, dist/1000)
	}
	occupied, what, err := evedb.PlanetOccupancy(ctx, sqldb, planetID)
	if err != nil {
		return err
	}
	if occupied {
		return fmt.Errorf("%s already has %s", planetName, what)
	}

	if err := evedb.AnchorPlatform(ctx, sqldb, itemID, ds.sysID, planetID, corpID, allianceID,
		pos.X, pos.Y, pos.Z); err != nil {
		return err
	}

	rec, found, err := evedb.GetOutpostPlatform(ctx, sqldb, itemID)
	if err != nil || !found {
		return fmt.Errorf("anchored but could not reload platform %d: %w", itemID, err)
	}
	swapPlatformBall(ss, rec, slim.AllianceID)
	log.Printf("[%s] platform %d anchored at planet %s (%.0f km) by char %d (corp %d)",
		addr, itemID, planetName, dist/1000, charID, corpID)
	return nil
}

// unanchorPlatformForPlayer returns an anchored platform to a free in-space
// item, which is then scoopable. Refused while the hold has anything in it:
// scooping the platform would take its contents out of the game.
func unanchorPlatformForPlayer(ds *destinyConn, addr string, charID, itemID int64, sqldb *sql.DB) error {
	if ds.solarSystem == nil || sqldb == nil {
		return fmt.Errorf("not in space")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rec, found, err := evedb.GetOutpostPlatform(ctx, sqldb, itemID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("platform %d is not anchored", itemID)
	}
	_, corpID, allianceID, err := evedb.GetCharSovContext(ctx, sqldb, charID)
	if err != nil {
		return err
	}
	if rec.OwnerCorpID != corpID {
		return fmt.Errorf("that platform belongs to another corporation")
	}
	if !mayFoundOutpost(ctx, sqldb, corpID, charID) {
		return fmt.Errorf("only the CEO or a Director may unanchor an outpost platform")
	}
	if n, err := evedb.CountContainerItems(ctx, sqldb, itemID); err == nil && n > 0 {
		return fmt.Errorf("empty the platform's hold first (%d stacks inside)", n)
	}
	if err := evedb.UnanchorPlatform(ctx, sqldb, itemID); err != nil {
		return err
	}

	ss := ds.solarSystem
	st := evedb.LaunchedStructure{
		ItemID: rec.EntityID, TypeID: rec.TypeID,
		GroupID: evedb.GroupConstructionPlatform, CategoryID: 2,
		TypeName: rec.TypeName, Radius: 1000, Mass: 100000,
	}
	bd, newSlim, dmg := launchedStructureBallSlim(st,
		destiny.Vec3{X: rec.X, Y: rec.Y, Z: rec.Z}, corpID, corpID, allianceID)
	ss.RemoveStarbase(uint32(itemID))
	ss.AddStarbase(bd, newSlim, dmg)
	broadcastBallSwap(ss, uint32(itemID), bd, newSlim, dmg)
	log.Printf("[%s] platform %d unanchored by char %d", addr, itemID, charID)
	return nil
}

// swapPlatformBall replaces the free launch ball with the anchored
// presentation (posState 1) and tells everyone in the system.
func swapPlatformBall(ss *destiny.SolarSystem, rec evedb.OutpostPlatform, allianceID int64) {
	posState := int64(1) // STRUCTURE_ANCHORED
	bd := destiny.BallDesc{
		EntityID: uint32(rec.EntityID),
		Mode:     destiny.ModeRigid,
		SubType:  6, // Global, non-massive, like a tower; not 66, which is docking geometry
		Pos:      destiny.Vec3{X: rec.X, Y: rec.Y, Z: rec.Z},
		Radius:   1000,
		Name:     rec.TypeName,
		CorpID:   uint32(rec.OwnerCorpID),
	}
	slim := destiny.SlimItem{
		BallID: rec.EntityID, ItemID: rec.EntityID,
		TypeID: rec.TypeID, GroupID: evedb.GroupConstructionPlatform, CategoryID: 2,
		OwnerID: rec.OwnerCorpID, CorpID: rec.OwnerCorpID,
		AllianceID: rec.OwnerAllianceID,
		PosState:   &posState,
		// posTimestamp stays 0 (None): the client raised its own AnchoringObject
		// countdown at the menu click; a past timestamp on state 1 re-renders it.
	}
	dmg := fullDamageState()
	ss.RemoveStarbase(uint32(rec.EntityID))
	ss.AddStarbase(bd, slim, dmg)
	broadcastBallSwap(ss, uint32(rec.EntityID), bd, slim, dmg)
}

// loadOutpostPlatformsIntoSystem rebuilds a system's platforms, anchored and
// free, when it is populated. The row outlives the ball, and the Anchor and
// Build menus hang off the ball.
func loadOutpostPlatformsIntoSystem(addr string, sqldb *sql.DB, ss *destiny.SolarSystem, sysID int64) {
	if sqldb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	recs, err := evedb.ListOutpostPlatformsForSystem(ctx, sqldb, sysID)
	if err != nil {
		log.Printf("[%s] loadOutpostPlatformsIntoSystem sysID=%d: %v", addr, sysID, err)
		return
	}
	for _, rec := range recs {
		posState := int64(1) // STRUCTURE_ANCHORED
		bd := destiny.BallDesc{
			EntityID: uint32(rec.EntityID),
			Mode:     destiny.ModeRigid,
			SubType:  6,
			Pos:      destiny.Vec3{X: rec.X, Y: rec.Y, Z: rec.Z},
			Radius:   1000,
			Name:     rec.TypeName,
			CorpID:   uint32(rec.OwnerCorpID),
		}
		slim := destiny.SlimItem{
			BallID: rec.EntityID, ItemID: rec.EntityID,
			TypeID: rec.TypeID, GroupID: evedb.GroupConstructionPlatform, CategoryID: 2,
			OwnerID: rec.OwnerCorpID, CorpID: rec.OwnerCorpID,
			AllianceID: rec.OwnerAllianceID,
			PosState:   &posState,
		}
		ss.AddStarbase(bd, slim, fullDamageState())
		log.Printf("[%s] outpost platform loaded: entityID=%d typeID=%d planet=%q owner=corp:%d",
			addr, rec.EntityID, rec.TypeID, rec.PlanetName, rec.OwnerCorpID)
	}

	// Free platforms too: a system is torn down when its last player leaves, and
	// the Anchor menu hangs off the ball.
	free, err := evedb.ListFreePlatformsForSystem(ctx, sqldb, sysID)
	if err != nil {
		log.Printf("[%s] loadOutpostPlatformsIntoSystem free sysID=%d: %v", addr, sysID, err)
		return
	}
	for _, p := range free {
		st := evedb.LaunchedStructure{
			ItemID: p.ItemID, TypeID: p.TypeID,
			GroupID: evedb.GroupConstructionPlatform, CategoryID: 2,
			TypeName: p.TypeName, Radius: 1000, Mass: 100000,
		}
		allianceID, _ := evedb.GetCorpAllianceID(ctx, sqldb, p.OwnerID)
		bd, slim, dmg := launchedStructureBallSlim(st,
			destiny.Vec3{X: p.X, Y: p.Y, Z: p.Z}, p.OwnerID, p.OwnerID, allianceID)
		ss.AddStarbase(bd, slim, dmg)
		log.Printf("[%s] unanchored platform loaded: itemID=%d typeID=%d owner=%d",
			addr, p.ItemID, p.TypeID, p.OwnerID)
	}
}



// mayFoundOutpost reports whether charID may found, unanchor or build an
// outpost for corpID: CEO or Director. The client gates none of them.
func mayFoundOutpost(ctx context.Context, sqldb *sql.DB, corpID, charID int64) bool {
	if isCorpCEO(ctx, sqldb, corpID, charID) {
		return true
	}
	return corp.HasRole(callerRoles(ctx, sqldb, charID), corp.RoleDirector)
}

// ─── Build ─────────────────────────────────────────────────────────────────

// buildOutpostForPlayer implements InventoryBound.Build() against an anchored platform and answers
// the CallRsp itself. A password refusal must be a UserError with the literal key
// 'PermissionDenied': menusvc's _BuildConstructionPlatform raises its password prompt and retries
// with securityCode= only on that key.
func buildOutpostForPlayer(
	c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID, charID, platformID int64,
	securityCode string, sqldb *sql.DB, ds *destinyConn,
) error {
	refuse := func(format string, a ...any) error {
		msg := fmt.Sprintf(format, a...)
		log.Printf("[%s] outpost Build(%d) refused: %s", addr, platformID, msg)
		return server.SendCallException(c, pkt, accountID, server.BuildUserError(
			"CustomError", map[string]marshal.PyRep{
				"error": marshal.PyString{Value: msg},
			}))
	}

	if sqldb == nil || ds == nil || ds.solarSystem == nil {
		return refuse("You must be in space to build an outpost.")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rec, found, err := evedb.GetOutpostPlatform(ctx, sqldb, platformID)
	if err != nil {
		return refuse("Could not read the platform: %v", err)
	}
	if !found {
		return refuse("That platform is not anchored yet.")
	}
	if rec.SolarSystemID != ds.sysID {
		return refuse("That platform is in another system.")
	}

	_, corpID, allianceID, err := evedb.GetCharSovContext(ctx, sqldb, charID)
	if err != nil {
		return refuse("Could not read your corporation: %v", err)
	}
	if rec.OwnerCorpID != corpID {
		return refuse("That platform belongs to another corporation.")
	}
	if !mayFoundOutpost(ctx, sqldb, corpID, charID) {
		return refuse("Only the CEO or a Director may complete an outpost.")
	}

	// The build password is the one refusal that must carry the client's own
	// key so its prompt-and-retry loop engages.
	_, buildPw, err := evedb.GetPlatformPasswords(ctx, sqldb, platformID)
	if err != nil {
		return refuse("Could not read the platform's passwords: %v", err)
	}
	if buildPw != "" && securityCode != buildPw {
		return server.SendCallException(c, pkt, accountID,
			server.BuildUserError("PermissionDenied", nil))
	}

	if lvl, err := evedb.GetCharSkillLevel(ctx, sqldb, charID, skillOutpostConstruction); err == nil && lvl < 1 {
		return refuse("Outpost Construction level 1 is required.")
	}

	// The anchoring delay is checked against the stored timestamp, not a timer
	// a restart loses.
	delayMs, hasDelay, err := evedb.GetTypeAttributeFloat(ctx, sqldb, rec.TypeID, "anchoringDelay")
	if err != nil {
		return refuse("Could not read the anchoring delay: %v", err)
	}
	if hasDelay {
		remaining := platformAnchorRemaining(rec.AnchoredAt, delayMs)
		if remaining > 0 {
			return refuse("The platform is still anchoring — %s remaining.", remaining.Round(time.Second))
		}
	}

	// Sovereignty is re-checked: the system can change hands during the delay.
	sov, held, err := evedb.GetSov(ctx, sqldb, ds.sysID)
	if err != nil {
		return refuse("Could not read sovereignty: %v", err)
	}
	if !held || allianceID == 0 || sov.AllianceID != allianceID {
		return refuse("Your alliance no longer holds sovereignty in this system.")
	}

	stationTypeID, ok, err := evedb.PlatformStationTypeID(ctx, sqldb, rec.TypeID)
	if err != nil || !ok {
		return refuse("Type %d is not a construction platform.", rec.TypeID)
	}

	stationID, err := evedb.CreateOutpost(ctx, sqldb, evedb.NewOutpost{
		PlatformID:    rec.EntityID,
		SolarSystemID: rec.SolarSystemID,
		PlanetID:      rec.PlanetID,
		OwnerCorpID:   rec.OwnerCorpID,
		StationTypeID: stationTypeID,
		X:             rec.X, Y: rec.Y, Z: rec.Z,
	})
	if err != nil {
		return refuse("Construction failed: %v", err)
	}

	commissionOutpost(addr, sqldb, ds.solarSystem, uint32(rec.EntityID), stationID)
	log.Printf("[%s] outpost %d built from platform %d by char %d (corp %d) in system %d",
		addr, stationID, rec.EntityID, charID, corpID, rec.SolarSystemID)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// platformAnchorRemaining reports how much of the anchoring delay is left.
// anchoredAt is a Win32 FILETIME (100-ns ticks since 1601) and delayMs is the
// dogma attribute in milliseconds.
func platformAnchorRemaining(anchoredAt uint64, delayMs float64) time.Duration {
	if anchoredAt == 0 {
		return 0
	}
	elapsed := time.Duration(server.Win32TimeNow()-anchoredAt) * 100 * time.Nanosecond
	total := time.Duration(delayMs) * time.Millisecond
	if elapsed >= total {
		return 0
	}
	return total - elapsed
}

// buildSecurityCode pulls the optional securityCode= keyword off a Build call.
// The client sends Build() bare first and Build(securityCode=pw) after a
// PermissionDenied; the argument is a keyword, never positional.
func buildSecurityCode(cs *server.CallStream) string {
	if cs == nil || cs.ArgDict == nil {
		return ""
	}
	for _, e := range cs.ArgDict.Items {
		k, ok := e.Key.(marshal.PyString)
		if !ok || k.Value != "securityCode" {
			continue
		}
		if v, ok := e.Value.(marshal.PyString); ok {
			return v.Value
		}
	}
	return ""
}

// ─── SetPassword ───────────────────────────────────────────────────────────

// setPlatformPasswordForPlayer implements InventoryBound.SetPassword(which, old, new); which is the
// client's constant, 1 = access, 2 = build. Both menu entries need the player docked, so the call
// usually names a platform in a hangar with no ball: passwords key on the itemID and
// platformBindTarget recognises a hangar platform.
func setPlatformPasswordForPlayer(
	c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID, charID, platformID int64,
	which int64, oldPw, newPw string, sqldb *sql.DB, ds *destinyConn,
) error {
	fail := func(format string, a ...any) error {
		msg := fmt.Sprintf(format, a...)
		log.Printf("[%s] platform SetPassword(%d) refused: %s", addr, platformID, msg)
		return server.SendCallException(c, pkt, accountID, server.BuildUserError(
			"CustomError", map[string]marshal.PyRep{
				"error": marshal.PyString{Value: msg},
			}))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, corpID, _, err := evedb.GetCharSovContext(ctx, sqldb, charID)
	if err != nil {
		return fail("Could not read your corporation: %v", err)
	}
	// In a hangar, or a ball owned by the caller's corp. Secure containers and ALSCs
	// share this method name and have no server-side passwords: silent no-op, not a refusal.
	if !evedb.IsHangarPlatform(ctx, sqldb, platformID, charID, corpID) {
		rec, found, err := evedb.GetOutpostPlatform(ctx, sqldb, platformID)
		if err != nil || !found {
			log.Printf("[%s] SetPassword on %d: not a construction platform, ignored", addr, platformID)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		if rec.OwnerCorpID != corpID {
			return fail("That platform is not yours to configure.")
		}
	}
	if !mayFoundOutpost(ctx, sqldb, corpID, charID) {
		return fail("Only the CEO or a Director may set an outpost platform's passwords.")
	}
	if err := evedb.SetPlatformPassword(ctx, sqldb, platformID, which, oldPw, newPw); err != nil {
		return fail("%v", err)
	}
	log.Printf("[%s] platform %d password %d set by char %d", addr, platformID, which, charID)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// parseSetPasswordArgs unpacks SetPassword(which, old, new).  old is None the
// first time a password is set, so a missing or non-string value is "".
func parseSetPasswordArgs(cs *server.CallStream) (which int64, oldPw, newPw string, ok bool) {
	if len(cs.ArgTuple.Items) < 3 {
		return 0, "", "", false
	}
	w, isInt := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !isInt {
		return 0, "", "", false
	}
	str := func(r marshal.PyRep) string {
		if s, ok := r.(marshal.PyString); ok {
			return s.Value
		}
		return ""
	}
	return int64(w.Value), str(cs.ArgTuple.Items[1]), str(cs.ArgTuple.Items[2]), true
}

// isDockedHangarPlatform reports whether a docked caller's GetInventoryFromId
// names a construction platform in a hangar. The probe runs only with no solar
// system (in space, platforms bind as structure bays) and only for IDs in the
// entity band, so the hot bind path is not given a query per call.
func isDockedHangarPlatform(sqldb *sql.DB, ds *destinyConn, charID, itemID int64) bool {
	if sqldb == nil || charID == 0 || itemID < 100_000_000 {
		return false
	}
	if ds != nil && ds.solarSystem != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	corpID, err := evedb.GetCharCorpID(ctx, sqldb, charID)
	if err != nil {
		corpID = 0
	}
	return evedb.IsHangarPlatform(ctx, sqldb, itemID, charID, corpID)
}

// ─── commissioning a new station into a live system ────────────────────────

// commissionOutpost makes a station that did not exist at boot visible without
// a restart: three caches assume the station set is fixed for the life of the
// process, and the pilots on the grid are told directly.
func commissionOutpost(addr string, sqldb *sql.DB, ss *destiny.SolarSystem, platformID uint32, stationID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var ownerCorp int64
	var stationName string
	if evedb.WorldEventsWanted() && sqldb.QueryRowContext(ctx, `SELECT corporationID, stationName FROM staStations WHERE stationID = ?`,
		stationID).Scan(&ownerCorp, &stationName) == nil && ownerCorp != 0 {
		evedb.EmitWorldEvent(evedb.OutpostCompletedEvent{At: time.Now(), CorpID: ownerCorp, StationID: stationID, StationName: stationName})
	}
	// An outpost counts twice a tower toward sovereignty (db/sovmajority.go).
	if _, err := evedb.RecomputeSystemSov(ctx, sqldb, ss.ID); err != nil {
		log.Printf("[%s] commissionOutpost: sov recompute %d: %v", addr, ss.ID, err)
	}

	// 1. Drop the caches that would otherwise answer from a snapshot taken
	//    before the station existed (a new station is also a new owner name).
	refreshStationOwnerCaches(ss.ID)

	// 2. Put the station into the system's shared celestial slices so the next
	//    pilot to arrive gets it in their SetState.
	cels, err := evedb.GetSystemCelestials(ctx, sqldb, ss.ID)
	if err != nil {
		log.Printf("[%s] commissionOutpost: reload celestials: %v", addr, err)
		return
	}
	var newCel *evedb.CelestialEntity
	for i := range cels {
		if cels[i].ItemID == stationID {
			newCel = &cels[i]
			break
		}
	}
	if newCel == nil {
		log.Printf("[%s] commissionOutpost: station %d missing from its own system reload", addr, stationID)
		return
	}
	// Both through the shared builders, so a mid-session station is identical to
	// the same station after a restart (SubType-66 miniball, owner default).
	bd := makeCelestialBallDescs([]evedb.CelestialEntity{*newCel})[0]
	slim := makeCelestialSlimItems([]evedb.CelestialEntity{*newCel})[0]
	dmg := fullDamageState()
	ss.AddCelestial(bd, slim, dmg)

	// 3. Register it as a shootable entity: an outpost is conquerable from the
	//    moment it exists, and nothing else runs this until the next system load.
	registerConquerableStation(ss, *newCel, sqldb, addr)
	if e := ss.GetStationEntity(uint32(stationID)); e != nil {
		ss.SetCelDamage(uint32(stationID), e.Damage)
		dmg = e.Damage
	}

	// 4. Swap the platform ball for the station ball on every grid.
	ss.RemoveStarbase(platformID)
	broadcastBallSwap(ss, platformID, bd, slim, dmg)
}

// ─── admin panel hooks ─────────────────────────────────────────────────────

// installOutpostESPHooks wires the panel's Force Build and Remove buttons to
// the live paths, so an admin action drops the same caches and swaps the same
// balls a player's Build does.  Called once at boot.
func installOutpostESPHooks(sqldb *sql.DB, sysMgr *destiny.SystemManager) {
	esp.PlatformForceBuilder = func(platformID int64) (int64, error) {
		return forceBuildOutpost(sqldb, sysMgr, platformID)
	}
	esp.PlatformRemover = func(platformID int64) error {
		return removeOutpostPlatform(sqldb, sysMgr, platformID)
	}
}

// sysMgrGet returns a loaded system or nil. A system nobody is in is not
// loaded; the tables carry the change and the next arrival builds from them.
func sysMgrGet(sysMgr *destiny.SystemManager, sysID int64) *destiny.SolarSystem {
	if sysMgr == nil {
		return nil
	}
	return sysMgr.Get(sysID)
}

// forceBuildOutpost completes a platform from the admin panel, skipping the
// role, skill and password gates. Sovereignty and the anchoring delay still apply.
func forceBuildOutpost(sqldb *sql.DB, sysMgr *destiny.SystemManager, platformID int64) (int64, error) {
	if sqldb == nil {
		return 0, fmt.Errorf("no database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rec, found, err := evedb.GetOutpostPlatform(ctx, sqldb, platformID)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, fmt.Errorf("platform %d is not anchored", platformID)
	}
	if delayMs, ok, err := evedb.GetTypeAttributeFloat(ctx, sqldb, rec.TypeID, "anchoringDelay"); err == nil && ok {
		if left := platformAnchorRemaining(rec.AnchoredAt, delayMs); left > 0 {
			return 0, fmt.Errorf("still anchoring — %s remaining", left.Round(time.Second))
		}
	}
	if _, held, err := evedb.GetSov(ctx, sqldb, rec.SolarSystemID); err != nil {
		return 0, err
	} else if !held {
		return 0, fmt.Errorf("no alliance holds sovereignty in system %d", rec.SolarSystemID)
	}
	stationTypeID, ok, err := evedb.PlatformStationTypeID(ctx, sqldb, rec.TypeID)
	if err != nil || !ok {
		return 0, fmt.Errorf("type %d is not a construction platform", rec.TypeID)
	}

	stationID, err := evedb.CreateOutpost(ctx, sqldb, evedb.NewOutpost{
		PlatformID:    rec.EntityID,
		SolarSystemID: rec.SolarSystemID,
		PlanetID:      rec.PlanetID,
		OwnerCorpID:   rec.OwnerCorpID,
		StationTypeID: stationTypeID,
		X:             rec.X, Y: rec.Y, Z: rec.Z,
	})
	if err != nil {
		return 0, err
	}
	// A system nobody is standing in has no live state to update; the station
	// is in the tables either way and the next arrival builds it from those.
	if ss := sysMgrGet(sysMgr, rec.SolarSystemID); ss != nil {
		commissionOutpost("esp", sqldb, ss, uint32(rec.EntityID), stationID)
	} else if globalCelestialCache != nil {
		globalCelestialCache.InvalidateSystem(rec.SolarSystemID)
		mapsvc.InvalidateStationExtraInfo()
		if globalCacheStore != nil {
			globalCacheStore.InvalidateOwnerNames()
		}
	}
	log.Printf("[esp] outpost %d force-built from platform %d", stationID, platformID)
	return stationID, nil
}

// removeOutpostPlatform deletes an anchored platform, its hold and its ball.
func removeOutpostPlatform(sqldb *sql.DB, sysMgr *destiny.SystemManager, platformID int64) error {
	if sqldb == nil {
		return fmt.Errorf("no database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	rec, found, err := evedb.GetOutpostPlatform(ctx, sqldb, platformID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("platform %d is not anchored", platformID)
	}
	if err := evedb.DeleteOutpostPlatform(ctx, sqldb, platformID); err != nil {
		return err
	}
	if ss := sysMgrGet(sysMgr, rec.SolarSystemID); ss != nil {
		ss.RemoveStarbase(uint32(platformID))
		for _, p := range ss.GetAllPlayersInSystem() {
			if p.Conn == nil {
				continue
			}
			p.SendStamped(func(stamp uint32) error {
				return destiny.SendRemoveBalls(p.Conn, globalNodeID, p.AccountID, stamp,
					[]uint32{uint32(platformID)})
			})
		}
	}
	log.Printf("[esp] outpost platform %d removed", platformID)
	return nil
}

// ─── AssembleConstructionPlatform ──────────────────────────────────────────

// assembleConstructionPlatform implements inventorymgr.AssembleConstructionPlatform(itemID,
// password, ?, 0.0), four args where the cargo-container sibling takes three. The client's menu
// entry never draws for a packaged item; this keeps the RPC from being a silent fallthrough.
func assembleConstructionPlatform(ctx context.Context, sqldb *sql.DB, charID, itemID int64) error {
	res, err := sqldb.ExecContext(ctx, `
		UPDATE entity e
		  JOIN invTypes t ON t.typeID = e.typeID
		   SET e.singleton = 1
		 WHERE e.itemID = ? AND e.ownerID = ? AND t.groupID = ? AND e.quantity = 1`,
		itemID, charID, evedb.GroupConstructionPlatform)
	if err != nil {
		return fmt.Errorf("assembleConstructionPlatform: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("item %d is not an unassembled platform you own", itemID)
	}
	return nil
}
