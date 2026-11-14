package db

// Market impact: seeded NPC orders move as players trade against them (bids fall, asks
// rise) and heal toward their seeded level on the dynamics tick. Only auto-Seeder orders
// move; AdminSeederCharID orders keep their hand-set prices. Depth, floor, cap and recovery
// are the security-tier levers in marketsec.go. See docs/market-notes.md#npc-impact-bounds.

import (
	"context"
	"fmt"
)

// impactSpill is the share of the primary cut applied to the same corporation's other
// orders for the type (server_settings market.impactSpill, default 0.40, read through mkt()).

// buyImpactFloorExpr is the SQL multiple of basePrice a bid cannot be sold below, per tier.
// Trade goods floor at secDynBuyFloor, the dynamics tick's own floor; flat goods floor at a
// fraction of their seeded bid. See docs/market-notes.md#npc-impact-bounds.
func buyImpactFloorExpr() string {
	return fmt.Sprintf("IF(it.groupID IN (%s), %s, %g * %s * %s)",
		tradeGoodGroupList,
		secCase(orderSecExpr, mkt().secDynBuyFloor),
		mkt().npcBuyFactor,
		secCase(orderSecExpr, mkt().secBidMul),
		secCase(orderSecExpr, mkt().secImpactFloor))
}

// askImpactCapExpr is the multiple of basePrice an ask cannot be bought above,
// per the order's tier: secTradeSellCap for trade goods (the band the dynamics
// tick clamps them into, so the two never fight), secDynSellCap otherwise.
func askImpactCapExpr() string {
	return fmt.Sprintf("IF(it.groupID IN (%s), %s, %s)",
		tradeGoodGroupList,
		secCase(orderSecExpr, mkt().secTradeSellCap),
		secCase(orderSecExpr, mkt().secDynSellCap))
}

// ApplyNPCBuyImpact lowers the bid that took a player sale and the same corporation's other
// bids for the type. It runs inside the sale's transaction, so the next unit sees the new
// price. volBefore is the bid's volRemaining before the fill; corpID 0 skips the spillover.
func ApplyNPCBuyImpact(ctx context.Context, tx DBTX, orderID, typeID, corpID, volBefore, fill int64) error {
	if fill <= 0 || volBefore <= 0 {
		return nil
	}

	// Depth consumed, clamped to 1: an oversized fill is one full clear-out.
	consumed := float64(fill) / float64(volBefore)
	if consumed > 1 {
		consumed = 1
	}

	// The floor is anchored to basePrice, not the order's price, so repeated sales converge on
	// it. A type with no basePrice has no floor and is skipped.
	if _, err := tx.ExecContext(ctx, `
		UPDATE market_orders o
		JOIN invTypes it ON it.typeID = o.typeID
		LEFT JOIN mapSolarSystems ms ON ms.solarSystemID = o.solarSystemID
		SET o.price = GREATEST(
		      ROUND(it.basePrice * `+buyImpactFloorExpr()+`, 2),
		      ROUND(o.price * (1.0 - `+secCase(orderSecExpr, mkt().secImpactDepth)+` * ?), 2),
		      0.01)
		WHERE o.orderID = ? AND o.charID = ? AND o.bid = 1
		  AND COALESCE(it.basePrice, 0) > 0`,
		consumed, orderID, SeederCharID,
	); err != nil {
		return fmt.Errorf("ApplyNPCBuyImpact primary: %w", err)
	}

	if corpID == 0 {
		return nil
	}

	// Spillover: each sibling takes impactSpill of the cut, at its own tier's depth and floor.
	if _, err := tx.ExecContext(ctx, `
		UPDATE market_orders o
		JOIN invTypes it ON it.typeID = o.typeID
		LEFT JOIN mapSolarSystems ms ON ms.solarSystemID = o.solarSystemID
		SET o.price = GREATEST(
		      ROUND(it.basePrice * `+buyImpactFloorExpr()+`, 2),
		      ROUND(o.price * (1.0 - `+secCase(orderSecExpr, mkt().secImpactDepth)+` * ?), 2),
		      0.01)
		WHERE o.charID = ? AND o.accountID = ? AND o.typeID = ?
		  AND o.bid = 1 AND o.orderState IN (1, 3) AND o.orderID <> ?
		  AND COALESCE(it.basePrice, 0) > 0`,
		consumed*mkt().impactSpill,
		SeederCharID, corpID, typeID, orderID,
	); err != nil {
		return fmt.Errorf("ApplyNPCBuyImpact spillover: %w", err)
	}
	return nil
}

