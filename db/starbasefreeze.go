package db

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// Tower fuel stands still while the server is down (starbase.freezeFuelWhileDown).
// See docs/pos-notes.md#fuel-freeze.

// starbaseFuelSweepAtKey holds when the fuel sweep last ran (FILETIME).
const starbaseFuelSweepAtKey = "starbase.fuelSweepAt"

// StarbaseFreezeFuelWhileDown is server_settings `starbase.freezeFuelWhileDown` (default on).
// Read uncached: it is asked once at boot, before the settings cache loads.
func StarbaseFreezeFuelWhileDown(ctx context.Context, db *sql.DB) bool {
	v, ok, err := GetServerSetting(ctx, db, "starbase.freezeFuelWhileDown")
	if err != nil || !ok {
		return true
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return true
	}
	return b
}

// StampStarbaseFuelSweep records that the server was up at now.
func StampStarbaseFuelSweep(ctx context.Context, db *sql.DB, now uint64) error {
	return SetServerSetting(ctx, db, starbaseFuelSweepAtKey, strconv.FormatUint(now, 10))
}

// LastStarbaseFuelSweep is when the server was last up, 0 when unknown.
// With no stamp yet, the newest online tower's fuel tick stands in.
func LastStarbaseFuelSweep(ctx context.Context, db *sql.DB) (uint64, error) {
	v, ok, err := GetServerSetting(ctx, db, starbaseFuelSweepAtKey)
	if err != nil {
		return 0, err
	}
	if ok {
		if n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64); err == nil && n > 0 {
			return n, nil
		}
	}
	if err := ensureStarbaseFuelColumn(ctx, db); err != nil {
		return 0, err
	}
	var tick sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT MAX(fuelTick) FROM starbase WHERE state = ? AND fuelTick > 0`, StarbaseStateOnline).Scan(&tick); err != nil {
		return 0, fmt.Errorf("LastStarbaseFuelSweep: %w", err)
	}
	if !tick.Valid || tick.Int64 <= 0 {
		return 0, nil
	}
	return uint64(tick.Int64), nil
}

// ShiftOnlineStarbaseFuelTicks moves every online tower's fuel clock forward by delta.
func ShiftOnlineStarbaseFuelTicks(ctx context.Context, db *sql.DB, delta uint64) (int64, error) {
	if delta == 0 {
		return 0, nil
	}
	if err := ensureStarbaseFuelColumn(ctx, db); err != nil {
		return 0, err
	}
	res, err := db.ExecContext(ctx,
		`UPDATE starbase SET fuelTick = fuelTick + ? WHERE state = ? AND fuelTick > 0`, delta, StarbaseStateOnline)
	if err != nil {
		return 0, fmt.Errorf("ShiftOnlineStarbaseFuelTicks: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// FuelFreezeShift is how far to move tower fuel clocks at boot: the time the server was down,
// or 0 when the freeze is off, the last-up time is unknown, or the gap is under minGap.
func FuelFreezeShift(enabled bool, lastUp, now, minGap uint64) uint64 {
	if !enabled || lastUp == 0 || now <= lastUp || now-lastUp < minGap {
		return 0
	}
	return now - lastUp
}
