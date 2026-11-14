package lsc

import (
	"context"
	"database/sql"
	"log"
	"strings"
	"sync"
)

// Shared mailboxes: the inbox's Corp and Alliance tabs.
//
// The client draws a tab per mailbox it may read (inbox.py InitTabGroup: charid,
// corpid and, when allied, allianceid) and fills each by filtering one GetMyMessages
// rowset on channelID, so the server serves all three. A shared mailbox holds one
// evemail row per message, so its `read` column cannot be per pilot, and the client
// blinks the neocom for every unread row. Read state for shared rows lives in
// evemailRead; the personal mailbox keeps its column. Posting to and reading from
// a shared mailbox require membership.

// mailScope is who a pilot is for mailbox purposes.
type mailScope struct {
	charID, corpID, allianceID int64
}

func loadMailScope(ctx context.Context, sqldb *sql.DB, charID int64) mailScope {
	s := mailScope{charID: charID}
	var alliance sql.NullInt64
	_ = sqldb.QueryRowContext(ctx, `
		SELECT c.corporationID, co.allianceID
		  FROM character_ c
		  LEFT JOIN corporation co ON co.corporationID = c.corporationID
		 WHERE c.characterID = ?`, charID).Scan(&s.corpID, &alliance)
	if alliance.Valid {
		s.allianceID = alliance.Int64
	}
	return s
}

// ownsMailbox reports whether id is one of this pilot's own three mailboxes.
func (s mailScope) ownsMailbox(id int64) bool {
	return id != 0 && (id == s.charID || id == s.corpID || id == s.allianceID)
}

// sharedMailboxes returns the corp and alliance mailbox IDs, 0s dropped.
func (s mailScope) sharedMailboxes() []int64 {
	var out []int64
	for _, id := range []int64{s.corpID, s.allianceID} {
		if id != 0 {
			out = append(out, id)
		}
	}
	return out
}

// Mailbox kinds, for the posting gate.
const (
	mailboxOther = iota // a character, mailing list, or unknown ID
	mailboxCorp
	mailboxAlliance
)

func classifyMailbox(ctx context.Context, sqldb *sql.DB, id int64) int {
	var one int
	if sqldb.QueryRowContext(ctx, `SELECT 1 FROM alliances WHERE allianceID = ?`, id).Scan(&one) == nil {
		return mailboxAlliance
	}
	if sqldb.QueryRowContext(ctx, `SELECT 1 FROM corporation WHERE corporationID = ?`, id).Scan(&one) == nil {
		return mailboxCorp
	}
	return mailboxOther
}

// mayPostTo is the posting gate: a corporation's or alliance's mailbox takes
// posts from its own members only. Characters and mailing lists are unchanged.
func mayPostTo(ctx context.Context, sqldb *sql.DB, s mailScope, id int64) bool {
	switch classifyMailbox(ctx, sqldb, id) {
	case mailboxCorp:
		return id == s.corpID
	case mailboxAlliance:
		return id == s.allianceID
	}
	return true
}

// isCharacterID reports whether id is a character (someone's personal
// mailbox).
func isCharacterID(ctx context.Context, sqldb *sql.DB, id int64) bool {
	var one int
	return sqldb.QueryRowContext(ctx, `SELECT 1 FROM character_ WHERE characterID = ?`, id).Scan(&one) == nil
}

// allianceCorpIDs lists the corporations in an alliance, for fanning a post
// out to every online member: Member carries a corpID but not an alliance.
func allianceCorpIDs(ctx context.Context, sqldb *sql.DB, allianceID int64) []int64 {
	rows, err := sqldb.QueryContext(ctx, `SELECT corporationID FROM corporation WHERE allianceID = ?`, allianceID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out
}

var (
	evemailReadOnce sync.Once
	evemailReadErr  error
)

func ensureEvemailRead(ctx context.Context, sqldb *sql.DB) error {
	evemailReadOnce.Do(func() {
		_, evemailReadErr = sqldb.ExecContext(ctx, `
			CREATE TABLE IF NOT EXISTS evemailRead (
			  messageID   INT UNSIGNED NOT NULL,
			  characterID INT UNSIGNED NOT NULL,
			  PRIMARY KEY (messageID, characterID)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
		if evemailReadErr != nil {
			log.Printf("lsc: evemailRead: %v", evemailReadErr)
		}
	})
	return evemailReadErr
}

// markSharedRead records that the pilot has read these messages, for the ones
// that sit in their corp or alliance mailbox.
func markSharedRead(ctx context.Context, sqldb *sql.DB, s mailScope, ids []int64) {
	shared := s.sharedMailboxes()
	if len(shared) == 0 || len(ids) == 0 || ensureEvemailRead(ctx, sqldb) != nil {
		return
	}
	idPH := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	boxPH := strings.TrimSuffix(strings.Repeat("?,", len(shared)), ",")
	args := []any{s.charID}
	for _, id := range ids {
		args = append(args, id)
	}
	for _, id := range shared {
		args = append(args, id)
	}
	if _, err := sqldb.ExecContext(ctx, `
		INSERT IGNORE INTO evemailRead (messageID, characterID)
		SELECT messageID, ? FROM evemail
		 WHERE messageID IN (`+idPH+`) AND channelID IN (`+boxPH+`)`, args...); err != nil {
		log.Printf("lsc: markSharedRead char %d: %v", s.charID, err)
	}
}
