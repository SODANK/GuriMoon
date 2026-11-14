package esp

// CONCORD's Capsuleer Incident Registry, kept by the Directive Enforcement Department. Routes:
// /concord/ (registry), incident?id, capsuleer?id, corporation?id, system?id, battles, battle?k,
// wars, war?id. Old aliases: /ig/kb, /ig/kill, /ig/pilot, /ig/corp.
// Front-page params: type=ship|pod, mine=1, npc=1 (include hostile-entity engagements), sys=<id>,
// involving=<charID>, page=<n>. Wording: docs/galnet-igb.md#wording.

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

// The registry's palette: a near-black body, a police-blue band, red for what was destroyed and
// green for what was recovered. Constants because igbprofile.go and igbkillboard.go build strings
// from them.
const (
	cdPage   = "#06070a"
	cdPanel  = "#10141b"
	cdRowA   = "#0b0e13"
	cdRowB   = "#11151c"
	cdHeader = "#0c2c66" // the blue band
	cdNow    = "#16294a" // the row a panel is about: the decisive strike, the top loss
	cdText   = "#dfe5ee"
	cdLink   = "#ffffff"
	cdTitle  = "#ffffff"
	cdDim    = "#7b8696"
	cdHi     = "#5c9bff" // counts and figures: light-bar blue
	cdGreen  = "#5fbf7a"
	cdRed    = "#ff4040" // siren red
)

// The registry is laid out flat: one band of side-by-side leaderboards (writePanelBand), then
// the incident list at full width. See docs/galnet-igb.md#browser-limits.

const (
	concordSidebarRows = 6
	concordSystemFeed  = 25
)

func concordIncidentURL(id int64) string {
	return newsHost + "/concord/incident?id=" + strconv.FormatInt(id, 10)
}
func concordCorpURL(id int64) string {
	return newsHost + "/concord/corporation?id=" + strconv.FormatInt(id, 10)
}
func concordSystemURL(id int64) string {
	return newsHost + "/concord/system?id=" + strconv.FormatInt(id, 10)
}

// concordPilotLink is a capsuleer's name linked to their dossier; plain text
// for a hostile entity (charID 0 has no dossier).
func concordPilotLink(charID int64, name string) string { return igbPilotLink(charID, name) }

// concordSystemLink is a system's name linked to its record.
func concordSystemLink(id int64, name string) string {
	if name == "" {
		name = "Unknown system"
	}
	if id == 0 {
		return igbText(name)
	}
	return `<a href="` + concordSystemURL(id) + `" alt="` + htmlAttrEscape("DED record for "+name) + `">` +
		igbText(name) + `</a>`
}

// concordCorpLogo is a corporation's logo linked to its dossier.
func concordCorpLogo(corpID int64, size int) string {
	if corpID == 0 {
		return ""
	}
	return `<a href="` + concordCorpURL(corpID) + `">` + igbCorpLogo(corpID, size) + `</a>`
}

// concordSec is a security status in its colour.
func concordSec(sec float64) string {
	return `<font color=` + igbSecColor(sec) + `>` + strconv.FormatFloat(sec, 'f', 1, 64) + `</font>`
}

// concordFilter is the registry front page's state; every link on the page is
// built from one so a filtered view stays filtered when you page or switch.
type concordFilter struct {
	Kind      string // "" | "ship" | "pod"
	Mine      bool
	NPC       bool
	Sys       int64
	Involving int64
	Page      int
}

func (f concordFilter) url() string {
	q := url.Values{}
	if f.Kind != "" {
		q.Set("type", f.Kind)
	}
	if f.Mine {
		q.Set("mine", "1")
	}
	if f.NPC {
		q.Set("npc", "1")
	}
	if f.Sys != 0 {
		q.Set("sys", strconv.FormatInt(f.Sys, 10))
	}
	if f.Involving != 0 {
		q.Set("involving", strconv.FormatInt(f.Involving, 10))
	}
	if f.Page > 0 {
		q.Set("page", strconv.Itoa(f.Page))
	}
	u := newsHost + "/concord/"
	if enc := q.Encode(); enc != "" {
		u += "?" + strings.ReplaceAll(enc, "&", "&amp;")
	}
	return u
}

func (f concordFilter) front() bool {
	return f.Page == 0 && f.Kind == "" && !f.Mine && f.Sys == 0 && f.Involving == 0
}

// writeConcordHeader is the registry's top: palette, top bar, masthead, menu and, for a sub-page,
// the trail.
func writeConcordHeader(b *strings.Builder, active string, crumbs ...igbCrumb) {
	writeConcordHeaderFor(b, active, concordFilter{}, crumbs...)
}

