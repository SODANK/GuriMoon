package esp

// Galnet: the front door of the in-game browser (/galnet/, /ig/, and /mb/news.asp whenever the
// browser sends a pilot identity).
//
// A directory with one tile per site: emblem, purpose and one live line. Each line is its own
// short-deadline query and is left out when it fails. The page sends no Refresh header.
// See docs/galnet-igb.md#caching-and-refresh.

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	evedb "gurimoon/db"
)

const galnetSnippetTimeout = 1500 * time.Millisecond

// moduleGalnetTiles is appended to from init() by overlay modules that add files to this package.
// Each builds one directory tile with a live line under galnetSnippetTimeout; tiles land after the
// core sites and before the Corporate Portal. A plain link uses IGBModuleNav instead.
var moduleGalnetTiles []func(s *NewsServer, ctx context.Context) galnetTile

// galnetTile is one entry of the directory.
type galnetTile struct {
	Name, Purpose, Path string
	Emblem              string // an <img>, or "" for a wordmark-only tile
	Snippet             string
}

// handleGalnet serves the front door.
func (s *NewsServer) handleGalnet(w http.ResponseWriter, r *http.Request) {
	who := igbIdentity(r)
	log.Printf("igb: galnet from %s char=%q (%d)", r.RemoteAddr, who.Name, who.ID)
	site := siteGalnet
	p := site.Pal

	var b strings.Builder
	writeSiteOpen(&b, site, "Galnet")

	// Masthead: the wordmark, the relay this terminal is reaching it through,
	// and the capsuleer it is talking to.
	relay := galnetRelay(who)
	b.WriteString("<table width=100% bgcolor=" + p.Panel + "><tr><td valign=middle>")
	b.WriteString("<font size=7 color=" + p.Title + "><b>GALNET</b></font><br>")
	b.WriteString("<font size=3 color=" + p.Accent + ">" + site.Tagline + "</font><br>")
	b.WriteString("<font size=3 color=" + p.Dim + ">")
	if relay != "" {
		b.WriteString("Connected via the " + igbText(relay) + " relay &middot; ")
	}
	b.WriteString(igbYCTime(time.Now()) + "</font></td>")
	if who.ID != 0 {
		b.WriteString("<td align=right valign=middle><font size=2 color=" + p.Dim + ">CAPSULEER</font><br>" +
			"<font size=4 color=" + p.Title + "><b>" + concordPilotLink(who.ID, who.Name) + "</b></font>")
		if who.CorpName != "" {
			b.WriteString("<br><font size=2>" + igbText(who.CorpName) + "</font>")
		}
		b.WriteString("</td><td width=72 valign=middle>" + igbPortrait(who.ID, 64) + "</td>")
	}
	b.WriteString("</tr></table>\n")

	// Every live line and panel is an independent short query. All start now and are collected as
	// the page is laid out, so the page waits for the slowest one.
	ctx := r.Context()
	headline := goSnip(ctx, s.galnetHeadline)
	training := goSnip(ctx, func(c context.Context) string { return s.galnetTraining(c, who.ID) })
	securityLine := goSnip(ctx, s.galnetSecurityLine)
	summitLine := goSnip(ctx, s.galnetSummitLine)
	moduleTiles := make([]func() galnetTile, len(moduleGalnetTiles))
	for i, tile := range moduleGalnetTiles {
		moduleTiles[i] = goSnip(ctx, func(c context.Context) galnetTile { return tile(s, c) })
	}
	corpTile := goSnip(ctx, func(c context.Context) galnetTile { return s.galnetCorpTile(c, who) })
	network := []func() igbBandPanel{goSnip(ctx, s.galnetNewsPanel)}
	for _, pn := range moduleGalnetPanels {
		network = append(network, goSnip(ctx, func(c context.Context) igbBandPanel { return pn(s, c) }))
	}
	network = append(network, goSnip(ctx, s.galnetIncidentPanel))
	summitPanel := goSnip(ctx, s.galnetSummitPanel)
	var terminal []func() igbBandPanel
	if who.ID != 0 {
		terminal = []func() igbBandPanel{
			goSnip(ctx, func(c context.Context) igbBandPanel { return s.galnetSchedulePanel(c, who.ID) }),
			goSnip(ctx, func(c context.Context) igbBandPanel { return s.galnetCorpPanel(c, who.ID) }),
			goSnip(ctx, func(c context.Context) igbBandPanel { return s.galnetRecordPanel(c, who.ID) }),
		}
	}

	tiles := []galnetTile{
		{Name: "The Scope", Purpose: "Independent news for New Eden", Path: "/scope/",
			Emblem: siteEmblem(siteScope, 48), Snippet: headline()},
		{Name: "Genolution Skill Services", Purpose: "Cerebral conditioning and neural implantation", Path: "/genolution/",
			Emblem: siteEmblem(siteGenolution, 48), Snippet: training()},
		{Name: "DED Incident Registry", Purpose: "The public record of capsuleer losses", Path: "/concord/",
			Emblem: siteEmblem(siteConcord, 48), Snippet: securityLine()},
		{Name: "Intergalactic Summit", Purpose: "The open floor where capsuleers address one another", Path: "/summit/",
			Emblem: siteEmblem(siteSummit, 48), Snippet: summitLine()},
	}
	for _, tile := range moduleTiles {
		tiles = append(tiles, tile())
	}
	tiles = append(tiles, corpTile())
	for _, e := range IGBModuleNav {
		tiles = append(tiles, galnetTile{Name: igbText(e.Label), Purpose: "", Path: e.Path})
	}

	b.WriteString("<br><font size=4 color=" + p.Title + "><b>DIRECTORY</b></font><br>\n")
	writeGalnetTiles(&b, site, tiles)

	// Below the directory: what each site is saying now and, for a capsuleer, what concerns them.
	// A failed panel renders its empty line.
	b.WriteString("<br><font size=4 color=" + p.Title + "><b>ON THE NETWORK</b></font><br>\n")
	writePanelBand(&b, site, collectPanels(network))
	// The Summit's topics need the width of the page, so they are a band of their own.
	writePanelBand(&b, site, []igbBandPanel{summitPanel()})
	if who.ID != 0 {
		b.WriteString("<br><font size=4 color=" + p.Title + "><b>YOUR TERMINAL</b></font><br>\n")
		writePanelBand(&b, site, collectPanels(terminal))
	}
	b.WriteString("<br><table width=100% bgcolor=" + p.Header + "><tr><td><font size=2 color=" + p.Dim +
		">Galnet carries the public sites of New Eden's institutions to every capsuleer terminal. " +
		"Each site is run by its own organisation and speaks for it alone.</font></td></tr></table>\n")
	b.WriteString("</body></html>\n")
	writeIGBResponse(w, b.String(), 0)
}

