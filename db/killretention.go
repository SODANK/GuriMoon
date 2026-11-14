package db

// Killboard retention: synthetic-victim detail is kept for a window and then
// deleted after its totals are folded into killboard_rollup. Killmails with
// victimIsBot = 0 are never touched.
// See docs/db-9-notes.md#killboard-retention.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

// Rollup scope discriminators. They are stored, so they are never renumbered.
const (
	rollupScopeChar = 1
	rollupScopeCorp = 2
)

const (
	settingKillRetentionDays = "killboard.botRetentionDays"

	// DefaultKillRetentionDays is a week of listable synthetic detail.
	DefaultKillRetentionDays = 7
)

// KillRetentionDays is how many days of synthetic killmail detail to keep.
// 0 disables the sweep and keeps everything.
func KillRetentionDays(ctx context.Context, db *sql.DB) int {
	v, ok, err := GetServerSetting(ctx, db, settingKillRetentionDays)
	if err != nil || !ok {
		return DefaultKillRetentionDays
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return DefaultKillRetentionDays
	}
	return n
}

// SetKillRetentionDays writes the window from the admin panel.
func SetKillRetentionDays(ctx context.Context, db *sql.DB, days int) error {
	if days < 0 {
		days = 0
	}
	return SetServerSetting(ctx, db, settingKillRetentionDays, strconv.Itoa(days))
}

// KillRetentionResult reports what one sweep did.
type KillRetentionResult struct {
	Cutoff       time.Time
	Kills        int64
	Attackers    int64
	Items        int64
	ActorsRolled int64
	Elapsed      time.Duration
	AlreadyClean bool
}

