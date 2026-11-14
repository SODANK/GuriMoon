package esp

// Industry → Blueprints.
//
// The list is every invBlueprintTypes row; the detail page edits one of them in three cards:
//
//   - Type (invBlueprintTypes): production time and waste factor. Applies to every copy; a
//     running job keeps the end time it was stamped with.
//   - Requirements (tl2materialsfortypewithactivity): consumed on install only.
//   - Instances (invBlueprints): one player's item, with its ME, PE, copy flag and runs.

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

// blueprintListLimit bounds the list: the table has a few thousand rows and rendering all of
// them is slower than searching.
const blueprintListLimit = 400

// handleBlueprints serves /industry/blueprints.
func (s *Server) handleBlueprints(w http.ResponseWriter, r *http.Request) {
	var result, resultErr string
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err == nil {
			result, resultErr = s.blueprintListAction(r)
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	f := evedb.ESPBlueprintFilter{
		Search:     strings.TrimSpace(r.URL.Query().Get("q")),
		CategoryID: parseInt64(r.URL.Query().Get("category")),
		TechLevel:  parseInt64(r.URL.Query().Get("tech")),
		OwnedOnly:  r.URL.Query().Get("owned") == "1",
		Missing:    r.URL.Query().Get("missing") == "1",
		Limit:      blueprintListLimit,
	}

	stats, err := evedb.ESPBlueprintStatsRow(ctx, s.db)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "blueprints"})
		return
	}
	rows, err := evedb.ESPListBlueprints(ctx, s.db, f)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "blueprints"})
		return
	}
	cats, _ := evedb.ESPBlueprintCategories(ctx, s.db)

	s.render(w, "blueprints.html", map[string]any{
		"Page":       "blueprints",
		"Result":     result,
		"ResultErr":  resultErr,
		"Stats":      stats,
		"Rows":       rows,
		"Categories": cats,
		"Query":      f.Search,
		"CategoryID": f.CategoryID,
		"TechLevel":  f.TechLevel,
		"OwnedOnly":  f.OwnedOnly,
		"Missing":    f.Missing,
		"Truncated":  len(rows) >= blueprintListLimit,
		"Limit":      blueprintListLimit,
	})
}

// blueprintListAction handles the one write the list page offers: seeding an
// invBlueprintTypes row for a blueprint item type that has none.
func (s *Server) blueprintListAction(r *http.Request) (result, resultErr string) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	switch r.FormValue("action") {
	case "createType":
		typeID := parseInt64(r.FormValue("blueprintTypeID"))
		productID := parseInt64(r.FormValue("productTypeID"))
		if err := evedb.ESPCreateBlueprintType(ctx, s.db, typeID, productID); err != nil {
			return "", "Error: " + err.Error()
		}
		return "Seeded blueprint data for type " + strconv.FormatInt(typeID, 10) +
			" with placeholder values — set its product and times below.", ""
	}
	return "", ""
}

// handleBlueprintView serves /industry/blueprint/<blueprintTypeID>.
func (s *Server) handleBlueprintView(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 4 {
		http.NotFound(w, r)
		return
	}
	blueprintTypeID, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	var result, resultErr string
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err == nil {
			result, resultErr = s.blueprintAction(r, blueprintTypeID)
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	detail, err := evedb.ESPGetBlueprintDetail(ctx, s.db, blueprintTypeID)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "blueprints"})
		return
	}

	s.render(w, "blueprint_view.html", map[string]any{
		"Page":       "blueprints",
		"Result":     result,
		"ResultErr":  resultErr,
		"BP":         detail,
		"Activities": blueprintActivityChoices(),
	})
}

// activityChoice is one entry of the material editor's activity dropdown.
type activityChoice struct {
	ID   int64
	Name string
}

// blueprintActivityChoices lists the activities a requirement row can be
// authored against.
func blueprintActivityChoices() []activityChoice {
	out := make([]activityChoice, 0, len(evedb.ESPEditableActivities))
	for _, id := range evedb.ESPEditableActivities {
		out = append(out, activityChoice{ID: id, Name: evedb.ESPActivityName[id]})
	}
	return out
}

