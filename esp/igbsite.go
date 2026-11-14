package esp

// Galnet: the in-game browser as a set of sites on one host. /galnet/, /scope/, /genolution/,
// /concord/, /scc/, /summit/ and /corp/ are told apart by path; each old /ig/ address is an alias that
// serves the same handler directly.
// See docs/galnet-igb.md#browser-limits for what every page must respect.

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

// igbPalette is a site's colours. Link, alink and vlink are always written as the same
// value, or visited links turn the client's default yellow.
type igbPalette struct {
	Page, Panel, RowA, RowB string
	Text, Link, Accent, Dim string
	Header, Up, Down        string
	Title                   string // headings: the brightest text
}

// igbNavItem is one entry of a site's own menu.
type igbNavItem struct{ Key, Label, Path string }

// igbSite is one site on Galnet.
type igbSite struct {
	Key          string
	Name         string
	Tagline      string
	Home         string // path of the site's front page
	EmblemCorpID int64  // 0 = wordmark only
	Pal          igbPalette
	Nav          []igbNavItem
}

func (s igbSite) url(path string) string { return newsHost + path }

// The organisations that run the sites. Their logos are corplogo:<corpID>, drawn by the client
// (igbexchange_logo.go).
const (
	corpTheScope   = 1000107
	corpCONCORD    = 1000125
	corpDED        = 1000137
	corpGenolution = 1000140
)

var siteGalnet = igbSite{
	Key: "galnet", Name: "GALNET", Tagline: "The interstellar network of New Eden", Home: "/galnet/",
	Pal: igbPalette{
		Page: "#080c14", Panel: "#101827", RowA: "#0d1420", RowB: "#121c2d",
		Text: "#c4ccd8", Link: "#8fbce8", Accent: "#a9d4ff", Dim: "#5a6779",
		Header: "#0c1322", Up: "#5fbf7a", Down: "#e05a4f", Title: "#eef3fa",
	},
}

var siteScope = igbSite{
	Key: "scope", Name: "THE SCOPE", Tagline: "Independent news for New Eden", Home: "/scope/",
	EmblemCorpID: corpTheScope,
	Pal: igbPalette{
		Page: "#0a0a0a", Panel: "#151515", RowA: "#111111", RowB: "#191919",
		Text: "#cfcfca", Link: "#f2f2ec", Accent: "#e0533a", Dim: "#77776f",
		Header: "#0f0f0f", Up: "#5fbf7a", Down: "#e05a4f", Title: "#fbfbf6",
	},
}

var siteGenolution = igbSite{
	Key: "genolution", Name: "GENOLUTION", Tagline: "Cerebral conditioning &amp; neural implantation", Home: "/genolution/",
	EmblemCorpID: corpGenolution,
	Pal: igbPalette{
		Page: "#07100a", Panel: "#0f1c12", RowA: "#0c170f", RowB: "#112015",
		Text: "#cfe3d2", Link: "#b8f08f", Accent: "#7fd36b", Dim: "#5f7a64",
		Header: "#0a150d", Up: "#7fd36b", Down: "#e0705a", Title: "#e8f6ea",
	},
}

var siteConcord = igbSite{
	Key: "concord", Name: "DED", Tagline: "Directive Enforcement Department &middot; a division of CONCORD &middot; Capsuleer Incident Registry",
	Home: "/concord/", EmblemCorpID: corpDED,
	Pal: igbPalette{
		Page: cdPage, Panel: cdPanel, RowA: cdRowA, RowB: cdRowB,
		Text: cdText, Link: cdLink, Accent: cdTitle, Dim: cdDim,
		Header: cdHeader, Up: cdGreen, Down: cdRed, Title: cdTitle,
	},
	Nav: []igbNavItem{
		{"front", "Incidents", "/concord/"},
		{"ships", "Ships", "/concord/?type=ship"},
		{"capsules", "Capsules", "/concord/?type=pod"},
		{"battles", "Battle Reports", "/concord/battles"},
		{"wars", "Wars", "/concord/wars"},
		{"sov", "Sovereignty", "/concord/sovereignty"},
	},
}

// The Corporate Portal's palette is per corporation (corpportal.go); this is
// the neutral charcoal it falls back to.
var siteCorp = igbSite{
	Key: "corp", Name: "CORPORATE PORTAL", Tagline: "Your corporation on Galnet", Home: "/corp/",
	Pal: igbPalette{
		Page: "#0f1011", Panel: "#191a1c", RowA: "#151618", RowB: "#1c1d20",
		Text: "#cdd0d4", Link: "#e2c98a", Accent: "#d8b45a", Dim: "#71757b",
		Header: "#131416", Up: "#5fbf7a", Down: "#e05a4f", Title: "#f1f2f4",
	},
}

