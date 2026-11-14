package esp

// Corporations browser and editor. The editor writes what the corp window writes
// (corpRegistry.UpdateCorporation / UpdateLogo, cmd/gurimoon/corpwrites.go) plus the fields no
// in-game path exposes: name, ticker, CEO, HQ station, member limit, alliance. An edit is pushed
// through CorpChangedHook so an open corp window updates in place.

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

// CorpFieldChange is one column an ESP edit changed. Values are string, int64, float64 or nil;
// the game server converts them to PyReps.
type CorpFieldChange struct {
	Column   string
	Old, New any
}

// OfficeDepositHook is set by a module. handleOfficeAdd (cmd/gurimoon) calls it on the RPC
// goroutine, so a handler must not block, when a pilot moves an item of its own into a corporation
// office hangar. The rolebots module credits a donated blueprint as equity.
var OfficeDepositHook func(corpID, charID, itemID, typeID, flag int64)

// MemberBaseChangedHook is set by a module. applyMemberUpdateRow (cmd/gurimoon) calls it on the
// RPC goroutine, so a handler must not block, after Edit Member changed a Base
// (chrCorporationRoles.baseID; 0 = none). actorID may be the member itself.
var MemberBaseChangedHook func(corpID, charID, actorID, oldBase, newBase int64)

// ShareMovedHook is set by a module. It is called on the RPC goroutine, so a handler must not
// block, after a share transfer (the wallet voucher's Give dialog) has committed: qty shares of
// stockCorpID went from fromHolder to toHolder.
var ShareMovedHook func(stockCorpID, fromHolder, toHolder, qty int64)

// CorpChangedHook, when the game server sets it, pushes an OnCorporationChanged
// notification to the corporation's online members.  Nil on a panel running
// without a server (tests, tooling).
var CorpChangedHook func(corpID int64, changes []CorpFieldChange)

// CorpSharesChangedHook is set by the game server. It pushes OnShareChange to every holder a
// panel share edit touched, pushes the new issued total when it moved, and re-weighs open votes,
// as the in-game Give dialog does.
var CorpSharesChangedHook func(corpID int64, deltas []evedb.ShareDelta, oldTotal, newTotal int64)

func notifyCorpSharesChanged(corpID int64, deltas []evedb.ShareDelta, oldTotal, newTotal int64) {
	if CorpSharesChangedHook != nil {
		CorpSharesChangedHook(corpID, deltas, oldTotal, newTotal)
	}
}

// OwnerNamesInvalidator is set by the game server and drops the owner-name and alliance-ticker
// bulk cachables, which are built once and handed to every client at login.
var OwnerNamesInvalidator func()

func invalidateOwnerNames() {
	if OwnerNamesInvalidator != nil {
		OwnerNamesInvalidator()
	}
}

// AllianceEventHook is set by the game server and pushes the matching OnAlliance*Changed after a
// panel edit, because the client caches Members, Standings and details for the session. kind
// names the edit; subjectID is the corpID or toID it concerns (0 for detail edits).
var AllianceEventHook func(kind string, allianceID, subjectID int64)

func notifyAllianceEvent(kind string, allianceID, subjectID int64) {
	if AllianceEventHook != nil {
		AllianceEventHook(kind, allianceID, subjectID)
	}
}

// CorpDeletedHook is set by the game server and sends every pilot kicked out of a deleted
// corporation the corpid session change (plus allianceid when the corp was in one).
var CorpDeletedHook func(corpID, oldAllianceID int64, kicked []evedb.ESPCorpKick)

// AllianceMembershipHook is set by the game server and pushes the allianceid session change to
// every online member of a corporation that joined or left an alliance. The client binds the
// alliance object off that value.
var AllianceMembershipHook func(corpID, oldAllianceID, newAllianceID int64)

func notifyAllianceMembership(corpID, oldAllianceID, newAllianceID int64) {
	if AllianceMembershipHook != nil && oldAllianceID != newAllianceID {
		AllianceMembershipHook(corpID, oldAllianceID, newAllianceID)
	}
}

