// Package trademgr implements the "trademgr" service and the bound trade session
// behind the player-to-player trade window (ui/station/pvptrade/*).
//
// InitiateTrade returns a bound object whose bind ID is the trade container's item
// ID. The client drives the trade through List, Add, OfferMoney, ToggleAccept,
// IsCEOTrade and Abort on it. Progress is pushed to both clients as OnTrade, whose
// first argument (Cancel, StateToggle, MoneyOffer, TradeComplete) svc.pvptrade
// dispatches on.
package trademgr

import (
	"context"
	"database/sql"
	"fmt"
	"log"

	evedb "gurimoon/db"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
	"gurimoon/trade"
)

// ConnLookup resolves a character ID to its live connection and that character's
// own accountID, or (nil, 0). Cross-player notifications carry the recipient's
// accountID as the packet userID.
type ConnLookup func(int64) (*evenet.Conn, int64)

// tradeItemCols is the row shape the trade offer views read.
var tradeItemCols = []string{
	"itemID", "typeID", "ownerID", "locationID", "flag",
	"quantity", "groupID", "categoryID", "singleton",
}

// Handle dispatches a CallReq addressed to the "trademgr" service.
func Handle(
	c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket,
	accountID, charID, stationID int64,
	reg *trade.Registry, connLookup ConnLookup, sqldb *sql.DB,
) error {
	if reg == nil {
		log.Printf("[%s] → trademgr.%s (no registry, stub None)", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}

	switch cs.Method {
	case "InitiateTrade":
		return handleInitiateTrade(c, addr, cs, pkt, accountID, charID, stationID, reg, connLookup, sqldb)
	default:
		server.RecordUnhandled(addr, "trademgr", cs, true)
		log.Printf("[%s] trademgr: unhandled method %q — returning None", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
}

// handleInitiateTrade opens a session with the target character and returns
// the bound session object.  Both traders must be docked in the same station.
func handleInitiateTrade(
	c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket,
	accountID, charID, stationID int64,
	reg *trade.Registry, connLookup ConnLookup, sqldb *sql.DB,
) error {
	ctx := context.Background()
	targetCharID := argInt(cs, 0)
	ceoTrade := len(cs.ArgTuple.Items) > 1 && argInt(cs, 1) != 0

	if targetCharID == 0 || targetCharID == charID {
		return userError(c, pkt, accountID, "There is nobody to trade with.")
	}
	if stationID == 0 {
		return userError(c, pkt, accountID, "You must be docked to trade.")
	}

	// Rejoining an existing session must return the same container, otherwise
	// the client opens a second, unrelated trade window.
	if existing := reg.GetByChar(charID); existing != nil {
		if existing.Other(charID) == targetCharID {
			log.Printf("[%s] → trademgr.InitiateTrade: rejoining container %d", addr, existing.ContainerID)
			return server.SendCallRsp(c, pkt, accountID, bindSpec(existing.ContainerID))
		}
		return server.SendCallException(c, pkt, accountID, server.BuildUserError(
			"ItemExchangeAlreadyTrading", map[string]marshal.PyRep{
				"who": marshal.PyString{Value: evedb.GetCharacterName(ctx, sqldb, existing.Other(charID))},
			}))
	}
	if reg.GetByChar(targetCharID) != nil {
		return userError(c, pkt, accountID, "That character is already trading with somebody else.")
	}

	targetStation, err := evedb.GetCharStationID(ctx, sqldb, targetCharID)
	if err != nil {
		log.Printf("[%s] trademgr.InitiateTrade station lookup: %v", addr, err)
		return userError(c, pkt, accountID, "That character cannot be reached.")
	}
	if targetStation != stationID {
		return userError(c, pkt, accountID, "You must both be docked in the same station to trade.")
	}

	containerID, err := evedb.CreateTradeContainer(ctx, sqldb, stationID)
	if err != nil {
		log.Printf("[%s] trademgr.InitiateTrade container: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}

	session, err := reg.Open(containerID, stationID, charID, targetCharID, ceoTrade)
	if err != nil {
		log.Printf("[%s] trademgr.InitiateTrade: %v", addr, err)
		evedb.ReturnTradeItems(ctx, sqldb, containerID, stationID)
		return userError(c, pkt, accountID, "That trade could not be opened.")
	}

	// Pop the window on the other client too: svc.pvptrade.OnInitiate binds
	// its own session object from the container ID.
	if conn, acct := connLookup(targetCharID); conn != nil {
		notify(conn, acct, "Initiate", marshal.PyInt{Value: uint64(charID)}, bindSpec(containerID))
	}

	log.Printf("[%s] → trademgr.InitiateTrade: char %d ↔ %d in container %d", addr, charID, targetCharID, containerID)
	_ = session
	return server.SendCallRsp(c, pkt, accountID, bindSpec(containerID))
}

// HandleBound dispatches a bound call on a trade session.  containerID is the
// bind ID the client called through.  Returns handled=false when the method is
// not part of the trade-session API, so the caller can fall through to its
// other bound-object handlers.
func HandleBound(
	c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket,
	accountID, charID, containerID int64,
	reg *trade.Registry, connLookup ConnLookup, sqldb *sql.DB,
) (handled bool, err error) {
	session := reg.Get(containerID)
	if session == nil || session.Index(charID) < 0 {
		return false, nil
	}
	ctx := context.Background()

	switch cs.Method {
	case "List":
		items, err := evedb.GetTradeItems(ctx, sqldb, containerID)
		if err != nil {
			log.Printf("[%s] trade.List(%d): %v", addr, containerID, err)
		}
		log.Printf("[%s] → trade.List(%d): %d items", addr, containerID, len(items))
		return true, server.SendCallRsp(c, pkt, accountID, sessionData(session, items))

	case "Add":
		itemID := argInt(cs, 0)
		quantity := argInt(cs, 1)
		if err := evedb.EscrowTradeItem(ctx, sqldb, charID, session.StationID, containerID, itemID, quantity); err != nil {
			log.Printf("[%s] trade.Add(%d): %v", addr, itemID, err)
			return true, userError(c, pkt, accountID, "That item cannot be put up for trade.")
		}
		// Changing the goods invalidates any prior agreement.
		if updated := reg.ClearAccepts(containerID); updated != nil {
			broadcastState(updated, connLookup)
		}
		log.Printf("[%s] → trade.Add: item %d ×%d into container %d", addr, itemID, quantity, containerID)
		return true, server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})

	case "OfferMoney":
		amount := argFloat(cs, 0)
		if amount < 0 {
			amount = 0
		}
		updated := reg.SetMoney(containerID, charID, amount)
		if updated == nil {
			return true, server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		broadcastMoney(updated, connLookup)
		broadcastState(updated, connLookup)
		log.Printf("[%s] → trade.OfferMoney: char %d offers %.2f", addr, charID, amount)
		return true, server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})

	case "ToggleAccept":
		accepted := argInt(cs, 0) != 0
		updated := reg.SetAccepted(containerID, charID, accepted)
		if updated == nil {
			return true, server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		broadcastState(updated, connLookup)

		if updated.BothAccepted() {
			return true, completeTrade(ctx, c, addr, pkt, accountID, updated, reg, connLookup, sqldb)
		}
		log.Printf("[%s] → trade.ToggleAccept: char %d accepted=%v", addr, charID, accepted)
		return true, server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})

	case "IsCEOTrade":
		return true, server.SendCallRsp(c, pkt, accountID, boolRep(session.CEOTrade))

	case "Abort", "Cancel":
		closed := reg.Close(containerID)
		if closed != nil {
			if err := evedb.ReturnTradeItems(ctx, sqldb, containerID, closed.StationID); err != nil {
				log.Printf("[%s] trade.Abort return items: %v", addr, err)
			}
			broadcast(closed, connLookup, "Cancel", marshal.PyInt{Value: uint64(containerID)})
		}
		log.Printf("[%s] → trade.Abort: container %d cancelled", addr, containerID)
		return true, server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})

	case "GetCapacity":
		// The trade container has no volume limit in this era; report a large
		// capacity so the client never refuses a drop on capacity grounds.
		return true, server.SendCallRsp(c, pkt, accountID, marshal.PyObject{
			Type: "util.KeyVal",
			Args: marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyString{Value: "capacity"}, Value: marshal.PyReal{Value: 1e9}},
				{Key: marshal.PyString{Value: "used"}, Value: marshal.PyReal{Value: 0}},
			}},
		})
	}

	return false, nil
}