// blueprintAction handles every write on the detail page.
func (s *Server) blueprintAction(r *http.Request, blueprintTypeID int64) (result, resultErr string) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	switch r.FormValue("action") {
	case "saveType":
		e := evedb.ESPBlueprintTypeEdit{
			BlueprintTypeID:            blueprintTypeID,
			ParentBlueprintTypeID:      parseInt64(r.FormValue("parentBlueprintTypeID")),
			ProductTypeID:              parseInt64(r.FormValue("productTypeID")),
			ProductionTime:             parseInt64(r.FormValue("productionTime")),
			TechLevel:                  parseInt64(r.FormValue("techLevel")),
			ResearchProductivityTime:   parseInt64(r.FormValue("researchProductivityTime")),
			ResearchMaterialTime:       parseInt64(r.FormValue("researchMaterialTime")),
			ResearchCopyTime:           parseInt64(r.FormValue("researchCopyTime")),
			ResearchTechTime:           parseInt64(r.FormValue("researchTechTime")),
			ProductivityModifier:       parseInt64(r.FormValue("productivityModifier")),
			MaterialModifier:           parseInt64(r.FormValue("materialModifier")),
			WasteFactor:                parseInt64(r.FormValue("wasteFactor")),
			ChanceOfReverseEngineering: parseFloat64(r.FormValue("chanceOfReverseEngineering")),
			MaxProductionLimit:         parseInt64(r.FormValue("maxProductionLimit")),
		}
		if err := evedb.ESPUpdateBlueprintType(ctx, s.db, e); err != nil {
			return "", "Error: " + err.Error()
		}
		return "Blueprint type saved. Jobs already running keep the end time they were stamped with; the next install uses the new numbers.", ""

	case "saveMaterial":
		activity := parseInt64(r.FormValue("activity"))
		reqType := parseInt64(r.FormValue("requiredTypeID"))
		qty := parseInt64(r.FormValue("quantity"))
		damage := parseFloat64(r.FormValue("damagePerJob"))
		var recycle int64
		if r.FormValue("recycle") != "" {
			recycle = 1
		}
		if err := evedb.ESPUpsertBlueprintMaterial(ctx, s.db, blueprintTypeID,
			activity, reqType, qty, damage, recycle); err != nil {
			return "", "Error: " + err.Error()
		}
		return "Saved requirement: type " + strconv.FormatInt(reqType, 10) +
			" ×" + strconv.FormatInt(qty, 10) + " for " + activityLabel(activity) + ".", ""

	case "deleteMaterial":
		activity := parseInt64(r.FormValue("activity"))
		reqType := parseInt64(r.FormValue("requiredTypeID"))
		if err := evedb.ESPDeleteBlueprintMaterial(ctx, s.db, blueprintTypeID, activity, reqType); err != nil {
			return "", "Error: " + err.Error()
		}
		return "Removed type " + strconv.FormatInt(reqType, 10) + " from " + activityLabel(activity) + ".", ""

	case "saveInstance":
		bpID := parseInt64(r.FormValue("blueprintID"))
		me := parseInt64(r.FormValue("materialLevel"))
		pe := parseInt64(r.FormValue("productivityLevel"))
		var isCopy int64
		if r.FormValue("copy") != "" {
			isCopy = 1
		}
		runs := parseInt64(r.FormValue("runsRemaining"))
		if err := evedb.ESPUpdateBlueprintInstance(ctx, s.db, bpID, me, pe, isCopy, runs); err != nil {
			return "", "Error: " + err.Error()
		}
		kind := "original"
		if isCopy == 1 {
			kind = "copy, " + strconv.FormatInt(runs, 10) + " runs"
		}
		return "Blueprint " + strconv.FormatInt(bpID, 10) + " is now ME " +
			strconv.FormatInt(me, 10) + " / PE " + strconv.FormatInt(pe, 10) +
			" (" + kind + "). The owner sees it after a session refresh.", ""
	}
	return "", ""
}

// activityLabel names an activity for a flash message.
func activityLabel(id int64) string {
	if n := evedb.ESPActivityName[id]; n != "" {
		return n
	}
	return "activity " + strconv.FormatInt(id, 10)
}

// handleItemTypesAPI backs the material picker's autocomplete at
// /industry/types/api?q=.
func (s *Server) handleItemTypesAPI(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	rows, err := evedb.ESPSearchItemTypes(ctx, s.db, r.URL.Query().Get("q"), 20)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if rows == nil {
		rows = []evedb.ESPTypeSearchRow{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rows)
}
