package db

// NPC corporations resell the ore and moon goo they absorb. A seeded ore bid restocks
// every tick, so without a shelf the units sold into it leave the game. What a
// corporation buys is re-listed at the same station as its own sell order (owned by
// ResaleSeederCharID), priced cost-plus. Off by default (market.npcResale).
// See docs/db-7-notes.md#npc-resale-shelf.

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
)

// ResaleSeederCharID owns the ore an NPC corporation has put on its shelf. It is separate
// from SeederCharID because SeedNPCMarketOrders deletes every SeederCharID order on a
// reseed, which would wipe the shelf. AdminSeederCharID is exempt for the same reason.
const ResaleSeederCharID = 90000003

const (
	// settingNPCResale gates the whole file. "0" (the default) leaves an NPC ore bid a sink.
	settingNPCResale = "market.npcResale"
	// settingNPCResaleDepth caps the shelf, in multiples of the corporation's seeded bid
	// volume for that type at that station. Past the cap the ore is consumed as before.
	settingNPCResaleDepth = "market.npcResaleMaxDepth"

	// npcResaleMarkupFloor is the least a corporation adds to what it paid. It must clear
	// 1/(1 - TaxRate) so buying the ask and selling it back into the bid never pays.
	npcResaleMarkupFloor = 1.15

	// npcResaleDepthRef is the shelf depth, relative to the seeded bid volume, at which the
	// markup reaches the floor (npcResaleMarkup).
	npcResaleDepthRef = 2.0

	// npcResaleMaxDepthDefault is the shelf cap when nobody has set one.
	npcResaleMaxDepthDefault = 3.0
)

// secResaleMarkup is what a corporation adds to what it paid, per security tier. Highsec
// is lowest so the shelf clears where the refiners are; nullsec is highest because the
// corporation is often the only seller there.
var secResaleMarkup = [3]float64{1.18, 1.28, 1.40}

var (
	npcResaleOn       atomic.Bool
	npcResaleDepthPct atomic.Int64 // hundredths; 0 = the default
)

// NPCResaleEnabled reports whether absorbed ore is put back on the shelf.
func NPCResaleEnabled() bool { return npcResaleOn.Load() }

func npcResaleMaxDepth() float64 {
	if v := npcResaleDepthPct.Load(); v > 0 {
		return float64(v) / 100
	}
	return npcResaleMaxDepthDefault
}

// LoadNPCResaleSettings reads the dials at boot. Never fatal: an unreadable
// dial leaves the shipped behaviour, which is off.
func LoadNPCResaleSettings(ctx context.Context, db *sql.DB) {
	if v, ok, err := GetServerSetting(ctx, db, settingNPCResale); err == nil && ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			npcResaleOn.Store(true)
		}
	}
	if v, ok, err := GetServerSetting(ctx, db, settingNPCResaleDepth); err == nil && ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f > 0 {
			npcResaleDepthPct.Store(int64(f * 100))
		}
	}
}

// SetNPCResale turns the shelf on or off, persistently.
func SetNPCResale(ctx context.Context, db *sql.DB, on bool) error {
	v := "0"
	if on {
		v = "1"
	}
	if err := SetServerSetting(ctx, db, settingNPCResale, v); err != nil {
		return fmt.Errorf("SetNPCResale: %w", err)
	}
	npcResaleOn.Store(on)
	return nil
}

// Moon goo groups the shelf takes beside ore: moon materials, intermediates and
// composites, which the T2 manufacturers bid on.
const (
	groupMoonMaterials = 427
	groupIntermediates = 428
	groupComposites    = 429
)

// npcResaleGroup is whether a type an NPC bid bought goes back on the shelf:
// ore (category 25) or moon goo. Pure.
func npcResaleGroup(categoryID, groupID int64) bool {
	switch {
	case categoryID == 25:
		return true
	case groupID == groupMoonMaterials, groupID == groupIntermediates, groupID == groupComposites:
		return true
	}
	return false
}

// npcResaleMarkup is the multiple a corporation adds to what it paid, given how deep its
// shelf of this type is. It falls toward the floor as the shelf deepens, so a market being
// dumped into gets cheaper. Pure: the arbitrage bound rests on this number.
func npcResaleMarkup(tier secTier, shelfUnits, seededBidUnits int64) float64 {
	top := secResaleMarkup[tierHigh]
	if int(tier) >= 0 && int(tier) < len(secResaleMarkup) {
		top = secResaleMarkup[tier]
	}
	if top < npcResaleMarkupFloor {
		top = npcResaleMarkupFloor
	}
	if seededBidUnits <= 0 || shelfUnits <= 0 {
		return top
	}
	share := float64(shelfUnits) / (float64(seededBidUnits) * npcResaleDepthRef)
	if share > 1 {
		share = 1
	}
	return npcResaleMarkupFloor + (top-npcResaleMarkupFloor)*(1-share)
}

