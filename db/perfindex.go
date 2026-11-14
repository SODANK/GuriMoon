package db

// Performance indexes, applied at boot by EnsurePerfIndexes. sql/optional/perf_indexes.sql and
// sql/optional/perf_indexes2.sql hold the same statements for manual loading.
// Creation is ALGORITHM=INPLACE, LOCK=NONE so it fails rather than copying a live table.
// A missing table is skipped, so boot order cannot break this file.
// Which index serves which query: docs/db-notes.md#performance-indexes.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

// perfIndex is one index this server wants to exist.
type perfIndex struct {
	table   string
	name    string
	columns string // the parenthesised column list, e.g. "(charID, orderState)"
	why     string // one line for the boot log, so a slow DDL explains itself
}

// perfIndexes is the full set, in the order the two .sql files declare them.
// Round one first, then round two, then the growing-table additions.
var perfIndexes = []perfIndex{
	// ── round one: sql/optional/perf_indexes.sql ──────────────────────────────────────

	// Live-order count before every placement (marketorders.go: WHERE charID = ? AND orderState = 1).
	{"market_orders", "idx_mo_char_state", "(charID, orderState)",
		"every order placement counts the character's live orders"},

	// Order matching (region + type + bid + orderState + volRemaining). typeID leads because
	// it is far more selective than regionID.
	{"market_orders", "idx_mo_type_region_bid", "(typeID, regionID, bid, orderState, volRemaining)",
		"order matching"},

	// Counterparty lookup by clientID. The Transactions tab reads characterID instead
	// (idx_mt_owner_date, EnsureTransactionOwnerColumn).
	{"market_transactions", "idx_mt_client_date", "(clientID, transactionDateTime)",
		"counterparty lookup"},

	// Price history aggregate over region + type + transactionType (marketdb.go).
	{"market_transactions", "idx_mt_region_type", "(regionID, typeID, transactionType)",
		"price history aggregate"},

	// Wallet journal reads and character purges (characterID), counterparty searches
	// (ownerID1, ownerID2).
	{"market_journal", "idx_mj_char_date", "(characterID, transDate)",
		"wallet journal read"},
	{"market_journal", "idx_mj_owner1", "(ownerID1)",
		"journal counterparty search"},
	{"market_journal", "idx_mj_owner2", "(ownerID2)",
		"journal counterparty search"},

	// character_ has no index on its location and affiliation columns. Corp rosters, station
	// guest lists and system population counts filter on them.
	{"character_", "idx_char_corp", "(corporationID)",
		"corp rosters"},
	{"character_", "idx_char_station", "(stationID)",
		"station guest lists"},
	{"character_", "idx_char_system", "(solarSystemID)",
		"system population counts"},

	// ── round two: sql/optional/perf_indexes2.sql ─────────────────────────────────────

	// FindAssemblyLineForActivity (ramdb.go) filters containerID + activityID.
	// idx_ramjobs_line below turns its correlated ramJobs subquery into a point lookup.
	{"ramassemblylines", "idx_ral_container_activity", "(containerID, activityID)",
		"factory line pick"},
	{"ramJobs", "idx_ramjobs_line", "(assemblyLineID, completedStatusID, endProductionTime)",
		"the factory line pick's correlated subquery"},

	// Science & Industry overview: the finish histogram (running jobs by end time),
	// delivered-per-day (status 101 by end time) and installed-per-day (install time).
	{"ramJobs", "idx_ramjobs_status_end", "(completedStatusID, endProductionTime)",
		"the Science & Industry finish histogram and delivered-per-day"},
	{"ramJobs", "idx_ramjobs_install", "(installTime)",
		"the Science & Industry installed-per-day"},

	// SkillMarketPrices (skillpurchasedb.go). Leads with its two filter columns and carries
	// typeID, quantity and price, so the aggregate is index-only.
	{"market_transactions", "idx_mt_type_date_agg",
		"(transactionType, transactionDateTime, typeID, quantity, price)",
		"the skill-price aggregate"},

	// The trade-route gap query groups the whole live order book by typeID. The index is
	// covering (price included) and ordered for the GROUP BY.
	{"market_orders", "idx_mo_gap", "(orderState, volRemaining, typeID, bid, price)",
		"the trade-route gap aggregate"},

	// WHERE groupID IN (...) AND published = 1 AND marketGroupID > 0 ORDER BY typeName,
	// from fit generation and the build book.
	{"invTypes", "idx_invtypes_group_pub", "(groupID, published, marketGroupID, typeName)",
		"the market catalogue lookup"},

	// ── growing tables ───────────────────────────────────────────────────────
	// entity has only PRIMARY(itemID) and KEY(typeID). Container listings filter locationID +
	// flag; asset lookups and character purges filter ownerID.
	{"entity", "idx_entity_location_flag", "(locationID, flag)",
		"every hangar, fitting and container listing"},
	{"entity", "idx_entity_owner", "(ownerID)",
		"asset lookups and character purges"},

	// ── round three: skill-level lookups ─────────────────────────────────────
	// GetCharacterSkillLevel, GetEffectiveCharAttr and ESPGetSkillCatalogue filter ownerID +
	// typeID + flag IN (7, 61). The ownerID-only indexes walk the pilot's whole inventory.
	{"entity", "idx_entity_owner_type", "(ownerID, typeID, flag)",
		"every skill-level lookup"},

	// Checked and not added: killmails, killmail_attackers, bills, chrSkillQueue,
	// entity_attributes and the standings tables already have covering keys.
	// See docs/db-notes.md#performance-indexes.

	// ── retention sweeps (historyretention.go, pricehistoryroll.go) ─────────

	// GetNewPriceHistory forces this index: a short index-only range over the days not yet
	// rolled. The bot module creates the same name with ADD KEY IF NOT EXISTS.
	// See docs/db-notes.md#price-history-index.
	{"market_transactions", "idx_mt_type_item_date",
		"(transactionType, typeID, transactionDateTime, quantity, price, regionID)",
		"the price chart's raw half"},

	// The closed-escrow sweep deletes by state + dateCompleted; the shipped indexes pair
	// state with a location.
	{"escrows", "idx_escrow_state_completed", "(state, dateCompleted)",
		"the closed-escrow retention sweep"},

	// The keep-window prune filters historyDate alone; the table is keyed
	// (regionID, typeID, historyDate).
	{"market_history_old", "idx_mho_date", "(historyDate)",
		"the price-history keep window"},

	// The dashboard's economy panel reads the trailing seven days of every journal
	// (ESPDashEconomyLoad); the shipped indexes all lead with an owner column.
	{"market_journal", "idx_mj_date", "(transDate)",
		"the dashboard's seven-day ISK faucet and sink window"},
}

