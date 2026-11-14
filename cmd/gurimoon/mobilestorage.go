package main

// Standalone anchorables: Mobile Storage (group 364) and anything else in
// category 23 that carries the generic anchor effects (649 anchorDrop, 650
// anchorLift) instead of the structure ones. The client anchors them through
// menusvc AnchorObject → dogmaIM.Activate(itemID, 649), as an outpost
// platform, with no coordinates and no tower. An anchored one is a
// starbaseStructures row with towerID 0. Anchoring is immediate on the server
// (the client draws the anchoringDelay countdown itself); unanchoring takes
// the type's unanchoringDelay, finished by a one-shot with the structure
// onlining sweep as the restart backstop.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/server"
)

// handleStandaloneEffect consumes anchorDrop/anchorLift on a standalone
// anchorable. Returns true when the call was consumed.
func handleStandaloneEffect(ds *destinyConn, addr string, charID, itemID, effectID int64, activate bool, sqldb *sql.DB) bool {
	// "Unanchor Structure" sends anchorLiftForStructures (1023) where the
	// generic "Unanchor" sends 650; both mean the same here.
	if effectID == effectAnchorLiftForStructures {
		effectID = effectAnchorLift
	}
	if !activate || (effectID != effectAnchorDrop && effectID != effectAnchorLift) ||
		ds == nil || ds.solarSystem == nil || sqldb == nil {
		return false
	}
	slim, ok := ds.solarSystem.GetStarbaseSlim(uint32(itemID))
	if !ok || !evedb.IsStructureLike(slim.CategoryID, slim.GroupID) {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), starbaseCtxTimeout)
	defer cancel()
	standalone, err := evedb.IsStandaloneAnchorable(ctx, sqldb, slim.TypeID)
	if err != nil || !standalone {
		return false
	}
	switch effectID {
	case effectAnchorDrop:
		if err := anchorStandaloneForPlayer(ctx, ds, addr, charID, itemID, slim, sqldb); err != nil {
			log.Printf("[%s] standalone %d anchor refused: %v", addr, itemID, err)
			posRefusal(ds, "Cannot anchor", err)
		}
	case effectAnchorLift:
		if err := unanchorStandaloneForPlayer(ctx, ds, addr, charID, itemID, sqldb); err != nil {
			log.Printf("[%s] standalone %d unanchor refused: %v", addr, itemID, err)
			posRefusal(ds, "Cannot unanchor", err)
		}
	}
	return true
}

// isStandaloneStructure reports whether a structure ball is a standalone
// anchorable (Mobile Storage), for the posMgr.AnchorStructure path the
// client's Anchor Structure entry takes for it.
func isStandaloneStructure(sqldb *sql.DB, slim destiny.SlimItem) bool {
	if !evedb.IsStructureLike(slim.CategoryID, slim.GroupID) || slim.GroupID == evedb.GroupControlTower {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), starbaseCtxTimeout)
	defer cancel()
	ok, err := evedb.IsStandaloneAnchorable(ctx, sqldb, slim.TypeID)
	return err == nil && ok
}

// anchorStandaloneForPlayer anchors the structure where its ball floats.
func anchorStandaloneForPlayer(ctx context.Context, ds *destinyConn, addr string, charID, itemID int64,
	slim destiny.SlimItem, sqldb *sql.DB) error {

	ss := ds.solarSystem
	if slim.PosState == nil || *slim.PosState != 0 || slim.PosTimestamp != 0 {
		if _, anchored, _ := evedb.GetStarbaseStructure(ctx, sqldb, itemID); anchored {
			return fmt.Errorf("it is already anchored")
		}
	}
	pos, ok := ss.GetStarbasePosition(uint32(itemID))
	if !ok {
		return fmt.Errorf("it has no ball here")
	}
	_, corpID, allianceID, err := evedb.GetCharSovContext(ctx, sqldb, charID)
	if err != nil {
		return err
	}
	if slim.CorpID != corpID && slim.OwnerID != charID {
		return fmt.Errorf("it belongs to another corporation")
	}
	// Security ceiling only where the type (or the server-wide override) declares
	// one; Mobile Storage ships without anchoringSecurityLevelMax.
	maxSec, hasCeiling := evedb.AnchoringSecurityLevelMax(ctx, sqldb, slim.TypeID)
	if ov, set := evedb.StarbaseSecurityLevelMaxOverride(ctx, sqldb); set {
		maxSec, hasCeiling = ov, true
	}
	if hasCeiling {
		if sec, secOK, err := evedb.GetSystemSecurity(ctx, sqldb, ds.sysID); err == nil && secOK && sec > maxSec {
			return fmt.Errorf("it cannot be anchored above %.1f security (this system is %.1f)", maxSec, sec)
		}
	}
	if err := evedb.AnchorStandaloneStructure(ctx, sqldb, itemID, ds.sysID, corpID, allianceID,
		pos.X, pos.Y, pos.Z); err != nil {
		return err
	}
	rec, found, err := evedb.GetStarbaseStructure(ctx, sqldb, itemID)
	if err != nil || !found {
		return fmt.Errorf("anchored but could not reload structure %d: %w", itemID, err)
	}
	ss.RemoveStarbase(uint32(itemID))
	bd, newSlim, dmg := starbaseBallSlimDmg(rec)
	ss.AddStarbase(bd, newSlim, dmg)
	broadcastBallSwap(ss, uint32(itemID), bd, newSlim, dmg)
	log.Printf("[%s] standalone structure %d (type %d) anchored by char %d (corp %d)",
		addr, itemID, rec.TypeID, charID, corpID)
	return nil
}

