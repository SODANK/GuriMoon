package esp

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	evedb "gurimoon/db"
)

// Starbases panel: every control tower on the shard, what it is anchored to, and what it is
// producing.
//
// The moon pool is shown per tower because it is the same GetMoonForTower answer the management
// window's Moon tab renders: a tower the client reports as "(not attached to moon)" is diagnosed
// by the two disagreeing.

// StarbaseTemplateSpawner stamps a POS template at a moon for a
// corporation. Installed by the server (cmd/gurimoon/postemplate.go), which
// owns the live grids; nil hides the spawn form.
var StarbaseTemplateSpawner func(templateID, moonID, corpID int64, online bool, fuelDays float64) (string, error)

type starbaseStructureRow struct {
	evedb.StarbaseRecord
	TypeName      string
	ProvisionName string
	FeedsInto     int64 // destination structure for its material, 0 = unwired
	FeedsName     string
}

type starbaseRow struct {
	evedb.StarbaseOverview
	MoonPool   []moonPoolRow
	Structures []starbaseStructureRow
}

type moonPoolRow struct {
	TypeID    int64
	TypeName  string
	Abundance int64
}

func (s *Server) handleStarbases(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	var msg, errMsg string
	if r.Method == http.MethodPost {
		msg, errMsg = s.handleStarbasePost(ctx, r)
	}

	towers, err := evedb.ListStarbaseOverview(ctx, s.db)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "starbases"})
		return
	}

	// Type names are resolved once per distinct type, not per row.
	names := map[int64]string{}
	typeName := func(id int64) string {
		if id == 0 {
			return ""
		}
		if n, ok := names[id]; ok {
			return n
		}
		n, err := evedb.GetTypeName(ctx, s.db, id)
		if err != nil {
			n = fmt.Sprintf("type %d", id)
		}
		names[id] = n
		return n
	}

	online := 0
	structureCount := 0
	rows := make([]starbaseRow, 0, len(towers))
	for _, t := range towers {
		if t.State == evedb.StarbaseStateOnline || t.State == evedb.StarbaseStateReinforced {
			online++
		}
		row := starbaseRow{StarbaseOverview: t}

		if t.MoonID != 0 {
			// The generating read: a moon that has never been asked about
			// gets its pool here, exactly as GetMoonForTower would.
			if pool, pErr := evedb.GetMoonResources(ctx, s.db, t.MoonID); pErr == nil {
				for _, p := range pool {
					row.MoonPool = append(row.MoonPool, moonPoolRow{
						TypeID: p.TypeID, TypeName: typeName(p.TypeID), Abundance: p.Quantity,
					})
				}
			}
		}

		structures, sErr := evedb.ListStarbaseStructuresForTower(ctx, s.db, t.EntityID)
		if sErr == nil && len(structures) > 0 {
			links, _ := evedb.ListStructureLinks(ctx, s.db, t.EntityID)
			for _, st := range structures {
				structureCount++
				sr := starbaseStructureRow{
					StarbaseRecord: st,
					TypeName:       typeName(st.TypeID),
					ProvisionName:  typeName(st.ProvisionTypeID),
				}
				for _, l := range links {
					if l.SourceID == st.EntityID && l.TypeID == st.ProvisionTypeID {
						sr.FeedsInto = l.DestID
						break
					}
				}
				row.Structures = append(row.Structures, sr)
			}
		}
		for i := range row.Structures {
			if row.Structures[i].FeedsInto == 0 {
				continue
			}
			for _, other := range row.Structures {
				if other.EntityID == row.Structures[i].FeedsInto {
					row.Structures[i].FeedsName = other.TypeName
					break
				}
			}
		}
		rows = append(rows, row)
	}

	secOverride := ""
	if v, ok := evedb.StarbaseSecurityLevelMaxOverride(ctx, s.db); ok {
		secOverride = strconv.FormatFloat(v, 'f', -1, 64)
	}

	templates, tErr := evedb.ListStarbaseTemplates(ctx, s.db)
	if tErr != nil && errMsg == "" {
		errMsg = "templates: " + tErr.Error()
	}

	s.render(w, "starbases.html", map[string]any{
		"Templates":      templates,
		"CanSpawn":       StarbaseTemplateSpawner != nil,
		"Page":           "starbases",
		"Msg":            msg,
		"Err":            errMsg,
		"Towers":         rows,
		"OnlineCount":    online,
		"StructureCount": structureCount,
		"SecOverride":    secOverride,
		"DefaultMaxSec":  evedb.DefaultMaxAnchorSecurity,
	})
}

func (s *Server) handleStarbasePost(ctx context.Context, r *http.Request) (msg, errMsg string) {
	if err := r.ParseForm(); err != nil {
		return "", err.Error()
	}
	switch r.FormValue("action") {
	case "secoverride":
		raw := r.FormValue("maxsec")
		if raw == "" {
			// Empty clears it: the shipped attributes take over again.
			if err := evedb.SetStarbaseSecurityLevelMaxOverride(ctx, s.db, -1); err != nil {
				return "", err.Error()
			}
			return "Security override cleared — anchoring uses the shipped attributes again.", ""
		}
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || v < 0 {
			return "", "Security ceiling must be a number, 0 or above (leave empty to clear)."
		}
		if err := evedb.SetStarbaseSecurityLevelMaxOverride(ctx, s.db, v); err != nil {
			return "", err.Error()
		}
		return fmt.Sprintf("Anchoring security ceiling set to %.2f for towers and structures alike.", v), ""

	case "tplsave":
		towerID, _ := strconv.ParseInt(r.FormValue("towerID"), 10, 64)
		fuel, _ := strconv.ParseFloat(r.FormValue("fuelDays"), 64)
		tpl, err := evedb.CaptureStarbaseTemplate(ctx, s.db, towerID, r.FormValue("name"), fuel, 0)
		if err != nil {
			return "", err.Error()
		}
		return fmt.Sprintf("Template %q saved from tower %d: %d structure(s), %d link(s).",
			tpl.Name, towerID, len(tpl.Structures), len(tpl.Links)), ""

	case "tpldelete":
		id, _ := strconv.ParseInt(r.FormValue("templateID"), 10, 64)
		if err := evedb.DeleteStarbaseTemplate(ctx, s.db, id); err != nil {
			return "", err.Error()
		}
		return fmt.Sprintf("Template %d deleted.", id), ""

	case "tplspawn":
		if StarbaseTemplateSpawner == nil {
			return "", "Spawning is not available."
		}
		id, _ := strconv.ParseInt(r.FormValue("templateID"), 10, 64)
		moonID, _ := strconv.ParseInt(r.FormValue("moonID"), 10, 64)
		corpID, _ := strconv.ParseInt(r.FormValue("corpID"), 10, 64)
		if moonID == 0 || corpID == 0 {
			return "", "Spawning needs a moonID and the owning corporationID."
		}
		fuel := -1.0 // the template's own
		if raw := r.FormValue("fuelDays"); raw != "" {
			if f, err := strconv.ParseFloat(raw, 64); err == nil && f >= 0 {
				fuel = f
			}
		}
		out, err := StarbaseTemplateSpawner(id, moonID, corpID, r.FormValue("online") != "", fuel)
		if err != nil {
			return "", err.Error()
		}
		return out, ""
	}
	return "", "Unknown action."
}
