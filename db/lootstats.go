package db

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Loot roll statistics: every RollLootTables result that reaches a pilot is counted here by
// source, and the core logs and resets the tally on a timer ([loot-stats] lines in server.log).
// Watched is the core death drop (a container in a loaded system), unwatched the rolebots'
// abstract kills.

// LootStatSource names where a roll came from in the [loot-stats] summary.
const (
	LootStatWatched   = "watched"
	LootStatUnwatched = "unwatched"
)

type lootSourceStats struct {
	kills, withDrops, roleKills, items int64
	byGroup                            map[int64]int64
	byType                             map[int64]int64
}

var lootStats struct {
	mu  sync.Mutex
	src map[string]*lootSourceStats
}

// NoteLootRoll counts one kill's roll for the [loot-stats] summary.
func NoteLootRoll(source, killedRole string, drops []LootDrop) {
	lootStats.mu.Lock()
	defer lootStats.mu.Unlock()
	if lootStats.src == nil {
		lootStats.src = make(map[string]*lootSourceStats)
	}
	s := lootStats.src[source]
	if s == nil {
		s = &lootSourceStats{byGroup: map[int64]int64{}, byType: map[int64]int64{}}
		lootStats.src[source] = s
	}
	s.kills++
	if killedRole != "" && killedRole != "normal" {
		s.roleKills++
	}
	if len(drops) > 0 {
		s.withDrops++
	}
	for _, d := range drops {
		s.items++
		s.byGroup[d.ItemGroupID]++
		s.byType[d.ItemTypeID]++
	}
}

// DrainLootStatsLines returns the [loot-stats] summary lines for the tally so far and resets
// it: per source, the kill count, the share of kills that dropped, items per kill, the top item
// groups and the top items. Names come from invGroups and invTypes in one query each.
func DrainLootStatsLines(ctx context.Context, db *sql.DB, topGroups, topTypes int) []string {
	lootStats.mu.Lock()
	src := lootStats.src
	lootStats.src = nil
	lootStats.mu.Unlock()
	if len(src) == 0 {
		return nil
	}
	groupIDs, typeIDs := map[int64]bool{}, map[int64]bool{}
	for _, s := range src {
		for g := range s.byGroup {
			groupIDs[g] = true
		}
		for t := range s.byType {
			typeIDs[t] = true
		}
	}
	groupNames := lootStatNames(ctx, db, "SELECT groupID, groupName FROM invGroups WHERE groupID IN (%s)", groupIDs)
	typeNames := lootStatNames(ctx, db, "SELECT typeID, typeName FROM invTypes WHERE typeID IN (%s)", typeIDs)

	sources := make([]string, 0, len(src))
	for k := range src {
		sources = append(sources, k)
	}
	sort.Strings(sources)
	var out []string
	for _, k := range sources {
		s := src[k]
		perKill := 0.0
		dropPct := 0.0
		if s.kills > 0 {
			perKill = float64(s.items) / float64(s.kills)
			dropPct = 100 * float64(s.withDrops) / float64(s.kills)
		}
		out = append(out, fmt.Sprintf("%s: %d kills (%d role), %.0f%% dropped, %d items, %.2f per kill",
			k, s.kills, s.roleKills, dropPct, s.items, perKill))
		if s.items > 0 {
			out = append(out, fmt.Sprintf("%s groups: %s", k, lootStatTop(s.byGroup, groupNames, s.items, topGroups)))
			out = append(out, fmt.Sprintf("%s items: %s", k, lootStatTop(s.byType, typeNames, s.items, topTypes)))
		}
	}
	return out
}

func lootStatTop(counts map[int64]int64, names map[int64]string, total int64, n int) string {
	type kv struct {
		id int64
		c  int64
	}
	list := make([]kv, 0, len(counts))
	for id, c := range counts {
		list = append(list, kv{id, c})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].c != list[j].c {
			return list[i].c > list[j].c
		}
		return list[i].id < list[j].id
	})
	if len(list) > n {
		list = list[:n]
	}
	parts := make([]string, 0, len(list))
	for _, e := range list {
		name := names[e.id]
		if name == "" {
			name = fmt.Sprintf("#%d", e.id)
		}
		parts = append(parts, fmt.Sprintf("%s %d (%.1f%%)", name, e.c, 100*float64(e.c)/float64(total)))
	}
	return strings.Join(parts, ", ")
}

func lootStatNames(ctx context.Context, db *sql.DB, query string, ids map[int64]bool) map[int64]string {
	names := make(map[int64]string, len(ids))
	if db == nil || len(ids) == 0 {
		return names
	}
	args := make([]any, 0, len(ids))
	marks := make([]string, 0, len(ids))
	for id := range ids {
		args = append(args, id)
		marks = append(marks, "?")
	}
	rows, err := db.QueryContext(ctx, fmt.Sprintf(query, strings.Join(marks, ",")), args...)
	if err != nil {
		return names
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name string
		if rows.Scan(&id, &name) == nil {
			names[id] = name
		}
	}
	return names
}
