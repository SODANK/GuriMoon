package esp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

// Sovereignty authoring panel. Faction sov lives in the static map tables
// (mapRegions / mapConstellations / mapSolarSystems.factionID); player
// alliance sov lives in solarSystemSov with a mapSolarSystems.allianceID
// mirror (db/sovdb.go; same data /claim writes). Two scopes:
//   - region broadstroke: /sov            (owner applied to a whole region)
//   - per-system:         /sov/region/<regionID>
//
// Owner form values are "0" (no sov), "f:<factionID>" or "a:<allianceID>".

// parseSovOwner decodes an owner form value into (factionID, allianceID).
func parseSovOwner(v string) (factionID, allianceID int64, err error) {
	v = strings.TrimSpace(v)
	switch {
	case v == "" || v == "0":
		return 0, 0, nil
	case strings.HasPrefix(v, "f:"):
		factionID, err = strconv.ParseInt(v[2:], 10, 64)
	case strings.HasPrefix(v, "a:"):
		allianceID, err = strconv.ParseInt(v[2:], 10, 64)
	default:
		err = fmt.Errorf("bad owner value %q", v)
	}
	if err == nil && factionID <= 0 && allianceID <= 0 && v != "0" && v != "" {
		err = fmt.Errorf("bad owner value %q", v)
	}
	return factionID, allianceID, err
}

// sovOwnerLabel resolves an owner value to a display name for messages.
func sovOwnerLabel(ctx context.Context, s *Server, factionID, allianceID int64) string {
	switch {
	case factionID > 0:
		var name string
		s.db.QueryRowContext(ctx,
			`SELECT factionName FROM chrFactions WHERE factionID = ?`, factionID).Scan(&name)
		if name == "" {
			return fmt.Sprintf("faction %d", factionID)
		}
		return name
	case allianceID > 0:
		name, _ := evedb.GetAllianceName(ctx, s.db, allianceID)
		if name == "" {
			return fmt.Sprintf("alliance %d", allianceID)
		}
		return name
	default:
		return "no sovereignty"
	}
}

// handleSov serves the sov overview: all regions with sov summary, per-region
// broadstroke owner apply, and a system jump search.
func (s *Server) handleSov(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if r.Method == http.MethodPost {
		s.handleSovPost(w, r, ctx, "/sov")
		return
	}

	query := r.URL.Query().Get("q")
	regions, err := evedb.ESPListRegionsSov(ctx, s.db, query)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "sov"})
		return
	}
	factions, err := evedb.ESPListFactions(ctx, s.db)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "sov"})
		return
	}
	alliances, err := evedb.ESPListAlliances(ctx, s.db)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "sov"})
		return
	}

	var totalSystems, totalFactionSov, totalAllianceSov int64
	for _, reg := range regions {
		totalSystems += reg.SystemCount
		totalFactionSov += reg.SystemsWithSov
		totalAllianceSov += reg.SystemsWithAlliance
	}

	s.render(w, "sov.html", map[string]any{
		"Regions":          regions,
		"Factions":         factions,
		"Alliances":        alliances,
		"Query":            query,
		"TotalSystems":     totalSystems,
		"TotalFactionSov":  totalFactionSov,
		"TotalAllianceSov": totalAllianceSov,
		"Msg":              r.URL.Query().Get("msg"),
		"Err":              r.URL.Query().Get("err"),
		"Page":             "sov",
	})
}

// handleSovRegion serves /sov/region/<regionID>: every system in the region
// with its current owner, per-system owner apply, and a region-wide apply.
func (s *Server) handleSovRegion(w http.ResponseWriter, r *http.Request) {
	regionID := lootPathID(r.URL.Path, "/sov/region/")
	if regionID == 0 {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	self := fmt.Sprintf("/sov/region/%d", regionID)
	if r.Method == http.MethodPost {
		s.handleSovPost(w, r, ctx, self)
		return
	}

	regionName, regionFactionID, ok, err := evedb.ESPGetRegionSov(ctx, s.db, regionID)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "sov"})
		return
	}
	if !ok {
		s.render(w, "error.html", map[string]any{
			"Error": fmt.Sprintf("unknown region %d", regionID), "Page": "sov"})
		return
	}
	systems, err := evedb.ESPListSystemsSovInRegion(ctx, s.db, regionID)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "sov"})
		return
	}
	factions, err := evedb.ESPListFactions(ctx, s.db)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "sov"})
		return
	}
	alliances, err := evedb.ESPListAlliances(ctx, s.db)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "sov"})
		return
	}

	regionFactionName := ""
	if regionFactionID > 0 {
		regionFactionName = sovOwnerLabel(ctx, s, regionFactionID, 0)
	}

	s.render(w, "sov_region.html", map[string]any{
		"RegionID":          regionID,
		"RegionName":        regionName,
		"RegionFactionID":   regionFactionID,
		"RegionFactionName": regionFactionName,
		"Systems":           systems,
		"Factions":          factions,
		"Alliances":         alliances,
		"Msg":               r.URL.Query().Get("msg"),
		"Err":               r.URL.Query().Get("err"),
		"Page":              "sov",
	})
}

