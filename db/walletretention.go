package db

// Wallet retention: sweeps for market_journal (newest N rows per character) and
// market_transactions (by age, clamped to the daily rollup cursor). Every window is a setting and
// 0 disables its sweep.
// See docs/db-8-notes.md#wallet-retention.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
)

const (
	settingJournalKeepRows    = "wallet.journalKeepRows"
	settingBotJournalKeepRows = "wallet.botJournalKeepRows"
	settingTxnKeepDays        = "wallet.transactionKeepDays"
	settingBotTxnKeepDays     = "wallet.botTransactionKeepDays"

	// DefaultJournalKeepRows is a player's per-character cap, four times what GetCharJournal serves.
	DefaultJournalKeepRows = 2000

	// DefaultBotJournalKeepRows is the cap for a synthetic pilot. It bounds the table, since the rows
	// are spread over the whole roster rather than a few characters.
	DefaultBotJournalKeepRows = 200

	// DefaultTxnKeepDays is the age window for a player's rows; the Transactions tab pages back with
	// no date limit (GetCharTransactionsBefore).
	DefaultTxnKeepDays = 365

	// DefaultBotTxnKeepDays covers the longest reader of other owners' rows: the 30-day price readers
	// (typevalue.go, skillpurchasedb.go, the bot module) plus slack for the hourly sweep.
	DefaultBotTxnKeepDays = 32
)

// WalletRetentionResult is what one sweep did, for the log line.
type WalletRetentionResult struct {
	JournalRows int64
	TxnRows     int64 // past the player window, anyone's
	BotTxnRows  int64 // past the bot window, non-players' only
	KeepRows    int
	BotKeepRows int
	KeepDays    int
	BotKeepDays int
}

// The clamp floors are 0 because 0 means disabled; settingInt clamps rather than rejects, so a
// floor of 1 would turn "off" into "keep one row".
const (
	maxJournalKeepRows = 1000000
	maxTxnKeepDays     = 36500
)

// JournalKeepRows is how many journal rows to keep per character. 0 disables.
func JournalKeepRows(ctx context.Context, db *sql.DB) int {
	return settingInt(ctx, db, settingJournalKeepRows,
		DefaultJournalKeepRows, 0, maxJournalKeepRows)
}

// BotJournalKeepRows is the same for a synthetic pilot. 0 disables the synthetic half only; the
// player cap still applies to everyone.
func BotJournalKeepRows(ctx context.Context, db *sql.DB) int {
	return settingInt(ctx, db, settingBotJournalKeepRows,
		DefaultBotJournalKeepRows, 0, maxJournalKeepRows)
}

// TxnKeepDays is how many days of a player's market transactions to keep.
// 0 disables.
func TxnKeepDays(ctx context.Context, db *sql.DB) int {
	return settingInt(ctx, db, settingTxnKeepDays,
		DefaultTxnKeepDays, 0, maxTxnKeepDays)
}

// BotTxnKeepDays is the same for every owner that is not a player. 0 disables
// that half; the player window still applies to everyone.
func BotTxnKeepDays(ctx context.Context, db *sql.DB) int {
	return settingInt(ctx, db, settingBotTxnKeepDays,
		DefaultBotTxnKeepDays, 0, maxTxnKeepDays)
}

// walletDeleteBatch bounds a single DELETE so a sweep holds locks briefly. See txretry.go.
const walletDeleteBatch = 2000

