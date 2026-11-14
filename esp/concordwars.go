package esp

// The DED's Wars tab: who is at war, what it has cost each side, and when it ends or the next
// fee falls due.
//
//	/concord/wars?v=active|ended     the list
//	/concord/war?id=N&show=&page=    one war report
//
// The report follows the client's War Report window: attacker left in red, defender right in blue,
// then every incident day by day. Rows are tinted by the victim's side. Side colours go on
// non-link text only: see docs/esp-3-notes.md#war-report-side-colours.

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

// Side colours: the attacker is red, the defender blue (the in-game window's colours). The row
// tints are the same hues, dark enough for white text, alternating like cdRowA/B.
const (
	cdWarAtk = cdRed
	cdWarDef = "#4f93ff"
)

var (
	cdWarAtkRows = [2]string{"#2a0f12", "#34141a"} // the attacker lost this one
	cdWarDefRows = [2]string{"#0e1c36", "#122648"} // the defender lost this one
)

const (
	concordWarListCap    = 50
	concordWarEndedDays  = 30
	concordWarListBudget = 12 * time.Second
)

func warURL(id int64, show string, page int) string {
	u := newsHost + "/concord/war?id=" + strconv.FormatInt(id, 10)
	if show != "" {
		u += "&amp;show=" + show
	}
	if page > 0 {
		u += "&amp;page=" + strconv.Itoa(page)
	}
	return u
}

// warVictimSide says which side a victim belonged to: 1 the attacker (red), 2 the
// defender (blue), 0 neither.
func warVictimSide(k evedb.KillmailRow, pair evedb.WarKillPair) int {
	in := func(p evedb.WarParty) bool {
		if p.Alliance {
			return k.VictimAllianceID == p.ID
		}
		return k.VictimCorpID == p.ID
	}
	switch {
	case in(pair.A):
		return 1
	case in(pair.B):
		return 2
	}
	return 0
}

// warStateWords is a war's state in the registry's words, with the colour it
// is written in.
func warStateWords(s evedb.WarSchedule) (string, string) {
	var text, color string
	switch s.State {
	case evedb.WarStateArming:
		text, color = "Arming", cdDim
	case evedb.WarStateHostile:
		text, color = "Hostile", cdRed
	case evedb.WarStateWindingDown:
		text, color = "Winding down", cdHi
	default:
		text, color = "Ended", cdDim
	}
	if s.Mutual && s.State != evedb.WarStateEnded {
		text += " &middot; mutual"
	}
	return text, color
}

func warDate(unix int64) string { return igbYC(time.Unix(unix, 0)) }
func warDateTime(unix int64) string {
	return igbYCTime(time.Unix(unix, 0))
}

// warNextFeeWords is the fee cell: when the next bill is issued, or why none is.
func warNextFeeWords(s evedb.WarSchedule, now int64) string {
	switch {
	case s.State == evedb.WarStateEnded:
		return "<font color=" + cdDim + ">&mdash;</font>"
	case s.Mutual:
		return "<font color=" + cdDim + ">none (mutual)</font>"
	case s.State == evedb.WarStateWindingDown:
		return "<font color=" + cdDim + ">none (retracted)</font>"
	}
	out := ""
	if s.OpenBill != nil {
		out += "<font color=" + cdRed + ">unpaid, due " + warDate(s.OpenBill.DueUnix) + "</font><br>"
	}
	if s.NextFeeUnix > 0 && s.NextFeeUnix <= now {
		return out + "next fee due now"
	}
	return out + "next " + warDate(s.NextFeeUnix)
}

// ── the list ────────────────────────────────────────────────────────────────

