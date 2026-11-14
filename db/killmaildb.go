package db

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"
)

// Kill report mails: on player ship or pod destruction CONCORD (ownerID 1000125) mails a kill
// report to the victim and the final-blow pilot. The body is built in main.go; this file owns
// the evemail persistence and the name lookups the builder needs.

// MailboxSuppressed is set by a module to skip a recipient's mailbox entirely: no evemail row
// and no evemailDetails row. It exists for synthetic pilots, who never read mail. Nil in a plain
// build, which mails everybody. A func var because db cannot import modapi (modapi -> esp -> db).
var MailboxSuppressed func(recipientID int64) bool

// SendEveMail inserts a server-originated mail into a character's mailbox and returns the new
// messageID for the OnMessage push. senderID must be a real owner, since the client looks the
// sender up. A suppressed mailbox returns messageID 0 and a nil error, which callers treat as
// nothing to push.
func SendEveMail(ctx context.Context, db *sql.DB, recipientID, senderID int64, subject, body string, created int64) (int64, error) {
	if MailboxSuppressed != nil && MailboxSuppressed(recipientID) {
		return 0, nil
	}
	res, err := db.ExecContext(ctx,
		`INSERT INTO evemail (channelID, senderID, subject, created, `+"`read`"+`) VALUES (?, ?, ?, ?, 0)`,
		recipientID, senderID, subject, created)
	if err != nil {
		return 0, fmt.Errorf("SendEveMail evemail: %w", err)
	}
	msgID, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("SendEveMail LastInsertId: %w", err)
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO evemailDetails (messageID, mimeTypeID, attachment) VALUES (?, 1, ?)`,
		msgID, body)
	if err != nil {
		return 0, fmt.Errorf("SendEveMail evemailDetails: %w", err)
	}
	return msgID, nil
}

// SendKillMail inserts a CONCORD kill report into a character's mailbox.
func SendKillMail(ctx context.Context, db *sql.DB, recipientID int64, subject, body string, created int64) (int64, error) {
	return SendEveMail(ctx, db, recipientID, OwnerCONCORD, subject, body, created)
}

// EnsureMailIndexes adds the composite (channelID, messageID) index behind every inbox open
// (WHERE channelID = ? ORDER BY messageID DESC); the shipped dump keys evemail on messageID
// alone. MySQL has no CREATE INDEX IF NOT EXISTS, so the index is probed through
// information_schema first. It runs at boot before the listener opens and logs both ends.
func EnsureMailIndexes(ctx context.Context, db *sql.DB) error {
	var have int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'evemail'
		  AND INDEX_NAME = 'idx_evemail_channel'`).Scan(&have); err != nil {
		return fmt.Errorf("EnsureMailIndexes check: %w", err)
	}
	if have > 0 {
		return nil
	}
	log.Printf("[mail] building idx_evemail_channel — this can take a while on a large mailbox table")
	start := time.Now()
	if _, err := db.ExecContext(ctx, `
		CREATE INDEX idx_evemail_channel ON evemail (channelID, messageID)`); err != nil {
		return fmt.Errorf("EnsureMailIndexes create: %w", err)
	}
	log.Printf("[mail] idx_evemail_channel built in %s", time.Since(start).Round(time.Millisecond))
	return nil
}

// win32FiletimeFloor is the smallest `created` value a correctly written evemail row holds.
// Anything below it is a Unix timestamp. The client needs a long for FmtDate, and appendInt
// encodes values under 0x7FFFFFFF as a 4-byte int, so a stray Unix value fails on the wire.
const win32FiletimeFloor = 1e14

// RepairMailTimestamps rewrites evemail rows whose created column is Unix-second scale into a
// Win32 FILETIME. It runs once per boot and is a no-op when no such row remains.
func RepairMailTimestamps(ctx context.Context, db *sql.DB) error {
	res, err := db.ExecContext(ctx,
		`UPDATE evemail SET created = created * 10000000 + 116444736000000000
		 WHERE created > 0 AND created < ?`, uint64(win32FiletimeFloor))
	if err != nil {
		return fmt.Errorf("RepairMailTimestamps: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("[mail] repaired %d evemail row(s) with Unix-scale created timestamps", n)
	}
	return nil
}

// DeleteMailboxes removes every evemail and evemailDetails row addressed to or sent by the
// given characters, the scope DeleteCharacter uses. It serves callers that delete characters
// without DeleteCharacter. Returns the number of evemail rows removed.
func DeleteMailboxes(ctx context.Context, db *sql.DB, charIDs []int64) (int64, error) {
	var total int64
	// Chunked, since a roster wipe can pass thousands of IDs.
	const chunk = 500
	for start := 0; start < len(charIDs); start += chunk {
		end := start + chunk
		if end > len(charIDs) {
			end = len(charIDs)
		}
		ids := charIDs[start:end]
		ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
		// Both statements take the ID list twice (channelID and senderID), so the args are doubled.
		args := make([]any, 0, len(ids)*2)
		for i := 0; i < 2; i++ {
			for _, id := range ids {
				args = append(args, id)
			}
		}
		if _, err := db.ExecContext(ctx, `
			DELETE FROM evemailDetails WHERE messageID IN (
				SELECT messageID FROM evemail
				WHERE channelID IN (`+ph+`) OR senderID IN (`+ph+`))`, args...); err != nil {
			return total, fmt.Errorf("DeleteMailboxes details: %w", err)
		}
		res, err := db.ExecContext(ctx,
			`DELETE FROM evemail WHERE channelID IN (`+ph+`) OR senderID IN (`+ph+`)`, args...)
		if err != nil {
			return total, fmt.Errorf("DeleteMailboxes evemail: %w", err)
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}

// GetCorporationName resolves a corp name for kill reports. Player corps live
// in the corporation table; NPC faction corps fall back to eveNames.
func GetCorporationName(ctx context.Context, db *sql.DB, corpID int64) string {
	var name string
	err := db.QueryRowContext(ctx,
		`SELECT corporationName FROM corporation WHERE corporationID = ?`, corpID).Scan(&name)
	if err == nil && name != "" {
		return name
	}
	err = db.QueryRowContext(ctx,
		`SELECT itemName FROM eveNames WHERE itemID = ?`, corpID).Scan(&name)
	if err != nil {
		return ""
	}
	return name
}

// GetSolarSystemName returns the name of a solar system.
func GetSolarSystemName(ctx context.Context, db *sql.DB, systemID int64) string {
	var name string
	err := db.QueryRowContext(ctx,
		`SELECT solarSystemName FROM mapSolarSystems WHERE solarSystemID = ?`, systemID).Scan(&name)
	if err != nil {
		return ""
	}
	return name
}
