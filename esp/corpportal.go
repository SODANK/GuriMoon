package esp

// The Corporate Portal: the viewer's own corporation on Galnet.
//
//	/corp/                  the viewer's corporation: Overview plus module sections
//	/corp/profile?id=N      any capsuleer corporation's public Overview
//	/corp/directory         capsuleer corporations, and whether they are recruiting
//
// The viewer's corporation comes from eve.charID through the database; the eve.corpID header is
// a hint. A pilot in a chartered (NPC) corporation gets that corporation's public profile and the
// directory. Public information only, module sections included: see docs/galnet-igb.md#public-profile-pages.
// The accent is colour 1 of the corporation's paintshop logo (logolib.go), charcoal otherwise.

import (
	"context"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

const corpDirectoryLimit = 200

// corpPortalSite builds the portal's chrome for one corporation. public is the
// /corp/profile view, which carries no module sections.
func corpPortalSite(corp *evedb.ESPCorpFull, public bool) igbSite {
	site := siteCorp
	if corp == nil {
		site.Nav = []igbNavItem{{"directory", "Directory", "/corp/directory"}}
		return site
	}
	site.Name = igbText(corp.CorporationName)
	tag := ""
	if corp.TickerName != "" {
		tag = "[" + igbText(corp.TickerName) + "]"
	}
	if corp.AllianceName != "" {
		tag += " &middot; " + igbText(corp.AllianceName)
	}
	if tag == "" {
		tag = "Corporate Portal"
	}
	site.Tagline = tag
	site.EmblemCorpID = corp.CorporationID
	if hex := logoColourHex(corp.Color1); igbReadableAccent(hex) {
		site.Pal.Accent, site.Pal.Link = hex, hex
	}
	if public {
		site.Home = "/corp/profile?id=" + strconv.FormatInt(corp.CorporationID, 10)
		site.Nav = []igbNavItem{
			{"overview", "Overview", site.Home},
			{"directory", "Directory", "/corp/directory"},
		}
		return site
	}
	site.Nav = []igbNavItem{{"overview", "Overview", "/corp/"}}
	secs := append([]CorpPortalSection(nil), CorpPortalSections...)
	sort.SliceStable(secs, func(i, j int) bool { return secs[i].Order < secs[j].Order })
	for _, sec := range secs {
		if !sec.Chartered {
			site.Nav = append(site.Nav, igbNavItem{sec.Key, sec.Label, sec.Path})
		}
	}
	site.Nav = append(site.Nav, igbNavItem{"directory", "Directory", "/corp/directory"})
	return site
}

// withCharteredNav adds the Chartered sections to a chartered corporation's
// public menu, for its own members and only when a module runs the
// corporation (CorpPortalChartered). Profile pages never call it.
func withCharteredNav(site igbSite, corp *evedb.ESPCorpFull) igbSite {
	if corp == nil || CorpPortalChartered == nil || !CorpPortalChartered(corp.CorporationID) {
		return site
	}
	var secs []CorpPortalSection
	for _, sec := range CorpPortalSections {
		if sec.Chartered {
			secs = append(secs, sec)
		}
	}
	if len(secs) == 0 {
		return site
	}
	sort.SliceStable(secs, func(i, j int) bool { return secs[i].Order < secs[j].Order })
	nav := []igbNavItem{site.Nav[0]}
	for _, sec := range secs {
		nav = append(nav, igbNavItem{sec.Key, sec.Label, sec.Path})
	}
	site.Nav = append(nav, site.Nav[1:]...)
	return site
}

// igbReadableAccent reports whether a "#rrggbb" colour is light enough to read
// as text on the portal's charcoal: Black Plain, Lead and Red Shade Paint are
// logo colours, not link colours.
func igbReadableAccent(hex string) bool {
	if len(hex) != 7 || hex[0] != '#' {
		return false
	}
	v, err := strconv.ParseUint(hex[1:], 16, 32)
	if err != nil {
		return false
	}
	r, g, b := float64(v>>16&0xff), float64(v>>8&0xff), float64(v&0xff)
	return 0.299*r+0.587*g+0.114*b >= 110
}

// writeCorpPortalHeader is the portal's top: the corporation's logo at 128 px,
// its name, ticker and alliance, and its menu.
func writeCorpPortalHeader(b *strings.Builder, site igbSite, active string, crumbs ...igbCrumb) {
	p := site.Pal
	writeSiteOpen(b, site, "Corporate Portal")
	writeSiteTopBar(b, site, "<font color="+p.Dim+">CORPORATE PORTAL</font>")
	b.WriteString("<table width=100% bgcolor=" + p.Panel + "><tr>")
	if site.EmblemCorpID != 0 {
		b.WriteString("<td width=136 valign=middle>" + igbCorpLogo(site.EmblemCorpID, 128) + "</td>")
	}
	b.WriteString("<td valign=middle><font size=7 color=" + p.Title + "><b>" + site.Name + "</b></font><br>" +
		"<font size=4 color=" + p.Accent + ">" + site.Tagline + "</font></td></tr></table>\n")
	writeSiteMenu(b, site, active)
	writeSiteCrumbs(b, site, crumbs...)
}

// viewerCorp resolves the viewer's corporation through the database.
func (s *NewsServer) viewerCorp(ctx context.Context, r *http.Request) (*evedb.ESPCorpFull, bool) {
	who := igbIdentity(r)
	if who.ID == 0 {
		return nil, false
	}
	corpID, err := evedb.GetCharCorpID(ctx, s.db, who.ID)
	if err != nil || corpID == 0 {
		return nil, false
	}
	corp, err := evedb.ESPGetCorpFull(ctx, s.db, corpID)
	if err != nil || corp == nil {
		return nil, false
	}
	npc, _ := evedb.IsNPCCorp(ctx, s.db, corpID)
	return corp, npc || corp.IsNPC
}

// handleCorpPortal serves /corp/: the viewer's own corporation.
func (s *NewsServer) handleCorpPortal(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	corp, npc := s.viewerCorp(ctx, r)
	var b strings.Builder
	switch {
	case corp == nil:
		site := corpPortalSite(nil, true)
		writeCorpPortalHeader(&b, site, "")
		b.WriteString("<br><font size=4>This terminal sent no capsuleer identity, so there is no corporation to show.</font><br>" +
			`<font size=3><a href="` + newsHost + `/corp/directory">Browse the directory of capsuleer corporations</a></font><br>`)
		finishSitePage(w, &b, site, 0)
	case npc:
		// A chartered corporation: its public face, then who is recruiting.
		site := withCharteredNav(corpPortalSite(corp, true), corp)
		writeCorpPortalHeader(&b, site, "overview")
		s.writeCorpOverview(ctx, &b, site, corp, r, 0)
		b.WriteString("<br>")
		siteSection(&b, site, "Capsuleer corporations accepting members")
		b.WriteString("<font size=3 color=" + site.Pal.Dim + ">You fly for a chartered corporation. These capsuleer " +
			"corporations have room for new members.</font><br>\n")
		s.writeCorpDirectory(ctx, &b, site, true)
		finishSitePage(w, &b, site, 0)
	default:
		site := corpPortalSite(corp, false)
		writeCorpPortalHeader(&b, site, "overview")
		s.writeCorpOverview(ctx, &b, site, corp, r, igbIdentity(r).ID)
		finishSitePage(w, &b, site, 0)
	}
}

// handleCorpProfile serves /corp/profile?id=N: a corporation's public Overview.
func (s *NewsServer) handleCorpProfile(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	corpID, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	var corp *evedb.ESPCorpFull
	if corpID != 0 {
		var err error
		if corp, err = evedb.ESPGetCorpFull(ctx, s.db, corpID); err != nil {
			log.Printf("igb: corp profile %d: %v", corpID, err)
		}
	}
	var b strings.Builder
	site := corpPortalSite(corp, true)
	if corp == nil {
		writeCorpPortalHeader(&b, site, "", igbCrumb{"Directory", newsHost + "/corp/directory"}, igbCrumb{Label: "Profile"})
		b.WriteString("<br><font size=4>No such corporation is registered.</font><br>\n")
		finishSitePage(w, &b, site, 0)
		return
	}
	writeCorpPortalHeader(&b, site, "overview")
	s.writeCorpOverview(ctx, &b, site, corp, r, 0)
	finishSitePage(w, &b, site, 0)
}

// handleCorpDirectory serves /corp/directory.
func (s *NewsServer) handleCorpDirectory(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	corp, npc := s.viewerCorp(ctx, r)
	site := corpPortalSite(corp, npc || corp == nil)
	var b strings.Builder
	writeCorpPortalHeader(&b, site, "directory")
	b.WriteString("<br>")
	siteSection(&b, site, "Capsuleer corporations")
	s.writeCorpDirectory(ctx, &b, site, false)
	finishSitePage(w, &b, site, 0)
}

// WriteCorpPortalPage renders a module section's body inside the portal: the
// viewer's corporation's masthead and colours, its menu with key marked, and
// the footer, headers and escaping every Galnet page needs.
func WriteCorpPortalPage(w http.ResponseWriter, r *http.Request, key, body string) {
	srv := corpPortalServer
	var corp *evedb.ESPCorpFull
	npc := false
	if srv != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		corp, npc = srv.viewerCorp(ctx, r)
		cancel()
	}
	site := corpPortalSite(corp, npc)
	if npc {
		site = withCharteredNav(site, corp)
	}
	var b strings.Builder
	writeCorpPortalHeader(&b, site, key)
	b.WriteString("<br>" + body)
	finishSitePage(w, &b, site, 0)
}

