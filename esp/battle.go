package esp

// Battle Reports on CONCORD's Incident Registry.
//
//	/concord/battles?w=24h|7d|30d&sys=N   the recent engagements, one line each
//	/concord/battle?k=N                   one engagement, seeded from incident N
//
// A battle is every incident in one system within a run of closely spaced losses; its parties
// split into two teams by who was shooting whom (db/battle.go). The permalink is a seed incident,
// not a battle ID, and nothing is stored. The two teams are one table (writePanelGrid), never two
// tables side by side or a table in a cell: see docs/galnet-igb.md#shared-panel-grid.

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

const (
	// Team colours: the registry's light-bar blue against amber, so neither reads as the red one.
	// Red is reserved for what was lost.
	cdTeamA = cdHi
	cdTeamB = "#e8a24a"

	battlePilotRows = 80  // capsuleers listed per team before "and N more"
	battleKillRows  = 100 // incidents listed under the report
	battleListRows  = 25
)

// The five-column grid every team panel shares (writePanelGrid): three icon
// cells, the text, and a right-aligned figure. Only the icon cells are fixed.
var (
	battleGridAlign = []string{"", "", "", "", "right"}
	battleGridWidth = []string{"36", "36", "36", "", "110"}
)

var battleWindows = []struct {
	Key, Label string
	D          time.Duration
}{
	{"24h", "24 hours", 24 * time.Hour},
	{"7d", "7 days", 7 * 24 * time.Hour},
	{"30d", "30 days", 30 * 24 * time.Hour},
}

func battleURL(killID int64) string {
	return newsHost + "/concord/battle?k=" + strconv.FormatInt(killID, 10)
}

// battleFilter is the list's state; every link on it is built from one.
type battleFilter struct {
	Window string // "24h" | "7d" | "30d"
	Sys    int64
}

func (f battleFilter) dur() time.Duration {
	for _, w := range battleWindows {
		if w.Key == f.Window {
			return w.D
		}
	}
	return 7 * 24 * time.Hour
}

func (f battleFilter) url() string {
	q := url.Values{}
	if f.Window != "" && f.Window != "7d" {
		q.Set("w", f.Window)
	}
	if f.Sys != 0 {
		q.Set("sys", strconv.FormatInt(f.Sys, 10))
	}
	u := newsHost + "/concord/battles"
	if enc := q.Encode(); enc != "" {
		u += "?" + strings.ReplaceAll(enc, "&", "&amp;")
	}
	return u
}

func teamColor(i int) string {
	if i == 0 {
		return cdTeamA
	}
	return cdTeamB
}

// battleSpan words a duration for the heading.
func battleSpan(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + " minutes"
	default:
		return strconv.Itoa(int(d.Hours())) + " h " + strconv.Itoa(int(d.Minutes())%60) + " min"
	}
}

// incidentHasBattle reports whether other incidents sit close enough to this
// one to make a battle report worth linking: the cheap test, a COUNT.
func (s *NewsServer) incidentHasBattle(ctx context.Context, km *evedb.KillmailDetail) bool {
	if km.NPCOnly || km.SolarSystemID == 0 {
		return false
	}
	opt := evedb.LoadBattleOptions(ctx, s.db)
	n, err := evedb.CountKillmails(ctx, s.db, evedb.KillmailFilter{
		SolarSystemID: km.SolarSystemID,
		Since:         km.KillTime.Add(-opt.Gap),
		Until:         km.KillTime.Add(opt.Gap),
	})
	return err == nil && n >= 2
}

// ── the list ────────────────────────────────────────────────────────────────

