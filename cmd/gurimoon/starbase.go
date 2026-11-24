// Starbase client flow: launch (ship.Drop), anchor (posMgr.AnchorStructure),
// online, offline and unanchor (dogmaIM structure effects). The /pos* slash
// commands are the admin bypass. Wire contract: docs/starbase-pos-investigation.md.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
	"gurimoon/syncx"
	"gurimoon/tick"
)

// Structure effect IDs (dgmEffects). The client's structure menus send these
// numeric IDs to dogmaIM.Activate/Deactivate; ship-module activation sends
// string names, so structureEffectID accepts both.
const (
	effectOnlineForStructures     = 901
	effectAnchorDropForStructures = 1022
	effectAnchorLiftForStructures = 1023
)

// Outpost platforms and standalone anchorables arrive on the generic anchor
// effects (menusvc AnchorObject); towers on the ...ForStructures pair.
// structureEffectID accepts all four.
const (
	effectAnchorDrop = 649
	effectAnchorLift = 650
)



// Context budgets: a one-or-two-query lookup, and a sweep that walks a whole table.
const (
	starbaseCtxTimeout   = 15 * time.Second
	starbaseSweepTimeout = 30 * time.Second
)

// ─── launch (ship.Drop) ────────────────────────────────────────────────────

// splitStructureDropIDs partitions a ship.Drop id list into category-23
// structures (in the ship's cargo) and everything else (drone path).
func splitStructureDropIDs(sqldb *sql.DB, shipID int64, ids []int64) (structures, rest []int64) {
	if sqldb == nil || shipID == 0 {
		return nil, ids
	}
	ctx, cancel := context.WithTimeout(context.Background(), starbaseCtxTimeout)
	defer cancel()
	// A stack arrives once per unit; classify each itemID once.
	isStructure := make(map[int64]bool, len(ids))
	for _, id := range ids {
		known, seen := isStructure[id]
		if !seen {
			_, ok, err := evedb.GetCargoStructure(ctx, sqldb, id, shipID)
			known = err == nil && ok
			isStructure[id] = known
		}
		if known {
			structures = append(structures, id)
		} else {
			rest = append(rest, id)
		}
	}
	return structures, rest
}

// launchStructuresForPlayer implements ship.Drop for structures: the item
// leaves the cargo hold and becomes a free-floating unanchored ball
// (posState 0) owned by the launching corp.  Returns the launched itemIDs.
func launchStructuresForPlayer(ds *destinyConn, addr string, charID, dropOwnerID int64, ids []int64, sqldb *sql.DB) []int64 {
	var launched []int64
	if ds.solarSystem == nil || ds.entity == nil || sqldb == nil {
		return launched
	}
	ctx, cancel := context.WithTimeout(context.Background(), starbaseCtxTimeout)
	defer cancel()

	_, corpID, allianceID, err := evedb.GetCharSovContext(ctx, sqldb, charID)
	if err != nil {
		log.Printf("[%s] launchStructures: sov context: %v", addr, err)
		return launched
	}
	// Structures are corp property: corp-own unless the passed owner is the corp
	// itself. A deployable launched for self stays the pilot's own.
	ownerID := corpID
	if dropOwnerID != 0 && dropOwnerID == corpID {
		ownerID = dropOwnerID
	}

	ss := ds.solarSystem
	shipPos := ds.entity.Pos()
	// The client sends a stack's itemID once per unit. Each distinct stack gives
	// up one unit per Drop; the repeats are ignored.
	seen := make(map[int64]bool, len(ids))
	for _, stackID := range ids {
		if seen[stackID] {
			continue
		}
		seen[stackID] = true
		i := len(launched)
		st, ok, err := evedb.GetCargoStructure(ctx, sqldb, stackID, ds.shipID)
		if err != nil || !ok {
			log.Printf("[%s] launchStructures: item %d not a cargo structure (%v)", addr, stackID, err)
			continue
		}
		// Take one unit off the stack as its own item; launching the row itself
		// would put the whole stack into space as one structure.
		stackQty := int64(1)
		if er, err := evedb.GetEntityRow(ctx, sqldb, stackID); err == nil && er != nil {
			stackQty = er.Quantity
		}
		itemID, err := evedb.SplitOneFromStack(ctx, sqldb, stackID)
		if err != nil {
			log.Printf("[%s] launchStructures: split %d: %v", addr, stackID, err)
			continue
		}
		st.ItemID = itemID
		structOwner := ownerID
		if dropOwnerID == charID && evedb.IsDeployableGroup(st.GroupID) {
			structOwner = charID
		}
		pos := destiny.Vec3{
			X: shipPos.X + 1000 + float64(i)*500,
			Y: shipPos.Y,
			Z: shipPos.Z + 1000,
		}
		if err := evedb.LaunchStructureToSpace(ctx, sqldb, itemID, structOwner, ds.sysID, pos.X, pos.Y, pos.Z); err != nil {
			log.Printf("[%s] launchStructures: %v", addr, err)
			continue
		}

		bd, slim, dmg := launchedStructureBallSlim(st, pos, structOwner, corpID, allianceID)
		ss.AddStarbase(bd, slim, dmg)
		launched = append(launched, itemID)
		broadcastBallAdd(ss, bd, slim, dmg)
		// Tell the launcher's inventory: the stack shrank, or the item left
		// the cargo hold.
		if itemID != stackID {
			if er, err := evedb.GetEntityRow(ctx, sqldb, stackID); err == nil && er != nil {
				changeDict := marshal.PyDict{Items: []marshal.DictEntry{
					{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(stackQty)}}, // ixQuantity
				}}
				_ = server.SendNotification(ds.entity.Conn, int64(ds.entity.AccountID), "OnItemChange", "charid",
					marshal.PyTuple{Items: []marshal.PyRep{evedb.BuildEntityRow(er), changeDict}})
			}
		} else if er, err := evedb.GetEntityRow(ctx, sqldb, itemID); err == nil && er != nil {
			changeDict := marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: uint64(ds.shipID)}},
				{Key: marshal.PyInt{Value: 4}, Value: marshal.PyInt{Value: 5}},
			}}
			_ = server.SendNotification(ds.entity.Conn, int64(ds.entity.AccountID), "OnItemChange", "charid",
				marshal.PyTuple{Items: []marshal.PyRep{evedb.BuildEntityRow(er), changeDict}})
		}
		log.Printf("[%s] structure launched: item=%d type=%d (%s) owner=%d in system %d",
			addr, itemID, st.TypeID, st.TypeName, structOwner, ds.sysID)
	}
	return launched
}

// launchedStructureBallSlim builds the free (unanchored) ball for a launched
// structure. ModeStop plus mass makes it a free ball; the client's ball.isFree
// drives the capsule model, the anchor menu predicates and scooping.
func launchedStructureBallSlim(st evedb.LaunchedStructure, pos destiny.Vec3, ownerID, corpID, allianceID int64) (destiny.BallDesc, destiny.SlimItem, destiny.DamageState) {
	bd := destiny.BallDesc{
		EntityID: uint32(st.ItemID),
		Mode:     destiny.ModeStop,
		SubType:  1, // ShipSector present (zeroed velocity)
		Pos:      pos,
		Radius:   st.Radius,
		Mass:     st.Mass,
		MaxSpeed: 1,
		Agility:  1,
		Name:     st.TypeName,
		CorpID:   uint32(corpID),
	}
	posState := int64(0) // STRUCTURE_UNANCHORED, no timestamp → fully unanchored
	slim := destiny.SlimItem{
		BallID:     st.ItemID,
		ItemID:     st.ItemID,
		TypeID:     st.TypeID,
		GroupID:    st.GroupID,
		CategoryID: st.CategoryID,
		OwnerID:    ownerID,
		CorpID:     corpID,
		AllianceID: allianceID,
		PosState:   &posState,
	}
	dmg := fullDamageState()
	return bd, slim, dmg
}

// ─── scoop (ship.Scoop) ────────────────────────────────────────────────────

// scoopStructureForPlayer implements ship.Scoop(objectID) for a free
// structure. The client's generic Scoop To Cargo Hold menu offers itself on
// any free anchorable ball; every check the menu made is made again here.
func scoopStructureForPlayer(ds *destinyConn, addr string, charID, itemID int64, sqldb *sql.DB) error {
	if ds.solarSystem == nil || ds.entity == nil || ds.shipID == 0 || sqldb == nil {
		return fmt.Errorf("you must be in space to scoop that")
	}
	ss := ds.solarSystem
	slim, ok := ss.GetStarbaseSlim(uint32(itemID))
	if !ok {
		return fmt.Errorf("there is nothing here to scoop")
	}
	// A structure counting down its unanchor is posState 0 on the wire but keeps
	// its overlay row; the GetStarbase checks below refuse it.
	if slim.PosState == nil || *slim.PosState != 0 {
		return fmt.Errorf("the %s must be fully unanchored before it can be scooped", slimTypeName(slim, sqldb))
	}
	bd, ok := ss.GetStarbaseBall(uint32(itemID))
	if !ok || !shipWithin(ds, bd.Pos, itemTransferRange+bd.Radius) {
		return fmt.Errorf("you are too far away to scoop that")
	}

	ctx, cancel := context.WithTimeout(context.Background(), starbaseCtxTimeout)
	defer cancel()
	er, err := evedb.GetEntityRow(ctx, sqldb, itemID)
	if err != nil || er == nil {
		return fmt.Errorf("there is nothing here to scoop")
	}
	_, corpID, _, err := evedb.GetCharSovContext(ctx, sqldb, charID)
	if err != nil {
		return err
	}
	if er.OwnerID != charID && er.OwnerID != corpID {
		return fmt.Errorf("that structure does not belong to you or your corporation")
	}
	if _, tower, _ := evedb.GetStarbase(ctx, sqldb, itemID); tower {
		return fmt.Errorf("the %s has not finished unanchoring", slimTypeName(slim, sqldb))
	}
	if _, slave, _ := evedb.GetStarbaseStructure(ctx, sqldb, itemID); slave {
		return fmt.Errorf("the %s has not finished unanchoring", slimTypeName(slim, sqldb))
	}

	shipER, _ := evedb.GetEntityRow(ctx, sqldb, ds.shipID)
	if shipER != nil {
		cargoCap, _ := evedb.GetShipCargoCapacityFitted(ctx, sqldb, shipER.TypeID, ds.shipID)
		usedVol, _ := evedb.GetCargoUsedVolume(ctx, sqldb, charID, ds.shipID)
		need := evedb.ItemVolume(ctx, sqldb, itemID, er.TypeID) * float64(max(er.Quantity, 1))
		if free := cargoCap - usedVol; need > free {
			return fmt.Errorf("not enough cargo space: the %s needs %.0f m3 and your hold has %.0f m3 free",
				slimTypeName(slim, sqldb), need, max(free, 0))
		}
	}

	// Moves the row, hands it to the pilot, and (through the global live
	// notifier) takes the ball off the grid for everyone in the system.
	if err := evedb.ScoopStructureToCargo(ctx, sqldb, itemID, ds.shipID, charID, nil); err != nil {
		return err
	}
	if newER, err := evedb.GetEntityRow(ctx, sqldb, itemID); err == nil && newER != nil {
		changeDict := marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyInt{Value: 2}, Value: marshal.PyInt{Value: uint64(er.OwnerID)}},
			{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: uint64(ds.sysID)}},
			{Key: marshal.PyInt{Value: 4}, Value: marshal.PyInt{Value: 0}},
		}}
		_ = server.SendNotification(ds.entity.Conn, int64(ds.entity.AccountID), "OnItemChange", "charid",
			marshal.PyTuple{Items: []marshal.PyRep{evedb.BuildEntityRow(newER), changeDict}})
	}
	log.Printf("[%s] structure scooped: item=%d type=%d by char %d into ship %d",
		addr, itemID, er.TypeID, charID, ds.shipID)
	return nil
}

