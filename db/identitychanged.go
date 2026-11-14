package db

import (
	"context"
	"database/sql"
)

// IdentityChanged is told which characters' public identity (corporation,
// alliance, titles) changed. main installs infocache.Shared.InvalidateID to
// drop their memoised Show Info answers. Nil does nothing.
var IdentityChanged func(charID int64)

func identityChanged(charIDs ...int64) {
	if hook := IdentityChanged; hook != nil {
		for _, id := range charIDs {
			hook(id)
		}
	}
}

// corpIdentityChanged is identityChanged for every pilot of corpID, for an
// alliance join or leave that changes all of them at once.
func corpIdentityChanged(ctx context.Context, db *sql.DB, corpID int64) {
	if IdentityChanged == nil || corpID == 0 {
		return
	}
	rows, err := db.QueryContext(ctx,
		`SELECT characterID FROM character_ WHERE corporationID = ? AND accountID > 0`, corpID)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			identityChanged(id)
		}
	}
}
