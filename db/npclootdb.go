package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

// NPC loot tables: authored drop lists tying item types to an NPC typeID (officers, named
// rats) or to an NPC group (invGroups categoryID 11). A kill rolls every matching entry,
// type entries on top of group entries, into a Cargo Container. Authored in the ESP "NPC Loot" panel.

// EnsureNPCLootTables creates the loot authoring tables at startup.
func EnsureNPCLootTables(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS npc_loot_entries (
			entryID    INT AUTO_INCREMENT PRIMARY KEY,
			npcTypeID  INT NOT NULL DEFAULT 0,
			npcGroupID INT NOT NULL DEFAULT 0,
			itemTypeID INT NOT NULL,
			dropChance DOUBLE NOT NULL DEFAULT 1,
			minQty     INT NOT NULL DEFAULT 1,
			maxQty     INT NOT NULL DEFAULT 1,
			INDEX idx_loot_npctype (npcTypeID),
			INDEX idx_loot_npcgroup (npcGroupID)
		)`); err != nil {
		return fmt.Errorf("EnsureNPCLootTables entries: %w", err)
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS npc_loot_caps (
			npcTypeID  INT NOT NULL DEFAULT 0,
			npcGroupID INT NOT NULL DEFAULT 0,
			maxDrops   INT NOT NULL,
			PRIMARY KEY (npcTypeID, npcGroupID)
		)`); err != nil {
		return fmt.Errorf("EnsureNPCLootTables caps: %w", err)
	}
	// requireRole gates a drop to a spawn role: '' = any kill, else the killed NPC's role must match.
	db.ExecContext(ctx, `ALTER TABLE npc_loot_entries ADD COLUMN requireRole VARCHAR(16) NOT NULL DEFAULT ''`)
	if err := ensureNPCLootUnique(ctx, db); err != nil {
		return err
	}
	return nil
}

// ensureNPCLootUnique enforces one entry per item per scope. Duplicates are collapsed
// (lowest entryID wins) before the index goes on.
func ensureNPCLootUnique(ctx context.Context, db *sql.DB) error {
	var have int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'npc_loot_entries'
		  AND INDEX_NAME = 'uq_loot_scope_item'`).Scan(&have); err != nil {
		return fmt.Errorf("EnsureNPCLootTables index check: %w", err)
	}
	if have > 0 {
		return nil
	}
	if _, err := db.ExecContext(ctx, `
		DELETE e FROM npc_loot_entries e
		JOIN npc_loot_entries keep
		  ON keep.npcTypeID = e.npcTypeID AND keep.npcGroupID = e.npcGroupID
		 AND keep.itemTypeID = e.itemTypeID AND keep.entryID < e.entryID`); err != nil {
		return fmt.Errorf("EnsureNPCLootTables dedupe: %w", err)
	}
	if _, err := db.ExecContext(ctx, `
		CREATE UNIQUE INDEX uq_loot_scope_item
		ON npc_loot_entries (npcTypeID, npcGroupID, itemTypeID)`); err != nil {
		return fmt.Errorf("EnsureNPCLootTables unique index: %w", err)
	}
	return nil
}

// ErrLootEntryExists is returned when an item is already on the targeted loot list.
var ErrLootEntryExists = errors.New("that item is already on this loot table — edit the existing entry instead")

// NPCLootEntry is one authored drop: item + share + quantity window. dropChance is stored
// as a fraction (0..1); the ESP UI shows percent.
type NPCLootEntry struct {
	EntryID     int64
	NPCTypeID   int64 // 0 = group-scoped entry
	NPCGroupID  int64 // 0 = type-scoped entry
	ItemTypeID  int64
	ItemGroupID int64 // invTypes.groupID of the item; a haul never holds two of one group
	DropChance  float64
	MinQty      int64
	MaxQty      int64
	RequireRole string // '' = any kill; else killed NPC's spawn role must match

	// Display fields, joined for ESP listings and empty on runtime loads.
	ItemName      string
	ItemGroupName string
	Icon          string
	IsBlueprint   bool    // drops as one copy; Min/MaxQty are its runs
	SharePct      float64 // dropChance as a percent of its role pool's budget
	PerKillPct    float64 // approx percent of kills whose haul contains this item
}

// ---- Drop-share budget ----
//
// dropChance is a relative weight at roll time, not an independent probability, so the ESP
// pages keep each role pool of a scope summing to 1.0. See docs/db-5-notes.md#npc-loot-share-budget.

// Default roll gates. The live values are LootRollChances (server_settings loot.npcAnyChance,
// loot.npcLuckyChance, loot.npcJackpotChance).
const (
	LootAnyChance     = 0.65 // a kill drops anything at all (before rates.LootDrop)
	LootLuckyChance   = 0.20 // haul of 2
	LootJackpotChance = 0.05 // haul of 3
)

// LootRoleBounty is the requireRole of a pool that pays in place of a kill bounty, for hulls
// that carry none (navy, police, customs): one guaranteed pick on every kill, whatever the
// spawn role. See docs/db-5-notes.md#npc-loot-roll-limits.
const LootRoleBounty = "bounty"

// LootRollChances answers the roll gates spawnNPCLoot uses, exported so the ESP pages quote
// the real odds. lucky + jackpot never exceeds 1: both are scaled down together.
func LootRollChances() (anyDrop, lucky, jackpot float64) {
	anyDrop = CachedSettingFloat("loot.npcAnyChance", LootAnyChance, 0, 1)
	lucky = CachedSettingFloat("loot.npcLuckyChance", LootLuckyChance, 0, 1)
	jackpot = CachedSettingFloat("loot.npcJackpotChance", LootJackpotChance, 0, 1)
	if s := lucky + jackpot; s > 1 {
		lucky, jackpot = lucky/s, jackpot/s
	}
	return anyDrop, lucky, jackpot
}

// LootExpectedDrops is the mean number of general-pool items a kill yields
// before the rate multiplier and the max-drops cap: gate × mean haul size.
func LootExpectedDrops() float64 {
	anyDrop, lucky, jackpot := LootRollChances()
	mean := 1*(1-lucky-jackpot) + 2*lucky + 3*jackpot
	return anyDrop * mean
}

// lootShareEpsilon is the "already normalized" tolerance; below it a rescale is skipped.
const lootShareEpsilon = 1e-9

// LootPoolSums returns the total drop weight of each role pool in one scope, keyed by role
// (the empty role is the general pool). A pool that sums to 1.0 is a 100% budget.
func LootPoolSums(ctx context.Context, db *sql.DB, npcTypeID, npcGroupID int64) (map[string]float64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT COALESCE(requireRole,''), SUM(dropChance)
		FROM npc_loot_entries WHERE npcTypeID=? AND npcGroupID=? GROUP BY requireRole`,
		npcTypeID, npcGroupID)
	if err != nil {
		return nil, fmt.Errorf("LootPoolSums: %w", err)
	}
	defer rows.Close()
	out := make(map[string]float64)
	for rows.Next() {
		var role string
		var sum sql.NullFloat64
		if err := rows.Scan(&role, &sum); err != nil {
			return nil, fmt.Errorf("LootPoolSums scan: %w", err)
		}
		out[role] = sum.Float64
	}
	return out, rows.Err()
}

