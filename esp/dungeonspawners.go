package esp

// Dungeon → Dungeon Spawners: rules that keep open dungeon sites alive across
// the map (db/dunspawnerdb.go), and the live sites they placed
// (cmd/gurimoon/opensites.go).

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

// OpenSiteLive is what the running server knows about a site beyond its row.
type OpenSiteLive struct {
	Dead, Required int   // NPCs dead / NPCs the clear rule currently requires
	Total          int   // every NPC of the site, waves not yet released included
	LiveNPCs       int   // NPCs of the site in space right now (0 until someone warps in)
	DespawnAt      int64 // unix seconds; 0 = not cleared
	Loaded         bool  // the site's system is loaded
}

// Set by the game server; nil when the panel runs without one.
var (
	OpenSiteDespawner      func(siteID int64) error
	DungeonSpawnerSpawnNow func(spawnerID int64) (string, error)
	OpenSiteLiveState      func() map[int64]OpenSiteLive
)

// dungeonSpawnerView is one rule as a table row. Every editable field is
// repeated on the row's data attributes: the page has one edit dialog, filled
// from the row that opened it.
type dungeonSpawnerView struct {
	evedb.DungeonSpawner
	Systems  int    // systems matching the filters
	NPCs     int    // every NPC of the dungeon, waves included
	BaseNPCs int    // NPCs that spawn with their rooms, waves aside
	Live     int    // live sites
	FillPct  int    // Live against TargetActive, 0 to 100
	Under    bool   // enabled and below its target
	CheckErr string // why the system count could not be computed
	Warn     string // everything wrong with the rule, "" when sound

	RegionFirst string // first picked region, "" = no regions picked
	RegionMore  int    // picked regions beyond the first
	RegionNames string // every picked region, comma separated
	RegionCSV   string // picked region IDs, comma separated
}

// setRegions fills the region columns from the rule's ID list.
func (v *dungeonSpawnerView) setRegions(names map[int64]string) {
	ids := make([]string, 0, len(v.RegionIDs))
	picked := make([]string, 0, len(v.RegionIDs))
	for _, id := range v.RegionIDs {
		ids = append(ids, strconv.FormatInt(id, 10))
		name := names[id]
		if name == "" {
			name = "#" + strconv.FormatInt(id, 10)
		}
		picked = append(picked, name)
	}
	v.RegionCSV = strings.Join(ids, ",")
	v.RegionNames = strings.Join(picked, ", ")
	if len(picked) > 0 {
		v.RegionFirst, v.RegionMore = picked[0], len(picked)-1
	}
}

// setHealth derives the fill figures and the warning from the counts.
func (v *dungeonSpawnerView) setHealth() {
	if v.TargetActive > 0 {
		v.FillPct = v.Live * 100 / v.TargetActive
		if v.FillPct > 100 {
			v.FillPct = 100
		}
	}
	v.Under = v.Enabled && v.Live < v.TargetActive

	var warns []string
	if v.DungeonName == "" {
		warns = append(warns, fmt.Sprintf("dungeon %d no longer exists", v.DungeonVID))
	}
	if v.BaseNPCs == 0 {
		warns = append(warns, "no NPCs spawn with this dungeon's rooms, so its sites can never clear")
	}
	switch {
	case v.CheckErr != "":
		warns = append(warns, v.CheckErr)
	case v.Systems == 0:
		warns = append(warns, "no system matches its filters")
	case v.Systems*v.MaxPerSystem < v.TargetActive:
		warns = append(warns, fmt.Sprintf("target %d is above the %d sites its %d matching systems can hold",
			v.TargetActive, v.Systems*v.MaxPerSystem, v.Systems))
	}
	v.Warn = strings.Join(warns, "; ")
}

// regionPick feeds the edit dialog's region picker: every region, unticked.
// The page script ticks the ones of the spawner being edited.
type regionPick struct {
	Options []regionPickOption
}

type regionPickOption struct {
	RegionID int64
	Name     string
}

