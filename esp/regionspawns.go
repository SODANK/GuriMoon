package esp

// Region Spawns pages, the flat belt-spawn system. One list per region: each row is an NPC type
// allowed in that region's belts with its own max security, weight, hull class, role and
// owning faction/corp. There are no templates, pools or per-system spawners.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
	"gurimoon/npc"
)

// BeltWaveTimerChanged, when the game server sets it, hands a new belt wave
// timer (seconds; 0 = built-in default) to the live spawner, so an edit applies
// without a restart. nil when the panel runs without a game server.
var BeltWaveTimerChanged func(sec int)

// RegionSpawnsChanged, when the game server sets it, reloads the belt rosters of the given regions
// after a Region Spawns write, so a new or edited listing rolls without a restart.
var RegionSpawnsChanged func(regionIDs []int64)

func notifyRegionSpawns(regionIDs []int64) {
	if RegionSpawnsChanged != nil {
		RegionSpawnsChanged(regionIDs)
	}
}

func notifyBeltWaveTimer(sec int) {
	if BeltWaveTimerChanged != nil {
		BeltWaveTimerChanged(sec)
	}
}

// beltWaveTimerSec is the stored wave timer, or the built-in default with no
// global row.
func beltWaveTimerSec(row *evedb.SpawnerRow) int {
	if row != nil && row.FullClearDelaySec > 0 {
		return row.FullClearDelaySec
	}
	return npc.DefaultSeedCooldownSec
}

// handleRegionSpawns serves /region-spawns: every region with its listing
// count, plus the global belt-settings card.
func (s *Server) handleRegionSpawns(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var result string

	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err == nil {
			switch r.FormValue("action") {
			case "save_global":
				fullClearDelaySec, _ := strconv.Atoi(r.FormValue("fullClearDelaySec"))
				scatterRadius := parseFloat64(r.FormValue("scatterRadius"))
				warpIn := r.FormValue("warpIn") == "on" || r.FormValue("warpIn") == "1"
				if err := evedb.ESPSaveGlobalBeltSettings(ctx, s.db,
					fullClearDelaySec, scatterRadius, warpIn); err != nil {
					result = "Error: " + err.Error()
				} else {
					result = "Global belt settings saved."
					notifyBeltWaveTimer(fullClearDelaySec)
				}

			case "delete_global":
				if err := evedb.ESPDeleteGlobalBeltSettings(ctx, s.db); err != nil {
					result = "Error: " + err.Error()
				} else {
					result = "Global belt settings removed — back to security scaling."
					notifyBeltWaveTimer(0)
				}

			case "save_rarity":
				// Separate from save_global: those three fields live in the npcSpawners row (delete_global
				// resets only them), while these are server_settings.
				if err := evedb.SetBeltSpawnSettings(ctx, s.db, collectBeltSpawnSettings(r)); err != nil {
					result = "Error: " + err.Error()
				} else {
					result = "Rarity settings saved — restart the server to apply them " +
						"(they are read once at boot)."
				}

			case "clear_officer_ledger":
				if err := evedb.ClearOfficerLedger(ctx, s.db); err != nil {
					result = "Error: " + err.Error()
				} else {
					result = "Officer ledger cleared. The running server keeps its own in-memory " +
						"counts until restarted."
				}
			}
		}
	}

	regions, err := evedb.ESPGetRegionSpawnSummaries(ctx, s.db)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "region-spawns"})
		return
	}

	// Configured regions first, empty ones behind a fold.
	var configured, empty []evedb.ESPRegionSpawnSummary
	for _, reg := range regions {
		if reg.ListingCount > 0 {
			configured = append(configured, reg)
		} else {
			empty = append(empty, reg)
		}
	}

	globalRow, _ := evedb.GetDefaultSpawnerConfig(ctx, s.db, 9)
	rarity := evedb.GetBeltSpawnSettings(ctx, s.db)
	ledger, _ := evedb.ESPGetOfficerLedger(ctx, s.db)

	s.render(w, "region_spawns.html", map[string]any{
		"Configured":    configured,
		"Empty":         empty,
		"Global":        globalRow,
		"WaveTimerSec":  beltWaveTimerSec(globalRow),
		"Rarity":        rarity,
		"OfficerLedger": ledger,
		"CooldownHours": float64(rarity.OfficerKillCooldownSec) / 3600,
		"Result":        result,
		"Page":          "region-spawns",
	})
}

