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

// boundCharStartTrainingSkill handles the bound call "CharStartTrainingSkill".
func boundCharStartTrainingSkill(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, sqldb := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.sqldb
	// skillMgr bound: called when the player right-clicks a skill and chooses "Train Skill".
	// Arg[0] is the skill item ID.
	if len(cs.ArgTuple.Items) < 1 {
		log.Printf("[%s] bound.CharStartTrainingSkill: missing args", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	skillIDRep, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		log.Printf("[%s] bound.CharStartTrainingSkill: arg[0] not int: %T", addr, cs.ArgTuple.Items[0])
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	skillItemID := int64(skillIDRep.Value)
	log.Printf("[%s] → bound.CharStartTrainingSkill charID=%d skillItemID=%d", addr, charID, skillItemID)
	oldLoc, oldFlag := snapshotSkillItemState(sqldb, skillItemID)
	var endOfTraining uint64
	if sqldb != nil && charID != 0 {
		prevItemID := trainingSkillItem(sqldb, charID)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var err error
		endOfTraining, err = evedb.StartTrainingSkill(ctx, sqldb, charID, skillItemID)
		if err != nil {
			log.Printf("[%s] bound.CharStartTrainingSkill refused: %v", addr, err)
			return server.SendCallException(c, pkt, accountID, skillTrainErrorPayload(err))
		}
		notifySkillSwitchedOff(c, accountID, prevItemID, skillItemID, sqldb)
	} else {
		endOfTraining = server.Win32TimeNow() + 25920000000000
	}
	notifyArgs := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(skillItemID)},
		marshal.PyInt{Value: endOfTraining},
	}}
	if err := server.SendNotification(c, accountID, "OnSkillStartTraining", "charid", notifyArgs); err != nil {
		log.Printf("[%s] bound.CharStartTrainingSkill notify: %v", addr, err)
	}
	armSkillCompletionTimer(c, accountID, charID, endOfTraining, sqldb)
	sendSkillFlagChange(c, accountID, skillItemID, oldFlag, oldLoc, sqldb)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: endOfTraining})
}

// boundCharStopTrainingSkill handles the bound call "CharStopTrainingSkill".
func boundCharStopTrainingSkill(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, sqldb := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.sqldb
	// skillMgr bound: client "Abort Training" menu. Banks partial SP, resets
	// the skill flag, and notifies the client to clear the training state.
	var stoppedItemID int64
	if sqldb != nil && charID != 0 {
		skReq := int64(0)
		if len(cs.ArgTuple.Items) >= 1 {
			if r, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
				skReq = int64(r.Value)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		var err error
		stoppedItemID, err = evedb.StopTrainingSkill(ctx, sqldb, charID, skReq)
		cancel()
		if err != nil {
			log.Printf("[%s] bound.CharStopTrainingSkill db: %v", addr, err)
		}
	}
	cancelSkillCompletionTimer(c, charID)
	if stoppedItemID != 0 {
		notifyArgs := marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(stoppedItemID)},
			marshal.PyInt{Value: 0}, // silent=0: the client shows "training stopped"
		}}
		if err := server.SendNotification(c, accountID, "OnSkillTrainingStopped", "charid", notifyArgs); err != nil {
			log.Printf("[%s] bound.CharStopTrainingSkill notify: %v", addr, err)
		}
		log.Printf("[%s] → bound.CharStopTrainingSkill charID=%d skill %d stopped", addr, charID, stoppedItemID)
		sendSkillFlagChange(c, accountID, stoppedItemID, 61, 0, sqldb)
	}
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// boundGetEndOfTraining handles the bound call "GetEndOfTraining".
func boundGetEndOfTraining(cc *callCtx) error {
	c, addr, pkt, accountID, charID, sqldb := cc.c, cc.addr, cc.pkt, cc.accountID, cc.charID, cc.sqldb
	// skillMgr bound: returns Win32 timestamp when the current training skill completes.
	var endOfTraining uint64
	if sqldb != nil && charID != 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if st, err := evedb.GetSkillTraining(ctx, sqldb, charID); err == nil && st != nil {
			endOfTraining = st.EndOfTraining
		}
	}
	if endOfTraining == 0 {
		endOfTraining = server.Win32TimeNow() + 25920000000000
	} else {
		// A training is active: (re)arm the completion timer so it fires after a server restart,
		// once the client opens the character sheet.
		armSkillCompletionTimer(c, accountID, charID, endOfTraining, sqldb)
	}
	log.Printf("[%s] → bound.GetEndOfTraining endOfTraining=%d", addr, endOfTraining)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: endOfTraining})
}
