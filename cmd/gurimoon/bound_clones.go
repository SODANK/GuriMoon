package main

import (
	"context"
	"errors"
	"log"
	"time"

	evedb "gurimoon/db"
	"gurimoon/marshal"
	_ "gurimoon/modules"
	"gurimoon/server"
)

// boundInstallCloneInStation handles the bound call "InstallCloneInStation".
func boundInstallCloneInStation(cc *callCtx) error {
	c, addr, pkt, accountID, charID, stationID, sqldb := cc.c, cc.addr, cc.pkt, cc.accountID, cc.charID, cc.stationID, cc.sqldb
	// jumpCloneSvc bound: installs a fresh clone at the current station.
	if sqldb == nil || charID == 0 || *stationID == 0 {
		return server.SendCallException(c, pkt, accountID, server.BuildUserError(
			"CustomInfo", map[string]marshal.PyRep{
				"info": marshal.PyString{Value: "You must be docked in a station to install a jump clone."},
			}))
	}
	{
		// Medical service, Infomorph cap, owner standing and the fee are ruled on in db
		// (JumpCloneInstallRefusal); a refusal carries the client's message key where one exists.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cloneID, balance, fee, err := evedb.InstallJumpClone(ctx, sqldb, charID, *stationID)
		if err != nil {
			log.Printf("[%s] bound.InstallCloneInStation charID=%d: %v", addr, charID, err)
			return refuseErr(c, pkt, accountID, err)
		}
		log.Printf("[%s] → bound.InstallCloneInStation charID=%d cloneID=%d station=%d fee=%.0f", addr, charID, cloneID, *stationID, fee)
		if err := server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}); err != nil {
			return err
		}
		if fee > 0 {
			pushAccountChange(c, accountID, charID, balance)
		}
	}
	sendJumpCloneInvalidatedAt(c, accountID, *stationID, charID)
	return nil
}

// boundDestroyInstalledClone handles the bound call "DestroyInstalledClone".
func boundDestroyInstalledClone(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, stationID, sqldb := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.stationID, cc.sqldb
	if sqldb == nil || charID == 0 || len(cs.ArgTuple.Items) < 1 {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	if idRep, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Only the clone at this station, as the client's own menus offer.
		// Undocked, *stationID is 0 and there is nothing local to destroy.
		if *stationID == 0 {
			return refuseErr(c, pkt, accountID, errors.New("You must be docked at the clone's station to destroy it."))
		}
		if err := evedb.DestroyJumpClone(ctx, sqldb, charID, int64(idRep.Value), *stationID); err != nil {
			log.Printf("[%s] bound.DestroyInstalledClone charID=%d: %v", addr, charID, err)
			return refuseErr(c, pkt, accountID, err)
		}
		log.Printf("[%s] → bound.DestroyInstalledClone charID=%d cloneID=%d", addr, charID, idRep.Value)
	}
	if err := server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}); err != nil {
		return err
	}
	sendJumpCloneInvalidatedAt(c, accountID, *stationID, charID)
	return nil
}

// boundSetCloneTypeID handles the bound call "SetCloneTypeID".
func boundSetCloneTypeID(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, stationID, sqldb := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.stationID, cc.sqldb
	// corpStationMgr bound: buy a better clone grade; arg 0 is the new typeID.
	// evedb.BuyCloneGrade refuses and charges in one transaction (docs/medical-clones.md).
	if len(cs.ArgTuple.Items) < 1 {
		log.Printf("[%s] → bound.SetCloneTypeID: missing typeID arg", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	newTypeID, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		log.Printf("[%s] → bound.SetCloneTypeID: arg not PyInt", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	if stationID == nil || *stationID == 0 {
		return clientMsgErr(c, pkt, accountID, "CustomInfo", map[string]marshal.PyRep{
			"info": marshal.PyString{Value: "You must be docked at a station with a medical bay to change your clone."},
		})
	}
	cloneCtx, cloneCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cloneCancel()
	if reason := evedb.StationServiceRefusal(cloneCtx, sqldb, *stationID, charID, evedb.StationServiceCloning); reason != "" {
		return corpUserErr(c, pkt, accountID, reason)
	}
	price, newBal, err := evedb.BuyCloneGrade(cloneCtx, sqldb, charID, int64(newTypeID.Value))
	if err != nil {
		log.Printf("[%s] → bound.SetCloneTypeID charID=%d newType=%d: %v", addr, charID, newTypeID.Value, err)
		return refuseErr(c, pkt, accountID, err)
	}
	if price > 0 {
		notifyArgs := marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyString{Value: "cash"},
			marshal.PyInt{Value: uint64(charID)},
			marshal.PyReal{Value: newBal},
		}}
		_ = server.SendNotification(c, accountID, "OnAccountChange", "charid", notifyArgs)
	}
	log.Printf("[%s] → bound.SetCloneTypeID charID=%d newType=%d cost=%.0f", addr, charID, newTypeID.Value, price)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}
