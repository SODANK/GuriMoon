package esp

import (
	"context"
	"net/http"
	"strings"

	evedb "gurimoon/db"
)

// IGBModuleHandlers holds extra routes a module adds on the news server (the one the in-game
// browser reaches, unauthenticated), keyed by path. Populate before NewNewsServer. The eve.charID
// header is a display hint, never authorization: a page shows nothing a pilot could not learn in game.
var IGBModuleHandlers = map[string]http.HandlerFunc{}

// IGBModuleNav is extra entries for the Galnet directory: each becomes a tile
// on the front door (galnet.go). Populate with IGBModuleHandlers. A page that
// belongs to the pilot's corporation should be a CorpPortalSection instead.
var IGBModuleNav []IGBNavEntry

// IGBNavEntry is one directory entry a module adds.
type IGBNavEntry struct{ Label, Path string }

// CorpPortalSection is one section of the Corporate Portal (/corp/) that a module provides: a
// menu entry beside the core's Overview (Order 1), served at Path under /corp/. The handler
// renders through WriteCorpPortalPage.
type CorpPortalSection struct {
	Key, Label, Path string
	Order            int
	Handler          http.HandlerFunc
	// Chartered shows the section only to a chartered (NPC) corporation's own
	// members, and only when CorpPortalChartered says a module runs it.
	Chartered bool
}

// CorpPortalSections is populated by modules before NewNewsServer runs.
var CorpPortalSections []CorpPortalSection

// CorpPortalChartered reports whether a module runs this chartered (NPC)
// corporation, so its members see the Chartered sections. Nil by default: a
// chartered corporation's members see only its public face.
var CorpPortalChartered func(corpID int64) bool

// moduleNewsRoutes is appended to from init() by overlay modules that add files to this package
// to serve a whole site of their own. Each runs once while the news server's mux is built. The
// identity rule of IGBModuleHandlers applies.
var moduleNewsRoutes []func(s *NewsServer, mux *http.ServeMux)

// moduleGalnetPanels is appended to from init() by overlay modules: each adds one panel to the
// Galnet front door's "On the network" band, after The Scope's latest. Each runs under
// galnetSnippetTimeout.
var moduleGalnetPanels []func(s *NewsServer, ctx context.Context) igbBandPanel

// moduleCorpFacts is appended to from init() by overlay modules: each may add one labelled line
// about a corporation to its Corporate Portal page (brief false) and to the Galnet front door's
// corporation panel (brief true). ok false adds nothing.
var moduleCorpFacts []func(ctx context.Context, s *NewsServer, corpID int64, ticker string, brief bool) (label, html string, ok bool)

// moduleScopeStrips is appended to from init() by overlay modules: each adds one strip
// (a title and a live line) beside SECURITY under The Scope's front page. An empty line is
// left out.
var moduleScopeStrips []func(s *NewsServer, ctx context.Context) (title, line string)

// moduleBillboardCaptions is appended to from init() by overlay modules: captions that ride the
// in-space billboards ahead of the news headlines (/mb/news-headlines.asp), at most n each.
var moduleBillboardCaptions []func(ctx context.Context, db evedb.DBTX, n int) []string

// WriteIGBPage writes a module's IGB page body with the legacy header and
// footer (the neutral Galnet style), and the headers and escaping every IGB
// page needs. active is ignored; it is kept for old callers.
func WriteIGBPage(w http.ResponseWriter, active, body string) {
	var b strings.Builder
	writeIGBHeader(&b, active)
	b.WriteString(body)
	finishSitePage(w, &b, siteLegacy, 0)
}

// IGBCharID is the eve.charID header of an IGB request (0 when absent).
func IGBCharID(r *http.Request) int64 { return igbIdentity(r).ID }

// IGBText escapes text for an IGB page.
func IGBText(s string) string { return igbText(s) }

// IGBHost is the base URL IGB links must use.
func IGBHost() string { return newsHost }
