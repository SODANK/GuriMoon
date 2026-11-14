package esp

// The Scope: New Eden's independent news network, and the in-game browser's news site.
//
//	/scope/                 front page: lead story, secondary grid, wire, strips
//	/scope/?section=Name    one desk (db.NewsSections)
//	/scope/story?id=N       one story (also /mb/news.asp?id=N)
//
// Stories are eve_news rows authored on the ESP News page. A body is raw EVE-HTML and
// "<!--more-->" splits the opening from the rest. A story may be tagged with a corporation (its
// logo prints beside the headline) and filed under a section (the menu). The character-select
// panel shows handleScopeCompact: the same stories as one plain narrow column.

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
	scopeFrontCount   = 13 // lead + 8 in the grid + the wire beyond
	scopeGridStories  = 8
	scopeWireCount    = 10
	scopeExcerptChars = 170
)

// scopeSite is siteScope with its section menu filled in.
func scopeSite() igbSite {
	site := siteScope
	site.Nav = []igbNavItem{{"front", "Front page", "/scope/"}}
	for _, sec := range evedb.NewsSections {
		site.Nav = append(site.Nav, igbNavItem{sec, sec, "/scope/?section=" + url.QueryEscape(sec)})
	}
	return site
}

func scopeStoryURL(id int64) string { return newsHost + "/scope/story?id=" + strconv.FormatInt(id, 10) }

// scopeByline is "By <author> · <igbYC date>".
func scopeByline(a evedb.NewsArticle) string {
	author := strings.TrimSpace(a.Author)
	if author == "" {
		author = "The Scope"
	}
	return "By " + igbText(author) + " &middot; " + igbYC(a.Date)
}

// scopeOpening is the authored part of a body before <!--more-->, and whether
// there is more after it.
func scopeOpening(body string) (string, bool) {
	if i := strings.Index(body, moreMarker); i >= 0 {
		return body[:i], true
	}
	return body, false
}

// scopeBodyHTML prepares an authored body for the browser: plain-text
// newlines become <br>, root-relative images become absolute.
func scopeBodyHTML(body string) string {
	body = strings.Replace(body, moreMarker, "", 1)
	body = strings.ReplaceAll(body, "<br>\n", "<br>")
	body = strings.ReplaceAll(body, "<br/>\n", "<br/>")
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\n", "<br>\n")
	return absolutizeBodyImages(body)
}

// scopeExcerpt is a plain-text teaser of at most n characters.
func scopeExcerpt(body string, n int) string {
	open, _ := scopeOpening(body)
	txt := strings.Join(strings.Fields(stripHTML(open)), " ")
	return igbText(igbTrim(txt, n))
}

// scopeCorpLogo is a tagged story's corporation logo, linked to where an
// outsider can read about that corporation: the Corporate Portal's public
// profile for a capsuleer corporation, CONCORD's dossier for an NPC one.
func scopeCorpLogo(a evedb.NewsArticle, size int) string {
	if a.CorpID == 0 {
		return ""
	}
	return `<a href="` + corpPublicURL(a.CorpID) + `">` + igbCorpLogo(a.CorpID, size) + `</a>`
}

// corpPublicURL is where a corporation's name or logo should lead a reader.
func corpPublicURL(corpID int64) string {
	if isNPCCorpID(corpID) {
		return concordCorpURL(corpID)
	}
	return newsHost + "/corp/profile?id=" + strconv.FormatInt(corpID, 10)
}

// isNPCCorpID is the client's minNPCCorporation..maxNPCCorporation band.
func isNPCCorpID(id int64) bool { return id >= 1000000 && id <= 1999999 }

// scopeHeadline is a linked headline at the given size, with the corp logo.
func scopeHeadline(a evedb.NewsArticle, size string, logo int) string {
	p := siteScope.Pal
	h := `<font size=` + size + `><b><a href="` + scopeStoryURL(a.ID) + `">` + igbText(a.Title) + `</a></b></font>`
	if l := scopeCorpLogo(a, logo); l != "" {
		h = l + " " + h
	}
	if a.Section != "" {
		h = `<font size=2 color=` + p.Accent + `><b>` + igbText(strings.ToUpper(a.Section)) + `</b></font><br>` + h
	}
	return h
}

func (s *NewsServer) writeScopeHeader(b *strings.Builder, active string, crumbs ...igbCrumb) igbSite {
	site := scopeSite()
	writeSiteOpen(b, site, "The Scope")
	writeSiteTopBar(b, site, "")
	writeSiteMasthead(b, site, "<font size=4 color="+site.Pal.Title+"><b>"+igbYC(time.Now())+"</b></font>")
	writeSiteMenu(b, site, active)
	writeSiteCrumbs(b, site, crumbs...)
	return site
}

