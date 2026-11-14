package esp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

// NPC loot authoring panel over npc_loot_entries / npc_loot_caps (db/npclootdb.go). Two scopes:
//   - group-wide: /loot/group/<invGroupID>
//   - type-specific: /loot/npc/<typeID> (officers and named rats, stacked on group loot)
//
// A drop table is a 100% budget per role pool, and an entry's number is its share of the pool.
// Sliders redistribute, adds carve a slice out of the existing entries, and nothing is authored
// as a free-floating percentage. See docs/esp-2-notes.md#loot-share-budget.

// decorateLootShares fills the display-only share fields on a scope's entries
// and returns the summed weight of each role pool. SharePct is the entry's cut
// of its pool; PerKillPct is the rough odds of seeing it in a given kill's
// haul (before rates.LootDrop and the max-drops cap).
func decorateLootShares(entries []evedb.NPCLootEntry) map[string]float64 {
	sums := make(map[string]float64)
	for _, e := range entries {
		if e.DropChance > 0 {
			sums[e.RequireRole] += e.DropChance
		}
	}
	for i := range entries {
		e := &entries[i]
		sum := sums[e.RequireRole]
		if sum <= 0 {
			continue
		}
		share := e.DropChance / sum
		e.SharePct = share * 100
		if e.RequireRole != "" {
			// Role and bounty pools get one guaranteed pick per kill, so the share is the odds.
			e.PerKillPct = share * 100
		} else {
			e.PerKillPct = share * evedb.LootExpectedDrops() * 100
		}
		if e.PerKillPct > 100 {
			e.PerKillPct = 100
		}
	}
	return sums
}

// decorateMergedOdds rewrites PerKillPct on an NPC type page's own and inherited entries from
// the table that rolls for that NPC (evedb.MergeLootScopes). SharePct stays each list's share of
// its own budget, which is what the sliders edit. Returns the entryIDs of inherited entries the
// merge drops (superseded item, or a role pool the type replaces); their PerKillPct is 0.
func decorateMergedOdds(typeEntries, groupEntries []evedb.NPCLootEntry) map[int64]bool {
	all := make([]evedb.NPCLootEntry, 0, len(typeEntries)+len(groupEntries))
	all = append(all, typeEntries...)
	all = append(all, groupEntries...)
	merged := evedb.MergeLootScopes(all)
	odds := make(map[int64]float64, len(merged))
	sums := make(map[string]float64)
	for _, e := range merged {
		if e.DropChance > 0 {
			sums[e.RequireRole] += e.DropChance
		}
	}
	for _, e := range merged {
		sum := sums[e.RequireRole]
		if sum <= 0 || e.DropChance <= 0 {
			odds[e.EntryID] = 0
			continue
		}
		share := e.DropChance / sum
		pct := share * 100
		if e.RequireRole == "" {
			pct = share * evedb.LootExpectedDrops() * 100
		}
		odds[e.EntryID] = math.Min(pct, 100)
	}
	dropped := make(map[int64]bool)
	for _, list := range [][]evedb.NPCLootEntry{typeEntries, groupEntries} {
		for i := range list {
			pct, ok := odds[list[i].EntryID]
			if !ok {
				dropped[list[i].EntryID] = true
			}
			list[i].PerKillPct = pct
		}
	}
	return dropped
}

// lootBudgetOK reports whether every role pool of a scope sums to 100%. Tables authored as
// independent percentages do not, and the page offers a one-click rebalance instead of rewriting
// on a GET.
func lootBudgetOK(sums map[string]float64) bool {
	for _, s := range sums {
		if math.Abs(s-1) > 0.0005 {
			return false
		}
	}
	return true
}

// lootPoolPct renders the pool sums for the "off budget" banner ("142% / 300%
// commander").
func lootPoolPct(sums map[string]float64) string {
	var parts []string
	if s, ok := sums[""]; ok {
		parts = append(parts, fmt.Sprintf("%.0f%%", s*100))
	}
	for role, s := range sums {
		if role == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("%.0f%% %s", s*100, role))
	}
	return strings.Join(parts, " / ")
}