// NormalizeLootShares rescales every role pool of one scope so its shares sum to 1.0.
// The relative odds, and so the drops, are unchanged.
func NormalizeLootShares(ctx context.Context, db *sql.DB, npcTypeID, npcGroupID int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("NormalizeLootShares begin: %w", err)
	}
	defer tx.Rollback()
	if err := normalizeLootScopeTx(ctx, tx, npcTypeID, npcGroupID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("NormalizeLootShares commit: %w", err)
	}
	return nil
}

func normalizeLootScopeTx(ctx context.Context, tx *sql.Tx, npcTypeID, npcGroupID int64) error {
	type ent struct {
		id     int64
		weight float64
		role   string
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT entryID, dropChance, COALESCE(requireRole,'')
		FROM npc_loot_entries WHERE npcTypeID=? AND npcGroupID=?`,
		npcTypeID, npcGroupID)
	if err != nil {
		return fmt.Errorf("normalizeLootScope select: %w", err)
	}
	var all []ent
	sums := make(map[string]float64)
	counts := make(map[string]int)
	for rows.Next() {
		var e ent
		if err := rows.Scan(&e.id, &e.weight, &e.role); err != nil {
			rows.Close()
			return fmt.Errorf("normalizeLootScope scan: %w", err)
		}
		if e.weight > 0 {
			sums[e.role] += e.weight
		}
		counts[e.role]++
		all = append(all, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("normalizeLootScope rows: %w", err)
	}

	for _, e := range all {
		var want float64
		if sum := sums[e.role]; sum > lootShareEpsilon {
			// A zeroed entry stays at zero.
			if e.weight > 0 {
				want = e.weight / sum
			}
		} else {
			// A pool that is all zero is split evenly so the table can still drop.
			want = 1 / float64(counts[e.role])
		}
		if math.Abs(want-e.weight) < lootShareEpsilon {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE npc_loot_entries SET dropChance=? WHERE entryID=?`, want, e.id); err != nil {
			return fmt.Errorf("normalizeLootScope update %d: %w", e.id, err)
		}
	}
	return nil
}

// CountOffBudgetLootPools reports how many role pools across every scope do not sum to
// 100%. Drives the overview's rebalance-everything button.
func CountOffBudgetLootPools(ctx context.Context, db *sql.DB) (pools, scopes int64, err error) {
	err = db.QueryRowContext(ctx, `
		SELECT COUNT(*), COUNT(DISTINCT npcTypeID, npcGroupID) FROM (
			SELECT npcTypeID, npcGroupID, SUM(dropChance) AS s
			FROM npc_loot_entries GROUP BY npcTypeID, npcGroupID, requireRole
		) t WHERE ABS(t.s - 1) > 0.0005`).Scan(&pools, &scopes)
	if err != nil {
		return 0, 0, fmt.Errorf("CountOffBudgetLootPools: %w", err)
	}
	return pools, scopes, nil
}

