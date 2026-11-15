package db

// Player market order engine: matching, escrow, cancel/modify, expiry. The server
// half of the client's quote.py MarketActionWindow and marketsvc svc.marketQuote;
// the client computes skill limits and fees for display only.
//
// Range values are the client's: rangeStation = -1, rangeSolarSystem = 0,
// rangeRegion = 32767, jump counts 1..40 between. The server has no pathfinder, so
// a jump-count range matches the same constellation. See docs/market-notes.md#order-range.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"time"

	"gurimoon/corp"
	"gurimoon/marshal"
)

// Market skill typeIDs (invTypes).
const (
	SkillTrade           = 3443
	SkillRetail          = 3444
	SkillBrokerRelations = 3446
	SkillWholesale       = 16596
	SkillTycoon          = 18580
	SkillAccounting      = 16622
	SkillMarginTrading   = 16597
)

// Base market fees and the client's range constants.
const (
	mktCommissionPct  = 0.01  // base broker fee: 1% of order value
	mktTransactionTax = 0.01  // base sales tax: 1% of sale value
	mktMinimumFee     = 100.0 // ISK floor on broker fees
	rangeStation      = -1
	rangeSolarSystem  = 0
	rangeRegion       = 32767
	win32TicksPerDay  = int64(864000000000)
)

// BrokerStandingFactor is the standings multiplier on broker fees, the client's
// marketsvc.BrokersFee: 2^(-2 × (0.7×faction + 0.3×corp)/10) toward the station
// owner, from raw chrNPCStandings (the client reads GetStanding, not the effective
// standing). Negative standings raise the fee; an unknown owner pays base. db may be a tx.
func BrokerStandingFactor(ctx context.Context, db rowQueryer, charID, stationID int64) float64 {
	var ownerID int64
	if err := db.QueryRowContext(ctx,
		`SELECT corporationID FROM staStations WHERE stationID = ?`,
		stationID).Scan(&ownerID); err != nil || ownerID == 0 {
		return 1.0
	}
	factionID, _ := GetAgentCorpFactionID(ctx, db, ownerID)
	fac := GetNPCStandingToChar(ctx, db, charID, factionID)
	coc := GetNPCStandingToChar(ctx, db, charID, ownerID)
	weighted := (0.7*fac + 0.3*coc) / 10.0
	return math.Pow(2.0, -2.0*weighted)
}

// EnsureMarketOrdersSchema widens two seed columns: `range` must be signed
// (rangeStation = -1) and `escrow` must hold ISK. Idempotent via information_schema.
func EnsureMarketOrdersSchema(ctx context.Context, db *sql.DB) error {
	var rangeType, escrowType string
	err := db.QueryRowContext(ctx, `
		SELECT
		  (SELECT COLUMN_TYPE FROM information_schema.COLUMNS
		    WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'market_orders' AND COLUMN_NAME = 'range'),
		  (SELECT COLUMN_TYPE FROM information_schema.COLUMNS
		    WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'market_orders' AND COLUMN_NAME = 'escrow')`,
	).Scan(&rangeType, &escrowType)
	if err != nil {
		return fmt.Errorf("EnsureMarketOrdersSchema inspect: %w", err)
	}
	if rangeType != "int(11)" && rangeType != "int" {
		if _, err := db.ExecContext(ctx,
			"ALTER TABLE market_orders MODIFY `range` int NOT NULL DEFAULT 0"); err != nil {
			return fmt.Errorf("EnsureMarketOrdersSchema range: %w", err)
		}
		log.Printf("market_orders: widened `range` %s → int (signed)", rangeType)
	}
	if escrowType != "double" {
		if _, err := db.ExecContext(ctx,
			"ALTER TABLE market_orders MODIFY escrow double NOT NULL DEFAULT 0"); err != nil {
			return fmt.Errorf("EnsureMarketOrdersSchema escrow: %w", err)
		}
		log.Printf("market_orders: widened escrow %s → double", escrowType)
	}
	return nil
}

// MarketSkillLimits mirrors the client's marketQuote.GetSkillLimits math.
type MarketSkillLimits struct {
	MaxOrders    int64
	FeeRate      float64 // broker fee fraction of order value
	TaxRate      float64 // sales tax fraction of sale value
	EscrowFactor float64 // fraction of buy-order value held in escrow
}

// GetMarketSkillLimits computes order-count cap, broker fee, sales tax and escrow
// factor from the character's trained skills, in one query. q may be the order
// engine's own transaction: the matching loop calls this per counterparty while
// holding a row lock and must not take a second pool connection.
func GetMarketSkillLimits(ctx context.Context, q rowQueryer, charID int64) (*MarketSkillLimits, error) {
	levels := make(map[int64]int64, 7)
	rows, err := q.QueryContext(ctx, `
		SELECT e.typeID, COALESCE(ea.valueInt, 1)
		  FROM entity e
		  LEFT JOIN entity_attributes ea ON ea.itemID = e.itemID AND ea.attributeID = 280
		 WHERE e.ownerID = ? AND e.flag IN (7, 61)
		   AND e.typeID IN (?, ?, ?, ?, ?, ?, ?)`,
		charID, SkillTrade, SkillRetail, SkillWholesale, SkillTycoon,
		SkillBrokerRelations, SkillAccounting, SkillMarginTrading)
	if err != nil {
		return nil, fmt.Errorf("GetMarketSkillLimits: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var typeID, level int64
		if err := rows.Scan(&typeID, &level); err != nil {
			return nil, fmt.Errorf("GetMarketSkillLimits scan: %w", err)
		}
		levels[typeID] = level
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetMarketSkillLimits: %w", err)
	}

	trade := levels[SkillTrade]
	retail := levels[SkillRetail]
	wholesale := levels[SkillWholesale]
	tycoon := levels[SkillTycoon]
	broker := levels[SkillBrokerRelations]
	accounting := levels[SkillAccounting]
	margin := levels[SkillMarginTrading]

	escrow := 1.0
	for i := int64(0); i < margin; i++ {
		escrow *= 0.75
	}
	return &MarketSkillLimits{
		MaxOrders:    5 + trade*4 + retail*8 + wholesale*16 + tycoon*32,
		FeeRate:      mktCommissionPct * (1.0 - 0.05*float64(broker)),
		TaxRate:      mktTransactionTax * (1.0 - 0.10*float64(accounting)),
		EscrowFactor: escrow,
	}, nil
}

// MarketFill records one executed trade during order matching.
type MarketFill struct {
	CounterpartyID int64 // NPC corpID for seeder orders, charID for players
	Price          float64
	Quantity       int64
	StationID      int64   // station where the trade executed
	ItemEntityIDs  []int64 // entity rows the goods landed in (empty = NPC side)
	// ItemDeltas describes those rows: a fill merged into an existing stack is a
	// quantity change, not a new item, and the client is told which.
	ItemDeltas []ItemDelta
	IsNPCOrder bool // true when the counterparty was an NPC seeder

	// CounterpartyBalance tells the resting order's owner their new wallet: their
	// client learns of a fill through OnOwnOrderChanged, which carries no balance,
	// and the neocom wealth advances only on OnAccountChange. Zero when no wallet moved.
	CounterpartyBalance        float64
	CounterpartyWalletOwnerID  int64 // charID, or corpID for a corp order
	CounterpartyBalanceChanged bool
}

// PlaceOrderOutcome is the result of PlaceBuyOrder / PlaceSellOrder.
// ErrorMsg carries a user-facing refusal; when set, nothing was changed.
type PlaceOrderOutcome struct {
	Fills          []MarketFill
	FilledQty      int64
	RestingOrderID int64
	NewBalance     float64
	BalanceChanged bool
	ErrorMsg       string

	// ErrorKey, when set, is the client's own message key (config.BulkData.messages)
	// and ErrorArgs its %(name)s arguments; the market service raises it instead of
	// ErrorMsg, which stays filled for callers that only log it (the rolebots module).
	ErrorKey  string
	ErrorArgs map[string]marshal.PyRep

	// HangarDeltas are the changes a sell made to the selling account's hangar: the
	// stacks consumed to back the order, plus anything handed back. The client caches
	// its hangar and must be pushed these, or the sold stack stays on screen.
	HangarDeltas []ItemDelta
}

func refuse(msg string, args ...interface{}) *PlaceOrderOutcome {
	return &PlaceOrderOutcome{ErrorMsg: fmt.Sprintf(msg, args...)}
}

// refuseKey is refuse with the client's message key for the same refusal.
func refuseKey(key string, keyArgs map[string]marshal.PyRep, msg string, args ...interface{}) *PlaceOrderOutcome {
	o := refuse(msg, args...)
	o.ErrorKey, o.ErrorArgs = key, keyArgs
	return o
}

// iskWhole renders whole ISK with thousands separators: 1,250,000.
func iskWhole(v float64) string {
	neg := v < 0
	s := strconv.FormatFloat(math.Abs(math.Round(v)), 'f', 0, 64)
	var b []byte
	for i := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b = append(b, ',')
		}
		b = append(b, s[i])
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

// mktISK formats ISK for a message argument the client prints with %s.
func mktISK(v float64) string {
	cents := int64(math.Round(math.Abs(v) * 100))
	s := iskWhole(float64(cents / 100))
	if v < 0 {
		s = "-" + s
	}
	return fmt.Sprintf("%s.%02d ISK", s, cents%100)
}

// mktNotEnoughCash is MktNotEnoughCash: "you would need the total sum of
// %(cashAmount)s (of which %(tax)s in tax), while your current balance is
// only %(balance)s".
func mktNotEnoughCash(total, tax, balance float64) map[string]marshal.PyRep {
	return map[string]marshal.PyRep{
		"cashAmount": marshal.PyString{Value: mktISK(total)},
		"tax":        marshal.PyString{Value: mktISK(tax)},
		"balance":    marshal.PyString{Value: mktISK(balance)},
	}
}

// mktOrderCap is MarketExceededOrderCount's arguments.
func mktOrderCap(open, max int64) map[string]marshal.PyRep {
	return map[string]marshal.PyRep{
		"curCnt": marshal.PyInt{Value: uint64(open)},
		"maxCnt": marshal.PyInt{Value: uint64(max)},
	}
}

// charBalance reads a character's wallet inside tx.
func charBalance(ctx context.Context, tx *sql.Tx, charID int64) (float64, error) {
	var bal float64
	err := tx.QueryRowContext(ctx,
		`SELECT balance FROM character_ WHERE characterID = ?`, charID).Scan(&bal)
	return bal, err
}

// adjustBalance applies delta to a character wallet and returns the new
// balance. NPC characters without a character_ row are silently skipped
// (returns 0, false).
func adjustBalance(ctx context.Context, tx *sql.Tx, charID int64, delta float64) (float64, bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE character_ SET balance = balance + ? WHERE characterID = ?`, delta, charID)
	if err != nil {
		return 0, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, false, nil
	}
	bal, err := charBalance(ctx, tx, charID)
	return bal, true, err
}

// journal writes a market_journal row. rowOwnerID is whose journal the row is in
// (market_journal.characterID). ownerID1/ownerID2 fill the refType's fixed text; for
// refType 2 that is "<ownerID1> bought stuff from <ownerID2>", so ownerID1 is always the
// buyer and ownerID2 the seller, on both journals. Errors follow txRowErr.
func journal(ctx context.Context, tx *sql.Tx, rowOwnerID, ownerID1, ownerID2 int64, amount, balance float64, reason string) error {
	return journalRef(ctx, tx, RefTypeMarketTransaction, rowOwnerID, ownerID1, ownerID2, amount, balance, reason)
}

// txRowErr is the error rule for the record rows a trade writes (journal, transaction
// list). A lock conflict is returned: InnoDB has already rolled the whole transaction back,
// so the caller must stop and let retryOutcome rerun the trade, or the statements after it
// would commit one by one on their own. Any other failure costs only that row, so it is
// logged and the trade goes on.
func txRowErr(err error, what string, args ...interface{}) error {
	if err == nil {
		return nil
	}
	if IsRetryableTxError(err) {
		return err
	}
	log.Printf(what+": %v", append(args, err)...)
	return nil
}

// Wallet journal refTypes used by the market. The client draws each row's text
// from the refType, and drops any row whose refType is missing from market_reftypes.
const (
	RefTypeMarketTransaction = 2
	RefTypeMarketEscrow      = 42
	RefTypeBrokerFee         = 46
	RefTypeTransactionTax    = 54
)

// journalRef writes a market_journal row under an explicit refType. Errors follow txRowErr.
func journalRef(ctx context.Context, tx *sql.Tx, refTypeID, rowOwnerID, ownerID1, ownerID2 int64, amount, balance float64, reason string) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO market_journal
		  (characterID, transDate, refTypeID, ownerID1, ownerID2, argID1,
		   accountID, amount, balance, reason, accountKey)
		VALUES (?, ?, ?, ?, ?, NULL, 0, ?, ?, ?, 1000)`,
		rowOwnerID, win32TimeNow(), refTypeID, ownerID1, ownerID2, amount, balance, reason)
	return txRowErr(err, "market journal (%s, char %d)", reason, rowOwnerID)
}