func writeConcordHeaderFor(b *strings.Builder, active string, f concordFilter, crumbs ...igbCrumb) {
	site := siteConcord
	all, ships, pods := f, f, f
	all.Kind, ships.Kind, pods.Kind = "", "ship", "pod"
	all.Page, ships.Page, pods.Page = 0, 0, 0
	site.Nav = []igbNavItem{
		{"front", "All incidents", strings.TrimPrefix(all.url(), newsHost)},
		{"ships", "Ships", strings.TrimPrefix(ships.url(), newsHost)},
		{"capsules", "Capsules", strings.TrimPrefix(pods.url(), newsHost)},
		{"battles", "Battle Reports", "/concord/battles"},
		{"wars", "Wars", "/concord/wars"},
		{"sov", "Sovereignty", "/concord/sovereignty"},
	}
	writeSiteOpen(b, site, "DED")
	writeSiteTopBar(b, site, "")
	writeSiteMasthead(b, site, "<font size=2 color="+cdDim+">PUBLIC RECORD OF CAPSULEER LOSSES<br>ACROSS NEW EDEN</font>")
	writeSiteMenu(b, site, active)
	writeSiteCrumbs(b, site, crumbs...)
}

// ── the registry ────────────────────────────────────────────────────────────

func (s *NewsServer) handleConcord(w http.ResponseWriter, r *http.Request) {
	who := igbIdentity(r)
	log.Printf("igb: concord %s from %s char=%q (%d)", r.URL.RawQuery, r.RemoteAddr, who.Name, who.ID)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	q := r.URL.Query()
	f := concordFilter{Kind: q.Get("type"), Mine: q.Get("mine") == "1", NPC: q.Get("npc") == "1"}
	if f.Kind != "ship" && f.Kind != "pod" {
		f.Kind = ""
	}
	f.Sys, _ = strconv.ParseInt(q.Get("sys"), 10, 64)
	f.Involving, _ = strconv.ParseInt(q.Get("involving"), 10, 64)
	f.Page, _ = strconv.Atoi(q.Get("page"))
	if f.Page < 0 {
		f.Page = 0
	}
	noIdentity := false
	if f.Mine && who.ID == 0 {
		f.Mine, noIdentity = false, true
	}

	filter := evedb.KillmailFilter{
		SolarSystemID: f.Sys, Kind: f.Kind, IncludeNPCKills: f.NPC,
		Limit: igbKBPageSize, Offset: f.Page * igbKBPageSize,
	}
	switch {
	case f.Mine:
		filter.Involving = who.ID
	case f.Involving != 0:
		filter.Involving = f.Involving
	}

	active := map[string]string{"": "front", "ship": "ships", "pod": "capsules"}[f.Kind]
	var b strings.Builder
	writeConcordHeaderFor(&b, active, f)

	// Summary band: the last day and the last week, side by side.
	b.WriteString("<table width=100% bgcolor=" + cdPanel + "><tr>")
	for _, win := range []struct {
		label string
		d     time.Duration
	}{{"LAST 24 HOURS", 24 * time.Hour}, {"LAST 7 DAYS", 7 * 24 * time.Hour}} {
		n, isk, err := evedb.KillboardTotals(ctx, s.db, time.Now().Add(-win.d), f.NPC)
		b.WriteString("<td valign=top width=25%><font size=2 color=" + cdDim + ">" + win.label + "</font><br>")
		if err != nil {
			b.WriteString("<font size=3 color=" + cdDim + ">unavailable</font></td><td width=25%></td>")
			continue
		}
		b.WriteString("<font size=5 color=" + cdTitle + "><b>" + igbInt(n) + "</b></font> <font size=2 color=" + cdDim +
			">losses</font></td><td valign=top width=25%><font size=2 color=" + cdDim + ">ASSESSED LOSS</font><br>" +
			"<font size=5 color=" + cdHi + "><b>" + igbISKShort(isk) + "</b></font> <font size=2 color=" + cdDim +
			">ISK</font></td>")
	}
	b.WriteString("</tr></table>\n")

	// Filters.
	b.WriteString("<table width=100% bgcolor=" + cdHeader + "><tr><td><font size=3>&nbsp;")
	all := f
	all.Kind, all.Page = "", 0
	ships, pods := all, all
	ships.Kind, pods.Kind = "ship", "pod"
	b.WriteString(igbFilterCell("All", all.url(), f.Kind == ""))
	b.WriteString(" &middot; " + igbFilterCell("Ships", ships.url(), f.Kind == "ship"))
	b.WriteString(" &middot; " + igbFilterCell("Capsules", pods.url(), f.Kind == "pod"))
	if who.ID != 0 {
		mine := f
		mine.Mine, mine.Involving, mine.Page = !f.Mine, 0, 0
		b.WriteString(" &nbsp;&nbsp; " + igbFilterCell("Your engagements", mine.url(), f.Mine))
		b.WriteString(` &nbsp;&nbsp; <a href="` + newsHost + `/concord/capsuleer?id=` + strconv.FormatInt(who.ID, 10) +
			`">Your dossier</a>`)
	}
	npc := f
	npc.NPC, npc.Page = !f.NPC, 0
	b.WriteString(" &nbsp;&nbsp; " + igbFilterCell("Include hostile-entity engagements", npc.url(), f.NPC))
	if f.Sys != 0 || f.Involving != 0 {
		clear := f
		clear.Sys, clear.Involving, clear.Page = 0, 0, 0
		b.WriteString(` &nbsp;&nbsp; <a href="` + clear.url() + `">clear narrowing</a>`)
	}
	b.WriteString("</font></td></tr></table>\n")
	if noIdentity {
		b.WriteString("<font size=2 color=" + cdDim + ">This terminal sent no capsuleer identity &mdash; showing every incident.</font><br>\n")
	}

	rows, err := evedb.ListKillmails(ctx, s.db, filter)
	if err != nil {
		log.Printf("igb: ListKillmails: %v", err)
		b.WriteString("<br><font size=3>The registry is not available.</font>\n")
		finishSitePage(w, &b, siteConcord, 0)
		return
	}

	// The leaderboards lead the first page as one flat band; deeper pages are
	// for reading the list.
	if f.Page == 0 {
		writePanelBand(&b, siteConcord, s.concordLeaderPanels(ctx, f))
		b.WriteString("<br>")
	}

	var main strings.Builder
	if len(rows) == 0 {
		// Say which: nothing happened, or everything that happened is filtered.
		main.WriteString("<font size=3>No incidents match.</font><br>\n")
		if n, _ := evedb.CountKillmails(ctx, s.db, evedb.KillmailFilter{IncludeNPCKills: true, Limit: 1}); n > 0 {
			main.WriteString("<font size=2 color=" + cdDim + ">" + igbInt(n) + " incident(s) are on record under other filters. ")
			if !f.NPC {
				main.WriteString(`<a href="` + npc.url() + `">Include hostile-entity engagements</a> `)
			}
			main.WriteString(`<a href="` + newsHost + `/concord/">clear every filter</a>.</font><br>` + "\n")
		}
	} else {
		writeIncidentRows(&main, rows)
		total, _ := evedb.CountKillmails(ctx, s.db, filter)
		main.WriteString("<font size=3>")
		if f.Page > 0 {
			prev := f
			prev.Page--
			main.WriteString(`<a href="` + prev.url() + `">&laquo; newer</a> &nbsp;`)
		}
		if int64((f.Page+1)*igbKBPageSize) < total {
			next := f
			next.Page++
			main.WriteString(`<a href="` + next.url() + `">older &raquo;</a>`)
		}
		main.WriteString("</font><br>\n")
	}

	b.WriteString(main.String())
	// No Refresh; the registry updates on demand.
	finishSitePage(w, &b, siteConcord, 0)
}