// NormalizeAllLootShares rescales every drop table so each role pool sums to 100%. The odds
// are unchanged. Returns the number of scopes touched.
func NormalizeAllLootShares(ctx context.Context, db *sql.DB) (int64, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT DISTINCT npcTypeID, npcGroupID FROM npc_loot_entries`)
	if err != nil {
		return 0, fmt.Errorf("NormalizeAllLootShares scopes: %w", err)
	}
	type scope struct{ typeID, groupID int64 }
	var scopes []scope
	for rows.Next() {
		var s scope
		if err := rows.Scan(&s.typeID, &s.groupID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("NormalizeAllLootShares scan: %w", err)
		}
		scopes = append(scopes, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(scopes) == 0 {
		return 0, nil
	}
	// One transaction, so a rebalance is all or nothing.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("NormalizeAllLootShares begin: %w", err)
	}
	defer tx.Rollback()
	for _, s := range scopes {
		if err := normalizeLootScopeTx(ctx, tx, s.typeID, s.groupID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("NormalizeAllLootShares commit: %w", err)
	}
	return int64(len(scopes)), nil
}

// SetLootShares rewrites the shares of one scope from the ESP sliders and renormalizes each
// role pool. Entries missing from the map keep their value and are rescaled by the pass.
func SetLootShares(ctx context.Context, db *sql.DB, npcTypeID, npcGroupID int64, shares map[int64]float64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("SetLootShares begin: %w", err)
	}
	defer tx.Rollback()
	for entryID, share := range shares {
		if share < 0 {
			share = 0
		}
		// Scoped UPDATE: an entryID from another scope cannot be reached from this page.
		if _, err := tx.ExecContext(ctx, `
			UPDATE npc_loot_entries SET dropChance=?
			WHERE entryID=? AND npcTypeID=? AND npcGroupID=?`,
			share, entryID, npcTypeID, npcGroupID); err != nil {
			return fmt.Errorf("SetLootShares update %d: %w", entryID, err)
		}
	}
	if err := normalizeLootScopeTx(ctx, tx, npcTypeID, npcGroupID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("SetLootShares commit: %w", err)
	}
	return nil
}

// AddLootEntryBalanced adds an item to a scope's drop table without a percentage: the new
// entry takes an equal slice of its role pool and the others are squeezed to pay for it.
func AddLootEntryBalanced(ctx context.Context, db *sql.DB, npcTypeID, npcGroupID, itemTypeID int64,
	minQty, maxQty int64, requireRole string) error {
	if minQty < 1 {
		minQty = 1
	}
	if maxQty < minQty {
		maxQty = minQty
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("AddLootEntryBalanced begin: %w", err)
	}
	defer tx.Rollback()

	// The average existing weight in the pool; after the normalize pass the newcomer holds an
	// equal share at any authored scale.
	var n int64
	var sum sql.NullFloat64
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*), SUM(dropChance) FROM npc_loot_entries
		WHERE npcTypeID=? AND npcGroupID=? AND COALESCE(requireRole,'')=?`,
		npcTypeID, npcGroupID, requireRole).Scan(&n, &sum); err != nil {
		return fmt.Errorf("AddLootEntryBalanced pool: %w", err)
	}
	share := 1.0
	if n > 0 && sum.Float64 > lootShareEpsilon {
		share = sum.Float64 / float64(n)
	}

	res, err := tx.ExecContext(ctx, `
		INSERT IGNORE INTO npc_loot_entries (npcTypeID, npcGroupID, itemTypeID, dropChance, minQty, maxQty, requireRole)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		npcTypeID, npcGroupID, itemTypeID, share, minQty, maxQty, requireRole)
	if err != nil {
		return fmt.Errorf("AddLootEntryBalanced: %w", err)
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return ErrLootEntryExists
	}
	if err := normalizeLootScopeTx(ctx, tx, npcTypeID, npcGroupID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("AddLootEntryBalanced commit: %w", err)
	}
	return nil
}

// UpdateNPCLootEntryQty rewrites quantity and role on an entry and leaves its share alone;
// shares belong to the slider form (SetLootShares).
func UpdateNPCLootEntryQty(ctx context.Context, db *sql.DB, entryID, minQty, maxQty int64, requireRole string) error {
	if minQty < 1 {
		minQty = 1
	}
	if maxQty < minQty {
		maxQty = minQty
	}
	_, err := db.ExecContext(ctx, `
		UPDATE npc_loot_entries SET minQty=?, maxQty=?, requireRole=? WHERE entryID=?`,
		minQty, maxQty, requireRole, entryID)
	if err != nil {
		return fmt.Errorf("UpdateNPCLootEntryQty: %w", err)
	}
	return nil
}

// AddNPCLootEntryToAllGroups inserts a group-wide drop entry into every NPC group that has
// types (invGroups categoryID 11), skipping groups that already carry the item. Each group
// is balanced like AddLootEntryBalanced. Returns how many groups received the entry.
func AddNPCLootEntryToAllGroups(ctx context.Context, db *sql.DB, itemTypeID int64, minQty, maxQty int64, requireRole string) (int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT g.groupID FROM invGroups g
		WHERE g.categoryID = 11
		  AND EXISTS (SELECT 1 FROM invTypes t WHERE t.groupID = g.groupID)`)
	if err != nil {
		return 0, fmt.Errorf("AddNPCLootEntryToAllGroups scan groups: %w", err)
	}
	var groupIDs []int64
	for rows.Next() {
		var gid int64
		if err := rows.Scan(&gid); err != nil {
			rows.Close()
			return 0, fmt.Errorf("AddNPCLootEntryToAllGroups scan: %w", err)
		}
		groupIDs = append(groupIDs, gid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return AddNPCLootEntryToGroups(ctx, db, groupIDs, itemTypeID, minQty, maxQty, requireRole)
}

// AddNPCLootEntryToGroups inserts a group-wide drop entry into the given NPC groups,
// skipping groups that already carry the item. Each group is balanced on its own. Returns
// how many groups received the entry.
func AddNPCLootEntryToGroups(ctx context.Context, db *sql.DB, groupIDs []int64, itemTypeID int64, minQty, maxQty int64, requireRole string) (int64, error) {
	if len(groupIDs) == 0 {
		return 0, nil
	}
	if minQty < 1 {
		minQty = 1
	}
	if maxQty < minQty {
		maxQty = minQty
	}
	var added int64
	for _, gid := range groupIDs {
		err := AddLootEntryBalanced(ctx, db, 0, gid, itemTypeID, minQty, maxQty, requireRole)
		if errors.Is(err, ErrLootEntryExists) {
			continue
		}
		if err != nil {
			return added, err
		}
		added++
	}
	return added, nil
}

// CopyGroupLootEntries clones one NPC group's group-wide drop list onto another. With
// replace the destination is wiped first; otherwise items it already carries keep their own
// chance and qty and count as skipped. Returns entries written and skipped.
func CopyGroupLootEntries(ctx context.Context, db *sql.DB, srcGroupID, dstGroupID int64, replace bool) (copied, skipped int64, err error) {
	if srcGroupID == dstGroupID {
		return 0, 0, errors.New("source and destination are the same group")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("CopyGroupLootEntries begin: %w", err)
	}
	defer tx.Rollback()

	var srcCount int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM npc_loot_entries WHERE npcTypeID = 0 AND npcGroupID = ?`,
		srcGroupID).Scan(&srcCount); err != nil {
		return 0, 0, fmt.Errorf("CopyGroupLootEntries source count: %w", err)
	}
	if srcCount == 0 {
		return 0, 0, errors.New("that group has no loot entries to copy")
	}

	if replace {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM npc_loot_entries WHERE npcTypeID = 0 AND npcGroupID = ?`,
			dstGroupID); err != nil {
			return 0, 0, fmt.Errorf("CopyGroupLootEntries clear: %w", err)
		}
	}
	// NOT EXISTS keeps the merge path off the uq_loot_scope_item index; on replace the
	// destination is empty.
	res, err := tx.ExecContext(ctx, `
		INSERT INTO npc_loot_entries (npcTypeID, npcGroupID, itemTypeID, dropChance, minQty, maxQty, requireRole)
		SELECT 0, ?, s.itemTypeID, s.dropChance, s.minQty, s.maxQty, s.requireRole
		FROM npc_loot_entries s
		WHERE s.npcTypeID = 0 AND s.npcGroupID = ?
		  AND NOT EXISTS (SELECT 1 FROM npc_loot_entries d
		                  WHERE d.npcTypeID = 0 AND d.npcGroupID = ? AND d.itemTypeID = s.itemTypeID)`,
		dstGroupID, srcGroupID, dstGroupID)
	if err != nil {
		return 0, 0, fmt.Errorf("CopyGroupLootEntries insert: %w", err)
	}
	copied, _ = res.RowsAffected()
	// A merge joins two independently budgeted lists, so renormalize the destination.
	if err := normalizeLootScopeTx(ctx, tx, 0, dstGroupID); err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("CopyGroupLootEntries commit: %w", err)
	}
	return copied, srcCount - copied, nil
}