// ApplyNPCSellImpact is the ask-side mirror of ApplyNPCBuyImpact: it raises the ask a player
// bought from and the corporation's other asks, up to askImpactCapExpr but never below the
// order's current price. See docs/market-notes.md#npc-impact-bounds.
func ApplyNPCSellImpact(ctx context.Context, tx DBTX, orderID, typeID, corpID, volBefore, fill int64) error {
	if fill <= 0 || volBefore <= 0 {
		return nil
	}

	// Depth consumed, clamped to 1: an oversized fill is one full clear-out.
	consumed := float64(fill) / float64(volBefore)
	if consumed > 1 {
		consumed = 1
	}

	// A type with no basePrice has no cap and is skipped.
	if _, err := tx.ExecContext(ctx, `
		UPDATE market_orders o
		JOIN invTypes it ON it.typeID = o.typeID
		LEFT JOIN mapSolarSystems ms ON ms.solarSystemID = o.solarSystemID
		SET o.price = LEAST(
		      GREATEST(ROUND(it.basePrice * `+askImpactCapExpr()+`, 2), o.price),
		      ROUND(o.price * (1.0 + `+secCase(orderSecExpr, mkt().secImpactDepth)+` * ?), 2))
		WHERE o.orderID = ? AND o.charID IN (?, ?) AND o.bid = 0
		  AND COALESCE(it.basePrice, 0) > 0`,
		consumed, orderID, SeederCharID, ResaleSeederCharID,
	); err != nil {
		return fmt.Errorf("ApplyNPCSellImpact primary: %w", err)
	}

	if corpID == 0 {
		return nil
	}

	// Spillover: each sibling rises by impactSpill of the cut, at its own tier's depth and cap.
	if _, err := tx.ExecContext(ctx, `
		UPDATE market_orders o
		JOIN invTypes it ON it.typeID = o.typeID
		LEFT JOIN mapSolarSystems ms ON ms.solarSystemID = o.solarSystemID
		SET o.price = LEAST(
		      GREATEST(ROUND(it.basePrice * `+askImpactCapExpr()+`, 2), o.price),
		      ROUND(o.price * (1.0 + `+secCase(orderSecExpr, mkt().secImpactDepth)+` * ?), 2))
		WHERE o.charID = ? AND o.accountID = ? AND o.typeID = ?
		  AND o.bid = 0 AND o.orderState IN (1, 3) AND o.orderID <> ?
		  AND COALESCE(it.basePrice, 0) > 0`,
		consumed*mkt().impactSpill,
		SeederCharID, corpID, typeID, orderID,
	); err != nil {
		return fmt.Errorf("ApplyNPCSellImpact spillover: %w", err)
	}
	return nil
}

// RecoverNPCPrices runs both recovery passes and returns the combined row count. Called by
// TickNPCMarketDynamics.
func RecoverNPCPrices(ctx context.Context, tx DBTX) (int64, error) {
	bids, err := RecoverNPCBuyPrices(ctx, tx)
	if err != nil {
		return bids, err
	}
	asks, err := RecoverNPCSellPrices(ctx, tx)
	return bids + asks, err
}

// RecoverNPCSellPrices lowers auto-Seeder asks above basePrice by the tier's recovery share
// of the gap. Trade goods are skipped: TickNPCMarketDynamics reprices them in both directions.
func RecoverNPCSellPrices(ctx context.Context, tx DBTX) (int64, error) {
	res, err := tx.ExecContext(ctx, `
		UPDATE market_orders o
		JOIN invTypes it ON it.typeID = o.typeID
		LEFT JOIN mapSolarSystems ms ON ms.solarSystemID = o.solarSystemID
		SET o.price = GREATEST(0.01, ROUND(o.price
		      - (o.price - it.basePrice) * `+secCase(orderSecExpr, mkt().secImpactRecovery)+`, 2))
		WHERE o.charID = ? AND o.bid = 0 AND o.orderState IN (1, 3)
		  AND COALESCE(it.basePrice, 0) > 0
		  AND o.price > it.basePrice
		  AND it.groupID NOT IN (`+tradeGoodGroupList+`)`,
		SeederCharID,
	)
	if err != nil {
		return 0, fmt.Errorf("RecoverNPCSellPrices: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// RecoverNPCBuyPrices raises auto-Seeder bids below basePrice x npcBuyFactor x the tier's bid
// multiplier by the tier's recovery share of the gap. Trade goods are skipped, as on the ask
// side: their floor (secDynBuyFloor) is above this target. Called by TickNPCMarketDynamics.
func RecoverNPCBuyPrices(ctx context.Context, tx DBTX) (int64, error) {
	res, err := tx.ExecContext(ctx, `
		UPDATE market_orders o
		JOIN invTypes it ON it.typeID = o.typeID
		LEFT JOIN mapSolarSystems ms ON ms.solarSystemID = o.solarSystemID
		SET o.price = ROUND(o.price
		      + (it.basePrice * ? * `+secCase(orderSecExpr, mkt().secBidMul)+` - o.price)
		        * `+secCase(orderSecExpr, mkt().secImpactRecovery)+`, 2)
		WHERE o.charID = ? AND o.bid = 1 AND o.orderState IN (1, 3)
		  AND COALESCE(it.basePrice, 0) > 0
		  AND o.price < it.basePrice * ? * `+secCase(orderSecExpr, mkt().secBidMul)+`
		  AND it.groupID NOT IN (`+tradeGoodGroupList+`)`,
		mkt().npcBuyFactor,
		SeederCharID,
		mkt().npcBuyFactor,
	)
	if err != nil {
		return 0, fmt.Errorf("RecoverNPCBuyPrices: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
