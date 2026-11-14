package db

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// NPC market dynamics: each tick reprices the auto-Seeder trade-good orders from their
// fill fraction (starved bids and sold-out asks rise, filled bids and idle asks fall), within
// the security-tier bands in marketsec.go, then restocks every Seeder order and clamps each
// bid under its station's own ask. Only SeederCharID orders move; AdminSeederCharID orders
// keep their hand-set prices. See docs/market-notes.md#npc-market-dynamics.

// tradeGoodGroupList is the SQL IN-list of invGroups.groupID values treated as haulable
// trade goods: General, Frozen, Radioactive, Livestock, Biohazard, Drugs, Miscellaneous.
// Not all of categoryID 17: datacores, tags and mission commodities stay out.
const tradeGoodGroupList = "280,281,282,283,284,313,314"

// Dynamics walk tuning. Fill fraction = (volEntered-volRemaining)/volEntered over one tick.
// The per-tier caps and floors (multiples of invTypes.basePrice) live in marketsec.go.
const (
	dynLowWater  = 0.10 // fill below this = starved/stagnant
	dynHighWater = 0.50 // fill above this = hot
	dynStepUp    = 1.06 // per-tick raise
	dynStepDown  = 0.94 // per-tick cut

	// An NPC bid is capped at this fraction of the cheapest NPC ask for the same type
	// in the same station.
	sameStationBidCap = 0.95
)

// TickNPCMarketDynamics runs one simulation step: reprice trade-good Seeder
// orders from their fill rate, restock every Seeder order, and re-apply the
// same-station bid clamp. Returns (repriced, restocked, error).
func TickNPCMarketDynamics(ctx context.Context, db *sql.DB) (int64, int64, error) {
	// Reprice trade goods from fill fraction with a +-1% random walk, clamped into the
	// per-side, per-tier band around basePrice.
	repRes, err := db.ExecContext(ctx, `
		UPDATE market_orders o
		JOIN invTypes it ON it.typeID = o.typeID
		LEFT JOIN mapSolarSystems ms ON ms.solarSystemID = o.solarSystemID
		SET o.price = ROUND(LEAST(
		      COALESCE(it.basePrice, 100) * IF(o.bid = 1,
		        `+secCase(orderSecExpr, mkt().secDynBuyCap)+`,
		        `+secCase(orderSecExpr, mkt().secTradeSellCap)+`),
		      GREATEST(
		        COALESCE(it.basePrice, 100) * IF(o.bid = 1,
		          `+secCase(orderSecExpr, mkt().secDynBuyFloor)+`,
		          `+secCase(orderSecExpr, mkt().secDynSellFloor)+`),
		        o.price * (0.99 + RAND() * 0.02) * CASE
		          WHEN o.bid = 1 AND (o.volEntered - o.volRemaining) / o.volEntered < ? THEN ?
		          WHEN o.bid = 1 AND (o.volEntered - o.volRemaining) / o.volEntered > ? THEN ?
		          WHEN o.bid = 0 AND (o.volEntered - o.volRemaining) / o.volEntered > ? THEN ?
		          WHEN o.bid = 0 AND (o.volEntered - o.volRemaining) / o.volEntered < ? THEN ?
		          ELSE 1.0
		        END
		      )
		    ), 2)
		WHERE o.charID = ? AND o.orderState IN (1, 3) AND o.volEntered > 0
		  AND it.groupID IN (`+tradeGoodGroupList+`)`,
		dynLowWater, dynStepUp,
		dynHighWater, dynStepDown,
		dynHighWater, dynStepUp,
		dynLowWater, dynStepDown,
		SeederCharID,
	)
	if err != nil {
		return 0, 0, fmt.Errorf("TickNPCMarketDynamics reprice: %w", err)
	}
	repriced, _ := repRes.RowsAffected()

	// Restock every Seeder order except trade-good bids in full and revive filled ones.
	restRes, err := db.ExecContext(ctx, `
		UPDATE market_orders o
		JOIN invTypes it ON it.typeID = o.typeID
		SET o.volRemaining = o.volEntered, o.orderState = 1
		WHERE o.charID = ? AND o.orderState IN (1, 3)
		  AND (o.volRemaining < o.volEntered OR o.orderState = 3)
		  AND NOT (o.bid = 1 AND it.groupID IN (`+tradeGoodGroupList+`))`,
		SeederCharID,
	)
	if err != nil {
		return repriced, 0, fmt.Errorf("TickNPCMarketDynamics restock: %w", err)
	}
	restocked, _ := restRes.RowsAffected()

	// Trade-good bids restock against the daily budget.
	tg, err := restockTradeGoodBids(ctx, db)
	if err != nil {
		return repriced, restocked, err
	}
	restocked += tg

	// Heal bids walked down by ApplyNPCBuyImpact and asks driven up by ApplyNPCSellImpact.
	// Runs before the same-station clamp so a recovered bid is still capped under the local ask.
	recovered, err := RecoverNPCPrices(ctx, db)
	if err != nil {
		return repriced, restocked, err
	}
	repriced += recovered

	if err := ClampNPCBidsBelowLocalAsks(ctx, db); err != nil {
		return repriced, restocked, err
	}
	return repriced, restocked, nil
}

