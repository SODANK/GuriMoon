package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ── Automatic bug tracker ───────────────────────────────────────────────────
//
// bug_reports stores deduplicated bug signals collected by the bugtrack
// package (log-line interception, unhandled-call reports, recovered panics).
// One row per signature; hitCount/lastSeen accumulate on repeat occurrences.
// Managed from the ESP admin panel (/bugs).

// Bug status values.
const (
	BugStatusOpen    = "open"
	BugStatusAcked   = "acked"
	BugStatusFixed   = "fixed"
	BugStatusIgnored = "ignored"
)

// EnsureBugReportsTable creates the bug tracker table.
func EnsureBugReportsTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS bug_reports (
			bugID         BIGINT NOT NULL AUTO_INCREMENT,
			signature     CHAR(16) NOT NULL,
			category      VARCHAR(32) NOT NULL,
			service       VARCHAR(64) NOT NULL DEFAULT '',
			method        VARCHAR(128) NOT NULL DEFAULT '',
			sampleMessage TEXT NOT NULL,
			hitCount      BIGINT NOT NULL DEFAULT 1,
			firstSeen     BIGINT NOT NULL,
			lastSeen      BIGINT NOT NULL,
			status        VARCHAR(16) NOT NULL DEFAULT 'open',
			notes         TEXT NULL,
			PRIMARY KEY (bugID),
			UNIQUE KEY uq_bug_signature (signature)
		)`)
	if err != nil {
		return fmt.Errorf("EnsureBugReportsTable: %w", err)
	}
	return nil
}

// UpsertBugReport records count occurrences of a signature.  New signatures
// open a fresh row; known ones bump hitCount/lastSeen. A signature previously
// marked fixed reopens (regression) and takes the new sample; ignored ones stay
// ignored but keep counting so their magnitude remains visible.
func UpsertBugReport(ctx context.Context, db *sql.DB,
	signature, category, service, method, sample string, count, when int64) error {
	return UpsertBugReports(ctx, db, []BugReportHit{{
		Signature: signature, Category: category, Service: service, Method: method,
		Sample: sample, Count: count, When: when,
	}})
}

// BugReportHit is one signature's coalesced occurrences for UpsertBugReports.
type BugReportHit struct {
	Signature, Category, Service, Method, Sample string
	Count, When                                  int64
}

// bugUpsertSQL keeps the stored sample unless the row was fixed: rewriting a 2 KB
// TEXT on every hit was most of the table's write volume. Assignments run left to
// right, so sampleMessage sees the old status.
const bugUpsertSQL = `
		INSERT INTO bug_reports
			(signature, category, service, method, sampleMessage, hitCount, firstSeen, lastSeen)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			hitCount      = hitCount + VALUES(hitCount),
			lastSeen      = VALUES(lastSeen),
			sampleMessage = IF(status = 'fixed', VALUES(sampleMessage), sampleMessage),
			status        = IF(status = 'fixed', 'open', status)`

// UpsertBugReports applies a batch of hits in one transaction.
func UpsertBugReports(ctx context.Context, db *sql.DB, hits []BugReportHit) error {
	if len(hits) == 0 {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("UpsertBugReports begin: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, bugUpsertSQL)
	if err != nil {
		return fmt.Errorf("UpsertBugReports prepare: %w", err)
	}
	defer stmt.Close()
	for _, h := range hits {
		if _, err := stmt.ExecContext(ctx, h.Signature, h.Category, h.Service, h.Method,
			h.Sample, h.Count, h.When, h.When); err != nil {
			return fmt.Errorf("UpsertBugReports %s: %w", h.Signature, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("UpsertBugReports commit: %w", err)
	}
	return nil
}

// PruneBugReports deletes rows past the newest keep (by lastSeen) that carry no
// notes, in chunks. cutoff is the lastSeen of the first row past keep; rows tied with
// it go too. Returns the number deleted.
func PruneBugReports(ctx context.Context, db *sql.DB, keep int) (int64, error) {
	var cutoff int64
	err := db.QueryRowContext(ctx,
		`SELECT lastSeen FROM bug_reports ORDER BY lastSeen DESC LIMIT 1 OFFSET ?`, keep).Scan(&cutoff)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("PruneBugReports cutoff: %w", err)
	}
	var total int64
	for i := 0; i < 20 && ctx.Err() == nil; i++ {
		res, err := db.ExecContext(ctx,
			`DELETE FROM bug_reports WHERE lastSeen <= ? AND (notes IS NULL OR notes = '') LIMIT 2000`, cutoff)
		if err != nil {
			return total, fmt.Errorf("PruneBugReports: %w", err)
		}
		n, _ := res.RowsAffected()
		total += n
		if n < 2000 {
			break
		}
	}
	return total, nil
}

// BugReport is one deduplicated bug row.
type BugReport struct {
	BugID         int64
	Signature     string
	Category      string
	Service       string
	Method        string
	SampleMessage string
	HitCount      int64
	FirstSeen     int64 // unix seconds
	LastSeen      int64 // unix seconds
	Status        string
	Notes         string
}

// ListBugReports returns bug rows, newest activity first, optionally filtered
// by status and/or category ("" = no filter).
func ListBugReports(ctx context.Context, db *sql.DB, status, category string) ([]BugReport, error) {
	q := `
		SELECT bugID, signature, category, service, method, sampleMessage,
		       hitCount, firstSeen, lastSeen, status, COALESCE(notes, '')
		FROM bug_reports WHERE 1=1`
	var args []any
	if status != "" {
		q += ` AND status = ?`
		args = append(args, status)
	}
	if category != "" {
		q += ` AND category = ?`
		args = append(args, category)
	}
	q += ` ORDER BY lastSeen DESC LIMIT 500`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("ListBugReports: %w", err)
	}
	defer rows.Close()
	var out []BugReport
	for rows.Next() {
		var b BugReport
		if err := rows.Scan(&b.BugID, &b.Signature, &b.Category, &b.Service,
			&b.Method, &b.SampleMessage, &b.HitCount, &b.FirstSeen,
			&b.LastSeen, &b.Status, &b.Notes); err != nil {
			return nil, fmt.Errorf("ListBugReports scan: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// GetBugReport returns one bug row, or nil if the ID does not exist.
func GetBugReport(ctx context.Context, db *sql.DB, bugID int64) (*BugReport, error) {
	var b BugReport
	err := db.QueryRowContext(ctx, `
		SELECT bugID, signature, category, service, method, sampleMessage,
		       hitCount, firstSeen, lastSeen, status, COALESCE(notes, '')
		FROM bug_reports WHERE bugID = ?`, bugID,
	).Scan(&b.BugID, &b.Signature, &b.Category, &b.Service, &b.Method,
		&b.SampleMessage, &b.HitCount, &b.FirstSeen, &b.LastSeen,
		&b.Status, &b.Notes)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("GetBugReport: %w", err)
	}
	return &b, nil
}

// UpdateBugStatus sets the workflow status of a bug row.
func UpdateBugStatus(ctx context.Context, db *sql.DB, bugID int64, status string) error {
	switch status {
	case BugStatusOpen, BugStatusAcked, BugStatusFixed, BugStatusIgnored:
	default:
		return fmt.Errorf("invalid bug status %q", status)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE bug_reports SET status = ? WHERE bugID = ?`, status, bugID); err != nil {
		return fmt.Errorf("UpdateBugStatus: %w", err)
	}
	return nil
}