func (s *NewsServer) handleConcordWars(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), concordWarListBudget)
	defer cancel()

	ended := r.URL.Query().Get("v") == "ended"
	var b strings.Builder
	writeConcordHeader(&b, "wars")
	b.WriteString("<br><font size=3 color=" + cdDim + ">WARS DECLARED BETWEEN CAPSULEER CORPORATIONS AND ALLIANCES</font><br>")
	b.WriteString("<font size=3>An attacker declares, the defender is notified, and after 24 hours either side may be fired upon " +
		"without CONCORD's interest. CONCORD bills the attacker weekly to keep the war legal; a war ends when the attacker " +
		"retracts it (24 hours later) or fails to pay.</font><br><br>\n")

	b.WriteString("<table width=100% bgcolor=" + cdHeader + "><tr><td><font size=3>&nbsp;")
	b.WriteString(igbFilterCell("Active wars", newsHost+"/concord/wars", !ended))
	b.WriteString(" &middot; " + igbFilterCell("Ended in the last "+strconv.Itoa(concordWarEndedDays)+" days",
		newsHost+"/concord/wars?v=ended", ended))
	b.WriteString("</font></td></tr></table>\n")

	wars, err := evedb.ListWarsForRegistry(ctx, s.db, ended, concordWarEndedDays, concordWarListCap)
	if err != nil {
		log.Printf("igb: ListWarsForRegistry: %v", err)
		b.WriteString("<br><font size=3>The registry is not available.</font><br>\n")
		finishSitePage(w, &b, siteConcord, 0)
		return
	}
	if len(wars) == 0 {
		if ended {
			b.WriteString("<br><font size=3>No war has ended in the last " + strconv.Itoa(concordWarEndedDays) +
				` days. <a href="` + newsHost + `/concord/wars">Show active wars</a></font><br>` + "\n")
		} else {
			b.WriteString("<br><font size=3>No war is being fought. " +
				`<a href="` + newsHost + `/concord/wars?v=ended">Show wars that have ended</a></font><br>` + "\n")
		}
		finishSitePage(w, &b, siteConcord, 0)
		return
	}

	writeWarList(ctx, &b, s, wars)
	finishSitePage(w, &b, siteConcord, 0)
}

// writeWarList is one flat table, a row per war: attacker and defender (logo, name in the side's
// colour), state, score, the fee, and the report link.
func writeWarList(ctx context.Context, b *strings.Builder, s *NewsServer, wars []evedb.WarView) {
	now := time.Now().Unix()
	b.WriteString("<table width=100%>\n")
	b.WriteString("<tr>")
	hd := func(attrs, label string) {
		b.WriteString("<td bgcolor=" + cdHeader + attrs + "><font size=3 color=" + cdTitle + "><b>&nbsp;" + label + "</b></font></td>")
	}
	hd(" colspan=2", "Attacker")
	hd(" colspan=2", "Defender")
	hd(" width=140", "State")
	hd(" width=130", "Kills / ISK")
	hd(" width=130", "Fees")
	b.WriteString("</tr>\n")

	for i, v := range wars {
		shade := cdRowA
		if i%2 == 1 {
			shade = cdRowB
		}
		td := func(attrs string) string { return "<td valign=top bgcolor=" + shade + attrs + ">" }
		rep := evedb.ReportForView(ctx, s.db, v)

		state, fee, score := "<font color="+cdDim+">&mdash;</font>", "<font color="+cdDim+">&mdash;</font>", "<font color="+cdDim+">&mdash;</font>"
		if sched, err := evedb.GetWarSchedule(ctx, s.db, v.War); err == nil {
			text, color := warStateWords(sched)
			state = "<font size=3 color=" + color + "><b>" + text + "</b></font><br><font size=2 color=" + cdDim + ">declared " +
				warDate(sched.DeclaredUnix) + "</font>"
			switch {
			case sched.State == evedb.WarStateEnded:
				state += "<br><font size=2 color=" + cdDim + ">ended " + warDate(sched.EndUnix) + "</font>"
			case sched.State == evedb.WarStateWindingDown:
				state += "<br><font size=2 color=" + cdDim + ">ends " + warDate(sched.EndUnix) + "</font>"
			case sched.State == evedb.WarStateArming:
				state += "<br><font size=2 color=" + cdDim + ">hostile " + warDate(sched.HostileFrom) + "</font>"
			}
			fee = "<font size=3>" + warNextFeeWords(sched, now) + "</font>"
		}
		if sc, err := rep.Score(ctx, s.db); err == nil {
			score = "<font size=3 color=" + cdWarAtk + "><b>" + igbInt(sc.A.Kills) + "</b></font> " +
				"<font size=3 color=" + cdDim + ">&ndash;</font> " +
				"<font size=3 color=" + cdWarDef + "><b>" + igbInt(sc.B.Kills) + "</b></font><br>" +
				"<font size=3 color=" + cdWarAtk + ">" + igbISKShort(sc.A.ISK) + "</font> " +
				"<font size=3 color=" + cdDim + ">/</font> " +
				"<font size=3 color=" + cdWarDef + ">" + igbISKShort(sc.B.ISK) + "</font>"
		}

		logo := func(corpID int64) string {
			if corpID == 0 {
				return ""
			}
			return `<a href="` + warURL(v.WarID, "", 0) + `">` + igbCorpLogo(corpID, 32) + `</a>`
		}
		b.WriteString("<tr>")
		b.WriteString(td(" width=34") + logo(v.DeclaredLogo) + "</td>")
		b.WriteString(td("") + "<font size=3 color=" + cdWarAtk + "><b>" + igbText(v.DeclaredByName) + "</b></font></td>")
		b.WriteString(td(" width=34") + logo(v.AgainstLogo) + "</td>")
		b.WriteString(td("") + "<font size=3 color=" + cdWarDef + "><b>" + igbText(v.AgainstName) + "</b></font></td>")
		b.WriteString(td(" width=140") + state + "</td>")
		b.WriteString(td(" width=130") + score + "</td>")
		b.WriteString(td(" width=130") + fee + "<br><font size=3><a href=\"" + warURL(v.WarID, "", 0) +
			"\">Report &raquo;</a></font></td>")
		b.WriteString("</tr>\n")
	}
	b.WriteString("</table>\n")
	b.WriteString("<font size=2 color=" + cdDim + "><font color=" + cdWarAtk + ">Red</font> is the attacker, " +
		"<font color=" + cdWarDef + ">blue</font> the defender. Kills and ISK are what each side has destroyed.</font><br>\n")
}

