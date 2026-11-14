package esp

// In-game browser profile pages: /concord/capsuleer?id=N and /concord/corporation?id=N.
// They render the public record only and are readable with no authentication.
// See docs/galnet-igb.md#public-profile-pages.

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

const (
	igbProfileFeed  = 25 // recent incidents in the combined list
	igbProfileTop   = 6  // rows per breakdown table
	igbProfileOpp   = 8  // rows per opponent table
	igbActivityDays = 14
)

// igbCorpLogo renders a corporation's logo from the client's own paintshop
// compositor. The corp window draws the same thing from the same ID.
func igbCorpLogo(corpID int64, size int) string {
	if corpID == 0 {
		return ""
	}
	return `<img src="corplogo:` + strconv.FormatInt(corpID, 10) + `" size=` + strconv.Itoa(size) + `>`
}

// igbPilotLink and igbCorpLink are the cross-links every table carries. Both give plain text for
// an ID of 0, which is what an NPC attacker looks like.
func igbPilotLink(charID int64, name string) string {
	if name == "" {
		name = "Unknown"
	}
	if charID == 0 {
		return htmlTextEscape(name)
	}
	return `<a href="` + newsHost + `/concord/capsuleer?id=` + strconv.FormatInt(charID, 10) +
		`" alt="` + htmlAttrEscape("DED dossier for "+name) + `">` + htmlTextEscape(name) + `</a>`
}

func igbCorpLink(corpID int64, name string) string {
	if name == "" {
		return "&mdash;"
	}
	if corpID == 0 {
		return htmlTextEscape(name)
	}
	return `<a href="` + newsHost + `/concord/corporation?id=` + strconv.FormatInt(corpID, 10) +
		`" alt="` + htmlAttrEscape("DED dossier for "+name) + `">` + htmlTextEscape(name) + `</a>`
}

// igbBar draws a proportion as repeated pipes in two colours, the only bar this renderer draws
// reliably. width is in characters.
func igbBar(frac float64, width int, color string) string {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	filled := int(float64(width)*frac + 0.5)
	if filled > width {
		filled = width
	}
	return "<font color=" + color + "><b>" + strings.Repeat("|", filled) + "</b></font>" +
		"<font color=#3a3a3a><b>" + strings.Repeat("|", width-filled) + "</b></font>"
}

// ── pilot page ───────────────────────────────────────────────────────────────

