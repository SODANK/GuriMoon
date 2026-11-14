package esp

// In-game browser helpers shared by every Galnet site (igbsite.go), served on the news server
// (port 80, reached through the myeve.eve-online.com hosts-file redirect).
//
// The client's browser fetches with plain urllib2. Because myeve.eve-online.com matches its
// autoTrusted list, every request carries identity headers (eve.charID, eve.charName,
// eve.corpName, eve.solarSystemName, eve.stationName). They are client-supplied text, so they are
// display hints, never authorization. The rendering limits (EVE-HTML subset, no CSS or
// JavaScript, Refresh, % substitution) are in docs/galnet-igb.md#browser-limits.

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const igbHiColor = "#ffcc66" // gold accent for numbers

// igbChar is the identity the client browser sends with each request.
type igbChar struct {
	ID          int64
	Name        string
	CorpID      int64 // the eve.corpID header: a hint; resolve through the DB to trust it
	CorpName    string
	SolarSystem string
	Station     string
}

func igbIdentity(r *http.Request) igbChar {
	id, _ := strconv.ParseInt(r.Header.Get("eve.charID"), 10, 64)
	corp, _ := strconv.ParseInt(r.Header.Get("eve.corpID"), 10, 64)
	return igbChar{
		ID:          id,
		CorpID:      corp,
		Name:        igbHeader(r, "eve.charName"),
		CorpName:    igbHeader(r, "eve.corpName"),
		SolarSystem: igbHeader(r, "eve.solarSystemName"),
		Station:     igbHeader(r, "eve.stationName"),
	}
}

// igbHeader reads one identity header. The client builds each value as
// str(session.x and name): an absent session value arrives as the literal
// text "None" (a pilot in space has no station), which is no value at all.
func igbHeader(r *http.Request, name string) string {
	v := strings.TrimSpace(r.Header.Get(name))
	if v == "None" {
		return ""
	}
	return v
}

// writeIGBHeader is the page top for anything not yet moved onto a Galnet site (module pages
// written against WriteIGBPage), in the neutral Galnet style with the way home. active is kept
// for those callers and marks nothing.
func writeIGBHeader(b *strings.Builder, active string) {
	writeSiteOpen(b, siteLegacy, "Galnet")
	writeSiteTopBar(b, siteLegacy, "")
}

// igbCrumb is one step of a breadcrumb trail. A URL of "" is the page you are
// already on and renders as plain text.
type igbCrumb struct {
	Label string
	URL   string
}

// writeIGBSubHeader is writeIGBHeader plus a breadcrumb trail.
func writeIGBSubHeader(b *strings.Builder, crumbs ...igbCrumb) {
	writeIGBHeader(b, "")
	writeSiteCrumbs(b, siteLegacy, crumbs...)
}

// igbISK formats an ISK amount with thousands separators: 1234567.8 → "1,234,567.80 ISK"
func igbISK(v float64) string {
	return igbNum(v) + " ISK"
}

func igbNum(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	dot := strings.IndexByte(s, '.')
	intPart, frac := s[:dot], s[dot:]
	neg := strings.HasPrefix(intPart, "-")
	if neg {
		intPart = intPart[1:]
	}
	var out []byte
	for i, c := range []byte(intPart) {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out) + frac
	}
	return string(out) + frac
}

func igbInt(v int64) string {
	if v < 0 {
		// The sign is not a digit, so it stays out of the thousands grouping.
		return "-" + igbInt(-v)
	}
	s := strconv.FormatInt(v, 10)
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

func igbUptime(d time.Duration) string {
	d = d.Round(time.Minute)
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	if days > 0 {
		return fmt.Sprintf("%dd %dh %dm", days, hours, mins)
	}
	return fmt.Sprintf("%dh %dm", hours, mins)
}

// handleIGBMarket serves /ig/market. The commodity price page is retired and appears in no menu;
// the address answers so an old bookmark lands somewhere.
func (s *NewsServer) handleIGBMarket(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder
	writeSiteHeader(&b, siteGalnet, "")
	b.WriteString("<br><font size=5 color=" + siteGalnet.Pal.Title + "><b>Commodity listings</b></font><br>\n")
	b.WriteString("<font size=3>Commodity listings are carried by every station's market terminal. " +
		"Open the market from your station services or the Neocom for live prices in your region.</font><br><br>\n")
	b.WriteString(`<font size=3><a href="` + newsHost + `/galnet/">&laquo; Back to the Galnet directory</a></font><br>` + "\n")
	finishSitePage(w, &b, siteGalnet, 0)
}

// htmlAttrEscape escapes text placed inside a double-quoted HTML attribute.
func htmlAttrEscape(s string) string {
	return strings.ReplaceAll(htmlTextEscape(s), `"`, "&quot;")
}
