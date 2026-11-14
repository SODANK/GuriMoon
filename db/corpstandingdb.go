package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"

	"gurimoon/marshal"
)

// Corporation standings: what a player corporation's directors have declared toward other
// owners. The table is crpStandings; EnsureCorpStandingTable moves legacy corp-owned
// chrStandings rows into it at boot. See docs/db-12-notes.md#corporation-standings.

// EventStandingPlayerCorpSetStanding is the eveconstants event type the client's
// FmtStandingTransaction renders as "Corp Set ... set by <int_1>".
const EventStandingPlayerCorpSetStanding = 68

// MaxCorpStandings is the client's own cap (standingsvc.SetCorpStanding raises
// MaxStandingsSet past 300), checked client-side against a cache primed at login.
const MaxCorpStandings = 300

// ErrMaxCorpStandings is returned by SetCorpStanding when a new entry would pass
// MaxCorpStandings.
var ErrMaxCorpStandings = errors.New("corporation standing limit reached")

// EnsureCorpStandingTable creates crpStandings and migrates the legacy corp-owned rows
// out of chrStandings. Idempotent: once they are gone the migration moves nothing.
func EnsureCorpStandingTable(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS crpStandings (
			corporationID BIGINT NOT NULL,
			toID          BIGINT NOT NULL,
			standing      DOUBLE NOT NULL DEFAULT 0,
			PRIMARY KEY (corporationID, toID),
			INDEX idx_toID (toID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`); err != nil {
		return fmt.Errorf("create crpStandings: %w", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The JOIN identifies a corp-owned row: chrStandings' owner column otherwise holds
	// characterIDs.
	res, err := tx.ExecContext(ctx, `
		INSERT IGNORE INTO crpStandings (corporationID, toID, standing)
		SELECT s.characterID, s.toID, s.standing
		  FROM chrStandings s
		  JOIN corporation c ON c.corporationID = s.characterID
		 WHERE s.standing <> 0`)
	if err != nil {
		return fmt.Errorf("migrate corp standings: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE s FROM chrStandings s
		  JOIN corporation c ON c.corporationID = s.characterID`); err != nil {
		return fmt.Errorf("clear legacy corp standings: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("crpStandings: migrated %d corporation standings out of chrStandings", n)
	}
	return nil
}

// GetCorpStandings returns the corporation's declared standings as a util.Rowset
// (toID, standing): slot [3] of standing2.GetMyStandings, which the client indexes by
// toID.
func GetCorpStandings(ctx context.Context, db *sql.DB, corpID int64) (marshal.PyRep, error) {
	return queryRows(ctx, db,
		[]string{"toID", "standing"},
		`SELECT toID, standing FROM crpStandings WHERE corporationID = ?`, corpID)
}

// GetCorpStanding returns corpID's declared standing toward toID, 0 if unset.
func GetCorpStanding(ctx context.Context, db rowQueryer, corpID, toID int64) float64 {
	var standing float64
	db.QueryRowContext(ctx,
		`SELECT standing FROM crpStandings WHERE corporationID = ? AND toID = ?`,
		corpID, toID).Scan(&standing)
	return standing
}

// SetCorpStanding sets corpID's standing toward toID to an absolute value in [-10, 10];
// zero removes it. A non-zero setByCharID logs a PlayerCorpSetStanding transaction naming
// that director (the client renders int_1 as a name, so a write with no character logs
// nothing).
func SetCorpStanding(ctx context.Context, db *sql.DB, corpID, toID int64, standing float64, setByCharID int64, reason string) error {
	if standing < -10 {
		standing = -10
	} else if standing > 10 {
		standing = 10
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var old float64
	var exists bool
	switch err := tx.QueryRowContext(ctx,
		`SELECT standing FROM crpStandings WHERE corporationID = ? AND toID = ? FOR UPDATE`,
		corpID, toID).Scan(&old); {
	case err == nil:
		exists = true
	case errors.Is(err, sql.ErrNoRows):
	default:
		return fmt.Errorf("SetCorpStanding read: %w", err)
	}

	if standing == 0 {
		if !exists {
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM crpStandings WHERE corporationID = ? AND toID = ?`, corpID, toID); err != nil {
			return fmt.Errorf("SetCorpStanding delete: %w", err)
		}
	} else {
		if !exists {
			var n int
			if err := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM crpStandings WHERE corporationID = ?`, corpID).Scan(&n); err != nil {
				return fmt.Errorf("SetCorpStanding count: %w", err)
			}
			if n >= MaxCorpStandings {
				return ErrMaxCorpStandings
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO crpStandings (corporationID, toID, standing) VALUES (?, ?, ?)
			 ON DUPLICATE KEY UPDATE standing = VALUES(standing)`,
			corpID, toID, standing); err != nil {
			return fmt.Errorf("SetCorpStanding upsert: %w", err)
		}
	}

	if setByCharID != 0 && standing != old {
		// modification is stored in internal units (displayed / 10), as
		// InsertStandingTransaction does.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO chrStandingTransactions
			 (eventTypeID, eventDateTime, fromID, toID, modification, originalFromID, originalToID, int_1, int_2, int_3, msg)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, 0, ?)`,
			EventStandingPlayerCorpSetStanding, win32TimeNow(), corpID, toID, (standing-old)/10.0,
			corpID, toID, setByCharID, reason); err != nil {
			return fmt.Errorf("SetCorpStanding log: %w", err)
		}
	}
	return tx.Commit()
}