// The trade-good bid budget caps the bid value (price x units) one tick may restore at
// budget x elapsed / 24h; over budget every depleted bid refills the same fraction of its
// deficit. See docs/market-notes.md#trade-good-bid-budget.
const (
	tradeGoodBidBudgetSetting = "market.tradeGoodBidBudget"   // ISK/day; 0 = unbudgeted
	tradeGoodBudgetAtSetting  = "market.tradeGoodBidBudgetAt" // unix seconds of the last tick

	// defaultTradeGoodBidBudget is 40B ISK/day of bid value.
	defaultTradeGoodBidBudget = 40e9
	tradeGoodBudgetMaxElapsed = 3 * 3600
	tradeGoodBudgetDefElapsed = 3600
)

// tradeGoodRestockFraction is the share of each bid's deficit one tick may
// refill: 1 when the allowance covers the whole deficit (or there is none).
func tradeGoodRestockFraction(allowance, deficit float64) float64 {
	if deficit <= 0 || allowance >= deficit {
		return 1
	}
	if allowance <= 0 {
		return 0
	}
	return allowance / deficit
}

// tradeGoodAllowance is the bid value one tick may restore: budget per day
// scaled by the elapsed seconds, clamped. budget <= 0 means unbudgeted (-1).
func tradeGoodAllowance(budget float64, elapsed int64) float64 {
	if budget <= 0 {
		return -1
	}
	if elapsed <= 0 {
		elapsed = tradeGoodBudgetDefElapsed
	}
	if elapsed > tradeGoodBudgetMaxElapsed {
		elapsed = tradeGoodBudgetMaxElapsed
	}
	return budget * float64(elapsed) / 86400
}

// restockTradeGoodBids refills auto-Seeder trade-good bids under the daily
// budget. Returns rows touched.
func restockTradeGoodBids(ctx context.Context, db *sql.DB) (int64, error) {
	budget := settingFloat(ctx, db, tradeGoodBidBudgetSetting, defaultTradeGoodBidBudget, 0, 1e13)
	now := time.Now().Unix()
	var elapsed int64
	if v, ok, err := GetServerSetting(ctx, db, tradeGoodBudgetAtSetting); err == nil && ok {
		if last, perr := strconv.ParseInt(strings.TrimSpace(v), 10, 64); perr == nil && last > 0 {
			elapsed = now - last
		}
	}
	if err := SetServerSetting(ctx, db, tradeGoodBudgetAtSetting, strconv.FormatInt(now, 10)); err != nil {
		return 0, fmt.Errorf("restockTradeGoodBids stamp: %w", err)
	}

	allowance := tradeGoodAllowance(budget, elapsed)
	frac := 1.0
	if allowance >= 0 {
		var deficit sql.NullFloat64
		if err := db.QueryRowContext(ctx, `
			SELECT SUM(o.price * (o.volEntered - o.volRemaining))
			  FROM market_orders o
			  JOIN invTypes it ON it.typeID = o.typeID
			 WHERE o.charID = ? AND o.bid = 1 AND o.orderState IN (1, 3)
			   AND o.volRemaining < o.volEntered
			   AND it.groupID IN (`+tradeGoodGroupList+`)`,
			SeederCharID,
		).Scan(&deficit); err != nil {
			return 0, fmt.Errorf("restockTradeGoodBids deficit: %w", err)
		}
		frac = tradeGoodRestockFraction(allowance, deficit.Float64)
	}
	if frac <= 0 {
		return 0, nil
	}

	// Single-table UPDATE with a subquery filter: MariaDB evaluates its SET left to right,
	// so the orderState test reads the refilled volRemaining. A multi-table UPDATE has no order.
	res, err := db.ExecContext(ctx, `
		UPDATE market_orders
		SET volRemaining = LEAST(volEntered,
		      volRemaining + FLOOR((volEntered - volRemaining) * ?)),
		    orderState = IF(volRemaining > 0, 1, orderState)
		WHERE charID = ? AND bid = 1 AND orderState IN (1, 3)
		  AND (volRemaining < volEntered OR orderState = 3)
		  AND typeID IN (SELECT typeID FROM invTypes
		                  WHERE groupID IN (`+tradeGoodGroupList+`))`,
		frac, SeederCharID,
	)
	if err != nil {
		return 0, fmt.Errorf("restockTradeGoodBids: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ClampNPCBidsBelowLocalAsks caps every auto-Seeder bid at sameStationBidCap x the
// cheapest NPC ask for the same type in the same station. Asks from both seeder identities
// count; only auto-Seeder bids move.
func ClampNPCBidsBelowLocalAsks(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		UPDATE market_orders b
		JOIN (SELECT stationID, typeID, MIN(price) AS minAsk
		        FROM market_orders
		       WHERE charID IN (?, ?) AND bid = 0 AND orderState = 1
		       GROUP BY stationID, typeID) s
		  ON s.stationID = b.stationID AND s.typeID = b.typeID
		SET b.price = GREATEST(0.01, ROUND(s.minAsk * ?, 2))
		WHERE b.charID = ? AND b.bid = 1 AND b.orderState = 1
		  AND b.price > s.minAsk * ?`,
		SeederCharID, AdminSeederCharID,
		sameStationBidCap,
		SeederCharID, sameStationBidCap,
	)
	if err != nil {
		return fmt.Errorf("ClampNPCBidsBelowLocalAsks: %w", err)
	}
	return nil
}
