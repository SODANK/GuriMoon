package main

// Placing an item in an opened dungeon container, the source of the itemPlaced trigger event, and
// the completeDungeon action. See docs/cmd-misc-notes.md#dungeon-triggers.

import (
	"context"
	"database/sql"
	"log"
	"time"

	evedb "gurimoon/db"
	"gurimoon/dogma"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
)

// dunCargoDefaultCapacity is the hold of a container whose type carries no capacity. Some Spawn
// Container types carry 0, which would refuse every drop.
const dunCargoDefaultCapacity = 27500.0

// dunCargoCapacity answers GetCapacity for an opened dungeon container.
func dunCargoCapacity(ctx context.Context, sqldb *sql.DB, ds *destinyConn, contID int64) (capacity, used float64) {
	capacity = dunCargoDefaultCapacity
	if e := npcCargoEntity(ds, contID); e != nil {
		if v, err := evedb.GetModuleCapacity(ctx, sqldb, e.Slim.TypeID); err == nil && v > 0 {
			capacity = v
		}
	}
	used, _ = evedb.GetContainerUsedVolume(ctx, sqldb, contID)
	return capacity, used
}

// dunCargoWatched reports whether an itemPlaced trigger of the loaded room listens to the ball.
// A container with none stays take-only. A fired trigger still counts, so a drop made after the
// event is answered the same way as the one that set it off.
func dunCargoWatched(ds *destinyConn, objectID int64) bool {
	ds.dunTrigMu.Lock()
	defer ds.dunTrigMu.Unlock()
	for _, t := range ds.dunTriggers {
		if t.Event != "itemPlaced" {
			continue
		}
		if !t.TargetObjectID.Valid || t.TargetObjectID.Int64 == 0 || t.TargetObjectID.Int64 == objectID {
			return true
		}
	}
	return false
}

// handleDunCargoAdd moves one stack from the pilot's cargo hold into an opened dungeon container
// and raises itemPlaced. The container keeps the item, so the pilot can take it back out; it goes
// to the death can with the rest if the ball dies. refused means the call was answered with the
// refusal, otherwise the caller still owes the reply.
func handleDunCargoAdd(c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID, charID, contID, itemID, qty int64,
	sqldb *sql.DB, ds *destinyConn) (refused bool, err error) {

	refuse := func(msg string) (bool, error) {
		return true, corpUserErr(c, pkt, accountID, msg)
	}
	e := npcCargoEntity(ds, contID)
	if e == nil || sqldb == nil {
		return refuse("That container is gone.")
	}
	if !shipWithin(ds, e.Pos(), itemTransferRange+e.Manager.Radius()) {
		return refuse("You are too far away from that container.")
	}
	objectID := ds.dunObjSnapshot()[e.ID]
	if objectID == 0 || !dunCargoWatched(ds, objectID) {
		return refuse("This container does not accept items.")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	er, gerr := evedb.GetEntityRow(ctx, sqldb, itemID)
	if gerr != nil || er == nil {
		return refuse("That item no longer exists.")
	}
	shipID, _ := evedb.GetCurrentShipID(ctx, sqldb, charID)
	if shipID == 0 || er.LocationID != shipID || er.Flag != dogma.FlagCargo || er.OwnerID != charID {
		return refuse("Only items in your own cargo hold can go in here.")
	}
	if evedb.IsContainerGroup(er.GroupID) && er.Singleton != 0 {
		return refuse("An assembled container cannot be put inside another container.")
	}

	moveQty := er.Quantity
	if qty > 0 && qty < er.Quantity {
		moveQty = qty
	}
	capacity, used := dunCargoCapacity(ctx, sqldb, ds, contID)
	unitVol, _ := evedb.GetTypeVolume(ctx, sqldb, er.TypeID)
	if need := unitVol * float64(moveQty); used+need > capacity+1e-6 {
		return true, clientMsgErr(c, pkt, accountID, "NotEnoughCargoSpace", map[string]marshal.PyRep{
			"volume":    marshal.PyReal{Value: need},
			"available": marshal.PyReal{Value: capacity - used},
		})
	}
	moveID, merr := moveStackIntoContainer(ctx, c, accountID, sqldb, er, contID, moveQty, 0)
	if merr != nil {
		return refuse(merr.Error())
	}
	log.Printf("[%s] → dungeon container %d (object %d): item %d (type %d ×%d) placed by char %d",
		addr, contID, objectID, moveID, er.TypeID, moveQty, charID)

	held, herr := evedb.ContainerTypeQuantities(ctx, sqldb, contID)
	if herr != nil {
		log.Printf("[%s] dungeon container %d: %v", addr, contID, herr)
		return false, nil
	}
	fireDungeonTriggersIf(c, addr, accountID, ds, "itemPlaced", objectID, func(t evedb.TriggerRow) bool {
		return t.ItemPlacedMatches(er.TypeID, held)
	})
	return false, nil
}

// completeDungeonSite finishes the connection's site: a mission's objective is met and an open site
// starts its clear countdown. Rats still alive stay where they are. A room loaded by neither (an
// ESP test spawn) has nothing to finish.
func completeDungeonSite(c *evenet.Conn, accountID int64, ds *destinyConn) error {
	if s := openSiteByID(ds.dunOpenSiteID); s != nil {
		completeOpenSite(s)
		return nil
	}
	if ds.dunMissionID <= 0 || globalDB == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m, err := evedb.GetMissionByID(ctx, globalDB, ds.dunMissionID)
	if err != nil || m == nil || m.State != "accepted" {
		return err
	}
	changed, err := evedb.CompleteKillObjective(ctx, globalDB, ds.dunMissionID)
	if err != nil {
		return err
	}
	if changed {
		log.Printf("[mission] missionID=%d completeDungeon trigger → objective complete (charID=%d)", m.MissionID, m.CharID)
		notifyAgentMissionChange(c, accountID, "progress", m.AgentID)
	}
	return nil
}

// sendNoneAfterDunCargoAdd answers bound.Add once handleDunCargoAdd has done the work.
func sendNoneAfterDunCargoAdd(c *evenet.Conn, pkt *evenet.PyPacket, accountID int64, refused bool, err error) error {
	if refused {
		return err
	}
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}
