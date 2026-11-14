package db

// market_journal schema repair. The shipped `reason` column is VARCHAR(43) and strict mode
// (STRICT_TRANS_TABLES) rolls back the whole transaction on an over-long value (error 1406).
// EnsureJournalSchema widens the column and JournalReason clamps every reason built from a name.
// See docs/db-13-notes.md#journal-reason-width.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"unicode/utf8"
)

// JournalReasonMax is the width EnsureJournalSchema widens `reason` to, and
// the length JournalReason clamps to.
const JournalReasonMax = 255

// EnsureJournalSchema widens market_journal.reason if it is still the shipped
// VARCHAR(43).  The probe keeps the ALTER (a full table rebuild) off every
// boot but the first.
func EnsureJournalSchema(ctx context.Context, db *sql.DB) error {
	var width sql.NullInt64
	err := db.QueryRowContext(ctx, `
		SELECT CHARACTER_MAXIMUM_LENGTH FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE()
		  AND TABLE_NAME   = 'market_journal'
		  AND COLUMN_NAME  = 'reason'`).Scan(&width)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // table not created yet; nothing to widen
	}
	if err != nil {
		return fmt.Errorf("EnsureJournalSchema probe: %w", err)
	}
	if width.Valid && width.Int64 >= JournalReasonMax {
		return nil
	}
	if _, err := db.ExecContext(ctx,
		`ALTER TABLE market_journal MODIFY reason VARCHAR(255) DEFAULT NULL`); err != nil {
		return fmt.Errorf("EnsureJournalSchema widen reason: %w", err)
	}
	return nil
}

// JournalReason clamps a wallet-journal reason to the column width, cutting
// back to a rune boundary so a multi-byte name never lands half-written.
func JournalReason(s string) string {
	if len(s) <= JournalReasonMax {
		return s
	}
	b := s[:JournalReasonMax]
	for len(b) > 0 && !utf8.ValidString(b) {
		b = b[:len(b)-1]
	}
	return b
}
