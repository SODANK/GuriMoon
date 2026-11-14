package db

import (
	"context"
	"database/sql"
	"math/rand"

	"gurimoon/rates"
)

// The NPC loot roll, shared by the core death drop (rollNPCLoot in cmd/gurimoon) and by modules
// that roll a kill without spawning a container (the rolebots' unwatched fights). One copy, so a
// kill pays the same whoever watched it. See docs/db-5-notes.md#npc-loot-roll-limits.

// LootRNG is the randomness a loot roll draws on. *rand.Rand satisfies it; GlobalLootRNG uses
// math/rand's shared source.
type LootRNG interface {
	Float64() float64
	Int63n(n int64) int64
}

type globalLootRNG struct{}

func (globalLootRNG) Float64() float64     { return rand.Float64() }
func (globalLootRNG) Int63n(n int64) int64 { return rand.Int63n(n) }

// GlobalLootRNG draws from math/rand's shared source.
var GlobalLootRNG LootRNG = globalLootRNG{}

// LootDrop is one rolled loot line: a type and a quantity.
type LootDrop struct {
	ItemTypeID  int64
	Qty         int64
	ItemGroupID int64 // invTypes.groupID of the item, for the [loot-stats] tally
}

// MergeLootScopes turns an NPC's type entries (NPCTypeID != 0) and group entries into the one
// table that rolls, in input order, with DropChance set to the merged weight. The input is not
// modified.
//   - A type entry supersedes the group entry for the same item.
//   - A role pool (commander, officer, bounty) the type defines replaces the group's pool of
//     that role.
//   - The general pools stack flat: each scope's weights become share times entry count, so a
//     one-item list beside a 150-item list gets 1/151 of the picks, not half.
//
// See docs/db-5-notes.md#npc-loot-share-budget.
func MergeLootScopes(entries []NPCLootEntry) []NPCLootEntry {
	typeItems := make(map[int64]bool)
	typeRoles := make(map[string]bool)
	for _, e := range entries {
		if e.NPCTypeID != 0 {
			typeItems[e.ItemTypeID] = true
			typeRoles[e.RequireRole] = true
		}
	}
	out := make([]NPCLootEntry, 0, len(entries))
	for _, e := range entries {
		if e.NPCTypeID == 0 {
			if typeItems[e.ItemTypeID] {
				continue
			}
			if e.RequireRole != "" && typeRoles[e.RequireRole] {
				continue
			}
		}
		out = append(out, e)
	}
	if !typeRoles[""] {
		return out
	}

	// Both scopes have a general pool: put each on a per-entry footing.
	var sum [2]float64
	var n [2]int
	scopeOf := func(e NPCLootEntry) int {
		if e.NPCTypeID != 0 {
			return 1
		}
		return 0
	}
	for _, e := range out {
		if e.RequireRole == "" && e.DropChance > 0 {
			s := scopeOf(e)
			sum[s] += e.DropChance
			n[s]++
		}
	}
	if n[0] == 0 || n[1] == 0 {
		return out
	}
	for i := range out {
		e := &out[i]
		if e.RequireRole != "" || e.DropChance <= 0 {
			continue
		}
		s := scopeOf(*e)
		e.DropChance = e.DropChance / sum[s] * float64(n[s])
	}
	return out
}

// SplitLootPools sorts a merged table by what rolls it: the general pool, the killed role's
// exclusive pool, and the bounty pool, which rolls for every role. Entries gated on another
// role never drop.
func SplitLootPools(entries []NPCLootEntry, killedRole string) (normal, role, bounty []NPCLootEntry) {
	for _, e := range entries {
		switch e.RequireRole {
		case "":
			normal = append(normal, e)
		case LootRoleBounty:
			bounty = append(bounty, e)
		case killedRole:
			role = append(role, e)
		}
	}
	return normal, role, bounty
}

// PickLootWeighted draws an index with DropChance as a relative weight (an entry's real cut is
// weight/total), or -1 when nothing can drop.
func PickLootWeighted(pool []NPCLootEntry, rng LootRNG) int {
	total := 0.0
	for _, e := range pool {
		if e.DropChance > 0 {
			total += e.DropChance
		}
	}
	if total <= 0 {
		return -1
	}
	r := rng.Float64() * total
	last := -1
	for i, e := range pool {
		if e.DropChance <= 0 {
			continue
		}
		last = i
		r -= e.DropChance
		if r < 0 {
			return i
		}
	}
	return last
}