// corpPortalServer is the news server WriteCorpPortalPage resolves the viewer
// against; set by NewNewsServer.
var corpPortalServer *NewsServer

// CorpOverviewSection is a module's block on a corporation's own Overview, drawn below the
// offices and description and above the wars. Render gets the viewer's corporation (resolved
// through the database) and character, and answers page HTML or "" for nothing. It is not called
// for /corp/profile or a chartered corporation.
type CorpOverviewSection struct {
	Order  int
	Render func(ctx context.Context, r *http.Request, corpID, viewerCharID int64) string
}

// CorpOverviewSections is appended to by modules before the news server
// serves its first page.
var CorpOverviewSections []CorpOverviewSection

// corpOverviewSectionTimeout bounds each module section, so one slow query
// cannot hold the whole Overview.
const corpOverviewSectionTimeout = 3 * time.Second

// writeCorpOverviewSections draws the module sections in Order. A section
// that panics is dropped, never the page.
func writeCorpOverviewSections(ctx context.Context, b *strings.Builder, r *http.Request, corpID, viewer int64) {
	secs := append([]CorpOverviewSection(nil), CorpOverviewSections...)
	sort.SliceStable(secs, func(i, j int) bool { return secs[i].Order < secs[j].Order })
	for _, sec := range secs {
		if sec.Render == nil {
			continue
		}
		html := func() (out string) {
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("igb: corp overview section %d: %v", sec.Order, rec)
					out = ""
				}
			}()
			sctx, cancel := context.WithTimeout(ctx, corpOverviewSectionTimeout)
			defer cancel()
			return sec.Render(sctx, r, corpID, viewer)
		}()
		if html != "" {
			b.WriteString("<br>" + html)
		}
	}
}

