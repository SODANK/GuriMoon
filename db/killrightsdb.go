package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Kill rights: one row per (holder, target). The holder is the pilot who was
// killed unlawfully; the target is the killer they may now shoot. Times are
// unix seconds. There is no foreign key: a deleted character's rows are
// swept by expiry like any other.

// KillRightPair identifies one right.
type KillRightPair struct {
	FromID int64 // holder
	ToID   int64 // target
}

func EnsureKillRightsTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS chrKillRights (
			fromID    BIGINT NOT NULL,
			toID      BIGINT NOT NULL,
			grantedAt BIGINT NOT NULL,
			expiresAt BIGINT NOT NULL,
			killID    BIGINT NULL,
			PRIMARY KEY (fromID, toID),
			KEY idx_killrights_to (toID),
			KEY idx_killrights_expiry (expiresAt)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("EnsureKillRightsTable: %w", err)
	}
	return nil
}

// GrantKillRight gives fromID a right against toID until expires, refreshing
// an existing one. killID may be 0.
func GrantKillRight(ctx context.Context, db *sql.DB, fromID, toID int64, granted, expires time.Time, killID int64) error {
	var kid any
	if killID != 0 {
		kid = killID
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO chrKillRights (fromID, toID, grantedAt, expiresAt, killID)
		VALUES (?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE grantedAt = VALUES(grantedAt), expiresAt = VALUES(expiresAt), killID = VALUES(killID)`,
		fromID, toID, granted.Unix(), expires.Unix(), kid)
	if err != nil {
		return fmt.Errorf("GrantKillRight: %w", err)
	}
	return nil
}

// KillRightHeld reports whether holder has a live right against target.
func KillRightHeld(ctx context.Context, db *sql.DB, holder, target int64, now time.Time) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM chrKillRights WHERE fromID = ? AND toID = ? AND expiresAt > ?`,
		holder, target, now.Unix()).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("KillRightHeld: %w", err)
	}
	return n > 0, nil
}

// SpendKillRight pulls a live right's expiry in to until, never pushing it out.
// Reports whether a live row was there to spend.
func SpendKillRight(ctx context.Context, db *sql.DB, fromID, toID int64, now, until time.Time) (bool, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE chrKillRights SET expiresAt = LEAST(expiresAt, ?)
		 WHERE fromID = ? AND toID = ? AND expiresAt > ?`,
		until.Unix(), fromID, toID, now.Unix())
	if err != nil {
		return false, fmt.Errorf("SpendKillRight: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("SpendKillRight: %w", err)
	}
	return n > 0, nil
}

// ListKillRights answers standing2.GetMyKillRights: the rights charID holds
// (target → expiry) and the rights held against them (holder → expiry).
func ListKillRights(ctx context.Context, db *sql.DB, charID int64, now time.Time) (held, against map[int64]time.Time, err error) {
	held, against = map[int64]time.Time{}, map[int64]time.Time{}
	rows, err := db.QueryContext(ctx,
		`SELECT fromID, toID, expiresAt FROM chrKillRights
		  WHERE (fromID = ? OR toID = ?) AND expiresAt > ?`, charID, charID, now.Unix())
	if err != nil {
		return nil, nil, fmt.Errorf("ListKillRights: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var from, to, exp int64
		if err := rows.Scan(&from, &to, &exp); err != nil {
			return nil, nil, fmt.Errorf("ListKillRights scan: %w", err)
		}
		t := time.Unix(exp, 0)
		if from == charID {
			held[to] = t
		}
		if to == charID {
			against[from] = t
		}
	}
	return held, against, rows.Err()
}

// ExpireKillRights deletes every lapsed right and returns the pairs removed,
// so the sweep can tell both parties.
func ExpireKillRights(ctx context.Context, db *sql.DB, now time.Time) ([]KillRightPair, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT fromID, toID FROM chrKillRights WHERE expiresAt <= ?`, now.Unix())
	if err != nil {
		return nil, fmt.Errorf("ExpireKillRights: %w", err)
	}
	var out []KillRightPair
	for rows.Next() {
		var p KillRightPair
		if err := rows.Scan(&p.FromID, &p.ToID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("ExpireKillRights scan: %w", err)
		}
		out = append(out, p)
	}
	rows.Close()
	if len(out) == 0 {
		return nil, nil
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM chrKillRights WHERE expiresAt <= ?`, now.Unix()); err != nil {
		return nil, fmt.Errorf("ExpireKillRights delete: %w", err)
	}
	return out, nil
}
