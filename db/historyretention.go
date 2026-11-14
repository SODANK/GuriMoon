package db

// History retention: closed escrows and delivered industry jobs. Both tables are live working
// sets with an append-only tail of finished rows. Closed escrows keep escrow.closedKeepDays
// (open ones are never touched); delivered and aborted jobs keep industry.jobHistoryKeepDays.
// See docs/db-11-notes.md#history-retention.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
)

const (
	settingEscrowClosedKeepDays = "escrow.closedKeepDays"
	settingJobHistoryKeepDays   = "industry.jobHistoryKeepDays"

	DefaultEscrowClosedKeepDays = 30
	DefaultJobHistoryKeepDays   = 180
)

// EscrowClosedKeepDays is how long a closed escrow outlives its close. 0
// disables.
func EscrowClosedKeepDays(ctx context.Context, db *sql.DB) int {
	return settingInt(ctx, db, settingEscrowClosedKeepDays,
		DefaultEscrowClosedKeepDays, 0, maxTxnKeepDays)
}

// JobHistoryKeepDays is how long a delivered or aborted job stays after its
// end time. 0 disables.
func JobHistoryKeepDays(ctx context.Context, db *sql.DB) int {
	return settingInt(ctx, db, settingJobHistoryKeepDays,
		DefaultJobHistoryKeepDays, 0, maxTxnKeepDays)
}

// HistoryRetentionResult is what one sweep did, for the log line.
type HistoryRetentionResult struct {
	EscrowRows     int64
	JobRows        int64
	EscrowKeepDays int
	JobKeepDays    int
}

// SweepHistoryRetention trims both tails. A failure on one does not stop the
// other.
func SweepHistoryRetention(ctx context.Context, db *sql.DB) (HistoryRetentionResult, error) {
	res := HistoryRetentionResult{
		EscrowKeepDays: EscrowClosedKeepDays(ctx, db),
		JobKeepDays:    JobHistoryKeepDays(ctx, db),
	}
	now := retentionNow()
	var firstErr error

	n, err := sweepClosedEscrows(ctx, db, now, res.EscrowKeepDays)
	res.EscrowRows = n
	if err != nil {
		firstErr = err
	}
	n, err = sweepDeliveredJobs(ctx, db, now, res.JobKeepDays)
	res.JobRows = n
	if err != nil && firstErr == nil {
		firstErr = err
	}
	return res, firstErr
}

// sweepClosedEscrows deletes delivered, expired and cancelled escrows whose
// close is older than days. dateCompleted is unix seconds; a closed row with
// no stamp is left alone rather than read as closed in 1970.
// idx_escrow_state_completed turns this into three short ranges.
func sweepClosedEscrows(ctx context.Context, db *sql.DB, now int64, days int) (int64, error) {
	if days <= 0 {
		return 0, nil
	}
	cutoff := now - int64(days)*unixDay
	return deleteInBatches(ctx, db, "escrow retention", `
		DELETE FROM escrows
		 WHERE state IN (?, ?, ?) AND dateCompleted > 0 AND dateCompleted < ?
		 LIMIT ?`,
		CourierStateDelivered, CourierStateExpired, CourierStateCancelled, cutoff)
}

// sweepDeliveredJobs deletes delivered (101) and aborted (102) jobs that
// ended more than days ago, over idx_ramjobs_status_end. Running and ready
// jobs (0, 1) are never in range.
func sweepDeliveredJobs(ctx context.Context, db *sql.DB, now int64, days int) (int64, error) {
	if days <= 0 {
		return 0, nil
	}
	cutoff := UnixToWin32(now - int64(days)*unixDay)
	return deleteInBatches(ctx, db, "job history retention", `
		DELETE FROM ramJobs
		 WHERE completedStatusID IN (101, 102) AND endProductionTime < ?
		 LIMIT ?`, cutoff)
}

// deleteInBatches runs a single-table DELETE ending in "LIMIT ?" until a
// batch comes back short. The batch size is appended to args.
func deleteInBatches(ctx context.Context, db *sql.DB, what, q string, args ...any) (int64, error) {
	args = append(args, walletDeleteBatch)
	var total int64
	for {
		r, err := db.ExecContext(ctx, q, args...)
		if err != nil {
			return total, fmt.Errorf("%s delete: %w", what, err)
		}
		n, _ := r.RowsAffected()
		total += n
		if n < walletDeleteBatch {
			return total, nil
		}
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
	}
}

// LogHistoryRetention prints one line, and only when the sweep did something.
func LogHistoryRetention(res HistoryRetentionResult, err error) {
	if err != nil {
		log.Printf("[history-retention] %v (escrows %d, jobs %d removed before the error)",
			err, res.EscrowRows, res.JobRows)
		return
	}
	if res.EscrowRows == 0 && res.JobRows == 0 {
		return
	}
	log.Printf("[history-retention] removed %d escrow(s) closed more than %dd ago, %d industry job(s) delivered more than %dd ago",
		res.EscrowRows, res.EscrowKeepDays, res.JobRows, res.JobKeepDays)
}
