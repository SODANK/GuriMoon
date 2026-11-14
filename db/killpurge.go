package db

// Killboard purge is the admin panel's destructive counterpart to the retention sweep. It
// discards, credits nothing to killboard_rollup, and may take player killmails; the sweep rolls
// synthetic detail up and never touches players. Nothing calls it on a timer.
// See docs/db-10-notes.md#killboard-purge.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"
)

// KillPurgeScope names what a purge is allowed to take.
type KillPurgeScope string

const (
	// KillPurgeAll takes every killmail, player and synthetic alike.
	KillPurgeAll KillPurgeScope = "all"

	// KillPurgeBots takes synthetic victims only, at every age. Player losses are untouched.
	KillPurgeBots KillPurgeScope = "bots"
)

// Valid reports whether s is a scope this package will act on. An unrecognised scope is refused
// and never defaulted.
func (s KillPurgeScope) Valid() bool {
	return s == KillPurgeAll || s == KillPurgeBots
}

// KillPurgeResult reports what one purge removed.
type KillPurgeResult struct {
	Scope     KillPurgeScope
	Kills     int64
	Attackers int64
	Items     int64
	Rollup    int64 // actor rows removed from killboard_rollup, 0 unless asked
	Truncated bool  // whole tables dropped rather than deleted row by row
	Elapsed   time.Duration
}