// CorpHQChangedHook is set by the game server and pushes the hqID session change to online
// members after the headquarters moves. The client applies rolesAtHQ only while stationid ==
// hqID.
var CorpHQChangedHook func(corpID, oldHQ, newHQ int64)

func notifyCorpHQChanged(corpID, oldHQ, newHQ int64) {
	if CorpHQChangedHook != nil && oldHQ != newHQ {
		CorpHQChangedHook(corpID, oldHQ, newHQ)
	}
}

// notifyCorpChanged pushes a change dict, after making it safe to read. The client reads a dict's
// meaning from its values (bco_base.GetAddRemoveFromChange): all-nil old is an add, all-nil new a
// remove, all (nil, nil) raises. Unchanged columns are dropped and corporationName, which is NOT
// NULL, is always sent, which pins the dict to the update branch.
func notifyCorpChanged(corpID int64, oldName, newName string, changes []CorpFieldChange) {
	if CorpChangedHook == nil {
		return
	}
	out := make([]CorpFieldChange, 0, len(changes)+1)
	out = append(out, CorpFieldChange{Column: "corporationName", Old: oldName, New: newName})
	for _, ch := range changes {
		if ch.Column == "corporationName" || ch.Old == ch.New {
			continue
		}
		out = append(out, ch)
	}
	if len(out) == 1 && oldName == newName {
		return // nothing but the anchor: nothing actually changed
	}
	CorpChangedHook(corpID, out)
}

// corpsPageSize is the server-side page for the corporation browser.
const corpsPageSize = 200

// corpPickerCap bounds the corporation <select> pickers on the alliance and war pages, which are
// dropdowns, not browse lists.
const corpPickerCap = 3000

// corpsFilterCookie holds the corporation browser's last filters (rememberListFilter).
const corpsFilterCookie = "esp_corps_filter"

// corpsFilterKeys are the corporation browser parameters that are remembered between visits.
var corpsFilterKeys = []string{"q", "kind", "alliance"}

// handleCorporations serves the corporation browser. With no filter parameters it reopens with the
// filters last used.
func (s *Server) handleCorporations(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	query := rememberListFilter(w, r, corpsFilterCookie, "/corps", corpsFilterKeys)
	q := strings.TrimSpace(query.Get("q"))
	kind := query.Get("kind")
	allianceID, _ := strconv.ParseInt(query.Get("alliance"), 10, 64)
	page := int(espQueryInt(query.Get("page"), 0))
	if page < 0 {
		page = 0
	}

	corps, err := evedb.ESPListCorps(ctx, s.db, q, allianceID, kind,
		corpsPageSize, page*corpsPageSize)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "corporations"})
		return
	}
	alliances, _ := evedb.ESPListAlliancesFull(ctx, s.db)

	// The headline counts are asked of the database under the same filters, not tallied over the
	// current page.
	total, _ := evedb.ESPCountCorps(ctx, s.db, q, allianceID, kind)
	players, _ := evedb.ESPCountCorps(ctx, s.db, q, allianceID, "player")
	npcs, _ := evedb.ESPCountCorps(ctx, s.db, q, allianceID, "npc")
	pageCount := int((total + corpsPageSize - 1) / corpsPageSize)

	s.render(w, "corporations.html", map[string]any{
		"Corps":      corps,
		"Alliances":  alliances,
		"Q":          q,
		"Kind":       kind,
		"AllianceID": allianceID,
		"Total":      total,
		"Players":    players,
		"NPCs":       npcs,
		"PageNum":    page,
		"PageCount":  pageCount,
		"Prev":       espPageQuery(query, page-1),
		"Next":       espPageQuery(query, page+1),
		"Msg":        query.Get("msg"),
		"Page":       "corporations",
	})
}

// CorpPageLink is one extra link a module hangs on a corporation's page.
type CorpPageLink struct {
	Label string
	Href  string
	Note  string // optional dim suffix, e.g. who runs the corporation
}

