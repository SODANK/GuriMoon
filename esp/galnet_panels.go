package esp

// The Galnet front door's live bands (galnet.go): "On the network" (The Scope's latest, the
// Exchange's movers, CONCORD's latest incidents) and, for a capsuleer, "Your terminal" (their
// conditioning schedule, corporation and public record).
//
// Everything here is public or already shown to the same header on its own site, so the front
// door reveals nothing its sites do not.

import (
	"context"
	"log"
	"strconv"
	"time"

	evedb "gurimoon/db"
)

const galnetPanelRows = 6

// goSnip starts f under the snippet deadline on its own goroutine and returns a func that waits
// for its result, so a page can start every query at once. A panic yields the zero value (an
// empty line); on this goroutine net/http would not recover it and it would take the server down.
func goSnip[T any](ctx context.Context, f func(context.Context) T) func() T {
	ch := make(chan T, 1)
	go func() {
		var v T
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[galnet] snippet panicked: %v", r)
			}
			ch <- v
		}()
		c, cancel := context.WithTimeout(ctx, galnetSnippetTimeout)
		defer cancel()
		v = f(c)
	}()
	return func() T { return <-ch }
}

// collectPanels waits for every panel of one band, in order.
func collectPanels(waits []func() igbBandPanel) []igbBandPanel {
	out := make([]igbBandPanel, len(waits))
	for i, wait := range waits {
		out[i] = wait()
	}
	return out
}

// galnetTitle is a panel title linked to its site.
func galnetTitle(label, path string) string {
	return `<a href="` + newsHost + path + `">` + label + `</a>`
}

// galnetCorpTile is the directory's Corporate Portal tile: the viewer's own
// corporation's logo and name when the identity names a pilot.
func (s *NewsServer) galnetCorpTile(ctx context.Context, who igbChar) galnetTile {
	tile := galnetTile{Name: "Corporate Portal", Purpose: "Your corporation's own pages", Path: "/corp/"}
	if who.ID == 0 {
		return tile
	}
	corpID, err := evedb.GetCharCorpID(ctx, s.db, who.ID)
	if err != nil || corpID == 0 {
		return tile
	}
	tile.Emblem = igbCorpLogo(corpID, 48)
	name := who.CorpName
	if corp, err := evedb.ESPGetCorpFull(ctx, s.db, corpID); err == nil && corp != nil && corp.CorporationName != "" {
		name = corp.CorporationName
		if corp.TickerName != "" {
			name += " [" + corp.TickerName + "]"
		}
	}
	if name != "" {
		tile.Snippet = igbText(name)
	}
	return tile
}

func (s *NewsServer) galnetNewsPanel(ctx context.Context) igbBandPanel {
	pn := igbBandPanel{Title: galnetTitle("The Scope", "/scope/"), Note: "latest stories",
		Align: []string{"", ""}, Width: []string{"50", ""}, Empty: "Quiet on the wire."}
	news, err := evedb.GetPublishedNews(s.db, galnetPanelRows)
	if err != nil {
		return pn
	}
	for _, a := range news {
		pn.Rows = append(pn.Rows, []string{
			"<font size=3 color=" + siteGalnet.Pal.Dim + ">" + a.Date.UTC().Format("01-02") + "</font>",
			`<font size=3><a href="` + scopeStoryURL(a.ID) + `">` + igbText(igbTrim(a.Title, 60)) + `</a></font>`,
		})
	}
	return pn
}

func (s *NewsServer) galnetIncidentPanel(ctx context.Context) igbBandPanel {
	pn := igbBandPanel{Title: galnetTitle("DED", "/concord/"), Note: "latest incidents",
		Align: []string{"", "", "right"}, Width: []string{"36", "", ""}, Empty: "No losses on record."}
	rows, err := evedb.ListKillmails(ctx, s.db, evedb.KillmailFilter{Limit: galnetPanelRows})
	if err != nil {
		return pn
	}
	for _, k := range rows {
		victim := k.VictimName
		if victim == "" {
			victim = "Unknown capsuleer"
		}
		pn.Rows = append(pn.Rows, []string{
			`<a href="` + concordIncidentURL(k.KillID) + `">` + igbShipIcon(k.ShipTypeID, 32) + `</a>`,
			"<font size=3>" + igbKillLink(k, igbText(k.ShipTypeName)) + "</font><br><font size=2 color=" +
				siteGalnet.Pal.Dim + ">" + igbText(victim) + " &middot; " + igbText(k.SystemName) + "</font>",
			"<font size=3 color=" + siteGalnet.Pal.Up + "><b>" + igbISKShort(k.TotalValue) + "</b></font>",
		})
	}
	return pn
}