// scoopFromCall unpacks ship.Scoop(objectID[, password]) and scoops a corpse,
// a mine, a drone or a free structure. The password is ignored: scooping a secure can
// stays owner/corp-only. A refusal goes to the notify line; the call answers None.
func scoopFromCall(ds *destinyConn, addr string, charID int64, args marshal.PyTuple, sqldb *sql.DB) {
	if len(args.Items) < 1 {
		return
	}
	id, ok := args.Items[0].(marshal.PyInt)
	if !ok {
		return
	}
	if handled, err := scoopLaunchedContainer(ds, addr, charID, int64(id.Value), sqldb); handled {
		if err != nil {
			log.Printf("[%s] ship.Scoop container %d refused: %v", addr, int64(id.Value), err)
			posRefusal(ds, "Cannot scoop", err)
		}
		return
	}
	if handled, err := scoopCorpseFromCall(ds, addr, charID, int64(id.Value), sqldb); handled {
		if err != nil {
			log.Printf("[%s] ship.Scoop corpse %d refused: %v", addr, int64(id.Value), err)
			posRefusal(ds, "Cannot scoop", err)
		}
		return
	}
	if handled, err := scoopMineFromCall(ds, addr, charID, int64(id.Value), sqldb); handled {
		if err != nil {
			log.Printf("[%s] ship.Scoop mine %d refused: %v", addr, int64(id.Value), err)
			posRefusal(ds, "Cannot scoop", err)
		}
		return
	}
	if handled, err := scoopDroneToCargo(ds, charID, int64(id.Value), sqldb); handled {
		if err != nil {
			log.Printf("[%s] ship.Scoop drone %d refused: %v", addr, int64(id.Value), err)
			posRefusal(ds, "Cannot scoop", err)
		}
		return
	}
	if err := scoopStructureForPlayer(ds, addr, charID, int64(id.Value), sqldb); err != nil {
		log.Printf("[%s] ship.Scoop %d refused: %v", addr, int64(id.Value), err)
		posRefusal(ds, "Cannot scoop", err)
	}
}

// posRefusal tells the pilot why a structure action was refused, on the notify
// line. POS actions arrive on inline binds or dogma Activates whose reply has
// already gone out, so no exception can reach the pilot.
func posRefusal(ds *destinyConn, action string, err error) {
	msg := err.Error()
	if msg != "" {
		msg = strings.ToUpper(msg[:1]) + msg[1:]
	}
	if !strings.HasSuffix(msg, ".") {
		msg += "."
	}
	platformNotify(ds, action+": "+msg)
}

// settleTowerOnlining finishes a tower's onlining when its delay has run. The
// client derives online from posTimestamp + delay itself and offers slave
// actions before the onlining sweep agrees; a pilot in that gap is not refused.
func settleTowerOnlining(ctx context.Context, sqldb *sql.DB, towerID int64) {
	rec, found, err := evedb.GetStarbase(ctx, sqldb, towerID)
	if err != nil || !found || rec.State != evedb.StarbaseStateOnlining {
		return
	}
	if evedb.DelayElapsed(rec.OnlinedAt, starbaseOnliningDelayMs(sqldb, rec.TypeID), server.Win32TimeNow()) {
		finishStarbaseOnlining(sqldb, towerID)
	}
}

// settleSystemTowersOnlining is settleTowerOnlining for every onlining tower
// in a system; the anchor path picks its tower from all of them.
func settleSystemTowersOnlining(ctx context.Context, sqldb *sql.DB, sysID int64) {
	towers, err := evedb.ListStarbasesForSystem(ctx, sqldb, sysID)
	if err != nil {
		return
	}
	for _, t := range towers {
		if t.State == evedb.StarbaseStateOnlining {
			settleTowerOnlining(ctx, sqldb, t.EntityID)
		}
	}
}

// towerNotOnlineReason words why a tower cannot take structures, with the
// time left when it is merely still coming up.
func towerNotOnlineReason(sqldb *sql.DB, tower evedb.StarbaseRecord, found bool) string {
	if !found {
		return "its control tower no longer exists."
	}
	if tower.State == evedb.StarbaseStateOnlining {
		left := starbaseOnliningDelayMs(sqldb, tower.TypeID) - float64(server.Win32TimeNow()-tower.OnlinedAt)/10000.0
		if left > 0 {
			return fmt.Sprintf("the control tower is still coming online (%s left).",
				(time.Duration(left) * time.Millisecond).Round(time.Second))
		}
	}
	return "the control tower is not online."
}

// slimTypeName names a structure for a refusal message.
func slimTypeName(slim destiny.SlimItem, sqldb *sql.DB) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var name string
	if err := sqldb.QueryRowContext(ctx, `SELECT typeName FROM invTypes WHERE typeID = ?`, slim.TypeID).Scan(&name); err != nil || name == "" {
		return "structure"
	}
	return name
}

// ─── anchor (posMgr.AnchorStructure) ───────────────────────────────────────

// anchorStructureForPlayer implements posMgr.AnchorStructure(itemID, (x,y,z)):
// a control tower at a moon, or a slave or standalone structure through the
// helpers below. The error is client-visible on the CallReq path.
func anchorStructureForPlayer(ds *destinyConn, addr string, charID, itemID int64, x, y, z float64, sqldb *sql.DB) error {
	if ds.solarSystem == nil || sqldb == nil {
		return fmt.Errorf("not in space")
	}
	ss := ds.solarSystem
	slim, ok := ss.GetStarbaseSlim(uint32(itemID))
	if !ok || slim.PosState == nil || *slim.PosState != 0 {
		return fmt.Errorf("item %d is not an unanchored structure here", itemID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), starbaseCtxTimeout)
	defer cancel()
	_, corpID, allianceID, err := evedb.GetCharSovContext(ctx, sqldb, charID)
	if err != nil {
		return err
	}
	if slim.CorpID != corpID {
		return fmt.Errorf("structure %d belongs to corp %d", itemID, slim.CorpID)
	}
	// Mobile Storage anchors on its own, wherever it floats: Anchor Structure
	// reaches it here, the generic Anchor through handleStandaloneEffect.
	if isStandaloneStructure(sqldb, slim) {
		return anchorStandaloneForPlayer(ctx, ds, addr, charID, itemID, slim, sqldb)
	}
	if slim.GroupID != evedb.GroupControlTower {
		return anchorSlaveStructureForPlayer(ctx, ds, addr, charID, itemID, slim, corpID, allianceID, x, y, z, sqldb)
	}


	// Placement rules live in db.CheckStarbaseAnchorPolicy, shared with
	// /posplant. Faction charters are the fuel sweep's business.
	decision, err := evedb.CheckStarbaseAnchorPolicy(ctx, sqldb, ds.sysID, corpID, allianceID)
	if err != nil {
		return err
	}
	allianceID = decision.AllianceID
	// The anchor radius is the tower's moonAnchorDistance (attr 711), the number
	// Show Info quotes; the client's own distance check never runs, so this is the gate.
	maxMoonDist := evedb.MoonAnchorDistance(ctx, sqldb, slim.TypeID)
	moonID, moonName, dist, foundMoon, err := evedb.NearestMoon(ctx, sqldb, ds.sysID, x, y, z)
	if err != nil {
		return err
	}
	if !foundMoon {
		return fmt.Errorf("this system has no moons to anchor at")
	}
	if dist > maxMoonDist {
		// moonAnchorDistance is measured from the warp-in point, which sits half a
		// radius above the surface; a pilot anchoring where they landed is accepted.
		wi, wErr := evedb.MoonWarpIn(ctx, sqldb, moonID)
		if wErr != nil || math.Sqrt((x-wi[0])*(x-wi[0])+(y-wi[1])*(y-wi[1])+(z-wi[2])*(z-wi[2])) > maxMoonDist {
			return fmt.Errorf("control towers must be anchored within %.0f km of a moon or its warp-in point (nearest: %s at %.0f km)",
				maxMoonDist/1000, moonName, dist/1000)
		}
	}

	if err := evedb.AnchorLaunchedStarbase(ctx, sqldb, itemID, moonID, corpID, allianceID, x, y, z); err != nil {
		return err
	}
	// Sovereignty is not claimed here: a tower scores nothing until online, and
	// the online event recomputes the system (db/sovmajority.go).
	evedb.EmitStarbaseEvent(evedb.StarbaseEvent{Kind: evedb.StarbaseEventPlanted, TowerID: itemID,
		SolarSystemID: ds.sysID, OwnerCorpID: corpID, OwnerAllianceID: allianceID})

	// Swap the free ball for the anchored tower presentation and broadcast.
	rec, found, err := evedb.GetStarbase(ctx, sqldb, itemID)
	if err != nil || !found {
		return fmt.Errorf("anchored but could not reload starbase %d: %w", itemID, err)
	}
	ss.RemoveStarbase(uint32(itemID))
	bd, newSlim, dmg := starbaseBallSlimDmg(rec)
	ss.AddStarbase(bd, newSlim, dmg)
	broadcastBallSwap(ss, uint32(itemID), bd, newSlim, dmg)
	log.Printf("[%s] structure %d anchored at moon %s (%.0f km away) by char %d",
		addr, itemID, moonName, dist/1000, charID)
	return nil
}

