package db

// Security-tiered NPC market tuning. Highsec is a thin market: shallow bids, fast saturation,
// slow recovery. Lowsec and nullsec pay more, with deeper volume and faster recovery. Tiers are
// cut on mapSolarSystems.security (truesec): >= 0.45 is highsec, > 0.0 lowsec, the rest nullsec;
// an order with no map row reads as highsec.
// See docs/market-notes.md#security-tiers.

import (
	"fmt"
	"log"
	"sync"
)

// hisecBoundary is the truesec cutoff for the highsec tier. Truesec 0.45 rounds to the 0.5 the
// client displays, matching the ore-distribution rules in destiny/system.go.
const hisecBoundary = 0.45

// secTier indexes the per-tier tuning tables below.
type secTier int

const (
	tierHigh secTier = iota
	tierLow
	tierNull
)

// tierForSec maps a system's truesec to its market tier.
func tierForSec(security float64) secTier {
	switch {
	case security >= hisecBoundary:
		return tierHigh
	case security > 0.0:
		return tierLow
	default:
		return tierNull
	}
}

// secCase renders a SQL CASE over a security expression that resolves to the tier's value from
// v. secExpr is a trusted column expression (never user input), typically
// COALESCE(ms.security, 1.0) off a mapSolarSystems join.
func secCase(secExpr string, v [3]float64) string {
	return fmt.Sprintf("(CASE WHEN %s >= %g THEN %g WHEN %s > 0.0 THEN %g ELSE %g END)",
		secExpr, hisecBoundary, v[tierHigh], secExpr, v[tierLow], v[tierNull])
}

// defaultMarketLevers are the per-tier tuning tables, indexed [tierHigh, tierLow, tierNull].
// mkt() answers the live set, with each overridable from server_settings (market.<name>).
var defaultMarketLevers = marketLevers{
	// secBidMul multiplies the seeded NPC bid factor (npcBuyFactor) for flat-priced goods (ore, ice,
	// minerals, everything outside the trade-good dynamics).
	secBidMul: [3]float64{0.95, 1.10, 1.22},

	// secVolMul scales seeded order volume. A shallow highsec bid takes a larger share of each sale,
	// so impact bites harder there.
	secVolMul: [3]float64{0.50, 1.00, 1.25},

	// Trade-good seed bands: ask = base x [sellLo, 1.0), bid = base x [buyLo, buyLo+buySpan). The
	// first dynamics ticks walk a fresh lane down to the worked-lane spread below.
	secTradeSellLo:  [3]float64{0.85, 0.78, 0.72},
	secTradeBuyLo:   [3]float64{1.18, 1.26, 1.34},
	secTradeBuySpan: [3]float64{0.10, 0.15, 0.20},

	// Trade-good dynamics bands (multiples of basePrice). secTradeSellCap is the ask ceiling for
	// trade goods and secDynSellCap the ceiling for every other good (askImpactCapExpr picks per
	// group). Two relations must hold:
	// secDynBuyFloor > secTradeSellCap in every tier (TestTradeGoodLanesNeverSaturateNegative), and
	// the floors stay near the highsec ask cap (TestTradeGoodCrossTierSpreadBounded).
	// Trade-good bid restock is also budgeted (marketdynamics.go, tradeGoodBidBudgetSetting).
	// See docs/market-notes.md#security-tiers.
	secDynBuyCap:    [3]float64{1.35, 1.55, 1.75},
	secDynBuyFloor:  [3]float64{1.14, 1.20, 1.26},
	secTradeSellCap: [3]float64{1.08, 1.12, 1.16},
	secDynSellFloor: [3]float64{0.85, 0.78, 0.72},

	// secDynSellCap is the ask ceiling ApplyNPCSellImpact enforces for every non-trade good.
	secDynSellCap: [3]float64{1.08, 1.28, 1.45},

	// tradeGoodVolMul deepens seeded trade-good orders on top of secVolMul. A bigger order takes a
	// smaller per-unit hit from ApplyNPCBuyImpact, so one hauler clears less of the lane.
	tradeGoodVolMul: 2.0,

	// Impact: how hard a player sale moves the NPC bid it fills, how far it can fall (fraction of
	// the tier's seeded bid), and how much of the gap heals per dynamics tick.
	secImpactDepth:    [3]float64{0.18, 0.10, 0.07},
	secImpactFloor:    [3]float64{0.45, 0.62, 0.72},
	secImpactRecovery: [3]float64{0.10, 0.18, 0.24},

	// npcBuyFactor is the spread between NPC sell and buy prices for the
	// same item: the seeded bid factor before secBidMul.
	npcBuyFactor: 0.90,

	// impactSpill is how much of the primary cut carries to the same NPC
	// corporation's other bids for the type (marketimpact.go).
	impactSpill: 0.40,
}