// UpdateBugNotes sets the admin notes of a bug row.
func UpdateBugNotes(ctx context.Context, db *sql.DB, bugID int64, notes string) error {
	if _, err := db.ExecContext(ctx,
		`UPDATE bug_reports SET notes = ? WHERE bugID = ?`, notes, bugID); err != nil {
		return fmt.Errorf("UpdateBugNotes: %w", err)
	}
	return nil
}

// DeleteBugReport removes a bug row entirely.  A recurring signal will simply
// open a fresh row.
func DeleteBugReport(ctx context.Context, db *sql.DB, bugID int64) error {
	if _, err := db.ExecContext(ctx,
		`DELETE FROM bug_reports WHERE bugID = ?`, bugID); err != nil {
		return fmt.Errorf("DeleteBugReport: %w", err)
	}
	return nil
}

// CountBugsByStatus returns row counts keyed by status for the /bugs stat
// tiles.
func CountBugsByStatus(ctx context.Context, db *sql.DB) (map[string]int64, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT status, COUNT(*) FROM bug_reports GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("CountBugsByStatus: %w", err)
	}
	defer rows.Close()
	out := make(map[string]int64)
	for rows.Next() {
		var s string
		var n int64
		if err := rows.Scan(&s, &n); err != nil {
			return nil, fmt.Errorf("CountBugsByStatus scan: %w", err)
		}
		out[s] = n
	}
	return out, rows.Err()
}