// The neutral Galnet style the legacy writeIGBHeader / WriteIGBPage helpers
// render in, for any page not yet moved onto a site.
var siteLegacy = siteGalnet

// ── text ────────────────────────────────────────────────────────────────────

// igbEntityNames maps the non-ASCII characters a page is likely to carry onto the HTML 4 entity
// names the client's htmlentitydefs knows. Named, never numeric.
// See docs/galnet-igb.md#text-and-entities.
var igbEntityNames = map[rune]string{
	' ': "nbsp", '©': "copy", '®': "reg", '°': "deg", '±': "plusmn",
	'·': "middot", '«': "laquo", '»': "raquo", '×': "times", '÷': "divide",
	'½': "frac12", '¼': "frac14", '¾': "frac34", '£': "pound", '§': "sect",
	'é': "eacute", 'è': "egrave", 'ê': "ecirc", 'ë': "euml", 'É': "Eacute",
	'á': "aacute", 'à': "agrave", 'â': "acirc", 'ä': "auml", 'å': "aring",
	'Ä': "Auml", 'Å': "Aring", 'ö': "ouml", 'Ö': "Ouml", 'ó': "oacute",
	'ô': "ocirc", 'ø': "oslash", 'Ø': "Oslash", 'ü': "uuml", 'Ü': "Uuml",
	'ú': "uacute", 'í': "iacute", 'ï': "iuml", 'ñ': "ntilde", 'ç': "ccedil",
	'ß': "szlig", 'æ': "aelig", 'Æ': "AElig",
	'–': "ndash", '—': "mdash", '‘': "lsquo", '’': "rsquo", '‚': "sbquo",
	'“': "ldquo", '”': "rdquo", '„': "bdquo", '†': "dagger", '‡': "Dagger",
	'•': "bull", '…': "hellip", '‰': "permil", '′': "prime", '″': "Prime",
	'‹': "lsaquo", '›': "rsaquo", '€': "euro", '™': "trade", '−': "minus",
	'←': "larr", '↑': "uarr", '→': "rarr", '↓': "darr", '≤': "le",
	'≥': "ge", '≠': "ne", '≈': "asymp", '∞': "infin",
}

// igbEntities rewrites every mapped non-ASCII character as its named entity.
// It touches no markup and is idempotent, so it runs over whole pages.
func igbEntities(s string) string {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 32)
	for _, r := range s {
		if r < 0x80 {
			b.WriteRune(r)
			continue
		}
		if name, ok := igbEntityNames[r]; ok {
			b.WriteString("&" + name + ";")
			continue
		}
		b.WriteRune(r) // the utf-8 <meta> carries the rest
	}
	return b.String()
}

// igbText is the one text cleaner every site uses: escape for HTML text, then
// the punctuation the client would otherwise garble.
func igbText(s string) string { return igbEntities(htmlTextEscape(s)) }

// igbYC renders a date in Yoke Count: 2026-10-03 is "YC 128-10-03" by default. The year
// offset is the calendar.ycYearOffset / calendar.ycYear setting (db/calendar.go). Dashes, not
// dots: the size-2 font drops "." and ":". Times (igbYCTime) are printed at size 3 or bold.
func igbYC(t time.Time) string { return evedb.YCDate(t) }

// igbYCTime is igbYC with the time of day.
func igbYCTime(t time.Time) string { return igbYC(t) + " " + t.UTC().Format("15:04") }

// igbISKShort is the short value: 40.63m, 1.20b, 812.40k.
func igbISKShort(v float64) string {
	a := v
	if a < 0 {
		a = -a
	}
	switch {
	case a >= 1e12:
		return strconv.FormatFloat(v/1e12, 'f', 2, 64) + "t"
	case a >= 1e9:
		return strconv.FormatFloat(v/1e9, 'f', 2, 64) + "b"
	case a >= 1e6:
		return strconv.FormatFloat(v/1e6, 'f', 2, 64) + "m"
	case a >= 1e3:
		return strconv.FormatFloat(v/1e3, 'f', 2, 64) + "k"
	}
	return strconv.FormatFloat(v, 'f', 0, 64)
}

// ── page chrome ─────────────────────────────────────────────────────────────

// writeSiteOpen starts the document in the site's palette.
func writeSiteOpen(b *strings.Builder, site igbSite, title string) {
	p := site.Pal
	b.WriteString(`<html><head><meta http-equiv="Content-Type" content="text/html; charset=utf-8"><title>` +
		title + "</title></head>\n")
	b.WriteString("<body bgcolor=" + p.Page + " text=" + p.Text + " link=" + p.Link + " alink=" + p.Link +
		" vlink=" + p.Link + ">\n")
}