// ── one war ─────────────────────────────────────────────────────────────────

func (s *NewsServer) handleConcordWar(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	q := r.URL.Query()
	id, _ := strconv.ParseInt(q.Get("id"), 10, 64)
	show := q.Get("show")
	dir := evedb.WarDirBoth
	switch show {
	case "atk":
		dir = evedb.WarDirAKilledB
	case "def":
		dir = evedb.WarDirBKilledA
	default:
		show = ""
	}
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 0 {
		page = 0
	}

	crumbs := []igbCrumb{{"Registry", newsHost + "/concord/"}, {"Wars", newsHost + "/concord/wars"}}
	var b strings.Builder

	var rep *evedb.WarReport
	if id > 0 {
		var err error
		if rep, err = evedb.LoadWarReport(ctx, s.db, id); err != nil {
			log.Printf("igb: LoadWarReport %d: %v", id, err)
			writeConcordHeader(&b, "wars", append(crumbs, igbCrumb{Label: "War"})...)
			b.WriteString("<br><font size=3>The registry is not available.</font><br>\n")
			finishSitePage(w, &b, siteConcord, 0)
			return
		}
	}
	if rep == nil {
		writeConcordHeader(&b, "wars", append(crumbs, igbCrumb{Label: "War"})...)
		b.WriteString("<br><font size=3>No such war is on record.</font><br>\n")
		finishSitePage(w, &b, siteConcord, 0)
		return
	}

	sched, serr := evedb.GetWarSchedule(ctx, s.db, rep.War)
	score, scerr := rep.Score(ctx, s.db)
	if serr != nil || scerr != nil {
		log.Printf("igb: war %d report: schedule=%v score=%v", id, serr, scerr)
	}

	writeConcordHeader(&b, "wars", append(crumbs,
		igbCrumb{Label: igbText(rep.AttackerName) + " vs " + igbText(rep.DefenderName)})...)
	b.WriteString("<br>")
	writeWarHeader(&b, rep, sched, score, serr == nil, scerr == nil)
	b.WriteString("<br>")
	if serr == nil {
		writeWarSchedule(&b, rep, sched)
		b.WriteString("<br>")
	}

	// Filter strip: the game's "Show all kills" dropdown.
	b.WriteString("<table width=100% bgcolor=" + cdHeader + "><tr><td><font size=3>&nbsp;")
	b.WriteString(igbFilterCell("All kills", warURL(id, "", 0), show == ""))
	b.WriteString(" &middot; " + igbFilterCell("Attacker's kills", warURL(id, "atk", 0), show == "atk"))
	b.WriteString(" &middot; " + igbFilterCell("Defender's kills", warURL(id, "def", 0), show == "def"))
	b.WriteString("</font></td></tr></table>\n")
	b.WriteString("<font size=2 color=" + cdDim + ">Each incident is coloured by the side that lost the ship: " +
		"<font color=" + cdWarAtk + ">red</font> the attacker, <font color=" + cdWarDef + ">blue</font> the defender.</font><br>\n")

	filter := rep.Filter(dir)
	filter.Limit, filter.Offset = igbKBPageSize, page*igbKBPageSize
	rows, err := evedb.ListKillmails(ctx, s.db, filter)
	if err != nil {
		log.Printf("igb: war %d ListKillmails: %v", id, err)
		b.WriteString("<br><font size=3>The incident list is not available.</font><br>\n")
		finishSitePage(w, &b, siteConcord, 0)
		return
	}
	if len(rows) == 0 {
		b.WriteString("<br><font size=3>No incidents between these parties are on record")
		if sched.State == evedb.WarStateArming {
			b.WriteString(" &mdash; hostilities begin " + warDateTime(sched.HostileFrom))
		}
		b.WriteString(".</font><br>\n")
		finishSitePage(w, &b, siteConcord, 0)
		return
	}

	writeWarIncidentRows(&b, rows, rep.Pair)
	total := score.A.Kills + score.B.Kills
	switch dir {
	case evedb.WarDirAKilledB:
		total = score.A.Kills
	case evedb.WarDirBKilledA:
		total = score.B.Kills
	}
	b.WriteString("<font size=3>")
	if page > 0 {
		b.WriteString(`<a href="` + warURL(id, show, page-1) + `">&laquo; newer</a> &nbsp;`)
	}
	if int64((page+1)*igbKBPageSize) < total {
		b.WriteString(`<a href="` + warURL(id, show, page+1) + `">older &raquo;</a>`)
	}
	b.WriteString("</font><br>\n")
	finishSitePage(w, &b, siteConcord, 0)
}