// perfDropIndexes are indexes that exist and should not.
var perfDropIndexes = []perfIndex{
	// Byte-for-byte duplicates of regionID and typeID: never chosen, still maintained on
	// every write. No foreign key depends on them.
	{table: "market_orders", name: "regionID_2"},
	{table: "market_orders", name: "typeID_2"},
}

// EnsurePerfIndexes applies perfIndexes, converts ramassemblylines to InnoDB if needed and
// drops perfDropIndexes. Idempotent. A failing statement is logged and skipped: a missing
// index only slows the server, and this runs before the listener opens.
func EnsurePerfIndexes(ctx context.Context, db *sql.DB) error {
	start := time.Now()
	built := 0

	for _, ix := range perfIndexes {
		// Use the server's spelling of the table: the shipped dump lowercases names, and DDL
		// against the wrong case fails when lower_case_table_names=0.
		real, err := realTableName(ctx, db, ix.table)
		if err != nil {
			return fmt.Errorf("EnsurePerfIndexes probe %s: %w", ix.table, err)
		}
		if real == "" {
			continue
		}
		ix.table = real
		created, err := ensureIndex(ctx, db, ix)
		if err != nil {
			log.Printf("[perf] %s.%s: %v (non-fatal)", ix.table, ix.name, err)
			continue
		}
		if created {
			built++
		}
	}

	for _, ix := range perfDropIndexes {
		dropped, err := dropIndexIfExists(ctx, db, ix.table, ix.name)
		if err != nil {
			log.Printf("[perf] drop %s.%s: %v (non-fatal)", ix.table, ix.name, err)
			continue
		}
		if dropped {
			log.Printf("[perf] dropped duplicate index %s.%s", ix.table, ix.name)
			built++
		}
	}

	if err := ensureAssemblyLinesInnoDB(ctx, db); err != nil {
		log.Printf("[perf] ramassemblylines engine: %v (non-fatal)", err)
	}

	if built > 0 {
		log.Printf("[perf] %d index change(s) applied in %s", built, time.Since(start).Round(time.Millisecond))
	}
	return nil
}

