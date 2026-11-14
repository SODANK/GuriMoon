package esp

// Alliance browser and editor: the operator's administration surface. The client's
// allianceRegistry gets GetAlliance, GetAlliances and GetRankedAlliances from the wire layer
// (cmd/gurimoon/alliancewrites.go).
//
// It owns two things. The ID: an alliance may be created at a specific allianceID, the only lever
// that changes the logo the client draws (db/alliancelogoslots.go), and changing it later
// renumbers every reference (db.ReassignAllianceID). The name rows: ESPSyncAllianceNames writes
// the ticker rows the client's short-name lookup reads, with a one-click repair for existing ones.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

// AllianceSweepHook, when the game server sets it, runs the accepted-application sweep at once.
// The "make effective now" button calls it so the join, the member push and the session change
// run on the same code path as the timer.
var AllianceSweepHook func()

func runAllianceSweep() {
	if AllianceSweepHook != nil {
		AllianceSweepHook()
	}
}

// allianceLogoSlotView is one selectable logo slot, for the create form and for
// the per-alliance picker.
type allianceLogoSlotView struct {
	ID         int64
	Tile       string
	Taken      bool   // already used by an existing alliance
	HolderName string // that alliance's name, for the hover text
	Current    bool   // the slot the alliance being edited is already on
}

// logoSlotViews lists every client logo slot with the alliance sitting on it, if any. A slot
// held by an alliance other than self is taken; the editing alliance's own slot is current.
func (s *Server) logoSlotViews(ctx context.Context, self int64) []allianceLogoSlotView {
	alliances, _ := evedb.ESPListAlliancesFull(ctx, s.db)
	holder := make(map[int64]string, len(alliances))
	for _, a := range alliances {
		holder[a.AllianceID] = a.AllianceName
	}
	out := make([]allianceLogoSlotView, 0, len(evedb.AllianceLogoSlots))
	for _, sl := range evedb.AllianceLogoSlots {
		v := allianceLogoSlotView{ID: sl.ID, Tile: sl.Tile, HolderName: holder[sl.ID]}
		_, v.Taken = holder[sl.ID]
		v.Current = sl.ID == self
		out = append(out, v)
	}
	return out
}

// handleAlliances serves the alliance browser and the create form.
func (s *Server) handleAlliances(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if r.Method == http.MethodPost {
		s.handleAllianceCreate(w, r, ctx)
		return
	}

	alliances, err := evedb.ESPListAlliancesFull(ctx, s.db)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "alliances"})
		return
	}
	issues, _ := evedb.ESPAllianceNameIssues(ctx, s.db)
	corps, _ := evedb.ESPListCorps(ctx, s.db, "", -1, "player", corpPickerCap, 0)

	logos := make(map[int64]bool, len(alliances))
	for _, a := range alliances {
		logos[a.AllianceID] = hasAllianceLogo(a.AllianceID)
	}
	slots := s.logoSlotViews(ctx, 0)

	s.render(w, "alliances.html", map[string]any{
		"Alliances":   alliances,
		"Issues":      issues,
		"FreeCorps":   corps,
		"Slots":       slots,
		"TileArt":     s.allianceTilesAvailable(),
		"StoredLogos": logos,
		"Msg":         r.URL.Query().Get("msg"),
		"Page":        "alliances",
	})
}