// writeCorpOverview is a corporation's face: the chief executive and the figures, the offices,
// the description, then for the viewer's own corporation (viewer != 0) the module sections,
// then the wars. viewer 0 is the public face.
func (s *NewsServer) writeCorpOverview(ctx context.Context, b *strings.Builder, site igbSite, corp *evedb.ESPCorpFull,
	r *http.Request, viewer int64) {
	p := site.Pal
	kv := func(label, value string) {
		if value == "" {
			value = "<font color=" + p.Dim + ">&mdash;</font>"
		}
		b.WriteString("<font size=2 color=" + p.Dim + ">" + label + "</font><br><font size=4>" + value + "</font><br>")
	}
	b.WriteString("<br><table width=100%><tr>")
	// The chief executive.
	b.WriteString("<td width=72 valign=top bgcolor=" + p.Panel + ">" + igbPortrait(corp.CEOID, 64) + "</td>")
	b.WriteString("<td valign=top bgcolor=" + p.Panel + ">")
	kv("CHIEF EXECUTIVE", concordPilotLink(corp.CEOID, corp.CEOName))
	kv("HEADQUARTERS", igbText(corp.StationName))
	b.WriteString("</td><td valign=top bgcolor=" + p.Panel + ">")
	members := igbInt(corp.Pilots)
	if corp.MemberLimit > 0 {
		members += " <font size=2 color=" + p.Dim + ">of " + igbInt(corp.MemberLimit) + "</font>"
	}
	kv("CAPSULEERS", members)
	kv("TAX RATE", strconv.FormatFloat(corp.TaxPct(), 'f', 1, 64)+"%")
	b.WriteString("</td><td valign=top bgcolor=" + p.Panel + ">")
	if u := strings.TrimSpace(corp.URL); u != "" {
		kv("WEBSITE", igbText(u))
	}
	for _, fact := range moduleCorpFacts {
		if label, html, ok := fact(ctx, s, corp.CorporationID, corp.TickerName, false); ok {
			kv(label, html)
		}
	}
	kv("PUBLIC RECORD", `<a href="`+concordCorpURL(corp.CorporationID)+`">DED dossier</a>`)
	b.WriteString("</td></tr></table>\n")

	// Offices, straight under the figures: where the corporation is.
	if offices, err := evedb.CorpOfficeStations(ctx, s.db, corp.CorporationID); err == nil && len(offices) > 0 {
		b.WriteString("<br>")
		siteSection(b, site, "Offices")
		b.WriteString("<table width=100%>")
		for i, o := range offices {
			bg := p.RowA
			if i%2 == 1 {
				bg = p.RowB
			}
			b.WriteString("<tr><td bgcolor=" + bg + "><font size=3>" + igbText(o.StationName) + "</font></td></tr>")
		}
		b.WriteString("</table>\n")
	}

	if d := strings.TrimSpace(corp.Description); d != "" {
		b.WriteString("<br><table width=100% bgcolor=" + p.RowA + "><tr><td><font size=3>" +
			strings.ReplaceAll(igbText(d), "\n", "<br>") + "</font></td></tr></table>\n")
	}

	// The module's sections, for the viewer's own corporation only.
	if viewer != 0 && r != nil {
		writeCorpOverviewSections(ctx, b, r, corp.CorporationID, viewer)
	}

	// Active wars, either side, the corporation's or its alliance's.
	if wars, err := evedb.ListWars(ctx, s.db); err == nil {
		var mine []evedb.WarView
		for _, v := range wars {
			if !v.Active {
				continue
			}
			party := func(id int64) bool {
				return id == corp.CorporationID || (corp.AllianceID != 0 && id == corp.AllianceID)
			}
			if party(v.DeclaredByID) || party(v.AgainstID) {
				mine = append(mine, v)
			}
		}
		if len(mine) > 0 {
			b.WriteString("<br>")
			siteSection(b, site, "Wars")
			b.WriteString("<table width=100%>")
			for i, v := range mine {
				bg := p.RowA
				if i%2 == 1 {
					bg = p.RowB
				}
				state := "<font color=" + p.Dim + ">preparing</font>"
				if v.HostileNow {
					state = "<font color=" + p.Down + "><b>hostilities open</b></font>"
				}
				mutual := ""
				if v.Mutual != 0 {
					mutual = " <font size=2 color=" + p.Dim + ">(mutual)</font>"
				}
				b.WriteString("<tr><td bgcolor=" + bg + "><font size=3>" + igbText(v.DeclaredByName) + " <font color=" + p.Dim +
					">against</font> " + igbText(v.AgainstName) + mutual + "</font></td><td align=right bgcolor=" + bg +
					"><font size=3>" + state + ` &nbsp;<a href="` + warURL(v.WarID, "", 0) + `">report &raquo;</a></font></td></tr>`)
			}
			b.WriteString("</table>\n")
		}
	}
}

