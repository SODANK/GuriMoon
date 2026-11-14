package main

import (
	"log"

	"gurimoon/gang"
	"gurimoon/marshal"
	_ "gurimoon/modules"
	"gurimoon/server"
)

// boundDrop handles the bound call "Drop".
func boundDrop(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, sqldb, ds := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.sqldb, cc.ds
	// ship.Drop(ids[, ownerID]) drops drones from the drone bay, or structures from the
	// cargo hold (Launch for Corporation).
	var dropIDs []int64
	if len(cs.ArgTuple.Items) >= 1 {
		dropIDs = parseDroneIDs(cs.ArgTuple.Items[0])
	}
	var dropOwnerID int64
	if len(cs.ArgTuple.Items) >= 2 {
		if v, ok := cs.ArgTuple.Items[1].(marshal.PyInt); ok {
			dropOwnerID = int64(v.Value)
		}
	}
	structIDs, droneIDs := splitStructureDropIDs(sqldb, ds.shipID, dropIDs)
	newIDs := launchStructuresForPlayer(ds, addr, charID, dropOwnerID, structIDs, sqldb)
	newIDs = append(newIDs, launchDronesForPlayer(ds, charID, droneIDs, sqldb)...)
	log.Printf("[%s] → ship.Drop %v → launched %v (%d structures)", addr, dropIDs, newIDs, len(structIDs))
	return server.SendCallRsp(c, pkt, accountID, pyIntList(newIDs))
}

// boundDock handles the bound call "Dock", "TurboDock".
func boundDock(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, stationID, sqldb, ds := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.stationID, cc.sqldb, cc.ds
	// TurboDock is the GM menu's Ctrl+Shift+Alt dock. The login role is not kept per
	// connection, so it is an ordinary request and does not skip clearance.
	if ds.entity != nil && ds.entity.IsDestroyed() {
		log.Printf("[%s] → destiny.Dock: BLOCKED — ship destroyed", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	// Crimewatch 1.0: a PvP act locks the aggressor out of docking for a minute
	// (pvptimers.go). A warp scrambler does not stop a dock. Checked again when
	// clearance comes through.
	if aggressionLocked(charID) {
		log.Printf("[%s] → destiny.Dock: BLOCKED — recent aggression", addr)
		return clientMsgErr(c, pkt, accountID, "CantDockAfterAggression", nil)
	}
	// A ship under a cloaking device cannot dock; the pilot decloaks first.
	if moduleCloakOn(ds) {
		log.Printf("[%s] → destiny.Dock: BLOCKED — cloaked", addr)
		return clientMsgErr(c, pkt, accountID, "CantDockWhileCloaked", nil)
	}
	// Dock is its own case here, so the siegeModeWarpStatus (852) check in
	// handleDestinyBoundCall does not cover it.
	if pinned, mod := shipImmobilised(ds); pinned {
		log.Printf("[%s] → destiny.Dock: BLOCKED — pinned by %s", addr, mod)
		return server.SendCallException(c, pkt, accountID, immobilisedError(mod, "dock"))
	}
	// Extract the target station from Dock args[0], fall back to connection station.
	dockStationID := *stationID
	if len(cs.ArgTuple.Items) >= 1 {
		if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok && v.Value != 0 {
			dockStationID = int64(v.Value)
		}
	}
	// A player owner's Docking rule and docking fee (docking.go dockGate); args[1] is the
	// fee the client agreed to pay on its retry.
	var paid float64
	if len(cs.ArgTuple.Items) >= 2 {
		paid = repFloat(cs.ArgTuple.Items[1])
	}
	fee, refused, err := dockGate(c, pkt, accountID, charID, sqldb, ds, dockStationID, paid)
	if refused || err != nil {
		log.Printf("[%s] → destiny.Dock station=%d: refused by the owner's rules", addr, dockStationID)
		return err
	}
	// A request, not the dock: answered now, docked once the station grants
	// clearance and the ship has burned into range (docking.go).
	return requestDock(c, addr, pkt, accountID, charID, stationID, sqldb, ds, dockStationID, fee)
}

// boundGangTagTarget handles the bound call "GangTagTarget".
func boundGangTagTarget(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID
	// Remote-park bound: overview Tag menu, GangTagTarget(itemID, tag) with tag
	// an int 0-9 or a letter string; None clears. The fresh state blob goes to every
	// online member.
	if globalGangReg == nil || charID == 0 || len(cs.ArgTuple.Items) < 2 {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	var tagItemID int64
	if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
		tagItemID = int64(v.Value)
	}
	var tag any
	switch v := cs.ArgTuple.Items[1].(type) {
	case marshal.PyInt:
		tag = int64(v.Value)
	case marshal.PyString:
		tag = v.Value
	}
	gangID := globalGangReg.SetTargetTag(charID, tagItemID, tag)
	if gangID == 0 {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	log.Printf("[%s] → bound.GangTagTarget gang=%d item=%d tag=%v", addr, gangID, tagItemID, tag)
	if g := globalGangReg.GetGang(gangID); g != nil {
		stateArgs := marshal.PyTuple{Items: []marshal.PyRep{
			buildGangStateRep(globalGangReg.TargetTagsFor(charID)),
		}}
		for memberID := range g.Members {
			if conn, acct := charConnLookup(memberID); conn != nil {
				_ = server.SendNotification(conn, acct, "OnGangStateChange", "charid", stateArgs)
			}
		}
	}
	if gang.ModuleTagged != nil {
		gang.ModuleTagged(gangID, charID, tagItemID, tag)
	}
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}
