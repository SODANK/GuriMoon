package esp

// R&D research + patent lottery admin page.
//
// The read half is the research browser: which fields exist, which agents offer them, what each
// field's Tech II patent pool holds, and who is collaborating. The write half is the lottery:
// settings, a forced draw, the draw log, the uncollected patents, and the pool editor that fills
// a field the invention-chain derivation left empty.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	evedb "gurimoon/db"
)

// RnDForceDrawHook is set by the game server. The forced-draw button calls it to close one
// agent's allocation round now, tell the winner and invalidate every entrant's journal, which a
// direct evedb call cannot do.
var RnDForceDrawHook func(agentID int64) (string, error)

// handleRnD is the R&D research browser and patent-lottery panel: field
// overview with agent counts and patent pools, active player projects, the
// live rounds, the draw log, uncollected patents, and per-field drilldown
// (?field=ID) with the pool editor.
func (s *Server) handleRnD(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if r.Method == http.MethodPost {
		s.handleRnDAction(w, r, ctx)
		return
	}

	fields, err := evedb.ESPGetResearchFields(ctx, s.db)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "rnd"})
		return
	}

	data := map[string]any{
		"Page":   "rnd",
		"Fields": fields,
		"Msg":    r.URL.Query().Get("msg"),
	}

	if fieldID, _ := strconv.ParseInt(r.URL.Query().Get("field"), 10, 64); fieldID > 0 {
		for i := range fields {
			if fields[i].SkillID == fieldID {
				data["Field"] = fields[i]
				break
			}
		}
		if data["Field"] != nil {
			bps, err := evedb.ESPGetFieldBlueprints(ctx, s.db, fieldID)
			if err != nil {
				s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "rnd"})
				return
			}
			agents, err := evedb.ESPGetFieldAgents(ctx, s.db, fieldID)
			if err != nil {
				s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "rnd"})
				return
			}
			data["Blueprints"] = bps
			data["FieldAgents"] = agents
		}
		s.render(w, "rnd.html", data)
		return
	}

	projects, err := evedb.ESPGetActiveResearch(ctx, s.db)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "rnd"})
		return
	}
	data["Projects"] = projects

	rounds, err := evedb.ESPListAgentRounds(ctx, s.db)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "rnd"})
		return
	}
	draws, _ := evedb.ESPListPatentDraws(ctx, s.db, 50)
	pending, _ := evedb.ESPListPendingPatents(ctx, s.db, false, 200)

	data["Rounds"] = rounds
	data["Draws"] = draws
	data["Pending"] = pending
	data["Enabled"] = evedb.PatentLotteryEnabled(ctx, s.db)
	data["RoundDays"] = evedb.GetPatentRoundDays(ctx, s.db)
	data["ClaimDays"] = evedb.GetPatentClaimDays(ctx, s.db)
	data["BotWeight"] = evedb.GetPatentBotWeightPct(ctx, s.db)
	data["MinTickets"] = evedb.GetPatentMinTickets(ctx, s.db)
	data["NoDupes"] = evedb.PatentAvoidDuplicates(ctx, s.db)
	data["HooksLive"] = RnDForceDrawHook != nil
	data["Now"] = time.Now()

	s.render(w, "rnd.html", data)
}

func (s *Server) handleRnDAction(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/rnd?msg="+url.QueryEscape("Bad form: "+err.Error()), http.StatusSeeOther)
		return
	}
	// Pool edits redirect back to the field drilldown they were made from.
	back := "/rnd"
	if fieldID := espFormInt(r, "fieldID"); fieldID > 0 {
		back = fmt.Sprintf("/rnd?field=%d", fieldID)
	}
	var result string

	switch r.FormValue("action") {

	case "settings":
		enabled := r.FormValue("enabled") != ""
		noDupes := r.FormValue("noDupes") != ""
		roundDays := espFormInt(r, "roundDays")
		claimDays := espFormInt(r, "claimDays")
		botWeight := espFormInt(r, "botWeight")
		minTickets := espFormInt(r, "minTickets")
		if err := evedb.SetPatentLotteryEnabled(ctx, s.db, enabled); err != nil {
			result = "Error: " + err.Error()
			break
		}
		_ = evedb.SetPatentRoundDays(ctx, s.db, int(roundDays))
		_ = evedb.SetPatentClaimDays(ctx, s.db, int(claimDays))
		_ = evedb.SetPatentBotWeightPct(ctx, s.db, int(botWeight))
		_ = evedb.SetPatentMinTickets(ctx, s.db, int(minTickets))
		_ = evedb.SetPatentAvoidDuplicates(ctx, s.db, noDupes)
		state := "disabled"
		if enabled {
			state = "enabled"
		}
		result = fmt.Sprintf("Lottery %s: %d-day rounds, %d days to collect, synthetic pilots at %d%% weight, %d RP floor. "+
			"The round length applies to the next round each agent opens — rounds already running keep the finish line their entrants planned around.",
			state, roundDays, claimDays, botWeight, minTickets)

	case "forcedraw":
		if RnDForceDrawHook == nil {
			result = "The game server has not wired the lottery hook — action unavailable."
			break
		}
		msg, err := RnDForceDrawHook(espFormInt(r, "agentID"))
		if err != nil {
			result = "Error: " + err.Error()
			break
		}
		result = msg

	case "voidoffer":
		if err := evedb.SetPatentOfferState(ctx, s.db, espFormInt(r, "offerID"), evedb.PatentOfferVoided); err != nil {
			result = "Error: " + err.Error()
			break
		}
		result = "Patent voided. The blueprint was never created, so nothing is taken back — the winner simply cannot collect it."

	case "extendoffer":
		days := int(espFormInt(r, "days"))
		if days <= 0 {
			days = evedb.GetPatentClaimDays(ctx, s.db)
		}
		if err := evedb.ExtendPatentOffer(ctx, s.db, espFormInt(r, "offerID"), days, time.Now()); err != nil {
			result = "Error: " + err.Error()
			break
		}
		result = fmt.Sprintf("Patent held for another %d day(s). An already-lapsed one is pending again.", days)

	case "pooladd":
		if err := evedb.AddFieldBlueprint(ctx, s.db, espFormInt(r, "fieldID"), espFormInt(r, "blueprintTypeID")); err != nil {
			result = "Error: " + err.Error()
			break
		}
		result = "Blueprint added to the patent pool. Every collaborator in this field can now win it."

	case "poolremove":
		if err := evedb.RemoveFieldBlueprint(ctx, s.db, espFormInt(r, "fieldID"), espFormInt(r, "blueprintTypeID")); err != nil {
			result = "Error: " + err.Error()
			break
		}
		result = "Blueprint removed from the patent pool. Patents already won are unaffected — the prize was drawn at the time."

	default:
		result = "Unknown action."
	}

	http.Redirect(w, r, back+joinQuery(back)+"msg="+url.QueryEscape(result), http.StatusSeeOther)
}

// joinQuery returns the separator a query parameter needs on this path.
func joinQuery(path string) string {
	for i := 0; i < len(path); i++ {
		if path[i] == '?' {
			return "&"
		}
	}
	return "?"
}