// RollLootQty rolls an entry's quantity inside its Min and Max.
func RollLootQty(e NPCLootEntry, rng LootRNG) int64 {
	qty := e.MinQty
	if e.MaxQty > e.MinQty {
		qty += rng.Int63n(e.MaxQty - e.MinQty + 1)
	}
	return qty
}

// RollLootTables rolls one kill against a merged table (MergeLootScopes):
//   - a role kill (killedRole other than "normal" or "") gets one guaranteed pick from its pool;
//   - a bounty pool pays one pick on every kill, outside the gate, rates.LootDrop and the cap;
//   - the general haul passes the gate (LootRollChances' anyDrop times rates.LootDrop), sizes at
//     1, 2 or 3 items, is truncated to maxDrops (0 = uncapped), and draws without repeats.
//
// A haul never carries two items of one invTypes group: a picked group is out of the running
// for every later pick, the role and bounty drops included.
func RollLootTables(entries []NPCLootEntry, killedRole string, maxDrops int64, rng LootRNG) []LootDrop {
	if killedRole == "" {
		killedRole = "normal"
	}
	normalPool, rolePool, bountyPool := SplitLootPools(entries, killedRole)
	takenGroups := make(map[int64]bool)
	var rolled []LootDrop

	if killedRole != "normal" {
		if i := PickLootWeighted(rolePool, rng); i >= 0 {
			if qty := RollLootQty(rolePool[i], rng); qty > 0 {
				rolled = append(rolled, LootDrop{rolePool[i].ItemTypeID, qty, rolePool[i].ItemGroupID})
				if g := rolePool[i].ItemGroupID; g != 0 {
					takenGroups[g] = true
				}
			}
		}
	}

	if i := PickLootWeighted(bountyPool, rng); i >= 0 {
		d := bountyPool[i]
		rolled = append(rolled, LootDrop{d.ItemTypeID, RollLootQty(d, rng), d.ItemGroupID})
		if d.ItemGroupID != 0 {
			takenGroups[d.ItemGroupID] = true
		}
	}

	anyDrop, lucky, jackpot := LootRollChances()
	if len(normalPool) == 0 || rng.Float64() >= anyDrop*rates.LootDrop {
		return rolled
	}
	n := 1
	switch r := rng.Float64(); {
	case r < jackpot:
		n = 3
	case r < jackpot+lucky:
		n = 2
	}
	if maxDrops > 0 && int64(n) > maxDrops {
		n = int(maxDrops)
	}
	pool := make([]NPCLootEntry, 0, len(normalPool))
	for _, e := range normalPool {
		if e.ItemGroupID != 0 && takenGroups[e.ItemGroupID] {
			continue
		}
		pool = append(pool, e)
	}
	for picked := 0; picked < n && len(pool) > 0; picked++ {
		i := PickLootWeighted(pool, rng)
		if i < 0 {
			break
		}
		e := pool[i]
		// Drop the picked entry and every other entry of its group before the next pick.
		filtered := pool[:0]
		for j := range pool {
			if j == i {
				continue
			}
			if e.ItemGroupID != 0 && pool[j].ItemGroupID == e.ItemGroupID {
				continue
			}
			filtered = append(filtered, pool[j])
		}
		pool = filtered
		if qty := RollLootQty(e, rng); qty > 0 {
			rolled = append(rolled, LootDrop{e.ItemTypeID, qty, e.ItemGroupID})
		}
	}
	return rolled
}

// RollNPCLootTables loads an NPC's merged table and cap and rolls one kill, counted as an
// unwatched roll in the [loot-stats] tally. Only the authored type and group tables: per-ball
// dungeon loot and the dungeon role lookup stay with the core death drop. An empty killedRole
// is an ordinary rat.
func RollNPCLootTables(ctx context.Context, db *sql.DB, npcTypeID, npcGroupID int64, killedRole string, rng LootRNG) ([]LootDrop, error) {
	entries, err := GetLootDropsForNPC(ctx, db, npcTypeID, npcGroupID)
	if err != nil || len(entries) == 0 {
		return nil, err
	}
	maxDrops, err := GetNPCLootCap(ctx, db, npcTypeID, npcGroupID)
	if err != nil {
		return nil, err
	}
	drops := RollLootTables(entries, killedRole, maxDrops, rng)
	NoteLootRoll(LootStatUnwatched, killedRole, drops)
	return drops, nil
}