// handleConcordCapsuleer serves /concord/capsuleer?id=N. With no id it falls back to the calling
// pilot's own record.
func (s *NewsServer) handleConcordCapsuleer(w http.ResponseWriter, r *http.Request) {
	who := igbIdentity(r)
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	charID, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if charID == 0 {
		charID = who.ID
	}
	log.Printf("igb: pilot profile id=%d from %s char=%q (%d)", charID, r.RemoteAddr, who.Name, who.ID)

	var b strings.Builder
	if charID == 0 {
		writeConcordHeader(&b, "", igbCrumb{"Registry", newsHost + "/concord/"},
			igbCrumb{Label: "Capsuleer dossier"})
		b.WriteString("<font size=3>No capsuleer selected, and this terminal sent no identity.</font><br>\n")
		finishSitePage(w, &b, siteConcord, 0)
		return
	}

	id, err := evedb.GetKillProfileIdentity(ctx, s.db, charID)
	// A failed lookup must not be reported as a deleted pilot.
	lookupFailed := err != nil
	if err != nil {
		log.Printf("igb: GetKillProfileIdentity %d: %v", charID, err)
		id = &evedb.KillProfileIdentity{CharID: charID}
	}
	// A pilot absent from the character table may still have killmails; recover the header from
	// them.
	evedb.FillIdentityFromKillboard(ctx, s.db, id)

	actor := evedb.PilotActor(charID)
	stats, err := evedb.ActorKillStats(ctx, s.db, actor, time.Time{})
	if err != nil {
		log.Printf("igb: ActorKillStats char=%d: %v", charID, err)
		stats = &evedb.KillActorStats{}
	}

	name := id.Name
	if name == "" {
		name = "Capsuleer " + strconv.FormatInt(charID, 10)
	}

	// ── header ───────────────────────────────────────────────────────────────
	crumbs := []igbCrumb{{"Registry", newsHost + "/concord/"}}
	if id.CorpID != 0 && id.CorpName != "" {
		crumbs = append(crumbs, igbCrumb{htmlTextEscape(id.CorpName),
			newsHost + "/concord/corporation?id=" + strconv.FormatInt(id.CorpID, 10)})
	}
	crumbs = append(crumbs, igbCrumb{Label: htmlTextEscape(name)})
	writeConcordHeader(&b, "", crumbs...)

	b.WriteString("<font size=6 color=" + cdTitle + "><b>" + htmlTextEscape(name) +
		"</b></font><br>\n")
	kv := []igbKVPair{{"CORPORATION", igbCorpLogo(id.CorpID, 32) + " " + igbCorpLink(id.CorpID, id.CorpName)}}
	if id.AllianceName != "" {
		kv = append(kv, igbKVPair{"ALLIANCE", htmlTextEscape(id.AllianceName)})
	}
	if id.Exists {
		kv = append(kv, igbKVPair{"SECURITY STATUS", "<font color=" + igbSecColor(id.SecStatus) + ">" +
			strconv.FormatFloat(id.SecStatus, 'f', 2, 64) + "</font>"})
	} else if !lookupFailed {
		kv = append(kv, igbKVPair{"STATUS", "<font color=" + cdDim + ">capsuleer deleted &mdash; incident record kept</font>"})
	}
	igbWriteDossierHeader(&b, igbPortrait(charID, 128), kv, nil, s.dossierStats(ctx, actor, stats))
	b.WriteString("<br>\n")

	if !stats.HasRecord() {
		b.WriteString("<font size=3>The registry holds no incidents involving this capsuleer.</font><br><br>\n")
		b.WriteString(`<font size=2><a href="` + newsHost + `/concord/">&laquo; back to the registry</a></font>` + "\n")
		finishSitePage(w, &b, siteConcord, 0)
		return
	}

	s.writeIGBProfileBody(ctx, &b, actor, charID, 0)

	b.WriteString(`<hr><font size=2><a href="` + newsHost + `/concord/">&laquo; registry</a>`)
	b.WriteString(` &nbsp;|&nbsp; <a href="` + concordFilter{NPC: true, Involving: charID}.url() +
		`">every incident this capsuleer is party to</a>`)
	b.WriteString("</font>\n")
	finishSitePage(w, &b, siteConcord, 0)
}

// ── corporation page ─────────────────────────────────────────────────────────

