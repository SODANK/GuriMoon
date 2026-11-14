package db

// Skill book purchase and injection. The ESP skill window and Genolution page act as a shop: a
// book is priced from the live market, the ISK moves from the character to Genolution Biotech
// with a journal row in each wallet and a market_transactions row, and the skill entity is
// written into the character at level 0. Prerequisites are checked against trained levels only,
// as the client's own injection gate does.
// See docs/db-8-notes.md#skill-shop-pricing.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// GenolutionCorpID is Genolution Biotech (crpNPCCorporations 1000140), the corporation that
// receives skill-injection payments.
const GenolutionCorpID = 1000140

// SkillPriceMarkup multiplies the market average to get the shop price, so ESP purchases do not
// undercut the market. Overridden from config.json (esp.skillMarkup) at startup.
var SkillPriceMarkup = 1.10

// SkillPriceSource names where a quote's average came from, for display.
const (
	SkillPriceFromOrders  = "sell orders"
	SkillPriceFromTrades  = "recent trades"
	SkillPriceFromBase    = "base price"
	SkillPriceUnavailable = "unpriced"
)

// SkillPriceQuote is one skillbook's price.
type SkillPriceQuote struct {
	TypeID  int64
	Average float64 // market average, before markup
	Price   float64 // what the shop charges: Average x SkillPriceMarkup
	Source  string  // SkillPriceFrom*: which market data produced Average
	Samples int64   // units backing the average (orders on sale, or units traded)
}

// skillTradeWindow is how far back SkillMarketPrices looks for transactions when no sell orders
// exist: 30 days in 100-ns Windows ticks.
const skillTradeWindow = int64(30) * 864000000000

// SkillMarketPrices returns a quote for every published skill type in one pass. Precedence: the
// volume-weighted average of active sell orders, else of 30 days of buy transactions, else
// invTypes.basePrice; with none, Price is 0 (SkillPriceUnavailable) and the shop refuses to sell.
// Cached process-wide for skillPriceTTL. See docs/db-8-notes.md#skill-shop-pricing.
func SkillMarketPrices(ctx context.Context, db *sql.DB) (map[int64]SkillPriceQuote, error) {
	skillPriceMu.Lock()
	defer skillPriceMu.Unlock()

	if skillPriceCache != nil && time.Since(skillPriceAt) < skillPriceTTL {
		return copySkillPrices(skillPriceCache), nil
	}

	fresh, err := skillMarketPricesUncached(ctx, db)
	if err != nil {
		// Serve a stale entry rather than failing the caller; a planner that cannot price anything
		// stops buying skills.
		if skillPriceCache != nil {
			return copySkillPrices(skillPriceCache), nil
		}
		return nil, err
	}
	skillPriceCache, skillPriceAt = fresh, time.Now()
	return copySkillPrices(fresh), nil
}

const skillPriceTTL = 60 * time.Second

var (
	skillPriceMu    sync.Mutex
	skillPriceCache map[int64]SkillPriceQuote
	skillPriceAt    time.Time
)