// AbortForCharacter tears down any session a character is in.  Called when
// they disconnect or undock so escrowed items are never stranded.
func AbortForCharacter(ctx context.Context, charID int64, reg *trade.Registry, connLookup ConnLookup, sqldb *sql.DB) {
	if reg == nil {
		return
	}
	session := reg.GetByChar(charID)
	if session == nil {
		return
	}
	closed := reg.Close(session.ContainerID)
	if closed == nil {
		return
	}
	if err := evedb.ReturnTradeItems(ctx, sqldb, closed.ContainerID, closed.StationID); err != nil {
		log.Printf("trade: returning items for aborted container %d: %v", closed.ContainerID, err)
	}
	broadcast(closed, connLookup, "Cancel", marshal.PyInt{Value: uint64(closed.ContainerID)})
	log.Printf("trade: container %d aborted (char %d left)", closed.ContainerID, charID)
}

// completeTrade settles an agreed trade and tells both clients.
func completeTrade(
	ctx context.Context, c *evenet.Conn, addr string, pkt *evenet.PyPacket,
	accountID int64, session *trade.Session,
	reg *trade.Registry, connLookup ConnLookup, sqldb *sql.DB,
) error {
	err := evedb.SettleTrade(ctx, sqldb,
		session.ContainerID, session.StationID,
		session.Traders[0], session.Traders[1],
		session.Money[0], session.Money[1])
	if err != nil {
		log.Printf("[%s] trade.Complete(%d): %v", addr, session.ContainerID, err)
		// Leave the session open so the traders can adjust and retry.
		if updated := reg.ClearAccepts(session.ContainerID); updated != nil {
			broadcastState(updated, connLookup)
		}
		return userError(c, pkt, accountID, "The trade could not be completed — check that both sides can cover their ISK offer.")
	}

	closed := reg.Close(session.ContainerID)
	if closed == nil {
		closed = session
	}
	broadcast(closed, connLookup, "TradeComplete", marshal.PyInt{Value: uint64(closed.ContainerID)})
	// TradeComplete moves the items but carries no balance, so both wallets are pushed
	// here; the neocom wealth tooltip only advances on OnAccountChange.
	pushTradeBalances(ctx, closed, connLookup, sqldb)

	log.Printf("[%s] → trade.Complete: container %d settled (%.2f ⇄ %.2f ISK)",
		addr, closed.ContainerID, closed.Money[0], closed.Money[1])
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// pushTradeBalances repaints both traders' wallets after a settle. The trade
// service has no balance of its own, so each is read back from the row that
// SettleTrade just committed.
func pushTradeBalances(ctx context.Context, s *trade.Session, connLookup ConnLookup, sqldb *sql.DB) {
	if s == nil || sqldb == nil || connLookup == nil {
		return
	}
	for _, charID := range s.Traders {
		if charID == 0 {
			continue
		}
		bal, err := evedb.GetCharacterBalance(ctx, sqldb, charID)
		if err != nil {
			log.Printf("trade: balance push char %d: %v", charID, err)
			continue
		}
		if conn, acctID := connLookup(charID); conn != nil {
			server.PushCashBalance(conn, acctID, charID, bal)
		}
	}
}

// ─── result shaping ────────────────────────────────────────────────────────

// sessionData builds the KeyVal pvptradewnd.Startup unpacks.  traders must
// keep a stable order: the client uses .index(charid) to work out which side
// of the window is its own.
func sessionData(s *trade.Session, items []evedb.TradeItem) marshal.PyRep {
	lines := make([]marshal.PyRep, 0, len(items))
	for _, it := range items {
		lines = append(lines, marshal.PyList{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(it.ItemID)},
			marshal.PyInt{Value: uint64(it.TypeID)},
			marshal.PyInt{Value: uint64(it.OwnerID)},
			marshal.PyInt{Value: uint64(it.LocationID)},
			marshal.PyInt{Value: uint64(it.Flag)},
			marshal.PyInt{Value: uint64(it.Quantity)},
			marshal.PyInt{Value: uint64(it.GroupID)},
			marshal.PyInt{Value: uint64(it.CategoryID)},
			marshal.PyInt{Value: uint64(it.Singleton)},
		}})
	}

	return marshal.PyObject{
		Type: "util.KeyVal",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "tradeContainerID"}, Value: marshal.PyInt{Value: uint64(s.ContainerID)}},
			{Key: marshal.PyString{Value: "traders"}, Value: marshal.PyList{Items: []marshal.PyRep{
				marshal.PyInt{Value: uint64(s.Traders[0])},
				marshal.PyInt{Value: uint64(s.Traders[1])},
			}}},
			{Key: marshal.PyString{Value: "items"}, Value: rowset(tradeItemCols, lines)},
			{Key: marshal.PyString{Value: "money"}, Value: moneyList(s)},
			{Key: marshal.PyString{Value: "state"}, Value: stateList(s)},
		}},
	}
}