// handleConcordCorporation serves /concord/corporation?id=N, defaulting to the caller's own
// corp.
func (s *NewsServer) handleConcordCorporation(w http.ResponseWriter, r *http.Request) {
	who := igbIdentity(r)
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	corpID, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if corpID == 0 && who.ID != 0 {
		// The browser sends a corp name, not an ID, so the caller's corp is resolved through the
		// character.
		if id, err := evedb.GetKillProfileIdentity(ctx, s.db, who.ID); err == nil {
			corpID = id.CorpID
		}
	}
	log.Printf("igb: corp profile id=%d from %s char=%q (%d)", corpID, r.RemoteAddr, who.Name, who.ID)

	var b strings.Builder
	if corpID == 0 {
		writeConcordHeader(&b, "", igbCrumb{"Registry", newsHost + "/concord/"},
			igbCrumb{Label: "Corporation dossier"})
		b.WriteString("<font size=3>No corporation selected.</font><br>\n")
		finishSitePage(w, &b, siteConcord, 0)
		return
	}

	corp, err := evedb.ESPGetCorpFull(ctx, s.db, corpID)
	lookupFailed := err != nil // an error is not a dissolved corporation
	if err != nil {
		log.Printf("igb: ESPGetCorpFull %d: %v", corpID, err)
	}

	actor := evedb.CorpActor(corpID)
	stats, err := evedb.ActorKillStats(ctx, s.db, actor, time.Time{})
	if err != nil {
		log.Printf("igb: ActorKillStats corp=%d: %v", corpID, err)
		stats = &evedb.KillActorStats{}
	}

	name := "Corporation " + strconv.FormatInt(corpID, 10)
	if corp != nil && corp.CorporationName != "" {
		name = corp.CorporationName
	}
	title := name
	if corp != nil && corp.TickerName != "" {
		title += " [" + corp.TickerName + "]"
	}

	writeConcordHeader(&b, "", igbCrumb{"Registry", newsHost + "/concord/"},
		igbCrumb{Label: htmlTextEscape(name)})

	b.WriteString("<font size=6 color=" + cdTitle + "><b>" + htmlTextEscape(title) +
		"</b></font><br>\n")
	var kv []igbKVPair
	if corp != nil {
		if corp.AllianceName != "" {
			kv = append(kv, igbKVPair{"ALLIANCE", htmlTextEscape(corp.AllianceName)})
		}
		if corp.CEOName != "" {
			kv = append(kv, igbKVPair{"CEO", igbPilotLink(corp.CEOID, corp.CEOName)})
		}
		// Pilots with an account. corporation.memberCount is stored and never maintained.
		kv = append(kv, igbKVPair{"CAPSULEERS", igbInt(corp.Pilots)},
			igbKVPair{"TAX RATE", strconv.FormatFloat(corp.TaxPct(), 'f', 1, 64) + "%"})
		if corp.IsNPC {
			kv = append(kv, igbKVPair{"TYPE", "<font color=" + cdDim + ">Chartered corporation</font>"})
		}
		if corp.StationName != "" {
			kv = append(kv, igbKVPair{"HEADQUARTERS", htmlTextEscape(corp.StationName)})
		}
	} else if !lookupFailed {
		kv = append(kv, igbKVPair{"STATUS", "<font color=" + cdDim + ">corporation dissolved &mdash; incident record kept</font>"})
	}

	// Loaded before the cell is written: the empty case needs the headcount.
	topKillers, err := evedb.CorpTopPilots(ctx, s.db, corpID, time.Time{}, 6)
	if err != nil {
		log.Printf("igb: CorpTopPilots %d: %v", corpID, err)
	}
	var headcount int64
	if corp != nil {
		headcount = corp.Pilots
	}
	igbWriteDossierHeader(&b, igbCorpLogo(corpID, 128), kv, func(b *strings.Builder, rowspan int) {
		igbTopKillersCell(b, topKillers, headcount, rowspan)
	}, s.dossierStats(ctx, actor, stats))

	if corp != nil && strings.TrimSpace(corp.Description) != "" {
		b.WriteString("<font size=2 color=" + cdDim + ">" +
			htmlTextEscape(igbTrim(corp.Description, 400)) + "</font><br>\n")
	}
	b.WriteString("<br>\n")

	if !stats.HasRecord() {
		b.WriteString("<font size=3>The registry holds no incidents involving this corporation.</font><br><br>\n")
		b.WriteString(`<font size=2><a href="` + newsHost + `/concord/">&laquo; back to the registry</a></font>` + "\n")
		finishSitePage(w, &b, siteConcord, 0)
		return
	}

	s.writeIGBProfileBody(ctx, &b, actor, 0, corpID)

	b.WriteString(`<hr><font size=2><a href="` + newsHost + `/concord/">&laquo; registry</a></font>` + "\n")
	finishSitePage(w, &b, siteConcord, 0)
}

// ── shared rendering ─────────────────────────────────────────────────────────