// CopyGroupLootCap mirrors one group's max-drops cap onto another, clearing the destination
// cap when the source is uncapped.
func CopyGroupLootCap(ctx context.Context, db *sql.DB, srcGroupID, dstGroupID int64) (int64, error) {
	cap, err := GetRawNPCLootCap(ctx, db, 0, srcGroupID)
	if err != nil {
		return 0, err
	}
	if err := SetNPCLootCap(ctx, db, 0, dstGroupID, cap); err != nil {
		return 0, err
	}
	return cap, nil
}

// DeleteNPCLootEntry removes a drop entry. A caller that owns a scope follows up with
// NormalizeLootShares so the survivors reclaim the share.
func DeleteNPCLootEntry(ctx context.Context, db *sql.DB, entryID int64) error {
	_, err := db.ExecContext(ctx, `DELETE FROM npc_loot_entries WHERE entryID=?`, entryID)
	if err != nil {
		return fmt.Errorf("DeleteNPCLootEntry: %w", err)
	}
	return nil
}

// Belt spawn roles by NPC type, cached beltRoleTTL: only the belt spawner records a role,
// so a dungeon rat takes the one its type carries in npcRegionSpawns.
// See docs/db-5-notes.md#npc-loot-share-budget.
const beltRoleTTL = 5 * time.Minute

