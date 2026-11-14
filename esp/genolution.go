package esp

// Genolution: cerebral conditioning and neural implantation, and the in-game browser's skill
// service (/genolution/, alias /ig/skills).
//
// The player-facing twin of /skillwindow/<id>: the same catalogue, schedule and shop, minus the GM
// grants. Identity comes from the eve.charID header. Mutations arrive as GET params, one per
// load: ?add=<typeID>, ?rm=<pos>, ?up=<pos>, ?dn=<pos>, ?buy=<typeID>, ?clear=1, ?fix=1 and
// ?minmax=1&lvl=&steps=&cap=&replace=. View state (g, q, full) rides on every link.
//
// An action's result is a banner at the top of the view it was taken from, because the browser
// never scrolls to a #fragment. No <form> sits inside a <td>. Both: docs/galnet-igb.md#browser-limits.

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

// Genolution's palette (siteGenolution carries the same values).
const (
	gnPanel  = "#0f1c12"
	gnRowA   = "#0c170f"
	gnRowB   = "#112015"
	gnTileHi = "#1b3320" // the selected category tile
	gnNow    = "#14301b" // the now-conditioning card
	gnAccent = "#7fd36b"
	gnHi     = "#b8f08f"
	gnText   = "#cfe3d2"
	gnTitle  = "#e8f6ea"
	gnDim    = "#5f7a64"
	gnLvl    = "#f0d77a" // level numerals
	gnBlock  = "#a07a5a" // unmet requirements
	gnBad    = "#e0705a"
)

const genolutionScheduleRows = 10