// writeSiteTopBar is the slim strip every site wears above its masthead: the
// way back to Galnet, and the date in YC. left replaces the default caption.
func writeSiteTopBar(b *strings.Builder, site igbSite, left string) {
	p := site.Pal
	if left == "" {
		left = "<font color=" + p.Dim + ">" + site.Name + "</font>"
	}
	b.WriteString("<table width=100% bgcolor=" + p.Header + "><tr><td><font size=2>" + left + "</font></td>" +
		"<td align=right><font size=3 color=" + p.Dim + ">" + igbYC(time.Now()) + " &nbsp; </font><font size=3>" +
		`<a href="` + newsHost + `/galnet/" alt="The Galnet directory">Galnet</a></font></td></tr></table>` + "\n")
}

// siteEmblem is a site's logo, with the site's name as the hover text.
func siteEmblem(site igbSite, size int) string {
	if site.EmblemCorpID == 0 {
		return ""
	}
	return `<img alt="` + htmlAttrEscape(stripHTML(site.Name)) + `" src="corplogo:` +
		strconv.FormatInt(site.EmblemCorpID, 10) + `" size=` + strconv.Itoa(size) + `>`
}

// writeSiteMasthead is the site's name plate: emblem, name, tagline, and an
// optional right-hand cell (a capsuleer's name, a clock, a figure).
func writeSiteMasthead(b *strings.Builder, site igbSite, right string) {
	p := site.Pal
	b.WriteString("<table width=100% bgcolor=" + p.Panel + "><tr>")
	if em := siteEmblem(site, 64); em != "" {
		b.WriteString("<td width=72 valign=middle>" + em + "</td>")
	}
	b.WriteString("<td valign=middle><font size=7 color=" + p.Title + "><b>" + site.Name + "</b></font><br>" +
		"<font size=3 color=" + p.Accent + ">" + site.Tagline + "</font></td>")
	if right != "" {
		b.WriteString("<td align=right valign=middle>" + right + "</td>")
	}
	b.WriteString("</tr></table>\n")
}

// writeSiteMenu is the site's own navigation. The active entry is plain text in the accent
// colour.
func writeSiteMenu(b *strings.Builder, site igbSite, active string) {
	if len(site.Nav) == 0 {
		return
	}
	p := site.Pal
	b.WriteString("<table width=100% bgcolor=" + p.Header + "><tr><td><font size=3>&nbsp;")
	for i, n := range site.Nav {
		if i > 0 {
			b.WriteString(" &nbsp;<font color=" + p.Dim + ">|</font>&nbsp; ")
		}
		if n.Key == active {
			b.WriteString("<font color=" + p.Accent + "><b>" + n.Label + "</b></font>")
		} else {
			b.WriteString(`<a href="` + newsHost + n.Path + `">` + n.Label + `</a>`)
		}
	}
	b.WriteString("</font></td></tr></table>\n")
}

// writeSiteCrumbs is the trail for a page below a menu entry. The first crumb is the way out and
// gets the back-arrow affordance; the last is the current page and is never a link. No menu entry is
// marked active on such a page.
func writeSiteCrumbs(b *strings.Builder, site igbSite, crumbs ...igbCrumb) {
	if len(crumbs) == 0 {
		return
	}
	p := site.Pal
	b.WriteString("<font size=2>")
	for i, c := range crumbs {
		if i > 0 {
			b.WriteString(" <font color=" + p.Dim + ">&raquo;</font> ")
		}
		switch {
		case c.URL == "":
			b.WriteString(`<font color=` + p.Title + `>` + c.Label + `</font>`)
		case i == 0:
			b.WriteString(`<a href="` + c.URL + `">&laquo; ` + c.Label + `</a>`)
		default:
			b.WriteString(`<a href="` + c.URL + `">` + c.Label + `</a>`)
		}
	}
	b.WriteString("</font><br>\n")
}

// writeSiteHeader is the standard top of a site page: palette, top bar,
// masthead, menu and (for a sub-page) the trail.
func writeSiteHeader(b *strings.Builder, site igbSite, active string, crumbs ...igbCrumb) {
	writeSiteOpen(b, site, stripHTML(site.Name))
	writeSiteTopBar(b, site, "")
	writeSiteMasthead(b, site, "")
	writeSiteMenu(b, site, active)
	writeSiteCrumbs(b, site, crumbs...)
}

