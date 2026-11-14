package db

// Item valuation for the killboard: what a destroyed hull and everything on it was worth, off
// the live market with basePrice as the floor. It prices anything at cost; SkillMarketPrices
// (skillpurchasedb.go) has the same precedence but is skill-only and applies the shop markup.
// RecordKillmail is the only write-path caller and stores the result on the killmail row; the
// boards read stored numbers. See docs/db-11-notes.md#killmail-item-values.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"
)

// TypeValueSource names where a value came from, for display and for deciding
// how much to trust it.
const (
	TypeValueFromOrders = "sell orders"
	TypeValueFromTrades = "recent trades"
	TypeValueFromBase   = "base price"
	TypeValueUnpriced   = "unpriced"
)

// TypeValue is one item type's ISK value per unit.
type TypeValue struct {
	TypeID int64
	Value  float64
	Source string // TypeValueFrom* / TypeValueUnpriced
}

// typeValueTradeWindow is how far back TypeValues looks for transactions when
// nothing is on sale: 30 days in 100-ns Windows ticks.  Same window
// skillTradeWindow uses, kept separate so tuning one does not move the other.
const typeValueTradeWindow = int64(30) * 864000000000

const typeValueTTL = 60 * time.Second

var (
	typeValueMu    sync.Mutex
	typeValueCache = map[int64]typeValueEntry{}
)

type typeValueEntry struct {
	val TypeValue
	at  time.Time
}

// InvalidateTypeValues drops every cached value. For bulk market moves (NPC reseed, admin price
// edit); the TTL covers ordinary trading.
func InvalidateTypeValues() {
	typeValueMu.Lock()
	typeValueCache = map[int64]typeValueEntry{}
	typeValueMu.Unlock()
}

// TypeValues prices a set of types in one pass: the VWAP of live sell orders wins, else the VWAP
// of the trailing 30 days of buy transactions, else invTypes.basePrice (the insurance base). A
// type with none of the three comes back Value 0 / TypeValueUnpriced. Cached per type for
// typeValueTTL; only uncached types are queried. No types in gives an empty map, not an error.
func TypeValues(ctx context.Context, db *sql.DB, typeIDs []int64) (map[int64]TypeValue, error) {
	out := make(map[int64]TypeValue, len(typeIDs))
	var missing []int64

	typeValueMu.Lock()
	now := time.Now()
	seen := make(map[int64]bool, len(typeIDs))
	for _, id := range typeIDs {
		if id == 0 || seen[id] {
			continue
		}
		seen[id] = true
		if e, ok := typeValueCache[id]; ok && now.Sub(e.at) < typeValueTTL {
			out[id] = e.val
			continue
		}
		missing = append(missing, id)
	}
	typeValueMu.Unlock()

	if len(missing) == 0 {
		return out, nil
	}

	fresh, err := typeValuesUncached(ctx, db, missing)
	if err != nil {
		// Serve stale rather than fail the caller: a killmail priced from the previous minute beats a
		// hole in the record.
		typeValueMu.Lock()
		for _, id := range missing {
			if e, ok := typeValueCache[id]; ok {
				out[id] = e.val
			}
		}
		typeValueMu.Unlock()
		if len(out) > 0 {
			return out, nil
		}
		return nil, err
	}

	typeValueMu.Lock()
	stamp := time.Now()
	for id, v := range fresh {
		typeValueCache[id] = typeValueEntry{val: v, at: stamp}
		out[id] = v
	}
	typeValueMu.Unlock()
	return out, nil
}

// TypeValue prices a single type. Convenience over TypeValues; the plural form
// is the one to use when more than one type is in hand.
func TypeValueOf(ctx context.Context, db *sql.DB, typeID int64) TypeValue {
	vals, err := TypeValues(ctx, db, []int64{typeID})
	if err != nil {
		return TypeValue{TypeID: typeID, Source: TypeValueUnpriced}
	}
	if v, ok := vals[typeID]; ok {
		return v
	}
	return TypeValue{TypeID: typeID, Source: TypeValueUnpriced}
}

func typeValuesUncached(ctx context.Context, db *sql.DB, typeIDs []int64) (map[int64]TypeValue, error) {
	out := make(map[int64]TypeValue, len(typeIDs))
	// Every requested type gets an entry, even unpriced, so a repeat request is a cache hit.
	for _, id := range typeIDs {
		out[id] = TypeValue{TypeID: id, Source: TypeValueUnpriced}
	}

	ph := strings.TrimSuffix(strings.Repeat("?,", len(typeIDs)), ",")
	args := make([]any, 0, len(typeIDs)+1)
	for _, id := range typeIDs {
		args = append(args, id)
	}

	// Base prices, the floor.
	rows, err := db.QueryContext(ctx,
		`SELECT typeID, COALESCE(basePrice, 0) FROM invTypes WHERE typeID IN (`+ph+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("TypeValues base: %w", err)
	}
	for rows.Next() {
		var id int64
		var base float64
		if err := rows.Scan(&id, &base); err != nil {
			rows.Close()
			return nil, fmt.Errorf("TypeValues base scan: %w", err)
		}
		if base > 0 {
			out[id] = TypeValue{TypeID: id, Value: base, Source: TypeValueFromBase}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("TypeValues base rows: %w", err)
	}

	// Recent buy transactions (transactionType 1), better than basePrice.
	rows, err = db.QueryContext(ctx, `
		SELECT typeID, SUM(price * quantity) / SUM(quantity)
		  FROM market_transactions
		 WHERE transactionType = 1 AND quantity > 0
		   AND transactionDateTime >= ?
		   AND typeID IN (`+ph+`)
		 GROUP BY typeID`, append([]any{win32TimeNow() - uint64(typeValueTradeWindow)}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("TypeValues trades: %w", err)
	}
	for rows.Next() {
		var id int64
		var avg float64
		if err := rows.Scan(&id, &avg); err != nil {
			rows.Close()
			return nil, fmt.Errorf("TypeValues trades scan: %w", err)
		}
		if avg > 0 {
			out[id] = TypeValue{TypeID: id, Value: avg, Source: TypeValueFromTrades}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("TypeValues trades rows: %w", err)
	}

	// Active sell orders: the live listed price, top precedence.
	rows, err = db.QueryContext(ctx, `
		SELECT typeID, SUM(price * volRemaining) / SUM(volRemaining)
		  FROM market_orders
		 WHERE bid = 0 AND volRemaining > 0 AND orderState = 1
		   AND typeID IN (`+ph+`)
		 GROUP BY typeID`, args...)
	if err != nil {
		return nil, fmt.Errorf("TypeValues orders: %w", err)
	}
	for rows.Next() {
		var id int64
		var avg float64
		if err := rows.Scan(&id, &avg); err != nil {
			rows.Close()
			return nil, fmt.Errorf("TypeValues orders scan: %w", err)
		}
		if avg > 0 {
			out[id] = TypeValue{TypeID: id, Value: avg, Source: TypeValueFromOrders}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("TypeValues orders rows: %w", err)
	}

	return out, nil
}