// recordTransaction writes a market_transactions row. ownerCharID is whose transaction
// list it is in (market_transactions.characterID); clientID is the counterparty in the
// Client column. transactionType: 1 buy, 0 sell. Errors follow txRowErr.
func recordTransaction(ctx context.Context, tx *sql.Tx, ownerCharID, clientID, typeID, quantity int64, price float64, txType, regionID, stationID int64) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO market_transactions
		  (transactionDateTime, typeID, quantity, price, transactionType, characterID, clientID, regionID, stationID)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		win32TimeNow(), typeID, quantity, price, txType, ownerCharID, clientID, regionID, stationID)
	return txRowErr(err, "market transaction (char %d type %d)", ownerCharID, typeID)
}

// typeInfo fetches name, group and category for entity creation; BuildEntityRow
// describes a delivered row with all three.
func typeInfo(ctx context.Context, tx *sql.Tx, typeID int64) (name string, groupID, categoryID int64) {
	err := tx.QueryRowContext(ctx, `
		SELECT t.typeName, t.groupID, g.categoryID
		  FROM invTypes t JOIN invGroups g ON t.groupID = g.groupID
		 WHERE t.typeID = ?`, typeID).Scan(&name, &groupID, &categoryID)
	if err != nil {
		return fmt.Sprintf("Item %d", typeID), 0, 0
	}
	return name, groupID, categoryID
}

// categoryBlueprint is invCategories.categoryID for Blueprint.
const categoryBlueprint = 9

// deliverItem lands quantity units of typeID in the account's hangar and reports
// the entity changes for the caller to push. Ordinary goods merge into an existing
// packaged stack, as every other Add path does. Blueprints are one singleton item
// each with its own invBlueprints row, which GetBlueprintAttributes requires.
func deliverItem(ctx context.Context, tx *sql.Tx, acct MarketAccount, typeID, quantity int64) ([]ItemDelta, error) {
	typeName, groupID, categoryID := typeInfo(ctx, tx, typeID)
	const flagHangar = 4

	row := func(itemID, singleton, qty int64) *EntityRow {
		return &EntityRow{
			ItemID: itemID, TypeID: typeID, OwnerID: acct.OwnerID,
			LocationID: acct.HangarLoc, Flag: flagHangar, GroupID: groupID,
			CategoryID: categoryID, Singleton: singleton, Quantity: qty,
		}
	}

	if categoryID != categoryBlueprint {
		// Merge into an existing packaged stack when there is one.
		var stackID, stackQty int64
		err := tx.QueryRowContext(ctx, `
			SELECT itemID, quantity FROM entity
			 WHERE ownerID = ? AND locationID = ? AND typeID = ? AND flag = ?
			   AND singleton = 0
			 ORDER BY itemID ASC LIMIT 1 FOR UPDATE`,
			acct.OwnerID, acct.HangarLoc, typeID, flagHangar).Scan(&stackID, &stackQty)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("deliverItem stack lookup: %w", err)
		}
		if err == nil {
			if _, err := tx.ExecContext(ctx,
				`UPDATE entity SET quantity = quantity + ? WHERE itemID = ?`,
				quantity, stackID); err != nil {
				return nil, fmt.Errorf("deliverItem stack: %w", err)
			}
			return []ItemDelta{{Kind: ItemDeltaQty, Row: row(stackID, 0, stackQty+quantity), OldQty: stackQty}}, nil
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO entity
			  (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
			VALUES (?, ?, ?, ?, ?, 0, 0, ?, 0, 0, 0, NULL)`,
			typeName, typeID, acct.OwnerID, acct.HangarLoc, flagHangar, quantity)
		if err != nil {
			return nil, fmt.Errorf("deliverItem: %w", err)
		}
		id, _ := res.LastInsertId()
		return []ItemDelta{{Kind: ItemDeltaNew, Row: row(id, 0, quantity)}}, nil
	}

	deltas := make([]ItemDelta, 0, quantity)
	for i := int64(0); i < quantity; i++ {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO entity
			  (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
			VALUES (?, ?, ?, ?, ?, 0, 1, 1, 0, 0, 0, NULL)`,
			typeName, typeID, acct.OwnerID, acct.HangarLoc, flagHangar)
		if err != nil {
			return nil, fmt.Errorf("deliverItem: %w", err)
		}
		id, _ := res.LastInsertId()
		if _, err := tx.ExecContext(ctx, `
			INSERT IGNORE INTO invBlueprints
			  (blueprintID, materialLevel, productivityLevel, copy, licensedProductionRunsRemaining)
			VALUES (?, 0, 0, 0, -1)`, id); err != nil {
			return nil, fmt.Errorf("deliverItem blueprint row: %w", err)
		}
		deltas = append(deltas, ItemDelta{Kind: ItemDeltaNew, Row: row(id, 1, 1)})
	}
	return deltas, nil
}