// collectBeltSpawnSettings reads the rarity form. Every value is clamped again
// in SetBeltSpawnSettings; this only has to turn checkboxes into bools.
func collectBeltSpawnSettings(r *http.Request) evedb.BeltSpawnSettings {
	on := func(name string) bool {
		v := r.FormValue(name)
		return v == "on" || v == "1"
	}
	num := func(name string, def int) int {
		if v, err := strconv.Atoi(strings.TrimSpace(r.FormValue(name))); err == nil {
			return v
		}
		return def
	}
	return evedb.BeltSpawnSettings{
		OfficersEnabled:          on("officersEnabled"),
		OfficerMaxAlivePerRegion: num("officerMaxAlivePerRegion", evedb.DefaultOfficerMaxAlivePerRegion),
		OfficerKillCooldownSec:   num("officerKillCooldownSec", evedb.DefaultOfficerKillCooldownSec),
		OfficerChancePct:         num("officerChancePct", evedb.DefaultSpawnChanceMultPct),
		CommandersEnabled:        on("commandersEnabled"),
		CommanderChancePct:       num("commanderChancePct", evedb.DefaultSpawnChanceMultPct),
		CommandersPerWave:        num("commandersPerWave", evedb.DefaultCommandersPerWave),
		WaveHardCap:              num("waveHardCap", evedb.DefaultWaveHardCap),
		TierHullBiasMult:         num("tierHullBiasMult", evedb.DefaultTierHullBiasMult),
	}
}

// The wave timer's bounds. Under a minute is below the 15 s belt sync's useful
// resolution and turns a belt into a rat fountain; a day is "never" in practice.
const (
	minBeltWaveTimerSec = 60
	maxBeltWaveTimerSec = 24 * 3600
)