// finishSitePage writes the footer and sends the page with the headers every IGB page needs.
// refresh > 0 asks the client to reload every that many seconds; most pages pass 0.
func finishSitePage(w http.ResponseWriter, b *strings.Builder, site igbSite, refresh int) {
	p := site.Pal
	b.WriteString("<br><table width=100% bgcolor=" + p.Header + "><tr><td><font size=2 color=" + p.Dim + ">" +
		site.Name + " &middot; " + site.Tagline + "</font></td><td align=right><font size=2>" +
		`<a href="` + newsHost + `/galnet/">Galnet directory</a></font></td></tr></table>` + "\n")
	b.WriteString("</body></html>\n")
	writeIGBResponse(w, b.String(), refresh)
}

// writeIGBResponse sends a finished page: headers, the entity pass, and the
// %-doubling the client's Python substitution needs.
func writeIGBResponse(w http.ResponseWriter, page string, refresh int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Pragma", "no-cache")
	if refresh > 0 {
		w.Header().Set("Refresh", strconv.Itoa(refresh))
	}
	w.Write([]byte(escapeEveHTML(igbEntities(igbFitWidth(page)))))
}

// igbFullWidth is what "width=100%" is sent as.
// See docs/galnet-igb.md#browser-limits.
const igbFullWidth = "96%"

// igbFitWidth rewrites every full-width table on a page (sites, modules, news bodies) to
// igbFullWidth. Source keeps writing width=100%.
func igbFitWidth(page string) string {
	return strings.ReplaceAll(page, "width=100%", "width="+igbFullWidth)
}

// igbBandPanel is one panel of a band (writePanelBand): a title, an optional
// note beside it, and rows of cells. Every row of a panel has the same number
// of cells; Align and Width are per column ("right", "36").
type igbBandPanel struct {
	Title, Note string
	Align       []string
	Width       []string
	Rows        [][]string
	Empty       string   // shown in the first row when Rows is empty
	Shades      []string // optional per-row background overriding the alternation (writePanelGrid)
}

// writePanelBand lays several panels side by side as one flat table, row i of every panel
// sharing a table row. Nested tables do not lay out in the client.
// See docs/galnet-igb.md#browser-limits.
func writePanelBand(b *strings.Builder, site igbSite, panels []igbBandPanel) {
	p := site.Pal
	rows := 0
	for _, pn := range panels {
		rows = max(rows, len(pn.Rows))
		if len(pn.Rows) == 0 && pn.Empty != "" {
			rows = max(rows, 1)
		}
	}
	if rows == 0 {
		return
	}
	b.WriteString("<table width=100%><tr>")
	for i, pn := range panels {
		if i > 0 {
			b.WriteString("<td width=6></td>")
		}
		b.WriteString("<td colspan=" + strconv.Itoa(len(pn.Align)) + " bgcolor=" + p.Header + "><font size=3 color=" +
			p.Title + "><b>&nbsp;" + pn.Title + "</b></font>")
		if pn.Note != "" {
			b.WriteString(" <font size=2 color=" + p.Dim + ">" + pn.Note + "</font>")
		}
		b.WriteString("</td>")
	}
	b.WriteString("</tr>\n")
	for r := 0; r < rows; r++ {
		shade := p.RowA
		if r%2 == 1 {
			shade = p.RowB
		}
		b.WriteString("<tr>")
		for i, pn := range panels {
			if i > 0 {
				b.WriteString("<td></td>")
			}
			if r == 0 && len(pn.Rows) == 0 {
				b.WriteString("<td colspan=" + strconv.Itoa(len(pn.Align)) + " valign=top bgcolor=" + shade + "><font size=3 color=" +
					p.Dim + ">&nbsp;" + pn.Empty + "</font></td>")
				continue
			}
			for c := range pn.Align {
				attrs := " valign=top bgcolor=" + shade
				if r >= len(pn.Rows) {
					attrs = ""
				}
				if pn.Align[c] != "" {
					attrs += " align=" + pn.Align[c]
				}
				if c < len(pn.Width) && pn.Width[c] != "" {
					attrs += " width=" + pn.Width[c]
				}
				cell := ""
				if r < len(pn.Rows) && c < len(pn.Rows[r]) {
					cell = pn.Rows[r][c]
				}
				b.WriteString("<td" + attrs + ">" + cell + "</td>")
			}
		}
		b.WriteString("</tr>\n")
	}
	b.WriteString("</table>\n")
}