// writeGalnetTiles lays the directory out two across. Each tile is two cells (emblem and text),
// so the grid is one flat table.
func writeGalnetTiles(b *strings.Builder, site igbSite, tiles []galnetTile) {
	p := site.Pal
	b.WriteString("<table width=100%>\n")
	for i, t := range tiles {
		if i%2 == 0 {
			b.WriteString("<tr>")
		}
		bg := p.RowA
		if (i/2)%2 == 1 {
			bg = p.RowB
		}
		href := newsHost + t.Path
		emblem := "&nbsp;"
		if t.Emblem != "" {
			emblem = `<a href="` + href + `">` + t.Emblem + `</a>`
		}
		b.WriteString("<td width=56 valign=top bgcolor=" + bg + ">" + emblem + "</td>")
		b.WriteString("<td width=44% valign=top bgcolor=" + bg + "><font size=5><b><a href=\"" + href + "\">" + t.Name +
			"</a></b></font><br>")
		if t.Purpose != "" {
			b.WriteString("<font size=2 color=" + p.Dim + ">" + t.Purpose + "</font><br>")
		}
		if t.Snippet != "" {
			b.WriteString("<font size=3>" + t.Snippet + "</font>")
		}
		b.WriteString("</td>")
		if i%2 == 1 || i == len(tiles)-1 {
			if i%2 == 0 {
				b.WriteString("<td width=56 bgcolor=" + p.Page + "></td><td bgcolor=" + p.Page + "></td>")
			}
			b.WriteString("</tr>\n")
		}
	}
	b.WriteString("</table>\n")
}

// galnetRelay names where the terminal is: the station's first two name
// parts ("Kakakela VI - Moon 5"), else the solar system.
func galnetRelay(who igbChar) string {
	if who.Station != "" {
		parts := strings.Split(who.Station, " - ")
		if len(parts) > 2 {
			parts = parts[:2]
		}
		return strings.Join(parts, " – ")
	}
	return who.SolarSystem
}

// ── the live lines, shared with The Scope's strips ──────────────────────────

func (s *NewsServer) galnetHeadline(ctx context.Context) string {
	a, err := evedb.GetPublishedNews(s.db, 1)
	if err != nil || len(a) == 0 {
		return ""
	}
	return `Latest: <a href="` + scopeStoryURL(a[0].ID) + `">` + igbText(a[0].Title) + `</a>`
}

func (s *NewsServer) galnetTraining(ctx context.Context, charID int64) string {
	if charID == 0 {
		return ""
	}
	t, err := evedb.GetSkillTrainingInfo(ctx, s.db, charID)
	if err != nil {
		return ""
	}
	if t == nil {
		return "Nothing in conditioning."
	}
	lvl := ""
	if t.TargetLevel >= 1 && t.TargetLevel <= 5 {
		lvl = " " + skillRoman[t.TargetLevel]
	}
	return "Conditioning " + igbText(t.TypeName) + lvl + " &middot; <font color=" + siteGenolution.Pal.Accent + ">" +
		espFmtRemaining(t.EndOfTraining) + "</font> left"
}

// galnetSecurityLine is CONCORD's 24-hour count, linked to the registry.
func (s *NewsServer) galnetSecurityLine(ctx context.Context) string {
	n, isk, err := evedb.KillboardTotals(ctx, s.db, time.Now().Add(-24*time.Hour), false)
	if err != nil {
		return ""
	}
	return `<a href="` + newsHost + `/concord/">` + igbInt(n) + ` capsuleer losses</a> recorded in the last 24 hours, ` +
		igbISKShort(isk) + ` ISK assessed`
}