// handleSpawnerRules serves /spawner-rules: how many ships a belt wave holds
// at each security band. What can fill those slots is the region's business.
func (s *Server) handleSpawnerRules(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var result string

	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err == nil {
			switch r.FormValue("action") {
			case "wave_timer":
				mins, err := strconv.ParseFloat(strings.TrimSpace(r.FormValue("waveTimerMin")), 64)
				sec := int(mins*60 + 0.5)
				if err != nil || sec < minBeltWaveTimerSec || sec > maxBeltWaveTimerSec {
					result = fmt.Sprintf("Wave timer must be between %g and %g minutes.",
						float64(minBeltWaveTimerSec)/60, float64(maxBeltWaveTimerSec)/60)
				} else if err := evedb.ESPSetBeltReseedCooldown(ctx, s.db, sec); err != nil {
					result = "Error: " + err.Error()
				} else {
					notifyBeltWaveTimer(sec)
					live := "saved — it applies at the next restart (no game server attached)"
					if BeltWaveTimerChanged != nil {
						live = "live now"
					}
					result = fmt.Sprintf("Wave timer set to %s, %s.", time.Duration(sec)*time.Second, live)
				}

			case "create":
				name := strings.TrimSpace(r.FormValue("ruleName"))
				secMin := parseFloat64(r.FormValue("secMin"))
				secMax := parseFloat64(r.FormValue("secMax"))
				minShips, _ := strconv.Atoi(r.FormValue("minShips"))
				maxShips, _ := strconv.Atoi(r.FormValue("maxShips"))
				desc := strings.TrimSpace(r.FormValue("description"))
				lo, hi := clampShips(minShips, maxShips)
				if name == "" {
					result = "Rule name is required."
				} else if _, err := evedb.ESPCreateSpawnerRule(ctx, s.db, name, secMin, secMax, lo, hi, desc); err != nil {
					result = "Error: " + err.Error()
				} else {
					result = fmt.Sprintf("Created rule %q", name)
				}

			case "update":
				ruleID, _ := strconv.ParseInt(r.FormValue("ruleID"), 10, 64)
				name := strings.TrimSpace(r.FormValue("ruleName"))
				secMin := parseFloat64(r.FormValue("secMin"))
				secMax := parseFloat64(r.FormValue("secMax"))
				minShips, _ := strconv.Atoi(r.FormValue("minShips"))
				maxShips, _ := strconv.Atoi(r.FormValue("maxShips"))
				if name == "" {
					result = "Rule name is required."
				} else {
					lo, hi := clampShips(minShips, maxShips)
					if err := evedb.ESPUpdateSpawnerRule(ctx, s.db, ruleID, name, secMin, secMax,
						lo, hi, strings.TrimSpace(r.FormValue("description"))); err != nil {
						result = "Error: " + err.Error()
					} else {
						result = fmt.Sprintf("Updated rule %d", ruleID)
					}
				}

			case "delete":
				ruleID, _ := strconv.ParseInt(r.FormValue("ruleID"), 10, 64)
				if err := evedb.ESPDeleteSpawnerRule(ctx, s.db, ruleID); err != nil {
					result = "Error: " + err.Error()
				} else {
					result = fmt.Sprintf("Deleted rule %d", ruleID)
				}

			case "tier_create":
				t := collectSquadTier(r)
				if t.TierName == "" {
					result = "Tier name is required."
				} else if _, err := evedb.ESPCreateSquadTier(ctx, s.db, t); err != nil {
					result = "Error: " + err.Error()
				} else {
					result = fmt.Sprintf("Created the %q tier for security %.1f to %.1f — "+
						"restart to apply it.", t.TierName, t.SecMin, t.SecMax)
				}

			case "tier_update":
				t := collectSquadTier(r)
				t.TierID, _ = strconv.ParseInt(r.FormValue("tierID"), 10, 64)
				if t.TierName == "" {
					result = "Tier name is required."
				} else if err := evedb.ESPUpdateSquadTier(ctx, s.db, t); err != nil {
					result = "Error: " + err.Error()
				} else {
					result = fmt.Sprintf("Updated tier %d — restart to apply it "+
						"(tiers are read once at boot).", t.TierID)
				}

			case "tier_delete":
				tierID, _ := strconv.ParseInt(r.FormValue("tierID"), 10, 64)
				if err := evedb.ESPDeleteSquadTier(ctx, s.db, tierID); err != nil {
					result = "Error: " + err.Error()
				} else {
					result = fmt.Sprintf("Deleted tier %d — restart to apply it.", tierID)
				}
			}
		}
	}

	rules, err := evedb.ESPGetAllSpawnerRules(ctx, s.db)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "spawner-rules"})
		return
	}
	tiers, _ := evedb.ESPGetAllSquadTiers(ctx, s.db)
	globalRow, _ := evedb.GetDefaultSpawnerConfig(ctx, s.db, 9)
	timerSec := beltWaveTimerSec(globalRow)

	s.render(w, "spawner_rules.html", map[string]any{
		"Rules":               rules,
		"Gaps":                spawnerRuleGaps(rules),
		"Tiers":               tiers,
		"TierGaps":            squadTierGaps(tiers),
		"WaveTimerSec":        timerSec,
		"WaveTimerMin":        float64(timerSec) / 60,
		"WaveTimerDefaultMin": float64(npc.DefaultSeedCooldownSec) / 60,
		"Result":              result,
		"Page":                "spawner-rules",
	})
}

// collectSquadTier reads a squad-tier form, clamped. secFloor stays inside the real security
// range (a floor above 1.0 empties every base pool, one below -1.0 does nothing); the two
// chances are percentages.
func collectSquadTier(r *http.Request) evedb.ESPSquadTierRow {
	clampF := func(v, lo, hi float64) float64 {
		if v < lo {
			return lo
		}
		if v > hi {
			return hi
		}
		return v
	}
	weight, _ := strconv.Atoi(r.FormValue("weight"))
	if weight < 0 {
		weight = 0
	}
	dMin, _ := strconv.Atoi(r.FormValue("sizeDeltaMin"))
	dMax, _ := strconv.Atoi(r.FormValue("sizeDeltaMax"))
	return evedb.ESPSquadTierRow{
		TierName:         strings.TrimSpace(r.FormValue("tierName")),
		SecMin:           parseFloat64(r.FormValue("secMin")),
		SecMax:           parseFloat64(r.FormValue("secMax")),
		Weight:           weight,
		SizeDeltaMin:     dMin,
		SizeDeltaMax:     dMax,
		SecFloor:         clampF(parseFloat64(r.FormValue("secFloor")), -1, 1),
		CmdrChancePct:    clampF(parseFloat64(r.FormValue("cmdrChancePct")), 0, 100),
		OfficerChancePct: clampF(parseFloat64(r.FormValue("officerChancePct")), 0, 100),
		HullBias:         strings.TrimSpace(r.FormValue("hullBias")),
		Description:      strings.TrimSpace(r.FormValue("description")),
	}
}