// deltaItemIDs lists the entity rows a delivery touched, newest last.
func deltaItemIDs(deltas []ItemDelta) []int64 {
	ids := make([]int64, 0, len(deltas))
	for _, d := range deltas {
		if d.Row != nil {
			ids = append(ids, d.Row.ItemID)
		}
	}
	return ids
}

// orderReachesSeller is SQL matching orders whose own `range` reaches a trader at
// the given station, system or constellation. The order's constellation comes from a
// staStations join aliased `st`.
const orderReachesSeller = `
	((o.range = -1 AND o.stationID = ?)
	 OR (o.range = 0 AND o.solarSystemID = ?)
	 OR (o.range BETWEEN 1 AND 32766 AND st.constellationID = ?)
	 OR (o.range >= 32767))`

// askWithinBuyerRange matches sell orders lying within the buyer's chosen
// range of the buyer's station.
func askWithinBuyerRange(orderRange int64) string {
	switch {
	case orderRange <= rangeStation:
		return `a.stationID = ?`
	case orderRange == rangeSolarSystem:
		return `a.solarSystemID = ?`
	case orderRange < rangeRegion:
		return `st.constellationID = ?`
	default:
		return `1 = ?` // regionID filter already applied; param is always 1
	}
}

func buyerRangeParam(orderRange int64, p *StationParents, stationID int64) int64 {
	switch {
	case orderRange <= rangeStation:
		return stationID
	case orderRange == rangeSolarSystem:
		return p.SolarSystemID
	case orderRange < rangeRegion:
		return p.ConstellationID
	default:
		return 1
	}
}

// sellerTaxFunc returns a memoized counterparty sales-tax lookup for one call: a
// buy walks several asks of one seller. A failed lookup keeps the base rate rather
// than failing a trade whose goods have already moved.
func sellerTaxFunc(ctx context.Context, q rowQueryer) func(charID int64) float64 {
	seen := make(map[int64]float64)
	return func(charID int64) float64 {
		if rate, ok := seen[charID]; ok {
			return rate
		}
		rate := mktTransactionTax
		if limits, err := GetMarketSkillLimits(ctx, q, charID); err == nil {
			rate = limits.TaxRate
		}
		seen[charID] = rate
		return rate
	}
}

// countOpenOrders returns the character's listed order count.
func countOpenOrders(ctx context.Context, tx *sql.Tx, charID int64) (int64, error) {
	var n int64
	err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM market_orders WHERE charID = ? AND orderState = 1`,
		charID).Scan(&n)
	return n, err
}

// ── Corporation trading ───────────────────────────────────────────────────────

// MarketAccount is the wallet and hangar an order trades out of: the placing
// character, or their corporation for a corp order, whose ISK leaves the corp
// wallet and whose goods sit in the corp office. Every wallet and hangar touch routes here.
type MarketAccount struct {
	CharID    int64
	OwnerID   int64 // charID, or corporationID for a corp order
	IsCorp    bool
	HangarLoc int64 // stationID for a character; the corp's officeID for a corp
	// Roles is the caller's corp role mask (RoleAll for the CEO). It decides which
	// office divisions a corp sell may take from.
	Roles uint64
}

// corpDivisionFlags are the entity flags of office divisions 1-7.
var corpDivisionFlags = [7]int64{4, 116, 117, 118, 119, 120, 121}

// corpTakeRoles are the per-division take rights, in the same order.
var corpTakeRoles = [7]uint64{
	corp.RoleHangarCanTake1, corp.RoleHangarCanTake2, corp.RoleHangarCanTake3,
	corp.RoleHangarCanTake4, corp.RoleHangarCanTake5, corp.RoleHangarCanTake6,
	corp.RoleHangarCanTake7,
}

// takeableDivisionFlags lists the office divisions this account may sell out of.
// A Director gets all seven unless CorpRoleFilter strips a take right; the filter
// runs over the Director's implied rights too.
func (a MarketAccount) takeableDivisionFlags() []int64 {
	if !a.IsCorp {
		return []int64{flagHangar}
	}
	roles := a.Roles
	if roles&corp.RoleDirector != 0 || roles == corp.RoleAll {
		for _, r := range corpTakeRoles {
			roles |= r
		}
	}
	if f := CorpRoleFilter; f != nil {
		roles = f(a.OwnerID, a.CharID, roles)
	}
	flags := make([]int64, 0, len(corpDivisionFlags))
	for i, f := range corpDivisionFlags {
		if roles&corpTakeRoles[i] != 0 {
			flags = append(flags, f)
		}
	}
	return flags
}

// CharMarketAccount is charID's own wallet and station hangar.
func CharMarketAccount(charID, stationID int64) MarketAccount {
	return MarketAccount{CharID: charID, OwnerID: charID, HangarLoc: stationID}
}

// corpTradeRoles may trade with corporation assets. The client shows its "Use corp
// account" box to Accountant or Trader (quote.py); Director is kept for its
// implied rights. Change this together with the refusal text below.
const corpTradeRoles = corp.RoleDirector | corp.RoleAccountant | corp.RoleTrader

// ResolveMarketAccount decides which wallet and hangar a useCorp order acts on and
// refuses when the corporation cannot hold one: it needs a player corporation, a
// trading role and an office at the station (corp goods land in the office hangar).
func ResolveMarketAccount(ctx context.Context, db *sql.DB, charID, stationID int64, useCorp bool) (MarketAccount, string, error) {
	if !useCorp {
		return CharMarketAccount(charID, stationID), "", nil
	}
	var corpID, ceoID int64
	if err := db.QueryRowContext(ctx, `
		SELECT ch.corporationID, COALESCE(co.ceoID, 0)
		  FROM character_ ch
		  LEFT JOIN corporation co ON co.corporationID = ch.corporationID
		 WHERE ch.characterID = ?`, charID).Scan(&corpID, &ceoID); err != nil {
		return MarketAccount{}, "", fmt.Errorf("ResolveMarketAccount corp of %d: %w", charID, err)
	}
	if corpID < MinPlayerEntityID {
		return MarketAccount{}, "An NPC corporation cannot hold market orders.", nil
	}
	roles := corp.RoleAll
	if charID != ceoID {
		info, err := GetCorporationMemberInfo(ctx, db, charID)
		if err != nil {
			return MarketAccount{}, "", err
		}
		if info.Corprole&corpTradeRoles == 0 {
			return MarketAccount{}, "You need the Accountant or Trader role in your corporation.", nil
		}
		roles = info.Corprole
	}
	officeID, _, err := GetCorpOfficeAt(ctx, db, corpID, stationID)
	if err != nil {
		return MarketAccount{}, "", err
	}
	if officeID == 0 {
		return MarketAccount{}, "Your corporation has no office at this station.", nil
	}
	return MarketAccount{CharID: charID, OwnerID: corpID, IsCorp: true, HangarLoc: officeID, Roles: roles}, "", nil
}

// accountForOrder resolves the wallet and hangar behind an order already on the
// book: a corp order's refund goes to the corp wallet and its goods to the corp
// office. With no office at the station any more, the placing character's hangar is used.
func accountForOrder(ctx context.Context, tx *sql.Tx, charID, stationID int64, isCorp bool) MarketAccount {
	if !isCorp {
		return CharMarketAccount(charID, stationID)
	}
	var corpID int64
	if err := tx.QueryRowContext(ctx,
		`SELECT corporationID FROM character_ WHERE characterID = ?`, charID,
	).Scan(&corpID); err != nil || corpID < MinPlayerEntityID {
		return CharMarketAccount(charID, stationID)
	}
	var officeID int64
	if err := tx.QueryRowContext(ctx,
		`SELECT itemID FROM crpoffices WHERE corporationID = ? AND stationID = ?`,
		corpID, stationID).Scan(&officeID); err != nil || officeID == 0 {
		// A corp that owns the station outright needs no office row.
		var owner int64
		if e := tx.QueryRowContext(ctx,
			`SELECT corporationID FROM staStations WHERE stationID = ?`, stationID,
		).Scan(&owner); e == nil && owner == corpID {
			officeID = stationID
		} else {
			return CharMarketAccount(charID, stationID)
		}
	}
	return MarketAccount{CharID: charID, OwnerID: corpID, IsCorp: true, HangarLoc: officeID}
}

// accountBalance reads the wallet behind a MarketAccount inside tx.
func accountBalance(ctx context.Context, tx *sql.Tx, a MarketAccount) (float64, error) {
	if a.IsCorp {
		var bal float64
		err := tx.QueryRowContext(ctx,
			`SELECT balance FROM corporation WHERE corporationID = ?`, a.OwnerID).Scan(&bal)
		return bal, err
	}
	return charBalance(ctx, tx, a.CharID)
}

// adjustAccount applies delta to a MarketAccount's wallet and returns the new
// balance. Owners with no wallet row (NPC characters) are silently skipped,
// as adjustBalance does.
func adjustAccount(ctx context.Context, tx *sql.Tx, a MarketAccount, delta float64) (float64, bool, error) {
	if !a.IsCorp {
		return adjustBalance(ctx, tx, a.CharID, delta)
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE corporation SET balance = balance + ? WHERE corporationID = ?`, delta, a.OwnerID)
	if err != nil {
		return 0, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, false, nil
	}
	bal, err := accountBalance(ctx, tx, a)
	return bal, true, err
}

