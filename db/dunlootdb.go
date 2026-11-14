package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"
)

// Per-ball dungeon loot: drops authored against one dunObjects row, rolled on top of the
// npc_loot_entries tables when the killed ball carries the objectID. Each entry rolls its
// own dropChance with no haul gate or max-drops cap, so 1.0 is guaranteed. A ball may also
// bound its distinct item count (dunObjects.lootMinDrops/lootMaxDrops, see RollBallLoot).

// EnsureDungeonLootTable creates the per-object dungeon loot table. Call once at
// startup after EnsureDungeonTables.
func EnsureDungeonLootTable(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS dunObjectLoot (
			lootID     INT PRIMARY KEY AUTO_INCREMENT,
			objectID   INT NOT NULL,
			itemTypeID INT NOT NULL,
			minQty     INT NOT NULL DEFAULT 1,
			maxQty     INT NOT NULL DEFAULT 1,
			dropChance DOUBLE NOT NULL DEFAULT 1,
			UNIQUE KEY uq_dunloot_obj_item (objectID, itemTypeID),
			INDEX idx_dunloot_obj (objectID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`); err != nil {
		return fmt.Errorf("EnsureDungeonLootTable: %w", err)
	}
	return nil
}

// DunObjectLootRow is one authored drop on a dungeon ball.
type DunObjectLootRow struct {
	LootID     int64
	ObjectID   int64
	ItemTypeID int64
	ItemName   string
	MinQty     int64
	MaxQty     int64
	DropChance float64
	// IsBlueprint: the item drops as one copy and Min/MaxQty are its runs
	// (CreateNPCLootItem).
	IsBlueprint bool
}

// GetObjectLoot returns the authored drops for one dungeon object.
func GetObjectLoot(ctx context.Context, db *sql.DB, objectID int64) ([]DunObjectLootRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT l.lootID, l.objectID, l.itemTypeID, COALESCE(t.typeName, ''),
		       l.minQty, l.maxQty, l.dropChance,
		       COALESCE(g.categoryID, 0) = 9
		FROM dunObjectLoot l
		LEFT JOIN invTypes t ON t.typeID = l.itemTypeID
		LEFT JOIN invGroups g ON g.groupID = t.groupID
		WHERE l.objectID = ?
		ORDER BY l.lootID`, objectID)
	if err != nil {
		return nil, fmt.Errorf("GetObjectLoot(%d): %w", objectID, err)
	}
	defer rows.Close()
	var out []DunObjectLootRow
	for rows.Next() {
		var l DunObjectLootRow
		if err := rows.Scan(&l.LootID, &l.ObjectID, &l.ItemTypeID, &l.ItemName,
			&l.MinQty, &l.MaxQty, &l.DropChance, &l.IsBlueprint); err != nil {
			return nil, fmt.Errorf("GetObjectLoot scan: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// GetRoomObjectLoot returns every authored drop in a room, keyed by objectID.
func GetRoomObjectLoot(ctx context.Context, db *sql.DB, roomID int64) (map[int64][]DunObjectLootRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT l.lootID, l.objectID, l.itemTypeID, COALESCE(t.typeName, ''),
		       l.minQty, l.maxQty, l.dropChance,
		       COALESCE(g.categoryID, 0) = 9
		FROM dunObjectLoot l
		JOIN dunObjects o ON o.objectID = l.objectID
		LEFT JOIN invTypes t ON t.typeID = l.itemTypeID
		LEFT JOIN invGroups g ON g.groupID = t.groupID
		WHERE o.roomID = ?
		ORDER BY l.objectID, l.lootID`, roomID)
	if err != nil {
		return nil, fmt.Errorf("GetRoomObjectLoot(%d): %w", roomID, err)
	}
	defer rows.Close()
	out := make(map[int64][]DunObjectLootRow)
	for rows.Next() {
		var l DunObjectLootRow
		if err := rows.Scan(&l.LootID, &l.ObjectID, &l.ItemTypeID, &l.ItemName,
			&l.MinQty, &l.MaxQty, &l.DropChance, &l.IsBlueprint); err != nil {
			return nil, fmt.Errorf("GetRoomObjectLoot scan: %w", err)
		}
		out[l.ObjectID] = append(out[l.ObjectID], l)
	}
	return out, rows.Err()
}

// SaveObjectLoot inserts or updates one drop on a dungeon object; a repeat item
// overwrites the amounts (one row per object and item).
func SaveObjectLoot(ctx context.Context, db DBTX, objectID, itemTypeID, minQty, maxQty int64, dropChance float64) error {
	if objectID == 0 || itemTypeID == 0 {
		return fmt.Errorf("SaveObjectLoot: objectID and itemTypeID are required")
	}
	if minQty < 1 {
		minQty = 1
	}
	if maxQty < minQty {
		maxQty = minQty
	}
	if dropChance <= 0 {
		dropChance = 1
	}
	if dropChance > 1 {
		dropChance = 1
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO dunObjectLoot (objectID, itemTypeID, minQty, maxQty, dropChance)
		VALUES (?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE minQty = VALUES(minQty), maxQty = VALUES(maxQty),
		                        dropChance = VALUES(dropChance)`,
		objectID, itemTypeID, minQty, maxQty, dropChance)
	if err != nil {
		return fmt.Errorf("SaveObjectLoot: %w", err)
	}
	return nil
}

// DeleteObjectLootEntry removes one authored drop.
func DeleteObjectLootEntry(ctx context.Context, db *sql.DB, lootID int64) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM dunObjectLoot WHERE lootID = ?`, lootID); err != nil {
		return fmt.Errorf("DeleteObjectLootEntry(%d): %w", lootID, err)
	}
	return nil
}

// DeleteObjectLootForObject clears every drop on an object; called when the object is
// deleted.
func DeleteObjectLootForObject(ctx context.Context, db DBTX, objectID int64) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM dunObjectLoot WHERE objectID = ?`, objectID); err != nil {
		return fmt.Errorf("DeleteObjectLootForObject(%d): %w", objectID, err)
	}
	return nil
}

// CopyObjectLoot duplicates an object's drop list onto another object, for the
// editor's copy and duplicate actions.
func CopyObjectLoot(ctx context.Context, db DBTX, fromObjectID, toObjectID int64) error {
	if fromObjectID == 0 || toObjectID == 0 || fromObjectID == toObjectID {
		return nil
	}
	_, err := db.ExecContext(ctx, `
		INSERT IGNORE INTO dunObjectLoot (objectID, itemTypeID, minQty, maxQty, dropChance)
		SELECT ?, itemTypeID, minQty, maxQty, dropChance
		FROM dunObjectLoot WHERE objectID = ?`, toObjectID, fromObjectID)
	if err != nil {
		return fmt.Errorf("CopyObjectLoot(%d→%d): %w", fromObjectID, toObjectID, err)
	}
	return nil
}

// PasteObjectLoot makes toObjectID's drop list and item-count bounds an exact copy of
// fromObjectID's, in one transaction. A source with no drops is refused, so a stale
// clipboard naming a deleted ball cannot wipe the target.
func PasteObjectLoot(ctx context.Context, db *sql.DB, fromObjectID, toObjectID int64) error {
	if fromObjectID == 0 || toObjectID == 0 {
		return fmt.Errorf("source and target balls are required")
	}
	if fromObjectID == toObjectID {
		return fmt.Errorf("source and target are the same ball")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("PasteObjectLoot: %w", err)
	}
	defer tx.Rollback()

	var drops, targets int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM dunObjectLoot WHERE objectID = ?`, fromObjectID).Scan(&drops); err != nil {
		return fmt.Errorf("PasteObjectLoot source: %w", err)
	}
	if drops == 0 {
		return fmt.Errorf("source ball #%d has no drops", fromObjectID)
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM dunObjects WHERE objectID = ?`, toObjectID).Scan(&targets); err != nil {
		return fmt.Errorf("PasteObjectLoot target: %w", err)
	}
	if targets == 0 {
		return fmt.Errorf("target ball #%d no longer exists", toObjectID)
	}

	lo, hi, err := GetObjectLootDropRange(ctx, tx, fromObjectID)
	if err != nil {
		return err
	}
	if err := DeleteObjectLootForObject(ctx, tx, toObjectID); err != nil {
		return err
	}
	if err := CopyObjectLoot(ctx, tx, fromObjectID, toObjectID); err != nil {
		return err
	}
	if err := EditObject(ctx, tx, toObjectID, EditObjectFields{LootMinDrops: &lo, LootMaxDrops: &hi}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("PasteObjectLoot commit: %w", err)
	}
	return nil
}

// GetObjectLootDropRange returns a ball's authored item-count bounds (dunObjects
// lootMinDrops/lootMaxDrops); 0 means no bound on that side.
func GetObjectLootDropRange(ctx context.Context, db DBTX, objectID int64) (minDrops, maxDrops int64, err error) {
	var lo, hi sql.NullInt64
	err = db.QueryRowContext(ctx,
		`SELECT lootMinDrops, lootMaxDrops FROM dunObjects WHERE objectID = ?`, objectID).Scan(&lo, &hi)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("GetObjectLootDropRange(%d): %w", objectID, err)
	}
	return lo.Int64, hi.Int64, nil
}