// writeIGBProfileBody renders everything below the header for either actor. charID and corpID
// build the recent feeds, which filter on different columns for the two cases.
func (s *NewsServer) writeIGBProfileBody(ctx context.Context, b *strings.Builder,
	actor evedb.KillActor, charID, corpID int64) {

	// Each section is skipped on its own error, so a failing aggregate costs only its own table.
	if act, err := evedb.ActorActivity(ctx, s.db, actor, igbActivityDays); err == nil {
		igbWriteActivity(b, act)
	}

	// The breakdowns go two abreast. An empty panel is dropped before pairing, so the page has no
	// hole.
	typePanel := func(title string, mode evedb.KillTypeMode) *igbBandPanel {
		rows, err := evedb.ActorTypeBreakdown(ctx, s.db, actor, mode, time.Time{}, igbProfileTop)
		if err != nil {
			return nil
		}
		if pn, ok := igbTypePanel(title, rows); ok {
			return &pn
		}
		return nil
	}
	oppPanel := func(title string, load func(context.Context, *sql.DB, evedb.KillActor, time.Time, int) ([]evedb.KillOpponent, error)) *igbBandPanel {
		rows, err := load(ctx, s.db, actor, time.Time{}, igbProfileOpp)
		if err != nil {
			return nil
		}
		if pn, ok := igbOpponentsPanel(title, rows); ok {
			return &pn
		}
		return nil
	}
	engaged := typePanel("Ships engaged", evedb.TypeShipsKilled)
	lostShips := typePanel("Ships lost", evedb.TypeShipsLost)
	weapons := typePanel("Weapons used", evedb.TypeWeaponsUsed)
	victims := oppPanel("Most engaged", evedb.ActorTopVictims)
	nemeses := oppPanel("Killed most by", evedb.ActorTopNemeses)
	var systems *igbBandPanel
	if sys, err := evedb.ActorTopSystems(ctx, s.db, actor, time.Time{}, igbProfileOpp); err == nil {
		if pn, ok := igbSystemsPanel("Top systems for kills", sys); ok {
			systems = &pn
		}
	}
	var locations *igbBandPanel
	if loc, err := evedb.ActorTopLocations(ctx, s.db, actor, time.Time{}, igbProfileOpp); err == nil {
		if pn, ok := igbLocationsPanel("Location", loc); ok {
			locations = &pn
		}
	}
	collect := func(ps ...*igbBandPanel) []igbBandPanel {
		var out []igbBandPanel
		for _, p := range ps {
			if p != nil {
				out = append(out, *p)
			}
		}
		return out
	}

	bands := igbPairBands(collect(engaged, lostShips, victims, nemeses, systems, locations, weapons))
	// One table for every band, so they share a column grid.
	writePanelGrid(b, siteConcord, bands)
	b.WriteString("<br>\n")

	kills, _ := evedb.ListKillmails(ctx, s.db, evedb.KillmailFilter{
		Involving: charID, InvolvingCorp: corpID, IncludeNPCKills: true, Limit: igbProfileFeed,
	})
	// The involving filter is attacker-or-victim for a character, so drop the pilot's own losses.
	if charID != 0 {
		kills = igbDropLossesOf(kills, charID)
	}
	losses, _ := evedb.ListKillmails(ctx, s.db, evedb.KillmailFilter{
		VictimCharID: charID, VictimCorpID: corpID, IncludeNPCKills: true, Limit: igbProfileFeed,
	})

	// One list in the front page's row style. Both queries take the newest igbProfileFeed, so the
	// newest igbProfileFeed of their union is exact.
	rows, lost := igbMergeIncidents(kills, losses)
	igbSectionTitle(b, "Recent incidents")
	b.WriteString("<font size=2 color=" + cdDim + ">Kills are <font color=" + cdGreen + ">green</font>, losses <font color=" +
		cdRed + ">red</font>.</font><br>\n")
	if len(rows) == 0 {
		b.WriteString("<font size=2 color=" + cdDim + ">Nothing on record.</font><br><br>\n")
		return
	}
	writeIncidentRowsTinted(b, rows, func(k evedb.KillmailRow) int {
		if lost[k.KillID] {
			return -1
		}
		return 1
	})
}