// handleLoot serves the loot overview: NPC groups (filterable), NPCs with
// type-specific loot, and a quick NPC search for jumping straight to a rat.
func (s *Server) handleLoot(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// POST: bulk actions on the overview (add an item to every group).
	if r.Method == http.MethodPost {
		s.handleLootOverviewPost(w, r, ctx)
		return
	}

	query := r.URL.Query().Get("q")
	onlyWithLoot := r.URL.Query().Get("withloot") == "1"

	groups, err := evedb.ESPListNPCLootGroups(ctx, s.db, query, onlyWithLoot)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "loot"})
		return
	}
	overrides, err := evedb.ESPListLootTypeOverrides(ctx, s.db)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "loot"})
		return
	}

	var totalEntries int64
	for _, g := range groups {
		totalEntries += g.EntryCount
	}
	// Counted across the whole game, not the filtered listing: the rebalance button is global.
	offPools, offScopes, err := evedb.CountOffBudgetLootPools(ctx, s.db)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "loot"})
		return
	}

	s.render(w, "loot.html", map[string]any{
		"Groups":       groups,
		"Overrides":    overrides,
		"Query":        query,
		"OnlyWithLoot": onlyWithLoot,
		"TotalEntries": totalEntries,
		"OffBudget":    offPools,
		"OffScopes":    offScopes,
		"Msg":          r.URL.Query().Get("msg"),
		"Err":          r.URL.Query().Get("err"),
		"Page":         "loot",
	})
}

// handleLootOverviewPost applies the overview's bulk actions against the tag
// selection: "addall" inserts one group-wide drop entry, "setcap" rewrites the
// max-drops cap. Both read the same comma-separated groupIDs field.
func (s *Server) handleLootOverviewPost(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	fail := func(msg string) {
		http.Redirect(w, r, "/loot?err="+url.QueryEscape(msg), http.StatusSeeOther)
	}

	// Tagged groups (comma-separated groupIDs from the tag checkboxes).
	var groupIDs []int64
	for _, part := range strings.Split(r.FormValue("groupIDs"), ",") {
		if gid, perr := strconv.ParseInt(strings.TrimSpace(part), 10, 64); perr == nil && gid > 0 {
			groupIDs = append(groupIDs, gid)
		}
	}

	switch r.FormValue("action") {
	case "addall":
	case "normalizeall":
		// Whole-game rebalance: every pool is rescaled to a 100% budget. Relative odds are unchanged.
		n, err := evedb.NormalizeAllLootShares(ctx, s.db)
		if err != nil {
			fail(err.Error())
			return
		}
		http.Redirect(w, r, "/loot?msg="+url.QueryEscape(fmt.Sprintf(
			"%d drop tables rebalanced to a 100%% budget — every item's share of its pool is unchanged", n)),
			http.StatusSeeOther)
		return
	case "setcap":
		// Caps are written to tagged groups only, with no "every group" fallback.
		if len(groupIDs) == 0 {
			fail("tag at least one NPC group before setting a cap")
			return
		}
		maxDrops, perr := strconv.ParseInt(strings.TrimSpace(r.FormValue("maxDrops")), 10, 64)
		if perr != nil || maxDrops < 0 {
			fail("max drops must be 0 or a positive whole number")
			return
		}
		n, err := evedb.SetNPCLootCapForGroups(ctx, s.db, groupIDs, maxDrops)
		if err != nil {
			fail(err.Error())
			return
		}
		msg := fmt.Sprintf("max drops set to %d on %d tagged groups", maxDrops, n)
		if maxDrops == 0 {
			msg = fmt.Sprintf("cap cleared (uncapped) on %d tagged groups", n)
		}
		http.Redirect(w, r, "/loot?msg="+url.QueryEscape(msg), http.StatusSeeOther)
		return
	default:
		fail("unknown action")
		return
	}

	itemTypeID, _ := strconv.ParseInt(r.FormValue("itemTypeID"), 10, 64)
	if itemTypeID == 0 {
		fail("pick an item first (use the search box)")
		return
	}
	minQty, maxQty := parseLootQty(r)
	requireRole := lootRequireRole(r)

	var n int64
	var err error
	scope := "NPC groups"
	if len(groupIDs) > 0 {
		n, err = evedb.AddNPCLootEntryToGroups(ctx, s.db, groupIDs, itemTypeID, minQty, maxQty, requireRole)
		scope = fmt.Sprintf("of %d tagged groups", len(groupIDs))
	} else {
		n, err = evedb.AddNPCLootEntryToAllGroups(ctx, s.db, itemTypeID, minQty, maxQty, requireRole)
	}
	if err != nil {
		fail(err.Error())
		return
	}
	roleNote := ""
	if requireRole != "" {
		roleNote = " (" + requireRole + "-only)"
	}
	msg := fmt.Sprintf("item added to %d %s%s — each table took an equal slice out of its existing budget", n, scope, roleNote)
	if n == 0 {
		msg = "no groups updated — the targeted groups already have this item as a group entry"
	}
	http.Redirect(w, r, "/loot?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

// handleLootGroup serves /loot/group/<groupID>: the group-wide drop list, the
// max-drops cap, and the group's member NPC types (with override badges).
func (s *Server) handleLootGroup(w http.ResponseWriter, r *http.Request) {
	groupID := lootPathID(r.URL.Path, "/loot/group/")
	if groupID == 0 {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	self := fmt.Sprintf("/loot/group/%d", groupID)
	if r.Method == http.MethodPost {
		s.handleLootPost(w, r, ctx, 0, groupID, self)
		return
	}

	groupName, err := evedb.ESPGetGroupName(ctx, s.db, groupID)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "loot"})
		return
	}
	if groupName == "" {
		s.render(w, "error.html", map[string]any{
			"Error": fmt.Sprintf("unknown NPC group %d", groupID), "Page": "loot"})
		return
	}
	entries, err := evedb.ESPListLootEntriesForGroup(ctx, s.db, groupID)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "loot"})
		return
	}
	types, err := evedb.ESPListNPCTypesInGroup(ctx, s.db, groupID)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "loot"})
		return
	}
	maxDrops, err := evedb.GetRawNPCLootCap(ctx, s.db, 0, groupID)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "loot"})
		return
	}
	sums := decorateLootShares(entries)

	s.render(w, "loot_group.html", map[string]any{
		"GroupID":       groupID,
		"GroupName":     groupName,
		"Entries":       entries,
		"Types":         types,
		"MaxDrops":      maxDrops,
		"BudgetOK":      lootBudgetOK(sums),
		"BudgetPct":     lootPoolPct(sums),
		"ExpectedDrops": evedb.LootExpectedDrops(),
		"Msg":           r.URL.Query().Get("msg"),
		"Err":           r.URL.Query().Get("err"),
		"Page":          "loot",
	})
}

