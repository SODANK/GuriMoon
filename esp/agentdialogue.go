package esp

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	evedb "gurimoon/db"
)

// ── Agents → Dialogue ────────────────────────────────────────────────────────
//
// Every line an agent speaks outside a mission template (the conversation shell: greeting,
// refusals, cooldown; the service pitches; the captions on the player's own choices), browsable
// and editable per division. Mission briefing, accept and decline texts stay in the mission
// editor. Scope 0 is "all agents"; a division scope overrides line by line and inherits the rest.

// espDialogueDivision is one entry in the division picker.
type espDialogueDivision struct {
	DivisionID   int64
	DivisionName string
	Agents       int
	Authored     int // division-scoped lines deviating from the global voice
}

// espDialogueGroup is one category section of rows.
type espDialogueGroup struct {
	Name string
	Rows []evedb.ESPDialogueRow
}

func (s *Server) handleAgentDialogue(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	division, _ := strconv.ParseInt(r.URL.Query().Get("division"), 10, 64)

	var result string
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err == nil {
			division, _ = strconv.ParseInt(r.FormValue("division"), 10, 64)
			changed := 0
			for _, def := range evedb.ServiceTextDefs {
				body, ok := r.Form["text_"+def.Key]
				if !ok {
					continue
				}
				before := evedb.GetAgentText(ctx, s.db, division, def.Key)
				if err := evedb.SetAgentText(ctx, s.db, division, def.Key, body[0]); err != nil {
					result = "Error: " + err.Error()
					break
				}
				if evedb.GetAgentText(ctx, s.db, division, def.Key) != before {
					changed++
				}
			}
			if result == "" {
				scope := "all agents"
				if division != 0 {
					scope = fmt.Sprintf("division %d", division)
				}
				result = fmt.Sprintf("Dialogue saved for %s: %d lines changed.", scope, changed)
			}
		}
	}

	divs, err := evedb.ESPGetNPCDivisions(ctx, s.db)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "agent-dialogue"})
		return
	}
	authored, _ := evedb.ESPDialogueDivisionCounts(ctx, s.db)

	picker := make([]espDialogueDivision, 0, len(divs))
	selectedName := "All agents (global)"
	for _, d := range divs {
		picker = append(picker, espDialogueDivision{
			DivisionID: d.DivisionID, DivisionName: d.DivisionName,
			Agents: d.Agents, Authored: authored[d.DivisionID],
		})
		if d.DivisionID == division {
			selectedName = d.DivisionName
		}
	}

	rows, err := evedb.ESPListDialogue(ctx, s.db, division)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "agent-dialogue"})
		return
	}
	byCat := map[string][]evedb.ESPDialogueRow{}
	for _, row := range rows {
		byCat[row.Category] = append(byCat[row.Category], row)
	}
	groups := make([]espDialogueGroup, 0, len(evedb.DialogueCategories))
	for _, c := range evedb.DialogueCategories {
		if len(byCat[c.Key]) > 0 {
			groups = append(groups, espDialogueGroup{Name: c.Name, Rows: byCat[c.Key]})
		}
	}

	s.render(w, "agent_dialogue.html", map[string]any{
		"Page":         "agent-dialogue",
		"Result":       result,
		"Divisions":    picker,
		"Division":     division,
		"DivisionName": selectedName,
		"Groups":       groups,
	})
}