// igbMergeIncidents folds a profile's kills and losses into one list, newest first, capped at
// igbProfileFeed. lost reports which killmails were losses. A mail in both lists is listed once,
// as a loss.
func igbMergeIncidents(kills, losses []evedb.KillmailRow) (rows []evedb.KillmailRow, lost map[int64]bool) {
	lost = make(map[int64]bool, len(losses))
	seen := make(map[int64]bool, len(kills)+len(losses))
	for _, k := range losses {
		if seen[k.KillID] {
			continue
		}
		seen[k.KillID] = true
		lost[k.KillID] = true
		rows = append(rows, k)
	}
	for _, k := range kills {
		if !seen[k.KillID] {
			seen[k.KillID] = true
			rows = append(rows, k)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if !rows[i].KillTime.Equal(rows[j].KillTime) {
			return rows[i].KillTime.After(rows[j].KillTime)
		}
		return rows[i].KillID > rows[j].KillID
	})
	if len(rows) > igbProfileFeed {
		rows = rows[:igbProfileFeed]
	}
	return rows, lost
}

// igbDropLossesOf removes mails where this pilot is the victim. KillmailFilter.Involving covers
// both ends and cannot express it.
func igbDropLossesOf(rows []evedb.KillmailRow, charID int64) []evedb.KillmailRow {
	out := rows[:0]
	for _, k := range rows {
		if k.VictimCharID != charID {
			out = append(out, k)
		}
	}
	return out
}

// dossierStats loads the header grid's two windows beside the lifetime record. Each window is its
// own query and falls back to nil (dashes) on error.
func (s *NewsServer) dossierStats(ctx context.Context, actor evedb.KillActor,
	life *evedb.KillActorStats) igbDossierStats {

	st := igbDossierStats{Lifetime: life}
	now := time.Now()
	if w, err := evedb.ActorKillStats(ctx, s.db, actor, now.AddDate(0, 0, -90)); err == nil {
		st.Days90 = w
	}
	if w, err := evedb.ActorKillStats(ctx, s.db, actor, now.AddDate(0, 0, -7)); err == nil {
		st.Days7 = w
	}
	return st
}

// igbTopKillersCell fills the middle of the corporation header with the corp's own leaderboard,
// compact enough for a 250 px cell. The empty case names its reason: most corporations have no
// killers, and a blank cell reads as a failed load.
func igbTopKillersCell(b *strings.Builder, pilots []evedb.KillboardLeader, pilotCount int64, rowspan int) {
	b.WriteString("<td rowspan=" + strconv.Itoa(rowspan) + " bgcolor=" + cdPanel + " valign=top width=" +
		strconv.Itoa(igbDossierMiddleW) + ">")
	b.WriteString("<font size=3 color=" + cdTitle + "><b>Most active</b></font><br>")

	if len(pilots) == 0 {
		if pilotCount > 0 {
			b.WriteString("<font size=3 color=" + cdDim + ">None of the " + igbInt(pilotCount) +
				" capsuleers here is party to a recorded incident.</font>")
		} else {
			b.WriteString("<font size=3 color=" + cdDim + ">No incidents on record.</font>")
		}
		b.WriteString("</td>")
		return
	}

	for i, p := range pilots {
		name := p.Name
		if name == "" {
			name = "Capsuleer " + strconv.FormatInt(p.CharID, 10)
		}
		b.WriteString(igbPortrait(p.CharID, 32))
		b.WriteString(" <font size=3 color=" + cdDim + ">" + strconv.Itoa(i+1) + ".</font> ")
		b.WriteString("<font size=3>" + igbPilotLink(p.CharID, name) + "</font> ")
		b.WriteString("<font size=3 color=" + cdHi + "><b>" + igbInt(p.Count) + "</b></font>")
		b.WriteString("<font size=2 color=" + cdDim + "> incidents</font><br>")
	}
	b.WriteString("</td>")
}

func igbSectionTitle(b *strings.Builder, title string) {
	b.WriteString("<font size=3><b><font color=" + cdTitle + ">" + title +
		"</font></b></font><br>\n")
}