func (s *NewsServer) handleConcordBattles(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	q := r.URL.Query()
	f := battleFilter{Window: q.Get("w")}
	f.Sys, _ = strconv.ParseInt(q.Get("sys"), 10, 64)
	known := false
	for _, win := range battleWindows {
		known = known || win.Key == f.Window
	}
	if !known {
		f.Window = "7d"
	}

	var b strings.Builder
	writeConcordHeader(&b, "battles")
	b.WriteString("<br><font size=3 color=" + cdDim + ">BATTLE REPORTS</font><br><font size=3>Engagements in which capsuleers of two " +
		"sides lost ships in the same system. Each is reconstructed from who was shooting whom.</font><br><br>\n")

	b.WriteString("<table width=100% bgcolor=" + cdHeader + "><tr><td><font size=3>&nbsp;Show: ")
	for i, win := range battleWindows {
		if i > 0 {
			b.WriteString(" &middot; ")
		}
		wf := f
		wf.Window = win.Key
		b.WriteString(igbFilterCell(win.Label, wf.url(), f.Window == win.Key))
	}
	if f.Sys != 0 {
		clear := f
		clear.Sys = 0
		b.WriteString(` &nbsp;&nbsp; <a href="` + clear.url() + `">clear narrowing</a>`)
	}
	b.WriteString("</font></td></tr></table>\n")

	list, err := evedb.ListBattles(ctx, s.db, evedb.BattleListQuery{
		Window: f.dur(), SystemID: f.Sys, IncludeBots: true,
	}, battleListRows)
	if err != nil {
		log.Printf("igb: ListBattles: %v", err)
		b.WriteString("<br><font size=3>The registry is not available.</font>\n")
		finishSitePage(w, &b, siteConcord, 0)
		return
	}
	writeBattleList(&b, list, f)
	finishSitePage(w, &b, siteConcord, 0)
}

// writeBattleList is the list's table: a dated bar between days, then per
// battle the time and total loss, the system, who fought, an ISK split bar and
// each side's loss.
func writeBattleList(b *strings.Builder, list []evedb.BattleSummary, f battleFilter) {
	if len(list) == 0 {
		b.WriteString("<br><font size=3>No battles on record in this window.</font><br>\n")
		if f.Window != "30d" {
			wide := f
			wide.Window = "30d"
			b.WriteString(`<font size=2 color=` + cdDim + `><a href="` + wide.url() + `">Look back 30 days</a></font><br>` + "\n")
		}
		return
	}
	b.WriteString("<br><table width=100%>\n")
	day := ""
	for i, bt := range list {
		if d := igbYC(bt.Start); d != day {
			day = d
			b.WriteString("<tr><td colspan=5 bgcolor=" + cdHeader + "><font size=3 color=" + cdTitle + "><b>&nbsp;" + d +
				"</b></font></td></tr>\n")
		}
		shade := cdRowA
		if i%2 == 1 {
			shade = cdRowB
		}
		td := func(attrs string) string { return "<td valign=top bgcolor=" + shade + attrs + ">" }

		total := bt.ISKLost[0] + bt.ISKLost[1]
		var frac float64
		if total > 0 {
			frac = bt.ISKLost[0] / total
		}
		ships := bt.ShipsLost[0] + bt.ShipsLost[1]
		who := bt.Involved[0] + bt.Involved[1]

		b.WriteString("<tr>")
		b.WriteString(td(" width=60") + "<font size=3 color=" + cdDim + ">" + bt.Start.UTC().Format("15:04") +
			"</font><br><font size=3 color=" + cdHi + "><b>" + igbISKShort(total) + "</b></font></td>")
		b.WriteString(td(" width=120") + "<font size=3>" + concordSystemLink(bt.SystemID, bt.SystemName) + " " +
			concordSec(bt.Security) + "</font><br><font size=2 color=" + cdDim + ">" + igbText(bt.RegionName) + "</font></td>")
		b.WriteString(td("") + "<font size=3><b><font color=" + cdTeamA + ">" + igbText(bt.Lead[0]) + "</font></b> " +
			"<font color=" + cdDim + ">versus</font> <b><font color=" + cdTeamB + ">" + igbText(bt.Lead[1]) + "</font></b></font>" +
			"<br><font size=2 color=" + cdDim + ">" + igbInt(int64(who)) + " capsuleers &middot; " + igbInt(int64(ships)) +
			" ships lost in " + igbInt(int64(bt.Kills)) + " incidents</font> <font size=3><a href=\"" + battleURL(bt.SeedKillID) +
			"\">report &raquo;</a></font></td>")
		b.WriteString(td(" width=170") + "<font size=2>" + igbSplitBar(frac, 20, cdTeamA, cdTeamB) + "</font></td>")
		b.WriteString(td(" width=110 align=right") + "<font size=3 color=" + cdTeamA + ">" + igbISKShort(bt.ISKLost[0]) +
			"</font><br><font size=3 color=" + cdTeamB + ">" + igbISKShort(bt.ISKLost[1]) + "</font></td>")
		b.WriteString("</tr>\n")
	}
	b.WriteString("</table>\n")
}