// anchorSlaveStructureForPlayer anchors a harvester, silo, reactor, array or
// battery to a control tower. Reached from the same posMgr.AnchorStructure as
// a tower: pwn.Anchor gates on effect 1022, which every anchorable group
// carries. The client's own checks run against its ballpark and are not the gate.
func anchorSlaveStructureForPlayer(
	ctx context.Context, ds *destinyConn, addr string,
	charID, itemID int64, slim destiny.SlimItem,
	corpID, allianceID int64, x, y, z float64, sqldb *sql.DB,
) error {
	anchorable, err := evedb.IsAnchorableStructureType(ctx, sqldb, slim.TypeID)
	if err != nil {
		return err
	}
	if !anchorable {
		return fmt.Errorf("type %d cannot be anchored", slim.TypeID)
	}

	// The security ceiling is the structure's own anchoringSecurityLevelMax (attr
	// 1032); absent on most structures, which then inherit the tower's placement rule.
	if maxSec, ok := evedb.AnchoringSecurityLevelMax(ctx, sqldb, slim.TypeID); ok {
		sec, secOK, sErr := evedb.GetSystemSecurity(ctx, sqldb, ds.sysID)
		if sErr != nil {
			return sErr
		}
		if secOK && sec > maxSec {
			return fmt.Errorf("this structure cannot be anchored in %.1f-sec space (%.1f and below only)", sec, maxSec)
		}
	}
	if evedb.AnchoringRequiresSovereignty(ctx, sqldb, slim.TypeID) {
		sov, owned, sErr := evedb.GetSov(ctx, sqldb, ds.sysID)
		if sErr != nil {
			return sErr
		}
		if !owned || allianceID == 0 || sov.AllianceID != allianceID {
			return fmt.Errorf("this structure requires sovereignty in the system")
		}
	}

	settleSystemTowersOnlining(ctx, sqldb, ds.sysID)
	target, err := evedb.FindAnchorTower(ctx, sqldb, ds.sysID, corpID, allianceID, x, y, z)
	if err != nil {
		// A tower still coming online is reported with its time left.
		var ref *evedb.StarbaseAnchorRefusal
		if errors.As(err, &ref) && ref.Reason == evedb.AnchorRefuseTowerOffline {
			if towers, lErr := evedb.ListStarbasesForSystem(ctx, sqldb, ds.sysID); lErr == nil {
				for _, t := range towers {
					if t.State == evedb.StarbaseStateOnlining && evedb.StarbaseManagePermitted(t, corpID, allianceID) {
						return errors.New(towerNotOnlineReason(sqldb, t, true))
					}
				}
			}
		}
		return err
	}
	tower := target.Tower
	if !deployAllowed(ctx, addr, sqldb, tower, charID, "anchor") {
		return fmt.Errorf("you are not permitted to anchor structures at this control tower")
	}
	if why := evedb.ForceFieldAnchorRefusal(ctx, sqldb, tower.TypeID, slim.TypeID,
		x-tower.X, y-tower.Y, z-tower.Z); why != "" {
		return errors.New(why)
	}

	// The slave takes the tower's ownership, not the launcher's: sovereignty, the
	// management window and every permission check read the tower.
	if err := evedb.AnchorSlaveStructure(ctx, sqldb, itemID, tower.EntityID,
		ds.sysID, tower.OwnerCorpID, tower.OwnerAllianceID, x, y, z); err != nil {
		return err
	}

	rec, found, err := evedb.GetStarbaseStructure(ctx, sqldb, itemID)
	if err != nil || !found {
		return fmt.Errorf("anchored but could not reload structure %d: %w", itemID, err)
	}
	ss := ds.solarSystem
	ss.RemoveStarbase(uint32(itemID))
	bd, newSlim, dmg := starbaseBallSlimDmg(rec)
	ss.AddStarbase(bd, newSlim, dmg)
	broadcastBallSwap(ss, uint32(itemID), bd, newSlim, dmg)
	log.Printf("[%s] structure %d (group %d) anchored to tower %d (%.0f km, max %.0f km) by char %d",
		addr, itemID, slim.GroupID, tower.EntityID, target.Distance/1000, target.MaxRange/1000, charID)
	return nil
}

// parseAnchorStructureArgs unpacks posMgr.AnchorStructure(itemID, (x,y,z)).
func parseAnchorStructureArgs(args marshal.PyTuple) (itemID int64, x, y, z float64, ok bool) {
	if len(args.Items) < 2 {
		return 0, 0, 0, 0, false
	}
	id, isInt := args.Items[0].(marshal.PyInt)
	if !isInt {
		return 0, 0, 0, 0, false
	}
	pos, isTuple := args.Items[1].(marshal.PyTuple)
	if !isTuple || len(pos.Items) < 3 {
		return 0, 0, 0, 0, false
	}
	coord := func(r marshal.PyRep) float64 {
		switch v := r.(type) {
		case marshal.PyReal:
			return v.Value
		case marshal.PyInt:
			return float64(int64(v.Value))
		}
		return 0
	}
	return int64(id.Value), coord(pos.Items[0]), coord(pos.Items[1]), coord(pos.Items[2]), true
}

// ─── onlining sweep ────────────────────────────────────────────────────────

// startStarbaseOnliningSweep registers the periodic that finishes any tower
// whose onlining delay has elapsed. The per-tower one-shot is memory only;
// this sweep is the backstop after a restart or in an unloaded system.
// See docs/starbase-pos-investigation.md#onlining-must-not-depend-on-an-in-memory-timer.
func startStarbaseOnliningSweep(tickMgr *tick.Manager, sqldb *sql.DB) {
	if tickMgr == nil || sqldb == nil {
		return
	}
	const everyTicks = 30 // tick interval is 1s
	tickMgr.RegisterPeriodic("starbase-onlining-sweep", everyTicks, func(uint64) bool {
		ctx, cancel := context.WithTimeout(context.Background(), starbaseCtxTimeout)
		recs, err := evedb.ListStarbasesByState(ctx, sqldb, evedb.StarbaseStateOnlining)
		cancel()
		if err != nil {
			log.Printf("[pos] onlining sweep: %v", err)
			return true
		}
		now := server.Win32TimeNow()
		for _, r := range recs {
			delayMs := starbaseOnliningDelayMs(sqldb, r.TypeID)
			if float64(now-r.OnlinedAt)/10000.0 < delayMs {
				continue
			}
			finishStarbaseOnlining(sqldb, r.EntityID)
		}
		// Tower unanchoring countdowns: the one-shot does not survive a restart; this does.
		sweepStarbaseUnanchoring(sqldb)
		return true
	})
}

// ─── fuel sweep ────────────────────────────────────────────────────────────

// runStarbaseBootReconcile settles POS state that no sweep reaches: schema,
// orphan rows, legacy tower positions, missing moons, past-due onlining,
// reinforcement and unanchoring timers, unbacked sovereignty. Idempotent;
// runs once at boot, synchronously, before the listeners open.
func runStarbaseBootReconcile(sqldb *sql.DB) {
	if sqldb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), starbaseSweepTimeout)
	defer cancel()

	// Every POS table up front, so a DDL failure shows at boot rather than at the first anchor.
	if err := evedb.EnsureStarbaseSchema(ctx, sqldb); err != nil {
		log.Printf("[pos] boot reconcile: schema: %v", err)
		return
	}

	// A starbase row whose entity row is gone is invisible to every starbaseSelect
	// query, yet still holds its moon. Reported, not repaired.
	if orphans, err := evedb.OrphanStarbaseIDs(ctx, sqldb); err != nil {
		log.Printf("[pos] boot reconcile: orphan scan: %v", err)
	} else if len(orphans) > 0 {
		log.Printf("[pos] boot reconcile: %d starbase row(s) have no entity row: %v — "+
			"they hold their moons and cannot be managed in game", len(orphans), orphans)
	}

	// Towers still on the moon + (radius + 5 km) signature move onto MoonTowerPoint
	// with their structures. Runs before sysMgr exists, so no live ball is skipped.
	moved, err := evedb.RelocateLegacyTowers(ctx, sqldb, nil)
	for _, m := range moved {
		log.Printf("[pos] boot reconcile: tower %d (moon %d, system %d) moved off the moon onto its "+
			"warp-in standoff with %d structure(s)", m.TowerID, m.MoonID, m.SolarSystemID, m.Structures)
	}
	if err != nil {
		log.Printf("[pos] boot reconcile: legacy tower relocation: %v", err)
	}

	recs, err := evedb.ListAllStarbases(ctx, sqldb)
	if err != nil {
		log.Printf("[pos] boot reconcile: %v", err)
		return
	}
	backfilled, settled := 0, 0
	now := server.Win32TimeNow()
	for _, r := range recs {
		if r.MoonID == 0 {
			if _, err := evedb.BackfillStarbaseMoon(ctx, sqldb, r.EntityID); err != nil {
				log.Printf("[pos] boot reconcile: starbase %d moon backfill: %v", r.EntityID, err)
			} else {
				backfilled++
			}
		}
		// A past-due onlining is finished now rather than by the sweep, so the
		// first pilot in sees the right state.
		if r.State == evedb.StarbaseStateOnlining {
			if float64(now-r.OnlinedAt)/10000.0 >= starbaseOnliningDelayMs(sqldb, r.TypeID) {
				finishStarbaseOnlining(sqldb, r.EntityID)
				settled++
			}
		}
		// Reinforcement exits are timers too; re-arm every one (a past-due
		// one fires at once).
		if r.State == evedb.StarbaseStateReinforced {
			scheduleReinforceExit(sqldb, r.EntityID, r.ReinforceUntil)
		}
		// Unanchoring countdowns: a past-due one is finished now, a running one re-armed.
		if r.State == evedb.StarbaseStateUnanchoring {
			delay := typeAttrMs(sqldb, r.TypeID, "unanchoringDelay")
			if evedb.DelayElapsed(r.UnanchorAt, delay, now) {
				finishStarbaseUnanchor(sqldb, r.EntityID)
				settled++
			} else {
				scheduleStructureUnanchor(sqldb, r.EntityID, true,
					evedb.FiletimeToTime(r.UnanchorAt).Add(time.Duration(delay)*time.Millisecond))
			}
		}
	}

	if sys, err := evedb.UnbackedSovSystems(ctx, sqldb); err != nil {
		log.Printf("[pos] boot reconcile: sov scan: %v", err)
	} else if len(sys) > 0 {
		// Reported, not cleared: sov has sources other than a tower.
		log.Printf("[pos] boot reconcile: %d system(s) hold sovereignty with no tower of the "+
			"holding alliance in them: %v", len(sys), sys)
	}
	log.Printf("[pos] boot reconcile: %d starbase(s); %d moon backfill(s), %d onlining/unanchoring settled",
		len(recs), backfilled, settled)
}