// squadTierGaps reports security levels no tier with positive weight covers. Belts there use a
// neutral built-in tier: they spawn, with no difficulty variance, commanders or officers.
func squadTierGaps(tiers []evedb.ESPSquadTierRow) []string {
	var gaps []string
	var runStart float64
	inRun := false
	for i := -10; i <= 10; i++ {
		sec := float64(i) / 10
		covered := false
		for _, t := range tiers {
			if t.Weight > 0 && sec >= t.SecMin-1e-9 && sec <= t.SecMax+1e-9 {
				covered = true
				break
			}
		}
		if !covered && !inRun {
			runStart, inRun = sec, true
		} else if covered && inRun {
			gaps = append(gaps, fmt.Sprintf("%.1f to %.1f", runStart, float64(i-1)/10))
			inRun = false
		}
	}
	if inRun {
		gaps = append(gaps, fmt.Sprintf("%.1f to 1.0", runStart))
	}
	return gaps
}

// collectRegionSpawnEdits pulls the edited values for the given listing IDs out
// of a submitted listings form. Every field is namespaced by spawnID
// ("row42_weight") rather than posted as parallel arrays, so a row that fails to
// submit one control can never shift another row's values onto it.
func collectRegionSpawnEdits(r *http.Request, ids []string) []evedb.ESPRegionSpawnUpdate {
	seen := make(map[int64]bool, len(ids))
	out := make([]evedb.ESPRegionSpawnUpdate, 0, len(ids))
	for _, raw := range ids {
		spawnID, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil || spawnID == 0 || seen[spawnID] {
			continue
		}
		seen[spawnID] = true
		p := fmt.Sprintf("row%d_", spawnID)
		// A row the page never rendered has no typeID field; skip it rather
		// than blanking a real listing to typeID 0.
		typeID, err := strconv.ParseInt(r.FormValue(p+"typeID"), 10, 64)
		if err != nil || typeID == 0 {
			continue
		}
		weight, _ := strconv.Atoi(r.FormValue(p + "weight"))
		if weight < 1 {
			weight = 1
		}
		factionID, _ := strconv.ParseInt(r.FormValue(p+"factionID"), 10, 64)
		corpID, _ := strconv.ParseInt(r.FormValue(p+"corporationID"), 10, 64)
		out = append(out, evedb.ESPRegionSpawnUpdate{
			SpawnID:       spawnID,
			TypeID:        typeID,
			MaxSec:        parseFloat64(r.FormValue(p + "maxSec")),
			Weight:        weight,
			ShipClass:     r.FormValue(p + "shipClass"),
			Role:          r.FormValue(p + "role"),
			FactionID:     factionID,
			CorporationID: corpID,
		})
	}
	return out
}

// clampShips keeps a rule's ship window sane: no negatives, max never below min.
func clampShips(minShips, maxShips int) (int, int) {
	if minShips < 0 {
		minShips = 0
	}
	if maxShips < minShips {
		maxShips = minShips
	}
	return minShips, maxShips
}

// spawnerRuleGaps reports security levels no rule covers. A gap means belts in
// those systems spawn nothing at all, which is easy to create by accident when
// editing bands, so the page calls it out.
func spawnerRuleGaps(rules []evedb.ESPSpawnerRuleRow) []string {
	var gaps []string
	// Walk the -1.0..1.0 range in 0.1 steps; report contiguous uncovered runs.
	var runStart float64
	inRun := false
	for i := -10; i <= 10; i++ {
		sec := float64(i) / 10
		covered := false
		for _, r := range rules {
			if sec >= r.SecMin-1e-9 && sec <= r.SecMax+1e-9 {
				covered = true
				break
			}
		}
		if !covered && !inRun {
			runStart, inRun = sec, true
		} else if covered && inRun {
			gaps = append(gaps, fmt.Sprintf("%.1f to %.1f", runStart, float64(i-1)/10))
			inRun = false
		}
	}
	if inRun {
		gaps = append(gaps, fmt.Sprintf("%.1f to 1.0", runStart))
	}
	return gaps
}