// ── one battle ──────────────────────────────────────────────────────────────

func (s *NewsServer) handleConcordBattle(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	killID, _ := strconv.ParseInt(r.URL.Query().Get("k"), 10, 64)
	var rows []evedb.KillmailRow
	if killID > 0 {
		var err error
		rows, err = evedb.BattleKillsAround(ctx, s.db, killID, evedb.LoadBattleOptions(ctx, s.db), true)
		if err != nil {
			log.Printf("igb: BattleKillsAround %d: %v", killID, err)
		}
	}
	crumbs := []igbCrumb{{"Registry", newsHost + "/concord/"}, {"Battle reports", newsHost + "/concord/battles"}}
	var b strings.Builder
	if len(rows) == 0 {
		writeConcordHeader(&b, "", append(crumbs, igbCrumb{Label: "Battle"})...)
		b.WriteString("<br><font size=3>No such battle is on record.</font><br>\n")
		finishSitePage(w, &b, siteConcord, 0)
		return
	}
	bt, err := evedb.LoadBattle(ctx, s.db, rows)
	if err != nil {
		log.Printf("igb: LoadBattle %d: %v", killID, err)
		writeConcordHeader(&b, "", append(crumbs, igbCrumb{Label: "Battle"})...)
		b.WriteString("<br><font size=3>The registry is not available.</font><br>\n")
		finishSitePage(w, &b, siteConcord, 0)
		return
	}
	writeConcordHeader(&b, "", append(crumbs, igbCrumb{Label: igbText(bt.SystemName) + " " + igbYC(bt.Start)})...)
	writeBattleReport(&b, bt, rows)
	finishSitePage(w, &b, siteConcord, 0)
}

// writeBattleReport renders one battle. kills are the cluster's incidents,
// oldest first; they are listed newest first under the teams.
func writeBattleReport(b *strings.Builder, bt *evedb.Battle, kills []evedb.KillmailRow) {
	b.WriteString("<br><font size=3 color=" + cdDim + ">BATTLE REPORT &middot; " + igbYC(bt.Start) + " " +
		bt.Start.UTC().Format("15:04") + " to " + bt.End.UTC().Format("15:04") + "</font><br>")
	b.WriteString("<font size=6 color=" + cdTitle + "><b>" + igbText(bt.SystemName) + "</b></font> <font size=5>" +
		concordSec(bt.Security) + "</font><br><font size=3>" + igbText(bt.RegionName) + " <font color=" + cdDim + ">&middot;</font> " +
		igbInt(int64(bt.Kills)) + " incidents over " + battleSpan(bt.End.Sub(bt.Start)) + "</font><br><br>\n")

	if !bt.Valid() {
		b.WriteString("<font size=3>These incidents do not form a two-sided engagement, so there is no battle to report.</font><br>\n")
		if bt.NoAttackerKills > 0 {
			b.WriteString("<font size=2 color=" + cdDim + ">" + igbInt(int64(bt.NoAttackerKills)) +
				" incident(s) carry no record of the parties involved.</font><br>\n")
		}
	} else {
		writePanelBand(b, siteConcord, battleBars(bt))
		b.WriteString("<br>")
		writePanelGrid(b, siteConcord, battleGrid(bt))
		b.WriteString("<br>")
		if bt.UninvolvedKills > 0 || bt.NoAttackerKills > 0 {
			b.WriteString("<font size=2 color=" + cdDim + ">")
			if bt.UninvolvedKills > 0 {
				b.WriteString(igbInt(int64(bt.UninvolvedKills)) + " incident(s) fell to parties on neither side and are not counted. ")
			}
			if bt.NoAttackerKills > 0 {
				b.WriteString(igbInt(int64(bt.NoAttackerKills)) + " incident(s) carry no record of the parties involved.")
			}
			b.WriteString("</font><br><br>\n")
		}
	}

	// Every incident, newest first.
	b.WriteString("<font size=4><b><font color=" + cdTitle + ">Incidents in this battle</font></b></font><br>\n")
	shown := make([]evedb.KillmailRow, 0, min(len(kills), battleKillRows))
	for i := len(kills) - 1; i >= 0 && len(shown) < battleKillRows; i-- {
		shown = append(shown, kills[i])
	}
	writeIncidentRows(b, shown)
	if len(kills) > len(shown) {
		b.WriteString("<font size=2 color=" + cdDim + ">The " + igbInt(int64(len(shown))) + " most recent of " +
			igbInt(int64(len(kills))) + " are shown.</font><br>\n")
	}
	b.WriteString(`<br><font size=2><a href="` + newsHost + `/concord/battles">&laquo; all battle reports</a> &nbsp;|&nbsp; ` +
		`<a href="` + concordSystemURL(bt.SystemID) + `">the record of ` + igbText(bt.SystemName) + `</a></font>` + "\n")
}