// ── PlaceBuyOrder ─────────────────────────────────────────────────────────────

// PlaceBuyOrder executes a player buy: fills against the cheapest matching
// sell orders (price ≤ bid price, within orderRange of stationID), then, if
// quantity remains and duration > 0, escrows a resting bid.
func PlaceBuyOrder(ctx context.Context, db *sql.DB, charID, stationID, typeID int64, price float64, quantity, orderRange, minVolume, duration int64, useCorp bool) (*PlaceOrderOutcome, error) {
	return retryOutcome(ctx, func() (*PlaceOrderOutcome, error) {
		return placeBuyOrderOnce(ctx, db, charID, stationID, typeID, price, quantity, orderRange, minVolume, duration, useCorp)
	})
}

// placeBuyOrderOnce is one attempt at PlaceBuyOrder; retryOutcome reruns it on a
// lock conflict (txretry.go).
func placeBuyOrderOnce(ctx context.Context, db *sql.DB, charID, stationID, typeID int64, price float64, quantity, orderRange, minVolume, duration int64, useCorp bool) (*PlaceOrderOutcome, error) {
	if quantity < 1 || price <= 0 {
		return refuse("Invalid price or quantity."), nil
	}
	parents, err := GetStationParents(ctx, db, stationID)
	if err != nil {
		return nil, err
	}
	limits, err := GetMarketSkillLimits(ctx, db, charID)
	if err != nil {
		return nil, err
	}
	// Standings do not move during a trade, so the broker factor is read before the
	// transaction opens.
	brokerFactor := BrokerStandingFactor(ctx, db, charID, stationID)

	// A corp order spends the corp wallet and fills the corp office.
	acct, refusal, err := ResolveMarketAccount(ctx, db, charID, stationID, useCorp)
	if err != nil {
		return nil, err
	}
	if refusal != "" {
		return refuse("%s", refusal), nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	sellerTax := sellerTaxFunc(ctx, tx)
	out := &PlaceOrderOutcome{}
	remaining := quantity
	balance, err := accountBalance(ctx, tx, acct)
	if err != nil {
		return nil, fmt.Errorf("PlaceBuyOrder balance: %w", err)
	}

	rangeSQL := askWithinBuyerRange(orderRange)
	rangeArg := buyerRangeParam(orderRange, parents, stationID)

	// Prices carry a half-cent tolerance: the client submits 0.01 ISK roundings while
	// stored orders may hold full-precision floats (seeded data).
	for remaining > 0 {
		var askID, askCharID, askVol, askStationID, askAccountID, askIsCorp int64
		var askPrice float64
		err := tx.QueryRowContext(ctx, `
			SELECT a.orderID, a.charID, a.price, a.volRemaining, a.stationID, a.accountID, a.isCorp
			  FROM market_orders a
			  JOIN staStations st ON st.stationID = a.stationID
			 WHERE a.regionID = ? AND a.typeID = ? AND a.bid = 0 AND a.orderState = 1
			   AND a.volRemaining > 0 AND a.price <= ? + 0.005 AND a.charID <> ?
			   AND `+rangeSQL+`
			 ORDER BY a.price ASC, a.issued ASC
			 LIMIT 1 FOR UPDATE`,
			parents.RegionID, typeID, price, charID, rangeArg,
		).Scan(&askID, &askCharID, &askPrice, &askVol, &askStationID, &askAccountID, &askIsCorp)
		// For an NPC seeder ask, accountID is the NPC corporation; journal and item
		// notifications name it rather than the seeder character.
		counterpartyID := askCharID
		if IsNPCSeeder(askCharID) && askAccountID != 0 {
			counterpartyID = askAccountID
		}
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("PlaceBuyOrder match: %w", err)
		}

		fill := remaining
		if askVol < fill {
			fill = askVol
		}
		cost := askPrice * float64(fill)
		if balance < cost {
			// Afford as much as possible; refuse outright when nothing fits.
			affordable := int64(balance / askPrice)
			if affordable < 1 {
				if len(out.Fills) == 0 {
					return refuseKey("MktNotEnoughCash", mktNotEnoughCash(cost, 0, balance),
						"You do not have enough ISK: this purchase requires %.2f ISK.", cost), nil
				}
				break
			}
			fill = affordable
			cost = askPrice * float64(fill)
		}

		// Buyer pays.
		balance -= cost
		if _, _, err := adjustAccount(ctx, tx, acct, -cost); err != nil {
			return nil, fmt.Errorf("PlaceBuyOrder debit: %w", err)
		}
		if err := journal(ctx, tx, acct.OwnerID, acct.OwnerID, counterpartyID, -cost, balance, "Market purchase"); err != nil {
			return nil, err
		}
		if err := recordTransaction(ctx, tx, charID, counterpartyID, typeID, fill, askPrice, 1, parents.RegionID, askStationID); err != nil {
			return nil, err
		}

		// Seller (player only) receives proceeds minus sales tax, into the corporation's
		// wallet when the ask was a corp order.
		var sellerBalance float64
		var sellerWalletOwner int64
		var sellerBalanceChanged bool
		if !IsNPCSeeder(askCharID) {
			askAcct := accountForOrder(ctx, tx, askCharID, askStationID, askIsCorp != 0)
			taxRate := sellerTax(askCharID)
			tax := cost * taxRate
			proceeds := cost - tax
			if newBal, okc, err := adjustAccount(ctx, tx, askAcct, proceeds); err != nil {
				return nil, fmt.Errorf("PlaceBuyOrder credit seller: %w", err)
			} else if okc {
				// Gross sale and sales tax are two journal rows, as the client
				// expects: a single net row hides the tax from the seller.
				if err := journal(ctx, tx, askAcct.OwnerID, acct.OwnerID, askAcct.OwnerID, cost, newBal+tax, "Market sale"); err != nil {
					return nil, err
				}
				if tax > 0 {
					if err := journalRef(ctx, tx, RefTypeTransactionTax, askAcct.OwnerID, askAcct.OwnerID, askStationID, -tax, newBal, "Sales tax"); err != nil {
						return nil, err
					}
				}
				sellerBalance, sellerWalletOwner, sellerBalanceChanged = newBal, askAcct.OwnerID, true
			}
			if err := recordTransaction(ctx, tx, askCharID, charID, typeID, fill, askPrice, 0, parents.RegionID, askStationID); err != nil {
				return nil, err
			}
		}

		// Consume the ask.
		if askVol-fill <= 0 {
			_, err = tx.ExecContext(ctx,
				`UPDATE market_orders SET volRemaining = 0, orderState = 3 WHERE orderID = ?`, askID)
		} else {
			_, err = tx.ExecContext(ctx,
				`UPDATE market_orders SET volRemaining = volRemaining - ? WHERE orderID = ?`, fill, askID)
		}
		if err != nil {
			return nil, fmt.Errorf("PlaceBuyOrder consume ask: %w", err)
		}

		// Buying out an NPC ask moves its price and its corporation's sibling asks
		// (ApplyNPCSellImpact), inside this transaction. See docs/market-notes.md#npc-impact.
		if askCharID == SeederCharID {
			if err := ApplyNPCSellImpact(ctx, tx, askID, typeID, askAccountID, askVol, fill); err != nil {
				return nil, err
			}
		}

		// Deliver at the ask's station; a corp order fills the corp office there, which
		// may differ from the caller's station.
		deliverAcct := acct
		deliverAcct.HangarLoc = askStationID
		if acct.IsCorp {
			deliverAcct = accountForOrder(ctx, tx, charID, askStationID, true)
		}
		deltas, err := deliverItem(ctx, tx, deliverAcct, typeID, fill)
		if err != nil {
			return nil, err
		}

		out.Fills = append(out.Fills, MarketFill{
			CounterpartyID: counterpartyID, Price: askPrice, Quantity: fill,
			StationID: askStationID, ItemEntityIDs: deltaItemIDs(deltas),
			ItemDeltas:                 deltas,
			IsNPCOrder:                 IsNPCSeeder(askCharID),
			CounterpartyBalance:        sellerBalance,
			CounterpartyWalletOwnerID:  sellerWalletOwner,
			CounterpartyBalanceChanged: sellerBalanceChanged,
		})
		out.FilledQty += fill
		remaining -= fill
	}

	if remaining > 0 && duration > 0 {
		// Resting bid: enforce order cap, take escrow + broker fee.
		open, err := countOpenOrders(ctx, tx, charID)
		if err != nil {
			return nil, err
		}
		if open >= limits.MaxOrders {
			if len(out.Fills) == 0 {
				return refuseKey("MarketExceededOrderCount", mktOrderCap(open, limits.MaxOrders),
					"You cannot have more than %d open market orders with your current skills.", limits.MaxOrders), nil
			}
			remaining = 0 // keep the fills, drop the resting remainder
		}
		if remaining > 0 {
			escrow := price * float64(remaining) * limits.EscrowFactor
			fee := limits.FeeRate * brokerFactor * price * float64(remaining)
			if fee < mktMinimumFee {
				fee = mktMinimumFee
			}
			if balance < escrow+fee {
				if len(out.Fills) == 0 {
					return refuseKey("MktNotEnoughCash", mktNotEnoughCash(escrow+fee, fee, balance),
						"You do not have enough ISK to cover escrow (%.2f) and broker fee (%.2f).", escrow, fee), nil
				}
				remaining = 0
			} else {
				balance -= escrow + fee
				if _, _, err := adjustAccount(ctx, tx, acct, -(escrow + fee)); err != nil {
					return nil, fmt.Errorf("PlaceBuyOrder escrow debit: %w", err)
				}
				if err := journalRef(ctx, tx, RefTypeMarketEscrow, acct.OwnerID, acct.OwnerID, stationID, -escrow, balance+fee, "Market escrow"); err != nil {
					return nil, err
				}
				if err := journalRef(ctx, tx, RefTypeBrokerFee, acct.OwnerID, acct.OwnerID, stationID, -fee, balance, "Broker fee"); err != nil {
					return nil, err
				}
				if minVolume < 1 {
					minVolume = 1
				}
				isCorp := 0
				if useCorp {
					isCorp = 1
				}
				res, err := tx.ExecContext(ctx, `
					INSERT INTO market_orders
					  (typeID, charID, regionID, stationID, `+"`range`"+`, bid, price,
					   volEntered, volRemaining, issued, orderState, minVolume, contraband,
					   accountID, duration, isCorp, solarSystemID, escrow)
					VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?, 1, ?, 0, ?, ?, ?, ?, ?)`,
					typeID, charID, parents.RegionID, stationID, orderRange, price,
					remaining, remaining, win32TimeNow(), minVolume,
					charID, duration, isCorp, parents.SolarSystemID, escrow)
				if err != nil {
					return nil, fmt.Errorf("PlaceBuyOrder insert bid: %w", err)
				}
				out.RestingOrderID, _ = res.LastInsertId()
			}
		}
	} else if remaining == quantity && duration == 0 {
		return refuseKey("MktOrderDidNotMatch", nil,
			"No sell orders match your bid — immediate buy orders require a matching seller."), nil
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	out.NewBalance = balance
	out.BalanceChanged = true
	return out, nil
}

// ── PlaceSellOrder ────────────────────────────────────────────────────────────

// loadResearchedBlueprints marks which of the given blueprint items carry research
// or a run limit and so cannot survive a market fill's destroy-and-recreate. The
// locking read is on invBlueprints alone, keyed by the seller's own itemIDs.
func loadResearchedBlueprints(ctx context.Context, tx *sql.Tx, ids []int64, out map[int64]bool) error {
	if len(ids) == 0 {
		return nil
	}
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT blueprintID, COALESCE(copy, 0), COALESCE(materialLevel, 0),
		       COALESCE(productivityLevel, 0),
		       COALESCE(licensedProductionRunsRemaining, -1)
		  FROM invBlueprints
		 WHERE blueprintID IN (?`+strings.Repeat(", ?", len(ids)-1)+`)
		 FOR UPDATE`, args...)
	if err != nil {
		return fmt.Errorf("loadResearchedBlueprints: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, copyFlag, me, pe, runs int64
		if err := rows.Scan(&id, &copyFlag, &me, &pe, &runs); err != nil {
			return err
		}
		// A blueprint with no row at all, or one at ME 0 / PE 0 / unlimited
		// runs, is an untouched original and sells like any other stack.
		if copyFlag != 0 || me != 0 || pe != 0 || runs != -1 {
			out[id] = true
		}
	}
	return rows.Err()
}

// consumeItems removes quantity units of typeID from the selling account's hangar,
// preferring the row itemID names, and reports the entity changes. Assembled items
// (repackage first), blueprint copies and researched originals (a fill mints the
// buyer's row at ME 0 / PE 0 / unlimited runs) are refused, not quietly consumed.
func consumeItems(ctx context.Context, tx *sql.Tx, acct MarketAccount, typeID, itemID, quantity int64) ([]ItemDelta, error) {
	// A corp office holds seven divisions; the account's take rights decide which are
	// in scope, so a Trader cannot sell out of a division they cannot open.
	flags := acct.takeableDivisionFlags()
	if len(flags) == 0 {
		return nil, fmt.Errorf("you cannot take items out of any of your corporation's hangar divisions")
	}
	// A corp sale names the stack it was placed from and sells out of that stack's
	// division only.
	if acct.IsCorp && itemID != 0 {
		var named int64
		if err := tx.QueryRowContext(ctx,
			`SELECT flag FROM entity WHERE itemID = ? AND ownerID = ? AND locationID = ? AND typeID = ?`,
			itemID, acct.OwnerID, acct.HangarLoc, typeID).Scan(&named); err == nil {
			for _, f := range flags {
				if f == named {
					flags = []int64{named}
					break
				}
			}
		}
	}
	// The locking read joins nothing: FOR UPDATE locks every table it reads, and the
	// shared invGroups row would serialise sellers. See docs/market-notes.md#for-update-join-locks.
	flagSQL := "e.flag IN (?" + strings.Repeat(", ?", len(flags)-1) + ")"
	args := []interface{}{acct.OwnerID, acct.HangarLoc, typeID}
	for _, f := range flags {
		args = append(args, f)
	}
	args = append(args, itemID)
	rows, err := tx.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, e.ownerID, e.locationID, e.flag,
		       e.singleton, e.quantity
		  FROM entity e
		 WHERE e.ownerID = ? AND e.locationID = ? AND e.typeID = ? AND `+flagSQL+`
		 ORDER BY (e.itemID = ?) DESC, e.itemID ASC
		 FOR UPDATE`, args...)
	if err != nil {
		return nil, fmt.Errorf("consumeItems query: %w", err)
	}
	type stack struct {
		row        EntityRow
		blueprint  bool
		unsellable bool
	}
	var stacks []stack
	var ids []int64
	for rows.Next() {
		var st stack
		if err := rows.Scan(&st.row.ItemID, &st.row.TypeID, &st.row.OwnerID, &st.row.LocationID,
			&st.row.Flag, &st.row.Singleton, &st.row.Quantity); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, st.row.ItemID)
		stacks = append(stacks, st)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Every row above shares the one typeID in the WHERE clause, so group and
	// category are a single unlocked lookup rather than a per-row join.
	_, groupID, categoryID := typeInfo(ctx, tx, typeID)

	researched := map[int64]bool{}
	if categoryID == categoryBlueprint && len(ids) > 0 {
		if err := loadResearchedBlueprints(ctx, tx, ids, researched); err != nil {
			return nil, err
		}
	}

	// A print the corporation has locked down may not leave its division,
	// and a sale is the most final way to leave (corplockdown.go).
	lockedDown := map[int64]bool{}
	if categoryID == categoryBlueprint && len(ids) > 0 {
		var err error
		if lockedDown, err = LockedDownAmong(ctx, tx, ids); err != nil {
			return nil, err
		}
	}

	var have, assembled, lockedBlueprints, lockedDownQty int64
	for i := range stacks {
		st := &stacks[i]
		st.row.GroupID, st.row.CategoryID = groupID, categoryID
		st.blueprint = categoryID == categoryBlueprint
		switch {
		case lockedDown[st.row.ItemID]:
			st.unsellable = true
			lockedDownQty += st.row.Quantity
		case st.blueprint && researched[st.row.ItemID]:
			st.unsellable = true
			lockedBlueprints += st.row.Quantity
		case !st.blueprint && st.row.Singleton != 0:
			st.unsellable = true
			assembled += st.row.Quantity
		default:
			have += st.row.Quantity
		}
	}
	if have < quantity {
		switch {
		case lockedDownQty > 0:
			return nil, fmt.Errorf("that blueprint is locked down by your corporation")
		case lockedBlueprints > 0:
			return nil, fmt.Errorf("researched blueprints and blueprint copies cannot be sold on the market — trade them directly")
		case assembled > 0:
			return nil, fmt.Errorf("assembled items must be repackaged before they can be sold")
		}
		return nil, fmt.Errorf("only %d of %d units available", have, quantity)
	}
	var deltas []ItemDelta
	need := quantity
	for i := range stacks {
		if need == 0 {
			break
		}
		st := stacks[i]
		if st.unsellable {
			continue
		}
		take := st.row.Quantity
		if take > need {
			take = need
		}
		row := st.row
		if take == st.row.Quantity {
			if _, err := tx.ExecContext(ctx, `DELETE FROM entity WHERE itemID = ?`, st.row.ItemID); err != nil {
				return nil, err
			}
			if st.blueprint {
				// entity has no foreign key onto invBlueprints; a child row left behind is
				// inherited by the next item allocated that itemID.
				if _, err := tx.ExecContext(ctx,
					`DELETE FROM invBlueprints WHERE blueprintID = ?`, st.row.ItemID); err != nil {
					return nil, err
				}
			}
			deltas = append(deltas, ItemDelta{Kind: ItemDeltaRemoved, Row: &row})
		} else {
			if _, err := tx.ExecContext(ctx,
				`UPDATE entity SET quantity = quantity - ? WHERE itemID = ?`, take, st.row.ItemID); err != nil {
				return nil, err
			}
			row.Quantity = st.row.Quantity - take
			deltas = append(deltas, ItemDelta{Kind: ItemDeltaQty, Row: &row, OldQty: st.row.Quantity})
		}
		need -= take
	}
	return deltas, nil
}