// startStarbaseFuelSweep registers the periodic that burns fuel for every
// online tower, off the DB across all systems: a tower in an unloaded system
// still eats fuel and offlines on exhaustion.
func startStarbaseFuelSweep(tickMgr *tick.Manager, sqldb *sql.DB) {
	if tickMgr == nil || sqldb == nil {
		return
	}
	const everyTicks = 60 // tick interval is 1s
	freezeStarbaseFuelForDowntime(sqldb)
	tickMgr.RegisterPeriodic("starbase-fuel-sweep", everyTicks, func(uint64) bool {
		runStarbaseFuelSweep(sqldb)
		return true
	})
}

func runStarbaseFuelSweep(sqldb *sql.DB) {
	ctx, cancel := context.WithTimeout(context.Background(), starbaseSweepTimeout)
	defer cancel()
	towers, err := evedb.ListOnlineStarbaseFuel(ctx, sqldb)
	if err != nil {
		log.Printf("[pos] fuel sweep: %v", err)
		return
	}
	now := server.Win32TimeNow()
	lowHours := -1.0 // read once, and only if some tower burned this sweep
	srLowHours := -1.0
	for _, r := range towers {
		cycleTicks := uint64(evedb.StarbaseFuelCycleMs(ctx, sqldb, r.TypeID)) * 10000 // ms → FILETIME
		if cycleTicks == 0 {
			continue
		}
		// First sight since (re)onlining: cycles start counting now, so nothing is
		// owed for the past.
		if r.FuelTick == 0 || r.FuelTick < r.OnlinedAt {
			if err := evedb.SetStarbaseFuelTick(ctx, sqldb, r.EntityID, now); err != nil {
				log.Printf("[pos] fuel sweep: init tick for %d: %v", r.EntityID, err)
			}
			continue
		}
		if now-r.FuelTick < cycleTicks {
			continue
		}

		needs, err := evedb.StarbaseFuelNeeds(ctx, sqldb, r)
		if err != nil {
			log.Printf("[pos] fuel sweep: needs for tower %d: %v", r.EntityID, err)
			continue
		}
		if len(needs) == 0 {
			// No purpose-1 rows for this type (GM towers): runs for free.
			_ = evedb.SetStarbaseFuelTick(ctx, sqldb, r.EntityID, now)
			continue
		}

		// Burn one bill per elapsed cycle. The cap bounds the catch-up after
		// downtime; anything beyond it is forgiven (clamped to now).
		const maxCatchUp = 48
		fuelTick := r.FuelTick
		cycles := 0
		exhausted := false
		for now-fuelTick >= cycleTicks && cycles < maxCatchUp {
			missing, ok, err := evedb.ConsumeFromStarbaseBay(ctx, sqldb, r.EntityID, needs)
			if err != nil {
				log.Printf("[pos] fuel sweep: consume for tower %d: %v", r.EntityID, err)
				break
			}
			if !ok {
				starbaseFuelExhausted(sqldb, r, missing)
				exhausted = true
				break
			}
			fuelTick += cycleTicks
			cycles++
		}
		if !exhausted && cycles >= maxCatchUp && now-fuelTick >= cycleTicks {
			fuelTick = now
		}
		if fuelTick != r.FuelTick {
			if err := evedb.SetStarbaseFuelTick(ctx, sqldb, r.EntityID, fuelTick); err != nil {
				log.Printf("[pos] fuel sweep: store tick for %d: %v", r.EntityID, err)
			}
		}
		if cycles > 0 && !exhausted {
			if lowHours < 0 {
				lowHours = evedb.StarbaseFuelLowHours(ctx, sqldb)
			}
			checkStarbaseFuelLow(ctx, sqldb, r, lowHours)
			if srLowHours < 0 {
				srLowHours = evedb.StarbaseStrontiumLowHours(ctx, sqldb)
			}
			evedb.CheckStarbaseStrontiumLow(ctx, sqldb, r, srLowHours)
		}
	}
	stampStarbaseFuelSweep(sqldb)
}

// freezeStarbaseFuelForDowntime gives online towers back the fuel time the server was down.
func freezeStarbaseFuelForDowntime(sqldb *sql.DB) {
	ctx, cancel := context.WithTimeout(context.Background(), starbaseSweepTimeout)
	defer cancel()
	enabled := evedb.StarbaseFreezeFuelWhileDown(ctx, sqldb)
	if !enabled {
		log.Printf("[pos] fuel freeze is off: towers burn fuel for the time the server was down")
		return
	}
	lastUp, err := evedb.LastStarbaseFuelSweep(ctx, sqldb)
	if err != nil {
		log.Printf("[pos] fuel freeze: %v", err)
		return
	}
	now := server.Win32TimeNow()
	shift := evedb.FuelFreezeShift(enabled, lastUp, now, 60*10_000_000)
	if shift == 0 {
		return
	}
	n, err := evedb.ShiftOnlineStarbaseFuelTicks(ctx, sqldb, shift)
	if err != nil {
		log.Printf("[pos] fuel freeze: %v", err)
		return
	}
	log.Printf("[pos] fuel frozen for %.1f h of downtime on %d tower(s)", float64(shift)/36e9, n)
	stampStarbaseFuelSweep(sqldb)
}

// stampStarbaseFuelSweep records that the server is up now.
func stampStarbaseFuelSweep(sqldb *sql.DB) {
	ctx, cancel := context.WithTimeout(context.Background(), starbaseCtxTimeout)
	defer cancel()
	if err := evedb.StampStarbaseFuelSweep(ctx, sqldb, server.Win32TimeNow()); err != nil {
		log.Printf("[pos] fuel sweep: stamp: %v", err)
	}
}

// starbaseFuelExhausted offlines a tower that failed its fuel bill: DB state
// back to anchored, force field dropped, slim change broadcast to anyone in
// system.  The remaining bay contents are untouched (all-or-nothing consume).
func starbaseFuelExhausted(sqldb *sql.DB, r evedb.StarbaseFuelRow, missingTypeID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), starbaseCtxTimeout)
	defer cancel()
	if _, err := evedb.OfflineStarbase(ctx, sqldb, r.EntityID); err != nil {
		log.Printf("[pos] fuel exhausted: offline tower %d: %v", r.EntityID, err)
		return
	}
	if globalSysMgr != nil {
		if ss := globalSysMgr.Get(r.SolarSystemID); ss != nil {
			starbaseRemoveForcefield(ss, r.EntityID)
		}
	}
	offlineTowerSlaves(ctx, "pos-fuel", sqldb, r.EntityID)
	msg := starbasePushStateAndSchedule("pos-fuel", sqldb, globalSysMgr, r.EntityID)
	log.Printf("[pos] tower %d in system %d ran out of fuel (missing type %d) — offlined.%s",
		r.EntityID, r.SolarSystemID, missingTypeID, msg)
	fuelLowNotified.Delete(r.EntityID)
	evedb.EmitStarbaseEvent(evedb.StarbaseEvent{Kind: evedb.StarbaseEventOutOfFuel, TowerID: r.EntityID,
		SolarSystemID: r.SolarSystemID, OwnerCorpID: r.OwnerCorpID, OwnerAllianceID: r.OwnerAllianceID})
}

// fuelLowNotified holds the towers already told they are low on fuel, so
// StarbaseEventFuelLow fires once per crossing of starbase.fuelLowHours.
// Cleared when the bay climbs back or the tower runs dry; memory only.
var fuelLowNotified syncx.Map[int64, struct{}] // towerID → struct{}