// handleLootNPC serves /loot/npc/<typeID>: type-specific drops for one NPC,
// with the inherited group loot shown alongside.
func (s *Server) handleLootNPC(w http.ResponseWriter, r *http.Request) {
	typeID := lootPathID(r.URL.Path, "/loot/npc/")
	if typeID == 0 {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	self := fmt.Sprintf("/loot/npc/%d", typeID)
	if r.Method == http.MethodPost {
		s.handleLootPost(w, r, ctx, typeID, 0, self)
		return
	}

	info, err := evedb.ESPGetNPCTypeInfo(ctx, s.db, typeID)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "loot"})
		return
	}
	if info == nil {
		s.render(w, "error.html", map[string]any{
			"Error": fmt.Sprintf("unknown NPC type %d", typeID), "Page": "loot"})
		return
	}
	entries, err := evedb.ESPListLootEntriesForType(ctx, s.db, typeID)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "loot"})
		return
	}
	groupEntries, err := evedb.ESPListLootEntriesForGroup(ctx, s.db, info.GroupID)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "loot"})
		return
	}
	typeCap, err := evedb.GetRawNPCLootCap(ctx, s.db, typeID, 0)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "loot"})
		return
	}
	groupCap, err := evedb.GetRawNPCLootCap(ctx, s.db, 0, info.GroupID)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "loot"})
		return
	}

	// A type-specific entry supersedes the group entry for the same item (GetLootDropsForNPC drops
	// the group row), so the inherited listing flags it.
	overridden := make(map[int64]bool, len(entries))
	for _, e := range entries {
		overridden[e.ItemTypeID] = true
	}

	sums := decorateLootShares(entries)
	decorateLootShares(groupEntries)
	dropped := decorateMergedOdds(entries, groupEntries)

	s.render(w, "loot_npc.html", map[string]any{
		"Info":          info,
		"Entries":       entries,
		"GroupEntries":  groupEntries,
		"Overridden":    overridden,
		"Dropped":       dropped,
		"MaxDrops":      typeCap,
		"GroupCap":      groupCap,
		"BudgetOK":      lootBudgetOK(sums),
		"BudgetPct":     lootPoolPct(sums),
		"ExpectedDrops": evedb.LootExpectedDrops(),
		"Msg":           r.URL.Query().Get("msg"),
		"Err":           r.URL.Query().Get("err"),
		"Page":          "loot",
	})
}