// PlaceSellOrder executes a player sell: removes the items from the hangar,
// fills against the highest matching bids (price ≥ ask, order in range of the
// seller, minVolume satisfied), then lists any remainder when duration > 0.
// Unfilled immediate remainders are returned to the hangar.
func PlaceSellOrder(ctx context.Context, db *sql.DB, charID, stationID, typeID, itemID int64, price float64, quantity, orderRange, minVolume, duration int64, useCorp bool) (*PlaceOrderOutcome, error) {
	out, err := retryOutcome(ctx, func() (*PlaceOrderOutcome, error) {
		return placeSellOrderOnce(ctx, db, charID, stationID, typeID, itemID, price, quantity, orderRange, minVolume, duration, useCorp)
	})
	if err == nil && out != nil && len(out.Fills) > 0 && WorldEventsWanted() {
		emitSellFills(ctx, db, charID, typeID, out.Fills)
	}
	return out, err
}

// emitSellFills tells modules (worldevents.go) about each fill of a committed
// sell. Fills can land in stations other than the seller's (a ranged bid), so
// each is located on its own; one station's lookup is reused.
func emitSellFills(ctx context.Context, db *sql.DB, charID, typeID int64, fills []MarketFill) {
	now := time.Now()
	parents := map[int64]*StationParents{}
	for _, f := range fills {
		p, ok := parents[f.StationID]
		if !ok {
			p, _ = GetStationParents(ctx, db, f.StationID)
			parents[f.StationID] = p
		}
		buyer := f.CounterpartyID
		if f.CounterpartyWalletOwnerID != 0 {
			buyer = f.CounterpartyWalletOwnerID
		}
		ev := MarketSellFillEvent{At: now, SellerID: charID, BuyerOwnerID: buyer,
			BuyerIsNPCSeeder: f.IsNPCOrder, TypeID: typeID, Quantity: f.Quantity,
			Price: f.Price, StationID: f.StationID}
		if p != nil {
			ev.SolarSystemID, ev.RegionID = p.SolarSystemID, p.RegionID
		}
		EmitWorldEvent(ev)
	}
}