// writeWarHeader is the War Report's masthead as one flat table row: the attacker (logo, name,
// "Attacker", ISK and ships killed), a separator, the defender in the same shape, and the dates.
func writeWarHeader(b *strings.Builder, rep *evedb.WarReport, sched evedb.WarSchedule, score evedb.WarScore, haveSched, haveScore bool) {
	side := func(corpID int64, name, role, color string, st evedb.WarSideScore) (string, string) {
		logo := concordCorpLogo(corpID, 64)
		text := "<font size=5 color=" + cdTitle + "><b>" + igbText(name) + "</b></font><br>" +
			"<font size=3 color=" + color + "><b>" + role + "</b></font><br>"
		if haveScore {
			text += "<font size=3>" + igbInt(int64(st.ISK)) + " ISK killed</font><br>" +
				"<font size=3>" + igbInt(st.Kills) + " ships killed</font>"
		}
		return logo, text
	}
	aLogo, aText := side(rep.AttackerLogo, rep.AttackerName, "Attacker", cdWarAtk, score.A)
	dLogo, dText := side(rep.DefenderLogo, rep.DefenderName, "Defender", cdWarDef, score.B)

	dates := ""
	if haveSched {
		dates = "<font size=3 color=" + cdDim + ">Started</font> <font size=3 color=" + cdTitle + "><b>" +
			warDateTime(sched.DeclaredUnix) + "</b></font><br>"
		switch sched.State {
		case evedb.WarStateEnded:
			dates += "<font size=3 color=" + cdDim + ">Finished</font> <font size=3 color=" + cdTitle + "><b>" +
				warDateTime(sched.EndUnix) + "</b></font>"
		case evedb.WarStateWindingDown:
			dates += "<font size=3 color=" + cdDim + ">Ends</font> <font size=3 color=" + cdTitle + "><b>" +
				warDateTime(sched.EndUnix) + "</b></font>"
		default:
			dates += "<font size=3 color=" + cdDim + ">In progress</font>"
		}
	}

	b.WriteString("<table width=100% bgcolor=" + cdPanel + "><tr>")
	b.WriteString("<td valign=top width=70>" + aLogo + "</td>")
	b.WriteString("<td valign=top>" + aText + "</td>")
	b.WriteString("<td valign=middle align=center width=40><font size=5 color=" + cdDim + "><b>vs</b></font></td>")
	b.WriteString("<td valign=top width=70>" + dLogo + "</td>")
	b.WriteString("<td valign=top>" + dText + "</td>")
	b.WriteString("<td valign=top align=right width=210>" + dates + "</td>")
	b.WriteString("</tr></table>\n")
}

