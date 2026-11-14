package esp

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

// MarketAdminNotify is set by main. After an NPC market reseed it nudges the given online
// owners' order windows and broadcasts a market-changed event to docked clients. Nil-safe; a hook
// because esp cannot import server.
var MarketAdminNotify func(changedCharIDs []int64)

func marketNotify(charIDs []int64) {
	if MarketAdminNotify != nil {
		MarketAdminNotify(charIDs)
	}
}

// handleMarketSupply serves the supply/demand editor page.
func (s *Server) handleMarketSupply(w http.ResponseWriter, r *http.Request) {
	var result, resultErr string

	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err == nil {
			switch r.FormValue("action") {
			case "saveTrade":
				corpID := parseInt64(r.FormValue("corpID"))
				typeID := parseInt64(r.FormValue("typeID"))
				// Units are the human-facing input; the stored supplyDemand is
				// units/10000, negative for buy orders.
				units := parseInt64(r.FormValue("units"))
				sd := evedb.UnitsToSupplyDemand(units, r.FormValue("side") == "buy")
				price := parsePriceOverride(r.FormValue("price"))
				if corpID == 0 || typeID == 0 {
					resultErr = "Corp and type are required."
				} else if units < 1 {
					resultErr = "Units per station must be at least 1."
				} else if err := evedb.UpsertNPCTrade(r.Context(), s.db, corpID, typeID, sd, price); err != nil {
					resultErr = "Error: " + err.Error()
				} else {
					side := "sells"
					if sd < 0 {
						side = "buys"
					}
					priceNote := "price auto (from basePrice)"
					if price.Valid {
						priceNote = fmt.Sprintf("price %.2f ISK", price.Float64)
					}
					result = fmt.Sprintf("Saved: corp %d %s %d units of type %d per station, %s.",
						corpID, side, units, typeID, priceNote)
				}
			case "setPrice":
				corpID := parseInt64(r.FormValue("corpID"))
				typeID := parseInt64(r.FormValue("typeID"))
				price := parsePriceOverride(r.FormValue("price"))
				if err := evedb.SetNPCTradePrice(r.Context(), s.db, corpID, typeID, price); err != nil {
					resultErr = "Error: " + err.Error()
				} else if price.Valid {
					result = fmt.Sprintf("Type %d priced at %.2f ISK.", typeID, price.Float64)
				} else {
					result = fmt.Sprintf("Type %d price cleared — back to basePrice.", typeID)
				}
			case "corpPrices":
				corpID := parseInt64(r.FormValue("corpID"))
				mode := r.FormValue("priceMode")
				factor := parseFloat(r.FormValue("factor"))
				if mode == "mult" && factor <= 0 {
					resultErr = "Multiplier must be greater than zero."
					break
				}
				ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
				n, err := evedb.SetNPCCorpPrices(ctx, s.db, corpID, mode, factor)
				cancel()
				if err != nil {
					resultErr = "Error: " + err.Error()
				} else if mode == "clear" {
					result = fmt.Sprintf("Cleared price overrides on %d rows — all back to basePrice.", n)
				} else {
					result = fmt.Sprintf("Priced %d rows at %g × basePrice.", n, factor)
				}
			case "deleteTrade":
				corpID := parseInt64(r.FormValue("corpID"))
				typeID := parseInt64(r.FormValue("typeID"))
				if err := evedb.DeleteNPCTrade(r.Context(), s.db, corpID, typeID); err != nil {
					resultErr = "Error: " + err.Error()
				} else {
					result = fmt.Sprintf("Deleted trade: corp %d / type %d.", corpID, typeID)
				}
			case "bulkTrades":
				rows, perr := parseTradeLines(r.FormValue("bulk"))
				if perr != "" {
					resultErr = perr
				} else if n, err := evedb.BulkUpsertNPCTrades(r.Context(), s.db, rows); err != nil {
					resultErr = fmt.Sprintf("Applied %d rows, then error: %v", n, err)
				} else {
					result = fmt.Sprintf("Applied %d trade rows.", n)
				}
			case "reseed":
				ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
				deleted, inserted, err := evedb.SeedNPCMarketOrders(ctx, s.db)
				var ore int64
				if err == nil {
					ore, err = evedb.SeedOreDemand(ctx, s.db)
				}
				cancel()
				if err != nil {
					resultErr = "Error: " + err.Error()
				} else {
					marketNotify(nil)
					result = fmt.Sprintf("Reseeded NPC orders: purged %d, inserted %d (+%d ore demand). Hand-seeded (admin) orders were preserved.",
						deleted, inserted, ore)
				}
			}
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	// Corp-first navigation: with no ?corp= the page is a corporation picker;
	// with one it shows just that corporation's supply/demand rows.
	selectedCorp := parseInt64(r.URL.Query().Get("corp"))
	if selectedCorp == 0 {
		selectedCorp = parseInt64(r.FormValue("corpID"))
	}

	data := map[string]any{
		"Result":    result,
		"ResultErr": resultErr,
		"Page":      "market-supply",
		"UnitScale": evedb.SupplyDemandUnitScale,
	}

	if selectedCorp == 0 {
		nameFilter := r.URL.Query().Get("q")
		corps, err := evedb.ListNPCCorpsWithTradeStats(ctx, s.db, nameFilter)
		if err != nil {
			s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "market-supply"})
			return
		}
		data["Corps"] = corps
		data["NameFilter"] = nameFilter
	} else {
		corpName, err := evedb.GetNPCCorpName(ctx, s.db, selectedCorp)
		if err != nil {
			s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "market-supply"})
			return
		}
		trades, err := evedb.ListNPCTradesForCorp(ctx, s.db, selectedCorp)
		if err != nil {
			s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "market-supply"})
			return
		}
		var stations, sells, buys, unseedable int64
		for _, t := range trades {
			stations = t.StationCount
			if t.Bid() {
				buys++
			} else {
				sells++
			}
			if !t.Seedable {
				unseedable++
			}
		}
		if len(trades) == 0 {
			if ids, err := evedb.GetCorpStationIDs(ctx, s.db, selectedCorp); err == nil {
				stations = int64(len(ids))
			}
		}
		data["Corp"] = evedb.NPCCorpTradeStats{
			CorporationID: selectedCorp, CorpName: corpName,
			StationCount: stations, SellRows: sells, BuyRows: buys,
			TotalRows: int64(len(trades)),
		}
		data["Trades"] = trades
		data["Panels"] = buildTradePanels(trades)
		data["Unseedable"] = unseedable
	}
	s.render(w, "market_supply.html", data)
}