func moneyList(s *trade.Session) marshal.PyRep {
	return marshal.PyList{Items: []marshal.PyRep{
		marshal.PyReal{Value: s.Money[0]},
		marshal.PyReal{Value: s.Money[1]},
	}}
}

func stateList(s *trade.Session) marshal.PyRep {
	return marshal.PyList{Items: []marshal.PyRep{
		boolRep(s.Accepted[0]),
		boolRep(s.Accepted[1]),
	}}
}

func boolRep(v bool) marshal.PyRep {
	if v {
		return marshal.PyInt{Value: 1}
	}
	return marshal.PyInt{Value: 0}
}

func rowset(cols []string, lines []marshal.PyRep) marshal.PyRep {
	header := make([]marshal.PyRep, len(cols))
	for i, col := range cols {
		header[i] = marshal.PyString{Value: col}
	}
	headerList := marshal.PyList{Items: header}
	return marshal.PyObject{
		Type: "util.Rowset",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "header"}, Value: headerList},
			{Key: marshal.PyString{Value: "RowClass"}, Value: marshal.PyString{Value: "util.Row", IsType1: true}},
			{Key: marshal.PyString{Value: "lines"}, Value: marshal.PyList{Items: lines}},
			{Key: marshal.PyString{Value: "columns"}, Value: headerList},
		}},
	}
}