// handleAllianceCreate handles the create form and the whole-table repairs.
func (s *Server) handleAllianceCreate(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/alliances?msg="+url.QueryEscape("Bad form: "+err.Error()), http.StatusSeeOther)
		return
	}
	var result string
	dest := "/alliances"

	switch r.FormValue("action") {
	case "create":
		in := evedb.ESPAllianceEdit{
			Name:        strings.TrimSpace(r.FormValue("allianceName")),
			ShortName:   strings.TrimSpace(r.FormValue("shortName")),
			Description: r.FormValue("description"),
			URL:         strings.TrimSpace(r.FormValue("url")),
			Dictatorial: r.FormValue("dictatorial") != "",
		}
		allianceID := espFormInt(r, "allianceID")
		founder := espFormInt(r, "founderCorpID")
		newID, err := evedb.ESPCreateAlliance(ctx, s.db, allianceID, in, founder)
		if err != nil {
			result = "Error: " + err.Error()
			break
		}
		invalidateOwnerNames()
		if founder > 0 {
			// The founding corp's online pilots need the new session value (the push addcorp sends).
			notifyAllianceMembership(founder, 0, newID)
			notifyAllianceEvent("member", newID, founder)
		}
		tile, mapped := evedb.AllianceLogoTile(newID)
		if mapped {
			result = fmt.Sprintf("Alliance %d created — the client draws logo tile %s for this ID.", newID, tile)
		} else {
			result = fmt.Sprintf("Alliance %d created. This ID is not in the client's logo table, so the game draws the 01_01 fallback; upload a logo for the panel and web pages on the alliance page.", newID)
		}
		dest = fmt.Sprintf("/alliance/%d", newID)

	case "repairnames":
		n, err := evedb.ESPRepairAllianceNames(ctx, s.db)
		if err != nil {
			result = fmt.Sprintf("Repaired %d, then failed: %v", n, err)
			break
		}
		invalidateOwnerNames()
		result = fmt.Sprintf("Rewrote name and ticker rows for %d alliance(s) and refreshed member counts.", n)

	default:
		result = "Unknown action."
	}

	http.Redirect(w, r, dest+"?msg="+url.QueryEscape(result), http.StatusSeeOther)
}

// handleAllianceView serves and edits one alliance.
func (s *Server) handleAllianceView(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 3 {
		http.NotFound(w, r)
		return
	}
	allianceID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if r.Method == http.MethodPost {
		s.handleAllianceEdit(w, r, ctx, allianceID)
		return
	}

	a, err := evedb.ESPGetAllianceFull(ctx, s.db, allianceID)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "alliances"})
		return
	}
	// Corporations that could be added: every player corp not already in an alliance. NPC corps
	// are left out of the picker.
	free, _ := evedb.ESPListCorps(ctx, s.db, "", -1, "player", corpPickerCap, 0)
	members, _ := evedb.ListAllianceMembers(ctx, s.db, allianceID)
	applications, _ := evedb.ListAllianceApplications(ctx, s.db, allianceID)
	relationships, _ := evedb.ListAllianceRelationships(ctx, s.db, allianceID)

	s.render(w, "alliance_view.html", map[string]any{
		"Alliance":      a,
		"FreeCorps":     free,
		"MemberRows":    members,
		"Applications":  applications,
		"Relationships": relationships,
		"AcceptDelay":   evedb.AllianceAcceptDelayHours(ctx, s.db),
		"RelTypes": []struct {
			ID   int64
			Name string
		}{
			{evedb.AllianceRelationshipNAP, evedb.AllianceRelationshipName(evedb.AllianceRelationshipNAP)},
			{evedb.AllianceRelationshipFriend, evedb.AllianceRelationshipName(evedb.AllianceRelationshipFriend)},
			{evedb.AllianceRelationshipCompetitor, evedb.AllianceRelationshipName(evedb.AllianceRelationshipCompetitor)},
			{evedb.AllianceRelationshipEnemy, evedb.AllianceRelationshipName(evedb.AllianceRelationshipEnemy)},
		},
		"Slots":    s.logoSlotViews(ctx, allianceID),
		"TileArt":  s.allianceTilesAvailable(),
		"HasLogo":  hasAllianceLogo(allianceID),
		"StoreDir": allianceLogoDir,
		"Self":     fmt.Sprintf("/alliance/%d", allianceID),
		"Msg":      r.URL.Query().Get("msg"),
		"Page":     "alliances",
	})
}