func (s *NewsServer) handleGenolution(w http.ResponseWriter, r *http.Request) {
	who := igbIdentity(r)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	site := siteGenolution
	site.Nav = []igbNavItem{
		{"account", "Your account", "/genolution/"},
		{"plan", "Fast-track plan", "/genolution/?plan=1"},
	}
	q := r.URL.Query()
	active := "account"
	if q.Get("plan") == "1" {
		active = "plan"
	}

	var b strings.Builder
	writeSiteOpen(&b, site, "Genolution")
	writeSiteTopBar(&b, site, "")
	right := ""
	if who.ID != 0 {
		right = "<font size=2 color=" + gnDim + ">CLIENT</font><br><font size=4 color=" + gnTitle + "><b>" +
			igbText(who.Name) + "</b></font>"
	}
	writeSiteMasthead(&b, site, right)
	writeSiteMenu(&b, site, active)

	if who.ID == 0 {
		b.WriteString("<br><font size=4>Genolution's client services need a capsuleer identity. " +
			"Open this page from your ship's or station's Galnet terminal.</font><br>\n")
		finishSitePage(w, &b, site, 0)
		return
	}

	// View state carried through every action link.
	selGroup := parseInt64(q.Get("g"))
	search := strings.TrimSpace(q.Get("q"))
	full := q.Get("full") == "1"
	self := newsHost + "/genolution/"
	keep := ""
	if selGroup != 0 {
		keep += "&g=" + strconv.FormatInt(selGroup, 10)
	}
	if search != "" {
		keep += "&q=" + url.QueryEscape(search)
	}
	if full {
		keep += "&full=1"
	}
	if active == "plan" {
		keep += "&plan=1"
	}
	link := func(params string) string { return self + "?" + strings.TrimPrefix(params+keep, "&") }

	// At most one action per load. The banner carries its result.
	var banner string
	bad := false
	note := func(m string) {
		banner = igbText(genolutionWording(m))
		bad = strings.HasPrefix(m, "Error")
	}
	switch {
	case q.Get("add") != "":
		note(applySkillQueueAction(ctx, s.db, s.sessions, who.ID, "add", parseInt64(q.Get("add")), 0))
	case q.Get("rm") != "":
		note(applySkillQueueAction(ctx, s.db, s.sessions, who.ID, "remove", 0, int(parseInt64(q.Get("rm")))))
	case q.Get("up") != "":
		note(applySkillQueueAction(ctx, s.db, s.sessions, who.ID, "up", 0, int(parseInt64(q.Get("up")))))
	case q.Get("dn") != "":
		note(applySkillQueueAction(ctx, s.db, s.sessions, who.ID, "down", 0, int(parseInt64(q.Get("dn")))))
	case q.Get("buy") != "":
		typeID := parseInt64(q.Get("buy"))
		p, err := evedb.PurchaseAndInjectSkill(ctx, s.db, who.ID, typeID)
		if err != nil {
			banner, bad = igbText("Genolution could not complete the procedure: "+err.Error()), true
			break
		}
		if SkillInjectNotify != nil {
			if sess := s.sessions.FindByCharID(who.ID); sess != nil {
				SkillInjectNotify(sess, p.ItemID, p.NewBalance)
			}
		}
		banner = "<b>Implanted: " + igbText(p.TypeName) + ".</b> &nbsp;<a href=\"" +
			link("add="+strconv.FormatInt(typeID, 10)) + "\">Queue level I</a>" +
			" &nbsp;<font size=2 color=" + gnDim + ">" + formatISK(p.Price) + " ISK charged &middot; balance " +
			formatISK(p.NewBalance) + " ISK</font>"
	case q.Get("clear") == "1":
		note(applySkillQueueAction(ctx, s.db, s.sessions, who.ID, "clear", 0, 0))
	case q.Get("fix") == "1":
		note(applySkillQueueAction(ctx, s.db, s.sessions, who.ID, "normalize", 0, 0))
	case q.Get("minmax") == "1":
		maxLevel := int(parseInt64(q.Get("lvl")))
		if maxLevel < 1 || maxLevel > 5 {
			maxLevel = 5
		}
		maxSteps := 40
		if n, err := strconv.Atoi(q.Get("steps")); err == nil && n >= 0 && n <= 500 {
			maxSteps = n
		}
		stepHours := 0.0
		if f, err := strconv.ParseFloat(q.Get("cap"), 64); err == nil && f >= 0 {
			stepHours = f
		}
		note(applyMinMaxQueue(ctx, s.db, s.sessions, who.ID, maxLevel, maxSteps, stepHours, q.Get("replace") != ""))
	default:
		// Plain view: still advance so a due conditioning completes on load.
		if adv, err := evedb.AdvanceSkillQueue(ctx, s.db, who.ID); err == nil && adv != nil &&
			(len(adv.Completed) > 0 || adv.StartedItemID != 0) && SkillQueueNotify != nil {
			if sess := s.sessions.FindByCharID(who.ID); sess != nil {
				SkillQueueNotify(sess, adv)
			}
		}
	}
	if banner != "" {
		bg, fg := gnNow, gnHi
		if bad {
			bg, fg = "#2a1410", gnBad
		}
		b.WriteString("<table width=100% bgcolor=" + bg + "><tr><td><font size=4 color=" + fg + ">&nbsp;" + banner +
			"</font></td></tr></table>\n")
	}

	catalogue, err := evedb.ESPGetSkillCatalogue(ctx, s.db, who.ID)
	if err != nil {
		log.Printf("igb: skill catalogue charID=%d: %v", who.ID, err)
	}
	stats := evedb.ESPEffectiveLearningStats(ctx, s.db, who.ID)
	training, _ := evedb.GetSkillTrainingInfo(ctx, s.db, who.ID)
	queueEntries, err := evedb.GetSkillQueue(ctx, s.db, who.ID)
	if err != nil {
		log.Printf("igb: skills queue charID=%d: %v", who.ID, err)
	}
	_ = evedb.EstimateSkillQueueETAs(ctx, s.db, who.ID, queueEntries)
	maxQueued := make(map[int64]int)
	for _, e := range queueEntries {
		if e.TargetLevel > maxQueued[e.TypeID] {
			maxQueued[e.TypeID] = e.TargetLevel
		}
	}
	var totalSP int64
	var knownCount int
	byType := make(map[int64]evedb.ESPSkillCatalogueRow, len(catalogue))
	for _, row := range catalogue {
		byType[row.TypeID] = row
		totalSP += row.SkillPoints
		if row.Known {
			knownCount++
		}
	}

	// Unmet requirements, judged against the schedule's end state: the gate AddSkillToQueue applies,
	// so no "queue" link here is one it would refuse.
	prereqs, perr := evedb.GetSkillPrereqMap(ctx, s.db)
	if perr != nil {
		log.Printf("igb: skills GetSkillPrereqMap: %v", perr)
	}
	trained := make(map[int64]int)
	projected := make(map[int64]int)
	typeNames := make(map[int64]string, len(catalogue))
	for _, row := range catalogue {
		typeNames[row.TypeID] = row.TypeName
		if row.Known {
			trained[row.TypeID] = row.Level
			projected[row.TypeID] = row.Level
		}
	}
	if training != nil && training.TargetLevel > projected[training.TypeID] {
		projected[training.TypeID] = training.TargetLevel
	}
	for typeID, lvl := range maxQueued {
		if lvl > projected[typeID] {
			projected[typeID] = lvl
		}
	}
	blockedBy := func(typeID int64) string {
		missing := evedb.UnmetSkillReqs(prereqs[typeID], projected, nil)
		parts := make([]string, 0, len(missing))
		for _, r := range missing {
			name := typeNames[r.SkillID]
			if name == "" {
				name = strconv.FormatInt(r.SkillID, 10)
			}
			parts = append(parts, name+" "+skillRoman[min(max(r.Level, 1), 5)])
		}
		return strings.Join(parts, ", ")
	}

	prices, pricesErr := evedb.SkillMarketPrices(ctx, s.db)
	if pricesErr != nil {
		log.Printf("igb: skills SkillMarketPrices: %v", pricesErr)
	}
	balance, _ := evedb.GetCharBalance(ctx, s.db, who.ID)

	// ── account band: three equal cards ──────────────────────────────────────
	b.WriteString("<br><table width=100%><tr>")
	b.WriteString("<td width=33% valign=top bgcolor=" + gnNow + "><font size=2 color=" + gnAccent + "><b>NOW CONDITIONING</b></font><br>")
	if training != nil {
		lvl := skillRoman[min(max(training.TargetLevel, 1), 5)]
		b.WriteString("<font size=4 color=" + gnTitle + "><b>" + igbText(training.TypeName) + "</b> <font color=" + gnLvl + ">" +
			lvl + "</font></font><br>")
		b.WriteString("<font size=4 color=" + gnHi + ">" + espFmtRemaining(training.EndOfTraining) + "</font> " +
			"<font size=2 color=" + gnDim + ">left &middot; completes " + igbYCTime(evedb.FiletimeToTime(training.EndOfTraining)) +
			"</font><br>")
		if row, ok := byType[training.TypeID]; ok {
			b.WriteString("<font size=3>" + igbBar(genolutionProgress(row, training, stats), 30, gnAccent) + "</font>")
		}
	} else {
		b.WriteString("<font size=4 color=" + gnDim + ">Nothing in conditioning.</font>")
	}
	b.WriteString("</td><td width=8></td>")

	b.WriteString("<td width=33% valign=top bgcolor=" + gnPanel + "><font size=2 color=" + gnAccent + "><b>YOUR SCHEDULE</b></font><br>")
	if len(queueEntries) == 0 {
		b.WriteString("<font size=4 color=" + gnDim + ">Empty.</font><br><font size=2 color=" + gnDim +
			">Schedule conditioning from the catalogue below.</font>")
	} else {
		last := queueEntries[len(queueEntries)-1]
		b.WriteString("<font size=4 color=" + gnTitle + "><b>" + strconv.Itoa(len(queueEntries)) + "</b></font> <font size=3>scheduled</font><br>")
		if last.EstEnd > 0 {
			b.WriteString("<font size=4 color=" + gnHi + ">" + espFmtRemaining(last.EstEnd) + "</font> <font size=2 color=" + gnDim +
				">in all &middot; final completion " + igbYCTime(evedb.FiletimeToTime(last.EstEnd)) + "</font>")
		}
	}
	b.WriteString("</td><td width=8></td>")

	b.WriteString("<td valign=top bgcolor=" + gnPanel + "><font size=2 color=" + gnAccent + "><b>YOUR ACCOUNT</b></font><br>" +
		"<font size=3 color=" + gnDim + ">Skill points</font> <font size=4 color=" + gnTitle + "><b>" + igbInt(totalSP) + "</b></font><br>" +
		"<font size=3 color=" + gnDim + ">Skills known</font> <font size=4 color=" + gnTitle + "><b>" + strconv.Itoa(knownCount) +
		"</b></font><br><font size=3 color=" + gnDim + ">Wallet</font> <font size=4 color=" + gnHi + "><b>" + formatISK(balance) +
		" ISK</b></font></td></tr></table>\n")

	// ── the fast-track planner, on its own tab ──────────────────────────────
	if active == "plan" {
		writeGenolutionPlanner(&b, self, selGroup, search)
	}

	// ── schedule ────────────────────────────────────────────────────────────
	b.WriteString("<br><table width=100% bgcolor=" + gnPanel + "><tr><td><font size=4><b>&nbsp;<font color=" + gnTitle +
		">Conditioning schedule</font></b></font>")
	if len(queueEntries) > 0 {
		b.WriteString(" &nbsp; <font size=2><a href=\"" + link("fix=1") +
			"\" alt=\"Re-sort the schedule so nothing sits ahead of its own lower levels or its requirements\">repair order</a> &middot; " +
			"<a href=\"" + link("clear=1") + "\">clear</a></font>")
	}
	b.WriteString("</td></tr></table>\n")
	if len(queueEntries) > 0 {
		shown := queueEntries
		if !full && len(shown) > genolutionScheduleRows {
			shown = shown[:genolutionScheduleRows]
		}
		b.WriteString("<table width=100%>\n")
		for i, e := range shown {
			shade := gnRowA
			if i%2 == 1 {
				shade = gnRowB
			}
			pos := strconv.Itoa(e.Position)
			var acts []string
			if i > 0 {
				acts = append(acts, `<a href="`+link("up="+pos)+`">up</a>`)
			}
			if i < len(queueEntries)-1 {
				acts = append(acts, `<a href="`+link("dn="+pos)+`">down</a>`)
			}
			acts = append(acts, `<a href="`+link("rm="+pos)+`">remove</a>`)
			rem, done := "&mdash;", ""
			if e.EstEnd > 0 {
				rem = espFmtRemaining(e.EstEnd)
				done = igbYCTime(evedb.FiletimeToTime(e.EstEnd))
			}
			b.WriteString("<tr><td width=24 bgcolor=" + shade + "><font size=2 color=" + gnDim + ">" + pos + "</font></td>" +
				"<td bgcolor=" + shade + "><font size=3>" + igbText(e.TypeName) + " <font color=" + gnLvl + "><b>" +
				skillRoman[min(max(e.TargetLevel, 1), 5)] + "</b></font></font></td>" +
				"<td width=110 bgcolor=" + shade + "><font size=3 color=" + gnHi + ">" + rem + "</font></td>" +
				"<td width=150 bgcolor=" + shade + "><font size=2 color=" + gnDim + ">" + done + "</font></td>" +
				"<td width=130 align=right bgcolor=" + shade + "><font size=2>" + strings.Join(acts, " &middot; ") +
				"&nbsp;</font></td></tr>\n")
		}
		b.WriteString("</table>\n")
		if len(queueEntries) > len(shown) {
			b.WriteString(`<font size=2>&nbsp;<a href="` + self + "?" + strings.TrimPrefix(strings.ReplaceAll(keep, "&full=1", "")+
				"&full=1", "&") + `">show all ` + strconv.Itoa(len(queueEntries)) + ` scheduled</a></font><br>` + "\n")
		} else if full && len(queueEntries) > genolutionScheduleRows {
			b.WriteString(`<font size=2>&nbsp;<a href="` + self + "?" + strings.TrimPrefix(strings.ReplaceAll(keep, "&full=1", ""), "&") +
				`">show the first ` + strconv.Itoa(genolutionScheduleRows) + `</a></font><br>` + "\n")
		}
	}

	// ── catalogue ───────────────────────────────────────────────────────────
	b.WriteString("<br><table width=100% bgcolor=" + gnPanel + "><tr><td><font size=4><b>&nbsp;<font color=" + gnTitle +
		">Catalogue</font></b></font> <font size=2 color=" + gnDim + ">Every procedure Genolution offers. " +
		"Prices are the market average plus Genolution's fee.</font></td></tr></table>\n")
	b.WriteString(`<form action="` + self + `" method="get"><font size=2>&nbsp;Find a skill: </font><input type="text" name="q" value="` +
		htmlAttrEscape(search) + `" size="24"> <input type="submit" value="Search">`)
	if full {
		b.WriteString(`<input type="hidden" name="full" value="1">`)
	}
	b.WriteString("</form>\n")

	type group struct {
		id           int64
		name         string
		known, total int
	}
	var groups []group
	gIdx := make(map[int64]int)
	for _, row := range catalogue {
		i, ok := gIdx[row.GroupID]
		if !ok {
			i = len(groups)
			gIdx[row.GroupID] = i
			groups = append(groups, group{id: row.GroupID, name: row.GroupName})
		}
		groups[i].total++
		if row.Known {
			groups[i].known++
		}
	}

	// Category tiles, four across. A tile link keeps the schedule state but
	// drops the search: picking a category is a new question.
	tileKeep := ""
	if full {
		tileKeep = "&full=1"
	}
	type tile struct {
		label, cnt, href string
		active, full     bool
	}
	tiles := []tile{{"Your skills", strconv.Itoa(knownCount), self + strings.Replace(tileKeep, "&", "?", 1),
		selGroup == 0 && search == "", true}}
	for _, g := range groups {
		tiles = append(tiles, tile{igbText(g.name), strconv.Itoa(g.known) + "/" + strconv.Itoa(g.total),
			self + "?g=" + strconv.FormatInt(g.id, 10) + tileKeep, g.id == selGroup && search == "", g.known == g.total && g.total > 0})
	}
	b.WriteString("<table width=100%>\n")
	for i, t := range tiles {
		if i%4 == 0 {
			if i > 0 {
				b.WriteString("</tr>\n")
			}
			b.WriteString("<tr>")
		}
		cnt := gnDim
		if t.full {
			cnt = gnAccent
		}
		if t.active {
			b.WriteString("<td width=25% bgcolor=" + gnTileHi + "><font size=2>&nbsp;<font color=" + gnTitle + "><b>" + t.label +
				"</b></font> <font color=" + cnt + ">" + t.cnt + "</font></font></td>")
		} else {
			b.WriteString("<td width=25% bgcolor=" + gnPanel + "><font size=2>&nbsp;<a href=\"" + t.href + "\">" + t.label +
				"</a> <font color=" + cnt + ">" + t.cnt + "</font></font></td>")
		}
	}
	for pad := len(tiles) % 4; pad != 0 && pad < 4; pad++ {
		b.WriteString("<td width=25%></td>")
	}
	b.WriteString("</tr>\n</table><br>\n")

	// One catalogue row: a known skill offers its next level, an unknown one reads like a product
	// (name, rank, price, Purchase & implant). Implantation requirements are judged on trained
	// levels, the gate the client applies to a skillbook in the hangar.
	writeRow := func(row evedb.ESPSkillCatalogueRow, shade string) {
		td := "<td bgcolor=" + shade + ">"
		name := igbText(row.TypeName)
		rank := "<font size=2 color=" + gnDim + ">rank " + strconv.FormatInt(row.Rank, 10) + "</font>"
		if !row.Known {
			cell := "<font size=2 color=" + gnDim + ">not offered</font>"
			if blocked := evedb.UnmetInjectReqs(prereqs[row.TypeID], trained, typeNames); blocked != "" {
				cell = "<font size=2 color=" + gnBlock + ">requires " + igbText(blocked) + "</font>"
			} else if pq := prices[row.TypeID]; pq.Price > 0 {
				cell = "<font size=3 color=" + gnHi + ">" + formatISK(pq.Price) + " ISK</font>"
				if balance < pq.Price {
					cell += " <font size=2 color=" + gnBlock + ">(beyond your means)</font>"
				} else {
					cell += " &nbsp;<font size=3><b><a href=\"" + link("buy="+strconv.FormatInt(row.TypeID, 10)) +
						"\" alt=\"" + htmlAttrEscape("Market average "+formatISK(pq.Average)+" ISK plus Genolution's fee") +
						"\">Purchase &amp; implant</a></b></font>"
				}
			}
			b.WriteString("<tr>" + td + "<font size=3 color=#2c3a2e><b>|||||</b></font></td>" + td + "<font size=3>" + name +
				"</font> " + rank + "</td><td bgcolor=" + shade + " colspan=3>" + cell + "</td></tr>\n")
			return
		}
		pips := "<font color=" + gnAccent + "><b>" + strings.Repeat("|", row.Level) + "</b></font>" +
			"<font color=#2c3a2e><b>" + strings.Repeat("|", 5-row.Level) + "</b></font>"
		next, eta := skillNextTraining(row, stats, training, maxQueued)
		switch {
		case next == 0:
			b.WriteString("<tr>" + td + "<font size=3>" + pips + "</font></td>" + td + "<font size=3>" + name + "</font> " + rank +
				"</td><td bgcolor=" + shade + " colspan=3><font size=2 color=" + gnAccent + ">fully conditioned</font></td></tr>\n")
		case blockedBy(row.TypeID) != "":
			b.WriteString("<tr>" + td + "<font size=3>" + pips + "</font></td>" + td + "<font size=3>" + name + "</font> " + rank +
				"</td><td bgcolor=" + shade + " colspan=3><font size=2 color=" + gnBlock + ">requires " +
				igbText(blockedBy(row.TypeID)) + "</font></td></tr>\n")
		default:
			b.WriteString("<tr>" + td + "<font size=3>" + pips + "</font></td>" + td + "<font size=3>" + name + "</font> " + rank +
				"</td>" + td + "<font size=3 color=" + gnLvl + "><b>" + skillRoman[next] + "</b></font></td>" + td +
				"<font size=3>" + igbText(eta) + "</font></td><td bgcolor=" + shade + " align=right><font size=2><a href=\"" +
				link("add="+strconv.FormatInt(row.TypeID, 10)) + "\" alt=\"Schedule " + htmlAttrEscape(row.TypeName) + " " +
				skillRoman[next] + "\">schedule</a>&nbsp;</font></td></tr>\n")
		}
	}
	writeTable := func(rows []evedb.ESPSkillCatalogueRow) {
		b.WriteString("<table width=100%>\n")
		for i, row := range rows {
			shade := gnRowA
			if i%2 == 1 {
				shade = gnRowB
			}
			writeRow(row, shade)
		}
		b.WriteString("</table>\n")
	}

	lower := strings.ToLower(search)
	switch {
	case lower != "":
		var shown []evedb.ESPSkillCatalogueRow
		for _, row := range catalogue {
			if strings.Contains(strings.ToLower(row.TypeName), lower) {
				shown = append(shown, row)
			}
		}
		if len(shown) == 0 {
			b.WriteString("<font size=3>No procedure by that name.</font><br>\n")
			break
		}
		b.WriteString("<font size=2 color=" + gnDim + ">Results for &quot;" + igbText(search) + "&quot;</font><br>\n")
		writeTable(shown)
	case selGroup != 0:
		var shown []evedb.ESPSkillCatalogueRow
		for _, row := range catalogue {
			if row.GroupID == selGroup {
				shown = append(shown, row)
			}
		}
		if len(shown) == 0 {
			b.WriteString("<font size=3>Nothing in this category.</font><br>\n")
			break
		}
		writeTable(shown)
	default:
		if knownCount == 0 {
			b.WriteString("<font size=3 color=" + gnDim + ">No skills implanted yet &mdash; pick a category above to browse the catalogue.</font><br>\n")
			break
		}
		for _, g := range groups {
			if g.known == 0 {
				continue
			}
			var shown []evedb.ESPSkillCatalogueRow
			for _, row := range catalogue {
				if row.GroupID == g.id && row.Known {
					shown = append(shown, row)
				}
			}
			b.WriteString("<font size=2 color=" + gnTitle + "><b>" + igbText(strings.ToUpper(g.name)) + "</b></font> <font size=2 color=" +
				gnDim + ">" + strconv.Itoa(g.known) + "/" + strconv.Itoa(g.total) + "</font><br>\n")
			writeTable(shown)
		}
	}
	finishSitePage(w, &b, site, 0)
}

