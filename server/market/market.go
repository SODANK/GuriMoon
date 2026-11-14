// Package market implements the "marketProxy" service: browsing orders, price
// history, placing, modifying and cancelling orders, and the pilot's open orders.
// The Seeder NPC (charID 90000001) holds sell orders for nearly every published item.
package market

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	evecache "gurimoon/cache"
	evedb "gurimoon/db"
	"gurimoon/esp"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
	"gurimoon/syncx"
)

// Handle dispatches a CallReq addressed to the "marketProxy" service.
//
// solarSystemID is the system the caller is flying in; it is what scopes the
// region-wide market calls when the caller is undocked (stationID == 0).
func Handle(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID, stationID, solarSystemID int64, sqldb *sql.DB, sessReg *esp.SessionRegistry) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	switch cs.Method {
	case "GetStationAsks":
		return handleGetStationAsks(ctx, c, addr, cs, pkt, accountID, stationID, sqldb)
	case "GetSystemAsks":
		return handleGetSystemAsks(ctx, c, addr, cs, pkt, accountID, stationID, solarSystemID, sqldb)
	case "GetRegionBest":
		return handleGetRegionBest(ctx, c, addr, cs, pkt, accountID, stationID, solarSystemID, sqldb)
	case "GetMarketGroups":
		return handleGetMarketGroups(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "GetOrders":
		return handleGetOrders(ctx, c, addr, cs, pkt, accountID, stationID, solarSystemID, sqldb)
	case "GetOldPriceHistory":
		return handleGetOldPriceHistory(ctx, c, addr, cs, pkt, accountID, stationID, solarSystemID, sqldb)
	case "GetNewPriceHistory":
		return handleGetNewPriceHistory(ctx, c, addr, cs, pkt, accountID, stationID, solarSystemID, sqldb)
	case "GetCharOrders":
		return handleGetCharOrders(ctx, c, addr, cs, pkt, accountID, charID, sqldb)
	case "GetCorporationOrders":
		return handleGetCorporationOrders(ctx, c, addr, cs, pkt, accountID, charID, sqldb)
	case "PlaceCharOrder":
		return handlePlaceCharOrder(ctx, c, addr, cs, pkt, accountID, charID, stationID, solarSystemID, sqldb, sessReg)
	case "CancelCharOrder":
		return handleCancelCharOrder(ctx, c, addr, cs, pkt, accountID, charID, stationID, solarSystemID, sqldb, sessReg)
	case "ModifyCharOrder":
		return handleModifyCharOrder(ctx, c, addr, cs, pkt, accountID, charID, stationID, solarSystemID, sqldb, sessReg)
	case "CharGetNewTransactions":
		// wallet.py ShowTransactions fetches the full transaction rowset here.
		r, err := evedb.GetCharTransactions(ctx, sqldb, charID)
		if err != nil {
			log.Printf("[%s] marketProxy.CharGetNewTransactions db: %v (empty)", addr, err)
			r = evedb.EmptyTransactionRowset()
		}
		log.Printf("[%s] → marketProxy.CharGetNewTransactions charID=%d", addr, charID)
		return server.SendCallRsp(c, pkt, accountID, r)
	case "CharGetTransactions":
		// ShowTransactions(browse=1) pages back: arg 0 is the oldest transactionID the window holds.
		var before int64
		if len(cs.ArgTuple.Items) > 0 {
			if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
				before = int64(v.Value)
			}
		}
		r, err := evedb.GetCharTransactionsBefore(ctx, sqldb, charID, before)
		if err != nil {
			log.Printf("[%s] marketProxy.CharGetTransactions db: %v (empty)", addr, err)
			r = evedb.EmptyTransactionRowset()
		}
		log.Printf("[%s] → marketProxy.CharGetTransactions charID=%d before=%d", addr, charID, before)
		return server.SendCallRsp(c, pkt, accountID, r)
	default:
		server.RecordUnhandled(addr, "marketProxy", cs, true)
		log.Printf("[%s] marketProxy: unhandled method %q — returning None", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
}

// marketParents resolves the location hierarchy the market view is scoped to.
// An undocked caller has no stationID, so the region comes from the solar system;
// the region-wide calls fail on station 0 otherwise.
func marketParents(ctx context.Context, sqldb *sql.DB, stationID, solarSystemID int64) (*evedb.StationParents, error) {
	if stationID != 0 {
		return evedb.GetStationParents(ctx, sqldb, stationID)
	}
	if solarSystemID == 0 {
		return nil, fmt.Errorf("marketParents: session has neither stationID nor solarSystemID")
	}
	constellationID, regionID, err := evedb.GetSystemParents(ctx, sqldb, solarSystemID)
	if err != nil {
		return nil, err
	}
	return &evedb.StationParents{
		SolarSystemID:   solarSystemID,
		ConstellationID: constellationID,
		RegionID:        regionID,
	}, nil
}

// ── browse-list caching & the "please wait" notice ────────────────────────────

// sendCachedRowset answers a browse call with an objectCaching.CachedMethodCallResult
// keyed on the named session field. The group tree build calls these once per node
// and the client does not memo them; marketsvc.py invalidates on own-order changes.
func sendCachedRowset(c *evenet.Conn, pkt *evenet.PyPacket, accountID int64, addr, method, sessionInfo string, result marshal.PyRep) error {
	rsp, err := evecache.MethodCallValue(result, "1 minute", sessionInfo)
	if err != nil {
		log.Printf("[%s] marketProxy.%s cache wrap: %v (sending bare)", addr, method, err)
		rsp = result
	}
	return server.SendCallRsp(c, pkt, accountID, rsp)
}

// lastMarketWaitNotice debounces the "receiving SCC market data" notice per accountID.
var lastMarketWaitNotice syncx.Map[int64, time.Time] // accountID(int64) to time.Time

// notifyMarketWait pushes a CustomNotify line into the client's notification log
// (gameui.OnRemoteMessage) while a browse query runs. The text stays ASCII: a PyString
// goes out as raw bytes and the client reads them as cp1252.
func notifyMarketWait(c *evenet.Conn, accountID int64) {
	if t, ok := lastMarketWaitNotice.Load(accountID); ok {
		if time.Since(t) < 15*time.Second {
			return
		}
	}
	lastMarketWaitNotice.Store(accountID, time.Now())
	_ = server.SendNotification(c, accountID, "OnRemoteMessage", "clientID",
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyString{Value: "CustomNotify"},
			marshal.PyDict{Items: []marshal.DictEntry{{
				Key:   marshal.PyString{Value: "notify"},
				Value: marshal.PyString{Value: "Requesting market data from the Secure Commerce Commission - please wait..."},
			}}},
		}})
}