// writeIncidentRows renders incidents: a dated bar between days, then per incident the time and
// assessed loss, the hull, the system, the victim and the decisive strike. The two text cells
// carry no width. See docs/galnet-igb.md#browser-limits.
func writeIncidentRows(b *strings.Builder, rows []evedb.KillmailRow) {
	writeIncidentRowsTinted(b, rows, nil)
}

// Row shades for a list about one party (a dossier's incidents): kills are green, losses red, as
// dark as cdRowA/B.
var (
	cdKillRows = [2]string{"#0b1a10", "#0f2216"}
	cdLossRows = [2]string{"#1c0c0e", "#251013"}
)

// writeIncidentRowsTinted is writeIncidentRows with a verdict per row: tint
// returns +1 for a kill, -1 for a loss, 0 for neither (nil tint = all 0). A
// kill or loss colours the row and its assessed value, and nothing else moves.
func writeIncidentRowsTinted(b *strings.Builder, rows []evedb.KillmailRow, tint func(evedb.KillmailRow) int) {
	b.WriteString("<table width=100%>\n")
	day := ""
	for i, k := range rows {
		if d := igbYC(k.KillTime); d != day {
			day = d
			b.WriteString("<tr><td colspan=7 bgcolor=" + cdHeader + "><font size=3 color=" + cdTitle + "><b>&nbsp;" + d +
				"</b></font></td></tr>\n")
		}
		shade, valueColor := cdRowA, cdHi
		if i%2 == 1 {
			shade = cdRowB
		}
		if tint != nil {
			switch tint(k) {
			case 1:
				shade, valueColor = cdKillRows[i%2], cdGreen
			case -1:
				shade, valueColor = cdLossRows[i%2], cdRed
			}
		}
		td := func(attrs string) string { return "<td valign=top bgcolor=" + shade + attrs + ">" }

		victim := k.VictimName
		if victim == "" {
			victim = "Unknown capsuleer"
		}
		hull := igbKillLink(k, igbText(k.ShipTypeName))
		if k.IsPod {
			hull += ` <font size=2 color=` + cdRed + `>CAPSULE</font>`
		}
		affil := igbCorpLink(k.VictimCorpID, k.VictimCorpName)
		if k.VictimAllianceName != "" {
			affil += " <font color=" + cdDim + ">/ " + igbText(k.VictimAllianceName) + "</font>"
		}

		final := k.FinalName
		switch {
		case final == "":
			final = "&mdash;"
		case k.FinalIsNPC:
			final = igbText(final) + ` <font size=2 color=` + cdDim + `>[hostile entity]</font>`
		default:
			final = concordPilotLink(k.FinalCharID, final)
		}
		parties := igbKillLink(k, "("+strconv.Itoa(k.AttackerCount)+")")
		solo := ""
		if k.AttackerCount == 1 && !k.FinalIsNPC {
			solo = ` <font size=2 color=` + cdGreen + `><b>SOLO</b></font>`
		}
		finalCorp := ""
		if k.FinalCorpName != "" {
			finalCorp = igbCorpLink(k.FinalCorpID, k.FinalCorpName)
		}

		b.WriteString("<tr>")
		b.WriteString(td(" width=60") + "<font size=3 color=" + cdDim + ">" + k.KillTime.UTC().Format("15:04") +
			"</font><br><font size=3 color=" + valueColor + "><b>" + igbISKShort(k.TotalValue) + "</b></font></td>")
		b.WriteString(td(" width=36") + `<a href="` + concordIncidentURL(k.KillID) + `">` + igbShipIcon(k.ShipTypeID, 32) +
			`</a></td>`)
		b.WriteString(td(" width=120") + "<font size=3>" + concordSystemLink(k.SolarSystemID, k.SystemName) + " " +
			concordSec(k.Security) + "</font><br><font size=2 color=" + cdDim + ">" + igbText(k.RegionName) + "</font></td>")
		b.WriteString(td(" width=34") + concordCorpLogo(k.VictimCorpID, 32) + "</td>")
		b.WriteString(td("") + "<font size=3><b>" + concordPilotLink(k.VictimCharID, victim) + "</b> " + hull +
			"</font><br><font size=2>" + affil + "</font></td>")
		b.WriteString(td(" width=34") + concordCorpLogo(k.FinalCorpID, 32) + "</td>")
		b.WriteString(td("") + "<font size=3>" + final + " " + parties + solo + "</font><br><font size=2>" +
			finalCorp + "</font></td>")
		b.WriteString("</tr>\n")
	}
	b.WriteString("</table>\n")
}

