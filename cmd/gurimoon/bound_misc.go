package main

import (
	"context"
	"log"
	"time"

	evedb "gurimoon/db"
	"gurimoon/logx"
	"gurimoon/marshal"
	_ "gurimoon/modules"
	"gurimoon/server"
	"gurimoon/server/infocache"
)

// boundGetInfoWindowDataForChar handles the bound call "GetInfoWindowDataForChar".
func boundGetInfoWindowDataForChar(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, sqldb := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.sqldb
	// charMgr bound: return corpID, allianceID, title for the character info card.
	// Arg[0] is the characterID to look up.
	targetID := charID
	if len(cs.ArgTuple.Items) >= 1 {
		if id, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
			targetID = int64(id.Value)
		}
	}
	if targetID != 0 && sqldb != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result, err := infocache.Shared.Get("bound.GetInfoWindowDataForChar", targetID, infocache.TTLIdentity,
			func() (marshal.PyRep, error) {
				return evedb.GetInfoWindowDataForChar(ctx, sqldb, targetID)
			})
		if err != nil {
			log.Printf("[%s] bound.GetInfoWindowDataForChar(%d): %v", addr, targetID, err)
		} else {
			logx.Debugf("[%s] → bound.GetInfoWindowDataForChar charID=%d", addr, targetID)
			return server.SendCallRsp(c, pkt, accountID, result)
		}
	}
	log.Printf("[%s] → bound.GetInfoWindowDataForChar (None)", addr)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// boundDoAction handles the bound call "DoAction".
func boundDoAction(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, stationID, agentBinds, sqldb := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.stationID, cc.agentBinds, cc.sqldb
	// Agent bound: DoAction(actionID). agents.py __GetConversation unpacks
	// ((agentSays, dialog), xtraInfo). actionID is a string when it came from the
	// conversation's name-entry form.
	actionID := 0
	actionText := ""
	if len(cs.ArgTuple.Items) >= 1 {
		switch v := cs.ArgTuple.Items[0].(type) {
		case marshal.PyInt:
			actionID = int(v.Value)
		case marshal.PyString:
			actionText = v.Value
		}
	}
	agentID := agentBinds.resolve(cs)
	stnID := int64(0)
	if stationID != nil {
		stnID = *stationID
	}
	log.Printf("[%s] → bound.DoAction agentID=%d actionID=%d text=%q charID=%d", addr, agentID, actionID, actionText, charID)
	return server.SendCallRsp(c, pkt, accountID, handleAgentDoAction(c, accountID, addr, agentID, charID, stnID, actionID, actionText, sqldb))
}

// boundGetCourierOptions handles the bound call "GetCourierOptions".
func boundGetCourierOptions(cc *callCtx) error {
	c, addr, pkt, accountID, charID, stationID, sqldb := cc.c, cc.addr, cc.pkt, cc.accountID, cc.charID, cc.stationID, cc.sqldb
	// Courier mission creator bound object. The client normally inlines this into
	// the bind (computeInlineBindResult); a cached moniker calls it here and still
	// needs .basicFee/.itemList. The bind params are gone, so use the docked station.
	sid := int64(0)
	if stationID != nil {
		sid = *stationID
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, err := evedb.GetCourierOptions(ctx, sqldb, charID, sid)
	if err != nil {
		log.Printf("[%s] bound.GetCourierOptions charID=%d stationID=%d: %v", addr, charID, sid, err)
	} else {
		log.Printf("[%s] → bound.GetCourierOptions charID=%d stationID=%d", addr, charID, sid)
	}
	return server.SendCallRsp(c, pkt, accountID, r)
}

// boundCreateCourierMission handles the bound call "CreateCourierMission".
func boundCreateCourierMission(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, stationID, sqldb := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.stationID, cc.sqldb
	sid := int64(0)
	if stationID != nil {
		sid = *stationID
	}
	ctxCr, cancelCr := context.WithTimeout(context.Background(), 15*time.Second)
	r, cerr := createCourierMission(ctxCr, addr, c, pkt, accountID, charID, sid, cs, sqldb)
	cancelCr()
	if cerr != nil {
		log.Printf("[%s] bound.CreateCourierMission charID=%d: %v", addr, charID, cerr)
		return server.SendCallException(c, pkt, accountID, server.BuildUserError(
			"CustomError", map[string]marshal.PyRep{
				"error": marshal.PyString{Value: cerr.Error()},
			}))
	}
	return server.SendCallRsp(c, pkt, accountID, r)
}