// ── GetStationAsks ─────────────────────────────────────────────────────────────

// handleGetStationAsks returns a util.IndexRowset of sell-order summaries for
// the current station, mirroring MarketProxyService::Handle_GetStationAsks.
func handleGetStationAsks(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, stationID int64, sqldb *sql.DB) error {
	notifyMarketWait(c, accountID)
	result, err := evedb.GetStationAsks(ctx, sqldb, stationID)
	if err != nil {
		log.Printf("[%s] marketProxy.GetStationAsks db: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	log.Printf("[%s] → marketProxy.GetStationAsks stationID=%d", addr, stationID)
	return sendCachedRowset(c, pkt, accountID, addr, "GetStationAsks", "stationid", result)
}

// ── GetSystemAsks ─────────────────────────────────────────────────────────────

// handleGetSystemAsks returns a util.IndexRowset of sell-order summaries for
// the current solar system, mirroring MarketProxyService::Handle_GetSystemAsks.
func handleGetSystemAsks(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, stationID, solarSystemID int64, sqldb *sql.DB) error {
	notifyMarketWait(c, accountID)
	parents, err := marketParents(ctx, sqldb, stationID, solarSystemID)
	if err != nil {
		log.Printf("[%s] marketProxy.GetSystemAsks: lookup parents: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	result, err := evedb.GetSystemAsks(ctx, sqldb, parents.SolarSystemID)
	if err != nil {
		log.Printf("[%s] marketProxy.GetSystemAsks db: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	log.Printf("[%s] → marketProxy.GetSystemAsks solarSystemID=%d", addr, parents.SolarSystemID)
	return sendCachedRowset(c, pkt, accountID, addr, "GetSystemAsks", "solarsystemid", result)
}

// ── GetRegionBest ─────────────────────────────────────────────────────────────

// handleGetRegionBest returns a util.IndexRowset of the best sell-order prices
// per typeID in the current region, mirroring MarketProxyService::Handle_GetRegionBest.
func handleGetRegionBest(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, stationID, solarSystemID int64, sqldb *sql.DB) error {
	notifyMarketWait(c, accountID)
	parents, err := marketParents(ctx, sqldb, stationID, solarSystemID)
	if err != nil {
		log.Printf("[%s] marketProxy.GetRegionBest: lookup parents: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	result, err := evedb.GetRegionBest(ctx, sqldb, parents.RegionID)
	if err != nil {
		log.Printf("[%s] marketProxy.GetRegionBest db: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	log.Printf("[%s] → marketProxy.GetRegionBest regionID=%d", addr, parents.RegionID)
	return sendCachedRowset(c, pkt, accountID, addr, "GetRegionBest", "regionid", result)
}

// ── GetMarketGroups ───────────────────────────────────────────────────────────

// handleGetMarketGroups returns the market group tree as a util.FilterRowset,
// mirroring MarketProxyService::Handle_GetMarketGroups.
func handleGetMarketGroups(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	result, err := evedb.GetMarketGroups(ctx, sqldb)
	if err != nil {
		log.Printf("[%s] marketProxy.GetMarketGroups db: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	log.Printf("[%s] → marketProxy.GetMarketGroups", addr)
	return server.SendCallRsp(c, pkt, accountID, result)
}

// ── GetOrders ─────────────────────────────────────────────────────────────────

// handleGetOrders returns a PyTuple(2) of [sellOrders, buyOrders] util.Rowsets
// for the requested typeID in the current region.
func handleGetOrders(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, stationID, solarSystemID int64, sqldb *sql.DB) error {
	if len(cs.ArgTuple.Items) < 1 {
		log.Printf("[%s] marketProxy.GetOrders: missing typeID arg", addr)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	typeIDRep, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		log.Printf("[%s] marketProxy.GetOrders: arg[0] is %T, want PyInt", addr, cs.ArgTuple.Items[0])
		return server.SendCallException(c, pkt, accountID, nil)
	}
	typeID := int64(typeIDRep.Value)

	parents, err := marketParents(ctx, sqldb, stationID, solarSystemID)
	if err != nil {
		log.Printf("[%s] marketProxy.GetOrders: lookup parents: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}

	result, err := evedb.GetMarketOrders(ctx, sqldb, parents.RegionID, typeID)
	if err != nil {
		log.Printf("[%s] marketProxy.GetOrders db: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	// Answered as a versioned cached call: marketsvc replaces its orderCache only when
	// the version objectCaching reports changes.
	rsp, err := evecache.MethodCallValue(result, "always", "regionid")
	if err != nil {
		log.Printf("[%s] marketProxy.GetOrders cache wrap: %v", addr, err)
		rsp = result
	}
	log.Printf("[%s] → marketProxy.GetOrders typeID=%d regionID=%d", addr, typeID, parents.RegionID)
	return server.SendCallRsp(c, pkt, accountID, rsp)
}

// ── GetOldPriceHistory ────────────────────────────────────────────────────────

func handleGetOldPriceHistory(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, stationID, solarSystemID int64, sqldb *sql.DB) error {
	typeID, err := firstIntArg(cs, "GetOldPriceHistory")
	if err != nil {
		log.Printf("[%s] marketProxy.GetOldPriceHistory: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	parents, err := marketParents(ctx, sqldb, stationID, solarSystemID)
	if err != nil {
		log.Printf("[%s] marketProxy.GetOldPriceHistory: parents: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	result, err := evedb.GetOldPriceHistory(ctx, sqldb, parents.RegionID, typeID)
	if err != nil {
		log.Printf("[%s] marketProxy.GetOldPriceHistory db: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	log.Printf("[%s] → marketProxy.GetOldPriceHistory typeID=%d", addr, typeID)
	return server.SendCallRsp(c, pkt, accountID, result)
}

// ── GetNewPriceHistory ────────────────────────────────────────────────────────

func handleGetNewPriceHistory(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, stationID, solarSystemID int64, sqldb *sql.DB) error {
	typeID, err := firstIntArg(cs, "GetNewPriceHistory")
	if err != nil {
		log.Printf("[%s] marketProxy.GetNewPriceHistory: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	parents, err := marketParents(ctx, sqldb, stationID, solarSystemID)
	if err != nil {
		log.Printf("[%s] marketProxy.GetNewPriceHistory: parents: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	result, err := evedb.GetNewPriceHistory(ctx, sqldb, parents.RegionID, typeID)
	if err != nil {
		log.Printf("[%s] marketProxy.GetNewPriceHistory db: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	log.Printf("[%s] → marketProxy.GetNewPriceHistory typeID=%d", addr, typeID)
	return server.SendCallRsp(c, pkt, accountID, result)
}

// ── GetCharOrders ─────────────────────────────────────────────────────────────

func handleGetCharOrders(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB) error {
	result, err := evedb.GetCharOrders(ctx, sqldb, charID)
	if err != nil {
		log.Printf("[%s] marketProxy.GetCharOrders db: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	log.Printf("[%s] → marketProxy.GetCharOrders charID=%d", addr, charID)
	return server.SendCallRsp(c, pkt, accountID, result)
}

// ── PlaceCharOrder ────────────────────────────────────────────────────────────

// Call_PlaceCharOrder arguments: [0] stationID, [1] typeID, [2] price, [3] quantity,
// [4] bid (true=buy), [5] orderRange, [6] itemID (sell only), [7] minVolume,
// [8] duration in days, [9] useCorp, [10] located.
func handlePlaceCharOrder(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID, dockedStationID, solarSystemID int64, sqldb *sql.DB, sessReg *esp.SessionRegistry) error {
	args := cs.ArgTuple.Items
	if len(args) < 9 {
		log.Printf("[%s] marketProxy.PlaceCharOrder: too few args (%d)", addr, len(args))
		return server.SendCallException(c, pkt, accountID, nil)
	}

	stationID, err := pickInt(args, 0, "stationID")
	if err != nil {
		return sendMarketErr(c, pkt, accountID, addr, err)
	}
	typeID, err := pickInt(args, 1, "typeID")
	if err != nil {
		return sendMarketErr(c, pkt, accountID, addr, err)
	}
	price, err := pickFloat(args, 2, "price")
	if err != nil {
		return sendMarketErr(c, pkt, accountID, addr, err)
	}
	quantity, err := pickInt(args, 3, "quantity")
	if err != nil {
		return sendMarketErr(c, pkt, accountID, addr, err)
	}
	bid, err := pickBool(args, 4, "bid")
	if err != nil {
		return sendMarketErr(c, pkt, accountID, addr, err)
	}
	orderRange, _ := pickInt(args, 5, "orderRange")
	minVolume, _ := pickInt(args, 7, "minVolume")
	duration, _ := pickInt(args, 8, "duration")

	useCorp, _ := pickBool(args, 9, "useCorp")

	var outcome *evedb.PlaceOrderOutcome
	if bid {
		outcome, err = evedb.PlaceBuyOrder(ctx, sqldb, charID, stationID, typeID, price, quantity, orderRange, minVolume, duration, useCorp)
	} else {
		itemID, _ := pickInt(args, 6, "itemID")
		outcome, err = evedb.PlaceSellOrder(ctx, sqldb, charID, stationID, typeID, itemID, price, quantity, orderRange, minVolume, duration, useCorp)
	}
	if err != nil {
		log.Printf("[%s] marketProxy.PlaceCharOrder bid=%v typeID=%d qty=%d: %v", addr, bid, typeID, quantity, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	if outcome.ErrorMsg != "" {
		log.Printf("[%s] marketProxy.PlaceCharOrder refused: %s", addr, outcome.ErrorMsg)
		if outcome.ErrorKey != "" {
			return server.SendCallException(c, pkt, accountID, server.BuildUserError(outcome.ErrorKey, outcome.ErrorArgs))
		}
		return server.SendCallException(c, pkt, accountID, server.BuildUserError(
			"CustomInfo", map[string]marshal.PyRep{
				"info": marshal.PyString{Value: outcome.ErrorMsg},
			}))
	}
	log.Printf("[%s] → marketProxy.PlaceCharOrder bid=%v typeID=%d qty=%d price=%.2f range=%d duration=%d stationID=%d filled=%d restingOrderID=%d",
		addr, bid, typeID, quantity, price, orderRange, duration, stationID, outcome.FilledQty, outcome.RestingOrderID)
	// Goods land at the counterparty's station, which may differ from the buyer's;
	// log remote deliveries.
	for _, fill := range outcome.Fills {
		if bid && dockedStationID != 0 && fill.StationID != dockedStationID {
			log.Printf("[%s]   PlaceCharOrder: %d x typeID=%d delivered to station %d (buyer docked at %d)",
				addr, fill.Quantity, typeID, fill.StationID, dockedStationID)
		}
	}

	if outcome.BalanceChanged {
		sendBalanceUpdate(c, addr, accountID, charID, outcome.NewBalance)
	}
	// A sell empties the seller's hangar stacks and returns the unsold remainder;
	// the client must be told or it refuses the next sell with "0 units available".
	sendHangarDeltas(c, addr, accountID, outcome.HangarDeltas)
	for _, fill := range outcome.Fills {
		// Tell whichever side gained the goods; a merge into an existing stack is a quantity change.
		if bid {
			sendHangarDeltas(c, addr, accountID, fill.ItemDeltas)
		} else {
			notifyHangarDeltas(sessReg, fill.CounterpartyID, fill.ItemDeltas)
		}
		// Refresh the counterparty's own-orders view and wallet.
		if !fill.IsNPCOrder {
			notifyOwnOrderChanged(sessReg, fill.CounterpartyID, "Fulfilled", typeID)
			// OnOwnOrderChanged carries no balance, so the neocom balance needs OnAccountChange.
			if fill.CounterpartyBalanceChanged {
				server.NotifyCashBalance(sessReg, fill.CounterpartyID,
					fill.CounterpartyWalletOwnerID, fill.CounterpartyBalance)
			}
		}
	}
	// Refresh the caller's own market windows.
	sendOwnOrderChanged(c, addr, accountID, "New", typeID)
	// The region of the order's own station, not the caller's.
	if parents, err := marketParents(ctx, sqldb, stationID, solarSystemID); err == nil {
		broadcastOrderChange(ctx, sqldb, sessReg, addr, parents.RegionID, typeID)
	}
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// ── CancelCharOrder ───────────────────────────────────────────────────────────

// Client call: CancelCharOrder(orderID, regionID).
func handleCancelCharOrder(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID, stationID, solarSystemID int64, sqldb *sql.DB, sessReg *esp.SessionRegistry) error {
	orderID, err := pickInt(cs.ArgTuple.Items, 0, "orderID")
	if err != nil {
		return sendMarketErr(c, pkt, accountID, addr, err)
	}
	out, err := evedb.CancelMarketOrder(ctx, sqldb, charID, orderID)
	if err != nil {
		log.Printf("[%s] marketProxy.CancelCharOrder %d: %v", addr, orderID, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	if out.ErrorMsg != "" {
		return server.SendCallException(c, pkt, accountID, server.BuildUserError(
			"CustomInfo", map[string]marshal.PyRep{
				"info": marshal.PyString{Value: out.ErrorMsg},
			}))
	}
	log.Printf("[%s] → marketProxy.CancelCharOrder orderID=%d bid=%v", addr, orderID, out.Bid)
	if out.BalanceChanged {
		sendBalanceUpdate(c, addr, accountID, charID, out.NewBalance)
	}
	// Cancelling an ask returns the unsold goods; the client is told as on the sell path.
	sendHangarDeltas(c, addr, accountID, out.HangarDeltas)
	sendOwnOrderChanged(c, addr, accountID, "Canceled", out.TypeID)
	if parents, err := marketParents(ctx, sqldb, stationID, solarSystemID); err == nil {
		broadcastOrderChange(ctx, sqldb, sessReg, addr, parents.RegionID, out.TypeID)
	}
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// ── ModifyCharOrder ───────────────────────────────────────────────────────────

// Client call: ModifyCharOrder(orderID, newPrice, bid, stationID,
// solarSystemID, price, volRemaining, issued). Only the first two are read.
func handleModifyCharOrder(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID, stationID, solarSystemID int64, sqldb *sql.DB, sessReg *esp.SessionRegistry) error {
	orderID, err := pickInt(cs.ArgTuple.Items, 0, "orderID")
	if err != nil {
		return sendMarketErr(c, pkt, accountID, addr, err)
	}
	newPrice, err := pickFloat(cs.ArgTuple.Items, 1, "newPrice")
	if err != nil {
		return sendMarketErr(c, pkt, accountID, addr, err)
	}
	out, err := evedb.ModifyMarketOrder(ctx, sqldb, charID, orderID, newPrice)
	if err != nil {
		log.Printf("[%s] marketProxy.ModifyCharOrder %d: %v", addr, orderID, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	if out.ErrorMsg != "" {
		return server.SendCallException(c, pkt, accountID, server.BuildUserError(
			"CustomInfo", map[string]marshal.PyRep{
				"info": marshal.PyString{Value: out.ErrorMsg},
			}))
	}
	log.Printf("[%s] → marketProxy.ModifyCharOrder orderID=%d newPrice=%.2f", addr, orderID, newPrice)
	if out.BalanceChanged {
		sendBalanceUpdate(c, addr, accountID, charID, out.NewBalance)
	}
	sendOwnOrderChanged(c, addr, accountID, "Modified", out.TypeID)
	if parents, err := marketParents(ctx, sqldb, stationID, solarSystemID); err == nil {
		broadcastOrderChange(ctx, sqldb, sessReg, addr, parents.RegionID, out.TypeID)
	}
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// ── GetCorporationOrders ──────────────────────────────────────────────────────

func handleGetCorporationOrders(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB) error {
	var corpID int64
	if err := sqldb.QueryRowContext(ctx,
		`SELECT corporationID FROM character_ WHERE characterID = ?`, charID).Scan(&corpID); err != nil {
		log.Printf("[%s] marketProxy.GetCorporationOrders corp lookup: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	result, err := evedb.GetCorporationOrders(ctx, sqldb, corpID)
	if err != nil {
		log.Printf("[%s] marketProxy.GetCorporationOrders db: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	log.Printf("[%s] → marketProxy.GetCorporationOrders corpID=%d", addr, corpID)
	return server.SendCallRsp(c, pkt, accountID, result)
}

// ── Notification helpers ──────────────────────────────────────────────────────

// sendBalanceUpdate refreshes the caller's wallet display.
func sendBalanceUpdate(c *evenet.Conn, addr string, accountID, charID int64, newBalance float64) {
	notifyArgs := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: "cash"},
		marshal.PyInt{Value: uint64(charID)},
		marshal.PyReal{Value: newBalance},
	}}
	if err := server.SendNotification(c, accountID, "OnAccountChange", "clientID", notifyArgs); err != nil {
		log.Printf("[%s] OnAccountChange notification: %v", addr, err)
	}
}

// notifyHangarDeltas pushes hangar changes to another online character, the
// counterparty of a fill.
func notifyHangarDeltas(sessReg *esp.SessionRegistry, charID int64, deltas []evedb.ItemDelta) {
	if sessReg == nil {
		return
	}
	for _, d := range deltas {
		if d.Row == nil {
			continue
		}
		server.NotifyCharacter(sessReg, charID, "OnItemChange", "charid", hangarDeltaArgs(d))
	}
}

// itemDeletedNotifArgs builds the OnItemChange payload for a deleted item: the row
// re-parented to the junkyard with quantity 0, old location and quantity in the change dict.
func itemDeletedNotifArgs(er *evedb.EntityRow) marshal.PyTuple {
	gone := *er
	gone.LocationID = 6 // junkyard
	gone.Quantity = 0
	return marshal.PyTuple{Items: []marshal.PyRep{
		evedb.BuildEntityRow(&gone),
		marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: uint64(er.LocationID)}}, // ixLocationID
			{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(er.Quantity)}},   // ixQuantity
		}},
	}}
}

// hangarDeltaArgs builds the OnItemChange payload for one hangar change. The change
// argument is always a dict: every subscriber iterates it and a None raises TypeError.
func hangarDeltaArgs(d evedb.ItemDelta) marshal.PyTuple {
	switch d.Kind {
	case evedb.ItemDeltaQty:
		return marshal.PyTuple{Items: []marshal.PyRep{
			evedb.BuildEntityRow(d.Row),
			marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(d.OldQty)}}, // ixQuantity
			}},
		}}
	case evedb.ItemDeltaNew:
		// ixLocationID (3) with old = 0 is how godma's insert path signals a new item.
		return marshal.PyTuple{Items: []marshal.PyRep{
			evedb.BuildEntityRow(d.Row),
			marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: 0}},
			}},
		}}
	default: // ItemDeltaRemoved
		return itemDeletedNotifArgs(d.Row)
	}
}

// sendHangarDeltas pushes one OnItemChange per hangar change the caller's order made.
func sendHangarDeltas(c *evenet.Conn, addr string, accountID int64, deltas []evedb.ItemDelta) {
	for _, d := range deltas {
		if d.Row == nil {
			continue
		}
		if err := server.SendNotification(c, accountID, "OnItemChange", "charid", hangarDeltaArgs(d)); err != nil {
			log.Printf("[%s] OnItemChange (hangar delta): %v", addr, err)
		}
	}
}

// ownOrderChangedArgs builds the OnOwnOrderChanged payload. Subscribers take exactly
// (order, reason); a third argument raises TypeError. marketsvc reads order.typeID to
// invalidate the cached GetOrders for that type and treats None as no invalidation, so
// the order is a util.KeyVal carrying typeID.
func ownOrderChangedArgs(reason string, typeID int64) marshal.PyTuple {
	var order marshal.PyRep = marshal.PyNone{}
	if typeID > 0 {
		order = marshal.PyObject{
			Type: "util.KeyVal",
			Args: marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyString{Value: "typeID"}, Value: marshal.PyInt{Value: uint64(typeID)}},
			}},
		}
	}
	return marshal.PyTuple{Items: []marshal.PyRep{
		order,
		marshal.PyString{Value: reason},
	}}
}

// sendOwnOrderChanged refreshes the caller's own market windows.
func sendOwnOrderChanged(c *evenet.Conn, addr string, accountID int64, reason string, typeID int64) {
	if err := server.SendNotification(c, accountID, "OnOwnOrderChanged", "charid", ownOrderChangedArgs(reason, typeID)); err != nil {
		log.Printf("[%s] OnOwnOrderChanged notification: %v", addr, err)
	}
}

// notifyOwnOrderChanged refreshes another character's market windows if they
// are online.
func notifyOwnOrderChanged(sessReg *esp.SessionRegistry, charID int64, reason string, typeID int64) {
	if sessReg == nil {
		return
	}
	server.NotifyCharacter(sessReg, charID, "OnOwnOrderChanged", "charid", ownOrderChangedArgs(reason, typeID))
}

// sessionRegions memoises solarSystemID to regionID for the order-change broadcast.
var sessionRegions syncx.Map[int64, int64]

func sessionRegionID(ctx context.Context, sqldb *sql.DB, solarSystemID int64) int64 {
	if solarSystemID == 0 {
		return 0
	}
	if v, ok := sessionRegions.Load(solarSystemID); ok {
		return v
	}
	_, regionID, err := evedb.SystemConstellationRegion(ctx, sqldb, solarSystemID)
	if err != nil {
		return 0
	}
	sessionRegions.Store(solarSystemID, regionID)
	return regionID
}

// broadcastOrderChange tells every other trader in the region that a market view
// changed. The scope is the region, not the station: order ranges reach past their station
// and a caller in space has no station.
func broadcastOrderChange(ctx context.Context, sqldb *sql.DB, sessReg *esp.SessionRegistry, excludeAddr string, regionID, typeID int64) {
	if sessReg == nil || regionID == 0 {
		return
	}
	args := ownOrderChangedArgs("Change", typeID)
	server.BroadcastNotificationExcept(sessReg, excludeAddr, func(s *esp.Session) bool {
		return sessionRegionID(ctx, sqldb, s.SolarSystemID) == regionID
	}, "OnOwnOrderChanged", "charid", args)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func firstIntArg(cs *server.CallStream, method string) (int64, error) {
	if len(cs.ArgTuple.Items) < 1 {
		return 0, fmt.Errorf("%s: missing arg", method)
	}
	v, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		return 0, fmt.Errorf("%s: arg[0] is %T, want PyInt", method, cs.ArgTuple.Items[0])
	}
	return int64(v.Value), nil
}

func pickInt(args []marshal.PyRep, idx int, name string) (int64, error) {
	if idx >= len(args) {
		return 0, fmt.Errorf("%s arg missing at index %d", name, idx)
	}
	switch v := args[idx].(type) {
	case marshal.PyInt:
		return int64(v.Value), nil
	case marshal.PyReal:
		return int64(v.Value), nil
	}
	return 0, fmt.Errorf("%s arg[%d] is %T, want PyInt", name, idx, args[idx])
}

func pickFloat(args []marshal.PyRep, idx int, name string) (float64, error) {
	if idx >= len(args) {
		return 0, fmt.Errorf("%s arg missing at index %d", name, idx)
	}
	switch v := args[idx].(type) {
	case marshal.PyReal:
		return v.Value, nil
	case marshal.PyInt:
		return float64(v.Value), nil
	}
	return 0, fmt.Errorf("%s arg[%d] is %T, want PyReal", name, idx, args[idx])
}

func pickBool(args []marshal.PyRep, idx int, name string) (bool, error) {
	if idx >= len(args) {
		return false, nil
	}
	switch v := args[idx].(type) {
	case marshal.PyBool:
		return v.Value, nil
	case marshal.PyInt:
		return v.Value != 0, nil
	}
	return false, fmt.Errorf("%s arg[%d] is %T, want PyBool", name, idx, args[idx])
}

func sendMarketErr(c *evenet.Conn, pkt *evenet.PyPacket, accountID int64, addr string, err error) error {
	log.Printf("[%s] marketProxy.PlaceCharOrder arg error: %v", addr, err)
	return server.SendCallException(c, pkt, accountID, nil)
}