// ── leaderboards ────────────────────────────────────────────────────────────

// igbTopValueWindows / igbTopValueCount (igbkillboard.go) are the ladder the
// highest-losses panel walks: the last day if it fills, wider if it does not,
// and the heading names the window it settled on.

// topKills walks the window ladder for the highest-losses panel.
func (s *NewsServer) topKills(ctx context.Context, base evedb.KillmailFilter) ([]evedb.KillmailRow, string) {
	var rows []evedb.KillmailRow
	var label string
	for _, w := range igbTopValueWindows {
		f := base
		f.Limit, f.Offset = igbTopValueCount, 0
		f.Since = time.Time{}
		if w.Dur > 0 {
			f.Since = time.Now().Add(-w.Dur)
		}
		got, err := evedb.ListValuableKillmails(ctx, s.db, f)
		if err != nil {
			log.Printf("igb: ListValuableKillmails: %v", err)
			return nil, ""
		}
		rows, label = got, w.Label
		if len(rows) >= igbTopValueCount {
			break
		}
	}
	return rows, label
}

// topKillsPanel is the highest-losses panel: hull icon, hull and victim, and the assessed loss.
// The loss is unlinked because an <a> takes the link colour. The top row is tinted.
func topKillsPanel(label string, rows []evedb.KillmailRow) igbBandPanel {
	pn := igbBandPanel{Title: "Highest assessed losses", Note: label,
		Align: []string{"", "", "right"}, Width: []string{"36", "", ""}, Empty: "Nothing on record."}
	for _, k := range rows {
		victim := k.VictimName
		if victim == "" {
			victim = "Unknown capsuleer"
		}
		hull := igbKillLink(k, igbText(k.ShipTypeName))
		if k.IsPod {
			hull += ` <font size=2 color=` + cdRed + `>CAPSULE</font>`
		}
		pn.Rows = append(pn.Rows, []string{
			`<a href="` + concordIncidentURL(k.KillID) + `">` + igbShipIcon(k.ShipTypeID, 32) + `</a>`,
			"<font size=3>" + hull + "</font><br><font size=2 color=" + cdDim + ">" + igbText(victim) + "</font>",
			"<font size=3 color=" + cdGreen + "><b>" + igbISKShort(k.TotalValue) + "</b></font>",
		})
	}
	return pn
}

// igbWriteTopKills renders the highest-losses panel on its own. Empty writes nothing.
func igbWriteTopKills(b *strings.Builder, label string, rows []evedb.KillmailRow) {
	if len(rows) == 0 {
		return
	}
	writePanelBand(b, siteConcord, []igbBandPanel{topKillsPanel(label, rows)})
}