// checkStarbaseFuelLow fires StarbaseEventFuelLow when a tower's bay first
// drops under lowHours. Called from the sweep after a burn, once per fuel cycle.
func checkStarbaseFuelLow(ctx context.Context, sqldb *sql.DB, r evedb.StarbaseFuelRow, lowHours float64) {
	hours, ok, err := evedb.StarbaseFuelHoursLeft(ctx, sqldb, r)
	if err != nil || !ok {
		return
	}
	if hours >= lowHours {
		fuelLowNotified.Delete(r.EntityID)
		return
	}
	if _, already := fuelLowNotified.LoadOrStore(r.EntityID, struct{}{}); already {
		return
	}
	log.Printf("[pos] tower %d in system %d is low on fuel: %.1f h left", r.EntityID, r.SolarSystemID, hours)
	evedb.EmitStarbaseEvent(evedb.StarbaseEvent{Kind: evedb.StarbaseEventFuelLow, TowerID: r.EntityID,
		SolarSystemID: r.SolarSystemID, OwnerCorpID: r.OwnerCorpID, OwnerAllianceID: r.OwnerAllianceID,
		HoursLeft: hours})
}

// offlineTowerSlaves takes a tower's structures offline with it and pushes
// each new state, because a slave runs on the tower's power and CPU.
func offlineTowerSlaves(ctx context.Context, addr string, sqldb *sql.DB, towerID int64) {
	ids, err := evedb.OfflineStructuresForTower(ctx, sqldb, towerID)
	if err != nil {
		log.Printf("[%s] offlineTowerSlaves %d: %v", addr, towerID, err)
		return
	}
	for _, id := range ids {
		structurePushStateAndSchedule(addr, sqldb, globalSysMgr, id)
	}
	if len(ids) > 0 {
		log.Printf("[%s] tower %d took %d structure(s) offline with it", addr, towerID, len(ids))
	}
}

// ─── structure bays (Access Resources / Access Storage) ────────────────────

// isStarbaseBay reports whether itemID is a category-23 structure in the
// player's current system, i.e. one whose bay the client may bind as an
// inventory. The client opens these with flagNone, so contents sit at
// locationID=structureID with flag 0.
func isStarbaseBay(ds *destinyConn, itemID int64) bool {
	if ds == nil || ds.solarSystem == nil || itemID <= 0 {
		return false
	}
	slim, ok := ds.solarSystem.GetStarbaseSlim(uint32(itemID))
	return ok && evedb.IsStructureLike(slim.CategoryID, slim.GroupID)
}

// secureCanGate is the password rule for a launched secure container (340/448). The owner always
// may; with a general password, anyone who entered it this session may (EnterPassword, alsc.go);
// without one, the owning corporation. Returns the client key to raise ("" = allowed;
// SCGeneralPasswordRequired makes invCache prompt and retry) and whether the item is a secure can.
func secureCanGate(ctx context.Context, sqldb *sql.DB, slim destiny.SlimItem, charID int64) (key string, isCan bool) {
	if !evedb.IsPasswordContainerGroup(slim.GroupID) {
		return "", false
	}
	// The entity row, not the slim: an anchored standalone's slim carries the
	// overlay's owning corporation, while the can itself stays the pilot's.
	ownerID := slim.OwnerID
	if er, err := evedb.GetEntityRow(ctx, sqldb, slim.ItemID); err == nil && er != nil {
		ownerID = er.OwnerID
	}
	if ownerID == charID {
		return "", true
	}
	sec, err := evedb.GetContainerSecurity(ctx, sqldb, slim.ItemID)
	if err == nil && sec.GeneralPw != "" {
		if containerUnlocked(charID, slim.ItemID).general {
			return "", true
		}
		return "SCGeneralPasswordRequired", true
	}
	if corpID := boundCharCorpID(charID, sqldb); corpID != 0 && ownerID == corpID {
		return "", true
	}
	return "CustomNotify", true
}

// isLootContainer reports whether itemID is a live loot container in the
// caller's current solar system.
func isLootContainer(ds *destinyConn, itemID int64) bool {
	if ds == nil || ds.solarSystem == nil || itemID <= 0 {
		return false
	}
	_, ok := ds.solarSystem.GetLootContainerPosition(uint32(itemID))
	return ok
}

// structureAccessAllowed is the Access tab's per-structure seam (view, put,
// take, use), the counterpart of deployAllowed. Allows everything unless
// starbase.enforceAccess is on; then applies db.CanUseStructure to slave
// structures only. Tower fuel bays and platform holds keep their own rules.
func structureAccessAllowed(ctx context.Context, addr string, sqldb *sql.DB, structureID, charID int64, action string) bool {
	if sqldb == nil || !evedb.StarbaseAccessEnforced(ctx, sqldb) {
		return true
	}
	s, found, err := evedb.GetStarbaseStructure(ctx, sqldb, structureID)
	if err != nil || !found {
		return true
	}
	if s.ControlTowerID == 0 {
		// A standalone structure (Mobile Storage) has no tower and so no
		// Access tab: its owner's corp, or alliance in 0.0, may use it.
		_, corpID, allianceID, err := evedb.GetCharSovContext(ctx, sqldb, charID)
		return err == nil && evedb.StarbaseManagePermitted(s, corpID, allianceID)
	}
	tower, found, err := evedb.GetStarbase(ctx, sqldb, s.ControlTowerID)
	if err != nil || !found {
		return true
	}
	acc, err := evedb.GetStructureAccess(ctx, sqldb, structureID, tower.EntityID)
	if err != nil {
		log.Printf("[%s] structureAccessAllowed %d: %v (using default)", addr, structureID, err)
	}
	a, err := evedb.LoadStarbaseActor(ctx, sqldb, charID)
	if err != nil {
		return false
	}
	if !evedb.CanUseStructure(acc, tower, a, action) {
		log.Printf("[%s] starbase access: char %d may not %s structure %d", addr, charID, action, structureID)
		return false
	}
	return true
}

// structureTakeAllowed gates a pilot taking an item out of a structure bay or
// platform hold. The owner rule mirrors the client's menu predicate
// (checkIsMineOrCorpsOrAlliances) and always applies; the Access tab's levels
// ride on top through structureAccessAllowed.
func structureTakeAllowed(ctx context.Context, addr string, sqldb *sql.DB, ds *destinyConn, structureID, charID, itemFlag int64) bool {
	if ds == nil || ds.solarSystem == nil || sqldb == nil {
		return false
	}
	slim, ok := ds.solarSystem.GetStarbaseSlim(uint32(structureID))
	if !ok {
		return false
	}
	_, corpID, allianceID, err := evedb.GetCharSovContext(ctx, sqldb, charID)
	if err != nil {
		return false
	}
	// A corp hangar's divisions are corp roles: members only, and the
	// division's take role.
	if evedb.IsCorpHangarStructureGroup(slim.GroupID) {
		if corpID == 0 || slim.CorpID != corpID || !corpHangarMayTake(ctx, sqldb, corpID, charID, itemFlag) {
			log.Printf("[%s] structure take: char %d lacks the take role for flag %d of corp hangar %d",
				addr, charID, itemFlag, structureID)
			return false
		}
		return structureAccessAllowed(ctx, addr, sqldb, structureID, charID, evedb.StructureActionTake)
	}
	if key, isCan := secureCanGate(ctx, sqldb, slim, charID); isCan {
		if key != "" {
			log.Printf("[%s] secure can take: char %d refused at %d (%s)", addr, charID, structureID, key)
			return false
		}
		return true
	}
	owned := slim.OwnerID == charID || (corpID != 0 && slim.CorpID == corpID) ||
		(allianceID != 0 && slim.AllianceID == allianceID)
	if !owned {
		log.Printf("[%s] structure take: char %d (corp %d alliance %d) does not own structure %d (corp %d alliance %d)",
			addr, charID, corpID, allianceID, structureID, slim.CorpID, slim.AllianceID)
		return false
	}
	return structureAccessAllowed(ctx, addr, sqldb, structureID, charID, evedb.StructureActionTake)
}

