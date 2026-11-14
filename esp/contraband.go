package esp

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

// CustomsPatrolFactions maps factionID → customs corp name for the factions that field customs
// officers. cmd/gurimoon/customs.go sets it at startup (esp cannot import main). Nil means
// unknown: the page says so instead of claiming a faction has no patrols.
var CustomsPatrolFactions map[int64]string

// handleContraband serves the contraband editor. Structured like the NPC supply/demand page:
// with no ?faction= it is a faction picker, and with one it shows that faction's ban list in two
// panels: rows customs officers seize and fine, and rows they open fire over.
func (s *Server) handleContraband(w http.ResponseWriter, r *http.Request) {
	var result, resultErr string

	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err == nil {
			result, resultErr = s.contrabandAction(r)
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	selectedFaction := parseInt64(r.URL.Query().Get("faction"))
	if selectedFaction == 0 {
		selectedFaction = parseInt64(r.FormValue("factionID"))
	}

	data := map[string]any{
		"Result":        result,
		"ResultErr":     resultErr,
		"Page":          "contraband",
		"CustomsMinSec": evedb.CustomsMinSec,
		"MaxSystemSec":  evedb.MaxSystemSec,
	}

	if selectedFaction == 0 {
		nameFilter := r.URL.Query().Get("q")
		factions, err := evedb.ListFactionContrabandStats(ctx, s.db, nameFilter)
		if err != nil {
			s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "contraband"})
			return
		}
		data["Factions"] = factions
		data["NameFilter"] = nameFilter
		data["Patrols"] = CustomsPatrolFactions

		// Drill-down: every faction that bans one type.
		if typeID := parseInt64(r.URL.Query().Get("type")); typeID != 0 {
			entries, err := evedb.ListContrabandByType(ctx, s.db, typeID)
			if err != nil {
				s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "contraband"})
				return
			}
			data["DrillType"] = typeID
			data["DrillEntries"] = entries
			if len(entries) > 0 {
				data["DrillName"] = entries[0].TypeName
			}
		}
		s.render(w, "contraband.html", data)
		return
	}

	factionName, err := evedb.GetChrFactionName(ctx, s.db, selectedFaction)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "contraband"})
		return
	}
	entries, err := evedb.ListContrabandByFaction(ctx, s.db, selectedFaction)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "contraband"})
		return
	}
	systems, highsec, err := evedb.GetFactionSpace(ctx, s.db, selectedFaction)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "contraband"})
		return
	}

	stats := evedb.FactionContrabandStats{
		FactionID: selectedFaction, FactionName: factionName,
		Systems: systems, HighsecSystems: highsec, Rows: int64(len(entries)),
	}
	for _, e := range entries {
		if e.Confiscates() {
			stats.ConfiscateRows++
		}
		if e.Fines() && e.BasePrice > 0 {
			stats.FineRows++
		}
		if e.Attacks() {
			stats.AttackRows++
		}
		if e.Inert() {
			stats.InertRows++
		}
		if !e.HasType {
			stats.MissingTypeRows++
		}
	}

	data["Faction"] = stats
	data["Entries"] = entries
	data["Panels"] = buildContrabandPanels(entries)
	data["Patrolled"] = CustomsPatrolFactions == nil || CustomsPatrolFactions[selectedFaction] != ""
	data["PatrolsKnown"] = CustomsPatrolFactions != nil
	data["PatrolCorp"] = CustomsPatrolFactions[selectedFaction]
	s.render(w, "contraband.html", data)
}