// concordLeaderPanels is the front page's band: the highest losses, the most
// active capsuleers and the most active corporations of the week.
func (s *NewsServer) concordLeaderPanels(ctx context.Context, f concordFilter) []igbBandPanel {
	week := time.Now().Add(-7 * 24 * time.Hour)
	rows, label := s.topKills(ctx, evedb.KillmailFilter{IncludeNPCKills: f.NPC, Kind: f.Kind, SolarSystemID: f.Sys})
	panels := []igbBandPanel{topKillsPanel(label, rows)}

	pilots := igbBandPanel{Title: "Most active capsuleers", Note: "7 days", Align: []string{"", "", "right"},
		Width: []string{"36", "", ""}, Empty: "Nobody this week."}
	if list, err := evedb.TopKillers(ctx, s.db, week, concordSidebarRows, f.NPC); err == nil {
		for _, p := range list {
			pilots.Rows = append(pilots.Rows, []string{
				igbPortrait(p.CharID, 32),
				"<font size=3>" + concordPilotLink(p.CharID, p.Name) + "</font><br><font size=2 color=" + cdDim + ">" +
					igbText(p.CorpName) + "</font>",
				"<font size=4 color=" + cdHi + "><b>" + igbInt(p.Count) + "</b></font>",
			})
		}
	}
	panels = append(panels, pilots)
	if corps, err := evedb.TopKillerCorps(ctx, s.db, week, concordSidebarRows, f.NPC); err == nil {
		panels = append(panels, corpBoardPanel("Most active corporations", "7 days", corps))
	}
	return panels
}

// corpBoardPanel is a corporation leaderboard: logo, name and value, count.
func corpBoardPanel(title, window string, corps []evedb.KillboardCorpLeader) igbBandPanel {
	pn := igbBandPanel{Title: title, Note: window, Align: []string{"", "", "right"}, Width: []string{"36", "", ""},
		Empty: "No corporation on record."}
	for _, c := range corps {
		pn.Rows = append(pn.Rows, []string{
			concordCorpLogo(c.CorpID, 32),
			"<font size=3>" + igbCorpLink(c.CorpID, c.Name) + "</font><br><font size=2 color=" + cdDim + ">" +
				igbISKShort(c.ISK) + " ISK</font>",
			"<font size=4 color=" + cdHi + "><b>" + igbInt(c.Count) + "</b></font>",
		})
	}
	return pn
}

// ── one incident ────────────────────────────────────────────────────────────

func (s *NewsServer) handleConcordIncident(w http.ResponseWriter, r *http.Request) {
	log.Printf("igb: incident id=%q from %s", r.URL.Query().Get("id"), r.RemoteAddr)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	killID, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	var km *evedb.KillmailDetail
	if killID > 0 {
		var err error
		if km, err = evedb.GetKillmail(ctx, s.db, killID); err != nil {
			log.Printf("igb: GetKillmail %d: %v", killID, err)
		}
	}
	var b strings.Builder
	if km == nil {
		writeConcordHeader(&b, "", igbCrumb{"Registry", newsHost + "/concord/"}, igbCrumb{Label: "Incident report"})
		b.WriteString("<br><font size=3>No such incident is on record.</font><br>\n")
		finishSitePage(w, &b, siteConcord, 0)
		return
	}
	writeConcordHeader(&b, "",
		igbCrumb{"Registry", newsHost + "/concord/"},
		igbCrumb{igbText(km.SystemName), concordSystemURL(km.SolarSystemID)},
		igbCrumb{Label: "Incident " + strconv.FormatInt(km.KillID, 10)})

	victim := km.VictimName
	if victim == "" {
		victim = "Unknown capsuleer"
	}
	b.WriteString("<br><font size=3 color=" + cdDim + ">INCIDENT REPORT " + strconv.FormatInt(km.KillID, 10) + " &middot; " +
		igbYCTime(km.KillTime) + "</font><br>")
	b.WriteString("<font size=6 color=" + cdTitle + "><b>" + igbText(km.ShipTypeName) + "</b></font>")
	if km.IsPod {
		b.WriteString(" <font size=4 color=" + cdRed + ">CAPSULE</font>")
	}
	b.WriteString("<br><font size=4>" + concordPilotLink(km.VictimCharID, victim) + "</font><br><br>\n")

	// Victim block: portrait, hull, corporation, place and the assessment.
	b.WriteString("<table width=100%><tr><td bgcolor=" + cdPanel + " valign=top width=136>" + igbPortrait(km.VictimCharID, 128) +
		"</td><td bgcolor=" + cdPanel + " valign=top width=72>" + igbShipIcon(km.ShipTypeID, 64) + "<br>" +
		concordCorpLogo(km.VictimCorpID, 64) + "</td><td bgcolor=" + cdPanel + " valign=top>")
	kv := func(label, value string) {
		if value == "" {
			value = "&mdash;"
		}
		b.WriteString("<font size=3 color=" + cdDim + ">" + label + ":</font> <font size=4>" + value + "</font><br>")
	}
	kv("Capsuleer", concordPilotLink(km.VictimCharID, victim))
	kv("Corporation", igbCorpLink(km.VictimCorpID, km.VictimCorpName))
	kv("Alliance", igbText(km.VictimAllianceName))
	kv("Ship", igbText(km.ShipTypeName))
	kv("Location", concordSystemLink(km.SolarSystemID, km.SystemName)+" "+concordSec(km.Security)+
		" <font size=3 color="+cdDim+">"+igbText(km.RegionName)+"</font>")
	kv("Damage sustained", `<font color=`+cdHi+`>`+igbInt(int64(km.DamageTaken+0.5))+`</font>`)
	b.WriteString("</td><td bgcolor=" + cdPanel + " valign=top align=right width=230>")
	money := func(label string, v float64, color, size string) {
		b.WriteString("<font size=3 color=" + cdDim + ">" + label + "</font> <font size=" + size + " color=" + color + ">" +
			igbISK(v) + "</font><br>")
	}
	money("Hull", km.ShipValue, cdText, "4")
	money("Recovered", km.DroppedValue, cdGreen, "4")
	money("Destroyed", km.DestroyedValue, cdRed, "4")
	money("Assessed loss", km.TotalValue, cdTitle, "5")
	b.WriteString("</td></tr></table><br>\n")

	// With a fit to show, the parties run down the right-hand side as cards beside the fitting
	// list. A mail with no items, or with more parties than the fit has rows, uses the full-width
	// table.
	var side *igbFitSide
	if len(km.FitBands()) > 0 {
		side = concordPartiesSide(km)
		igbWriteFitList(&b, km, side)
	}
	if side == nil {
		igbWriteParties(&b, km)
		b.WriteString("<br>\n")
	}

	b.WriteString(`<font size=2><a href="` + newsHost + `/concord/">&laquo; back to the registry</a>`)
	if km.SolarSystemID != 0 {
		b.WriteString(` &nbsp;|&nbsp; <a href="` + concordSystemURL(km.SolarSystemID) + `">the record of ` +
			igbText(km.SystemName) + `</a>`)
	}
	if s.incidentHasBattle(ctx, km) {
		b.WriteString(` &nbsp;|&nbsp; <a href="` + battleURL(km.KillID) + `">battle report</a>`)
	}
	b.WriteString("</font>\n")
	finishSitePage(w, &b, siteConcord, 0)
}

