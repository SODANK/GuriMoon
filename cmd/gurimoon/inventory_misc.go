package main

// Small bound calls the client makes:
//
//   - ListDroneBay: ui/inflight/drone.py and parklife/tactical.py ask the ship bind for the bay.
//     The drone window normally reads the ship's List (flags 5 + 87); this is the explicit answer.
//   - RemoveChargeToCargo / RemoveChargeToHangar: invCache.MakeDBLessItemReal. Every loaded charge
//     is a real row here, so these should not arrive; the log line is the finding.
//   - GotoBookmark: People & Places Approach on a bookmark, a sub-warp flight. Warp To goes through
//     WarpToStuff('bookmark').

import (
	"context"
	"database/sql"
	"log"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
)

func handleListDroneBay(c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB, ds *destinyConn) error {
	if sqldb == nil || charID == 0 {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	shipID := ds.shipID
	if shipID == 0 {
		shipID, _ = evedb.GetCurrentShipID(ctx, sqldb, charID)
	}
	if shipID == 0 {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyList{})
	}
	r, err := evedb.GetContainerItemsFlag(ctx, sqldb, shipID, 87)
	if err != nil {
		log.Printf("[%s] ListDroneBay ship=%d: %v", addr, shipID, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyList{})
	}
	log.Printf("[%s] → bound.ListDroneBay ship=%d", addr, shipID)
	return server.SendCallRsp(c, pkt, accountID, r)
}

func handleRemoveChargeTo(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64) error {
	// Logged because a DB-less charge should not exist here. None is the answer: the client has no
	// itemID to work with.
	log.Printf("[%s] bound.%s reached — client holds a DB-less charge (charID=%d args=%+v); "+
		"this server reports loaded charges as real rows, so this path should be dead",
		addr, cs.Method, charID, cs.ArgTuple.Items)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// bookmarkApproachPoint resolves a bookmark the caller owns, in the caller's system, to a point:
// the celestial it marks, else its stored coordinates. Same rules as WarpToStuff's bookmark arm
// minus the mission-bookmark band (an Approach on a mission site is a warp).
func bookmarkApproachPoint(ds *destinyConn, sqldb *sql.DB, bookmarkID int64) (destiny.Vec3, bool) {
	if sqldb == nil || bookmarkID < evedb.BookmarkIDFloor {
		return destiny.Vec3{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bm, err := evedb.GetBookmark(ctx, sqldb, bookmarkID)
	if err != nil || bm == nil || bm.OwnerID != ds.charID {
		return destiny.Vec3{}, false
	}
	if bm.LocationID != 0 && bm.LocationID != ds.sysID {
		return destiny.Vec3{}, false
	}
	if bm.ItemID != 0 {
		for _, cel := range ds.celestials {
			if cel.ItemID == bm.ItemID {
				return destiny.Vec3{X: cel.X, Y: cel.Y, Z: cel.Z}, true
			}
		}
	}
	if bm.X != 0 || bm.Y != 0 || bm.Z != 0 {
		return destiny.Vec3{X: bm.X, Y: bm.Y, Z: bm.Z}, true
	}
	return destiny.Vec3{}, false
}

// handleGotoBookmark flies the ship to the bookmark at full speed. The
// server sim uses the same goto as GotoDirection (navGoto converges on the
// target); the client is told GotoPoint, the dll's own decelerate-at-point
// mode, so the flight ends where it should on both sides.
func handleGotoBookmark(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB, ds *destinyConn) error {
	dm := ds.dm
	var bmID int64
	if len(cs.ArgTuple.Items) > 0 {
		if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
			bmID = int64(v.Value)
		}
	}
	target, ok := bookmarkApproachPoint(ds, sqldb, bmID)
	if !ok {
		log.Printf("[%s] → destiny.GotoBookmark %d: not resolvable for char %d in system %d", addr, bmID, ds.charID, ds.sysID)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	breakGateCloak(ds)
	abortPendingWarp(ds)
	dm.GotoDirection(target)
	ds.syncStamp()
	log.Printf("[%s] → destiny.GotoBookmark %d entityID=%d target=(%.0f,%.0f,%.0f)",
		addr, bmID, dm.EntityID, target.X, target.Y, target.Z)
	if err := server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}); err != nil {
		return err
	}
	actions := []marshal.PyRep{
		destiny.BuildSimpleAction("SetSpeedFraction",
			marshal.PyInt{Value: uint64(dm.EntityID)},
			marshal.PyReal{Value: dm.SpeedFraction()}),
		destiny.BuildSimpleAction("GotoPoint",
			marshal.PyInt{Value: uint64(dm.EntityID)},
			marshal.PyReal{Value: target.X}, marshal.PyReal{Value: target.Y}, marshal.PyReal{Value: target.Z}),
	}
	if err := dm.SendStampedUrgent(func(stamp uint32) error {
		return destiny.SendActionList(c, globalNodeID, uint64(accountID), stamp, actions...)
	}); err != nil {
		return err
	}
	broadcastActionList(ds, actions...)
	return nil
}