func newRegionPick(all []evedb.ESPRegionOption) regionPick {
	p := regionPick{Options: make([]regionPickOption, 0, len(all))}
	for _, r := range all {
		p.Options = append(p.Options, regionPickOption{RegionID: r.RegionID, Name: r.Name})
	}
	return p
}

// Live site states, as the page script filters and sorts them.
const (
	siteStateWaiting = "waiting" // nobody is fighting it
	siteStateEngaged = "engaged" // its NPCs are in space
	siteStateCleared = "cleared" // cleared, counting down to its despawn
)

// dungeonSiteRow is one live site as the page script receives it. The live
// sites table is rendered, filtered, sorted and paged in the browser, so the
// page stays light with thousands of sites.
type dungeonSiteRow struct {
	ID         int64   `json:"id"`
	SpawnerID  int64   `json:"spid"`
	Spawner    string  `json:"sp"` // "" when the rule is gone
	Dungeon    string  `json:"dun"`
	System     string  `json:"sys"`
	Region     string  `json:"reg"`
	Sec        float64 `json:"sec"`
	Location   string  `json:"loc"` // planet name, "" = deadspace
	Beacon     bool    `json:"vis"` // false = probe-only cosmic signature
	CreatedAt  int64   `json:"at"`
	Age        string  `json:"age"`
	Known      bool    `json:"known"` // the running server reported on it
	Dead       int     `json:"dead"`
	Total      int     `json:"total"`
	Unreleased int     `json:"waves"`   // NPCs in waves no trigger has released
	InSpace    int     `json:"inSpace"` // NPCs in space right now
	Loaded     bool    `json:"loaded"`
	State      string  `json:"state"`
	Clearing   string  `json:"left"` // "m:ss" before despawn, "" when not counting
}

func newDungeonSiteRow(site evedb.ESPOpenSiteRow, live OpenSiteLive, known bool, now int64) dungeonSiteRow {
	row := dungeonSiteRow{
		ID: site.SiteID, SpawnerID: site.SpawnerID, Spawner: site.SpawnerName,
		Dungeon: site.DungeonName, System: site.SystemName, Region: site.RegionName,
		Sec: site.Security, Beacon: site.BeaconVisible, CreatedAt: site.CreatedAt,
		Age:   shortAge(time.Duration(now-site.CreatedAt) * time.Second),
		Known: known, State: siteStateWaiting,
	}
	if site.PlanetID != 0 {
		row.Location = site.PlanetName
		if row.Location == "" {
			row.Location = fmt.Sprintf("#%d", site.PlanetID)
		}
	}
	if known {
		row.Dead, row.Total, row.InSpace, row.Loaded = live.Dead, live.Total, live.LiveNPCs, live.Loaded
		if live.Required < live.Total {
			row.Unreleased = live.Total - live.Required
		}
	}
	switch {
	case known && live.DespawnAt > 0:
		left := live.DespawnAt - now
		if left < 0 {
			left = 0
		}
		row.State, row.Clearing = siteStateCleared, fmt.Sprintf("%d:%02d", left/60, left%60)
	case site.ClearedAt != 0:
		row.State = siteStateCleared
	case row.InSpace > 0:
		row.State = siteStateEngaged
	}
	return row
}

// dungeonSpawnerSummary is the figure strip above the tables.
type dungeonSpawnerSummary struct {
	Spawners, Enabled int
	Under             int // enabled rules below their target
	Issues            int // rules with a warning
	Sites, Target     int // live sites against the enabled rules' targets
	FillPct           int
	Engaged, Cleared  int
	Signatures        int // probe-only sites
}

func summarizeDungeonSpawners(views []dungeonSpawnerView, sites []dungeonSiteRow) dungeonSpawnerSummary {
	sum := dungeonSpawnerSummary{Spawners: len(views), Sites: len(sites)}
	for _, v := range views {
		if v.Enabled {
			sum.Enabled++
			sum.Target += v.TargetActive
		}
		if v.Under {
			sum.Under++
		}
		if v.Warn != "" {
			sum.Issues++
		}
	}
	for _, s := range sites {
		switch s.State {
		case siteStateEngaged:
			sum.Engaged++
		case siteStateCleared:
			sum.Cleared++
		}
		if !s.Beacon {
			sum.Signatures++
		}
	}
	if sum.Target > 0 {
		sum.FillPct = sum.Sites * 100 / sum.Target
		if sum.FillPct > 100 {
			sum.FillPct = 100
		}
	}
	return sum
}