// igbSidePartiesMax caps the right-hand column. A spanned cell taller than the rows it spans
// stretches them; the rest are counted, and the battle report shows a big fight in full.
// igbPartyCardRows is the fit rows one party card spans: about 85 px, enough for its 64 px images
// and five short lines. A fit too short to give every card that many shares what it has.
const (
	igbSidePartiesMax = 10
	igbPartyCardRows  = 3
)

// igbPartyName is the display name of one attacker, escaped and linked.
func igbPartyName(a evedb.KillmailAttackerRow) string {
	name := a.Name
	if name == "" {
		name = a.ShipTypeName
	}
	return concordPilotLink(a.CharID, name)
}

// igbPartyHasFace reports whether the party is a capsuleer with a portrait to draw.
func igbPartyHasFace(a evedb.KillmailAttackerRow) bool {
	return !a.IsNPC && a.CharID != 0
}

// igbPartyFace is the portrait, or the hull icon for a hostile entity.
func igbPartyFace(a evedb.KillmailAttackerRow, size int) string {
	if !igbPartyHasFace(a) {
		return igbShipIcon(a.ShipTypeID, size)
	}
	return igbPortraitScaled(a.CharID, size)
}

// igbPartyAffil is "corporation / alliance", linked and escaped.
func igbPartyAffil(a evedb.KillmailAttackerRow) string {
	affil := igbCorpLink(a.CorpID, a.CorpName)
	if a.AllianceName != "" {
		affil += " / " + igbText(a.AllianceName)
	}
	return affil
}

// igbPartyShare is the party's fraction of the damage the victim took.
func igbPartyShare(km *evedb.KillmailDetail, a evedb.KillmailAttackerRow) float64 {
	if km.DamageTaken <= 0 {
		return 0
	}
	return a.Damage / km.DamageTaken
}

// igbWriteParties is the full-width parties table, used when there is no fit to
// sit beside. Each party shows its corporation's logo; the decisive strike is
// the highlighted row.
func igbWriteParties(b *strings.Builder, km *evedb.KillmailDetail) {
	b.WriteString("<font size=4><b><font color=" + cdTitle + ">Parties involved</font></b></font> <font size=2 color=" + cdDim +
		">" + strconv.Itoa(len(km.Attackers)) + "</font><br>\n")
	if len(km.Attackers) == 0 {
		b.WriteString("<font size=2 color=" + cdDim + ">No party was recorded.</font><br>\n")
		return
	}
	b.WriteString("<table width=100%>\n")
	for i, a := range km.Attackers {
		shade := cdRowA
		if i%2 == 1 {
			shade = cdRowB
		}
		if a.FinalBlow {
			shade = cdNow
		}
		name := igbPartyName(a)
		if a.FinalBlow {
			name += ` <font size=2 color=` + cdGreen + `><b>DECISIVE STRIKE</b></font>`
		}
		if a.IsNPC {
			name += ` <font size=2 color=` + cdDim + `>[hostile entity]</font>`
		}
		share := igbPartyShare(km, a)
		b.WriteString("<tr><td width=68 bgcolor=" + shade + ">" + igbPartyFace(a, 64) + "</td>")
		b.WriteString("<td width=68 bgcolor=" + shade + ">" + concordCorpLogo(a.CorpID, 64) + "</td>")
		b.WriteString("<td bgcolor=" + shade + "><font size=3>" + name + "</font><br><font size=2 color=" + cdDim + ">" +
			igbPartyAffil(a) + "</font></td>")
		b.WriteString("<td width=68 bgcolor=" + shade + ">" + igbShipIcon(a.ShipTypeID, 64) + "</td>")
		b.WriteString("<td bgcolor=" + shade + "><font size=2>" + igbText(a.ShipTypeName) + "<br><font color=" + cdDim + ">" +
			igbText(a.WeaponName) + "</font></font></td>")
		b.WriteString("<td width=150 bgcolor=" + shade + "><font size=2>" + igbBar(share, 20, cdHi) + "</font></td>")
		b.WriteString("<td width=90 align=right bgcolor=" + shade + "><font size=2 color=" + cdHi + ">" +
			igbInt(int64(a.Damage+0.5)) + "</font><br><font size=3 color=" + cdDim + ">" +
			strconv.FormatFloat(share*100, 'f', 1, 64) + "%</font></td></tr>\n")
	}
	b.WriteString("</table>\n")
}