var beltRoleCache struct {
	mu    sync.Mutex
	roles map[int64]string
	at    time.Time
}

var beltRoleRank = map[string]int{"normal": 0, "hauler": 1, "commander": 2, "officer": 3}

// BeltSpawnRoleForType returns the role npcRegionSpawns gives typeID, or "normal" for a type
// no belt spawns. On a query error it returns the error and "normal" and keeps its old map.
func BeltSpawnRoleForType(ctx context.Context, db *sql.DB, typeID int64) (string, error) {
	c := &beltRoleCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.roles == nil || time.Since(c.at) > beltRoleTTL {
		rows, err := db.QueryContext(ctx,
			`SELECT typeID, role FROM npcRegionSpawns GROUP BY typeID, role`)
		if err != nil {
			return "normal", fmt.Errorf("BeltSpawnRoleForType: %w", err)
		}
		roles := make(map[int64]string)
		for rows.Next() {
			var id int64
			var role string
			if err := rows.Scan(&id, &role); err != nil {
				rows.Close()
				return "normal", fmt.Errorf("BeltSpawnRoleForType scan: %w", err)
			}
			if cur, ok := roles[id]; !ok || beltRoleRank[role] > beltRoleRank[cur] {
				roles[id] = role
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return "normal", fmt.Errorf("BeltSpawnRoleForType rows: %w", err)
		}
		c.roles, c.at = roles, time.Now()
	}
	if role, ok := c.roles[typeID]; ok && role != "" {
		return role, nil
	}
	return "normal", nil
}

// GetLootDropsForNPC returns the table that rolls for an NPC of the given type: its type
// entries merged with its group entries by MergeLootScopes, with no display joins. DropChance
// is the merged weight, so it is not the authored share when both scopes have a general pool.
func GetLootDropsForNPC(ctx context.Context, db *sql.DB, npcTypeID, npcGroupID int64) ([]NPCLootEntry, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT e.entryID, e.npcTypeID, e.npcGroupID, e.itemTypeID, COALESCE(t.groupID, 0),
		       e.dropChance, e.minQty, e.maxQty, COALESCE(e.requireRole, '')
		FROM npc_loot_entries e
		LEFT JOIN invTypes t ON t.typeID = e.itemTypeID
		WHERE e.npcTypeID = ? OR (e.npcTypeID = 0 AND e.npcGroupID = ?)`,
		npcTypeID, npcGroupID)
	if err != nil {
		return nil, fmt.Errorf("GetLootDropsForNPC: %w", err)
	}
	defer rows.Close()
	var out []NPCLootEntry
	for rows.Next() {
		var e NPCLootEntry
		if err := rows.Scan(&e.EntryID, &e.NPCTypeID, &e.NPCGroupID, &e.ItemTypeID, &e.ItemGroupID,
			&e.DropChance, &e.MinQty, &e.MaxQty, &e.RequireRole); err != nil {
			return nil, fmt.Errorf("GetLootDropsForNPC scan: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return MergeLootScopes(out), nil
}

// GetNPCLootCap resolves the max-drops cap for an NPC: a type cap wins over the group cap;
// 0 means uncapped.
func GetNPCLootCap(ctx context.Context, db *sql.DB, npcTypeID, npcGroupID int64) (int64, error) {
	var cap int64
	err := db.QueryRowContext(ctx,
		`SELECT maxDrops FROM npc_loot_caps WHERE npcTypeID=? AND npcGroupID=0`, npcTypeID).Scan(&cap)
	if err == nil {
		return cap, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("GetNPCLootCap type: %w", err)
	}
	err = db.QueryRowContext(ctx,
		`SELECT maxDrops FROM npc_loot_caps WHERE npcTypeID=0 AND npcGroupID=?`, npcGroupID).Scan(&cap)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("GetNPCLootCap group: %w", err)
	}
	return cap, nil
}

// SetNPCLootCap upserts the max-drops cap for a type or group scope; maxDrops <= 0 clears it.
func SetNPCLootCap(ctx context.Context, db *sql.DB, npcTypeID, npcGroupID, maxDrops int64) error {
	if maxDrops <= 0 {
		_, err := db.ExecContext(ctx,
			`DELETE FROM npc_loot_caps WHERE npcTypeID=? AND npcGroupID=?`, npcTypeID, npcGroupID)
		if err != nil {
			return fmt.Errorf("SetNPCLootCap delete: %w", err)
		}
		return nil
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO npc_loot_caps (npcTypeID, npcGroupID, maxDrops) VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE maxDrops = VALUES(maxDrops)`,
		npcTypeID, npcGroupID, maxDrops)
	if err != nil {
		return fmt.Errorf("SetNPCLootCap upsert: %w", err)
	}
	return nil
}

