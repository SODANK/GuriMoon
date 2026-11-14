package main

// Refusals in the client's own words: raise a key from config.BulkData.messages
// (sql/evemessages.sql) with clientMsgErr as a call's answer, or pushRemoteMessage after the
// answer has gone. See docs/cmd-misc-notes.md#client-messages.

import (
	"context"
	"database/sql"
	"fmt"

	evedb "gurimoon/db"
	"gurimoon/dogma"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
)

// clientMsgErr answers a call with one of the client's message keys. dict must carry every
// %(name)s the text uses.
func clientMsgErr(c *evenet.Conn, pkt *evenet.PyPacket, accountID int64, key string, dict map[string]marshal.PyRep) error {
	return server.SendCallException(c, pkt, accountID, server.BuildUserError(key, dict))
}

// refuseErr answers a call with a refusal from db: the client's own message
// when the error carries a key (evedb.MsgErr), a CustomError box with the
// error's text otherwise.
func refuseErr(c *evenet.Conn, pkt *evenet.PyPacket, accountID int64, err error) error {
	if m, ok := evedb.AsClientMsg(err); ok {
		return server.SendCallException(c, pkt, accountID, server.BuildUserError(m.Key, m.Dict))
	}
	return corpUserErr(c, pkt, accountID, err.Error())
}

// pushRemoteMessage shows one of the client's message keys without a call to answer.
// OnRemoteMessage is eve.Message on the client: a notify key fades in, an infomodal key opens its box.
func pushRemoteMessage(c *evenet.Conn, accountID int64, key string, dict map[string]marshal.PyRep) {
	if c == nil {
		return
	}
	var d marshal.PyRep = marshal.PyDict{}
	if len(dict) > 0 {
		items := make([]marshal.DictEntry, 0, len(dict))
		for k, v := range dict {
			items = append(items, marshal.DictEntry{Key: marshal.PyString{Value: k}, Value: v})
		}
		d = marshal.PyDict{Items: items}
	}
	_ = server.SendNotification(c, accountID, "OnRemoteMessage", "charid", marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: key}, d,
	}})
}

// lockTargetLabel names a lock target for the targeting messages: its group
// ("Frigate", "Asteroid Belt") for TargetNotWithinRange, which reads "The
// %(targetGroupName)s is too far away", and its type name for the rest.
// Entities answer from their slim item; anything else from its entity row.
func lockTargetLabel(ctx context.Context, sqldb *sql.DB, ds *destinyConn, tid uint32) (group, name string) {
	var typeID int64
	if ds != nil && ds.solarSystem != nil {
		if e := ds.solarSystem.GetEntity(tid); e != nil {
			typeID = e.Slim.TypeID
		}
	}
	if typeID == 0 && sqldb != nil {
		if er, err := evedb.GetEntityRow(ctx, sqldb, int64(tid)); err == nil && er != nil {
			typeID = er.TypeID
		}
	}
	group, name = "target", "the target"
	if typeID == 0 || sqldb == nil {
		return
	}
	name = typeNameOrID(ctx, sqldb, typeID)
	_ = sqldb.QueryRowContext(ctx, `SELECT g.groupName FROM invTypes t
		JOIN invGroups g ON g.groupID = t.groupID WHERE t.typeID = ?`, typeID).Scan(&group)
	return
}

// onlineShortfall answers the client message for a module whose CPU or power need does not fit the
// ship, or "" when it fits. needCPU must carry the superweapon factor (dogma.SuperWeaponCPUUsage),
// as the ship's load does. CPU is reported first when both are short.
func onlineShortfall(modName string, needCPU, needPG, curCPU, curPG, cpuCap, pgCap float64) (string, map[string]marshal.PyRep) {
	args := func(require, used, total float64) map[string]marshal.PyRep {
		remaining := total - used
		if remaining < 0 {
			remaining = 0
		}
		return map[string]marshal.PyRep{
			"moduleName": marshal.PyString{Value: modName},
			"require":    marshal.PyReal{Value: require},
			"remaining":  marshal.PyReal{Value: remaining},
			"total":      marshal.PyReal{Value: total},
		}
	}
	switch {
	case curCPU+needCPU > cpuCap:
		return "NotEnoughCpu", args(needCPU, curCPU, cpuCap)
	case needPG > pgCap:
		// More than the whole power core: the client's "blow your fuse" box.
		return "NotEnoughPowerOutput", args(needPG, curPG, pgCap)
	case curPG+needPG > pgCap:
		return "NotEnoughPower", args(needPG, curPG, pgCap)
	}
	return "", nil
}

// fitOnlineRefusal decides whether a module just fitted may come online and, when not, the message
// saying why: skills, then the one-per-group limit, then CPU and power. The Basic Miner exemption
// matches missingFitSkills. See docs/cmd-misc-notes.md#fit-online-refusal.
func fitOnlineRefusal(ctx context.Context, sqldb *sql.DB, charID, shipID, itemID, typeID, groupID int64,
	cpuCap, pgCap float64) (string, map[string]marshal.PyRep) {
	modName := typeNameOrID(ctx, sqldb, typeID)
	if missing := missingFitSkills(ctx, sqldb, charID, typeID); missing != "" {
		return "FittingHasSkillPrerequisites", map[string]marshal.PyRep{
			"requiredSkills": marshal.PyString{Value: missing},
		}
	}
	attrs, _ := evedb.GetTypeAttributes(ctx, sqldb, typeID)
	if maxOnline := int(attrs[dogma.AttrMaxGroupOnline]); maxOnline > 0 && groupID != 0 {
		if n, err := evedb.CountOnlineModulesInGroup(ctx, sqldb, shipID, groupID, itemID); err == nil && n >= maxOnline {
			return "CustomNotify", map[string]marshal.PyRep{
				"notify": marshal.PyString{Value: fmt.Sprintf(
					"%s stays offline: you can only have %d module of this type online at a time.", modName, maxOnline)},
			}
		}
	}
	curCPU, curPG, _ := evedb.GetShipCPUPowerLoad(ctx, sqldb, shipID)
	// The hull's role bonuses price the module, as they price the load.
	var shipTypeID int64
	if shipER, err := evedb.GetEntityRow(ctx, sqldb, shipID); err == nil && shipER != nil {
		shipTypeID = shipER.TypeID
	}
	needCPU, needPG, _ := evedb.GetModuleFittingNeed(ctx, sqldb, charID, shipTypeID, typeID)
	return onlineShortfall(modName, needCPU, needPG, curCPU, curPG, cpuCap, pgCap)
}