// CorpPageLinks is set by a module and answers extra links for the /corporation/<id> page (the
// rolebots module points at the bot that runs the corporation). Nil or empty renders nothing.
var CorpPageLinks func(ctx context.Context, corpID int64) []CorpPageLink

// handleCorporationView serves and edits one corporation.
func (s *Server) handleCorporationView(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 3 {
		http.NotFound(w, r)
		return
	}
	corpID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if r.Method == http.MethodPost {
		s.handleCorporationEdit(w, r, ctx, corpID)
		return
	}

	detail, err := evedb.ESPGetCorpFull(ctx, s.db, corpID)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "corporations"})
		return
	}
	members, _ := evedb.ESPGetCorpMembers(ctx, s.db, corpID)
	alliances, _ := evedb.ESPListAlliancesFull(ctx, s.db)
	graphics, _ := evedb.ESPListLogoShapes(ctx, s.db)
	shapes := make([]shapeOption, 0, len(graphics))
	for _, g := range graphics {
		_, _, hasArt := logoShapeCell(g.GraphicID)
		shapes = append(shapes, shapeOption{GraphicID: g.GraphicID, Name: g.Name, HasArt: hasArt})
	}

	var extra []CorpPageLink
	if CorpPageLinks != nil {
		extra = CorpPageLinks(ctx, corpID)
	}

	var holders []evedb.CorpShareholder
	var issued, ceoWeight int64
	if !detail.IsNPC {
		holders, issued, _ = evedb.ESPCorpShareholders(ctx, s.db, corpID)
		if detail.CEOID > 0 {
			ceoWeight = evedb.ShareVoteWeight(ctx, s.db, detail.CEOID, corpID)
		}
	}

	s.render(w, "corporation.html", map[string]any{
		"Corp":      detail,
		"Extra":     extra,
		"Members":   members,
		"Alliances": alliances,
		"Shapes":    shapes,
		"Colours":   logoColours,
		"HasArt":    s.logoLibAvailable(),
		"Layers":    corpLogoLayers(detail),
		"Holders":   holders,
		"Issued":    issued,
		"CEOWeight": ceoWeight,
		"Self":      fmt.Sprintf("/corporation/%d", corpID),
		"Msg":       r.URL.Query().Get("msg"),
		"Page":      "corporations",
	})
}

// shapeOption is one entry of the logo shape picker. HasArt is false for the three shape files
// that point at an icon sheet: they are listed because a corporation may wear one, but draw no
// layer.
type shapeOption struct {
	GraphicID int64
	Name      string
	HasArt    bool
}

// corpLogoLayer is one composited layer of a corporation logo, ready to render.
type corpLogoLayer struct {
	Index    int
	Shape    int64
	Colour   int64
	Hex      string
	Name     string
	HasArt   bool
	Unusable bool // colour graphic the client cannot read a diffuse from
}

// corpLogoLayers pairs the three shape/colour columns for display.
func corpLogoLayers(c *evedb.ESPCorpFull) []corpLogoLayer {
	shapes := [3]int64{c.Shape1, c.Shape2, c.Shape3}
	colours := [3]int64{c.Color1, c.Color2, c.Color3}
	out := make([]corpLogoLayer, 3)
	for i := 0; i < 3; i++ {
		_, _, hasArt := logoShapeCell(shapes[i])
		hex := logoColourHex(colours[i])
		out[i] = corpLogoLayer{
			Index:    i + 1,
			Shape:    shapes[i],
			Colour:   colours[i],
			Hex:      hex,
			Name:     logoColourName(colours[i]),
			HasArt:   hasArt,
			Unusable: colours[i] > 0 && hex == "",
		}
	}
	return out
}