// tradeGroup is one heading in the supply/demand panels. The client's Market Activity tab lists
// lines as "Category / Group / Item", so the editor groups rows the same way.
type tradeGroup struct {
	CategoryName string
	GroupName    string
	Rows         []evedb.NPCTrade
	UnitsTotal   int64 // units/station summed over the group
}

// tradePanel is one side of the editor: the client shows Supply above Demand, the page shows
// them side by side.
type tradePanel struct {
	Title      string // "Supply" / "Demand"
	Sub        string // what the side means for this corp
	Side       string // saveTrade side value for rows already in this panel
	Other      string // side value that moves a row to the other panel
	OtherTitle string
	Class      string
	Groups     []tradeGroup
	RowCount   int64
	UnitsTotal int64 // units/station summed over the panel
	Unseedable int64
}

// buildTradePanels splits one corp's rows into supply and demand, bucketing
// each side by category/group in the order the query returned them.
func buildTradePanels(trades []evedb.NPCTrade) []tradePanel {
	panels := []tradePanel{
		{Title: "Supply", Sub: "corp sells these", Side: "sell", Other: "buy",
			OtherTitle: "Move to Demand", Class: "panel-supply"},
		{Title: "Demand", Sub: "corp buys these", Side: "buy", Other: "sell",
			OtherTitle: "Move to Supply", Class: "panel-demand"},
	}
	for _, t := range trades {
		p := &panels[0]
		if t.Bid() {
			p = &panels[1]
		}
		p.RowCount++
		p.UnitsTotal += t.UnitsPerStation()
		if !t.Seedable {
			p.Unseedable++
		}
		cat, grp := t.CategoryName, t.GroupName
		if cat == "" {
			cat = "Unknown"
		}
		if grp == "" {
			grp = "Unknown"
		}
		if n := len(p.Groups); n > 0 && p.Groups[n-1].CategoryName == cat && p.Groups[n-1].GroupName == grp {
			p.Groups[n-1].Rows = append(p.Groups[n-1].Rows, t)
			p.Groups[n-1].UnitsTotal += t.UnitsPerStation()
			continue
		}
		p.Groups = append(p.Groups, tradeGroup{CategoryName: cat, GroupName: grp,
			Rows: []evedb.NPCTrade{t}, UnitsTotal: t.UnitsPerStation()})
	}
	return panels
}

// parseTradeLines parses "corpID typeID supplyDemand" lines. Returns the rows
// and a non-empty error string on the first malformed line.
func parseTradeLines(s string) ([]evedb.NPCTrade, string) {
	var out []evedb.NPCTrade
	for n, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			return nil, fmt.Sprintf("Line %d: expected at least 3 values (corpID typeID supplyDemand [price]), got %q", n+1, line)
		}
		corpID, err1 := strconv.ParseInt(fields[0], 10, 64)
		typeID, err2 := strconv.ParseInt(fields[1], 10, 64)
		sd, err3 := strconv.ParseFloat(fields[2], 64)
		if err1 != nil || err2 != nil || err3 != nil {
			return nil, fmt.Sprintf("Line %d: could not parse numbers in %q", n+1, line)
		}
		row := evedb.NPCTrade{CorporationID: corpID, TypeID: typeID, SupplyDemand: sd}
		// Optional 4th field: explicit price. Omitted = derive from basePrice.
		if len(fields) >= 4 {
			p, err := strconv.ParseFloat(fields[3], 64)
			if err != nil {
				return nil, fmt.Sprintf("Line %d: could not parse price %q", n+1, fields[3])
			}
			row.PriceOverride = sql.NullFloat64{Float64: p, Valid: p > 0}
		}
		out = append(out, row)
	}
	return out, ""
}

// parseFloat parses a form value to float64, returning 0 on failure.
func parseFloat(s string) float64 {
	v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v
}

// parsePriceOverride reads an optional price field. Blank, zero or unparseable means no
// override: the price derives from basePrice.
func parsePriceOverride(s string) sql.NullFloat64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return sql.NullFloat64{}
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v <= 0 {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: v, Valid: true}
}