// npcResaleBlend is the new resting price after one lot joins the shelf: the weighted
// average of the shelf and this lot's marked-up cost, so the price follows what the
// corporation paid and not what the book says.
func npcResaleBlend(shelfPrice float64, shelfUnits int64, lotPrice float64, lotUnits int64) float64 {
	if lotUnits <= 0 {
		return shelfPrice
	}
	if shelfUnits <= 0 || shelfPrice <= 0 {
		return lotPrice
	}
	total := float64(shelfUnits) + float64(lotUnits)
	return (shelfPrice*float64(shelfUnits) + lotPrice*float64(lotUnits)) / total
}

// RestockNPCResale puts ore an NPC corporation just bought back on its shelf at that
// station. It runs inside the sale's transaction beside ApplyNPCBuyImpact so shelving and
// the bid walk are atomic. The error is returned and the caller aborts the sale.
func RestockNPCResale(ctx context.Context, tx DBTX, stationID, solarSystemID, regionID,
	typeID, corpID int64, paidPrice float64, units int64) error {

	if !npcResaleOn.Load() || units <= 0 || paidPrice <= 0 || stationID == 0 {
		return nil
	}
	// Only ore and moon goo (npcResaleGroup): the refinery and T2 manufacturer sinks.
	var categoryID, groupID int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(g.categoryID, 0), t.groupID FROM invTypes t
		  JOIN invGroups g ON g.groupID = t.groupID
		 WHERE t.typeID = ?`, typeID).Scan(&categoryID, &groupID); err != nil || !npcResaleGroup(categoryID, groupID) {
		return nil
	}

	// The station's security tier and the corporation's seeded bid depth for this type,
	// which the shelf cap and the markup are measured against.
	var security sql.NullFloat64
	_ = tx.QueryRowContext(ctx,
		`SELECT security FROM mapSolarSystems WHERE solarSystemID = ?`, solarSystemID).Scan(&security)
	sec := 1.0
	if security.Valid {
		sec = security.Float64
	}
	tier := tierForSec(sec)

	var seeded sql.NullInt64
	_ = tx.QueryRowContext(ctx, `
		SELECT SUM(volRemaining) FROM market_orders
		 WHERE charID = ? AND bid = 1 AND orderState = 1
		   AND stationID = ? AND typeID = ?`,
		SeederCharID, stationID, typeID).Scan(&seeded)

	// The shelf as it stands.
	var orderID sql.NullInt64
	var shelfPrice sql.NullFloat64
	var shelfUnits sql.NullInt64
	_ = tx.QueryRowContext(ctx, `
		SELECT orderID, price, volRemaining FROM market_orders
		 WHERE charID = ? AND bid = 0 AND orderState = 1
		   AND stationID = ? AND typeID = ?
		 ORDER BY orderID LIMIT 1`,
		ResaleSeederCharID, stationID, typeID).Scan(&orderID, &shelfPrice, &shelfUnits)

	// The cap: past it the ore is consumed as before.
	if seeded.Valid && seeded.Int64 > 0 {
		if float64(shelfUnits.Int64) >= float64(seeded.Int64)*npcResaleMaxDepth() {
			return nil
		}
	}

	markup := npcResaleMarkup(tier, shelfUnits.Int64, seeded.Int64)
	lotPrice := paidPrice * markup
	price := npcResaleBlend(shelfPrice.Float64, shelfUnits.Int64, lotPrice, units)
	price = float64(int64(price*100+0.5)) / 100
	if price <= 0 {
		return nil
	}

	if orderID.Valid && orderID.Int64 != 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE market_orders SET price = ?, volRemaining = volRemaining + ?
			 WHERE orderID = ?`, price, units, orderID.Int64); err != nil {
			return fmt.Errorf("RestockNPCResale update: %w", err)
		}
		return nil
	}
	// Range 32767 (region-wide) so a refiner need not stand at the dock. `range` is a
	// reserved word, hence the backticks.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO market_orders
		  (typeID, charID, regionID, stationID, `+"`range`"+`, bid, price,
		   volEntered, volRemaining, issued, orderState, minVolume, contraband,
		   accountID, duration, isCorp, solarSystemID, escrow, jumps)
		VALUES (?, ?, ?, ?, 32767, 0, ?, ?, ?, ?, 1, 1, 0, ?, 365, 0, ?, 0, 1)`,
		typeID, ResaleSeederCharID, regionID, stationID, price, units, units,
		win32TimeNow(), corpID, solarSystemID); err != nil {
		return fmt.Errorf("RestockNPCResale insert: %w", err)
	}
	return nil
}