// handleStarbaseBayAdd implements InventoryBound.Add against a structure bay.
// Only items in the caller's cargo hold or already inside the structure move.
func handleStarbaseBayAdd(
	c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID, charID int64,
	structureID, itemID, qty, flag int64, respond bool, sqldb *sql.DB, ds *destinyConn,
) error {
	// MultiAdd calls this once per item and answers the call itself.
	ok := func() error {
		if !respond {
			return nil
		}
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	refuse := func(key string, args map[string]marshal.PyRep) error {
		if !respond {
			return nil
		}
		return server.SendCallException(c, pkt, accountID, server.BuildUserError(key, args))
	}
	custom := func(msg string) error {
		return refuse("CustomError", map[string]marshal.PyRep{"error": marshal.PyString{Value: msg}})
	}
	if sqldb == nil || charID == 0 {
		return ok()
	}
	ctx, cancel := context.WithTimeout(context.Background(), starbaseCtxTimeout)
	defer cancel()

	srcER, err := evedb.GetEntityRow(ctx, sqldb, itemID)
	if err != nil || srcER == nil {
		log.Printf("[%s] starbase bay Add: GetEntityRow(%d): %v", addr, itemID, err)
		return ok()
	}
	// A drag between two windows of one structure is a flag change on a row
	// already inside; anything else must come out of the pilot's hold.
	internal := srcER.LocationID == structureID
	if !internal && (ds.shipID == 0 || srcER.LocationID != ds.shipID) {
		log.Printf("[%s] starbase bay Add: item %d is not in the caller's cargo (loc %d, ship %d)",
			addr, itemID, srcER.LocationID, ds.shipID)
		return custom("That item must be in your cargo hold.")
	}
	// Range: the bind outlives the client's menu, so distance is checked here too.
	if bd, found := ds.solarSystem.GetStarbaseBall(uint32(structureID)); !found ||
		!shipWithin(ds, bd.Pos, itemTransferRange+bd.Radius) {
		return custom("You are too far away from that structure.")
	}
	// Flag: evedb.StructureBayFlags is the per-group table (corp hangar
	// divisions, the refinery's processing area, a sentry's ammunition).
	slim, _ := ds.solarSystem.GetStarbaseSlim(uint32(structureID))
	flag = evedb.StructureBayFlag(slim.GroupID, flag)
	if flag == evedb.FlagSentryAmmo && evedb.IsSentryGroup(slim.GroupID) {
		if srcER.CategoryID != 8 {
			return custom("Only ammunition can be loaded into a sentry.")
		}
		if err := evedb.ValidateChargeForModule(ctx, sqldb, srcER.TypeID, slim.TypeID); err != nil {
			log.Printf("[%s] starbase bay Add: charge %d does not fit sentry %d: %v", addr, srcER.TypeID, slim.TypeID, err)
			return custom("That ammunition cannot be used by this structure.")
		}
	}
	if internal && flag == srcER.Flag && (qty <= 0 || qty >= srcER.Quantity) {
		return ok() // dropped where it already is; a smaller qty is a split in place
	}
	// A corp hangar holds corp property in role-gated divisions, as an office does:
	// members only, the take role on both ends, and the deposit becomes the corporation's.
	var ownerOverride int64
	corpHangar := evedb.IsCorpHangarStructureGroup(slim.GroupID)
	if corpHangar {
		if boundCharCorpID(charID, sqldb) != slim.CorpID {
			return custom("Only members of the owning corporation may use its hangar.")
		}
		if !corpHangarMayTake(ctx, sqldb, slim.CorpID, charID, flag) ||
			(internal && !corpHangarMayTake(ctx, sqldb, slim.CorpID, charID, srcER.Flag)) {
			return custom("You do not have the hangar role for that division.")
		}
		ownerOverride = slim.CorpID
	} else if internal && !structureTakeAllowed(ctx, addr, sqldb, ds, structureID, charID, srcER.Flag) {
		return custom("You are not permitted to take items from this structure.")
	}
	oldLocID, oldFlag := srcER.LocationID, srcER.Flag
	if !structureAccessAllowed(ctx, addr, sqldb, structureID, charID, evedb.StructureActionPut) {
		return custom("You are not permitted to put items into this structure.")
	}

	res, err := evedb.AddToStarbaseBay(ctx, sqldb, structureID, itemID, qty, flag, ownerOverride, corpHangar)
	if errors.Is(err, evedb.ErrBayFull) {
		capFlag := flag
		if corpHangar {
			capFlag = -1
		}
		capacity, used, _ := evedb.StructureBayCapacity(ctx, sqldb, structureID, capFlag)
		// The units that would have moved: qty, or the whole stack for qty <= 0 or over it.
		movedQty := qty
		if movedQty <= 0 || movedQty > srcER.Quantity {
			movedQty = srcER.Quantity
		}
		return refuse("NotEnoughCargoSpace", map[string]marshal.PyRep{
			"volume":    marshal.PyReal{Value: evedb.ItemVolume(ctx, sqldb, srcER.ItemID, srcER.TypeID) * float64(movedQty)},
			"available": marshal.PyReal{Value: capacity - used},
		})
	}
	if err != nil {
		log.Printf("[%s] starbase bay Add: %v", addr, err)
		return custom(err.Error())
	}

	notify := func(row *evedb.EntityRow, change marshal.PyRep) {
		if row == nil {
			return
		}
		_ = server.SendNotification(c, accountID, "OnItemChange", "charid",
			marshal.PyTuple{Items: []marshal.PyRep{evedb.BuildEntityRow(row), change}})
	}
	changeKey := func(key, val int64) marshal.PyRep {
		return marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyInt{Value: uint64(key)}, Value: marshal.PyInt{Value: uint64(val)}},
		}}
	}

	switch {
	case res.SourceDeleted:
		// The dragged row is gone; the client removes it when the change
		// dict is None.
		notify(srcER, marshal.PyNone{})
	case res.SourceMoved:
		if movedER, err := evedb.GetEntityRow(ctx, sqldb, itemID); err == nil {
			notify(movedER, marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: uint64(oldLocID)}},
				{Key: marshal.PyInt{Value: 4}, Value: marshal.PyInt{Value: uint64(oldFlag)}},
			}})
		}
	case res.SourceOldQty > 0:
		if leftER, err := evedb.GetEntityRow(ctx, sqldb, itemID); err == nil {
			notify(leftER, changeKey(7, res.SourceOldQty))
		}
	}
	// The bay-side row: a brand new stack has no prior state to diff against.
	if res.TargetItemID != 0 && res.TargetItemID != itemID {
		if dstER, err := evedb.GetEntityRow(ctx, sqldb, res.TargetItemID); err == nil {
			if res.TargetNew {
				notify(dstER, marshal.PyDict{})
			} else {
				notify(dstER, changeKey(7, res.TargetOldQty))
			}
		}
	}

	log.Printf("[%s] starbase bay Add: item %d qty %d → structure %d flag %d (target row %d, merged=%v)",
		addr, itemID, qty, structureID, flag, res.TargetItemID, !res.TargetNew && !res.SourceMoved)
	return ok()
}

// ─── online / offline / unanchor (dogmaIM structure effects) ───────────────

// structureEffectID extracts the effect argument of dogmaIM.Activate/
// Deactivate when it names a structure effect. Menus send numeric IDs, godma
// sends string names; both are accepted.
func structureEffectID(arg marshal.PyRep) (int64, bool) {
	switch v := arg.(type) {
	case marshal.PyInt:
		id := int64(v.Value)
		switch id {
		case effectOnlineForStructures, effectAnchorDropForStructures,
			effectAnchorLiftForStructures, effectAnchorDrop, effectAnchorLift:
			return id, true
		}
	case marshal.PyString:
		switch v.Value {
		case "onlineForStructures":
			return effectOnlineForStructures, true
		case "anchorDropForStructures":
			return effectAnchorDropForStructures, true
		case "anchorLiftForStructures":
			return effectAnchorLiftForStructures, true
		case "anchorDrop":
			return effectAnchorDrop, true
		case "anchorLift":
			return effectAnchorLift, true
		}
	}
	return 0, false
}

// handleStructureEffect processes a structure effect Activate/Deactivate.
// Returns true when the call was consumed (caller replies PyNone and skips
// the ship-module path).
func handleStructureEffect(ds *destinyConn, addr string, charID, itemID, effectID int64, activate bool, sqldb *sql.DB) bool {
	if ds.solarSystem == nil || sqldb == nil {
		return false
	}
	// Outpost platforms answer the generic anchor effects and are not in the
	// starbase table; dispatched first, as a GetStarbase miss drops the call into the ship-module path.
	if handlePlatformEffect(ds, addr, charID, itemID, effectID, activate, sqldb) {
		return true
	}
	// Mobile Storage and other standalone anchorables answer the same generic
	// pair and have no tower; see mobilestorage.go.
	if handleStandaloneEffect(ds, addr, charID, itemID, effectID, activate, sqldb) {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), starbaseCtxTimeout)
	defer cancel()
	rec, isStarbase, err := evedb.GetStarbase(ctx, sqldb, itemID)
	if err != nil {
		log.Printf("[%s] structureEffect: GetStarbase %d: %v", addr, itemID, err)
		return false
	}
	if !isStarbase {
		// Slave structures carry the same three effects in their own overlay;
		// anything still unrecognised is not an anchored structure.
		return handleSlaveStructureEffect(ctx, ds, addr, charID, itemID, effectID, activate, sqldb)
	}
	_, corpID, allianceID, err := evedb.GetCharSovContext(ctx, sqldb, charID)
	if err != nil || !evedb.StarbaseManagePermitted(rec, corpID, allianceID) {
		log.Printf("[%s] structureEffect: char %d (corp %d alliance %d) cannot manage tower %d (corp %d alliance %d)",
			addr, charID, corpID, allianceID, itemID, rec.OwnerCorpID, rec.OwnerAllianceID)
		platformNotify(ds, "You are not permitted to manage this control tower.")
		return true // consumed; refused
	}
	if !deployAllowed(ctx, addr, sqldb, rec, charID, structureEffectAction(effectID, activate)) {
		platformNotify(ds, "This control tower's access settings do not allow you to do that.")
		return true // consumed; refused by the tower's Access tab
	}

	switch {
	case effectID == effectOnlineForStructures && activate:
		if _, err := evedb.OnlineStarbase(ctx, sqldb, itemID); err != nil {
			log.Printf("[%s] structureEffect online %d: %v", addr, itemID, err)
			platformNotify(ds, "Cannot put the control tower online: "+err.Error())
			return true
		}
		msg := starbasePushStateAndSchedule(addr, sqldb, globalSysMgr, itemID)
		log.Printf("[%s] tower %d onlining via dogma effect.%s", addr, itemID, msg)
		return true

	case effectID == effectOnlineForStructures && !activate:
		if _, err := evedb.OfflineStarbase(ctx, sqldb, itemID); err != nil {
			log.Printf("[%s] structureEffect offline %d: %v", addr, itemID, err)
			platformNotify(ds, "Cannot take the control tower offline: "+err.Error())
			return true
		}
		starbaseRemoveForcefield(ds.solarSystem, itemID)
		offlineTowerSlaves(ctx, addr, sqldb, itemID)
		msg := starbasePushStateAndSchedule(addr, sqldb, globalSysMgr, itemID)
		log.Printf("[%s] tower %d offlined via dogma effect.%s", addr, itemID, msg)
		evedb.EmitStarbaseEvent(evedb.StarbaseEventFor(evedb.StarbaseEventOffline, rec))
		return true

	case effectID == effectAnchorLiftForStructures && activate:
		// Unanchoring runs the type's unanchoringDelay first. Until then the tower stays
		// anchored (moon, sov, rigid ball) and cannot be onlined; finishStarbaseUnanchor completes it.
		if why := beginUnanchorRefusal(sqldb, rec); why != "" {
			platformNotify(ds, "Cannot unanchor: "+why)
			return true
		}
		if _, err := evedb.BeginStarbaseUnanchor(ctx, sqldb, itemID); err != nil {
			log.Printf("[%s] structureEffect unanchor %d: %v", addr, itemID, err)
			platformNotify(ds, "Cannot unanchor: "+err.Error())
			return true
		}
		msg := starbasePushStateAndSchedule(addr, sqldb, globalSysMgr, itemID)
		scheduleStructureUnanchor(sqldb, itemID, true, time.Now().Add(
			time.Duration(typeAttrMs(sqldb, rec.TypeID, "unanchoringDelay"))*time.Millisecond))
		log.Printf("[%s] tower %d unanchoring (char %d).%s", addr, itemID, charID, msg)
		return true
	}
	return false
}