// marketLevers is one complete set of NPC market levers.
type marketLevers struct {
	secBidMul, secVolMul                              [3]float64
	secTradeSellLo, secTradeBuyLo, secTradeBuySpan    [3]float64
	secDynBuyCap, secDynBuyFloor, secTradeSellCap     [3]float64
	secDynSellFloor, secDynSellCap                    [3]float64
	tradeGoodVolMul                                   float64
	secImpactDepth, secImpactFloor, secImpactRecovery [3]float64
	npcBuyFactor, impactSpill                         float64
}

// marketLeverTiers names each tier table's server_settings key. A value is three
// comma-separated numbers, high,low,null (config.json may write an array). Seed-time levers
// (bid and volume multipliers, trade-good bands) reach existing orders only through an ESP
// market reseed.
func marketLeverTiers(l *marketLevers) map[string]*[3]float64 {
	return map[string]*[3]float64{
		"market.secBidMul":         &l.secBidMul,
		"market.secVolMul":         &l.secVolMul,
		"market.secTradeSellLo":    &l.secTradeSellLo,
		"market.secTradeBuyLo":     &l.secTradeBuyLo,
		"market.secTradeBuySpan":   &l.secTradeBuySpan,
		"market.secDynBuyCap":      &l.secDynBuyCap,
		"market.secDynBuyFloor":    &l.secDynBuyFloor,
		"market.secTradeSellCap":   &l.secTradeSellCap,
		"market.secDynSellFloor":   &l.secDynSellFloor,
		"market.secDynSellCap":     &l.secDynSellCap,
		"market.secImpactDepth":    &l.secImpactDepth,
		"market.secImpactFloor":    &l.secImpactFloor,
		"market.secImpactRecovery": &l.secImpactRecovery,
	}
}

var marketLeverMemo struct {
	sync.Mutex
	gen    *map[string]string // settings-cache snapshot the memo was built from
	built  bool
	levers marketLevers
}

// mkt answers the live market levers: defaultMarketLevers with server_settings and config.json
// overrides applied, rebuilt once per settings-cache reload. If an override leaves
// secDynBuyFloor <= secTradeSellCap in any tier, both revert to the defaults.
func mkt() marketLevers {
	gen := settingsCache.Load()
	marketLeverMemo.Lock()
	defer marketLeverMemo.Unlock()
	if marketLeverMemo.built && marketLeverMemo.gen == gen {
		return marketLeverMemo.levers
	}
	l := defaultMarketLevers
	for key, dst := range marketLeverTiers(&l) {
		v := CachedSettingFloats(key, dst[:], 0, 100)
		copy(dst[:], v)
	}
	l.tradeGoodVolMul = CachedSettingFloat("market.tradeGoodVolMul", l.tradeGoodVolMul, 0, 100)
	l.npcBuyFactor = CachedSettingFloat("market.npcBuyFactor", l.npcBuyFactor, 0, 10)
	l.impactSpill = CachedSettingFloat("market.impactSpill", l.impactSpill, 0, 1)
	for t := 0; t < 3; t++ {
		if l.secDynBuyFloor[t] <= l.secTradeSellCap[t] {
			log.Printf("[market] secDynBuyFloor %v must exceed secTradeSellCap %v in every tier — keeping the defaults for both",
				l.secDynBuyFloor, l.secTradeSellCap)
			l.secDynBuyFloor = defaultMarketLevers.secDynBuyFloor
			l.secTradeSellCap = defaultMarketLevers.secTradeSellCap
			break
		}
	}
	marketLeverMemo.gen, marketLeverMemo.built, marketLeverMemo.levers = gen, true, l
	return l
}

// There is no secImpactCeil beside secImpactFloor: an ask's ceiling is secTradeSellCap for trade
// goods (the band TickNPCMarketDynamics clamps into) and secDynSellCap otherwise, so impact and
// the repricer share one number. secImpactDepth and secImpactRecovery apply to both sides.

// orderSecExpr is the security expression used in order UPDATEs, off a LEFT JOIN
// mapSolarSystems ms ON ms.solarSystemID = o.solarSystemID. Missing map rows read as highsec.
const orderSecExpr = "COALESCE(ms.security, 1.0)"