// handleCorporationEdit applies one POSTed action.
func (s *Server) handleCorporationEdit(w http.ResponseWriter, r *http.Request, ctx context.Context, corpID int64) {
	self := fmt.Sprintf("/corporation/%d", corpID)
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, self+"?msg="+url.QueryEscape("Bad form: "+err.Error()), http.StatusSeeOther)
		return
	}
	var result string
	dest := self

	switch r.FormValue("action") {
	case "details":
		before, err := evedb.ESPGetCorpFull(ctx, s.db, corpID)
		if err != nil {
			result = "Error: " + err.Error()
			break
		}
		tax, _ := strconv.ParseFloat(strings.TrimSpace(r.FormValue("taxRate")), 64)
		if tax > 10 {
			// The client's tax edit box is bounded 0-10% (corp_dlg_editcorpdetails), so a higher stored value
			// clamps and rewrites itself on the next in-game edit.
			result = "Refused: the client's tax field is capped at 10% — a higher value clamps and rewrites itself on the next in-game edit."
			break
		}
		in := evedb.ESPCorpEdit{
			Name:         strings.TrimSpace(r.FormValue("corporationName")),
			Ticker:       strings.TrimSpace(r.FormValue("tickerName")),
			Description:  r.FormValue("description"),
			URL:          strings.TrimSpace(r.FormValue("url")),
			TaxRate:      tax / 100, // the form is a percentage, the column is a fraction
			CEOID:        espFormInt(r, "ceoID"),
			StationID:    espFormInt(r, "stationID"),
			MemberLimit:  espFormInt(r, "memberLimit"),
			AllowedRaces: espFormInt(r, "allowedMemberRaceIDs"),
		}
		for i := 0; i < 7; i++ {
			in.Divisions[i] = strings.TrimSpace(r.FormValue(fmt.Sprintf("division%d", i+1)))
		}
		if err := evedb.ESPUpdateCorp(ctx, s.db, corpID, in); err != nil {
			result = "Error: " + err.Error()
			break
		}
		notifyCorpChanged(corpID, before.CorporationName, in.Name, []CorpFieldChange{
			{Column: "tickerName", Old: before.TickerName, New: in.Ticker},
			{Column: "description", Old: before.Description, New: in.Description},
			{Column: "url", Old: before.URL, New: in.URL},
			{Column: "taxRate", Old: before.TaxRate, New: in.TaxRate},
			{Column: "ceoID", Old: before.CEOID, New: in.CEOID},
			{Column: "stationID", Old: before.StationID, New: in.StationID},
			{Column: "memberLimit", Old: before.MemberLimit, New: in.MemberLimit},
			{Column: "allowedMemberRaceIDs", Old: before.AllowedRaces, New: in.AllowedRaces},
		})
		if before.CorporationName != in.Name {
			invalidateOwnerNames()
		}
		notifyCorpHQChanged(corpID, before.StationID, in.StationID)
		result = "Corporation saved."

	case "logo":
		before, err := evedb.ESPGetCorpFull(ctx, s.db, corpID)
		if err != nil {
			result = "Error: " + err.Error()
			break
		}
		// Range-check before storing: an eveGraphics ID with no row is a KeyError in uix.GetCorpLogo.
		// Shapes must be in the shipped-art band, colours among the 15 real rows, and not Thin Black (682).
		var vals [6]sql.NullInt64
		badLogo := ""
		for i, field := range []string{"shape1", "shape2", "shape3", "color1", "color2", "color3"} {
			v := espFormInt(r, field)
			if v <= 0 {
				continue
			}
			if i < 3 {
				if v < evedb.LogoShapeMinID || v > evedb.LogoShapeMaxID {
					badLogo = fmt.Sprintf("shape %d is outside the client's shipped art (%d–%d)",
						v, evedb.LogoShapeMinID, evedb.LogoShapeMaxID)
					break
				}
			} else {
				if v < evedb.LogoColorMinID || v > evedb.LogoColorMaxID {
					badLogo = fmt.Sprintf("colour %d is outside the client's shipped art (%d–%d)",
						v, evedb.LogoColorMinID, evedb.LogoColorMaxID)
					break
				}
				if v == 682 {
					badLogo = "Thin Black (682) carries no diffuse — the client cannot paint with it"
					break
				}
			}
			vals[i] = sql.NullInt64{Int64: v, Valid: true}
		}
		if badLogo != "" {
			result = "Refused: " + badLogo + "."
			break
		}
		old, uerr := evedb.UpdateCorpLogo(ctx, s.db, corpID, vals)
		if uerr != nil {
			result = "Error: " + uerr.Error()
			break
		}
		cols := []string{"shape1", "shape2", "shape3", "color1", "color2", "color3"}
		changes := make([]CorpFieldChange, 0, 6)
		nv := func(v sql.NullInt64) any {
			if !v.Valid {
				return nil
			}
			return v.Int64
		}
		for i, col := range cols {
			changes = append(changes, CorpFieldChange{Column: col, Old: nv(old[i]), New: nv(vals[i])})
		}
		notifyCorpChanged(corpID, before.CorporationName, before.CorporationName, changes)
		result = "Logo saved."

	case "alliance":
		allianceID := espFormInt(r, "allianceID")
		old, err := evedb.ESPSetCorpAlliance(ctx, s.db, corpID, allianceID)
		if err != nil {
			result = "Error: " + err.Error()
			break
		}
		notifyAllianceMembership(corpID, old, allianceID)
		if allianceID > 0 {
			result = "Corporation joined the alliance; its online members were told."
		} else {
			result = "Corporation left its alliance; its online members were told."
		}

	case "issue-shares":
		// New shares go into the treasury, exactly as a passed "Create
		// Shares" vote puts them there.
		qty := espFormInt(r, "qty")
		issue, err := evedb.IssueCorpShares(ctx, s.db, corpID, qty)
		if err != nil {
			result = "Error: " + err.Error()
			break
		}
		notifyCorpSharesChanged(corpID,
			[]evedb.ShareDelta{{HolderID: corpID, Old: issue.OldTreasury, New: issue.NewTreasury}},
			issue.OldTotal, issue.NewTotal)
		result = fmt.Sprintf("Issued %d shares into the treasury (%d → %d issued).", qty, issue.OldTotal, issue.NewTotal)

	case "grant-shares":
		// Out of the treasury to any pilot or player corporation, the GM half of the corp wallet's Give
		// dialog.
		qty := espFormInt(r, "qty")
		to, err := evedb.ResolveShareOwner(ctx, s.db, strings.TrimSpace(r.FormValue("owner")))
		if err != nil {
			result = "Refused: " + err.Error() + "."
			break
		}
		if to == corpID {
			result = "Refused: the treasury already holds them."
			break
		}
		move, err := evedb.MoveShares(ctx, s.db, corpID, corpID, to, qty)
		if err != nil {
			result = "Error: " + err.Error()
			break
		}
		notifyCorpSharesChanged(corpID, move.Deltas(corpID, to), 0, 0)
		result = fmt.Sprintf("Granted %d shares from the treasury to %d (treasury %d → %d).",
			qty, to, move.FromOld, move.FromNew)

	case "delete":
		if strings.TrimSpace(r.FormValue("confirm")) != "DELETE" {
			result = "Type DELETE to confirm."
			break
		}
		res, err := evedb.ESPDeleteCorp(ctx, s.db, corpID)
		if err != nil {
			result = "Error: " + err.Error()
			break
		}
		if CorpDeletedHook != nil {
			CorpDeletedHook(corpID, res.OldAllianceID, res.Kicked)
		}
		if res.OldAllianceID > 0 {
			notifyAllianceEvent("member-removed", res.OldAllianceID, corpID)
		}
		result = fmt.Sprintf(
			"Corporation %q closed: %d pilot(s) moved to their NPC corps, %d war(s) finished, %d bill(s) written off.",
			res.Name, len(res.Kicked), res.WarsFinished, res.BillsWrittenOff)
		if res.ItemsLeft > 0 {
			result += fmt.Sprintf(" %d item(s) still owned by the corp were left in place.", res.ItemsLeft)
		}
		dest = "/corporations"

	default:
		result = "Unknown action."
	}

	http.Redirect(w, r, dest+"?msg="+url.QueryEscape(result), http.StatusSeeOther)
}
