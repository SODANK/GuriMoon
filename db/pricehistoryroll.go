package db

// Price-history rollup. The client concatenates GetOldPriceHistory (market_history_old) and
// GetNewPriceHistory (raw trades). Once a UTC day is over (plus a grace hour) it is folded into
// market_history_old and the cursor market.historyRolledThrough moves past it in the same
// transaction. GetNewPriceHistory reads raw rows only from the cursor on, and no raw row is
// deleted before its day is rolled. See docs/db-10-notes.md#price-history-rollup.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	settingHistoryRolledThrough = "market.historyRolledThrough"
	settingHistoryKeepDays      = "market.historyKeepDays"

	// DefaultHistoryKeepDays is the client's longest chart window plus slack.
	DefaultHistoryKeepDays = 400

	// priceChartDays is the longest window the 2005 client draws.
	priceChartDays = 365

	// historyRollGraceSec is how long after midnight a day stays open. A trade is stamped before
	// its transaction commits, so a day is final only once nothing stamped before midnight can
	// still be in flight.
	historyRollGraceSec = 3600

	// historyRollDaysPerPass bounds one pass, so a long backlog is worked off over several hourly
	// passes.
	historyRollDaysPerPass = 14

	historyUpsertBatch = 500
	historyDeleteBatch = 2000

	unixDay = int64(86400)
)

// retentionNow is the clock every retention sweep reads, in unix seconds. A var so tests can
// fix it.
var retentionNow = func() int64 { return time.Now().Unix() }

// rolledThroughCache is the cursor as GetNewPriceHistory sees it: -1 = not
// read yet, 0 = no rollup has ever run, else unix midnight of the first day
// not yet in market_history_old. RollPriceHistory keeps it current.
var rolledThroughCache atomic.Int64

func init() { rolledThroughCache.Store(-1) }

// HistoryKeepDays is how many days of market_history_old to keep. 0 disables.
func HistoryKeepDays(ctx context.Context, db *sql.DB) int {
	return settingInt(ctx, db, settingHistoryKeepDays, DefaultHistoryKeepDays, 0, maxTxnKeepDays)
}