// writeWarSchedule is the two-panel band under the masthead: where the war stands, and the
// weekly CONCORD fee (who pays it, when the next is issued, what happens when it is not paid).
func writeWarSchedule(b *strings.Builder, rep *evedb.WarReport, s evedb.WarSchedule) {
	now := time.Now().Unix()
	kv := func(k, v string) []string {
		return []string{"<font size=3 color=" + cdDim + ">&nbsp;" + k + "</font>", "<font size=3>" + v + "</font>"}
	}
	stateText, stateColor := warStateWords(s)

	status := igbBandPanel{Title: "Status", Align: []string{"", "right"}}
	status.Rows = append(status.Rows,
		kv("State", "<font color="+stateColor+"><b>"+stateText+"</b></font>"),
		kv("Declared", warDateTime(s.DeclaredUnix)))
	switch {
	case s.State == evedb.WarStateArming:
		status.Rows = append(status.Rows, kv("Hostilities begin", warDateTime(s.HostileFrom)))
	default:
		status.Rows = append(status.Rows, kv("Hostilities began", warDateTime(s.HostileFrom)))
	}
	switch s.State {
	case evedb.WarStateWindingDown:
		status.Rows = append(status.Rows, kv("Ends", warDateTime(s.EndUnix)),
			kv("", "<font size=2 color="+cdDim+">retracted by the attacker</font>"))
	case evedb.WarStateEnded:
		status.Rows = append(status.Rows, kv("Ended", warDateTime(s.EndUnix)))
	}

	fees := igbBandPanel{Title: "Fees", Align: []string{"", "right"}}
	switch {
	case s.State == evedb.WarStateEnded:
		fees.Rows = append(fees.Rows, kv("Fees", "none &mdash; the war is over"))
	case s.Mutual:
		fees.Rows = append(fees.Rows, kv("Fees", "none &mdash; mutual war"))
	case s.State == evedb.WarStateWindingDown:
		fees.Rows = append(fees.Rows, kv("Fees", "none &mdash; retracted"))
	default:
		fees.Rows = append(fees.Rows, kv("Weekly fee", igbISKShort(s.WeeklyFee)+" ISK"))
		if s.PayerName != "" {
			fees.Rows = append(fees.Rows, kv("Paid by", igbCorpLink(s.PayerCorpID, s.PayerName)))
		}
		if s.NextFeeUnix > now {
			fees.Rows = append(fees.Rows, kv("Next fee issued", warDateTime(s.NextFeeUnix)))
		} else {
			fees.Rows = append(fees.Rows, kv("Next fee issued", "due now"))
		}
		if s.OpenBill != nil {
			fees.Rows = append(fees.Rows,
				kv("Unpaid bill", "<font color="+cdRed+">"+igbISKShort(s.OpenBill.Amount)+" ISK</font>"),
				kv("War ends if unpaid by", "<font color="+cdRed+">"+warDateTime(s.OpenBill.DueUnix)+"</font>"))
		}
	}
	writePanelBand(b, siteConcord, []igbBandPanel{status, fees})
}