// handleAllianceEdit applies one POSTed action.
func (s *Server) handleAllianceEdit(w http.ResponseWriter, r *http.Request, ctx context.Context, allianceID int64) {
	self := fmt.Sprintf("/alliance/%d", allianceID)
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, self+"?msg="+url.QueryEscape("Bad form: "+err.Error()), http.StatusSeeOther)
		return
	}
	var result string
	dest := self

	switch r.FormValue("action") {
	case "details":
		in := evedb.ESPAllianceEdit{
			Name:           strings.TrimSpace(r.FormValue("allianceName")),
			ShortName:      strings.TrimSpace(r.FormValue("shortName")),
			Description:    r.FormValue("description"),
			URL:            strings.TrimSpace(r.FormValue("url")),
			ExecutorCorpID: espFormInt(r, "executorCorpID"),
			Dictatorial:    r.FormValue("dictatorial") != "",
		}
		if err := evedb.ESPUpdateAlliance(ctx, s.db, allianceID, in); err != nil {
			result = "Error: " + err.Error()
			break
		}
		invalidateOwnerNames()
		notifyAllianceEvent("details", allianceID, 0)
		result = "Alliance saved."

	case "addcorp":
		corpID := espFormInt(r, "corporationID")
		if corpID <= 0 {
			result = "Select a corporation."
			break
		}
		old, err := evedb.ESPSetCorpAlliance(ctx, s.db, corpID, allianceID)
		if err != nil {
			result = "Error: " + err.Error()
			break
		}
		notifyAllianceMembership(corpID, old, allianceID)
		notifyAllianceEvent("member", allianceID, corpID)
		result = "Corporation added; its online members were told about the new alliance."

	case "removecorp":
		corpID := espFormInt(r, "corporationID")
		old, err := evedb.ESPSetCorpAlliance(ctx, s.db, corpID, 0)
		if err != nil {
			result = "Error: " + err.Error()
			break
		}
		notifyAllianceMembership(corpID, old, 0)
		notifyAllianceEvent("member-removed", allianceID, corpID)
		result = "Corporation removed; the executor was recomputed from the remaining votes."

	case "executor":
		a, err := evedb.ESPGetAllianceFull(ctx, s.db, allianceID)
		if err != nil {
			result = "Error: " + err.Error()
			break
		}
		in := evedb.ESPAllianceEdit{
			Name: a.AllianceName, ShortName: a.ShortName,
			Description: a.Description, URL: a.URL,
			ExecutorCorpID: espFormInt(r, "executorCorpID"),
			Dictatorial:    a.Dictatorial,
		}
		if err := evedb.ESPUpdateAlliance(ctx, s.db, allianceID, in); err != nil {
			result = "Error: " + err.Error()
			break
		}
		result = "Executor corporation set."

	case "support":
		corpID := espFormInt(r, "corporationID")
		backed := espFormInt(r, "chosenExecutorID")
		if backed == 0 {
			// "Secret": clear the vote rather than backing corporation 0.
			if err := evedb.ClearExecutorSupport(ctx, s.db, corpID); err != nil {
				result = "Error: " + err.Error()
				break
			}
			if _, err := evedb.RecomputeAllianceExecutor(ctx, s.db, allianceID); err != nil {
				result = "Error: " + err.Error()
				break
			}
			result = "Executor support cleared."
			break
		}
		executor, err := evedb.DeclareExecutorSupport(ctx, s.db, allianceID, corpID, backed)
		if err != nil {
			result = "Error: " + err.Error()
			break
		}
		notifyAllianceEvent("member", allianceID, corpID)
		notifyAllianceEvent("details", allianceID, 0)
		result = fmt.Sprintf("Support recorded; the executor is now corporation %d.", executor)

	case "executorvote":
		executor, err := evedb.RecomputeAllianceExecutor(ctx, s.db, allianceID)
		if err != nil {
			result = "Error: " + err.Error()
			break
		}
		notifyAllianceEvent("details", allianceID, 0)
		result = fmt.Sprintf("Executor recomputed from the members' declared support: corporation %d.", executor)

	case "application":
		corpID := espFormInt(r, "corporationID")
		switch r.FormValue("decision") {
		case "accept":
			if err := evedb.UpdateAllianceApplication(ctx, s.db, allianceID, corpID, r.FormValue("text"), evedb.AllianceApplicationAccepted); err != nil {
				result = "Error: " + err.Error()
				break
			}
			notifyAllianceEvent("app", allianceID, corpID)
			result = fmt.Sprintf("Application accepted. The corporation joins when the waiting period is up (%.4g h).",
				evedb.AllianceAcceptDelayHours(ctx, s.db))
		case "reject":
			if err := evedb.UpdateAllianceApplication(ctx, s.db, allianceID, corpID, r.FormValue("text"), evedb.AllianceApplicationRejected); err != nil {
				result = "Error: " + err.Error()
				break
			}
			notifyAllianceEvent("app", allianceID, corpID)
			result = "Application rejected."
		case "now":
			if err := evedb.ForceApplicationDue(ctx, s.db, allianceID, corpID); err != nil {
				result = "Error: " + err.Error()
				break
			}
			runAllianceSweep()
			result = "Waiting period skipped; the corporation joins on this pass."
		case "delete":
			if err := evedb.DeleteAllianceApplication(ctx, s.db, corpID, allianceID); err != nil {
				result = "Error: " + err.Error()
				break
			}
			notifyAllianceEvent("app-removed", allianceID, corpID)
			result = "Application deleted."
		default:
			result = "Unknown decision."
		}

	case "relationship":
		toID := espFormInt(r, "toID")
		if r.FormValue("clear") != "" {
			if err := evedb.DeleteAllianceRelationship(ctx, s.db, allianceID, toID); err != nil {
				result = "Error: " + err.Error()
				break
			}
			notifyAllianceEvent("rel-del", allianceID, toID)
			result = "Standing cleared."
			break
		}
		rel := espFormInt(r, "relationship")
		if err := evedb.SetAllianceRelationship(ctx, s.db, allianceID, toID, rel); err != nil {
			result = "Error: " + err.Error()
			break
		}
		notifyAllianceEvent("rel-set", allianceID, toID)
		result = fmt.Sprintf("Standing towards %d set to %s.", toID, evedb.AllianceRelationshipName(rel))

	case "acceptdelay":
		hours, _ := strconv.ParseFloat(strings.TrimSpace(r.FormValue("hours")), 64)
		if err := evedb.SetAllianceAcceptDelayHours(ctx, s.db, hours); err != nil {
			result = "Error: " + err.Error()
			break
		}
		result = fmt.Sprintf("Accepted applications now take %.4g hour(s) to become effective. "+
			"The client's own accept dialog still says 24 hours.", hours)

	case "recount":
		if err := evedb.ESPRecountAlliance(ctx, s.db, allianceID); err != nil {
			result = "Error: " + err.Error()
			break
		}
		result = "Member count refreshed from the corporations actually linked to this alliance."

	case "logoslot":
		newID := espFormInt(r, "newAllianceID")
		// Read the member corps before the renumber: afterwards nothing points at the old ID.
		members, _ := evedb.ListAllianceMembers(ctx, s.db, allianceID)
		newID, rows, err := evedb.ReassignAllianceID(ctx, s.db, allianceID, newID)
		if err != nil {
			result = "Error: " + err.Error()
			break
		}
		if err := moveAllianceLogo(allianceID, newID); err != nil {
			result = "Renumbered, but the stored logo could not be moved: " + err.Error()
		}
		for _, m := range members {
			notifyAllianceMembership(m.CorporationID, allianceID, newID)
		}
		invalidateOwnerNames()
		notifyAllianceEvent("details", newID, 0)
		tile, mapped := evedb.AllianceLogoTile(newID)
		art := "the 01_01 fallback"
		if mapped {
			art = "logo tile " + tile
		}
		if result == "" {
			result = fmt.Sprintf("Alliance moved from %d to %d across %d row(s); the client now draws %s. "+
				"Pilots already in space keep the old logo until their client refetches the owner names.",
				allianceID, newID, rows, art)
		}
		dest = fmt.Sprintf("/alliance/%d", newID)

	case "droplogo":
		dropped, err := dropAllianceLogo(allianceID)
		if err != nil {
			result = "Error: " + err.Error()
			break
		}
		if dropped {
			result = "Stored logo removed."
		} else {
			result = "No stored logo to remove."
		}

	case "delete":
		if strings.TrimSpace(r.FormValue("confirm")) != "DELETE" {
			result = "Type DELETE to confirm."
			break
		}
		members, err := evedb.ESPDeleteAlliance(ctx, s.db, allianceID)
		if err != nil {
			result = "Error: " + err.Error()
			break
		}
		// Every online member's session still says allianceid=<deleted> and the client's alliance
		// window raises on a bound alliance that no longer resolves, so each corp is sent None.
		for _, corpID := range members {
			notifyAllianceMembership(corpID, allianceID, 0)
		}
		_, _ = dropAllianceLogo(allianceID)
		invalidateOwnerNames()
		result = fmt.Sprintf("Alliance %d deleted: members unlinked, sovereignty claims dropped, name rows removed.", allianceID)
		dest = "/alliances"

	default:
		result = "Unknown action."
	}

	http.Redirect(w, r, dest+"?msg="+url.QueryEscape(result), http.StatusSeeOther)
}