// bindSpec returns the bound-object reference the client turns into a remote
// proxy.  The bind ID is the trade container so later calls identify the
// session without any extra lookup table.
func bindSpec(containerID int64) marshal.PyRep {
	return marshal.PySubStruct{Value: &marshal.PySubStream{
		Decoded: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyString{Value: fmt.Sprintf("N=%d:%d", server.NodeID, containerID)},
			marshal.PyInt{Value: 0},
		}},
	}}
}

// ─── notifications ─────────────────────────────────────────────────────────

// notify sends one OnTrade event.  svc.pvptrade.OnTrade dispatches on the
// first argument, calling On<what>(*rest).
func notify(c *evenet.Conn, accountID int64, what string, args ...marshal.PyRep) {
	if c == nil {
		return
	}
	items := make([]marshal.PyRep, 0, len(args)+1)
	items = append(items, marshal.PyString{Value: what})
	items = append(items, args...)
	if err := server.SendNotification(c, accountID, "OnTrade", "charid",
		marshal.PyTuple{Items: items}); err != nil {
		log.Printf("trade: OnTrade %s: %v", what, err)
	}
}

func broadcast(s *trade.Session, connLookup ConnLookup, what string, args ...marshal.PyRep) {
	for _, charID := range s.Traders {
		conn, acct := connLookup(charID)
		notify(conn, acct, what, args...)
	}
}

func broadcastState(s *trade.Session, connLookup ConnLookup) {
	broadcast(s, connLookup, "StateToggle",
		marshal.PyInt{Value: uint64(s.ContainerID)}, stateList(s))
}

func broadcastMoney(s *trade.Session, connLookup ConnLookup) {
	broadcast(s, connLookup, "MoneyOffer",
		marshal.PyInt{Value: uint64(s.ContainerID)}, moneyList(s))
}

// ─── helpers ───────────────────────────────────────────────────────────────

func userError(c *evenet.Conn, pkt *evenet.PyPacket, accountID int64, msg string) error {
	return server.SendCallException(c, pkt, accountID, server.BuildUserError(
		"CustomInfo", map[string]marshal.PyRep{
			"info": marshal.PyString{Value: msg},
		}))
}

func argAt(cs *server.CallStream, idx int) marshal.PyRep {
	if idx >= len(cs.ArgTuple.Items) {
		return nil
	}
	return cs.ArgTuple.Items[idx]
}

func argInt(cs *server.CallStream, idx int) int64 {
	if i, ok := argAt(cs, idx).(marshal.PyInt); ok {
		return int64(i.Value)
	}
	return 0
}

func argFloat(cs *server.CallStream, idx int) float64 {
	switch n := argAt(cs, idx).(type) {
	case marshal.PyReal:
		return n.Value
	case marshal.PyInt:
		return float64(n.Value)
	}
	return 0
}