// writeWarIncidentRows is the War Report's list: a dated bar between days and, per incident, the
// hull, a two-line entry (victim, then final blow, each with corporation and alliance), the
// system, and the time and assessed loss. Shade and figure follow the victim's side.
// Flat, four cells, no width on the text cell (as writeIncidentRowsTinted).
func writeWarIncidentRows(b *strings.Builder, rows []evedb.KillmailRow, pair evedb.WarKillPair) {
	b.WriteString("<table width=100%>\n")
	day := ""
	for i, k := range rows {
		if d := igbYC(k.KillTime); d != day {
			day = d
			b.WriteString("<tr><td colspan=4 bgcolor=" + cdHeader + "><font size=3 color=" + cdTitle + "><b>&nbsp;" + d +
				"</b></font></td></tr>\n")
		}
		shade, figure, label := cdRowA, cdHi, cdDim
		if i%2 == 1 {
			shade = cdRowB
		}
		switch warVictimSide(k, pair) {
		case 1:
			shade, figure, label = cdWarAtkRows[i%2], cdWarAtk, cdWarAtk
		case 2:
			shade, figure, label = cdWarDefRows[i%2], cdWarDef, cdWarDef
		}
		td := func(attrs string) string { return "<td valign=top bgcolor=" + shade + attrs + ">" }

		victim := k.VictimName
		if victim == "" {
			victim = "Unknown capsuleer"
		}
		vLine := "<font color=" + label + "><b>Victim</b></font> " + concordPilotLink(k.VictimCharID, victim) +
			warAffiliation(k.VictimCorpID, k.VictimCorpName, k.VictimAllianceName)
		if k.IsPod {
			vLine += " <font size=2 color=" + cdRed + ">CAPSULE</font>"
		}

		final := k.FinalName
		switch {
		case final == "":
			final = "&mdash;"
		case k.FinalIsNPC:
			final = igbText(final) + " <font size=2 color=" + cdDim + ">[hostile entity]</font>"
		default:
			final = concordPilotLink(k.FinalCharID, final)
		}
		fLine := "<font color=" + cdDim + "><b>Final blow</b></font> " + final +
			warAffiliation(k.FinalCorpID, k.FinalCorpName, k.FinalAllianceName)

		b.WriteString("<tr>")
		b.WriteString(td(" width=36") + `<a href="` + concordIncidentURL(k.KillID) + `">` + igbShipIcon(k.ShipTypeID, 32) +
			`</a></td>`)
		b.WriteString(td("") + "<font size=3>" + vLine + "</font><br><font size=3>" + fLine + "</font></td>")
		b.WriteString(td(" width=120") + "<font size=3>" + concordSystemLink(k.SolarSystemID, k.SystemName) + " " +
			concordSec(k.Security) + "</font><br><font size=2 color=" + cdDim + ">" + igbText(k.RegionName) + "</font></td>")
		b.WriteString(td(" width=64 align=right") + "<font size=3 color=" + cdDim + ">" + k.KillTime.UTC().Format("15:04") +
			"</font><br><font size=3 color=" + figure + "><b>" + igbISKShort(k.TotalValue) + "</b></font></td>")
		b.WriteString("</tr>\n")
	}
	b.WriteString("</table>\n")
}

// warAffiliation is "(Corporation) [Alliance]" in the War Report's form; either
// part is left out when the kill does not carry it.
func warAffiliation(corpID int64, corp, alliance string) string {
	out := ""
	if corp != "" {
		out += " (" + igbCorpLink(corpID, corp) + ")"
	}
	if alliance != "" {
		out += " [" + igbText(alliance) + "]"
	}
	return out
}