// handleRegionSpawnView serves /region-spawns/{regionID}: the region's
// listing editor with shotgun add and copy-to-regions.
func (s *Server) handleRegionSpawnView(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}
	regionID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || regionID == 0 {
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	regionName := evedb.ESPGetRegionName(ctx, s.db, regionID)
	if regionName == "" {
		http.NotFound(w, r)
		return
	}

	var result string

	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err == nil {
			// Every action below writes npcRegionSpawns for this region and, for add and copy_to,
			// the other regions it names.
			touched := []int64{regionID}
			for _, key := range []string{"extraRegionIDs", "targetRegionIDs"} {
				for _, v := range r.Form[key] {
					if id, err := strconv.ParseInt(v, 10, 64); err == nil && id != regionID {
						touched = append(touched, id)
					}
				}
			}
			defer func() {
				if !strings.HasPrefix(result, "Error") {
					notifyRegionSpawns(touched)
				}
			}()
			switch r.FormValue("action") {
			case "add":
				typeID, _ := strconv.ParseInt(r.FormValue("typeID"), 10, 64)
				maxSec := parseFloat64(r.FormValue("maxSec"))
				weight, _ := strconv.Atoi(r.FormValue("weight"))
				shipClass := r.FormValue("shipClass")
				role := r.FormValue("role")
				factionID, _ := strconv.ParseInt(r.FormValue("factionID"), 10, 64)
				corpID, _ := strconv.ParseInt(r.FormValue("corporationID"), 10, 64)
				// Shotgun: current region plus any extra tagged regions.
				targets := []int64{regionID}
				for _, v := range r.Form["extraRegionIDs"] {
					if id, err := strconv.ParseInt(v, 10, 64); err == nil && id != regionID {
						targets = append(targets, id)
					}
				}
				if typeID == 0 {
					result = "Pick an NPC type."
				} else {
					if weight < 1 {
						weight = 1
					}
					if shipClass == "" {
						shipClass = "frigate"
					}
					if role == "" {
						role = "normal"
					}
					added, err := evedb.ESPAddRegionSpawn(ctx, s.db, targets, typeID,
						maxSec, weight, shipClass, role, factionID, corpID)
					if err != nil {
						result = "Error: " + err.Error()
					} else {
						result = fmt.Sprintf("Added typeID %d to %d region(s).", typeID, added)
					}
				}

			case "save_all":
				// The listing table is one form: Del removes a row, a row's Save writes that row, Save all
				// writes every submitted row.
				if delID, _ := strconv.ParseInt(r.FormValue("deleteOne"), 10, 64); delID != 0 {
					if err := evedb.ESPDeleteRegionSpawn(ctx, s.db, delID); err != nil {
						result = "Error: " + err.Error()
					} else {
						result = fmt.Sprintf("Deleted listing %d.", delID)
					}
					break
				}
				ids := r.Form["spawnIDs"]
				single := r.FormValue("saveOne") != ""
				if single {
					ids = []string{r.FormValue("saveOne")}
				}
				updates := collectRegionSpawnEdits(r, ids)
				if len(updates) == 0 {
					result = "No changes to save."
				} else if err := evedb.ESPUpdateRegionSpawns(ctx, s.db, updates); err != nil {
					result = "Error: " + err.Error()
				} else if single {
					result = fmt.Sprintf("Saved listing %d.", updates[0].SpawnID)
				} else {
					result = fmt.Sprintf("Saved %d listing(s).", len(updates))
				}

			case "set_owner_all":
				factionID, _ := strconv.ParseInt(r.FormValue("factionID"), 10, 64)
				corpID, _ := strconv.ParseInt(r.FormValue("corporationID"), 10, 64)
				if n, err := evedb.ESPSetRegionSpawnOwner(ctx, s.db, regionID, factionID, corpID); err != nil {
					result = "Error: " + err.Error()
				} else {
					result = fmt.Sprintf("Owner set on %d listings.", n)
				}

			case "clear":
				if err := evedb.ESPClearRegionSpawns(ctx, s.db, regionID); err != nil {
					result = "Error: " + err.Error()
				} else {
					result = "All listings removed — no belt spawns in this region."
				}

			case "fix_officer_maxsec":
				if n, err := evedb.ESPFixRegionOfficerMaxSec(ctx, s.db, regionID); err != nil {
					result = "Error: " + err.Error()
				} else if n == 0 {
					result = "Nothing to fix — no officer listing here is authored above 0.0 security."
				} else {
					result = fmt.Sprintf("Repaired %d officer listing(s): null-sec only, "+
						"role officer, battleship class.", n)
				}

			case "copy_to":
				var targets []int64
				for _, v := range r.Form["targetRegionIDs"] {
					if id, err := strconv.ParseInt(v, 10, 64); err == nil {
						targets = append(targets, id)
					}
				}
				replace := r.FormValue("replace") == "on" || r.FormValue("replace") == "1"
				if len(targets) == 0 {
					result = "Pick at least one target region."
				} else if copied, err := evedb.ESPCopyRegionSpawns(ctx, s.db, regionID, targets, replace); err != nil {
					result = "Error: " + err.Error()
				} else {
					result = fmt.Sprintf("Copied %d listings to %d region(s).", copied, len(targets))
				}
			}
		}
	}

	rows, err := evedb.ESPGetRegionSpawnRows(ctx, s.db, regionID)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "region-spawns"})
		return
	}
	factions, _ := evedb.ESPGetSpawnFactionOptions(ctx, s.db)
	npcGroups, _ := evedb.ESPGetNPCGroups(ctx, s.db)
	regionOpts, _ := evedb.ESPGetAllRegionOptions(ctx, s.db)

	// The corp list is hundreds of rows, so it ships once as JSON and the page builds each row's
	// corp dropdown from the chosen faction.
	corps, _ := evedb.ESPGetNPCCorpsWithFaction(ctx, s.db)
	corpJSON, err := json.Marshal(corps)
	if err != nil {
		corpJSON = []byte("[]")
	}

	s.render(w, "region_spawn_view.html", map[string]any{
		"RegionID":     regionID,
		"RegionName":   regionName,
		"Rows":         rows,
		"Bands":        regionPoolBands(rows),
		"BadOfficers":  countRoleAboveSec(rows, "officer", 0.0),
		"BadCommander": countRoleAboveSec(rows, "commander", 0.8),
		"Factions":     factions,
		"CorpJSON":     string(corpJSON),
		"NPCGroups":    npcGroups,
		"RegionOpts":   regionOpts,
		"Result":       result,
		"Page":         "region-spawns",
	})
}