// KillboardCounts reports the current size of each table, for the panel's confirmation screen.
func KillboardCounts(ctx context.Context, db *sql.DB) (kills, bots, attackers, items, rollup int64, err error) {
	// One row of four scalar subqueries; not a hot path.
	err = db.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM killmails),
		       (SELECT COUNT(*) FROM killmails WHERE victimIsBot = 1),
		       (SELECT COUNT(*) FROM killmail_attackers),
		       (SELECT COUNT(*) FROM killmail_items),
		       (SELECT COUNT(*) FROM killboard_rollup)`).
		Scan(&kills, &bots, &attackers, &items, &rollup)
	if err != nil {
		return 0, 0, 0, 0, 0, fmt.Errorf("KillboardCounts: %w", err)
	}
	return kills, bots, attackers, items, rollup, nil
}

// PurgeKillboard deletes killboard rows. alsoRollup additionally empties killboard_rollup,
// which holds the arithmetic of mails the retention sweep already removed, so it is a separate
// decision from scope. Children are removed before parents in every path, because these tables
// carry no foreign key and an orphaned child cannot be found again.
func PurgeKillboard(ctx context.Context, db *sql.DB, scope KillPurgeScope, alsoRollup bool) (*KillPurgeResult, error) {
	if !scope.Valid() {
		return nil, fmt.Errorf("PurgeKillboard: unknown scope %q", scope)
	}
	start := time.Now()
	res := &KillPurgeResult{Scope: scope}

	// Counted before the delete, since TRUNCATE reports no row count.
	kills, bots, attackers, items, rollup, err := KillboardCounts(ctx, db)
	if err != nil {
		return nil, err
	}

	switch scope {
	case KillPurgeAll:
		if err := purgeAllKillmails(ctx, db, res); err != nil {
			return nil, err
		}
		if res.Truncated {
			res.Kills, res.Attackers, res.Items = kills, attackers, items
		}
	case KillPurgeBots:
		if bots == 0 {
			break
		}
		if err := purgeBotKillmails(ctx, db, res); err != nil {
			return nil, err
		}
	}

	if alsoRollup {
		if _, err := db.ExecContext(ctx, `DELETE FROM killboard_rollup`); err != nil {
			return nil, fmt.Errorf("PurgeKillboard rollup: %w", err)
		}
		res.Rollup = rollup
	}

	res.Elapsed = time.Since(start)
	return res, nil
}

// purgeAllKillmails empties the three detail tables. TRUNCATE comes first (instant, resets the
// killID counter) but needs the DROP privilege, which the shipped `gurimoon` account lacks.
// On failure it falls back to the batched DELETE, which leaves AUTO_INCREMENT alone.
func purgeAllKillmails(ctx context.Context, db *sql.DB, res *KillPurgeResult) error {
	// Children first even here, so a partial TRUNCATE cannot leave unreachable rows.
	var truncErr error
	for _, t := range []string{"killmail_items", "killmail_attackers", "killmails"} {
		if _, err := db.ExecContext(ctx, `TRUNCATE TABLE `+t); err != nil {
			truncErr = err
			break
		}
	}
	if truncErr == nil {
		res.Truncated = true
		return nil
	}
	log.Printf("[killboard-purge] TRUNCATE unavailable (%v) — falling back to batched DELETE", truncErr)
	return purgeByBatch(ctx, db, `SELECT killID FROM killmails ORDER BY killID LIMIT ?`, res)
}

// purgeBotKillmails removes every synthetic victim's mail, at any age. victimIsBot = 1 is
// restated in the final DELETE so that statement is safe to read alone.
func purgeBotKillmails(ctx context.Context, db *sql.DB, res *KillPurgeResult) error {
	return purgeByBatch(ctx, db,
		`SELECT killID FROM killmails WHERE victimIsBot = 1 ORDER BY killID LIMIT ?`, res)
}

const killPurgeBatch = 2000

// purgeByBatch walks the ID list idQuery returns, deleting children then parents one batch at a
// time, so no single delete holds locks on the table the recorder is writing to.
func purgeByBatch(ctx context.Context, db *sql.DB, idQuery string, res *KillPurgeResult) error {
	for {
		ids, err := purgeIDs(ctx, db, idQuery)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		in, args := inClause(ids)

		r, err := db.ExecContext(ctx, `DELETE FROM killmail_items WHERE killID IN `+in, args...)
		if err != nil {
			return fmt.Errorf("purge items: %w", err)
		}
		n, _ := r.RowsAffected()
		res.Items += n

		r, err = db.ExecContext(ctx, `DELETE FROM killmail_attackers WHERE killID IN `+in, args...)
		if err != nil {
			return fmt.Errorf("purge attackers: %w", err)
		}
		n, _ = r.RowsAffected()
		res.Attackers += n

		// The parent predicate is restated from idQuery so this statement cannot widen if the ID query
		// is edited.
		guard := ""
		if strings.Contains(idQuery, "victimIsBot = 1") {
			guard = "victimIsBot = 1 AND "
		}
		r, err = db.ExecContext(ctx, `DELETE FROM killmails WHERE `+guard+`killID IN `+in, args...)
		if err != nil {
			return fmt.Errorf("purge killmails: %w", err)
		}
		n, _ = r.RowsAffected()
		res.Kills += n

		// A batch that matched IDs but removed no parent would loop on the same rows. The guard mirrors
		// the SELECT, so this cannot happen; stop instead of spinning.
		if n == 0 {
			return fmt.Errorf("purge killmails: batch of %d matched no parent rows", len(ids))
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
}

func purgeIDs(ctx context.Context, db *sql.DB, idQuery string) ([]int64, error) {
	rows, err := db.QueryContext(ctx, idQuery, killPurgeBatch)
	if err != nil {
		return nil, fmt.Errorf("purgeIDs: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("purgeIDs scan: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// LogKillPurge writes the one line a purge is worth. Unlike the retention sweep's it prints
// even when nothing was removed.
func LogKillPurge(res *KillPurgeResult, err error) {
	if err != nil {
		log.Printf("[killboard-purge] failed: %v", err)
		return
	}
	how := "deleted"
	if res.Truncated {
		how = "truncated"
	}
	log.Printf("[killboard-purge] scope=%s %s %d kill(s), %d attacker row(s), %d item row(s), "+
		"%d rollup row(s) in %s",
		res.Scope, how, res.Kills, res.Attackers, res.Items, res.Rollup,
		res.Elapsed.Round(time.Millisecond))
}