// beginUnanchorRefusal is the server's copy of the client's
// CanUnanchorStructure: anchored (never online or onlining) and done
// anchoring. "" when the countdown may begin.
func beginUnanchorRefusal(sqldb *sql.DB, rec evedb.StarbaseRecord) string {
	switch rec.State {
	case evedb.StarbaseStateAnchored:
	case evedb.StarbaseStateUnanchoring:
		return "it is already unanchoring"
	default:
		return "take it offline first"
	}
	if !evedb.DelayElapsed(rec.AnchoredAt, typeAttrMs(sqldb, rec.TypeID, "anchoringDelay"), server.Win32TimeNow()) {
		return "it has not finished anchoring yet"
	}
	return ""
}

// finishStarbaseUnanchor completes a tower's unanchoring once its delay has
// run: overlay gone, slaves released, sov dropped if it was the alliance's last
// tower here, ball set free. Idempotent (the one-shot and the sweep may both
// arrive) and works with the system unloaded.
func finishStarbaseUnanchor(sqldb *sql.DB, entityID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), starbaseCtxTimeout)
	defer cancel()
	rec, found, err := evedb.GetStarbase(ctx, sqldb, entityID)
	if err != nil || !found || rec.State != evedb.StarbaseStateUnanchoring {
		return
	}
	if !evedb.DelayElapsed(rec.UnanchorAt, typeAttrMs(sqldb, rec.TypeID, "unanchoringDelay"), server.Win32TimeNow()) {
		return
	}
	if _, err := evedb.UnanchorStarbase(ctx, sqldb, entityID); err != nil {
		log.Printf("[pos] tower %d finish unanchor: %v", entityID, err)
		return
	}
	var ss *destiny.SolarSystem
	if globalSysMgr != nil {
		ss = globalSysMgr.Get(rec.SolarSystemID)
	}
	// The tower's slaves go free with it: bound to a tower that no longer exists
	// they could never be onlined, and the client's LocateControlTower finds nothing.
	releaseTowerSlaves(ctx, "pos-unanchor", sqldb, ss, entityID)
	starbaseDropSovIfLast(ctx, "pos-unanchor", sqldb, rec)
	if ss != nil {
		starbaseSwapToFreeSettled(ss, rec)
	}
	posTowerCache.Delete(entityID)
	log.Printf("[pos] tower %d unanchored", entityID)
	evedb.EmitStarbaseEvent(evedb.StarbaseEventFor(evedb.StarbaseEventUnanchored, rec))
}

// scheduleStructureUnanchor arms the one-shot that finishes an unanchoring
// countdown, for a tower or any starbaseStructures row. The onlining sweeps
// are the backstop after a restart.
func scheduleStructureUnanchor(sqldb *sql.DB, entityID int64, tower bool, fireAt time.Time) {
	if globalTickMgr == nil {
		return
	}
	globalTickMgr.ScheduleOnce(fmt.Sprintf("pos-unanchor-%d", entityID), fireAt, func() {
		if tower {
			finishStarbaseUnanchor(sqldb, entityID)
		} else {
			finishStructureUnanchor(sqldb, entityID)
		}
	})
}

// sweepStarbaseUnanchoring finishes every tower whose unanchoring delay has
// run; called from the tower onlining sweep.
func sweepStarbaseUnanchoring(sqldb *sql.DB) {
	ctx, cancel := context.WithTimeout(context.Background(), starbaseCtxTimeout)
	recs, err := evedb.ListStarbasesByState(ctx, sqldb, evedb.StarbaseStateUnanchoring)
	cancel()
	if err != nil {
		log.Printf("[pos] tower unanchor sweep: %v", err)
		return
	}
	now := server.Win32TimeNow()
	for _, r := range recs {
		if evedb.DelayElapsed(r.UnanchorAt, typeAttrMs(sqldb, r.TypeID, "unanchoringDelay"), now) {
			finishStarbaseUnanchor(sqldb, r.EntityID)
		}
	}
}

// structureEffectAction names the tower-window deploy flag an effect call is
// governed by ("" = none).
func structureEffectAction(effectID int64, activate bool) string {
	switch {
	case effectID == effectOnlineForStructures && activate:
		return "online"
	case effectID == effectOnlineForStructures && !activate:
		return "offline"
	case effectID == effectAnchorLiftForStructures && activate:
		return "unanchor"
	}
	return ""
}

// deployAllowed is the enforcement seam for the Access tab's deploy flags
// (anchor, unanchor, online, offline), consulted after StarbaseManagePermitted
// by every path that performs one. Allows everything unless starbase.enforceAccess
// is on; then applies db.CanDeploy against the tower's policy (a slave's tower).
func deployAllowed(ctx context.Context, addr string, sqldb *sql.DB, tower evedb.StarbaseRecord, charID int64, action string) bool {
	if action == "" || !evedb.StarbaseAccessEnforced(ctx, sqldb) {
		return true
	}
	p, err := evedb.GetStarbasePolicy(ctx, sqldb, tower) // default policy on error
	if err != nil {
		log.Printf("[%s] deployAllowed tower %d policy: %v (using default)", addr, tower.EntityID, err)
	}
	a, err := evedb.LoadStarbaseActor(ctx, sqldb, charID)
	if err != nil {
		log.Printf("[%s] deployAllowed char %d: %v", addr, charID, err)
		return false
	}
	if !evedb.CanDeploy(p, tower, a, action) {
		log.Printf("[%s] starbase access: char %d may not %s on tower %d (level %d)",
			addr, charID, action, tower.EntityID, p.Deploy[action])
		return false
	}
	return true
}

// handleSlaveStructureEffect is handleStructureEffect for a structure anchored
// to a tower. Same three effects; the permission rule reads the slave's
// recorded owner, which it took from the tower at anchor time.
func handleSlaveStructureEffect(
	ctx context.Context, ds *destinyConn, addr string,
	charID, itemID, effectID int64, activate bool, sqldb *sql.DB,
) bool {
	rec, ok, err := evedb.GetStarbaseStructure(ctx, sqldb, itemID)
	if err != nil {
		log.Printf("[%s] structureEffect: GetStarbaseStructure %d: %v", addr, itemID, err)
		return false
	}
	if !ok {
		return false
	}
	_, corpID, allianceID, err := evedb.GetCharSovContext(ctx, sqldb, charID)
	if err != nil || !evedb.StarbaseManagePermitted(rec, corpID, allianceID) {
		log.Printf("[%s] structureEffect: char %d cannot manage structure %d (corp %d alliance %d)",
			addr, charID, itemID, rec.OwnerCorpID, rec.OwnerAllianceID)
		platformNotify(ds, "You are not permitted to manage this structure.")
		return true // consumed; refused
	}
	if action := structureEffectAction(effectID, activate); action != "" {
		if tower, found, _ := evedb.GetStarbase(ctx, sqldb, rec.ControlTowerID); found &&
			!deployAllowed(ctx, addr, sqldb, tower, charID, action) {
			platformNotify(ds, "This control tower's access settings do not allow you to do that.")
			return true // consumed; refused by the tower's Access tab
		}
	}

	switch {
	case effectID == effectOnlineForStructures && activate:
		// A slave draws power and CPU from its tower, so it cannot come up under a
		// tower that is not up; the client refuses the same way (CantTowerNotOnline).
		if rec.ControlTowerID == 0 {
			// Mobile Storage: anchored on its own, nothing to power.
			platformNotify(ds, "This structure works as soon as it is anchored; it has no online state.")
			return true
		}
		settleTowerOnlining(ctx, sqldb, rec.ControlTowerID)
		tower, found, tErr := evedb.GetStarbase(ctx, sqldb, rec.ControlTowerID)
		if tErr != nil || !found ||
			(tower.State != evedb.StarbaseStateOnline && tower.State != evedb.StarbaseStateReinforced) {
			log.Printf("[%s] structureEffect: structure %d cannot online — tower %d is not online",
				addr, itemID, rec.ControlTowerID)
			platformNotify(ds, "Cannot put online: "+towerNotOnlineReason(sqldb, tower, found))
			return true
		}
		if why := structureGridRefusal(ctx, sqldb, tower, rec); why != "" {
			log.Printf("[%s] structureEffect: structure %d refused online — %s", addr, itemID, why)
			platformNotify(ds, why)
			return true
		}
		if _, err := evedb.OnlineStructure(ctx, sqldb, itemID); err != nil {
			log.Printf("[%s] structureEffect online structure %d: %v", addr, itemID, err)
			platformNotify(ds, "Cannot put online: the structure is not in the anchored state.")
			return true
		}
		msg := structurePushStateAndSchedule(addr, sqldb, globalSysMgr, itemID)
		log.Printf("[%s] structure %d onlining via dogma effect.%s", addr, itemID, msg)
		return true

	case effectID == effectOnlineForStructures && !activate:
		if _, err := evedb.OfflineStructure(ctx, sqldb, itemID); err != nil {
			log.Printf("[%s] structureEffect offline structure %d: %v", addr, itemID, err)
			platformNotify(ds, "Cannot take offline: the structure is not online.")
			return true
		}
		msg := structurePushStateAndSchedule(addr, sqldb, globalSysMgr, itemID)
		log.Printf("[%s] structure %d offlined via dogma effect.%s", addr, itemID, msg)
		return true

	case effectID == effectAnchorLiftForStructures && activate:
		// The type's unanchoringDelay (10 min on most slaves) runs first; the
		// structure stays anchored to its tower until finishStructureUnanchor.
		if why := beginUnanchorRefusal(sqldb, rec); why != "" {
			platformNotify(ds, "Cannot unanchor: "+why)
			return true
		}
		if _, err := evedb.BeginStructureUnanchor(ctx, sqldb, itemID); err != nil {
			log.Printf("[%s] structureEffect unanchor structure %d: %v", addr, itemID, err)
			platformNotify(ds, "Cannot unanchor: "+err.Error())
			return true
		}
		msg := structurePushStateAndSchedule(addr, sqldb, globalSysMgr, itemID)
		scheduleStructureUnanchor(sqldb, itemID, false, time.Now().Add(
			time.Duration(typeAttrMs(sqldb, rec.TypeID, "unanchoringDelay"))*time.Millisecond))
		log.Printf("[%s] structure %d unanchoring (char %d).%s", addr, itemID, charID, msg)
		return true
	}
	return false
}

