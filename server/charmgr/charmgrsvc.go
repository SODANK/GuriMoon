// Package charmgr implements the "charmgr" service.
package charmgr

import (
	"context"
	"database/sql"
	"log"
	"time"

	evedb "gurimoon/db"
	"gurimoon/logx"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
	"gurimoon/server/infocache"
)

// Handle dispatches a CallReq addressed to the "charmgr" service. isOnline reports
// whether a character has a live connection; nil makes bounty listings report
// everyone offline.
func Handle(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB, isOnline func(charID int64) bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	switch cs.Method {
	case "GetPublicInfo":
		return handleGetPublicInfo(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "GetPublicInfo3":
		return handleGetPublicInfo3(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "AddToBounty":
		return handleAddToBounty(ctx, c, addr, cs, pkt, accountID, charID, sqldb)
	case "GetTopBounties":
		return handleGetTopBounties(ctx, c, addr, pkt, accountID, sqldb, isOnline)
	// The next two are called only by the optional bountyportraits client
	// patch (portraitjobs.go); the shipped client never sends them.
	case "GetPortraitJobs":
		return handleGetPortraitJobs(ctx, c, addr, pkt, accountID, charID, sqldb)
	case "SubmitPortrait":
		return handleSubmitPortrait(ctx, c, addr, cs, pkt, accountID, charID)
	default:
		server.RecordUnhandled(addr, "charmgr", cs, false)
		log.Printf("[%s] charmgr: unhandled method %q — sending exception", addr, cs.Method)
		return server.SendCallException(c, pkt, accountID, nil)
	}
}

// handleGetPublicInfo returns a util.KeyVal with public character information.
// Arg[0] is the character ID (or corp ID).
func handleGetPublicInfo(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	if len(cs.ArgTuple.Items) < 1 {
		log.Printf("[%s] charmgr.GetPublicInfo: missing arg", addr)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	id, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		log.Printf("[%s] charmgr.GetPublicInfo: arg[0] is %T, want PyInt", addr, cs.ArgTuple.Items[0])
		return server.SendCallException(c, pkt, accountID, nil)
	}
	// Memoized: the client asks this once per pilot it can see, serially on
	// this connection's goroutine. See package infocache.
	result, err := infocache.Shared.Get("charmgr.GetPublicInfo", int64(id.Value), infocache.TTLIdentity,
		func() (marshal.PyRep, error) {
			return evedb.GetCharPublicInfo(ctx, sqldb, int64(id.Value))
		})
	if err != nil {
		log.Printf("[%s] charmgr.GetPublicInfo db: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	logx.Debugf("[%s] → charmgr.GetPublicInfo charID=%d", addr, id.Value)
	return server.SendCallRsp(c, pkt, accountID, result)
}

// handleGetPublicInfo3 returns a util.Rowset with extended public character info.
// Arg[0] is the character ID.
func handleGetPublicInfo3(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	if len(cs.ArgTuple.Items) < 1 {
		log.Printf("[%s] charmgr.GetPublicInfo3: missing arg", addr)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	id, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		log.Printf("[%s] charmgr.GetPublicInfo3: arg[0] is %T, want PyInt", addr, cs.ArgTuple.Items[0])
		return server.SendCallException(c, pkt, accountID, nil)
	}
	result, err := infocache.Shared.Get("charmgr.GetPublicInfo3", int64(id.Value), infocache.TTLIdentity,
		func() (marshal.PyRep, error) {
			return evedb.GetCharPublicInfo3(ctx, sqldb, int64(id.Value))
		})
	if err != nil {
		log.Printf("[%s] charmgr.GetPublicInfo3 db: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	logx.Debugf("[%s] → charmgr.GetPublicInfo3 charID=%d", addr, id.Value)
	return server.SendCallRsp(c, pkt, accountID, result)
}

// handleAddToBounty places a bounty on a target character.
// Arg[0] = targetCharID (int), Arg[1] = amount (int, ISK).
func handleAddToBounty(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB) error {
	if len(cs.ArgTuple.Items) < 2 {
		log.Printf("[%s] charmgr.AddToBounty: need 2 args, got %d", addr, len(cs.ArgTuple.Items))
		return server.SendCallException(c, pkt, accountID, nil)
	}
	targetID, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		log.Printf("[%s] charmgr.AddToBounty: arg[0] is %T, want PyInt", addr, cs.ArgTuple.Items[0])
		return server.SendCallException(c, pkt, accountID, nil)
	}
	amountArg, ok := cs.ArgTuple.Items[1].(marshal.PyInt)
	if !ok {
		log.Printf("[%s] charmgr.AddToBounty: arg[1] is %T, want PyInt", addr, cs.ArgTuple.Items[1])
		return server.SendCallException(c, pkt, accountID, nil)
	}
	amount := float64(amountArg.Value)
	if amount <= 0 {
		log.Printf("[%s] charmgr.AddToBounty: invalid amount %.0f", addr, amount)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	if int64(targetID.Value) == charID {
		log.Printf("[%s] charmgr.AddToBounty: cannot bounty self", addr)
		return server.SendCallException(c, pkt, accountID, nil)
	}

	err := evedb.AddToBounty(ctx, sqldb, charID, int64(targetID.Value), amount)
	if err != nil {
		log.Printf("[%s] charmgr.AddToBounty db: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	log.Printf("[%s] → charmgr.AddToBounty caller=%d target=%d amount=%.0f", addr, charID, targetID.Value, amount)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// handleGetTopBounties returns the top evedb.TopBountiesLimit characters with bounties.
// No args.
func handleGetTopBounties(ctx context.Context, c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB, isOnline func(charID int64) bool) error {
	result, err := evedb.GetTopBounties(ctx, sqldb, isOnline)
	if err != nil {
		log.Printf("[%s] charmgr.GetTopBounties db: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	// Log row count for debugging billboard display.
	count := 0
	if rs, ok := result.(marshal.PyObject); ok {
		for _, e := range rs.Args.(marshal.PyDict).Items {
			if k, ok := e.Key.(marshal.PyString); ok && k.Value == "lines" {
				if lines, ok := e.Value.(marshal.PyList); ok {
					count = len(lines.Items)
				}
			}
		}
	}
	log.Printf("[%s] → charmgr.GetTopBounties rows=%d", addr, count)
	return server.SendCallRsp(c, pkt, accountID, result)
}
