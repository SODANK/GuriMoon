package main

// Ejecting from a ship in space, and boarding an unpiloted one.
//
// Eject (menusvc.Eject → ship.Eject under PerformSessionChange): the pilot
// gets a fresh capsule beside the hull and the hull stays where it floats,
// unpiloted, still owned by them. Board on such a hull (menusvc "Board Ship")
// moves the boarder in, makes them the owner of the hull and what it holds,
// and offlines the modules they lack the skills for or the fitting skills to
// power. A boarder who was in a capsule loses it; one in another ship leaves
// that ship floating unpiloted in turn.
//
// An unpiloted hull rides the loot slices like a corpse (destiny
// AddAbandonedShip): grid delivery, SetState and warp arrival come for free,
// it is never a container and never expires. entity.customInfo marks it so a
// system reload restores it (db/abandonedship.go).
//
// The client builds a fresh ship moniker per call (moniker.GetShipAccess), so
// these calls usually arrive inline on MachoBindObject. Both paths go through
// shipSpaceCall: a refusal is raised as an exception, the only answer that
// releases the client's 30 s session-change lock; on success the reply goes
// out first and the swap and its shipid session change follow.

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
)

// boardInSpaceRange is how close (metres, centre to hull surface) a pilot must
// be to board an unpiloted ship.
const boardInSpaceRange = 6500.0

// shipSpaceCall runs ship.Eject, ship.Evacuate or an in-space ship.Board.
// It returns the UserError to raise, or a function to run once the reply
// (plain, or the inline bind tuple) has gone out.
func shipSpaceCall(c *evenet.Conn, addr string, accountID, charID int64, method string,
	args []marshal.PyRep, sqldb *sql.DB, ds *destinyConn) (refusal marshal.PyRep, after func()) {

	switch method {
	case "Evacuate":
		return customError("Evacuating a ship's clone vats is not supported."), nil
	case "Eject":
		return ejectFromShip(c, addr, accountID, charID, sqldb, ds)
	case "Board":
		var shipID int64
		if len(args) >= 1 {
			if v, ok := args[0].(marshal.PyInt); ok {
				shipID = int64(v.Value)
			}
		}
		return boardUnpilotedShip(c, addr, accountID, charID, shipID, sqldb, ds)
	}
	return customError("That is not possible here."), nil
}

func customError(text string) marshal.PyRep {
	return server.BuildUserError("CustomError", map[string]marshal.PyRep{
		"error": marshal.PyString{Value: text}})
}

// inSpace reports whether ds has a live ship ball in a loaded system.
func inSpace(ds *destinyConn, sqldb *sql.DB) bool {
	return sqldb != nil && ds != nil && ds.entity != nil && ds.solarSystem != nil && ds.shipID != 0
}