// battleBars is the top band: assessed loss and ships lost, one bar per side,
// each scaled against the larger of the two.
func battleBars(bt *evedb.Battle) []igbBandPanel {
	mk := func(title string, vals [2]float64, text func(i int) string) igbBandPanel {
		pn := igbBandPanel{Title: title, Align: []string{"", "", "right"}, Width: []string{"", "", "110"}}
		top := max(vals[0], vals[1])
		for i := 0; i < 2; i++ {
			var frac float64
			if top > 0 {
				frac = vals[i] / top
			}
			pn.Rows = append(pn.Rows, []string{
				"<font size=3 color=" + teamColor(i) + ">" + igbText(bt.Teams[i].Title()) + "</font>",
				"<font size=2>" + igbBar(frac, 20, teamColor(i)) + "</font>",
				"<font size=3 color=" + teamColor(i) + "><b>" + text(i) + "</b></font>",
			})
		}
		return pn
	}
	return []igbBandPanel{
		mk("Assessed loss", [2]float64{bt.Teams[0].ISKLost, bt.Teams[1].ISKLost},
			func(i int) string { return igbISKShort(bt.Teams[i].ISKLost) }),
		mk("Ships lost", [2]float64{float64(bt.Teams[0].ShipsLost), float64(bt.Teams[1].ShipsLost)},
			func(i int) string { return igbInt(int64(bt.Teams[i].ShipsLost)) }),
	}
}