// SweepWalletRetention trims both wallet tables and reports what it removed. A failure on one
// table does not stop the other.
func SweepWalletRetention(ctx context.Context, db *sql.DB) (WalletRetentionResult, error) {
	res := WalletRetentionResult{
		KeepRows:    JournalKeepRows(ctx, db),
		BotKeepRows: BotJournalKeepRows(ctx, db),
		KeepDays:    TxnKeepDays(ctx, db),
		BotKeepDays: BotTxnKeepDays(ctx, db),
	}
	var firstErr error

	n, err := sweepJournalRows(ctx, db, res.KeepRows, res.BotKeepRows)
	res.JournalRows = n
	if err != nil {
		firstErr = err
	}
	rolled := HistoryRolledThrough(ctx, db)
	now := retentionNow()
	n, err = sweepOldTransactions(ctx, db, txnCutoff(now, res.KeepDays, rolled), nil)
	res.TxnRows = n
	if err != nil && firstErr == nil {
		firstErr = err
	}
	// The non-player half needs the player set. With no bot module every account character is a
	// player, so there is nothing to do.
	if SyntheticPilot != nil {
		if cut := txnCutoff(now, res.BotKeepDays, rolled); cut > 0 {
			players, err := playerCharacterIDs(ctx, db)
			if err == nil {
				n, err = sweepOldTransactions(ctx, db, cut, players)
				res.BotTxnRows = n
			}
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return res, firstErr
}

// sweepJournalRows keeps the newest N rows per character: keep for a player, botKeep for a
// synthetic pilot (SyntheticPilot, nil when no module is installed).
// The HAVING pass scans idx_mj_char_date at the smaller enabled cap, then each over-cap
// character is trimmed by a cutoff lookup and batched DELETE.
func sweepJournalRows(ctx context.Context, db *sql.DB, keep, botKeep int) (int64, error) {
	// The scan threshold is the lowest cap that is switched on, so a disabled half never lowers the
	// other's floor to zero.
	scanAt := keep
	if botKeep > 0 && (scanAt <= 0 || botKeep < scanAt) {
		scanAt = botKeep
	}
	if scanAt <= 0 {
		return 0, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT characterID FROM market_journal
		 GROUP BY characterID HAVING COUNT(*) > ?`, scanAt)
	if err != nil {
		return 0, fmt.Errorf("journal retention scan: %w", err)
	}
	var chars []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		chars = append(chars, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	var total int64
	for _, charID := range chars {
		keepN := keep
		if isSyntheticPilot(charID) && botKeep > 0 {
			keepN = botKeep
		}
		if keepN <= 0 {
			continue
		}
		// The transDate of the oldest row worth keeping. Everything strictly older goes; ties on
		// transDate are kept.
		var cutoff sql.NullInt64
		err := db.QueryRowContext(ctx, `
			SELECT transDate FROM market_journal
			 WHERE characterID = ?
			 ORDER BY transDate DESC
			 LIMIT 1 OFFSET ?`, charID, keepN-1).Scan(&cutoff)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && !cutoff.Valid) {
			continue
		}
		if err != nil {
			return total, fmt.Errorf("journal retention cutoff char %d: %w", charID, err)
		}
		for {
			r, err := db.ExecContext(ctx, `
				DELETE FROM market_journal
				 WHERE characterID = ? AND transDate < ?
				 LIMIT ?`, charID, cutoff.Int64, walletDeleteBatch)
			if err != nil {
				return total, fmt.Errorf("journal retention delete char %d: %w", charID, err)
			}
			n, _ := r.RowsAffected()
			total += n
			if n < walletDeleteBatch {
				break
			}
			if ctx.Err() != nil {
				return total, ctx.Err()
			}
		}
	}
	return total, nil
}

// txnCutoff is the unix second before which a window of days lets rows go, clamped to the rollup
// cursor so no row leaves before its day is in market_history_old. 0 means delete nothing.
func txnCutoff(now int64, days int, rolledThrough int64) int64 {
	if days <= 0 || rolledThrough <= 0 {
		return 0
	}
	cut := now - int64(days)*unixDay
	if cut > rolledThrough {
		cut = rolledThrough
	}
	return cut
}

// playerCharacterIDs is every account character the bot module does not claim. The list is short
// enough to ride a NOT IN.
func playerCharacterIDs(ctx context.Context, db *sql.DB) ([]int64, error) {
	rows, err := db.QueryContext(ctx, `SELECT characterID FROM character_ WHERE accountID > 0`)
	if err != nil {
		return nil, fmt.Errorf("transaction retention players: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if !isSyntheticPilot(id) {
			out = append(out, id)
		}
	}
	return out, rows.Err()
}

// sweepOldTransactions drops transactions stamped before cutoff (unix seconds) in batches,
// sparing the owners in keep.
// The "transactionType IN (0, 1)" term matches every row; it lets the range use
// idx_mt_type_date_agg.
func sweepOldTransactions(ctx context.Context, db *sql.DB, cutoff int64, keep []int64) (int64, error) {
	if cutoff <= 0 {
		return 0, nil
	}
	q := `DELETE FROM market_transactions
		 WHERE transactionType IN (0, 1) AND transactionDateTime < ?`
	args := []any{UnixToWin32(cutoff)}
	if len(keep) > 0 {
		q += ` AND characterID NOT IN (` + strings.TrimSuffix(strings.Repeat("?,", len(keep)), ",") + `)`
		for _, id := range keep {
			args = append(args, id)
		}
	}
	q += ` LIMIT ?`
	args = append(args, walletDeleteBatch)
	var total int64
	for {
		r, err := db.ExecContext(ctx, q, args...)
		if err != nil {
			return total, fmt.Errorf("transaction retention delete: %w", err)
		}
		n, _ := r.RowsAffected()
		total += n
		if n < walletDeleteBatch {
			return total, nil
		}
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
	}
}

// LogWalletRetention prints one line per sweep, and only when it removed something.
func LogWalletRetention(res WalletRetentionResult, err error) {
	if err != nil {
		log.Printf("[wallet-retention] %v (journal %d, transactions %d removed before the error)",
			err, res.JournalRows, res.TxnRows)
		return
	}
	if res.JournalRows == 0 && res.TxnRows == 0 && res.BotTxnRows == 0 {
		return
	}
	log.Printf("[wallet-retention] removed %d journal row(s) beyond the newest %d per pilot (%d for synthetic), %d transaction(s) older than %dd, %d non-player transaction(s) older than %dd",
		res.JournalRows, res.KeepRows, res.BotKeepRows, res.TxnRows, res.KeepDays, res.BotTxnRows, res.BotKeepDays)
}