// handleLootPost applies add/update/delete/cap actions for either scope
// (npcTypeID xor npcGroupID nonzero) and redirects back to the calling page.
func (s *Server) handleLootPost(w http.ResponseWriter, r *http.Request, ctx context.Context,
	npcTypeID, npcGroupID int64, redirect string) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	fail := func(msg string) {
		http.Redirect(w, r, redirect+"?err="+url.QueryEscape(msg), http.StatusSeeOther)
	}
	ok := func(msg string) {
		http.Redirect(w, r, redirect+"?msg="+url.QueryEscape(msg)+"#entries", http.StatusSeeOther)
	}

	switch r.FormValue("action") {
	case "add":
		itemTypeID, _ := strconv.ParseInt(r.FormValue("itemTypeID"), 10, 64)
		if itemTypeID == 0 {
			fail("pick an item first (use the search box)")
			return
		}
		minQty, maxQty := parseLootQty(r)
		// No chance field: the new entry takes an equal slice of the pool and the existing entries are
		// squeezed to pay for it.
		if err := evedb.AddLootEntryBalanced(ctx, s.db, npcTypeID, npcGroupID, itemTypeID, minQty, maxQty, lootRequireRole(r)); err != nil {
			fail(err.Error())
			return
		}
		ok("entry added — the table was rebalanced to 100%")

	case "update":
		entryID, _ := strconv.ParseInt(r.FormValue("entryID"), 10, 64)
		if entryID == 0 {
			fail("invalid entry ID")
			return
		}
		minQty, maxQty := parseLootQty(r)
		if err := evedb.UpdateNPCLootEntryQty(ctx, s.db, entryID, minQty, maxQty, lootRequireRole(r)); err != nil {
			fail(err.Error())
			return
		}
		// The commander-only tick moves the entry between role pools, so both budgets are re-struck.
		if err := evedb.NormalizeLootShares(ctx, s.db, npcTypeID, npcGroupID); err != nil {
			fail(err.Error())
			return
		}
		ok("entry saved")

	case "shares":
		// The slider form posts every row at once: share_<entryID> in percent.
		shares := make(map[int64]float64)
		for key, vals := range r.Form {
			if !strings.HasPrefix(key, "share_") || len(vals) == 0 {
				continue
			}
			entryID, perr := strconv.ParseInt(strings.TrimPrefix(key, "share_"), 10, 64)
			if perr != nil || entryID == 0 {
				continue
			}
			pct, perr := strconv.ParseFloat(strings.TrimSpace(vals[0]), 64)
			if perr != nil || pct < 0 {
				fail("drop shares must be percentages of 0 or more")
				return
			}
			shares[entryID] = pct / 100
		}
		if len(shares) == 0 {
			fail("no shares posted")
			return
		}
		if err := evedb.SetLootShares(ctx, s.db, npcTypeID, npcGroupID, shares); err != nil {
			fail(err.Error())
			return
		}
		ok(fmt.Sprintf("drop shares saved — %d entries, budget re-struck at 100%%", len(shares)))

	case "normalize":
		if err := evedb.NormalizeLootShares(ctx, s.db, npcTypeID, npcGroupID); err != nil {
			fail(err.Error())
			return
		}
		ok("table rebalanced to a 100% budget — the odds between items are unchanged")

	case "delete":
		entryID, _ := strconv.ParseInt(r.FormValue("entryID"), 10, 64)
		if entryID == 0 {
			fail("invalid entry ID")
			return
		}
		if err := evedb.DeleteNPCLootEntry(ctx, s.db, entryID); err != nil {
			fail(err.Error())
			return
		}
		// The freed share goes back to the survivors proportionally.
		if err := evedb.NormalizeLootShares(ctx, s.db, npcTypeID, npcGroupID); err != nil {
			fail(err.Error())
			return
		}
		ok("entry deleted — its share went back to the rest of the table")

	case "copyfrom":
		// Group scope only: an NPC type's list is authored against its group's inherited loot, so
		// cloning another group's list would duplicate entries.
		if npcGroupID == 0 {
			fail("copying a drop list is only available on a group page")
			return
		}
		srcGroupID, _ := strconv.ParseInt(r.FormValue("sourceGroupID"), 10, 64)
		if srcGroupID == 0 {
			fail("pick a group to copy from (use the search box)")
			return
		}
		replace := r.FormValue("replace") == "1"
		copied, skipped, err := evedb.CopyGroupLootEntries(ctx, s.db, srcGroupID, npcGroupID, replace)
		if err != nil {
			fail(err.Error())
			return
		}
		srcName, _ := evedb.ESPGetGroupName(ctx, s.db, srcGroupID)
		if srcName == "" {
			srcName = fmt.Sprintf("group %d", srcGroupID)
		}
		msg := fmt.Sprintf("copied %d entries from %s", copied, srcName)
		if replace {
			msg = fmt.Sprintf("replaced this list with %s — %d entries", srcName, copied)
		} else if skipped > 0 {
			msg = fmt.Sprintf("copied %d entries from %s (%d already here, left untouched)", copied, srcName, skipped)
		}
		if r.FormValue("copyCap") == "1" {
			cap, cerr := evedb.CopyGroupLootCap(ctx, s.db, srcGroupID, npcGroupID)
			if cerr != nil {
				fail(cerr.Error())
				return
			}
			if cap > 0 {
				msg += fmt.Sprintf("; max drops set to %d", cap)
			} else {
				msg += "; max drops cleared (source is uncapped)"
			}
		}
		ok(msg)

	case "cap":
		maxDrops, _ := strconv.ParseInt(r.FormValue("maxDrops"), 10, 64)
		if err := evedb.SetNPCLootCap(ctx, s.db, npcTypeID, npcGroupID, maxDrops); err != nil {
			fail(err.Error())
			return
		}
		if maxDrops <= 0 {
			ok("max drops cap cleared (uncapped)")
		} else {
			ok(fmt.Sprintf("max drops cap set to %d", maxDrops))
		}

	default:
		fail("unknown action")
	}
}