// EnsureKillRollupSchema creates the rollup table. Called at boot beside the
// killboard's own schema bootstrap.
func EnsureKillRollupSchema(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS killboard_rollup (
			scope        TINYINT UNSIGNED NOT NULL,
			actorID      INT UNSIGNED     NOT NULL,
			name         VARCHAR(100)     NOT NULL DEFAULT '',
			kills        BIGINT UNSIGNED  NOT NULL DEFAULT 0,
			losses       BIGINT UNSIGNED  NOT NULL DEFAULT 0,
			iskDestroyed DOUBLE           NOT NULL DEFAULT 0,
			iskLost      DOUBLE           NOT NULL DEFAULT 0,
			soloKills    BIGINT UNSIGNED  NOT NULL DEFAULT 0,
			finalBlows   BIGINT UNSIGNED  NOT NULL DEFAULT 0,
			lastKill     BIGINT UNSIGNED  NOT NULL DEFAULT 0,
			lastLoss     BIGINT UNSIGNED  NOT NULL DEFAULT 0,
			PRIMARY KEY (scope, actorID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("EnsureKillRollupSchema: %w", err)
	}
	return nil
}

// SweepKillboardRetention rolls up and then deletes synthetic killmails older
// than the retention window. days <= 0 disables it. Each batch is one
// transaction with the rollup before the delete, so a failure leaves the mails
// to be rolled up on the next pass. Batched to bound lock time on killmails.
func SweepKillboardRetention(ctx context.Context, db *sql.DB, days int, batch int) (*KillRetentionResult, error) {
	res := &KillRetentionResult{}
	if days <= 0 {
		res.AlreadyClean = true
		return res, nil
	}
	if batch <= 0 {
		batch = 2000
	}
	start := time.Now()
	res.Cutoff = time.Now().AddDate(0, 0, -days)
	cutoff := res.Cutoff.Unix()

	for {
		// One batch of expiring kill IDs. victimIsBot = 1 appears in every statement
		// that selects what to touch, not only in the ID list's query.
		ids, err := expiringKillIDs(ctx, db, cutoff, batch)
		if err != nil {
			return res, err
		}
		if len(ids) == 0 {
			break
		}
		rolled, atk, items, err := rollAndDelete(ctx, db, ids)
		if err != nil {
			return res, err
		}
		res.Kills += int64(len(ids))
		res.Attackers += atk
		res.Items += items
		res.ActorsRolled += rolled

		// Yield between batches so a long pass does not hold the connection pool.
		select {
		case <-ctx.Done():
			res.Elapsed = time.Since(start)
			return res, ctx.Err()
		default:
		}
	}
	res.Elapsed = time.Since(start)
	res.AlreadyClean = res.Kills == 0
	return res, nil
}

// expiringKillIDs lists the next batch of synthetic kills past the cutoff.
// Covered by idx_km_bot (victimIsBot, killTime).
func expiringKillIDs(ctx context.Context, db *sql.DB, cutoff int64, batch int) ([]int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT killID FROM killmails
		 WHERE victimIsBot = 1 AND killTime < ?
		 ORDER BY killTime LIMIT ?`, cutoff, batch)
	if err != nil {
		return nil, fmt.Errorf("expiringKillIDs: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("expiringKillIDs scan: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// rollAndDelete folds one batch into the rollup and removes it, in one
// transaction.
func rollAndDelete(ctx context.Context, db *sql.DB, ids []int64) (rolled, attackers, items int64, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("rollAndDelete begin: %w", err)
	}
	defer tx.Rollback()

	in, args := inClause(ids)

	// Kills per attacking character, collapsed to one row per (actor, kill) as
	// ActorKillStats does, so a gang from one corp on one mail credits it once.
	n, err := rollupExec(ctx, tx, `
		INSERT INTO killboard_rollup
		       (scope, actorID, name, kills, iskDestroyed, soloKills, finalBlows, lastKill)
		SELECT ?, t.actorID, MAX(t.name), COUNT(*), COALESCE(SUM(t.totalValue), 0),
		       COALESCE(SUM(t.solo), 0), COALESCE(SUM(t.fb), 0), COALESCE(MAX(t.killTime), 0)
		  FROM (SELECT a.charID AS actorID, MAX(a.name) AS name, k.killID,
		               MAX(k.totalValue)        AS totalValue,
		               MAX(k.killTime)          AS killTime,
		               MAX(k.attackerCount = 1) AS solo,
		               MAX(a.finalBlow)         AS fb
		          FROM killmail_attackers a
		          JOIN killmails k ON k.killID = a.killID
		         WHERE a.killID IN `+in+` AND a.charID <> 0
		         GROUP BY a.charID, k.killID) t
		 GROUP BY t.actorID
		ON DUPLICATE KEY UPDATE
		       name         = VALUES(name),
		       kills        = kills        + VALUES(kills),
		       iskDestroyed = iskDestroyed + VALUES(iskDestroyed),
		       soloKills    = soloKills    + VALUES(soloKills),
		       finalBlows   = finalBlows   + VALUES(finalBlows),
		       lastKill     = GREATEST(lastKill, VALUES(lastKill))`,
		append([]any{rollupScopeChar}, args...))
	if err != nil {
		return 0, 0, 0, fmt.Errorf("rollup char kills: %w", err)
	}
	rolled += n

	// Kills per attacking corporation. No isNPC filter, matching
	// KillActor.attackerPred.
	n, err = rollupExec(ctx, tx, `
		INSERT INTO killboard_rollup
		       (scope, actorID, name, kills, iskDestroyed, soloKills, finalBlows, lastKill)
		SELECT ?, t.actorID, MAX(t.name), COUNT(*), COALESCE(SUM(t.totalValue), 0),
		       COALESCE(SUM(t.solo), 0), COALESCE(SUM(t.fb), 0), COALESCE(MAX(t.killTime), 0)
		  FROM (SELECT a.corpID AS actorID, MAX(a.corpName) AS name, k.killID,
		               MAX(k.totalValue)        AS totalValue,
		               MAX(k.killTime)          AS killTime,
		               MAX(k.attackerCount = 1) AS solo,
		               MAX(a.finalBlow)         AS fb
		          FROM killmail_attackers a
		          JOIN killmails k ON k.killID = a.killID
		         WHERE a.killID IN `+in+` AND a.corpID <> 0
		         GROUP BY a.corpID, k.killID) t
		 GROUP BY t.actorID
		ON DUPLICATE KEY UPDATE
		       name         = VALUES(name),
		       kills        = kills        + VALUES(kills),
		       iskDestroyed = iskDestroyed + VALUES(iskDestroyed),
		       soloKills    = soloKills    + VALUES(soloKills),
		       finalBlows   = finalBlows   + VALUES(finalBlows),
		       lastKill     = GREATEST(lastKill, VALUES(lastKill))`,
		append([]any{rollupScopeCorp}, args...))
	if err != nil {
		return 0, 0, 0, fmt.Errorf("rollup corp kills: %w", err)
	}
	rolled += n

	// Losses per victim character and corporation: killmails is already one row
	// per loss.
	for _, l := range []struct {
		scope int
		idCol string
		nmCol string
	}{
		{rollupScopeChar, "victimCharID", "victimName"},
		{rollupScopeCorp, "victimCorpID", "victimCorpName"},
	} {
		n, err = rollupExec(ctx, tx, `
			INSERT INTO killboard_rollup (scope, actorID, name, losses, iskLost, lastLoss)
			SELECT ?, k.`+l.idCol+`, MAX(k.`+l.nmCol+`), COUNT(*),
			       COALESCE(SUM(k.totalValue), 0), COALESCE(MAX(k.killTime), 0)
			  FROM killmails k
			 WHERE k.killID IN `+in+` AND k.`+l.idCol+` <> 0
			 GROUP BY k.`+l.idCol+`
			ON DUPLICATE KEY UPDATE
			       name     = VALUES(name),
			       losses   = losses  + VALUES(losses),
			       iskLost  = iskLost + VALUES(iskLost),
			       lastLoss = GREATEST(lastLoss, VALUES(lastLoss))`,
			append([]any{l.scope}, args...))
		if err != nil {
			return 0, 0, 0, fmt.Errorf("rollup losses scope %d: %w", l.scope, err)
		}
		rolled += n
	}

	// Deletion, children first: these tables carry no foreign key, so a parent
	// deleted ahead of them orphans its rows.
	r, err := tx.ExecContext(ctx, `DELETE FROM killmail_items WHERE killID IN `+in, args...)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("delete items: %w", err)
	}
	items, _ = r.RowsAffected()

	r, err = tx.ExecContext(ctx, `DELETE FROM killmail_attackers WHERE killID IN `+in, args...)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("delete attackers: %w", err)
	}
	attackers, _ = r.RowsAffected()

	// victimIsBot = 1 is restated so this statement is safe on its own.
	if _, err = tx.ExecContext(ctx,
		`DELETE FROM killmails WHERE victimIsBot = 1 AND killID IN `+in, args...); err != nil {
		return 0, 0, 0, fmt.Errorf("delete killmails: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return 0, 0, 0, fmt.Errorf("rollAndDelete commit: %w", err)
	}
	return rolled, attackers, items, nil
}

// rollupExec runs one rollup statement and reports the actor rows touched.
// MySQL reports 2 for an upsert update and 1 for an insert, so the count is
// halved for the log line only.
func rollupExec(ctx context.Context, tx *sql.Tx, query string, args []any) (int64, error) {
	r, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	n, _ := r.RowsAffected()
	return (n + 1) / 2, nil
}

// inClause renders "(?,?,?)" plus its arguments. Killmail IDs are server
// generated; they still travel as placeholders.
func inClause(ids []int64) (string, []any) {
	if len(ids) == 0 {
		return "(NULL)", nil
	}
	b := make([]byte, 0, 2*len(ids)+2)
	b = append(b, '(')
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '?')
		args = append(args, id)
	}
	b = append(b, ')')
	return string(b), args
}

// KillRollup is one actor's pre-purge arithmetic.
type KillRollup struct {
	Kills        int64
	Losses       int64
	ISKDestroyed float64
	ISKLost      float64
	SoloKills    int64
	FinalBlows   int64
	LastKill     time.Time
	LastLoss     time.Time
}

// GetKillRollup reads the archived totals for one actor. A missing row means
// nothing of theirs has aged out.
func GetKillRollup(ctx context.Context, db *sql.DB, actor KillActor) (*KillRollup, error) {
	out := &KillRollup{}
	scope, id := rollupScopeChar, actor.CharID
	if actor.CorpID != 0 {
		scope, id = rollupScopeCorp, actor.CorpID
	}
	if id == 0 {
		return out, nil
	}
	err := db.QueryRowContext(ctx, `
		SELECT kills, losses, iskDestroyed, iskLost, soloKills, finalBlows, lastKill, lastLoss
		  FROM killboard_rollup WHERE scope = ? AND actorID = ?`, scope, id).
		Scan(&out.Kills, &out.Losses, &out.ISKDestroyed, &out.ISKLost,
			&out.SoloKills, &out.FinalBlows, &scanUnix{&out.LastKill}, &scanUnix{&out.LastLoss})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return out, nil
	case err != nil:
		return nil, fmt.Errorf("GetKillRollup: %w", err)
	}
	return out, nil
}

// Empty reports whether this actor has nothing archived (the profile pages
// skip their "includes N archived" footnote).
func (r *KillRollup) Empty() bool {
	return r == nil || (r.Kills == 0 && r.Losses == 0)
}

// KillRetentionPending counts what the next sweep would remove, for the admin
// panel.
func KillRetentionPending(ctx context.Context, db *sql.DB, days int) (int64, error) {
	if days <= 0 {
		return 0, nil
	}
	var n int64
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM killmails WHERE victimIsBot = 1 AND killTime < ?`,
		time.Now().AddDate(0, 0, -days).Unix()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("KillRetentionPending: %w", err)
	}
	return n, nil
}

// LogKillRetention writes the sweep's one log line, silent when nothing was
// removed.
func LogKillRetention(res *KillRetentionResult, err error) {
	switch {
	case err != nil:
		log.Printf("[killboard-retention] sweep failed: %v", err)
	case res == nil || res.AlreadyClean:
		return
	default:
		log.Printf("[killboard-retention] rolled up %d actor row(s) and removed %d synthetic kill(s), "+
			"%d attacker row(s), %d item row(s) older than %s in %s",
			res.ActorsRolled, res.Kills, res.Attackers, res.Items,
			res.Cutoff.Format("2006-01-02 15:04"), res.Elapsed.Round(time.Millisecond))
	}
}
