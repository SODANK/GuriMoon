package db

import (
	"context"
	"database/sql"
	"fmt"
)

// clientMessageRows are catalogue rows (config.BulkData.messages, served from eveMessages) that
// the shipped dump lacks: CustomInfo and the ErrMessage* fallbacks cfg.GetMessage uses.
// EnsureClientMessages inserts them with INSERT IGNORE. See docs/db-13-notes.md#client-message-rows.
var clientMessageRows = [][3]string{
	{"CustomInfo", "infomodal", "%(info)s"},
	{"ErrMessageNotFound", "infomodal", "Message Not Found::The message %(msgid)s could not be found.<br><br>Arguments: %(args)s"},
	{"ErrMessageKeyError", "infomodal", "Message Arguments::The message %(msgid)s could not be shown: argument %(err)s is missing.<br><br>%(text)s<br><br>Arguments: %(args)s"},
	{"ErrMessageDictMissing", "infomodal", "Message Arguments::The message %(msgid)s needs arguments that were not given.<br><br>%(text)s"},
	// The bystander combat log (cmd/gurimoon/combatobserver.go). The client
	// ships lines only for the two parties of a hit; these are what everyone
	// else on the grid reads. Names arrive pre-resolved as strings.
	{"CombatObservedHit", "notify", "<color=0xffaaaaaa>%(attacker)s hits %(victim)s, doing %(damage)0.1f damage."},
	{"CombatObservedHits", "notify", "<color=0xffaaaaaa>%(attacker)s hits %(victim)s %(hits)d times, doing %(damage)0.1f damage."},
	{"CombatObservedKill", "notify", "<color=0xffdddddd>%(victim)s is destroyed by %(attacker)s."},
}

// EnsureClientMessages adds the catalogue rows above if they are missing.
func EnsureClientMessages(ctx context.Context, db *sql.DB) error {
	for _, r := range clientMessageRows {
		if _, err := db.ExecContext(ctx,
			`INSERT IGNORE INTO eveMessages (messageID, messageType, messageText, urlAudio, urlIcon)
			 VALUES (?, ?, ?, '', '')`, r[0], r[1], r[2]); err != nil {
			return fmt.Errorf("EnsureClientMessages %s: %w", r[0], err)
		}
	}
	return nil
}