// structureGridRefusal is the power-grid / CPU gate on onlining a slave: ""
// when the tower can carry it or starbase.enforceGrid is off, else the sentence
// the pilot is shown, in the numbers the management window's gauges print.
func structureGridRefusal(ctx context.Context, sqldb *sql.DB, tower, rec evedb.StarbaseRecord) string {
	// The rule lives in db so the headless online seam refuses what a pilot is refused.
	return evedb.StructureGridRefusal(ctx, sqldb, tower, rec)
}

// releaseTowerSlaves drops every slave overlay row bound to towerID and
// returns their balls to the free presentation with no countdown, so a pilot
// can scoop or re-anchor them. ss may be nil: the rows still go and the loader
// builds free balls.
func releaseTowerSlaves(ctx context.Context, addr string, sqldb *sql.DB, ss *destiny.SolarSystem, towerID int64) {
	slaves, err := evedb.ListStarbaseStructuresForTower(ctx, sqldb, towerID)
	if err != nil {
		log.Printf("[%s] releaseTowerSlaves %d: %v", addr, towerID, err)
		return
	}
	if len(slaves) == 0 {
		return
	}
	if _, err := evedb.RemoveStarbaseStructures(ctx, sqldb, towerID); err != nil {
		log.Printf("[%s] releaseTowerSlaves %d delete: %v", addr, towerID, err)
		return
	}
	for _, s := range slaves {
		if ss != nil {
			starbaseSwapToFreeSettled(ss, s)
		}
	}
	log.Printf("[%s] tower %d released %d anchored structure(s)", addr, towerID, len(slaves))
}

// starbasePushStateAndSchedule re-reads a tower, arms the onlining completion
// when needed, updates the stored slim, and broadcasts OnSlimItemChange.
// Shared by the /posonline notifier and the dogma effect path.
func starbasePushStateAndSchedule(addr string, sqldb *sql.DB, sysMgr *destiny.SystemManager, entityID int64) string {
	if sqldb == nil || sysMgr == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), starbaseCtxTimeout)
	defer cancel()
	rec, ok, err := evedb.GetStarbase(ctx, sqldb, entityID)
	if err != nil || !ok {
		return ""
	}
	if rec.State == evedb.StarbaseStateOnlining {
		delayMs := starbaseOnliningDelayMs(sqldb, rec.TypeID)
		scheduleStarbaseOnliningCompletion(sqldb, entityID,
			time.Now().Add(time.Duration(delayMs)*time.Millisecond))
	}
	ss := sysMgr.Get(rec.SolarSystemID)
	if ss == nil {
		return ""
	}
	_, slim, _ := starbaseBallSlimDmg(rec)
	if !ss.UpdateStarbaseSlim(uint32(entityID), slim) {
		return ""
	}
	n := broadcastSlimChange(ss, slim)
	return fmt.Sprintf(" State pushed to %d pilot(s) in system.", n)
}

// structurePushStateAndSchedule is starbasePushStateAndSchedule for a slave:
// re-read, arm the onlining completion, update the stored slim, broadcast.
func structurePushStateAndSchedule(addr string, sqldb *sql.DB, sysMgr *destiny.SystemManager, entityID int64) string {
	if sqldb == nil || sysMgr == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), starbaseCtxTimeout)
	defer cancel()
	rec, ok, err := evedb.GetStarbaseStructure(ctx, sqldb, entityID)
	if err != nil || !ok {
		return ""
	}
	if rec.State == evedb.StarbaseStateOnlining {
		delayMs := starbaseOnliningDelayMs(sqldb, rec.TypeID)
		scheduleStructureOnliningCompletion(sqldb, entityID,
			time.Now().Add(time.Duration(delayMs)*time.Millisecond))
	}
	ss := sysMgr.Get(rec.SolarSystemID)
	if ss == nil {
		return ""
	}
	_, slim, _ := starbaseBallSlimDmg(rec)
	if !ss.UpdateStarbaseSlim(uint32(entityID), slim) {
		return ""
	}
	n := broadcastSlimChange(ss, slim)
	return fmt.Sprintf(" State pushed to %d pilot(s) in system.", n)
}

// startStructureOnliningSweep is the slave counterpart of the tower onlining
// sweep: the DB is walked as the backstop for the in-memory one-shot.
func startStructureOnliningSweep(tickMgr *tick.Manager, sqldb *sql.DB) {
	if tickMgr == nil || sqldb == nil {
		return
	}
	const everyTicks = 30 // tick interval is 1s
	tickMgr.RegisterPeriodic("starbase-structure-onlining-sweep", everyTicks, func(uint64) bool {
		ctx, cancel := context.WithTimeout(context.Background(), starbaseCtxTimeout)
		recs, err := evedb.ListStarbaseStructuresByState(ctx, sqldb, evedb.StarbaseStateOnlining)
		cancel()
		if err != nil {
			log.Printf("[pos] structure onlining sweep: %v", err)
			return true
		}
		now := server.Win32TimeNow()
		for _, r := range recs {
			if float64(now-r.OnlinedAt)/10000.0 < starbaseOnliningDelayMs(sqldb, r.TypeID) {
				continue
			}
			finishStructureOnlining(sqldb, r.EntityID)
		}
		// Slave and standalone unanchoring countdowns: the one-shot does not survive a restart; this does.
		sweepStructureUnanchoring(sqldb)
		return true
	})
}

// refreshStarbaseForcefield re-presents an online tower's force-field ball after
// its settings changed (posmgr.ForcefieldChanged). The Force Field tab reads its
// checkboxes off the ball's corpID/allianceID, sent only in AddBalls, so the ball
// is removed and re-added. No-op with the system unloaded, the tower offline, or enforceAccess off.
func refreshStarbaseForcefield(towerID int64) {
	if globalSysMgr == nil || globalDB == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	enforced := evedb.StarbaseAccessEnforced(ctx, globalDB)
	rec, ok, err := evedb.GetStarbase(ctx, globalDB, towerID)
	cancel()
	if !enforced || err != nil || !ok {
		return // unenforced: the ball would be rebuilt identical
	}
	ss := globalSysMgr.Get(rec.SolarSystemID)
	if ss == nil {
		return
	}
	ffID := starbaseForcefieldID(towerID)
	if _, exists := ss.GetStarbasePosition(ffID); !exists {
		return
	}
	ss.RemoveStarbase(ffID)
	broadcastBallRemove(ss, ffID)
	bd, slim, dmg := starbaseForcefield(globalDB, rec)
	ss.AddStarbase(bd, slim, dmg)
	broadcastBallAdd(ss, bd, slim, dmg)
}

// starbaseRemoveForcefield drops a tower's force-field ball (if present) and
// broadcasts the removal.
func starbaseRemoveForcefield(ss *destiny.SolarSystem, towerID int64) {
	ffID := starbaseForcefieldID(towerID)
	if !ss.RemoveStarbase(ffID) {
		return
	}
	broadcastBallRemove(ss, ffID)
}

// starbaseSwapToFree replaces an unanchored tower's rigid ball with the free
// launched-structure presentation (posState 0, unanchoring countdown from
// now) and broadcasts the swap.
func starbaseSwapToFree(ss *destiny.SolarSystem, rec evedb.StarbaseRecord) {
	swapStructureToFree(ss, rec, true)
}

// starbaseSwapToFreeSettled is starbaseSwapToFree for a structure whose
// unanchoring countdown has already run on the server: the free ball carries
// no timestamp, or the client draws the countdown a second time.
func starbaseSwapToFreeSettled(ss *destiny.SolarSystem, rec evedb.StarbaseRecord) {
	swapStructureToFree(ss, rec, false)
}

func swapStructureToFree(ss *destiny.SolarSystem, rec evedb.StarbaseRecord, countdown bool) {
	ss.RemoveStarbase(uint32(rec.EntityID))
	st := evedb.LaunchedStructure{
		ItemID: rec.EntityID, TypeID: rec.TypeID, GroupID: rec.GroupID,
		CategoryID: rec.CategoryID, TypeName: rec.ItemName,
		Radius: rec.Radius, Mass: 100000,
	}
	bd, slim, dmg := launchedStructureBallSlim(st,
		destiny.Vec3{X: rec.X, Y: rec.Y, Z: rec.Z},
		rec.OwnerCorpID, rec.OwnerCorpID, rec.OwnerAllianceID)
	// posState 0 with a fresh timestamp: the client draws 'unanchoring' until
	// unanchoringDelay elapses (cosmetic; the server treats the ball as free).
	if countdown {
		slim.PosTimestamp = server.Win32TimeNow()
	}
	ss.AddStarbase(bd, slim, dmg)
	broadcastBallSwap(ss, uint32(rec.EntityID), bd, slim, dmg)
}

// starbaseDropSovIfLast releases sovereignty when the unanchored/removed
// tower was its alliance's last POS in the system (same rule as /posremove).
func starbaseDropSovIfLast(ctx context.Context, addr string, sqldb *sql.DB, rec evedb.StarbaseRecord) {
	released, err := evedb.DropSovIfLastStarbase(ctx, sqldb, rec.SolarSystemID, rec.OwnerAllianceID)
	if err != nil {
		log.Printf("[%s] unanchor: clear sov in %d: %v", addr, rec.SolarSystemID, err)
		return
	}
	if released {
		log.Printf("[%s] sov released in system %d (last POS of alliance %d unanchored)",
			addr, rec.SolarSystemID, rec.OwnerAllianceID)
	}
}