// lootRequireRole maps the loot form's role condition to a spawn-role string.
// The "Commander only" checkbox (commanderOnly=1) restricts a drop to NPCs
// that spawned as commanders; empty means the drop applies to any kill. A bounty row posts
// keepRole instead, so a Save never moves it out of its guaranteed pool.
func lootRequireRole(r *http.Request) string {
	if r.FormValue("keepRole") == evedb.LootRoleBounty {
		return evedb.LootRoleBounty
	}
	if r.FormValue("commanderOnly") == "1" {
		return "commander"
	}
	return ""
}

// parseLootQty reads the quantity window from the form. Shares are set only by the sliders.
func parseLootQty(r *http.Request) (minQty, maxQty int64) {
	minQty, _ = strconv.ParseInt(r.FormValue("minQty"), 10, 64)
	maxQty, _ = strconv.ParseInt(r.FormValue("maxQty"), 10, 64)
	if minQty < 1 {
		minQty = 1
	}
	if maxQty < minQty {
		maxQty = minQty
	}
	return minQty, maxQty
}

// handleLootItemsAPI serves GET /loot/items/api?q=, a JSON item search for the add-entry autocomplete.
func (s *Server) handleLootItemsAPI(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	w.Header().Set("Content-Type", "application/json")
	if len(q) < 2 {
		w.Write([]byte("[]"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	items, err := evedb.ESPSearchLootItems(ctx, s.db, q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type apiItem struct {
		TypeID    int64   `json:"typeID"`
		TypeName  string  `json:"typeName"`
		Group     string  `json:"group"`
		Category  string  `json:"category"`
		BasePrice float64 `json:"basePrice"`
		Published bool    `json:"published"`
	}
	out := make([]apiItem, len(items))
	for i, it := range items {
		out[i] = apiItem{it.TypeID, it.TypeName, it.GroupName, it.CategoryName, it.BasePrice, it.Published}
	}
	json.NewEncoder(w).Encode(out)
}

// handleLootGroupsAPI serves GET /loot/groups/api?q=, a JSON NPC-group search for the "copy loot
// from another group" picker. Groups with no entries are listed too (greyed in the UI).
func (s *Server) handleLootGroupsAPI(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	w.Header().Set("Content-Type", "application/json")
	if len(q) < 2 {
		w.Write([]byte("[]"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	groups, err := evedb.ESPListNPCLootGroups(ctx, s.db, q, false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type apiGroup struct {
		GroupID   int64  `json:"groupID"`
		GroupName string `json:"groupName"`
		Entries   int64  `json:"entries"`
		TypeCount int64  `json:"types"`
		MaxDrops  int64  `json:"maxDrops"`
	}
	if len(groups) > 50 {
		groups = groups[:50]
	}
	out := make([]apiGroup, len(groups))
	for i, g := range groups {
		out[i] = apiGroup{g.GroupID, g.GroupName, g.EntryCount, g.TypeCount, g.MaxDrops}
	}
	json.NewEncoder(w).Encode(out)
}

// lootPathID extracts the trailing numeric ID from a loot URL path.
func lootPathID(path, prefix string) int64 {
	rest := strings.TrimPrefix(path, prefix)
	rest = strings.TrimSuffix(rest, "/")
	id, _ := strconv.ParseInt(rest, 10, 64)
	return id
}
