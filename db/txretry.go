package db

// Transient transaction failures. InnoDB kills one transaction of a lock cycle (Error
// 1213) and the waiter of a lock it could not get in time (Error 1205); both mean the
// server already rolled the transaction back and a retry is safe. Market paths retry
// them instead of answering a refusal. See docs/db-12-notes.md#transaction-retry.

import (
	"context"
	"errors"
	"math/rand"
	"time"

	"github.com/go-sql-driver/mysql"
)

// MySQL server error numbers that mean "try the whole transaction again".
const (
	erLockDeadlock    = 1213
	erLockWaitTimeout = 1205
)

// IsRetryableTxError reports whether err is InnoDB telling the caller to restart the
// transaction rather than reporting a bad request.
func IsRetryableTxError(err error) bool {
	if err == nil {
		return false
	}
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return me.Number == erLockDeadlock || me.Number == erLockWaitTimeout
	}
	return false
}

// txRetryAttempts is small: a contended row clears in milliseconds or it is not
// contention, and retrying a hot row adds to the pile-up.
const txRetryAttempts = 4

// RetryOnTxConflict runs fn until it succeeds, fails for a reason other than a lock
// conflict, or runs out of attempts, returning the last error either way so a caller
// that logs it sees the real 1213. The back-off is randomised so two transactions that
// deadlocked do not wake together and deadlock again.
func RetryOnTxConflict(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 0; attempt < txRetryAttempts; attempt++ {
		if err = fn(); !IsRetryableTxError(err) {
			return err
		}
		// 5ms, 10ms, 20ms, each plus or minus up to half itself.
		base := time.Duration(5<<uint(attempt)) * time.Millisecond
		wait := base/2 + time.Duration(rand.Int63n(int64(base)))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	return err
}

// retryOutcome is the shape every market entry point has (a result value plus an
// error), so wrappers do not each repeat the closure carrying it out of RetryOnTxConflict.
func retryOutcome[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	var out T
	err := RetryOnTxConflict(ctx, func() error {
		var e error
		out, e = fn()
		return e
	})
	return out, err
}