// countRoleAboveSec counts listings of a role eligible above a security. Both
// call sites are warnings about a listing reaching further up the security
// ladder than its role is meant to: officers are a null-sec event, and no
// shipped commander listing goes above 0.8.
func countRoleAboveSec(rows []evedb.ESPRegionSpawnRow, role string, sec float64) int {
	n := 0
	for _, r := range rows {
		if r.Role == role && r.MaxSec > sec+1e-9 {
			n++
		}
	}
	return n
}

// regionPoolBand is the size of each draw pool at one security band. Commanders and officers
// roll against their own pools, so the base-pool weight alone does not give their odds.
type regionPoolBand struct {
	Label      string
	Sec        float64
	Base       int
	BaseWeight int
	Cmdr       int
	CmdrWeight int
	Officer    int
	OffWeight  int
}

// regionPoolBands measures the three pools at one representative security per
// shipped spawner-rule band.
func regionPoolBands(rows []evedb.ESPRegionSpawnRow) []regionPoolBand {
	bands := []regionPoolBand{
		{Label: "1.0 – 0.9", Sec: 1.0},
		{Label: "0.9 – 0.7", Sec: 0.8},
		{Label: "0.7 – 0.5", Sec: 0.6},
		{Label: "0.5 – 0.1", Sec: 0.3},
		{Label: "0.1 – -1.0", Sec: -0.5},
	}
	for i := range bands {
		for _, r := range rows {
			if bands[i].Sec > r.MaxSec+1e-9 {
				continue
			}
			w := r.Weight
			if w < 1 {
				w = 1
			}
			switch r.Role {
			case "commander":
				bands[i].Cmdr++
				bands[i].CmdrWeight += w
			case "officer":
				bands[i].Officer++
				bands[i].OffWeight += w
			default:
				bands[i].Base++
				bands[i].BaseWeight += w
			}
		}
	}
	return bands
}
