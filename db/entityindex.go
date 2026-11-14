package db

// The composite hangar index. Hangar lookups filter on (ownerID, locationID, typeID,
// flag), and a SELECT ... FOR UPDATE over single-column indexes takes next-key locks
// across unrelated owners and stations. See docs/db-12-notes.md#entity-hangar-index.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"
)

// entityHangarIndex is the composite index name. Column order is owner, location,
// type, flag: flag is last because callers pass it as a set.
const entityHangarIndex = "idx_entity_owner_loc_type"

// EnsureEntityIndexes builds the composite hangar index if it is missing. Callers treat
// failure as non-fatal: without it the server is slower and deadlocks more.
func EnsureEntityIndexes(ctx context.Context, db *sql.DB) error {
	var have int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'entity'
		  AND INDEX_NAME = ?`, entityHangarIndex).Scan(&have); err != nil {
		return fmt.Errorf("EnsureEntityIndexes check: %w", err)
	}
	if have > 0 {
		return nil
	}
	log.Printf("[entity] building %s — this can take a while on a large item table", entityHangarIndex)
	start := time.Now()
	if _, err := db.ExecContext(ctx,
		`CREATE INDEX `+entityHangarIndex+` ON entity (ownerID, locationID, typeID, flag)`); err != nil {
		return fmt.Errorf("EnsureEntityIndexes create: %w", err)
	}
	log.Printf("[entity] %s built in %s", entityHangarIndex, time.Since(start).Round(time.Millisecond))
	return nil
}