// writePanelGrid is writePanelBand for several bands that must line up: every band goes into one
// table and shares one column grid. Every panel uses the same column count and Width list, with
// the columns that absorb the slack left empty.
// See docs/galnet-igb.md#shared-panel-grid.
func writePanelGrid(b *strings.Builder, site igbSite, bands [][]igbBandPanel) {
	p := site.Pal
	cols := 0
	for _, band := range bands {
		for _, pn := range band {
			cols = max(cols, len(pn.Align))
		}
	}
	if cols == 0 {
		return
	}
	span := strconv.Itoa(cols)
	// Present on every row: the client takes a column's fixed width from the widest cell that
	// declares one.
	spacer := "<td width=6></td>"
	full := strconv.Itoa(cols*2 + 1)

	b.WriteString("<table width=100%>\n")
	first := true
	for _, band := range bands {
		rows := 0
		for _, pn := range band {
			rows = max(rows, len(pn.Rows))
			// An empty panel with an Empty line still draws, so its band keeps the heading.
			if len(pn.Rows) == 0 && pn.Empty != "" {
				rows = max(rows, 1)
			}
		}
		if rows == 0 {
			continue
		}
		if !first {
			b.WriteString("<tr><td colspan=" + full + "><font size=2>&nbsp;</font></td></tr>\n")
		}
		first = false

		b.WriteString("<tr>")
		for i, pn := range band {
			if i > 0 {
				b.WriteString(spacer)
			}
			b.WriteString("<td colspan=" + span + " bgcolor=" + p.Header + "><font size=3 color=" +
				p.Title + "><b>&nbsp;" + pn.Title + "</b></font>")
			if pn.Note != "" {
				b.WriteString(" <font size=2 color=" + p.Dim + ">" + pn.Note + "</font>")
			}
			b.WriteString("</td>")
		}
		b.WriteString("</tr>\n")

		for r := 0; r < rows; r++ {
			shade := p.RowA
			if r%2 == 1 {
				shade = p.RowB
			}
			b.WriteString("<tr>")
			for i, pn := range band {
				if i > 0 {
					b.WriteString(spacer)
				}
				cellShade := shade
				if r < len(pn.Shades) && pn.Shades[r] != "" {
					cellShade = pn.Shades[r]
				}
				if r == 0 && len(pn.Rows) == 0 {
					b.WriteString("<td colspan=" + span + " valign=top bgcolor=" + cellShade + "><font size=3 color=" +
						p.Dim + ">&nbsp;" + pn.Empty + "</font></td>")
					continue
				}
				for c := 0; c < cols; c++ {
					attrs := " valign=top bgcolor=" + cellShade
					if r >= len(pn.Rows) {
						attrs = ""
					}
					if c < len(pn.Align) && pn.Align[c] != "" {
						attrs += " align=" + pn.Align[c]
					}
					if c < len(pn.Width) && pn.Width[c] != "" {
						attrs += " width=" + pn.Width[c]
					}
					cell := ""
					if r < len(pn.Rows) && c < len(pn.Rows[r]) {
						cell = pn.Rows[r][c]
					}
					b.WriteString("<td" + attrs + ">" + cell + "</td>")
				}
			}
			b.WriteString("</tr>\n")
		}
	}
	b.WriteString("</table>\n")
}

// siteSection writes a section heading in the site's colours.
func siteSection(b *strings.Builder, site igbSite, title string) {
	b.WriteString("<font size=4><b><font color=" + site.Pal.Title + ">" + title + "</font></b></font><br>\n")
}

// ── routing ─────────────────────────────────────────────────────────────────

// redispatch serves a path the client gave a trailing slash: strip the slash and serve the exact
// registration. A single segment naming a corporation is its Galnet page (corpslug.go); anything
// else is a 404. See docs/galnet-igb.md#trailing-slash.
func (s *NewsServer) redispatch(w http.ResponseWriter, r *http.Request) {
	if p := strings.TrimRight(r.URL.Path, "/"); p != r.URL.Path && p != "" && s.mux != nil {
		r2 := r.Clone(r.Context())
		r2.URL.Path = p
		if r2.URL.RawPath != "" {
			r2.URL.RawPath = strings.TrimRight(r2.URL.RawPath, "/")
		}
		// Only an exact registration may answer; a subtree pattern would recurse.
		if h, pattern := s.mux.Handler(r2); pattern == p {
			h.ServeHTTP(w, r2)
			return
		}
	}
	// A corporation, addressed by name (corpslug.go).
	if s.serveCorpSlug(w, r) {
		return
	}
	http.NotFound(w, r)
}

// siteRoot serves h at exactly root (a "/x/" subtree registration) and sends
// everything else under it through redispatch.
func (s *NewsServer) siteRoot(root string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == root {
			h(w, r)
			return
		}
		s.redispatch(w, r)
	}
}
