package main

import (
	"context"
	"log"
	"time"

	evedb "gurimoon/db"
	"gurimoon/marshal"
	_ "gurimoon/modules"
	"gurimoon/server"
)

// boundDoStandingCheckForStationService handles the bound call "DoStandingCheckForStationService".
func boundDoStandingCheckForStationService(cc *callCtx) error {
	c, cs, pkt, accountID, charID, stationID, sqldb := cc.c, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.stationID, cc.sqldb
	// corpStationMgr bound: the lobby checks this before opening any station
	// service and caches the answer for 5 minutes; None permits. Access rules apply
	// only at player-owned stations with station.enforceManagement on.
	if sqldb != nil && stationID != nil && len(cs.ArgTuple.Items) >= 1 {
		if svc, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
			scCtx, scCancel := context.WithTimeout(context.Background(), 5*time.Second)
			msg := stationServiceStandingCheck(scCtx, sqldb, *stationID, charID, int64(svc.Value))
			scCancel()
			if msg != "" {
				return server.SendCallException(c, pkt, accountID, server.BuildUserError(
					"CustomInfo", map[string]marshal.PyRep{"info": marshal.PyString{Value: msg}}))
			}
		}
	}
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// boundGetPotentialHomeStations handles the bound call "GetPotentialHomeStations".
func boundGetPotentialHomeStations(cc *callCtx) error {
	c, addr, pkt, accountID, stationID, sqldb := cc.c, cc.addr, cc.pkt, cc.accountID, cc.stationID, cc.sqldb
	// corpStationMgr bound: stations where the player can set home; only the
	// current station is offered.
	log.Printf("[%s] → bound.GetPotentialHomeStations stationID=%d", addr, *stationID)
	var rows [][]marshal.PyRep
	if *stationID != 0 && sqldb != nil {
		hsCtx, hsCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer hsCancel()
		stnTypeID, err := evedb.GetStationTypeID(hsCtx, sqldb, int64(*stationID))
		if err != nil {
			log.Printf("[%s] → bound.GetPotentialHomeStations: station type error: %v", addr, err)
		} else {
			rows = append(rows, []marshal.PyRep{
				marshal.PyInt{Value: uint64(*stationID)},
				marshal.PyInt{Value: uint64(stnTypeID)},
			})
		}
	}
	return server.SendCallRsp(c, pkt, accountID, makeInvRowset([]string{"stationID", "typeID"}, rows))
}

// boundSetHomeStation handles the bound call "SetHomeStation".
func boundSetHomeStation(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, stationID, sqldb := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.stationID, cc.sqldb
	// corpStationMgr bound: a new medical clone station; arg 0 is the target stationID.
	newHomeID := *stationID // default: the current station
	if len(cs.ArgTuple.Items) >= 1 {
		if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok && v.Value != 0 {
			newHomeID = int64(v.Value)
		}
	}
	hsCtx, hsCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer hsCancel()
	// A player owner's Cloning rule and clone contract fee (db/stationfeedb.go).
	if cur, _ := evedb.GetHomeStationID(hsCtx, sqldb, charID); cur == newHomeID {
		return clientMsgErr(c, pkt, accountID, "MedicalYouAlreadyHaveACloneContractAtThatStation", nil)
	}
	if reason := evedb.StationServiceRefusal(hsCtx, sqldb, newHomeID, charID, evedb.StationServiceCloning); reason != "" {
		return corpUserErr(c, pkt, accountID, reason)
	}
	if fee := evedb.CloneContractFee(hsCtx, sqldb, newHomeID, charID); fee > 0 {
		if _, err := evedb.ChargeStationFee(hsCtx, sqldb, charID, newHomeID, fee, evedb.RefCloneTransfer, "Clone contract"); err != nil {
			log.Printf("[%s] → bound.SetHomeStation charID=%d stationID=%d fee %.0f: %v", addr, charID, newHomeID, fee, err)
			return refuseErr(c, pkt, accountID, err)
		}
	}
	if err := evedb.SetHomeStationID(hsCtx, sqldb, charID, newHomeID); err != nil {
		log.Printf("[%s] → bound.SetHomeStation charID=%d stationID=%d ERROR: %v", addr, charID, newHomeID, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	log.Printf("[%s] → bound.SetHomeStation charID=%d → stationID=%d", addr, charID, newHomeID)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}