func (s *NewsServer) galnetSchedulePanel(ctx context.Context, charID int64) igbBandPanel {
	pn := igbBandPanel{Title: galnetTitle("Conditioning", "/genolution/"), Note: "Genolution Skill Services",
		Align: []string{"", "right"}, Empty: "Nothing scheduled."}
	dim := siteGalnet.Pal.Dim
	if t, err := evedb.GetSkillTrainingInfo(ctx, s.db, charID); err == nil && t != nil {
		pn.Rows = append(pn.Rows, []string{
			"<font size=3 color=" + siteGenolution.Pal.Accent + "><b>" + igbText(t.TypeName) + " " +
				skillRoman[min(max(t.TargetLevel, 1), 5)] + "</b></font><br><font size=2 color=" + dim + ">in conditioning</font>",
			"<font size=3 color=" + siteGenolution.Pal.Accent + ">" + espFmtRemaining(t.EndOfTraining) + "</font>",
		})
	}
	queue, err := evedb.GetSkillQueue(ctx, s.db, charID)
	if err != nil {
		return pn
	}
	_ = evedb.EstimateSkillQueueETAs(ctx, s.db, charID, queue)
	for _, e := range queue {
		if len(pn.Rows) >= galnetPanelRows {
			break
		}
		left := "&mdash;"
		if e.EstEnd > 0 {
			left = espFmtRemaining(e.EstEnd)
		}
		pn.Rows = append(pn.Rows, []string{
			"<font size=3>" + igbText(e.TypeName) + " " + skillRoman[min(max(e.TargetLevel, 1), 5)] + "</font>",
			"<font size=3 color=" + dim + ">" + left + "</font>",
		})
	}
	if len(queue) > 0 && len(pn.Rows) >= galnetPanelRows {
		pn.Note = strconv.Itoa(len(queue)) + " scheduled"
	}
	return pn
}

func (s *NewsServer) galnetCorpPanel(ctx context.Context, charID int64) igbBandPanel {
	pn := igbBandPanel{Title: galnetTitle("Your corporation", "/corp/"), Align: []string{"", ""},
		Width: []string{"110", ""}, Empty: "No corporation on record."}
	corpID, err := evedb.GetCharCorpID(ctx, s.db, charID)
	if err != nil || corpID == 0 {
		return pn
	}
	corp, err := evedb.ESPGetCorpFull(ctx, s.db, corpID)
	if err != nil || corp == nil {
		return pn
	}
	dim := siteGalnet.Pal.Dim
	kv := func(label, value string) {
		if value == "" {
			return
		}
		pn.Rows = append(pn.Rows, []string{"<font size=3 color=" + dim + ">" + label + "</font>", "<font size=3>" + value + "</font>"})
	}
	name := igbText(corp.CorporationName)
	if corp.TickerName != "" {
		name += " [" + igbText(corp.TickerName) + "]"
	}
	pn.Rows = append(pn.Rows, []string{`<a href="` + newsHost + `/corp/">` + igbCorpLogo(corpID, 64) + `</a>`,
		"<font size=4><b>" + name + "</b></font><br><font size=2 color=" + dim + ">" + igbText(corp.AllianceName) + "</font>"})
	kv("Chief executive", concordPilotLink(corp.CEOID, corp.CEOName))
	members := igbInt(corp.Pilots)
	if corp.MemberLimit > 0 {
		members += " of " + igbInt(corp.MemberLimit)
	}
	kv("Capsuleers", members)
	kv("Headquarters", igbText(igbTrim(corp.StationName, 40)))
	for _, fact := range moduleCorpFacts {
		if label, html, ok := fact(ctx, s, corpID, corp.TickerName, true); ok {
			kv(label, html)
		}
	}
	return pn
}

func (s *NewsServer) galnetRecordPanel(ctx context.Context, charID int64) igbBandPanel {
	pn := igbBandPanel{Title: galnetTitle("Your public record", "/concord/capsuleer?id="+strconv.FormatInt(charID, 10)),
		Note: "DED", Align: []string{"", "right"}, Empty: "The registry holds nothing on you."}
	st, err := evedb.ActorKillStats(ctx, s.db, evedb.PilotActor(charID), time.Time{})
	if err != nil || st == nil || !st.HasRecord() {
		return pn
	}
	dim := siteGalnet.Pal.Dim
	row := func(label, value string) {
		pn.Rows = append(pn.Rows, []string{"<font size=3 color=" + dim + ">" + label + "</font>", "<font size=3>" + value + "</font>"})
	}
	row("Engagements", "<b>"+igbInt(st.Kills)+"</b>")
	row("Losses", "<font color="+siteGalnet.Pal.Down+"><b>"+igbInt(st.Losses)+"</b></font>")
	row("Inflicted", igbISKShort(st.ISKDestroyed)+" ISK")
	row("Lost", igbISKShort(st.ISKLost)+" ISK")
	row("Efficiency", strconv.FormatFloat(st.Efficiency(), 'f', 1, 64)+"%")
	if !st.LastLoss.IsZero() {
		row("Last loss", igbRelTime(st.LastLoss))
	}
	return pn
}