// handleSovPost applies a region-wide or single-system owner change and
// redirects back to the calling page.
func (s *Server) handleSovPost(w http.ResponseWriter, r *http.Request, ctx context.Context, redirect string) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	fail := func(msg string) {
		http.Redirect(w, r, redirect+"?err="+url.QueryEscape(msg), http.StatusSeeOther)
	}
	ok := func(msg string) {
		http.Redirect(w, r, redirect+"?msg="+url.QueryEscape(msg), http.StatusSeeOther)
	}

	factionID, allianceID, err := parseSovOwner(r.FormValue("owner"))
	if err != nil {
		fail(err.Error())
		return
	}
	label := sovOwnerLabel(ctx, s, factionID, allianceID)

	// parseIDList decodes a comma-separated ID list from the tag checkboxes.
	parseIDList := func(field string) []int64 {
		var ids []int64
		for _, part := range strings.Split(r.FormValue(field), ",") {
			if id, perr := strconv.ParseInt(strings.TrimSpace(part), 10, 64); perr == nil && id > 0 {
				ids = append(ids, id)
			}
		}
		return ids
	}

	switch r.FormValue("action") {
	case "bulkregion":
		// Tagged regions; empty = every region (loot-style broadstroke).
		regionIDs := parseIDList("regionIDs")
		nRegions, nSystems, err := evedb.SetRegionsSov(ctx, s.db, regionIDs, factionID, allianceID)
		if err != nil {
			fail(err.Error())
			return
		}
		scope := "tagged regions"
		if len(regionIDs) == 0 {
			scope = "regions (entire universe)"
		}
		if factionID == 0 && allianceID == 0 {
			ok(fmt.Sprintf("sovereignty cleared across %d %s, %d systems", nRegions, scope, nSystems))
		} else {
			ok(fmt.Sprintf("%s now holds sovereignty over %d %s, %d systems", label, nRegions, scope, nSystems))
		}

	case "bulksystem":
		// Tagged systems on a region page; tags are required here.
		systemIDs := parseIDList("systemIDs")
		if len(systemIDs) == 0 {
			fail("tag some systems first")
			return
		}
		n, err := evedb.SetSystemsSov(ctx, s.db, systemIDs, factionID, allianceID)
		if err != nil {
			fail(err.Error())
			return
		}
		if factionID == 0 && allianceID == 0 {
			ok(fmt.Sprintf("sovereignty cleared in %d tagged systems", n))
		} else {
			ok(fmt.Sprintf("%s now holds sovereignty in %d tagged systems", label, n))
		}

	case "region":
		regionID, _ := strconv.ParseInt(r.FormValue("regionID"), 10, 64)
		if regionID == 0 {
			fail("invalid region ID")
			return
		}
		n, err := evedb.SetRegionSov(ctx, s.db, regionID, factionID, allianceID)
		if err != nil {
			fail(err.Error())
			return
		}
		if factionID == 0 && allianceID == 0 {
			ok(fmt.Sprintf("sovereignty cleared across %d systems", n))
		} else {
			ok(fmt.Sprintf("%s now holds sovereignty over %d systems", label, n))
		}

	case "system":
		systemID, _ := strconv.ParseInt(r.FormValue("systemID"), 10, 64)
		if systemID == 0 {
			fail("invalid system ID")
			return
		}
		if err := evedb.SetSystemSov(ctx, s.db, systemID, factionID, allianceID); err != nil {
			fail(err.Error())
			return
		}
		sysName, _ := evedb.GetSystemName(ctx, s.db, systemID)
		if sysName == "" {
			sysName = fmt.Sprintf("system %d", systemID)
		}
		if factionID == 0 && allianceID == 0 {
			ok(fmt.Sprintf("sovereignty cleared in %s", sysName))
		} else {
			ok(fmt.Sprintf("%s now holds sovereignty in %s", label, sysName))
		}

	default:
		fail("unknown action")
	}
}

// handleSovSystemsAPI serves GET /sov/systems/api?q=, a JSON system search for the jump box on
// the sov overview.
func (s *Server) handleSovSystemsAPI(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	w.Header().Set("Content-Type", "application/json")
	if len(q) < 2 {
		w.Write([]byte("[]"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	hits, err := evedb.ESPSearchSovSystems(ctx, s.db, q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if hits == nil {
		hits = []evedb.SovSystemHit{}
	}
	json.NewEncoder(w).Encode(hits)
}