// battleGrid is the two teams as bands of one table: who they are, the hulls
// they lost by class, the totals, and the capsuleers.
func battleGrid(bt *evedb.Battle) [][]igbBandPanel {
	panel := func(title, note string) igbBandPanel {
		return igbBandPanel{Title: title, Note: note, Align: battleGridAlign, Width: battleGridWidth}
	}
	row := func(c0, c1, c2, text, fig string) []string { return []string{c0, c1, c2, text, fig} }

	var nodes, classes, totals, pilots [2]igbBandPanel
	for i := 0; i < 2; i++ {
		t := bt.Teams[i]
		col := teamColor(i)
		head := "<font color=" + col + ">" + igbText(t.Title()) + "</font>"

		nodes[i] = panel(head, "("+igbInt(int64(t.Involved()))+" involved)")
		for _, n := range t.Nodes {
			name := igbText(n.Name)
			switch {
			case n.Alliance:
				// An alliance has no dossier of its own; its corporations do.
			case n.LogoCorpID == 0:
				name = igbPilotLink(n.ID, n.Name)
			default:
				name = igbCorpLink(n.ID, n.Name)
			}
			nodes[i].Rows = append(nodes[i].Rows, row(concordCorpLogo(n.LogoCorpID, 32), "", "",
				"<font size=3>"+name+"</font>",
				"<font size=2 color="+cdDim+">"+igbInt(int64(n.Pilots))+" capsuleers</font>"))
		}

		classes[i] = panel("Ships lost", "")
		for _, c := range t.Classes {
			classes[i].Rows = append(classes[i].Rows, row(
				"<font size=3 color="+cdRed+"><b>"+igbInt(int64(c.Lost))+"</b></font>", "", "",
				"<font size=3>"+igbText(c.Name)+"</font>",
				"<font size=3 color="+col+">"+igbISKShort(c.ISK)+"</font>"))
		}
		if len(t.Classes) == 0 {
			classes[i].Rows = append(classes[i].Rows, row("", "", "", "<font size=3 color="+cdDim+">No ship was lost.</font>", ""))
		}

		totals[i] = panel("Totals", "")
		kv := func(label, value string) {
			totals[i].Rows = append(totals[i].Rows, row("", "", "",
				"<font size=3 color="+cdDim+">"+label+"</font>", "<font size=3>"+value+"</font>"))
		}
		kv("Ships lost", igbInt(int64(t.ShipsLost)))
		kv("Assessed loss", "<font color="+cdRed+">"+igbISKShort(t.ISKLost)+"</font>")
		kv("Recovered in wrecks", "<font color="+cdGreen+">"+igbISKShort(t.ISKDropped)+"</font>")
		kv("Capsuleers involved", igbInt(int64(t.Involved())))

		lost := 0
		for _, p := range t.Pilots {
			if p.Died() {
				lost++
			}
		}
		pilots[i] = panel("Capsuleers", igbInt(int64(lost))+" lost a ship")
		for n, p := range t.Pilots {
			if n >= battlePilotRows {
				pilots[i].Rows = append(pilots[i].Rows, row("", "", "",
					"<font size=2 color="+cdDim+">and "+igbInt(int64(len(t.Pilots)-battlePilotRows))+" more capsuleers</font>", ""))
				pilots[i].Shades = append(pilots[i].Shades, "")
				break
			}
			pilots[i].Rows = append(pilots[i].Rows, pilotRow(p))
			shade := ""
			if p.Died() {
				shade = cdLossRows[(len(pilots[i].Rows)-1)%2]
			}
			pilots[i].Shades = append(pilots[i].Shades, shade)
		}
	}
	return [][]igbBandPanel{
		{nodes[0], nodes[1]},
		{classes[0], classes[1]},
		{totals[0], totals[1]},
		{pilots[0], pilots[1]},
	}
}

// pilotRow is one capsuleer: hull icon, portrait, corporation logo, then who
// they are and what they flew, and on the right either what they lost or how
// hard they hit.
func pilotRow(p evedb.BattlePilot) []string {
	hull := igbText(p.ShipName)
	if hull == "" {
		hull = "unknown hull"
	}
	tag := ""
	if p.Died() {
		pods, ships := 0, 0
		for _, l := range p.Losses {
			if l.IsPod {
				pods++
			} else {
				ships++
			}
		}
		if pods > 0 && ships > 0 {
			tag = " <font size=2 color=" + cdRed + ">+ CAPSULE</font>"
		}
	}
	affil := igbCorpLink(p.CorpID, p.CorpName)
	if p.AllianceName != "" {
		affil += " <font color=" + cdDim + ">/ " + igbText(p.AllianceName) + "</font>"
	}
	name := p.Name
	if name == "" {
		name = "Unknown capsuleer"
	}
	fig := "<font size=2 color=" + cdDim + ">" + igbInt(int64(p.Damage+0.5)) + " damage</font>"
	if p.Died() {
		fig = "<font size=3 color=" + cdRed + "><b>" + igbISKShort(p.LostISK) + "</b></font>"
		if len(p.Losses) > 1 {
			fig += "<br><font size=2 color=" + cdDim + ">" + igbInt(int64(len(p.Losses))) + " ships</font>"
		}
	}
	return []string{
		igbShipIcon(p.ShipTypeID, 32),
		igbPortrait(p.CharID, 32),
		concordCorpLogo(p.CorpID, 32),
		"<font size=3><b>" + igbPilotLink(p.CharID, name) + "</b> (" + hull + ")" + tag + "</font><br><font size=2>" + affil + "</font>",
		fig,
	}
}