// writeCorpDirectory lists capsuleer corporations. acceptingOnly narrows it to
// those with room, for a pilot in a chartered corporation looking for one.
func (s *NewsServer) writeCorpDirectory(ctx context.Context, b *strings.Builder, site igbSite, acceptingOnly bool) {
	p := site.Pal
	rows, err := evedb.CorpDirectory(ctx, s.db, corpDirectoryLimit)
	if err != nil {
		log.Printf("igb: CorpDirectory: %v", err)
		b.WriteString("<font size=3>The directory is not available.</font><br>\n")
		return
	}
	shown := 0
	b.WriteString("<table width=100%>\n<tr><td></td>" +
		"<td><font size=2 color=" + p.Dim + "><b>Corporation</b></font></td>" +
		"<td><font size=2 color=" + p.Dim + "><b>Chief executive</b></font></td>" +
		"<td align=right><font size=2 color=" + p.Dim + "><b>Capsuleers</b></font></td>" +
		"<td align=right><font size=2 color=" + p.Dim + "><b>Tax</b></font></td>" +
		"<td><font size=2 color=" + p.Dim + "><b>Recruiting</b></font></td></tr>\n")
	for _, c := range rows {
		if acceptingOnly && !c.Accepting {
			continue
		}
		bg := p.RowA
		if shown%2 == 1 {
			bg = p.RowB
		}
		shown++
		prof := newsHost + "/corp/profile?id=" + strconv.FormatInt(c.CorpID, 10)
		name := `<a href="` + prof + `"><b>` + igbText(c.Name) + `</b></a>`
		if c.Ticker != "" {
			name += " <font color=" + p.Dim + ">[" + igbText(c.Ticker) + "]</font>"
		}
		if c.AllianceName != "" {
			name += "<br><font size=2 color=" + p.Dim + ">" + igbText(c.AllianceName) + "</font>"
		}
		members := igbInt(c.Capsuleers)
		if c.MemberLimit > 0 {
			members += "<font size=2 color=" + p.Dim + "> / " + igbInt(c.MemberLimit) + "</font>"
		}
		recruiting := "<font color=" + p.Dim + ">full</font>"
		if c.Accepting {
			recruiting = "<font color=" + p.Up + "><b>accepting members</b></font>"
		}
		b.WriteString("<tr><td width=36 bgcolor=" + bg + `><a href="` + prof + `">` + igbCorpLogo(c.CorpID, 32) + "</a></td>" +
			"<td bgcolor=" + bg + "><font size=3>" + name + "</font></td>" +
			"<td bgcolor=" + bg + "><font size=3>" + concordPilotLink(c.CEOID, c.CEOName) + "</font></td>" +
			"<td align=right bgcolor=" + bg + "><font size=3>" + members + "</font></td>" +
			"<td align=right bgcolor=" + bg + "><font size=3>" + strconv.FormatFloat(c.TaxRate*100, 'f', 1, 64) + "%</font></td>" +
			"<td bgcolor=" + bg + "><font size=3>" + recruiting + "</font></td></tr>\n")
	}
	b.WriteString("</table>\n")
	if shown == 0 {
		b.WriteString("<font size=3 color=" + p.Dim + ">No capsuleer corporation is listed here yet.</font><br>\n")
	}
}
