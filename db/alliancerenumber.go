package db

// Moving an alliance onto a different ID. The client draws an alliance logo from the allianceID
// (see alliancelogoslots.go), so choosing a logo for an existing alliance means renumbering it:
// one transaction rewrites every column that can hold an allianceID (allianceRefColumns, shared
// with the player-entity migration). The target ID must be free across the whole owner space,
// not only in `alliances`, because owner columns are polymorphic.

import (
	"context"
	"database/sql"
	"fmt"
)

// AllianceIDTaken reports the kind of row already holding id, or "" when the ID
// is free for an alliance to move onto.  oldID is exempt: it is the alliance
// being moved and its own rows are what the caller is about to rewrite.
func AllianceIDTaken(ctx context.Context, db *sql.DB, id, oldID int64) (string, error) {
	for _, probe := range [][3]string{
		{"alliances", "allianceID", "an alliance"},
		{"corporation", "corporationID", "a corporation"},
		{"character_", "characterID", "a character"},
		{"entity", "itemID", "an item"},
		{"eveNames", "itemID", "a named object"},
	} {
		ok, err := tableExists(ctx, db, probe[0])
		if err != nil {
			return "", err
		}
		if !ok {
			continue
		}
		var n int
		q := fmt.Sprintf("SELECT COUNT(*) FROM `%s` WHERE `%s` = ? AND `%s` <> ?",
			probe[0], probe[1], probe[1])
		if err := db.QueryRowContext(ctx, q, id, oldID).Scan(&n); err != nil {
			return "", fmt.Errorf("AllianceIDTaken %s: %w", probe[0], err)
		}
		if n > 0 {
			return probe[2], nil
		}
	}
	return "", nil
}

// ReassignAllianceID moves an alliance from oldID to newID, rewriting every reference. It
// returns the ID it landed on and the rows rewritten; newID 0 allocates the next free
// player-entity ID. The caller refreshes the owner-name bulk cachables and the allianceid
// session value of online members.
func ReassignAllianceID(ctx context.Context, db *sql.DB, oldID, newID int64) (int64, int64, error) {
	if err := ensureAlliancesTable(ctx, db); err != nil {
		return 0, 0, err
	}
	if newID == 0 {
		var err error
		if newID, err = nextPlayerEntityID(ctx, db); err != nil {
			return 0, 0, fmt.Errorf("ReassignAllianceID next ID: %w", err)
		}
	}
	if oldID == newID {
		return newID, 0, fmt.Errorf("alliance %d is already on that ID", oldID)
	}
	if !AllianceIDIsUsable(newID) {
		return 0, 0, fmt.Errorf("ID %d is below the player-entity floor %d; cfg.IsAlliance would refuse it",
			newID, MinPlayerEntityID)
	}

	var exists int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM alliances WHERE allianceID = ?`, oldID).Scan(&exists); err != nil {
		return 0, 0, fmt.Errorf("ReassignAllianceID: %w", err)
	}
	if exists == 0 {
		return 0, 0, fmt.Errorf("alliance %d not found", oldID)
	}
	if kind, err := AllianceIDTaken(ctx, db, newID, oldID); err != nil {
		return 0, 0, err
	} else if kind != "" {
		return 0, 0, fmt.Errorf("ID %d is already used by %s", newID, kind)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	n, err := renumber(ctx, tx, db, allianceRefColumns, oldID, newID)
	if err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return newID, n, nil
}