// ValidateLootDropRange checks an authored item-count range. Negative values mean no
// bound; a min above a set max is refused.
func ValidateLootDropRange(minDrops, maxDrops int64) (int64, int64, error) {
	if minDrops < 0 {
		minDrops = 0
	}
	if maxDrops < 0 {
		maxDrops = 0
	}
	if maxDrops > 0 && minDrops > maxDrops {
		return 0, 0, fmt.Errorf("min items (%d) is above max items (%d)", minDrops, maxDrops)
	}
	return minDrops, maxDrops, nil
}

// BallLootDrop is one item a ball's authored loot rolled.
type BallLootDrop struct {
	ItemTypeID int64
	Qty        int64
}

// RollBallLoot rolls a ball's authored drops: each entry rolls its own dropChance, then
// the distinct item count is clamped to [minDrops, maxDrops] (0 = no bound). Below the
// floor, missed entries are added weighted by dropChance; above the cap, extras are cut at
// random with guaranteed (100%) entries last. Quantity rolls between minQty and maxQty.
func RollBallLoot(entries []DunObjectLootRow, minDrops, maxDrops int64, rng *rand.Rand) []BallLootDrop {
	if len(entries) == 0 {
		return nil
	}
	var hits, misses []DunObjectLootRow
	for _, l := range entries {
		if l.DropChance >= 1 || rng.Float64() < l.DropChance {
			hits = append(hits, l)
		} else {
			misses = append(misses, l)
		}
	}

	for minDrops > 0 && int64(len(hits)) < minDrops && len(misses) > 0 {
		total := 0.0
		for _, l := range misses {
			total += math.Max(l.DropChance, 0)
		}
		pick := 0
		if total > 0 {
			r := rng.Float64() * total
			for i, l := range misses {
				r -= math.Max(l.DropChance, 0)
				if r < 0 {
					pick = i
					break
				}
			}
		} else {
			pick = rng.Intn(len(misses))
		}
		hits = append(hits, misses[pick])
		misses = append(misses[:pick], misses[pick+1:]...)
	}

	if maxDrops > 0 && int64(len(hits)) > maxDrops {
		rng.Shuffle(len(hits), func(i, j int) { hits[i], hits[j] = hits[j], hits[i] })
		sort.SliceStable(hits, func(i, j int) bool {
			return hits[i].DropChance >= 1 && hits[j].DropChance < 1
		})
		hits = hits[:maxDrops]
	}

	out := make([]BallLootDrop, 0, len(hits))
	for _, l := range hits {
		qty := l.MinQty
		if l.MaxQty > l.MinQty {
			qty += rng.Int63n(l.MaxQty - l.MinQty + 1)
		}
		if qty > 0 {
			out = append(out, BallLootDrop{l.ItemTypeID, qty})
		}
	}
	return out
}