func copySkillPrices(src map[int64]SkillPriceQuote) map[int64]SkillPriceQuote {
	out := make(map[int64]SkillPriceQuote, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// InvalidateSkillPrices drops the cached quotes. For paths that move the market in bulk (an NPC
// market reseed, an admin price edit); ordinary trading is covered by the TTL.
func InvalidateSkillPrices() {
	skillPriceMu.Lock()
	skillPriceCache, skillPriceAt = nil, time.Time{}
	skillPriceMu.Unlock()
}

func skillMarketPricesUncached(ctx context.Context, db *sql.DB) (map[int64]SkillPriceQuote, error) {
	out := make(map[int64]SkillPriceQuote, 512)

	// Base prices first: the floor every published skill falls back to.
	rows, err := db.QueryContext(ctx, `
		SELECT t.typeID, COALESCE(t.basePrice, 0)
		  FROM invTypes t
		  JOIN invGroups g ON g.groupID = t.groupID AND g.categoryID = 16
		 WHERE t.published = 1`)
	if err != nil {
		return nil, fmt.Errorf("SkillMarketPrices base: %w", err)
	}
	for rows.Next() {
		var typeID int64
		var base float64
		if err := rows.Scan(&typeID, &base); err != nil {
			rows.Close()
			return nil, fmt.Errorf("SkillMarketPrices base scan: %w", err)
		}
		q := SkillPriceQuote{TypeID: typeID, Source: SkillPriceUnavailable}
		if base > 0 {
			q.Average, q.Source = base, SkillPriceFromBase
		}
		out[typeID] = q
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("SkillMarketPrices base rows: %w", err)
	}

	// Recent transactions (transactionType 1 = buy) override basePrice.
	rows, err = db.QueryContext(ctx, `
		SELECT tx.typeID, SUM(tx.price * tx.quantity) / SUM(tx.quantity), SUM(tx.quantity)
		  FROM market_transactions tx
		  JOIN invTypes  t ON t.typeID  = tx.typeID
		  JOIN invGroups g ON g.groupID = t.groupID AND g.categoryID = 16
		 WHERE tx.transactionType = 1 AND tx.quantity > 0
		   AND tx.transactionDateTime >= ?
		 GROUP BY tx.typeID`, win32TimeNow()-uint64(skillTradeWindow))
	if err != nil {
		return nil, fmt.Errorf("SkillMarketPrices trades: %w", err)
	}
	for rows.Next() {
		var typeID, units int64
		var avg float64
		if err := rows.Scan(&typeID, &avg, &units); err != nil {
			rows.Close()
			return nil, fmt.Errorf("SkillMarketPrices trades scan: %w", err)
		}
		if q, ok := out[typeID]; ok && avg > 0 {
			q.Average, q.Source, q.Samples = avg, SkillPriceFromTrades, units
			out[typeID] = q
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("SkillMarketPrices trades rows: %w", err)
	}

	// Active sell orders are the live listed price and take precedence.
	rows, err = db.QueryContext(ctx, `
		SELECT o.typeID, SUM(o.price * o.volRemaining) / SUM(o.volRemaining), SUM(o.volRemaining)
		  FROM market_orders o
		  JOIN invTypes  t ON t.typeID  = o.typeID
		  JOIN invGroups g ON g.groupID = t.groupID AND g.categoryID = 16
		 WHERE o.bid = 0 AND o.volRemaining > 0 AND o.orderState = 1
		 GROUP BY o.typeID`)
	if err != nil {
		return nil, fmt.Errorf("SkillMarketPrices orders: %w", err)
	}
	for rows.Next() {
		var typeID, units int64
		var avg float64
		if err := rows.Scan(&typeID, &avg, &units); err != nil {
			rows.Close()
			return nil, fmt.Errorf("SkillMarketPrices orders scan: %w", err)
		}
		if q, ok := out[typeID]; ok && avg > 0 {
			q.Average, q.Source, q.Samples = avg, SkillPriceFromOrders, units
			out[typeID] = q
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("SkillMarketPrices orders rows: %w", err)
	}

	markup := SkillPriceMarkup
	if markup < 1 {
		markup = 1
	}
	for typeID, q := range out {
		q.Price = roundISK(q.Average * markup)
		out[typeID] = q
	}
	return out, nil
}

// SkillMarketPrice quotes a single skill type, built on SkillMarketPrices so the precedence rules
// live in one place. Callers that need many quotes use the bulk form.
func SkillMarketPrice(ctx context.Context, db *sql.DB, typeID int64) (SkillPriceQuote, error) {
	all, err := SkillMarketPrices(ctx, db)
	if err != nil {
		return SkillPriceQuote{}, err
	}
	q, ok := all[typeID]
	if !ok {
		return SkillPriceQuote{TypeID: typeID, Source: SkillPriceUnavailable},
			fmt.Errorf("typeID %d is not a published skill", typeID)
	}
	return q, nil
}

// roundISK trims a price to whole ISK cents, so the charged and displayed amounts agree.
func roundISK(v float64) float64 {
	if v <= 0 {
		return 0
	}
	return float64(int64(v*100+0.5)) / 100
}

// SkillPurchase is the outcome of a successful buy-and-inject.
type SkillPurchase struct {
	TypeID     int64
	TypeName   string
	ItemID     int64   // the new skill entity in the character's brain
	Price      float64 // ISK charged
	Average    float64 // market average the price was derived from
	Source     string  // SkillPriceFrom*
	NewBalance float64 // character's wallet after the purchase
	CorpID     int64   // wallet that received the ISK (GenolutionCorpID)
}

// ErrSkillOwned is returned when the character already has the skill injected.
var ErrSkillOwned = errors.New("skill already injected")

// ErrSkillPrereq is returned when a prerequisite of the book is not trained yet. The book can be
// bought once the prerequisite finishes, so a planner skips it rather than giving up.
var ErrSkillPrereq = errors.New("cannot inject")

// PurchaseAndInjectSkill buys skillbook typeID for charID at the current shop price and injects
// it at level 0. It refuses an unpublished or owned skill, unmet trained prerequisites, no price,
// or an insufficient wallet. Each wallet gets a Market Transaction journal row (refTypeID 2),
// plus a market_transactions buy row.
func PurchaseAndInjectSkill(ctx context.Context, db *sql.DB, charID, typeID int64) (*SkillPurchase, error) {
	quote, err := SkillMarketPrice(ctx, db, typeID)
	if err != nil {
		return nil, err
	}
	if quote.Price <= 0 {
		return nil, fmt.Errorf("no market price for this skill (no sell orders, no recent trades, no base price)")
	}

	// Prerequisites are judged on trained levels only, the gate the client applies when injecting a
	// book from the hangar.
	reqs, err := GetRequiredSkills(ctx, db, typeID)
	if err != nil {
		return nil, err
	}
	if len(reqs) > 0 {
		trained, err := trainedSkillLevels(ctx, db, charID)
		if err != nil {
			return nil, err
		}
		if missing := UnmetSkillReqs(reqs, trained, nil); len(missing) > 0 {
			return nil, fmt.Errorf("%w: requires %s", ErrSkillPrereq,
				FormatSkillReqs(ctx, db, missing))
		}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("PurchaseAndInjectSkill begin: %w", err)
	}
	defer tx.Rollback()

	var typeName string
	var categoryID, published int64
	err = tx.QueryRowContext(ctx, `
		SELECT t.typeName, COALESCE(g.categoryID, 0), COALESCE(t.published, 0)
		  FROM invTypes t
		  LEFT JOIN invGroups g ON g.groupID = t.groupID
		 WHERE t.typeID = ?`, typeID).Scan(&typeName, &categoryID, &published)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("typeID %d not found in invTypes", typeID)
	}
	if err != nil {
		return nil, fmt.Errorf("PurchaseAndInjectSkill lookup: %w", err)
	}
	if categoryID != 16 {
		return nil, fmt.Errorf("typeID %d (%s) is not a skill", typeID, typeName)
	}
	if published == 0 {
		return nil, fmt.Errorf("%s is not published and cannot be bought", typeName)
	}

	var owned int64
	err = tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entity WHERE typeID = ? AND ownerID = ? AND flag IN (7, 61)`,
		typeID, charID).Scan(&owned)
	if err != nil {
		return nil, fmt.Errorf("PurchaseAndInjectSkill owned: %w", err)
	}
	if owned > 0 {
		return nil, fmt.Errorf("%s: %w", typeName, ErrSkillOwned)
	}

	// Lock the wallet row for the balance check and deduction.
	var balance float64
	var stationID int64
	err = tx.QueryRowContext(ctx,
		`SELECT balance, COALESCE(stationID, 0) FROM character_ WHERE characterID = ? FOR UPDATE`,
		charID).Scan(&balance, &stationID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("character %d not found", charID)
	}
	if err != nil {
		return nil, fmt.Errorf("PurchaseAndInjectSkill balance: %w", err)
	}
	if balance < quote.Price {
		return nil, fmt.Errorf("insufficient funds: %s costs %.2f ISK, wallet holds %.2f ISK",
			typeName, quote.Price, balance)
	}
	newBalance := balance - quote.Price

	if _, err := tx.ExecContext(ctx,
		`UPDATE character_ SET balance = ? WHERE characterID = ?`, newBalance, charID); err != nil {
		return nil, fmt.Errorf("PurchaseAndInjectSkill debit: %w", err)
	}

	// Credit Genolution and read back its balance for the corp journal line.
	if _, err := tx.ExecContext(ctx,
		`UPDATE corporation SET balance = balance + ? WHERE corporationID = ?`,
		quote.Price, GenolutionCorpID); err != nil {
		return nil, fmt.Errorf("PurchaseAndInjectSkill credit corp: %w", err)
	}
	var corpBalance float64
	if err := tx.QueryRowContext(ctx,
		`SELECT balance FROM corporation WHERE corporationID = ?`,
		GenolutionCorpID).Scan(&corpBalance); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("PurchaseAndInjectSkill corp balance: %w", err)
	}

	// Wallet journal: refTypeID 2 (Market Transaction), one row per wallet. The client renders it
	// as "Market: <ownerID1> bought stuff from <ownerID2>" and ignores argID1 (util.FmtRef), so the
	// buyer and Genolution go in the owner columns and argID1 stays NULL like every refType-2 row.
	reason := JournalReason("Skill injection: " + typeName)
	now := win32TimeNow()
	const journalSQL = `
		INSERT INTO market_journal
		  (characterID, transDate, refTypeID, ownerID1, ownerID2, argID1,
		   accountID, amount, balance, reason, accountKey)
		VALUES (?, ?, 2, ?, ?, NULL, 0, ?, ?, ?, 1000)`
	if _, err := tx.ExecContext(ctx, journalSQL,
		charID, now, charID, int64(GenolutionCorpID),
		-quote.Price, newBalance, reason); err != nil {
		return nil, fmt.Errorf("PurchaseAndInjectSkill char journal: %w", err)
	}
	if _, err := tx.ExecContext(ctx, journalSQL,
		int64(GenolutionCorpID), now, charID, int64(GenolutionCorpID),
		quote.Price, corpBalance, reason); err != nil {
		return nil, fmt.Errorf("PurchaseAndInjectSkill corp journal: %w", err)
	}

	// Market transaction row so the purchase feeds price history. Region comes from the character's
	// station; a character in space (stationID 0) records no location, which history aggregation
	// never groups into a region.
	var regionID int64
	if stationID != 0 {
		_ = tx.QueryRowContext(ctx,
			`SELECT COALESCE(regionID, 0) FROM staStations WHERE stationID = ?`,
			stationID).Scan(&regionID)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO market_transactions
		  (transactionDateTime, typeID, quantity, price, transactionType, characterID, clientID, regionID, stationID)
		VALUES (?, ?, 1, ?, 1, ?, ?, ?, ?)`,
		now, typeID, quote.Price, charID, int64(GenolutionCorpID), regionID, stationID); err != nil {
		return nil, fmt.Errorf("PurchaseAndInjectSkill transaction: %w", err)
	}

	// Inject: skills live in the character (locationID = charID, flag 7), not in its ship. See
	// GiveSkill.
	res, err := tx.ExecContext(ctx, `
		INSERT INTO entity
		  (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
		VALUES ('Skill', ?, ?, ?, 7, 0, 1, 1, 0, 0, 0, NULL)`,
		typeID, charID, charID)
	if err != nil {
		return nil, fmt.Errorf("PurchaseAndInjectSkill insert skill: %w", err)
	}
	itemID, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("PurchaseAndInjectSkill itemID: %w", err)
	}
	// Level (280) and skill points (276) start at zero. Both rows are written explicitly so the
	// skill window and godma read a value instead of a type default.
	for _, attr := range [2]int64{280, 276} {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO entity_attributes (itemID, attributeID, valueInt, valueFloat)
			VALUES (?, ?, 0, NULL)
			ON DUPLICATE KEY UPDATE valueInt = 0`, itemID, attr); err != nil {
			return nil, fmt.Errorf("PurchaseAndInjectSkill attr %d: %w", attr, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("PurchaseAndInjectSkill commit: %w", err)
	}
	return &SkillPurchase{
		TypeID: typeID, TypeName: typeName, ItemID: itemID,
		Price: quote.Price, Average: quote.Average, Source: quote.Source,
		NewBalance: newBalance, CorpID: GenolutionCorpID,
	}, nil
}

// trainedSkillLevels returns the character's injected skills and their trained levels. It
// ignores the queue and the skill in training: injection prerequisites are met by trained SP.
func trainedSkillLevels(ctx context.Context, db *sql.DB, charID int64) (map[int64]int, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT e.typeID, COALESCE(lv.valueInt, 0)
		  FROM entity e
		  LEFT JOIN entity_attributes lv ON lv.itemID = e.itemID AND lv.attributeID = 280
		 WHERE e.ownerID = ? AND e.flag IN (7, 61)`, charID)
	if err != nil {
		return nil, fmt.Errorf("trainedSkillLevels charID=%d: %w", charID, err)
	}
	defer rows.Close()
	out := make(map[int64]int, 64)
	for rows.Next() {
		var typeID int64
		var lvl int
		if err := rows.Scan(&typeID, &lvl); err != nil {
			return nil, fmt.Errorf("trainedSkillLevels scan: %w", err)
		}
		if lvl > out[typeID] {
			out[typeID] = lvl
		}
	}
	return out, rows.Err()
}

// UnmetInjectReqs formats the prerequisites charID is missing for typeID, or "" when the skill
// can be injected. The skill window uses it to label a book unbuyable.
func UnmetInjectReqs(reqs []SkillReq, trained map[int64]int, names map[int64]string) string {
	missing := UnmetSkillReqs(reqs, trained, nil)
	if len(missing) == 0 {
		return ""
	}
	parts := make([]string, 0, len(missing))
	for _, r := range missing {
		name := names[r.SkillID]
		if name == "" {
			name = fmt.Sprintf("type %d", r.SkillID)
		}
		parts = append(parts, name+" "+skillLevelRoman(r.Level))
	}
	return strings.Join(parts, ", ")
}

// TrainedSkillLevels exposes trainedSkillLevels to other packages (the ESP skill window gates
// the whole catalogue in one pass).
func TrainedSkillLevels(ctx context.Context, db *sql.DB, charID int64) (map[int64]int, error) {
	return trainedSkillLevels(ctx, db, charID)
}