// concordPartiesSide is the parties as cards beside the fitting list, or nil when more cards are
// shown than the fit has rows to start them on.
func concordPartiesSide(km *evedb.KillmailDetail) *igbFitSide {
	side := &igbFitSide{Heading: "<font size=4><b><font color=" + cdTitle + ">Parties involved</font></b></font> <font size=2 color=" +
		cdDim + ">" + strconv.Itoa(len(km.Attackers)) + "</font>"}
	if len(km.Attackers) == 0 {
		side.Cards = []igbFitCard{{Text: "<font size=2 color=" + cdDim + ">No party was recorded.</font>", Shade: cdPanel}}
		return side
	}
	shown := km.Attackers
	if len(shown) > igbSidePartiesMax {
		shown = shown[:igbSidePartiesMax]
	}
	rows := igbFitRows(km) - 1
	if len(shown) > rows {
		return nil
	}
	side.Span = min(igbPartyCardRows, rows/len(shown))
	for _, a := range shown {
		side.Cards = append(side.Cards, concordPartyCard(km, a))
	}
	if more := len(km.Attackers) - len(shown); more > 0 {
		side.Cards[len(shown)-1].Text += "<br><font size=2 color=" + cdDim + ">and " + strconv.Itoa(more) + " more</font>"
	}
	return side
}

// concordPartyCard is one party's card: portrait and corp logo side by side at 64 px, then name,
// the decisive-strike tag, affiliation, ship and weapon on one line, and damage with its bar on
// one line. No hull icon in the text, since a 32 px icon would make its line 32 px tall. A hostile
// entity's portrait slot already shows its hull.
func concordPartyCard(km *evedb.KillmailDetail, a evedb.KillmailAttackerRow) igbFitCard {
	card := igbFitCard{Shade: cdPanel, Images: igbPartyFace(a, 64) + concordCorpLogo(a.CorpID, 64)}
	if a.FinalBlow {
		card.Shade = cdNow
	}

	var b strings.Builder
	b.WriteString("<font size=3>" + igbPartyName(a) + "</font>")
	if a.IsNPC {
		b.WriteString(` <font size=2 color=` + cdDim + `>[hostile entity]</font>`)
	}
	if a.FinalBlow {
		b.WriteString(`<br><font size=2 color=` + cdGreen + `><b>DECISIVE STRIKE</b></font>`)
	}
	b.WriteString("<br><font size=2 color=" + cdDim + ">" + igbPartyAffil(a) + "</font>")
	b.WriteString("<br><font size=2>" + igbText(a.ShipTypeName))
	if a.WeaponName != "" {
		b.WriteString(" <font color=" + cdDim + ">/ " + igbText(a.WeaponName) + "</font>")
	}
	b.WriteString("</font>")
	share := igbPartyShare(km, a)
	b.WriteString("<br><font size=3 color=" + cdHi + ">" + igbInt(int64(a.Damage+0.5)) + "</font> <font size=3 color=" + cdDim +
		">" + strconv.FormatFloat(share*100, 'f', 1, 64) + "%</font> <font size=2>" + igbBar(share, 10, cdHi) + "</font>")
	card.Text = b.String()
	return card
}

// ── one system ──────────────────────────────────────────────────────────────