// ensureIndex creates one index if information_schema shows it absent (there is no
// CREATE INDEX IF NOT EXISTS). Reports whether it built it. Online DDL can still take
// minutes on a large table, so start and end are logged.
func ensureIndex(ctx context.Context, db *sql.DB, ix perfIndex) (bool, error) {
	var have int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE() AND LOWER(TABLE_NAME) = LOWER(?)
		  AND INDEX_NAME = ?`,
		ix.table, ix.name).Scan(&have); err != nil {
		return false, fmt.Errorf("probe: %w", err)
	}
	if have > 0 {
		return false, nil
	}

	log.Printf("[perf] building %s.%s %s — %s; this can take a while on a large table",
		ix.table, ix.name, ix.columns, ix.why)
	start := time.Now()

	// ALGORITHM=INPLACE, LOCK=NONE is stated so the statement fails rather than
	// silently falling back to a table copy that would block the live server.
	stmt := fmt.Sprintf("ALTER TABLE `%s` ADD INDEX `%s` %s, ALGORITHM=INPLACE, LOCK=NONE",
		ix.table, ix.name, ix.columns)
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		// A MyISAM table cannot do INPLACE/LOCK=NONE. Retry plainly for those:
		// they are small seed tables here, and the alternative is no index.
		if isUnsupportedAlgorithm(err) {
			plain := fmt.Sprintf("ALTER TABLE `%s` ADD INDEX `%s` %s", ix.table, ix.name, ix.columns)
			if _, err2 := db.ExecContext(ctx, plain); err2 != nil {
				return false, fmt.Errorf("create (after INPLACE refused): %w", err2)
			}
		} else {
			return false, fmt.Errorf("create: %w", err)
		}
	}
	log.Printf("[perf] %s.%s built in %s", ix.table, ix.name, time.Since(start).Round(time.Millisecond))
	return true, nil
}

// dropIndexIfExists removes an index if present. Reports whether it dropped it.
func dropIndexIfExists(ctx context.Context, db *sql.DB, table, name string) (bool, error) {
	table, err := realTableName(ctx, db, table)
	if err != nil || table == "" {
		return false, err
	}
	var have int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE() AND LOWER(TABLE_NAME) = LOWER(?)
		  AND INDEX_NAME = ?`,
		table, name).Scan(&have); err != nil {
		return false, fmt.Errorf("probe: %w", err)
	}
	if have == 0 {
		return false, nil
	}
	if _, err := db.ExecContext(ctx,
		fmt.Sprintf("ALTER TABLE `%s` DROP INDEX `%s`", table, name)); err != nil {
		return false, fmt.Errorf("drop: %w", err)
	}
	return true, nil
}

// ensureAssemblyLinesInnoDB converts ramassemblylines from MyISAM to InnoDB, a locking
// rebuild. Industry installs and deliveries write this table, and a MyISAM write lock
// serialises them all. Other MyISAM tables are read-only seed data and stay.
func ensureAssemblyLinesInnoDB(ctx context.Context, db *sql.DB) error {
	table, err := realTableName(ctx, db, "ramassemblylines")
	if err != nil || table == "" {
		return err
	}
	var engine sql.NullString
	err = db.QueryRowContext(ctx, `
		SELECT ENGINE FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`, table).Scan(&engine)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("engine probe: %w", err)
	}
	if !engine.Valid || strings.EqualFold(engine.String, "InnoDB") {
		return nil
	}

	log.Printf("[perf] converting ramassemblylines from %s to InnoDB — this LOCKS the table "+
		"for the rebuild (seconds); every industry action serialises on a MyISAM write lock "+
		"until it is done", engine.String)
	start := time.Now()
	if _, err := db.ExecContext(ctx,
		fmt.Sprintf("ALTER TABLE `%s` ENGINE=InnoDB", table)); err != nil {
		return fmt.Errorf("convert: %w", err)
	}
	log.Printf("[perf] ramassemblylines converted in %s", time.Since(start).Round(time.Second))
	return nil
}

// realTableName returns the table's spelling in this schema, or "" if it does not exist.
// A missing table is not an error. The case comes from the server because the shipped dump
// lowercases names, and an ALTER against the wrong case fails.
func realTableName(ctx context.Context, db *sql.DB, table string) (string, error) {
	var name string
	err := db.QueryRowContext(ctx, `
		SELECT TABLE_NAME FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = DATABASE() AND LOWER(TABLE_NAME) = ?
		LIMIT 1`, strings.ToLower(table)).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("realTableName %s: %w", table, err)
	}
	return name, nil
}

// isUnsupportedAlgorithm reports whether an ALTER failed because the storage
// engine cannot do the requested ALGORITHM/LOCK, rather than for a real reason.
// MySQL raises ER_ALTER_OPERATION_NOT_SUPPORTED (1845) and MariaDB 1845/1846.
func isUnsupportedAlgorithm(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "1845") || strings.Contains(s, "1846") ||
		strings.Contains(s, "ALGORITHM=INPLACE is not supported") ||
		strings.Contains(s, "LOCK=NONE is not supported")
}