// igbWriteActivity draws the last fortnight as two pipe bars a day, kills over losses, scaled to
// the busiest day in the window.
func igbWriteActivity(b *strings.Builder, days []evedb.KillDayCount) {
	var max int64
	var any bool
	for _, d := range days {
		if d.Kills > max {
			max = d.Kills
		}
		if d.Losses > max {
			max = d.Losses
		}
		if d.Kills > 0 || d.Losses > 0 {
			any = true
		}
	}
	if !any {
		return
	}
	igbSectionTitle(b, "Last "+strconv.Itoa(len(days))+" days")
	b.WriteString("<table width=100%>\n")
	for i, d := range days {
		shade := cdRowA
		if i%2 == 1 {
			shade = cdRowB
		}
		td := "<td bgcolor=" + shade + ">"
		b.WriteString("<tr><td width=90 bgcolor=" + shade + "><font size=2 color=" + cdDim + ">" +
			d.Day.Format("01.02 Mon") + "</font></td>")
		b.WriteString(td + "<font size=2>" + igbBar(float64(d.Kills)/float64(max), 24, cdGreen) +
			" <font color=" + cdDim + ">" + igbInt(d.Kills) + "</font></font></td>")
		b.WriteString(td + "<font size=2>" + igbBar(float64(d.Losses)/float64(max), 24, cdRed) +
			" <font color=" + cdDim + ">" + igbInt(d.Losses) + "</font></font></td></tr>\n")
	}
	b.WriteString("</table><br>\n")
}

// igbPairBands groups panels two to a band, for writePanelGrid: a pair of lists
// reads side by side without a nested table. An odd last panel takes a band of
// its own.
func igbPairBands(panels []igbBandPanel) [][]igbBandPanel {
	var bands [][]igbBandPanel
	for i := 0; i < len(panels); i += 2 {
		bands = append(bands, panels[i:min(i+2, len(panels))])
	}
	return bands
}

// The column grid every breakdown panel shares (writePanelGrid): icon, name, bar, count, ISK.
// See docs/galnet-igb.md#shared-panel-grid.
var (
	igbGridAlign = []string{"", "", "", "right", "right"}
	igbGridWidth = []string{"36", "", "64", "40", ""}
)

// igbTypePanel builds one ship/weapon breakdown with the client's own icon for every row. ok is
// false when there is nothing to show. The bar is 10 pips to fit half a window.
func igbTypePanel(title string, rows []evedb.KillTypeCount) (igbBandPanel, bool) {
	if len(rows) == 0 {
		return igbBandPanel{}, false
	}
	var top int64
	for _, r := range rows {
		if r.Count > top {
			top = r.Count
		}
	}
	pn := igbBandPanel{Title: title, Align: igbGridAlign, Width: igbGridWidth}
	for _, r := range rows {
		name := r.TypeName
		if name == "" {
			name = "Type " + strconv.FormatInt(r.TypeID, 10)
		}
		pn.Rows = append(pn.Rows, []string{
			igbShipIcon(r.TypeID, 32),
			"<font size=3>" + htmlTextEscape(name) + "</font>",
			"<font size=2>" + igbBar(float64(r.Count)/float64(top), 10, cdHi) + "</font>",
			"<font size=2 color=" + cdHi + ">" + igbInt(r.Count) + "</font>",
			"<font size=2 color=" + cdGreen + ">" + igbISK(r.ISK) + "</font>",
		})
	}
	return pn, true
}

// igbOpponentsPanel builds a "most killed" / "killed most by" panel. ok is false when there is
// nothing to show. An NPC row shows its ship icon and no link.
// See docs/galnet-igb.md#public-profile-pages.
func igbOpponentsPanel(title string, rows []evedb.KillOpponent) (igbBandPanel, bool) {
	if len(rows) == 0 {
		return igbBandPanel{}, false
	}
	var top int64
	for _, o := range rows {
		top = max(top, o.Count)
	}
	pn := igbBandPanel{Title: title, Align: igbGridAlign, Width: igbGridWidth}
	for _, o := range rows {
		face := igbPortrait(o.CharID, 32)
		if o.IsNPC || o.CharID == 0 {
			face = igbShipIcon(o.ShipID, 32)
		}
		name := igbPilotLink(o.CharID, o.Name)
		if o.IsNPC {
			name += ` <font size=2 color=` + cdDim + `>[hostile entity]</font>`
		}
		pn.Rows = append(pn.Rows, []string{
			face,
			"<font size=3>" + name + "</font><br><font size=2 color=" + cdDim + ">" +
				igbCorpLink(o.CorpID, o.CorpName) + "</font>",
			"<font size=2>" + igbBar(float64(o.Count)/float64(top), 10, cdHi) + "</font>",
			"<font size=2 color=" + cdHi + ">" + igbInt(o.Count) + "</font><font size=2 color=" + cdDim + ">x</font>",
			"<font size=2 color=" + cdGreen + ">" + igbISK(o.ISK) + "</font>",
		})
	}
	return pn, true
}