// placeSellOrderOnce is one attempt at PlaceSellOrder; retryOutcome reruns it on a
// lock conflict (txretry.go).
func placeSellOrderOnce(ctx context.Context, db *sql.DB, charID, stationID, typeID, itemID int64, price float64, quantity, orderRange, minVolume, duration int64, useCorp bool) (*PlaceOrderOutcome, error) {
	if quantity < 1 || price <= 0 {
		return refuse("Invalid price or quantity."), nil
	}
	parents, err := GetStationParents(ctx, db, stationID)
	if err != nil {
		return nil, err
	}
	limits, err := GetMarketSkillLimits(ctx, db, charID)
	if err != nil {
		return nil, err
	}
	brokerFactor := BrokerStandingFactor(ctx, db, charID, stationID)

	// A corp sell takes corp property out of the corp office and pays the corp
	// wallet; the pilot's own hangar is not touched.
	acct, refusal, err := ResolveMarketAccount(ctx, db, charID, stationID, useCorp)
	if err != nil {
		return nil, err
	}
	if refusal != "" {
		return refuse("%s", refusal), nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	consumed, err := consumeItems(ctx, tx, acct, typeID, itemID, quantity)
	if err != nil {
		// A lock conflict is this attempt losing a race; hand it up for the retry.
		if IsRetryableTxError(err) {
			return nil, err
		}
		return refuse("Cannot sell: %v.", err), nil
	}

	out := &PlaceOrderOutcome{HangarDeltas: consumed}
	remaining := quantity
	var balance float64
	balance, err = accountBalance(ctx, tx, acct)
	if err != nil {
		return nil, fmt.Errorf("PlaceSellOrder balance: %w", err)
	}

	for remaining > 0 {
		var bidID, bidCharID, bidVol, bidStationID, bidAccountID, bidIsCorp int64
		var bidPrice, bidEscrow float64
		err := tx.QueryRowContext(ctx, `
			SELECT o.orderID, o.charID, o.price, o.volRemaining, o.stationID, o.escrow, o.accountID, o.isCorp
			  FROM market_orders o
			  JOIN staStations st ON st.stationID = o.stationID
			 WHERE o.regionID = ? AND o.typeID = ? AND o.bid = 1 AND o.orderState = 1
			   AND o.volRemaining > 0 AND o.price >= ? - 0.005 AND o.charID <> ?
			   AND o.minVolume <= ?
			   AND `+orderReachesSeller+`
			 ORDER BY o.price DESC, o.issued ASC
			 LIMIT 1 FOR UPDATE`,
			parents.RegionID, typeID, price, charID, remaining,
			stationID, parents.SolarSystemID, parents.ConstellationID,
		).Scan(&bidID, &bidCharID, &bidPrice, &bidVol, &bidStationID, &bidEscrow, &bidAccountID, &bidIsCorp)
		// A seeder bid is counterpartied by its NPC corporation.
		bidCounterpartyID := bidCharID
		if IsNPCSeeder(bidCharID) && bidAccountID != 0 {
			bidCounterpartyID = bidAccountID
		}
		if errors.Is(err, sql.ErrNoRows) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("PlaceSellOrder match: %w", err)
		}

		fill := remaining
		if bidVol < fill {
			fill = bidVol
		}
		gross := bidPrice * float64(fill)

		// Buyer side: player bids pay from escrow first, wallet for any
		// margin-trading shortfall. NPC (seeder) bids conjure ISK.
		if !IsNPCSeeder(bidCharID) {
			fromEscrow := gross
			if fromEscrow > bidEscrow {
				fromEscrow = bidEscrow
			}
			shortfall := gross - fromEscrow
			if fromEscrow > 0 {
				if _, err := tx.ExecContext(ctx,
					`UPDATE market_orders SET escrow = escrow - ? WHERE orderID = ?`, fromEscrow, bidID); err != nil {
					return nil, fmt.Errorf("PlaceSellOrder escrow draw: %w", err)
				}
			}
			bidAcct := accountForOrder(ctx, tx, bidCharID, bidStationID, bidIsCorp != 0)
			var buyerBalance float64
			var buyerBalanceChanged bool
			if shortfall > 0 {
				if newBal, okc, err := adjustAccount(ctx, tx, bidAcct, -shortfall); err != nil {
					return nil, fmt.Errorf("PlaceSellOrder buyer debit: %w", err)
				} else if okc {
					if err := journal(ctx, tx, bidAcct.OwnerID, bidAcct.OwnerID, acct.OwnerID, -shortfall, newBal, "Market purchase (margin)"); err != nil {
						return nil, err
					}
					buyerBalance, buyerBalanceChanged = newBal, true
				}
			}
			// Deliver to the buyer at the bid's station, into their corp office when the
			// bid was a corp order.
			bidDeltas, err := deliverItem(ctx, tx, bidAcct, typeID, fill)
			if err != nil {
				return nil, err
			}
			if err := recordTransaction(ctx, tx, bidCharID, charID, typeID, fill, bidPrice, 1, parents.RegionID, bidStationID); err != nil {
				return nil, err
			}
			out.Fills = append(out.Fills, MarketFill{
				CounterpartyID: bidCharID, Price: bidPrice, Quantity: fill,
				StationID: bidStationID, ItemEntityIDs: deltaItemIDs(bidDeltas),
				ItemDeltas:                 bidDeltas,
				CounterpartyBalance:        buyerBalance,
				CounterpartyWalletOwnerID:  bidAcct.OwnerID,
				CounterpartyBalanceChanged: buyerBalanceChanged,
			})
		} else {
			out.Fills = append(out.Fills, MarketFill{
				CounterpartyID: bidCounterpartyID, Price: bidPrice, Quantity: fill, StationID: bidStationID,
				IsNPCOrder: true,
			})
		}

		// Seller proceeds minus sales tax.
		tax := gross * limits.TaxRate
		proceeds := gross - tax
		balance += proceeds
		if _, _, err := adjustAccount(ctx, tx, acct, proceeds); err != nil {
			return nil, fmt.Errorf("PlaceSellOrder credit: %w", err)
		}
		if err := journal(ctx, tx, acct.OwnerID, bidCounterpartyID, acct.OwnerID, gross, balance+tax, "Market sale"); err != nil {
			return nil, err
		}
		if tax > 0 {
			if err := journalRef(ctx, tx, RefTypeTransactionTax, acct.OwnerID, acct.OwnerID, stationID, -tax, balance, "Sales tax"); err != nil {
				return nil, err
			}
		}
		if err := recordTransaction(ctx, tx, charID, bidCounterpartyID, typeID, fill, bidPrice, 0, parents.RegionID, bidStationID); err != nil {
			return nil, err
		}

		// Consume the bid.
		if bidVol-fill <= 0 {
			_, err = tx.ExecContext(ctx,
				`UPDATE market_orders SET volRemaining = 0, orderState = 3 WHERE orderID = ?`, bidID)
		} else {
			_, err = tx.ExecContext(ctx,
				`UPDATE market_orders SET volRemaining = volRemaining - ? WHERE orderID = ?`, fill, bidID)
		}
		if err != nil {
			return nil, fmt.Errorf("PlaceSellOrder consume bid: %w", err)
		}

		// Selling into an NPC bid moves its price and its corporation's sibling bids
		// (ApplyNPCBuyImpact), inside this transaction. See docs/market-notes.md#npc-impact.
		if bidCharID == SeederCharID {
			if err := ApplyNPCBuyImpact(ctx, tx, bidID, typeID, bidAccountID, bidVol, fill); err != nil {
				return nil, err
			}
			// RestockNPCResale (off by default) shelves what the corporation bought, in the
			// same transaction as the impact: the two are one act.
			if err := RestockNPCResale(ctx, tx, bidStationID, parents.SolarSystemID,
				parents.RegionID, typeID, bidAccountID, bidPrice, fill); err != nil {
				return nil, err
			}
		}

		out.FilledQty += fill
		remaining -= fill
	}

	if remaining > 0 {
		if duration > 0 {
			open, err := countOpenOrders(ctx, tx, charID)
			if err != nil {
				return nil, err
			}
			if open >= limits.MaxOrders && len(out.Fills) == 0 {
				return refuseKey("MarketExceededOrderCount", mktOrderCap(open, limits.MaxOrders),
					"You cannot have more than %d open market orders with your current skills.", limits.MaxOrders), nil
			}
			if open < limits.MaxOrders {
				fee := limits.FeeRate * brokerFactor * price * float64(remaining)
				if fee < mktMinimumFee {
					fee = mktMinimumFee
				}
				if balance < fee && len(out.Fills) == 0 {
					return refuseKey("MktNotEnoughCash", mktNotEnoughCash(fee, fee, balance),
						"You do not have enough ISK for the broker fee (%.2f).", fee), nil
				}
				if balance >= fee {
					balance -= fee
					if _, _, err := adjustAccount(ctx, tx, acct, -fee); err != nil {
						return nil, fmt.Errorf("PlaceSellOrder fee debit: %w", err)
					}
					if err := journalRef(ctx, tx, RefTypeBrokerFee, acct.OwnerID, acct.OwnerID, stationID, -fee, balance, "Broker fee"); err != nil {
						return nil, err
					}
					if minVolume < 1 {
						minVolume = 1
					}
					isCorp := 0
					if useCorp {
						isCorp = 1
					}
					res, err := tx.ExecContext(ctx, `
						INSERT INTO market_orders
						  (typeID, charID, regionID, stationID, `+"`range`"+`, bid, price,
						   volEntered, volRemaining, issued, orderState, minVolume, contraband,
						   accountID, duration, isCorp, solarSystemID, escrow)
						VALUES (?, ?, ?, ?, ?, 0, ?, ?, ?, ?, 1, ?, 0, ?, ?, ?, ?, 0)`,
						typeID, charID, parents.RegionID, stationID, orderRange, price,
						remaining, remaining, win32TimeNow(), minVolume,
						charID, duration, isCorp, parents.SolarSystemID)
					if err != nil {
						return nil, fmt.Errorf("PlaceSellOrder insert ask: %w", err)
					}
					out.RestingOrderID, _ = res.LastInsertId()
					remaining = 0
				}
			}
		}
		if remaining > 0 {
			// Immediate order (or refused listing) leftover: hand the items back.
			if len(out.Fills) == 0 && duration == 0 {
				return refuseKey("MktOrderDidNotMatch", nil,
			"No buy orders match your ask — immediate sell orders require a matching buyer."), nil
			}
			returned, err := deliverItem(ctx, tx, acct, typeID, remaining)
			if err != nil {
				return nil, err
			}
			// Handed-back items are entity changes the client has not seen: a new row, or a
			// merge back into a stack it thinks is smaller.
			out.HangarDeltas = append(out.HangarDeltas, returned...)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	out.NewBalance = balance
	out.BalanceChanged = true
	return out, nil
}

// ── Cancel / Modify ───────────────────────────────────────────────────────────

// CancelOutcome reports what a cancellation refunded or returned.
type CancelOutcome struct {
	Bid            bool
	TypeID         int64
	NewBalance     float64
	BalanceChanged bool
	ErrorMsg       string
	// HangarDeltas are the entity changes made returning an unsold ask to its
	// hangar; the client must be pushed them, as on the sell path.
	HangarDeltas []ItemDelta
}

// CancelMarketOrder cancels charID's open order: bids refund remaining
// escrow, asks return the unsold items to the hangar at the order's station.
func CancelMarketOrder(ctx context.Context, db *sql.DB, charID, orderID int64) (*CancelOutcome, error) {
	return retryOutcome(ctx, func() (*CancelOutcome, error) {
		return cancelMarketOrderOnce(ctx, db, charID, orderID)
	})
}

// cancelMarketOrderOnce is one attempt at CancelMarketOrder; retryOutcome reruns
// it on a lock conflict (txretry.go).
func cancelMarketOrderOnce(ctx context.Context, db *sql.DB, charID, orderID int64) (*CancelOutcome, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var ownerID, typeID, stationID, volRemaining, bidV, isCorp int64
	var escrow float64
	err = tx.QueryRowContext(ctx, `
		SELECT charID, typeID, stationID, volRemaining, bid, escrow, isCorp
		  FROM market_orders WHERE orderID = ? AND orderState = 1 FOR UPDATE`,
		orderID).Scan(&ownerID, &typeID, &stationID, &volRemaining, &bidV, &escrow, &isCorp)
	if errors.Is(err, sql.ErrNoRows) {
		return &CancelOutcome{ErrorMsg: "Order not found or already closed."}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("CancelMarketOrder lookup: %w", err)
	}
	// A corp order belongs to the corporation, so any of its traders may pull it.
	if ownerID != charID && !(isCorp != 0 && corpTraderOfSameCorp(ctx, tx, charID, ownerID)) {
		return &CancelOutcome{ErrorMsg: "That order does not belong to you."}, nil
	}

	acct := accountForOrder(ctx, tx, ownerID, stationID, isCorp != 0)
	out := &CancelOutcome{Bid: bidV != 0, TypeID: typeID}
	if bidV != 0 {
		if escrow > 0 {
			newBal, okc, err := adjustAccount(ctx, tx, acct, escrow)
			if err != nil {
				return nil, fmt.Errorf("CancelMarketOrder refund: %w", err)
			}
			if okc {
				if err := journalRef(ctx, tx, RefTypeMarketEscrow, acct.OwnerID, acct.OwnerID, stationID, escrow, newBal, "Market escrow refund"); err != nil {
					return nil, err
				}
				// Only the caller's own wallet display is refreshed from this;
				// a corp refund shows up in the corp wallet on its next read.
				out.NewBalance, out.BalanceChanged = newBal, !acct.IsCorp
			}
		}
	} else if volRemaining > 0 {
		returned, err := deliverItem(ctx, tx, acct, typeID, volRemaining)
		if err != nil {
			return nil, err
		}
		out.HangarDeltas = returned
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM market_orders WHERE orderID = ?`, orderID); err != nil {
		return nil, fmt.Errorf("CancelMarketOrder delete: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// corpTraderOfSameCorp reports whether charID may act on another member's corp
// order: same corporation, and CEO or a trading role.
func corpTraderOfSameCorp(ctx context.Context, tx *sql.Tx, charID, ownerID int64) bool {
	var mine, theirs int64
	if err := tx.QueryRowContext(ctx,
		`SELECT corporationID FROM character_ WHERE characterID = ?`, charID).Scan(&mine); err != nil {
		return false
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT corporationID FROM character_ WHERE characterID = ?`, ownerID).Scan(&theirs); err != nil {
		return false
	}
	if mine != theirs || mine < MinPlayerEntityID {
		return false
	}
	var ceoID int64
	if err := tx.QueryRowContext(ctx,
		`SELECT ceoID FROM corporation WHERE corporationID = ?`, mine).Scan(&ceoID); err == nil && ceoID == charID {
		return true
	}
	info := &CorpMemberInfo{}
	if err := tx.QueryRowContext(ctx,
		`SELECT corprole FROM chrCorporationRoles WHERE characterID = ?`, charID).Scan(&info.Corprole); err != nil {
		return false
	}
	// A trade role held through a title counts the same as one on the member row.
	if err := AddTitleRoles(ctx, tx, charID, info); err != nil {
		return false
	}
	return info.Corprole&corpTradeRoles != 0
}

// ModifyOutcome reports a price modification.
type ModifyOutcome struct {
	NewBalance     float64
	BalanceChanged bool
	ErrorMsg       string
	// TypeID is what the order trades in; the client invalidates its cached order
	// book per type, so a push without it invalidates nothing.
	TypeID int64
}

// ModifyMarketOrder changes the price of charID's open order. Price
// increases pay a broker fee on the delta (client shows the same math);
// bid orders also adjust escrow by the delta.
func ModifyMarketOrder(ctx context.Context, db *sql.DB, charID, orderID int64, newPrice float64) (*ModifyOutcome, error) {
	return retryOutcome(ctx, func() (*ModifyOutcome, error) {
		return modifyMarketOrderOnce(ctx, db, charID, orderID, newPrice)
	})
}

// modifyMarketOrderOnce is one attempt at ModifyMarketOrder; retryOutcome reruns
// it on a lock conflict (txretry.go).
func modifyMarketOrderOnce(ctx context.Context, db *sql.DB, charID, orderID int64, newPrice float64) (*ModifyOutcome, error) {
	if newPrice <= 0 {
		return &ModifyOutcome{ErrorMsg: "Invalid price."}, nil
	}
	limits, err := GetMarketSkillLimits(ctx, db, charID)
	if err != nil {
		return nil, err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var ownerID, volRemaining, bidV, stationID, isCorp, typeID int64
	var oldPrice, escrow float64
	err = tx.QueryRowContext(ctx, `
		SELECT charID, price, volRemaining, bid, escrow, stationID, isCorp, typeID
		  FROM market_orders WHERE orderID = ? AND orderState = 1 FOR UPDATE`,
		orderID).Scan(&ownerID, &oldPrice, &volRemaining, &bidV, &escrow, &stationID, &isCorp, &typeID)
	if errors.Is(err, sql.ErrNoRows) {
		return &ModifyOutcome{ErrorMsg: "Order not found or already closed."}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ModifyMarketOrder lookup: %w", err)
	}
	if ownerID != charID && !(isCorp != 0 && corpTraderOfSameCorp(ctx, tx, charID, ownerID)) {
		return &ModifyOutcome{ErrorMsg: "That order does not belong to you."}, nil
	}
	acct := accountForOrder(ctx, tx, ownerID, stationID, isCorp != 0)

	fee := mktMinimumFee
	if newPrice > oldPrice {
		// tx, not db: the station is only known from the order row this
		// transaction has already locked.
		f := (newPrice - oldPrice) * float64(volRemaining) * limits.FeeRate *
			BrokerStandingFactor(ctx, tx, charID, stationID)
		if f > fee {
			fee = f
		}
	}
	escrowDelta := 0.0
	if bidV != 0 {
		escrowDelta = (newPrice - oldPrice) * float64(volRemaining) * limits.EscrowFactor
	}

	balance, err := accountBalance(ctx, tx, acct)
	if err != nil {
		return nil, fmt.Errorf("ModifyMarketOrder balance: %w", err)
	}
	if balance < fee+escrowDelta {
		return &ModifyOutcome{ErrorMsg: fmt.Sprintf(
			"You need %.2f ISK to modify this order (fee %.2f, escrow change %.2f).",
			fee+escrowDelta, fee, escrowDelta)}, nil
	}

	balance -= fee + escrowDelta
	if _, _, err := adjustAccount(ctx, tx, acct, -(fee + escrowDelta)); err != nil {
		return nil, fmt.Errorf("ModifyMarketOrder debit: %w", err)
	}
	if err := journalRef(ctx, tx, RefTypeBrokerFee, acct.OwnerID, acct.OwnerID, stationID, -fee, balance+escrowDelta, "Broker fee (modify)"); err != nil {
		return nil, err
	}
	if escrowDelta != 0 {
		if err := journalRef(ctx, tx, RefTypeMarketEscrow, acct.OwnerID, acct.OwnerID, stationID, -escrowDelta, balance, "Market escrow adjustment"); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE market_orders SET price = ?, escrow = escrow + ?, issued = ? WHERE orderID = ?`,
		newPrice, escrowDelta, win32TimeNow(), orderID); err != nil {
		return nil, fmt.Errorf("ModifyMarketOrder update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	// A corp order's fee left the corp wallet, so the caller's own displayed
	// balance did not change.
	return &ModifyOutcome{NewBalance: balance, BalanceChanged: !acct.IsCorp, TypeID: typeID}, nil
}

// ── Corporation orders ────────────────────────────────────────────────────────

// GetCorporationOrders returns open orders flagged isCorp placed by members
// of corporationID, in the same rowset shape as GetCharOrders.
func GetCorporationOrders(ctx context.Context, db *sql.DB, corporationID int64) (marshal.PyRep, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT o.orderID, o.typeID, o.charID, o.regionID, o.stationID,
		       o.`+"`range`"+`, o.bid, o.price, o.volEntered, o.volRemaining,
		       o.issued, o.orderState, o.minVolume, o.contraband,
		       o.accountID, o.duration, o.isCorp, o.solarSystemID, o.escrow
		  FROM market_orders o
		  JOIN character_ ch ON ch.characterID = o.charID
		 WHERE ch.corporationID = ? AND o.isCorp = 1 AND o.orderState = 1`,
		corporationID)
	if err != nil {
		return makeRowset(charOrderCols, nil), nil
	}
	defer rows.Close()
	return scanCharOrderRows(rows)
}

// ── Expiry ────────────────────────────────────────────────────────────────────

// ExpiredOrder identifies an order removed by the expiry sweep.
type ExpiredOrder struct {
	OrderID int64
	CharID  int64
	TypeID  int64
	Bid     bool
}

// ExpireMarketOrders removes player orders past issued + duration days,
// refunding bid escrow and returning unsold ask items. Returns the expired
// orders so callers can notify online owners.
func ExpireMarketOrders(ctx context.Context, db *sql.DB) ([]ExpiredOrder, error) {
	now := int64(win32TimeNow())
	rows, err := db.QueryContext(ctx, `
		SELECT orderID, charID, typeID, bid
		  FROM market_orders
		 WHERE charID NOT IN (?, ?) AND orderState = 1 AND duration > 0
		   AND issued + duration * ? < ?`,
		SeederCharID, AdminSeederCharID, win32TicksPerDay, now)
	if err != nil {
		return nil, fmt.Errorf("ExpireMarketOrders query: %w", err)
	}
	var expired []ExpiredOrder
	for rows.Next() {
		var e ExpiredOrder
		var bidV int64
		if err := rows.Scan(&e.OrderID, &e.CharID, &e.TypeID, &bidV); err != nil {
			rows.Close()
			return nil, err
		}
		e.Bid = bidV != 0
		expired = append(expired, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, e := range expired {
		// CancelMarketOrder already does refund/return + delete atomically.
		if _, err := CancelMarketOrder(ctx, db, e.CharID, e.OrderID); err != nil {
			log.Printf("ExpireMarketOrders order %d: %v", e.OrderID, err)
		}
	}
	return expired, nil
}