func collectDungeonSpawner(r *http.Request) evedb.DungeonSpawner {
	sp := evedb.DungeonSpawner{
		Name:      strings.TrimSpace(r.FormValue("name")),
		SecMin:    parseFloat64(r.FormValue("secMin")),
		SecMax:    parseFloat64(r.FormValue("secMax")),
		Enabled:   r.FormValue("enabled") == "1",
		Placement: r.FormValue("placement"),
		// An unticked checkbox is absent from the form.
		BeaconVisible: r.FormValue("beaconVisible") == "1",
	}
	sp.SpawnerID, _ = strconv.ParseInt(r.FormValue("spawnerID"), 10, 64)
	sp.DungeonVID, _ = strconv.ParseInt(r.FormValue("dungeonVID"), 10, 64)
	sp.MaxPerSystem, _ = strconv.Atoi(r.FormValue("maxPerSystem"))
	sp.TargetActive, _ = strconv.Atoi(r.FormValue("targetActive"))
	sp.RespawnDelayMinutes, _ = strconv.Atoi(r.FormValue("respawnDelayMinutes"))
	for _, v := range r.Form["regionIDs"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil && id > 0 {
			sp.RegionIDs = append(sp.RegionIDs, id)
		}
	}
	return sp
}

func (s *Server) handleDungeonSpawners(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	var result string
	isErr := false
	fail := func(err error) { result, isErr = "Error: "+err.Error(), true }

	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err == nil {
			switch r.FormValue("action") {
			case "save":
				sp := collectDungeonSpawner(r)
				if id, err := evedb.SaveDungeonSpawner(ctx, s.db, sp); err != nil {
					fail(err)
				} else if sp.SpawnerID == 0 {
					result = fmt.Sprintf("Created spawner %d %q — it places its first sites within a minute.", id, sp.Name)
				} else {
					result = fmt.Sprintf("Saved spawner %d %q — the next pass (each minute) uses the new filters; live sites stay where they are.", id, sp.Name)
				}

			case "toggle":
				id, _ := strconv.ParseInt(r.FormValue("spawnerID"), 10, 64)
				on := r.FormValue("enabled") == "1"
				if err := evedb.SetDungeonSpawnerEnabled(ctx, s.db, id, on); err != nil {
					fail(err)
				} else if on {
					result = fmt.Sprintf("Spawner %d enabled.", id)
				} else {
					result = fmt.Sprintf("Spawner %d disabled — no new sites; its live sites stay until cleared.", id)
				}

			case "delete":
				id, _ := strconv.ParseInt(r.FormValue("spawnerID"), 10, 64)
				removed := 0
				if OpenSiteDespawner != nil {
					sites, _ := evedb.ESPListOpenSites(ctx, s.db)
					for _, site := range sites {
						if site.SpawnerID == id && OpenSiteDespawner(site.SiteID) == nil {
							removed++
						}
					}
				}
				if err := evedb.DeleteDungeonSpawner(ctx, s.db, id); err != nil {
					fail(err)
				} else {
					result = fmt.Sprintf("Deleted spawner %d and despawned %d live site(s).", id, removed)
				}

			case "spawnNow":
				id, _ := strconv.ParseInt(r.FormValue("spawnerID"), 10, 64)
				if DungeonSpawnerSpawnNow == nil {
					result, isErr = "No game server attached — sites are placed by the running server.", true
				} else if msg, err := DungeonSpawnerSpawnNow(id); err != nil {
					fail(err)
				} else {
					result = "Spawned: " + msg
				}

			case "despawnSite":
				id, _ := strconv.ParseInt(r.FormValue("siteID"), 10, 64)
				if OpenSiteDespawner == nil {
					result, isErr = "No game server attached — a live site can only be despawned by the running server.", true
				} else if err := OpenSiteDespawner(id); err != nil {
					fail(err)
				} else {
					result = fmt.Sprintf("Site %d despawned.", id)
				}

			case "clearMinutes":
				m, _ := strconv.Atoi(r.FormValue("clearMinutes"))
				if err := evedb.SetOpenSiteClearMinutes(ctx, s.db, m); err != nil {
					fail(err)
				} else {
					result = "Despawn delay saved — it applies to sites cleared from now on."
				}
			}
		}
	}

	spawners, err := evedb.ListDungeonSpawners(ctx, s.db)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "dungeon-spawners"})
		return
	}
	dungeons, _ := evedb.ListDungeonOptions(ctx, s.db)
	regions, _ := evedb.ESPGetAllRegionOptions(ctx, s.db)
	sites, err := evedb.ESPListOpenSites(ctx, s.db)
	if err != nil && result == "" {
		result, isErr = "Error listing sites: "+err.Error(), true
	}

	regionNames := make(map[int64]string, len(regions))
	for _, reg := range regions {
		regionNames[reg.RegionID] = reg.Name
	}
	liveCount := map[int64]int{}
	for _, site := range sites {
		liveCount[site.SpawnerID]++
	}

	// Many rules share a dungeon or a filter set, so each distinct system
	// count and rat census is read once per page, not once per rule.
	type systemCount struct {
		n   int
		err string
	}
	type ratCount struct{ all, base int }
	systemCounts := map[string]systemCount{}
	ratCounts := map[int64]ratCount{}

	views := make([]dungeonSpawnerView, 0, len(spawners))
	for _, sp := range spawners {
		v := dungeonSpawnerView{DungeonSpawner: sp, Live: liveCount[sp.SpawnerID]}
		v.setRegions(regionNames)

		key := fmt.Sprintf("%s|%g|%g", v.RegionCSV, sp.SecMin, sp.SecMax)
		sc, ok := systemCounts[key]
		if !ok {
			if systems, err := evedb.SpawnerCandidateSystems(ctx, s.db, sp.RegionIDs, sp.SecMin, sp.SecMax); err != nil {
				sc.err = err.Error()
			} else {
				sc.n = len(systems)
			}
			systemCounts[key] = sc
		}
		v.Systems, v.CheckErr = sc.n, sc.err

		rc, ok := ratCounts[sp.DungeonVID]
		if !ok {
			if census, err := evedb.LoadDungeonRatCensus(ctx, s.db, sp.DungeonVID); err == nil {
				rc = ratCount{all: census.Count(), base: census.BaseCount()}
			}
			ratCounts[sp.DungeonVID] = rc
		}
		v.NPCs, v.BaseNPCs = rc.all, rc.base

		v.setHealth()
		views = append(views, v)
	}

	var live map[int64]OpenSiteLive
	if OpenSiteLiveState != nil {
		live = OpenSiteLiveState()
	}
	now := time.Now().Unix()
	siteRows := make([]dungeonSiteRow, 0, len(sites))
	for _, site := range sites {
		state, known := live[site.SiteID]
		siteRows = append(siteRows, newDungeonSiteRow(site, state, known, now))
	}

	s.render(w, "dungeon_spawners.html", map[string]any{
		"Spawners":     views,
		"SiteRows":     siteRows,
		"Summary":      summarizeDungeonSpawners(views, siteRows),
		"Dungeons":     dungeons,
		"NewPick":      newRegionPick(regions),
		"ClearMinutes": evedb.OpenSiteClearMinutes(ctx, s.db),
		"Attached":     OpenSiteLiveState != nil,
		"Result":       result,
		"ResultErr":    isErr,
		"Page":         "dungeon-spawners",
	})
}

// shortAge renders a site's age as "3d 4h", "5h 12m" or "7m".
func shortAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	default:
		return fmt.Sprintf("%dm", mins)
	}
}
