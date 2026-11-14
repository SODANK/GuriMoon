// marketadmindb.go: admin-panel market tooling. Bulk order seeding, order browsing and
// deletion, and the editor for the NPC supply/demand table (crpnpccorporationtrades).
// Everything here writes market_orders directly rather than through PlaceBuyOrder /
// PlaceSellOrder: no escrow is debited and no hangar items are consumed. Player buy
// orders fill from the owner's wallet at fill time; player sell orders deliver conjured items.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
)

// EnsureNPCTradePriceColumn adds the `price` column to crpnpccorporationtrades if it is
// missing. It is an admin-set per-corp, per-type override; NULL means derive the price
// from invTypes.basePrice. Idempotent.
func EnsureNPCTradePriceColumn(ctx context.Context, db *sql.DB) error {
	var n int
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.COLUMNS
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'crpnpccorporationtrades'
		   AND COLUMN_NAME = 'price'`).Scan(&n)
	if err != nil {
		return fmt.Errorf("EnsureNPCTradePriceColumn inspect: %w", err)
	}
	if n > 0 {
		return nil
	}
	if _, err := db.ExecContext(ctx,
		`ALTER TABLE crpnpccorporationtrades ADD COLUMN price DOUBLE NULL DEFAULT NULL`); err != nil {
		return fmt.Errorf("EnsureNPCTradePriceColumn alter: %w", err)
	}
	log.Printf("crpnpccorporationtrades: added price column (NULL = derive from basePrice)")
	return nil
}

// GetCorpStationIDs returns the stationIDs owned by an NPC corporation, for
// the "all stations of corp X" convenience button.
func GetCorpStationIDs(ctx context.Context, db *sql.DB, corpID int64) ([]int64, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT stationID FROM staStations WHERE corporationID = ? ORDER BY stationID`, corpID)
	if err != nil {
		return nil, fmt.Errorf("GetCorpStationIDs: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ── crpnpccorporationtrades editor ───────────────────────────────────────────

// SupplyDemandUnitScale converts a supplyDemand magnitude to seeded order volume:
// volume = |supplyDemand| * 10000. The server reads only the sign (sell or buy) and the
// magnitude of the stored share.
const SupplyDemandUnitScale = 10000

// UnitsToSupplyDemand converts a human-entered unit count and a side into the
// stored fraction. Buy orders are stored negative.
func UnitsToSupplyDemand(units int64, bid bool) float64 {
	v := float64(units) / SupplyDemandUnitScale
	if bid {
		return -v
	}
	return v
}

// NPCTrade is one row of NPC supply/demand data. CategoryName/GroupName mirror
// the client's Market Activity tab, which reads every line as
// "Category / Group / Item".
type NPCTrade struct {
	CorporationID int64
	CorpName      string
	TypeID        int64
	TypeName      string
	CategoryName  string
	GroupName     string
	BasePrice     float64
	SupplyDemand  float64
	StationCount  int64  // stations the corp owns (0 = seeding produces nothing)
	Seedable      bool   // type is published and in a market group (SeedNPCMarketOrders' filter)
	SkipReason    string // why reseeding skips it: "", TradeSkipMissing/Unpublished/NoMarketGroup

	// PriceOverride is the admin-set order price for this corp/type. Invalid (NULL in the DB)
	// means derive from basePrice.
	PriceOverride sql.NullFloat64
}

// Reasons a trade row never becomes a market order. SeedNPCMarketOrders joins invTypes and
// filters on published = 1 AND marketGroupID IS NOT NULL, so a row can fail three ways.
const (
	TradeSkipMissing     = "missing"     // no invTypes row at all on this server
	TradeSkipUnpublished = "unpublished" // invTypes.published = 0
	TradeSkipNoMarketGrp = "nomarketgrp" // invTypes.marketGroupID IS NULL
)

// SkipLabel is a short human label for SkipReason.
func (t NPCTrade) SkipLabel() string {
	switch t.SkipReason {
	case TradeSkipMissing:
		return "not in invTypes"
	case TradeSkipUnpublished:
		return "unpublished"
	case TradeSkipNoMarketGrp:
		return "no market group"
	}
	return ""
}

// SkipDetail explains SkipReason in a sentence, for a tooltip.
func (t NPCTrade) SkipDetail() string {
	switch t.SkipReason {
	case TradeSkipMissing:
		return "This server's invTypes has no row for this typeID, so the item does not exist here at all — the trade data is from a newer dump than the item data. Reseeding skips it."
	case TradeSkipUnpublished:
		return "The item exists but invTypes.published = 0, so the seeder's published = 1 filter skips it. Publish the type to seed it."
	case TradeSkipNoMarketGrp:
		return "The item exists and is published but has no marketGroupID, so the seeder skips it."
	}
	return ""
}

// Bid reports whether this row makes the corp buy (negative supplyDemand).
func (t NPCTrade) Bid() bool { return t.SupplyDemand < 0 }

// Exists reports whether the type resolved to an invTypes row.
func (t NPCTrade) Exists() bool { return t.SkipReason != TradeSkipMissing }

// Price is the order price reseeding would use. An admin override is used as entered
// (no jitter, no buy-side discount). Otherwise it is derived before the +-5% jitter:
// sells at basePrice, buys at the buy factor of it, 100 ISK when basePrice is 0, like
// SeedNPCMarketOrders.
func (t NPCTrade) Price() float64 {
	if t.HasPriceOverride() {
		return t.PriceOverride.Float64
	}
	return t.DerivedPrice()
}

// HasPriceOverride reports whether an admin price is set for this row.
func (t NPCTrade) HasPriceOverride() bool {
	return t.PriceOverride.Valid && t.PriceOverride.Float64 > 0
}

// DerivedPrice is the price reseeding computes from invTypes.basePrice when no
// override is set.
func (t NPCTrade) DerivedPrice() float64 {
	bp := t.BasePrice
	if bp <= 0 {
		bp = 100
	}
	if t.Bid() {
		return bp * mkt().npcBuyFactor
	}
	return bp
}

// UnitsPerStation is the order volume this row seeds at each station, matching
// the clamp applied by SeedNPCMarketOrders.
func (t NPCTrade) UnitsPerStation() int64 {
	v := int64(math.Abs(t.SupplyDemand) * SupplyDemandUnitScale)
	if v < 1 {
		v = 1
	}
	if v > 999999 {
		v = 999999
	}
	return v
}

// TotalUnits is the volume seeded across every station the corp owns.
func (t NPCTrade) TotalUnits() int64 { return t.UnitsPerStation() * t.StationCount }

// NPCCorpTradeStats summarises one NPC corporation for the corp picker.
type NPCCorpTradeStats struct {
	CorporationID int64
	CorpName      string
	StationCount  int64
	SellRows      int64
	BuyRows       int64
	TotalRows     int64
}

// ListNPCCorpsWithTradeStats returns every NPC corporation that owns stations
// or has supply/demand rows, with its trade-row counts, for the corp picker.
// nameFilter is an optional name substring.
func ListNPCCorpsWithTradeStats(ctx context.Context, db *sql.DB, nameFilter string) ([]NPCCorpTradeStats, error) {
	var where string
	var args []interface{}
	if nameFilter != "" {
		where = "WHERE c.corporationName LIKE ?"
		args = append(args, "%"+nameFilter+"%")
	}
	rows, err := db.QueryContext(ctx, `
		SELECT c.corporationID, COALESCE(c.corporationName, ''),
		       (SELECT COUNT(*) FROM staStations s WHERE s.corporationID = c.corporationID),
		       COALESCE(SUM(CASE WHEN t.supplyDemand > 0 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN t.supplyDemand < 0 THEN 1 ELSE 0 END), 0),
		       COUNT(t.typeID)
		  FROM crpnpccorporations c
		  LEFT JOIN crpnpccorporationtrades t ON t.corporationID = c.corporationID
		  `+where+`
		 GROUP BY c.corporationID, c.corporationName
		 ORDER BY c.corporationName`, args...)
	if err != nil {
		return nil, fmt.Errorf("ListNPCCorpsWithTradeStats: %w", err)
	}
	defer rows.Close()
	var out []NPCCorpTradeStats
	for rows.Next() {
		var c NPCCorpTradeStats
		if err := rows.Scan(&c.CorporationID, &c.CorpName, &c.StationCount,
			&c.SellRows, &c.BuyRows, &c.TotalRows); err != nil {
			return nil, fmt.Errorf("ListNPCCorpsWithTradeStats scan: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetNPCCorpName returns one NPC corporation's name.
func GetNPCCorpName(ctx context.Context, db *sql.DB, corpID int64) (string, error) {
	var name string
	err := db.QueryRowContext(ctx,
		`SELECT corporationName FROM crpnpccorporations WHERE corporationID = ?`, corpID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("GetNPCCorpName: %w", err)
	}
	return name, nil
}

// ListNPCTradesForCorp returns one corporation's supply/demand rows, ordered
// the way the client's Market Activity tab groups them (category, then group,
// then item).
func ListNPCTradesForCorp(ctx context.Context, db *sql.DB, corpID int64) ([]NPCTrade, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT t.corporationID, COALESCE(c.corporationName, ''),
		       t.typeID, COALESCE(it.typeName, ''),
		       COALESCE(ic.categoryName, ''), COALESCE(ig.groupName, ''),
		       COALESCE(it.basePrice, 0), t.supplyDemand, t.price,
		       CASE WHEN it.typeID IS NULL THEN '`+TradeSkipMissing+`'
		            WHEN it.published <> 1 THEN '`+TradeSkipUnpublished+`'
		            WHEN it.marketGroupID IS NULL THEN '`+TradeSkipNoMarketGrp+`'
		            ELSE '' END,
		       (SELECT COUNT(*) FROM staStations s WHERE s.corporationID = t.corporationID)
		  FROM crpnpccorporationtrades t
		  LEFT JOIN crpnpccorporations c ON c.corporationID = t.corporationID
		  LEFT JOIN invTypes it ON it.typeID = t.typeID
		  LEFT JOIN invGroups ig ON ig.groupID = it.groupID
		  LEFT JOIN invCategories ic ON ic.categoryID = ig.categoryID
		 WHERE t.corporationID = ?
		 ORDER BY ic.categoryName, ig.groupName, it.typeName, t.typeID`, corpID)
	if err != nil {
		return nil, fmt.Errorf("ListNPCTradesForCorp: %w", err)
	}
	defer rows.Close()
	var out []NPCTrade
	for rows.Next() {
		var t NPCTrade
		if err := rows.Scan(&t.CorporationID, &t.CorpName, &t.TypeID, &t.TypeName,
			&t.CategoryName, &t.GroupName, &t.BasePrice, &t.SupplyDemand, &t.PriceOverride,
			&t.SkipReason, &t.StationCount); err != nil {
			return nil, fmt.Errorf("ListNPCTradesForCorp scan: %w", err)
		}
		t.Seedable = t.SkipReason == ""
		out = append(out, t)
	}
	return out, rows.Err()
}

// PurgeMissingTypeTrades deletes every trade row whose typeID has no invTypes
// row. Those rows can never seed anything on this server.
func PurgeMissingTypeTrades(ctx context.Context, db *sql.DB) (int64, error) {
	res, err := db.ExecContext(ctx, `
		DELETE t FROM crpnpccorporationtrades t
		  LEFT JOIN invTypes it ON it.typeID = t.typeID
		 WHERE it.typeID IS NULL`)
	if err != nil {
		return 0, fmt.Errorf("PurgeMissingTypeTrades: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// PublishTradedUnpublishedTypes publishes every existing type that some NPC
// corp trades but that is marked unpublished. categoryName, when set, limits
// it to one inventory category. Returns the number of types changed.
func PublishTradedUnpublishedTypes(ctx context.Context, db *sql.DB, categoryName string) (int64, error) {
	q := `
		UPDATE invTypes it
		   JOIN invGroups ig ON ig.groupID = it.groupID
		   JOIN invCategories ic ON ic.categoryID = ig.categoryID
		  SET it.published = 1
		 WHERE it.published <> 1
		   AND it.marketGroupID IS NOT NULL
		   AND it.typeID IN (SELECT typeID FROM crpnpccorporationtrades)`
	var args []interface{}
	if categoryName != "" {
		q += ` AND ic.categoryName = ?`
		args = append(args, categoryName)
	}
	res, err := db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("PublishTradedUnpublishedTypes: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// MigrateMarketVariantVisibility repairs two invTypes defects that hide meta and Tech II
// variants from the client's market tree: marketGroupID = 0 (the parent's group in
// invMetaTypes is copied down) and published = 0 in a real group. Types outside
// invMetaTypes (test and NPC-only junk) are left alone. Old values go to
// invtypes_market_backup. One-shot via server_migrations; returns regrouped, published.
func MigrateMarketVariantVisibility(ctx context.Context, db *sql.DB) (int64, int64, error) {
	const migrationName = "market-variant-visibility"
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS server_migrations (
			name    VARCHAR(64) NOT NULL,
			applied BIGINT NOT NULL,
			PRIMARY KEY (name)
		)`); err != nil {
		return 0, 0, fmt.Errorf("MigrateMarketVariantVisibility meta table: %w", err)
	}
	var done int
	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM server_migrations WHERE name = ?`, migrationName).Scan(&done)
	if done > 0 {
		return 0, 0, nil
	}

	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS invtypes_market_backup (
			typeID        INT NOT NULL,
			published     TINYINT NOT NULL,
			marketGroupID INT NOT NULL,
			PRIMARY KEY (typeID)
		)`); err != nil {
		return 0, 0, fmt.Errorf("MigrateMarketVariantVisibility backup table: %w", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT IGNORE INTO invtypes_market_backup (typeID, published, marketGroupID)
		SELECT it.typeID, it.published, it.marketGroupID
		  FROM invTypes it
		  JOIN invMetaTypes mt ON mt.typeID = it.typeID
		  LEFT JOIN invTypes p ON p.typeID = mt.parentTypeID
		 WHERE (it.marketGroupID = 0 AND p.marketGroupID > 0)
		    OR (it.marketGroupID > 0 AND it.published = 0)`); err != nil {
		return 0, 0, fmt.Errorf("MigrateMarketVariantVisibility backup: %w", err)
	}

	// Variants inherit the market group of the type they are a variant of.
	regroup, err := db.ExecContext(ctx, `
		UPDATE invTypes it
		   JOIN invMetaTypes mt ON mt.typeID = it.typeID
		   JOIN invTypes p ON p.typeID = mt.parentTypeID
		  SET it.marketGroupID = p.marketGroupID
		 WHERE it.marketGroupID = 0 AND p.marketGroupID > 0`)
	if err != nil {
		return 0, 0, fmt.Errorf("MigrateMarketVariantVisibility regroup: %w", err)
	}
	nRegrouped, _ := regroup.RowsAffected()

	// Anything now sitting in a real market group has to be published to show.
	publish, err := db.ExecContext(ctx, `
		UPDATE invTypes it
		   JOIN invMetaTypes mt ON mt.typeID = it.typeID
		  SET it.published = 1
		 WHERE it.published <> 1 AND it.marketGroupID > 0`)
	if err != nil {
		return nRegrouped, 0, fmt.Errorf("MigrateMarketVariantVisibility publish: %w", err)
	}
	nPublished, _ := publish.RowsAffected()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO server_migrations (name, applied) VALUES (?, ?)`,
		migrationName, win32TimeNow()); err != nil {
		return nRegrouped, nPublished, fmt.Errorf("MigrateMarketVariantVisibility mark applied: %w", err)
	}
	return nRegrouped, nPublished, nil
}

// MigrateNPCTradeSeedability makes crpnpccorporationtrades consistent with the shipped
// invTypes: rows naming a typeID with no invTypes row are deleted, and rows naming a
// market-grouped type left at published = 0 publish it. One-shot, guarded by
// server_migrations. Returns the types published and the rows purged.
func MigrateNPCTradeSeedability(ctx context.Context, db *sql.DB) (int64, int64, error) {
	const migrationName = "npc-trade-seedability"
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS server_migrations (
			name    VARCHAR(64) NOT NULL,
			applied BIGINT NOT NULL,
			PRIMARY KEY (name)
		)`); err != nil {
		return 0, 0, fmt.Errorf("MigrateNPCTradeSeedability meta table: %w", err)
	}
	var done int
	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM server_migrations WHERE name = ?`, migrationName).Scan(&done)
	if done > 0 {
		return 0, 0, nil
	}

	published, err := PublishTradedUnpublishedTypes(ctx, db, "")
	if err != nil {
		return 0, 0, err
	}
	purged, err := PurgeMissingTypeTrades(ctx, db)
	if err != nil {
		return published, 0, err
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO server_migrations (name, applied) VALUES (?, ?)`,
		migrationName, win32TimeNow()); err != nil {
		return published, purged, fmt.Errorf("MigrateNPCTradeSeedability mark applied: %w", err)
	}
	return published, purged, nil
}

// MigratePublishMarketGroupedItems publishes every invTypes row with a marketGroupID > 0
// still at published = 0, covering the base items the two migrations above miss. One-shot,
// guarded by server_migrations.
func MigratePublishMarketGroupedItems(ctx context.Context, db *sql.DB) (int64, error) {
	const migrationName = "publish-market-grouped-items"
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS server_migrations (
			name    VARCHAR(64) NOT NULL,
			applied BIGINT NOT NULL,
			PRIMARY KEY (name)
		)`); err != nil {
		return 0, fmt.Errorf("MigratePublishMarketGroupedItems meta table: %w", err)
	}
	var done int
	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM server_migrations WHERE name = ?`, migrationName).Scan(&done)
	if done > 0 {
		return 0, nil
	}

	res, err := db.ExecContext(ctx, `
		UPDATE invTypes
		   SET published = 1
		 WHERE published = 0 AND marketGroupID > 0`)
	if err != nil {
		return 0, fmt.Errorf("MigratePublishMarketGroupedItems update: %w", err)
	}
	n, _ := res.RowsAffected()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO server_migrations (name, applied) VALUES (?, ?)`,
		migrationName, win32TimeNow()); err != nil {
		return n, fmt.Errorf("MigratePublishMarketGroupedItems mark applied: %w", err)
	}
	return n, nil
}

// UpsertNPCTrade sets the supplyDemand and price override for a (corp, type).
// The table has no primary key, so this is DELETE + INSERT keyed on the pair.
// An invalid price stores NULL (derive from basePrice at seed time).
func UpsertNPCTrade(ctx context.Context, db *sql.DB, corpID, typeID int64, sd float64, price sql.NullFloat64) error {
	if _, err := db.ExecContext(ctx,
		`DELETE FROM crpnpccorporationtrades WHERE corporationID = ? AND typeID = ?`,
		corpID, typeID); err != nil {
		return fmt.Errorf("UpsertNPCTrade delete: %w", err)
	}
	var priceArg interface{}
	if price.Valid && price.Float64 > 0 {
		priceArg = price.Float64
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO crpnpccorporationtrades (corporationID, typeID, supplyDemand, price) VALUES (?, ?, ?, ?)`,
		corpID, typeID, sd, priceArg); err != nil {
		return fmt.Errorf("UpsertNPCTrade insert: %w", err)
	}
	return nil
}

// SetNPCTradePrice updates only the price override on existing (corp, type)
// rows, leaving supplyDemand alone. An invalid price clears the override back
// to NULL (derive from basePrice).
func SetNPCTradePrice(ctx context.Context, db *sql.DB, corpID, typeID int64, price sql.NullFloat64) error {
	var priceArg interface{}
	if price.Valid && price.Float64 > 0 {
		priceArg = price.Float64
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE crpnpccorporationtrades SET price = ? WHERE corporationID = ? AND typeID = ?`,
		priceArg, corpID, typeID); err != nil {
		return fmt.Errorf("SetNPCTradePrice: %w", err)
	}
	return nil
}

// SetNPCCorpPrices bulk-applies a price policy to every one of a corporation's
// trade rows: mode "clear" resets them all to derived, "mult" sets each to its
// derived price times factor. Returns rows affected.
func SetNPCCorpPrices(ctx context.Context, db *sql.DB, corpID int64, mode string, factor float64) (int64, error) {
	var q string
	var args []interface{}
	switch mode {
	case "clear":
		q = `UPDATE crpnpccorporationtrades SET price = NULL WHERE corporationID = ?`
		args = []interface{}{corpID}
	case "mult":
		// Mirror DerivedPrice: basePrice (fallback 100), times npcBuyFactor on buys.
		q = `UPDATE crpnpccorporationtrades t
		       LEFT JOIN invTypes it ON it.typeID = t.typeID
		        SET t.price = GREATEST(0.01, ROUND(
		              COALESCE(NULLIF(it.basePrice, 0), 100)
		              * IF(t.supplyDemand < 0, ?, 1.0) * ?, 2))
		      WHERE t.corporationID = ?`
		args = []interface{}{mkt().npcBuyFactor, factor, corpID}
	default:
		return 0, fmt.Errorf("SetNPCCorpPrices: unknown mode %q", mode)
	}
	res, err := db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("SetNPCCorpPrices: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// DeleteNPCTrade removes a (corp, type) supply/demand row.
func DeleteNPCTrade(ctx context.Context, db *sql.DB, corpID, typeID int64) error {
	if _, err := db.ExecContext(ctx,
		`DELETE FROM crpnpccorporationtrades WHERE corporationID = ? AND typeID = ?`,
		corpID, typeID); err != nil {
		return fmt.Errorf("DeleteNPCTrade: %w", err)
	}
	return nil
}

// BulkUpsertNPCTrades applies many trade rows in one call. Returns the count
// applied.
func BulkUpsertNPCTrades(ctx context.Context, db *sql.DB, rows []NPCTrade) (int, error) {
	n := 0
	for _, r := range rows {
		if err := UpsertNPCTrade(ctx, db, r.CorporationID, r.TypeID, r.SupplyDemand, r.PriceOverride); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

// placeholders returns "?,?,?" with n placeholders.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}