// handleScope serves /scope/: the front page, or one desk.
func (s *NewsServer) handleScope(w http.ResponseWriter, r *http.Request) {
	section := ""
	for _, sec := range evedb.NewsSections {
		if sec == r.URL.Query().Get("section") {
			section = sec
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var b strings.Builder
	active := "front"
	if section != "" {
		active = section
	}
	site := s.writeScopeHeader(&b, active)
	p := site.Pal

	var articles []evedb.NewsArticle
	var err error
	if section != "" {
		articles, err = evedb.GetPublishedNewsInSection(s.db, section, scopeFrontCount)
	} else {
		articles, err = evedb.GetPublishedNews(s.db, scopeFrontCount)
	}
	if err != nil {
		log.Printf("scope: news: %v", err)
	}
	if len(articles) == 0 {
		b.WriteString("<br><font size=4>No stories on this desk yet.</font><br>\n")
		finishSitePage(w, &b, site, 0)
		return
	}

	// The lead: full width, the image across the top, the opening paragraph.
	lead := articles[0]
	b.WriteString("<br><table width=100% bgcolor=" + p.Panel + "><tr><td valign=top>")
	if lead.ImageURL != "" {
		b.WriteString(`<a href="` + scopeStoryURL(lead.ID) + `"><img src="` + absoluteImageURL(lead.ImageURL) +
			`" width=320></a>`)
		b.WriteString("</td><td valign=top>")
	}
	b.WriteString(scopeHeadline(lead, "7", 64) + "<br>")
	b.WriteString("<font size=2 color=" + p.Dim + ">" + scopeByline(lead) + "</font><br><br>")
	open, more := scopeOpening(lead.Body)
	b.WriteString("<font size=4>" + scopeBodyHTML(open) + "</font>")
	if more {
		b.WriteString(`<br><font size=3><b><a href="` + scopeStoryURL(lead.ID) + `">Read the full story &raquo;</a></b></font>`)
	}
	b.WriteString("</td></tr></table>\n")

	// Secondary stories two across, the wire running down the right.
	rest := articles[1:]
	grid := rest
	if len(grid) > scopeGridStories {
		grid = grid[:scopeGridStories]
	}
	rows := (len(grid) + 1) / 2
	if rows == 0 {
		rows = 1
	}
	b.WriteString("<table width=100%>\n")
	for row := 0; row < rows; row++ {
		b.WriteString("<tr>")
		for col := 0; col < 2; col++ {
			i := row*2 + col
			bg := p.RowA
			if (row+col)%2 == 1 {
				bg = p.RowB
			}
			if i >= len(grid) {
				b.WriteString("<td width=37% bgcolor=" + bg + "></td>")
				continue
			}
			a := grid[i]
			b.WriteString("<td width=37% valign=top bgcolor=" + bg + ">")
			if a.ImageURL != "" {
				b.WriteString(`<a href="` + scopeStoryURL(a.ID) + `"><img src="` + absoluteImageURL(a.ImageURL) +
					`" width=120></a><br>`)
			}
			b.WriteString(scopeHeadline(a, "4", 32) + "<br>")
			b.WriteString("<font size=3>" + scopeExcerpt(a.Body, scopeExcerptChars) + "</font><br>")
			b.WriteString("<font size=2 color=" + p.Dim + ">" + scopeByline(a) + "</font></td>")
		}
		if row == 0 {
			b.WriteString("<td valign=top bgcolor=" + p.Panel + " rowspan=" + strconv.Itoa(rows) + ">")
			s.writeScopeWire(&b, site)
			b.WriteString("</td>")
		}
		b.WriteString("</tr>\n")
	}
	b.WriteString("</table>\n")

	// The strips: what the markets (a module's) and the police did today, each linking out.
	b.WriteString("<table width=100% bgcolor=" + p.Header + "><tr>")
	for _, strip := range moduleScopeStrips {
		if title, line := strip(s, ctx); line != "" {
			b.WriteString("<td valign=top width=50%><font size=2 color=" + p.Accent + "><b>" + title + "</b></font><br><font size=3>" +
				line + "</font></td>")
		}
	}
	b.WriteString("<td valign=top><font size=2 color=" + p.Accent + "><b>SECURITY</b></font><br><font size=3>" +
		s.galnetSecurityLine(ctx) + "</font></td>")
	b.WriteString("</tr></table>\n")

	finishSitePage(w, &b, site, 0)
}

// writeScopeWire is the column of latest headlines.
func (s *NewsServer) writeScopeWire(b *strings.Builder, site igbSite) {
	p := site.Pal
	b.WriteString("<font size=3 color=" + p.Accent + "><b>THE WIRE</b></font><br>")
	wire, err := evedb.GetPublishedNews(s.db, scopeWireCount)
	if err != nil || len(wire) == 0 {
		b.WriteString("<font size=2 color=" + p.Dim + ">Quiet on the wire.</font>")
		return
	}
	for _, a := range wire {
		b.WriteString("<font size=3 color=" + p.Dim + ">" + a.Date.UTC().Format("01-02 15:04") + "</font><br>")
		b.WriteString(`<font size=3><a href="` + scopeStoryURL(a.ID) + `">` + igbText(a.Title) + `</a></font><br>`)
	}
}

// handleScopeStory serves /scope/story?id=N (and /mb/news.asp?id=N).
func (s *NewsServer) handleScopeStory(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	var a *evedb.NewsArticle
	if id > 0 {
		var err error
		if a, err = evedb.GetNewsArticle(s.db, id); err != nil {
			log.Printf("scope: story %d: %v", id, err)
		}
	}
	var b strings.Builder
	if a == nil || !a.Published {
		site := s.writeScopeHeader(&b, "", igbCrumb{"Front page", newsHost + "/scope/"}, igbCrumb{Label: "Story"})
		b.WriteString("<br><font size=4>That story is not on file.</font><br>\n")
		finishSitePage(w, &b, site, 0)
		return
	}
	crumbs := []igbCrumb{{"Front page", newsHost + "/scope/"}}
	if a.Section != "" {
		crumbs = append(crumbs, igbCrumb{igbText(a.Section), newsHost + "/scope/?section=" + url.QueryEscape(a.Section)})
	}
	crumbs = append(crumbs, igbCrumb{Label: igbText(igbTrim(a.Title, 60))})
	site := s.writeScopeHeader(&b, "", crumbs...)
	p := site.Pal

	b.WriteString("<br><table width=100% bgcolor=" + p.Panel + "><tr><td valign=top>")
	if a.Section != "" {
		b.WriteString("<font size=2 color=" + p.Accent + "><b>" + igbText(strings.ToUpper(a.Section)) + "</b></font><br>")
	}
	if l := scopeCorpLogo(*a, 64); l != "" {
		b.WriteString(l + " ")
	}
	b.WriteString("<font size=7 color=" + p.Title + "><b>" + igbText(a.Title) + "</b></font><br>")
	b.WriteString("<font size=3 color=" + p.Dim + ">" + scopeByline(*a) + " &middot; " +
		a.Date.UTC().Format("15:04") + " NEST</font></td>")
	if a.ImageURL != "" {
		b.WriteString(`<td width=330 align=right valign=top><img src="` + absoluteImageURL(a.ImageURL) + `" width=320></td>`)
	}
	b.WriteString("</tr></table><br>\n")
	b.WriteString("<font size=4>" + scopeBodyHTML(a.Body) + "</font><br><br>\n")

	// More from The Scope.
	more, _ := evedb.GetPublishedNews(s.db, 7)
	b.WriteString("<table width=100% bgcolor=" + p.Header + "><tr><td><font size=3 color=" + p.Accent +
		"><b>MORE FROM THE SCOPE</b></font><br>")
	n := 0
	for _, m := range more {
		if m.ID == a.ID || n >= 6 {
			continue
		}
		n++
		b.WriteString(`<font size=3><a href="` + scopeStoryURL(m.ID) + `">` + igbText(m.Title) + `</a></font> <font size=2 color=` +
			p.Dim + `>` + igbYC(m.Date) + `</font><br>`)
	}
	b.WriteString("</td></tr></table>\n")
	finishSitePage(w, &b, site, 0)
}

// handleScopeCompact is the character-select news panel: The Scope as one
// narrow column: headline, byline, the opening, a link to the rest.
func (s *NewsServer) handleScopeCompact(w http.ResponseWriter, r *http.Request) {
	site := siteScope
	p := site.Pal
	var b strings.Builder
	writeSiteOpen(&b, site, "The Scope")
	b.WriteString("<table width=100% bgcolor=" + p.Panel + "><tr><td width=40>" + siteEmblem(site, 32) +
		"</td><td><font size=5 color=" + p.Title + "><b>THE SCOPE</b></font> <font size=2 color=" + p.Accent + ">" +
		site.Tagline + "</font></td><td align=right><font size=2 color=" + p.Dim + ">" + igbYC(time.Now()) +
		"</font></td></tr></table>\n")

	articles, err := evedb.GetPublishedNews(s.db, 20)
	if err != nil {
		log.Printf("scope: compact: %v", err)
	}
	if len(articles) == 0 {
		b.WriteString("<br><font size=4>No news is good news.</font>\n")
	}
	for _, a := range articles {
		b.WriteString("<br><table width=100%><tr><td valign=top>")
		b.WriteString(scopeHeadline(a, "6", 32) + "<br><font size=2 color=" + p.Dim + ">" + scopeByline(a) + "</font>")
		b.WriteString("</td>")
		if a.ImageURL != "" {
			b.WriteString(`<td width=180 valign=top align=right><img src="` + absoluteImageURL(a.ImageURL) + `" width=170></td>`)
		}
		b.WriteString("</tr></table>\n")
		open, more := scopeOpening(a.Body)
		b.WriteString("<font size=4>" + scopeBodyHTML(open) + "</font>")
		if more {
			b.WriteString(`<br><font size=3><b><a href="` + scopeStoryURL(a.ID) + `">Read the full story &raquo;</a></b></font>`)
		}
		b.WriteString("<br><hr>\n")
	}
	b.WriteString("</body></html>\n")
	writeIGBResponse(w, b.String(), 0)
}