func (s *NewsServer) handleConcordSystem(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	sysID, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	var sys *evedb.RegistrySystem
	if sysID > 0 {
		var err error
		if sys, err = evedb.GetRegistrySystem(ctx, s.db, sysID); err != nil {
			log.Printf("igb: GetRegistrySystem %d: %v", sysID, err)
		}
	}
	var b strings.Builder
	if sys == nil {
		writeConcordHeader(&b, "", igbCrumb{"Registry", newsHost + "/concord/"}, igbCrumb{Label: "System record"})
		b.WriteString("<br><font size=3>No such solar system is on record.</font><br>\n")
		finishSitePage(w, &b, siteConcord, 0)
		return
	}
	writeConcordHeader(&b, "", igbCrumb{"Registry", newsHost + "/concord/"}, igbCrumb{Label: igbText(sys.Name)})

	b.WriteString("<br><table width=100% bgcolor=" + cdPanel + "><tr><td valign=top>")
	b.WriteString("<font size=2 color=" + cdDim + ">SYSTEM RECORD</font><br><font size=7 color=" + cdTitle + "><b>" +
		igbText(sys.Name) + "</b></font> <font size=5>" + concordSec(sys.Security) + "</font><br>")
	b.WriteString("<font size=3>" + igbText(sys.ConstellationName) + " <font color=" + cdDim + ">&middot;</font> " +
		igbText(sys.RegionName) + "</font></td>")
	for _, win := range []struct {
		label string
		d     time.Duration
	}{{"24 HOURS", 24 * time.Hour}, {"7 DAYS", 7 * 24 * time.Hour}, {"30 DAYS", 30 * 24 * time.Hour}} {
		n, isk, err := evedb.KillmailTotals(ctx, s.db, evedb.KillmailFilter{SolarSystemID: sysID,
			Since: time.Now().Add(-win.d), IncludeNPCKills: true})
		if err != nil {
			continue
		}
		b.WriteString("<td valign=top align=right width=130><font size=2 color=" + cdDim + ">" + win.label + "</font><br>" +
			"<font size=5 color=" + cdTitle + "><b>" + igbInt(n) + "</b></font> <font size=2 color=" + cdDim +
			">losses</font><br><font size=3 color=" + cdHi + ">" + igbISKShort(isk) + " ISK</font></td>")
	}
	b.WriteString("</tr></table><br>\n")

	if days, err := evedb.SystemActivity(ctx, s.db, sysID, igbActivityDays); err == nil {
		writeSystemActivity(&b, days)
	}

	top, label := s.topKills(ctx, evedb.KillmailFilter{SolarSystemID: sysID, IncludeNPCKills: true})
	panels := []igbBandPanel{topKillsPanel(label, top)}
	if corps, err := evedb.SystemTopCorps(ctx, s.db, sysID, time.Now().Add(-30*24*time.Hour), concordSidebarRows); err == nil {
		panels = append(panels, corpBoardPanel("Most active aggressors", "30 days", corps))
	}
	writePanelBand(&b, siteConcord, panels)

	var main strings.Builder
	main.WriteString("<br><font size=4><b><font color=" + cdTitle + ">Recent incidents</font></b></font><br>\n")
	rows, err := evedb.ListKillmails(ctx, s.db, evedb.KillmailFilter{SolarSystemID: sysID, IncludeNPCKills: true,
		Limit: concordSystemFeed})
	if err != nil || len(rows) == 0 {
		main.WriteString("<font size=3 color=" + cdDim + ">No incidents are on record in this system.</font><br>\n")
	} else {
		writeIncidentRows(&main, rows)
		main.WriteString(`<font size=3><a href="` + concordFilter{Sys: sysID, NPC: true}.url() + `">every incident in ` +
			igbText(sys.Name) + ` &raquo;</a> &nbsp;&nbsp; <a href="` +
			battleFilter{Window: "30d", Sys: sysID}.url() + `">battle reports in ` + igbText(sys.Name) + ` &raquo;</a></font><br>`)
	}
	b.WriteString(main.String())
	finishSitePage(w, &b, siteConcord, 0)
}

// writeSystemActivity draws the system's fortnight: capsuleer-caused losses
// over hostile-entity losses, scaled to the busiest day.
func writeSystemActivity(b *strings.Builder, days []evedb.SystemDayCount) {
	var max int64
	any := false
	for _, d := range days {
		if d.Capsuleer > max {
			max = d.Capsuleer
		}
		if d.HostileOnly > max {
			max = d.HostileOnly
		}
		if d.Capsuleer+d.HostileOnly > 0 {
			any = true
		}
	}
	if !any {
		return
	}
	b.WriteString("<font size=4><b><font color=" + cdTitle + ">Last " + strconv.Itoa(len(days)) + " days</font></b></font> " +
		"<font size=2><font color=" + cdRed + "><b>|</b></font> <font color=" + cdDim + ">capsuleer engagements</font> &nbsp;" +
		"<font color=" + cdHi + "><b>|</b></font> <font color=" + cdDim + ">hostile-entity engagements</font></font><br>\n")
	b.WriteString("<table width=100%>\n")
	for i, d := range days {
		shade := cdRowA
		if i%2 == 1 {
			shade = cdRowB
		}
		b.WriteString("<tr><td width=110 bgcolor=" + shade + "><font size=2 color=" + cdDim + ">" + igbYC(d.Day) +
			"</font></td><td bgcolor=" + shade + "><font size=2>" + igbBar(float64(d.Capsuleer)/float64(max), 24, cdRed) +
			" " + igbInt(d.Capsuleer) + "</font></td><td bgcolor=" + shade + "><font size=2>" +
			igbBar(float64(d.HostileOnly)/float64(max), 24, cdHi) + " " + igbInt(d.HostileOnly) + "</font></td>" +
			"<td align=right bgcolor=" + shade + "><font size=2 color=" + cdDim + ">" + igbISKShort(d.AssessedLoss) +
			" ISK</font></td></tr>\n")
	}
	b.WriteString("</table><br>\n")
}