// contrabandAction applies one POST action and returns its result messages.
func (s *Server) contrabandAction(r *http.Request) (result, resultErr string) {
	factionID := parseInt64(r.FormValue("factionID"))

	switch r.FormValue("action") {
	case "save":
		typeID := parseInt64(r.FormValue("typeID"))
		standingLoss := parseContrabandFloat(r.FormValue("standingLoss"))
		confiscateMinSec := parseContrabandFloat(r.FormValue("confiscateMinSec"))
		fineByValue := parseContrabandFloat(r.FormValue("fineByValue"))
		attackMinSec := parseContrabandFloat(r.FormValue("attackMinSec"))
		if factionID == 0 || typeID == 0 {
			return "", "Faction and type are required."
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		err := evedb.UpsertContraband(ctx, s.db, factionID, typeID,
			standingLoss, confiscateMinSec, fineByValue, attackMinSec)
		cancel()
		if err != nil {
			return "", "Error: " + err.Error()
		}
		return fmt.Sprintf("Saved type %d: standing %.2f, seize at %.2f, fine ×%.2f, attack at %.2f.",
			typeID, standingLoss, confiscateMinSec, fineByValue, attackMinSec), ""

	case "toggleAttack":
		typeID := parseInt64(r.FormValue("typeID"))
		value := parseContrabandFloat(r.FormValue("value"))
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		err := evedb.SetContrabandAttack(ctx, s.db, factionID, typeID, value)
		cancel()
		if err != nil {
			return "", "Error: " + err.Error()
		}
		if value > 0 {
			return fmt.Sprintf("Type %d is now shoot-on-sight at security %.2f and above.", typeID, value), ""
		}
		return fmt.Sprintf("Type %d no longer draws fire — seizure and fine only.", typeID), ""

	case "delete":
		typeID := parseInt64(r.FormValue("typeID"))
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		err := evedb.DeleteContraband(ctx, s.db, factionID, typeID)
		cancel()
		if err != nil {
			return "", "Error: " + err.Error()
		}
		return fmt.Sprintf("Removed type %d from faction %d's ban list.", typeID, factionID), ""

	case "factionField":
		field := r.FormValue("field")
		value := parseContrabandFloat(r.FormValue("value"))
		if factionID == 0 {
			return "", "No faction given."
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		n, err := evedb.SetFactionContrabandField(ctx, s.db, factionID, field, value)
		cancel()
		if err != nil {
			return "", "Error: " + err.Error()
		}
		return fmt.Sprintf("Set %s = %g on %d rows.", contrabandFieldLabel(field), value, n), ""

	case "clampThresholds":
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		n, err := evedb.ClampFactionContrabandThresholds(ctx, s.db, factionID)
		cancel()
		if err != nil {
			return "", "Error: " + err.Error()
		}
		return fmt.Sprintf("Rewrote %d unreachable thresholds to 0 (off). Behaviour is unchanged — they never fired.", n), ""

	case "purgeMissing":
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		n, err := evedb.DeleteFactionMissingTypeContraband(ctx, s.db, factionID)
		cancel()
		if err != nil {
			return "", "Error: " + err.Error()
		}
		return fmt.Sprintf("Deleted %d rows whose typeID has no invTypes entry.", n), ""

	case "bulkAdd":
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		added, perr := s.bulkAddContraband(ctx, r.FormValue("bulk"))
		cancel()
		if perr != "" {
			return "", perr
		}
		return fmt.Sprintf("Added %d contraband entries.", added), ""
	}
	return "", ""
}

func contrabandFieldLabel(field string) string {
	switch field {
	case evedb.ContrabandFieldStanding:
		return "standingLoss"
	case evedb.ContrabandFieldConfiscate:
		return "confiscateMinSec"
	case evedb.ContrabandFieldFine:
		return "fineByValue"
	case evedb.ContrabandFieldAttack:
		return "attackMinSec"
	}
	return field
}

// contrabandGroup is one heading inside a panel. The client lists items as "Category / Group /
// Item", so rows are bucketed as in the supply/demand editor.
type contrabandGroup struct {
	CategoryName string
	GroupName    string
	Rows         []evedb.ContrabandEntry
}

// contrabandPanel is one enforcement tier. The two sides partition the faction's
// rows on whether a reachable attack threshold is set, so the row-level swap
// button moves a row from one panel to the other.
type contrabandPanel struct {
	Title     string
	Sub       string
	Class     string
	SwapTitle string
	Groups    []contrabandGroup
	RowCount  int64
	InertRows int64
	Missing   int64
}

// buildContrabandPanels splits one faction's rows into seize-and-fine and
// shoot-on-sight, bucketing each side by category/group in query order.
func buildContrabandPanels(entries []evedb.ContrabandEntry) []contrabandPanel {
	panels := []contrabandPanel{
		{Title: "Seize & Fine", Sub: "officers confiscate and/or fine", Class: "panel-seize",
			SwapTitle: "Make shoot-on-sight"},
		{Title: "Shoot on Sight", Sub: "officers open fire", Class: "panel-attack",
			SwapTitle: "Stop shooting — seize only"},
	}
	for _, e := range entries {
		p := &panels[0]
		if e.Attacks() {
			p = &panels[1]
		}
		p.RowCount++
		if e.Inert() {
			p.InertRows++
		}
		if !e.HasType {
			p.Missing++
		}
		cat, grp := e.CategoryName, e.GroupName
		if cat == "" {
			cat = "Unknown"
		}
		if grp == "" {
			grp = "Unknown"
		}
		if n := len(p.Groups); n > 0 && p.Groups[n-1].CategoryName == cat && p.Groups[n-1].GroupName == grp {
			p.Groups[n-1].Rows = append(p.Groups[n-1].Rows, e)
			continue
		}
		p.Groups = append(p.Groups, contrabandGroup{CategoryName: cat, GroupName: grp,
			Rows: []evedb.ContrabandEntry{e}})
	}
	return panels
}

// bulkAddContraband parses and inserts bulk contraband lines.
// Format per line: factionID typeID standingLoss confiscateMinSec fineByValue attackMinSec
func (s *Server) bulkAddContraband(ctx context.Context, text string) (int, string) {
	var count int
	for n, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 6 {
			return count, fmt.Sprintf("Line %d: expected 6 values (factionID typeID standingLoss confiscateMinSec fineByValue attackMinSec), got %d", n+1, len(fields))
		}
		factionID, err1 := strconv.ParseInt(fields[0], 10, 64)
		typeID, err2 := strconv.ParseInt(fields[1], 10, 64)
		standingLoss, err3 := strconv.ParseFloat(fields[2], 64)
		confiscateMinSec, err4 := strconv.ParseFloat(fields[3], 64)
		fineByValue, err5 := strconv.ParseFloat(fields[4], 64)
		attackMinSec, err6 := strconv.ParseFloat(fields[5], 64)
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil || err6 != nil {
			return count, fmt.Sprintf("Line %d: could not parse numbers in %q", n+1, line)
		}
		if err := evedb.UpsertContraband(ctx, s.db, factionID, typeID,
			standingLoss, confiscateMinSec, fineByValue, attackMinSec); err != nil {
			return count, fmt.Sprintf("Line %d: %v", n+1, err)
		}
		count++
	}
	return count, ""
}

func parseContrabandFloat(s string) float64 {
	v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v
}