// igbSystemsPanel builds the "where they score kills" panel: the system's
// security in its tint where the icon would be, then the system (linked to its
// record) over its region. ok is false when there is nothing to show.
func igbSystemsPanel(title string, rows []evedb.KillSystemCount) (igbBandPanel, bool) {
	if len(rows) == 0 {
		return igbBandPanel{}, false
	}
	var top int64
	for _, r := range rows {
		top = max(top, r.Count)
	}
	pn := igbBandPanel{Title: title, Align: igbGridAlign, Width: igbGridWidth}
	for _, r := range rows {
		name := "<font size=3>" + concordSystemLink(r.SystemID, r.Name) + "</font>"
		if r.RegionName != "" {
			name += "<br><font size=2 color=" + cdDim + ">" + igbText(r.RegionName) + "</font>"
		}
		pn.Rows = append(pn.Rows, []string{
			"<font size=3 color=" + igbSecColor(r.Security) + "><b>" +
				strconv.FormatFloat(r.Security, 'f', 1, 64) + "</b></font>",
			name,
			"<font size=2>" + igbBar(float64(r.Count)/float64(top), 10, cdHi) + "</font>",
			"<font size=2 color=" + cdHi + ">" + igbInt(r.Count) + "</font>",
			"<font size=2 color=" + cdGreen + ">" + igbISK(r.ISK) + "</font>",
		})
	}
	return pn, true
}

// igbLocationTag is the short label standing where the icon would, for the kind
// of place a kill happened at. No per-celestial icon is assumed to exist in the
// client's atlas, so the kind is spelled out in a few letters.
func igbLocationTag(groupID int64) string {
	switch groupID {
	case 10:
		return "GATE"
	case 15:
		return "STN"
	case 9:
		return "BELT"
	case 7:
		return "PLAN"
	case 8:
		return "MOON"
	case 6:
		return "STAR"
	}
	return "DEEP"
}

// igbLocationsPanel builds the "where in the system" panel: the kind of place in
// the icon column, the place's own name over its system, then the count and ISK.
// ok is false when there is nothing to show.
func igbLocationsPanel(title string, rows []evedb.KillLocationCount) (igbBandPanel, bool) {
	if len(rows) == 0 {
		return igbBandPanel{}, false
	}
	var top int64
	for _, r := range rows {
		top = max(top, r.Count)
	}
	pn := igbBandPanel{Title: title, Align: igbGridAlign, Width: igbGridWidth}
	for _, r := range rows {
		name := r.Name
		if name == "" {
			name = evedb.KillLocationDeepSpace
		}
		sys := concordSystemLink(r.SystemID, r.SystemName)
		pn.Rows = append(pn.Rows, []string{
			"<font size=2 color=" + cdDim + "><b>" + igbLocationTag(r.GroupID) + "</b></font>",
			"<font size=3>" + igbText(name) + "</font><br><font size=2>" + sys + "</font>",
			"<font size=2>" + igbBar(float64(r.Count)/float64(top), 10, cdHi) + "</font>",
			"<font size=2 color=" + cdHi + ">" + igbInt(r.Count) + "</font>",
			"<font size=2 color=" + cdGreen + ">" + igbISK(r.ISK) + "</font>",
		})
	}
	return pn, true
}

// igbTrim shortens a body of user-authored text to something a panel can hold,
// cutting on a word boundary when there is one nearby.
func igbTrim(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	if i := strings.LastIndexByte(cut, ' '); i > max/2 {
		cut = cut[:i]
	}
	return cut + "..."
}