// HistoryRolledThrough is the rollup cursor (unix UTC midnight), 0 when no day
// has ever been rolled.
func HistoryRolledThrough(ctx context.Context, db *sql.DB) int64 {
	v, ok, err := GetServerSetting(ctx, db, settingHistoryRolledThrough)
	if err != nil || !ok {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// cachedRolledThrough is HistoryRolledThrough read once per process and then
// kept current by the rollup.
func cachedRolledThrough(ctx context.Context, db *sql.DB) int64 {
	if v := rolledThroughCache.Load(); v >= 0 {
		return v
	}
	v := HistoryRolledThrough(ctx, db)
	rolledThroughCache.Store(v)
	return v
}

// floorDay is unix UTC midnight at or before sec.
func floorDay(sec int64) int64 { return sec - ((sec%unixDay)+unixDay)%unixDay }

// newHistoryFrom is the earliest raw trade GetNewPriceHistory reads: the
// rollup cursor, never further back than the chart's year.
func newHistoryFrom(rolledThrough, now int64) int64 {
	from := floorDay(now) - priceChartDays*unixDay
	if rolledThrough > from {
		from = rolledThrough
	}
	return from
}

// rollableUntil is the first day that is NOT final yet at now.
func rollableUntil(now int64) int64 { return floorDay(now - historyRollGraceSec) }

// EnsureMarketHistorySchema widens market_history_old's volume column from INT UNSIGNED. The
// rollup adds onto seeded rows, and an overflow under STRICT_TRANS_TABLES rolls the day back
// and stalls the cursor.
func EnsureMarketHistorySchema(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS market_history_old (
		  regionID    INT UNSIGNED    NOT NULL DEFAULT 0,
		  typeID      INT UNSIGNED    NOT NULL DEFAULT 0,
		  historyDate INT UNSIGNED    NOT NULL DEFAULT 0,
		  volume      BIGINT UNSIGNED NOT NULL DEFAULT 0,
		  orders      INT UNSIGNED    NOT NULL DEFAULT 0,
		  lowPrice    DOUBLE NOT NULL DEFAULT 0,
		  highPrice   DOUBLE NOT NULL DEFAULT 0,
		  avgPrice    DOUBLE NOT NULL DEFAULT 0,
		  PRIMARY KEY (regionID, typeID, historyDate),
		  KEY idx_mho_date (historyDate)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`); err != nil {
		return fmt.Errorf("EnsureMarketHistorySchema create: %w", err)
	}
	var colType string
	err := db.QueryRowContext(ctx, `
		SELECT DATA_TYPE FROM information_schema.COLUMNS
		 WHERE TABLE_SCHEMA = DATABASE()
		   AND TABLE_NAME = 'market_history_old' AND COLUMN_NAME = 'volume'`).Scan(&colType)
	if err != nil {
		return fmt.Errorf("EnsureMarketHistorySchema probe: %w", err)
	}
	if !strings.EqualFold(colType, "bigint") {
		if _, err := db.ExecContext(ctx, `
			ALTER TABLE market_history_old
			  MODIFY volume BIGINT UNSIGNED NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("EnsureMarketHistorySchema widen volume: %w", err)
		}
		log.Printf("[market] market_history_old.volume widened to BIGINT UNSIGNED")
	}
	return nil
}

// PriceHistoryRollResult is what one pass did, for the log line.
type PriceHistoryRollResult struct {
	Days          int   // days folded
	Rows          int64 // (region, type, day) rows written or merged
	Pruned        int64 // market_history_old rows past the keep window
	RolledThrough int64 // the cursor after the pass
}

// RollPriceHistory folds every finished day since the cursor into
// market_history_old, then prunes that table to its keep window.
func RollPriceHistory(ctx context.Context, db *sql.DB) (PriceHistoryRollResult, error) {
	var res PriceHistoryRollResult
	now := retentionNow()
	cursor := HistoryRolledThrough(ctx, db)
	if cursor == 0 {
		start, ok, err := firstRawTradeDay(ctx, db)
		if err != nil {
			return res, err
		}
		if !ok {
			res.Pruned, err = pruneMarketHistory(ctx, db, now)
			return res, err
		}
		cursor = start
	}
	// The chart shows at most a year, so the rollup reaches no further back; the wallet sweep drops
	// older rows without rolling them.
	if floor := floorDay(now) - (priceChartDays+1)*unixDay; cursor < floor {
		cursor = floor
	}

	until := rollableUntil(now)
	for cursor < until && res.Days < historyRollDaysPerPass {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		n, err := rollDay(ctx, db, cursor)
		if err != nil {
			res.RolledThrough = cursor
			return res, err
		}
		cursor += unixDay
		rolledThroughCache.Store(cursor)
		res.Days++
		res.Rows += n
	}
	res.RolledThrough = cursor

	var err error
	res.Pruned, err = pruneMarketHistory(ctx, db, now)
	return res, err
}

// firstRawTradeDay is the UTC day of the oldest buy row still in
// market_transactions (idx_mt_type_date_agg makes this one index probe).
func firstRawTradeDay(ctx context.Context, db *sql.DB) (int64, bool, error) {
	var first sql.NullInt64
	if err := db.QueryRowContext(ctx, `
		SELECT MIN(transactionDateTime) FROM market_transactions
		 WHERE transactionType = 1`).Scan(&first); err != nil {
		return 0, false, fmt.Errorf("price history start: %w", err)
	}
	if !first.Valid || first.Int64 <= 0 {
		return 0, false, nil
	}
	return floorDay(Win32ToUnix(first.Int64)), true, nil
}

type historyDayRow struct {
	regionID, typeID, volume, orders int64
	low, high, avg                   float64
}

// rollDay folds one UTC day. The aggregate is a plain consistent read; the upserts and the
// cursor share one transaction, so the day is either rolled and behind the cursor or neither.
func rollDay(ctx context.Context, db *sql.DB, day int64) (int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT regionID, typeID, MIN(price), MAX(price), AVG(price),
		       SUM(quantity), COUNT(*)
		  FROM market_transactions
		 WHERE transactionType = 1
		   AND transactionDateTime >= ? AND transactionDateTime < ?
		 GROUP BY regionID, typeID`,
		UnixToWin32(day), UnixToWin32(day+unixDay))
	if err != nil {
		return 0, fmt.Errorf("price history day %d: %w", day, err)
	}
	var out []historyDayRow
	for rows.Next() {
		var r historyDayRow
		if err := rows.Scan(&r.regionID, &r.typeID, &r.low, &r.high, &r.avg, &r.volume, &r.orders); err != nil {
			rows.Close()
			return 0, fmt.Errorf("price history day %d scan: %w", day, err)
		}
		if r.volume < 0 {
			r.volume = 0
		}
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("price history day %d rows: %w", day, err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("price history day %d begin: %w", day, err)
	}
	defer tx.Rollback()

	for i := 0; i < len(out); i += historyUpsertBatch {
		chunk := out[i:min(i+historyUpsertBatch, len(out))]
		var b strings.Builder
		args := make([]any, 0, len(chunk)*8)
		b.WriteString(`INSERT INTO market_history_old
			  (regionID, typeID, historyDate, volume, orders, lowPrice, highPrice, avgPrice)
			VALUES `)
		for j, r := range chunk {
			if j > 0 {
				b.WriteString(",")
			}
			b.WriteString("(?,?,?,?,?,?,?,?)")
			args = append(args, r.regionID, r.typeID, day, r.volume, r.orders, r.low, r.high, r.avg)
		}
		// A key collision is a seeded row for the same day. Assignments run left to right and see
		// earlier results, so avgPrice weights by the old orders before orders is added to.
		b.WriteString(` ON DUPLICATE KEY UPDATE
			  avgPrice  = (avgPrice * orders + VALUES(avgPrice) * VALUES(orders))
			              / GREATEST(orders + VALUES(orders), 1),
			  lowPrice  = LEAST(lowPrice, VALUES(lowPrice)),
			  highPrice = GREATEST(highPrice, VALUES(highPrice)),
			  volume    = volume + VALUES(volume),
			  orders    = orders + VALUES(orders)`)
		if _, err := tx.ExecContext(ctx, b.String(), args...); err != nil {
			return 0, fmt.Errorf("price history day %d upsert: %w", day, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO server_settings (name, value) VALUES (?, ?)
		ON DUPLICATE KEY UPDATE value = VALUES(value)`,
		settingHistoryRolledThrough, strconv.FormatInt(day+unixDay, 10)); err != nil {
		return 0, fmt.Errorf("price history day %d cursor: %w", day, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("price history day %d commit: %w", day, err)
	}
	return int64(len(out)), nil
}

// pruneMarketHistory drops market_history_old rows past the keep window, in
// batches over idx_mho_date.
func pruneMarketHistory(ctx context.Context, db *sql.DB, now int64) (int64, error) {
	days := HistoryKeepDays(ctx, db)
	if days <= 0 {
		return 0, nil
	}
	cutoff := floorDay(now) - int64(days)*unixDay
	var total int64
	for {
		r, err := db.ExecContext(ctx, `
			DELETE FROM market_history_old WHERE historyDate < ? LIMIT ?`,
			cutoff, historyDeleteBatch)
		if err != nil {
			return total, fmt.Errorf("price history prune: %w", err)
		}
		n, _ := r.RowsAffected()
		total += n
		if n < historyDeleteBatch || ctx.Err() != nil {
			return total, ctx.Err()
		}
	}
}

// LogPriceHistoryRoll prints one line, and only when the pass did something.
func LogPriceHistoryRoll(res PriceHistoryRollResult, err error) {
	if err != nil {
		log.Printf("[price-history] %v (%d day(s) rolled before the error)", err, res.Days)
		return
	}
	if res.Days == 0 && res.Pruned == 0 {
		return
	}
	log.Printf("[price-history] rolled %d day(s) into market_history_old (%d row(s)), now through %s; pruned %d old row(s)",
		res.Days, res.Rows, time.Unix(res.RolledThrough, 0).UTC().Format("2006-01-02"), res.Pruned)
}