// writeGenolutionPlanner is the fast-track plan form. It sits outside any table: the panel above
// it closes before the form opens.
func writeGenolutionPlanner(b *strings.Builder, self string, selGroup int64, search string) {
	b.WriteString("<br><table width=100% bgcolor=" + gnPanel + "><tr><td><font size=4><b>&nbsp;<font color=" + gnTitle +
		">Fast-track plan</font></b></font><br><font size=2 color=" + gnDim + ">&nbsp;Genolution schedules every remaining " +
		"level of every skill you hold, the quickest first &mdash; all the first levels before any second level &mdash; so the " +
		"most skills complete per hour. Requirements are respected.</font></td></tr></table>\n")
	b.WriteString(`<form action="` + self + `" method="get"><input type="hidden" name="minmax" value="1">` +
		`<input type="hidden" name="plan" value="1">`)
	if selGroup != 0 {
		b.WriteString(`<input type="hidden" name="g" value="` + strconv.FormatInt(selGroup, 10) + `">`)
	}
	if search != "" {
		b.WriteString(`<input type="hidden" name="q" value="` + htmlAttrEscape(search) + `">`)
	}
	b.WriteString(`<font size=2>&nbsp;Up to level </font><select name="lvl">`)
	for lvl := 1; lvl <= 5; lvl++ {
		sel := ""
		if lvl == 5 {
			sel = " selected"
		}
		b.WriteString(`<option value="` + strconv.Itoa(lvl) + `"` + sel + `>` + skillRoman[lvl] + `</option>`)
	}
	b.WriteString(`</select><font size=2> &nbsp;At most </font><input type="text" name="steps" value="40" size="3">` +
		`<font size=2> steps &nbsp;Skip any step longer than </font><select name="cap">`)
	for i, c := range []struct{ v, label string }{
		{"0", "no limit"}, {"1", "1 hour"}, {"4", "4 hours"}, {"12", "12 hours"}, {"24", "1 day"}, {"72", "3 days"}, {"168", "7 days"},
	} {
		sel := ""
		if i == 0 {
			sel = " selected"
		}
		b.WriteString(`<option value="` + c.v + `"` + sel + `>` + c.label + `</option>`)
	}
	b.WriteString(`</select><br>` + "\n" +
		`&nbsp;<input type="checkbox" name="replace" value="1" checked><font size=2> Replace my current schedule &nbsp;</font>` +
		`<input type="submit" value="Build my plan"><br></form>` + "\n")
}