// SetNPCLootCapForGroups applies one max-drops cap to several NPC groups; maxDrops <= 0
// clears them. Returns how many groups were targeted.
func SetNPCLootCapForGroups(ctx context.Context, db *sql.DB, groupIDs []int64, maxDrops int64) (int64, error) {
	if len(groupIDs) == 0 {
		return 0, nil
	}
	placeholders := make([]string, len(groupIDs))
	args := make([]any, 0, len(groupIDs)+1)
	if maxDrops > 0 {
		args = append(args, maxDrops)
	}
	for i, gid := range groupIDs {
		placeholders[i] = "?"
		args = append(args, gid)
	}
	in := strings.Join(placeholders, ",")

	if maxDrops <= 0 {
		if _, err := db.ExecContext(ctx,
			`DELETE FROM npc_loot_caps WHERE npcTypeID = 0 AND npcGroupID IN (`+in+`)`,
			args...); err != nil {
			return 0, fmt.Errorf("SetNPCLootCapForGroups clear: %w", err)
		}
		return int64(len(groupIDs)), nil
	}
	// An upsert counts 2 affected rows per update, so report the target count.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO npc_loot_caps (npcTypeID, npcGroupID, maxDrops)
		SELECT 0, g.groupID, ?
		FROM invGroups g
		WHERE g.groupID IN (`+in+`)
		ON DUPLICATE KEY UPDATE maxDrops = VALUES(maxDrops)`,
		args...); err != nil {
		return 0, fmt.Errorf("SetNPCLootCapForGroups upsert: %w", err)
	}
	return int64(len(groupIDs)), nil
}

// GetRawNPCLootCap reads the cap row for exactly one scope, with no fallback.
func GetRawNPCLootCap(ctx context.Context, db *sql.DB, npcTypeID, npcGroupID int64) (int64, error) {
	var cap int64
	err := db.QueryRowContext(ctx,
		`SELECT maxDrops FROM npc_loot_caps WHERE npcTypeID=? AND npcGroupID=?`,
		npcTypeID, npcGroupID).Scan(&cap)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("GetRawNPCLootCap: %w", err)
	}
	return cap, nil
}

// CreateNPCLootItem inserts a rolled drop into a loot container as a non-singleton stack.
// A blueprint drops as one singleton copy whose runs are the rolled qty (LootBlueprintRuns):
// a packaged blueprint with no invBlueprints row reads as an original everywhere.
// See docs/db-5-notes.md#npc-loot-share-budget.
func CreateNPCLootItem(ctx context.Context, db *sql.DB, containerID, ownerID, itemTypeID, qty int64) error {
	var categoryID int64
	var maxRuns sql.NullInt64
	err := db.QueryRowContext(ctx, `
		SELECT COALESCE(g.categoryID, 0), b.maxProductionLimit
		  FROM invTypes t
		  LEFT JOIN invGroups g ON g.groupID = t.groupID
		  LEFT JOIN invBlueprintTypes b ON b.blueprintTypeID = t.typeID
		 WHERE t.typeID = ?`, itemTypeID).Scan(&categoryID, &maxRuns)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("CreateNPCLootItem: unknown itemTypeID %d", itemTypeID)
	}
	if err != nil {
		return fmt.Errorf("CreateNPCLootItem type lookup: %w", err)
	}
	if categoryID == categoryBlueprint {
		return createLootBlueprintCopy(ctx, db, containerID, ownerID, itemTypeID,
			LootBlueprintRuns(qty, maxRuns.Int64))
	}

	res, err := db.ExecContext(ctx, `
		INSERT INTO entity (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
		SELECT typeName, typeID, ?, ?, 0, 0, 0, ?, 0, 0, 0, NULL
		FROM invTypes WHERE typeID = ?`,
		ownerID, containerID, qty, itemTypeID)
	if err != nil {
		return fmt.Errorf("CreateNPCLootItem: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("CreateNPCLootItem: unknown itemTypeID %d", itemTypeID)
	}
	return nil
}

// LootBlueprintRuns turns a rolled quantity into the runs of a dropped copy: at least 1,
// capped at maxRuns (maxRuns <= 0 means no limit row, so no cap).
func LootBlueprintRuns(qty, maxRuns int64) int64 {
	if qty < 1 {
		qty = 1
	}
	if maxRuns > 0 && qty > maxRuns {
		qty = maxRuns
	}
	return qty
}

// createLootBlueprintCopy writes one singleton blueprint copy (copy = 1, ME 0, PE 0) into a
// loot container: the entity row and its invBlueprints row in one transaction.
func createLootBlueprintCopy(ctx context.Context, db *sql.DB, containerID, ownerID, typeID, runs int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("createLootBlueprintCopy begin: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `
		INSERT INTO entity (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
		SELECT typeName, typeID, ?, ?, 0, 0, 1, 1, 0, 0, 0, NULL
		FROM invTypes WHERE typeID = ?`,
		ownerID, containerID, typeID)
	if err != nil {
		return fmt.Errorf("createLootBlueprintCopy entity: %w", err)
	}
	itemID, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("createLootBlueprintCopy LastInsertId: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO invBlueprints (blueprintID, materialLevel, productivityLevel, copy, licensedProductionRunsRemaining)
		VALUES (?, 0, 0, 1, ?)`, itemID, runs); err != nil {
		return fmt.Errorf("createLootBlueprintCopy blueprint row: %w", err)
	}
	return tx.Commit()
}

// ── ESP browse/authoring queries ────────────────────────────────────────────

// ESPLootGroupRow is one NPC group (invGroups categoryID 11) with loot stats.
type ESPLootGroupRow struct {
	GroupID    int64
	GroupName  string
	TypeCount  int64 // NPC types in the group
	EntryCount int64 // group-wide loot entries
	MaxDrops   int64 // group cap (0 = uncapped)
}

// ESPListNPCLootGroups lists NPC entity groups filtered by name, with per-group loot entry
// counts. onlyWithLoot narrows to groups that already have entries.
func ESPListNPCLootGroups(ctx context.Context, db *sql.DB, query string, onlyWithLoot bool) ([]ESPLootGroupRow, error) {
	having := ""
	if onlyWithLoot {
		having = "HAVING entryCount > 0"
	}
	rows, err := db.QueryContext(ctx, `
		SELECT g.groupID, g.groupName,
		       (SELECT COUNT(*) FROM invTypes t WHERE t.groupID = g.groupID) AS typeCount,
		       (SELECT COUNT(*) FROM npc_loot_entries e WHERE e.npcGroupID = g.groupID AND e.npcTypeID = 0) AS entryCount,
		       COALESCE((SELECT c.maxDrops FROM npc_loot_caps c WHERE c.npcGroupID = g.groupID AND c.npcTypeID = 0), 0)
		FROM invGroups g
		WHERE g.categoryID = 11 AND g.groupName LIKE ?
		`+having+`
		ORDER BY g.groupName`, "%"+query+"%")
	if err != nil {
		return nil, fmt.Errorf("ESPListNPCLootGroups: %w", err)
	}
	defer rows.Close()
	var out []ESPLootGroupRow
	for rows.Next() {
		var r ESPLootGroupRow
		if err := rows.Scan(&r.GroupID, &r.GroupName, &r.TypeCount, &r.EntryCount, &r.MaxDrops); err != nil {
			return nil, fmt.Errorf("ESPListNPCLootGroups scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ESPLootTypeRow is one NPC type with its type-specific loot entry count.
type ESPLootTypeRow struct {
	TypeID     int64
	TypeName   string
	GroupID    int64
	GroupName  string
	EntryCount int64
}

// ESPListNPCTypesInGroup lists the NPC types of a group with per-type override counts.
func ESPListNPCTypesInGroup(ctx context.Context, db *sql.DB, groupID int64) ([]ESPLootTypeRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT t.typeID, t.typeName, t.groupID, g.groupName,
		       (SELECT COUNT(*) FROM npc_loot_entries e WHERE e.npcTypeID = t.typeID) AS entryCount
		FROM invTypes t
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE t.groupID = ?
		ORDER BY t.typeName`, groupID)
	if err != nil {
		return nil, fmt.Errorf("ESPListNPCTypesInGroup: %w", err)
	}
	defer rows.Close()
	var out []ESPLootTypeRow
	for rows.Next() {
		var r ESPLootTypeRow
		if err := rows.Scan(&r.TypeID, &r.TypeName, &r.GroupID, &r.GroupName, &r.EntryCount); err != nil {
			return nil, fmt.Errorf("ESPListNPCTypesInGroup scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ESPListLootTypeOverrides lists every NPC type that has type-specific loot.
func ESPListLootTypeOverrides(ctx context.Context, db *sql.DB) ([]ESPLootTypeRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT e.npcTypeID, COALESCE(t.typeName, CONCAT('type ', e.npcTypeID)),
		       COALESCE(t.groupID, 0), COALESCE(g.groupName, ''), COUNT(*)
		FROM npc_loot_entries e
		LEFT JOIN invTypes t ON t.typeID = e.npcTypeID
		LEFT JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.npcTypeID > 0
		GROUP BY e.npcTypeID, t.typeName, t.groupID, g.groupName
		ORDER BY t.typeName`)
	if err != nil {
		return nil, fmt.Errorf("ESPListLootTypeOverrides: %w", err)
	}
	defer rows.Close()
	var out []ESPLootTypeRow
	for rows.Next() {
		var r ESPLootTypeRow
		if err := rows.Scan(&r.TypeID, &r.TypeName, &r.GroupID, &r.GroupName, &r.EntryCount); err != nil {
			return nil, fmt.Errorf("ESPListLootTypeOverrides scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// espListLootEntries loads entries for one scope with display joins.
func espListLootEntries(ctx context.Context, db *sql.DB, where string, arg int64) ([]NPCLootEntry, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT e.entryID, e.npcTypeID, e.npcGroupID, e.itemTypeID, e.dropChance, e.minQty, e.maxQty,
		       COALESCE(e.requireRole, ''),
		       COALESCE(t.typeName, CONCAT('type ', e.itemTypeID)),
		       COALESCE(g.groupName, ''),
		       COALESCE(eg.icon, ''),
		       COALESCE(g.categoryID, 0) = 9
		FROM npc_loot_entries e
		LEFT JOIN invTypes t ON t.typeID = e.itemTypeID
		LEFT JOIN invGroups g ON g.groupID = t.groupID
		LEFT JOIN eveGraphics eg ON eg.graphicID = t.graphicID
		WHERE `+where+`
		ORDER BY e.dropChance DESC, t.typeName`, arg)
	if err != nil {
		return nil, fmt.Errorf("espListLootEntries: %w", err)
	}
	defer rows.Close()
	var out []NPCLootEntry
	for rows.Next() {
		var e NPCLootEntry
		if err := rows.Scan(&e.EntryID, &e.NPCTypeID, &e.NPCGroupID, &e.ItemTypeID,
			&e.DropChance, &e.MinQty, &e.MaxQty, &e.RequireRole,
			&e.ItemName, &e.ItemGroupName, &e.Icon, &e.IsBlueprint); err != nil {
			return nil, fmt.Errorf("espListLootEntries scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ESPListLootEntriesForGroup returns the group-wide entries of an NPC group.
func ESPListLootEntriesForGroup(ctx context.Context, db *sql.DB, groupID int64) ([]NPCLootEntry, error) {
	return espListLootEntries(ctx, db, "e.npcTypeID = 0 AND e.npcGroupID = ?", groupID)
}

// ESPListLootEntriesForType returns the type-specific entries of one NPC type.
func ESPListLootEntriesForType(ctx context.Context, db *sql.DB, typeID int64) ([]NPCLootEntry, error) {
	return espListLootEntries(ctx, db, "e.npcTypeID = ?", typeID)
}

// ESPLootItemRow is one droppable item candidate from the item search box.
type ESPLootItemRow struct {
	TypeID       int64
	TypeName     string
	GroupName    string
	CategoryName string
	BasePrice    float64
	Icon         string
	Published    bool
}

// ESPSearchLootItems searches item types by name (or exact typeID when the query is numeric)
// for the loot entry autocomplete. Unpublished types are included, since many named and meta
// modules are published = 0; they sort last. Exact and prefix matches sort first.
func ESPSearchLootItems(ctx context.Context, db *sql.DB, query string) ([]ESPLootItemRow, error) {
	like := "%" + escapeLootLike(query) + "%"
	rows, err := db.QueryContext(ctx, `
		SELECT t.typeID, t.typeName, g.groupName, c.categoryName,
		       COALESCE(t.basePrice, 0), COALESCE(eg.icon, ''), COALESCE(t.published, 0) = 1
		FROM invTypes t
		JOIN invGroups g ON g.groupID = t.groupID
		JOIN invCategories c ON c.categoryID = g.categoryID
		LEFT JOIN eveGraphics eg ON eg.graphicID = t.graphicID
		WHERE (t.typeName LIKE ? ESCAPE '\\' OR t.typeID = ?)
		  AND g.categoryID NOT IN (11)
		ORDER BY (t.typeName = ?) DESC,
		         (t.typeName LIKE ? ESCAPE '\\') DESC,
		         t.published DESC,
		         t.typeName
		LIMIT 50`, like, query, query, escapeLootLike(query)+"%")
	if err != nil {
		return nil, fmt.Errorf("ESPSearchLootItems: %w", err)
	}
	defer rows.Close()
	var out []ESPLootItemRow
	for rows.Next() {
		var r ESPLootItemRow
		if err := rows.Scan(&r.TypeID, &r.TypeName, &r.GroupName, &r.CategoryName, &r.BasePrice, &r.Icon, &r.Published); err != nil {
			return nil, fmt.Errorf("ESPSearchLootItems scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// escapeLootLike neutralises LIKE wildcards in a typed query so '_' and '%' match literally.
func escapeLootLike(q string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(q)
}

// ESPGetNPCTypeInfo returns name and group of one NPC type for the loot page header (nil if unknown).
func ESPGetNPCTypeInfo(ctx context.Context, db *sql.DB, typeID int64) (*ESPLootTypeRow, error) {
	var r ESPLootTypeRow
	err := db.QueryRowContext(ctx, `
		SELECT t.typeID, t.typeName, t.groupID, g.groupName,
		       (SELECT COUNT(*) FROM npc_loot_entries e WHERE e.npcTypeID = t.typeID)
		FROM invTypes t
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE t.typeID = ?`, typeID).Scan(&r.TypeID, &r.TypeName, &r.GroupID, &r.GroupName, &r.EntryCount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("ESPGetNPCTypeInfo: %w", err)
	}
	return &r, nil
}

// ESPGetGroupName returns the name of an invGroups row ("" if unknown).
func ESPGetGroupName(ctx context.Context, db *sql.DB, groupID int64) (string, error) {
	var name string
	err := db.QueryRowContext(ctx,
		`SELECT groupName FROM invGroups WHERE groupID = ?`, groupID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("ESPGetGroupName: %w", err)
	}
	return name, nil
}