// unanchorStandaloneForPlayer starts the unanchoring countdown.
func unanchorStandaloneForPlayer(ctx context.Context, ds *destinyConn, addr string, charID, itemID int64, sqldb *sql.DB) error {
	rec, found, err := evedb.GetStarbaseStructure(ctx, sqldb, itemID)
	if err != nil {
		return err
	}
	if !found || rec.ControlTowerID != 0 {
		return fmt.Errorf("it is not anchored on its own")
	}
	_, corpID, allianceID, err := evedb.GetCharSovContext(ctx, sqldb, charID)
	if err != nil {
		return err
	}
	if !evedb.StarbaseManagePermitted(rec, corpID, allianceID) {
		return fmt.Errorf("it belongs to another corporation")
	}
	if rec.State == evedb.StarbaseStateUnanchoring {
		return fmt.Errorf("it is already unanchoring")
	}
	anchorMs := typeAttrMs(sqldb, rec.TypeID, "anchoringDelay")
	if !evedb.DelayElapsed(rec.AnchoredAt, anchorMs, server.Win32TimeNow()) {
		return fmt.Errorf("it has not finished anchoring yet")
	}
	if _, err := evedb.BeginStructureUnanchor(ctx, sqldb, itemID); err != nil {
		return err
	}
	msg := structurePushStateAndSchedule(addr, sqldb, globalSysMgr, itemID)
	scheduleStructureUnanchor(sqldb, itemID, false, time.Now().Add(
		time.Duration(typeAttrMs(sqldb, rec.TypeID, "unanchoringDelay"))*time.Millisecond))
	log.Printf("[%s] standalone structure %d unanchoring (char %d).%s", addr, itemID, charID, msg)
	return nil
}

// typeAttrMs reads a millisecond delay attribute off a type, 0 when absent.
func typeAttrMs(sqldb *sql.DB, typeID int64, name string) float64 {
	ctx, cancel := context.WithTimeout(context.Background(), starbaseCtxTimeout)
	defer cancel()
	v, ok, err := evedb.GetTypeAttributeFloat(ctx, sqldb, typeID, name)
	if err != nil || !ok {
		return 0
	}
	return v
}

// finishStructureUnanchor completes an unanchoring whose delay has run, a
// standalone's or a tower's slave: the overlay row goes and the item floats
// free, scoopable. Safe to call twice (timer and sweep may both arrive).
func finishStructureUnanchor(sqldb *sql.DB, entityID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), starbaseCtxTimeout)
	defer cancel()
	rec, found, err := evedb.GetStarbaseStructure(ctx, sqldb, entityID)
	if err != nil || !found || rec.State != evedb.StarbaseStateUnanchoring {
		return
	}
	if !evedb.DelayElapsed(rec.UnanchorAt, typeAttrMs(sqldb, rec.TypeID, "unanchoringDelay"), server.Win32TimeNow()) {
		return
	}
	if _, err := evedb.UnanchorStructure(ctx, sqldb, entityID); err != nil {
		log.Printf("[pos] structure %d finish unanchor: %v", entityID, err)
		return
	}
	if globalSysMgr != nil {
		if ss := globalSysMgr.Get(rec.SolarSystemID); ss != nil {
			starbaseSwapToFreeSettled(ss, rec)
		}
	}
	log.Printf("[pos] structure %d unanchored", entityID)
}

// sweepStructureUnanchoring finishes every unanchoring whose delay has run;
// called from the structure onlining sweep.
func sweepStructureUnanchoring(sqldb *sql.DB) {
	ctx, cancel := context.WithTimeout(context.Background(), starbaseCtxTimeout)
	recs, err := evedb.ListStarbaseStructuresByState(ctx, sqldb, evedb.StarbaseStateUnanchoring)
	cancel()
	if err != nil {
		log.Printf("[pos] structure unanchor sweep: %v", err)
		return
	}
	now := server.Win32TimeNow()
	for _, r := range recs {
		if evedb.DelayElapsed(r.UnanchorAt, typeAttrMs(sqldb, r.TypeID, "unanchoringDelay"), now) {
			finishStructureUnanchor(sqldb, r.EntityID)
		}
	}
}