// genolutionProgress is how far the current conditioning has come, from the
// skill points it started at and the client's learning rate.
func genolutionProgress(row evedb.ESPSkillCatalogueRow, t *evedb.SkillTrainingInfo, stats map[int64]float64) float64 {
	rate := evedb.ESPSkillSPPerMinute(stats, row.PrimaryAttrID, row.SecondaryAttrID)
	if rate <= 0 || t.TargetLevel < 1 {
		return 0
	}
	start := float64(row.SkillPoints)
	if prev := evedb.ESPSkillSPForLevel(row.Rank, t.TargetLevel-1); prev > start {
		start = prev
	}
	totalMin := (evedb.ESPSkillSPForLevel(row.Rank, t.TargetLevel) - start) / rate
	if totalMin <= 0 {
		return 1
	}
	remMin := time.Until(evedb.FiletimeToTime(t.EndOfTraining)).Minutes()
	f := 1 - remMin/totalMin
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// genolutionWording turns the shared queue helpers' messages (written for the
// admin panel) into Genolution's voice.
func genolutionWording(m string) string {
	r := strings.NewReplacer(
		"min-max steps", "fast-track steps",
		"Queued ", "Scheduled ",
		"Queue entry removed.", "Removed from your schedule.",
		"Queue reordered.", "Schedule reordered.",
		"Queue cleared.", "Schedule cleared.",
		"Queue order already valid.", "Your schedule was already in order.",
		"Queue repaired", "Schedule repaired",
		"Training started, ends", "Conditioning started, completes",
		"finished training(s) completed.", "conditioning(s) completed.",
		"Error: ", "",
	)
	return fmt.Sprint(r.Replace(m))
}