// ejectFromShip leaves the pilot's hull unpiloted where it floats and puts the
// pilot in a new capsule beside it.
func ejectFromShip(c *evenet.Conn, addr string, accountID, charID int64,
	sqldb *sql.DB, ds *destinyConn) (marshal.PyRep, func()) {

	if !inSpace(ds, sqldb) {
		return customError("You can only eject from a ship in space."), nil
	}
	if ds.shipGroupID == groupCapsule {
		return customError("You cannot eject from a capsule."), nil
	}
	if ds.entity.IsInWarp() {
		return customError("You cannot eject while warping."), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	oldShip := ds.shipID
	bd, slim, dmg := unpilotedShipBall(ctx, sqldb, ds)
	pos := bd.Pos

	capsuleID, err := evedb.CreateCapsuleEntity(ctx, sqldb, charID, ds.sysID, ds.charName)
	if err != nil {
		log.Printf("[%s] Eject capsule: %v", addr, err)
		return customError("Could not eject you into a capsule."), nil
	}
	// The pod pops out just clear of the hull.
	podPos := destiny.Vec3{X: pos.X, Y: pos.Y + bd.Radius + 50, Z: pos.Z}
	_ = evedb.UpdateEntityPosition(ctx, sqldb, capsuleID, podPos.X, podPos.Y, podPos.Z)

	persistShipDamage(ds, sqldb)
	if err := evedb.MarkShipAbandoned(ctx, sqldb, oldShip, ds.sysID, pos.X, pos.Y, pos.Z); err != nil {
		log.Printf("[%s] Eject: %v", addr, err)
		_ = evedb.DeleteEntity(ctx, sqldb, capsuleID)
		return customError("Could not eject you from the ship."), nil
	}

	return nil, func() {
		swapPilotInSpace(c, addr, accountID, charID, sqldb, ds, capsuleID,
			oldHull{unpiloted: &unpilotedHull{bd: bd, slim: slim, dmg: dmg}})
		log.Printf("[%s] char %d ejected from ship %d (type %d) into capsule %d at (%.0f,%.0f,%.0f)",
			addr, charID, oldShip, slim.TypeID, capsuleID, pos.X, pos.Y, pos.Z)
	}
}

// boardUnpilotedShip moves the pilot into an unpiloted hull on their grid.
func boardUnpilotedShip(c *evenet.Conn, addr string, accountID, charID, shipID int64,
	sqldb *sql.DB, ds *destinyConn) (marshal.PyRep, func()) {

	if !inSpace(ds, sqldb) {
		return customError("You must be in space to board that ship."), nil
	}
	ss := ds.solarSystem
	tbd, tslim, tdmg, ok := ss.GetAbandonedShip(uint32(shipID))
	if !ok {
		return customError("That ship cannot be boarded: it is piloted, or not here."), nil
	}
	if ds.entity.IsInWarp() {
		return customError("You cannot board a ship while warping."), nil
	}
	if !shipWithin(ds, tbd.Pos, boardInSpaceRange+tbd.Radius) {
		return customError("You are too far away to board that ship. Get within 6,500 m."), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if uerr := boardSkillError(ctx, sqldb, charID, shipID); uerr != nil {
		return uerr, nil
	}
	// A pilot the police are engaging may not swap out of the fight.
	if concordIsFlagged(charID) {
		return server.BuildUserError("ShipInvolvedInPoliceCrime", nil), nil
	}

	// Taking the ball out of the system is the claim: of two pilots boarding
	// one hull, only one gets past this.
	if !ss.RemoveLootContainer(uint32(shipID)) {
		return customError("Someone else boarded that ship first."), nil
	}
	if err := evedb.ClaimAbandonedShip(ctx, sqldb, shipID, ds.sysID, charID); err != nil {
		if errors.Is(err, evedb.ErrShipNotAbandoned) {
			removeLootBallFromGrid(ss, shipID)
			return customError("That ship can no longer be boarded."), nil
		}
		log.Printf("[%s] Board in space: %v", addr, err)
		ss.AddAbandonedShip(tbd, tslim, tdmg) // still floating; put it back
		return customError("Could not board the ship."), nil
	}
	offlineUnskilledModules(ctx, addr, sqldb, charID, shipID, tslim.TypeID)

	// The boarder's current hull: a capsule goes, a ship stays behind unpiloted.
	oldShip := ds.shipID
	wasCapsule := ds.shipGroupID == groupCapsule
	old := oldHull{deleteRow: wasCapsule}
	if !wasCapsule {
		obd, oslim, odmg := unpilotedShipBall(ctx, sqldb, ds)
		old.unpiloted = &unpilotedHull{bd: obd, slim: oslim, dmg: odmg}
		persistShipDamage(ds, sqldb)
		pos := obd.Pos
		if err := evedb.MarkShipAbandoned(ctx, sqldb, oldShip, ds.sysID, pos.X, pos.Y, pos.Z); err != nil {
			log.Printf("[%s] Board in space: leave ship %d: %v", addr, oldShip, err)
		}
	}

	return nil, func() {
		// The hull's loot ball leaves every grid before the piloted ball with
		// the same ID arrives.
		removeLootBallFromGrid(ss, shipID)
		swapPilotInSpace(c, addr, accountID, charID, sqldb, ds, shipID, old)
		log.Printf("[%s] char %d boarded unpiloted ship %d (type %d, was owned by %d); left ship %d (capsule=%v)",
			addr, charID, shipID, tslim.TypeID, tslim.OwnerID, oldShip, wasCapsule)
	}
}

// offlineUnskilledModules offlines the modules on shipID charID lacks the
// skills to run, then whatever the new owner's fitting skills cannot power.
func offlineUnskilledModules(ctx context.Context, addr string, sqldb *sql.DB, charID, shipID, shipTypeID int64) {
	online, err := evedb.GetOnlineModuleIDs(ctx, sqldb, shipID)
	if err != nil {
		log.Printf("[%s] Board in space: online modules of %d: %v", addr, shipID, err)
	}
	mods, err := evedb.GetFittedModules(ctx, sqldb, shipID)
	if err != nil {
		log.Printf("[%s] Board in space: fitted modules of %d: %v", addr, shipID, err)
	}
	var offlined int
	for _, m := range mods {
		if !online[m.ItemID] || missingFitSkills(ctx, sqldb, charID, m.TypeID) == "" {
			continue
		}
		if err := evedb.SetModuleOnlineState(ctx, sqldb, m.ItemID, false); err == nil {
			offlined++
		}
	}
	shed, err := evedb.ShedShipFittingOverload(ctx, sqldb, shipID, shipTypeID)
	if err != nil {
		log.Printf("[%s] Board in space: fitting load of %d: %v", addr, shipID, err)
	}
	if offlined+len(shed) > 0 {
		log.Printf("[%s] Board in space: ship %d — %d module(s) offlined for skills, %d for CPU/powergrid",
			addr, shipID, offlined, len(shed))
	}
}

// unpilotedShipBall builds the ball, slim and damage an unpiloted ds.shipID
// shows: the piloted ship's look (type, turrets, owner) with no pilot.
func unpilotedShipBall(ctx context.Context, sqldb *sql.DB, ds *destinyConn) (destiny.BallDesc, destiny.SlimItem, destiny.DamageState) {
	e := ds.entity
	slim := e.Slim
	slim.CharID = 0
	slim.SecurityStatus = 0
	slim.Bounty = 0
	slim.WarFactionID = 0
	slim.Modules = append([]destiny.ModuleRef(nil), e.Slim.Modules...)
	name := ds.shipName
	if name == "" {
		name = typeNameOrID(ctx, sqldb, ds.shipTypeID)
	}
	radius := e.BallDesc().Radius
	if radius <= 0 {
		radius = 50
	}
	bd := destiny.BallDesc{
		EntityID: uint32(ds.shipID), Mode: destiny.ModeRigid, SubType: 2,
		Pos: e.Pos(), Radius: radius, Name: name,
	}
	dmg := e.DamageStateFrac()
	dmg.Tau = 1e9 // nobody aboard to run the shield
	dmg.Timestamp = server.Win32TimeNow()
	return bd, slim, dmg
}

// oldHull is what becomes of the hull a pilot leaves in space.
type oldHull struct {
	deleteRow bool           // a capsule: its row goes too
	unpiloted *unpilotedHull // a ship left floating, converted in place
}

// unpilotedHull is the ball, slim and damage an unpiloted ship shows.
type unpilotedHull struct {
	bd   destiny.BallDesc
	slim destiny.SlimItem
	dmg  destiny.DamageState
}

// swapPilotInSpace moves the pilot from their current ball to newShipID's,
// without tearing the old ball off any grid first, which made the hull
// vanish and left the client a moment with no ego:
//
//   - a hull left unpiloted stays the same ball everywhere: it is stopped,
//     snapped to where the server holds it, loses its interactive (piloted)
//     flag and gets a slim with no pilot (destiny.SendSettleUnpiloted);
//   - anything else (a capsule) leaves bystanders' grids at once and the
//     pilot's own only after session.shipid has moved the ego off it.
//
// The new ball still reaches the pilot before the shipid session change: the
// client sets bp.ego only to a ball it holds. Its "Boarding Ship" progress
// and fade (gameui.OnNewState) are the client's own reaction to that change.
func swapPilotInSpace(c *evenet.Conn, addr string, accountID, charID int64, sqldb *sql.DB,
	ds *destinyConn, newShipID int64, old oldHull) {

	ctx, cancel := context.WithTimeout(context.Background(), starbaseSweepTimeout)
	defer cancel()

	oldEntity := ds.entity
	oldID := oldEntity.ID
	oldShipDBID := ds.shipID
	ss := ds.solarSystem

	clearPlayerCombatState(ds)
	if globalDroneMgr != nil && oldShipDBID != 0 {
		globalDroneMgr.AbandonControl(oldShipDBID)
	}
	if err := evedb.MoveCharacterIntoShip(ctx, sqldb, charID, newShipID); err != nil {
		log.Printf("[%s] ship swap: MoveCharacterIntoShip %d: %v", addr, newShipID, err)
		return
	}
	if old.deleteRow {
		_ = evedb.DeleteEntity(ctx, sqldb, oldShipDBID)
	}

	others := ss.RemoveEntity(oldID)
	if u := old.unpiloted; u != nil {
		ss.AddAbandonedShip(u.bd, u.slim, u.dmg)
		pos := u.bd.Pos
		settle := func(p *destiny.Entity, conn *evenet.Conn, acct uint64) {
			_ = p.SendStampedUrgent(func(stamp uint32) error {
				return destiny.SendSettleUnpiloted(conn, globalNodeID, acct, stamp, oldID, pos)
			})
			_ = p.SendFXStamped(func(stamp uint32) error {
				return destiny.SendSlimItemChange(conn, globalNodeID, acct, stamp, u.slim)
			})
		}
		for _, p := range others {
			if p.Conn != nil && p.KnowsBall(oldID) {
				settle(p, p.Conn, p.AccountID)
			}
		}
		if c != nil {
			settle(oldEntity, c, uint64(accountID))
		}
	} else {
		for _, p := range others {
			if p.Conn == nil || !p.KnowsBall(oldID) {
				continue
			}
			if err := p.SendStamped(func(stamp uint32) error {
				return destiny.SendRemoveBalls(p.Conn, globalNodeID, p.AccountID, stamp, []uint32{oldID})
			}); err == nil {
				p.ForgetBalls(oldID)
			}
		}
	}

	// Rebuild the pilot's destiny state on the new hull. Old ID 0: the new
	// entity goes on knowing the old ball, which is still on the pilot's screen.
	ds.pendingBallparkFrom = oldEntity
	ds.pendingBallparkOldID = 0
	ds.dm = nil
	initDestiny(addr, charID, 0, sqldb, ds)
	if ds.dm == nil {
		ds.pendingBallparkFrom = nil
		log.Printf("[%s] ship swap: initDestiny failed for ship %d", addr, newShipID)
		return
	}
	registerUndock(c, addr, uint64(accountID), charID, sqldb, ds)
	ds.pendingBallparkFrom = nil
	if ds.entity == nil || c == nil {
		return
	}

	e := ds.entity
	bd := e.BallDesc()
	bd.CorpID = uint32(e.Slim.CorpID)
	bd.Name = e.BallName()
	dmg := e.DamageStateFrac()
	dmg.Timestamp = server.Win32TimeNow()
	if err := e.SendStampedUrgent(func(stamp uint32) error {
		return destiny.SendAddBallsFull(c, globalNodeID, uint64(accountID), stamp,
			[]destiny.BallDesc{bd}, []destiny.SlimItem{e.Slim},
			map[uint32]destiny.DamageState{e.ID: dmg})
	}); err != nil {
		log.Printf("[%s] ship swap: AddBalls %d: %v", addr, newShipID, err)
	}
	e.ClaimUnknownBalls(e.ID)
	time.Sleep(50 * time.Millisecond) // let the ball land before the session points at it
	if err := sendShipIDSessionChange(c, addr, oldShipDBID, newShipID); err != nil {
		log.Printf("[%s] ship swap: session change: %v", addr, err)
	}
	if old.unpiloted == nil {
		// The ego has moved off the old ball; now it can go.
		if err := e.SendStampedUrgent(func(stamp uint32) error {
			return destiny.SendRemoveBalls(c, globalNodeID, uint64(accountID), stamp, []uint32{oldID})
		}); err == nil {
			e.ForgetBalls(oldID)
		}
	}
}

// loadAbandonedShipsIntoSystem restores the unpiloted hulls floating in sysID
// when the system loads.
func loadAbandonedShipsIntoSystem(addr string, sqldb *sql.DB, ss *destiny.SolarSystem, sysID int64) {
	if sqldb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ships, err := evedb.ListAbandonedShips(ctx, sqldb, sysID)
	if err != nil {
		log.Printf("[%s] loadAbandonedShipsIntoSystem sysID=%d: %v", addr, sysID, err)
		return
	}
	for _, s := range ships {
		name := s.Name
		if name == "" {
			name = typeNameOrID(ctx, sqldb, s.TypeID)
		}
		radius := s.Radius
		if radius <= 0 {
			radius = 50
		}
		bd := destiny.BallDesc{
			EntityID: uint32(s.ShipID), Mode: destiny.ModeRigid, SubType: 2,
			Pos: destiny.Vec3{X: s.X, Y: s.Y, Z: s.Z}, Radius: radius, Name: name,
		}
		slim := destiny.SlimItem{
			BallID: s.ShipID, ItemID: s.ShipID,
			TypeID: s.TypeID, GroupID: s.GroupID, CategoryID: 6,
			OwnerID: s.OwnerID, CorpID: s.CorpID, AllianceID: s.AllianceID,
		}
		if mods, err := evedb.GetFittedModules(ctx, sqldb, s.ShipID); err == nil {
			for _, m := range mods {
				slim.Modules = append(slim.Modules, destiny.ModuleRef{ItemID: m.ItemID, TypeID: m.TypeID})
			}
		}
		dmg := destiny.DamageState{Shield: 1.0, Tau: 1e9, Armor: 1.0, Structure: 1.0}
		if structDmg, armorDmg, err := evedb.GetShipCombatDamage(ctx, sqldb, s.ShipID); err == nil {
			dmg.Armor, dmg.Structure = hpFraction(ctx, sqldb, s.TypeID, armorDmg, structDmg)
		}
		ss.AddAbandonedShip(bd, slim, dmg)
	}
	if len(ships) > 0 {
		log.Printf("[%s] loadAbandonedShipsIntoSystem sysID=%d: restored %d unpiloted ships", addr, sysID, len(ships))
	}
}

// hpFraction turns persisted armor and structure damage into the fractions a
// damage state carries, against the hull's base HP.
func hpFraction(ctx context.Context, sqldb *sql.DB, typeID int64, armorDmg, structDmg float64) (armor, structure float64) {
	frac := func(attr string, dmg float64) float64 {
		hp, ok, err := evedb.GetTypeAttributeFloat(ctx, sqldb, typeID, attr)
		if err != nil || !ok || hp <= 0 {
			return 1.0
		}
		return min(max((hp-dmg)/hp, 0.01), 1.0)
	}
	return frac("armorHP", armorDmg), frac("hp", structDmg)
}
