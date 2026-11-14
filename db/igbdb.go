package db

// In-game browser (Galnet) queries, served by the esp news server on port 80
// to the client's built-in browser.

import (
	"context"
	"database/sql"
)

// IGBCharacterBalance returns a character's wallet balance, 0 if unknown.
func IGBCharacterBalance(ctx context.Context, db *sql.DB, charID int64) float64 {
	var bal float64
	db.QueryRowContext(ctx,
		`SELECT balance FROM character_ WHERE characterID = ?`, charID).Scan(&bal)
	return bal
}
