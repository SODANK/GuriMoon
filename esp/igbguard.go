package esp

import (
	"html"
	"net/http"
	"strconv"
	"strings"
)

// igbGuard answers every in-game-browser request before the panel sees it. The panel's pages
// (scripts, CSS masks, forms in table cells) kill the 3913 renderer: see
// docs/galnet-igb.md#browser-limits. A corporation page /corporation/<id> becomes a Refresh to
// that corporation's Galnet page (the corp info window opens a corporation's url in the in-game
// browser). Anything else gets one short page naming the address to open on the desktop. Image
// routes pass through.
func igbGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isIGBRequest(r) || igbSafeAdminPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if corpID, ok := corpIDFromAdminPath(r.URL.Path); ok {
			igbRefreshTo(w, corpGalnetURL(corpID))
			return
		}
		writeIGBAdminNotice(w, r)
	})
}

// isIGBRequest tells the in-game browser apart by its User-Agent ("EVE-minibrowser/3.0"). The
// eve.* identity headers are absent before a character is chosen, so they are not the test.
func isIGBRequest(r *http.Request) bool {
	return strings.Contains(strings.ToLower(r.UserAgent()), "eve-minibrowser")
}

// igbSafeAdminPath is the panel's image routes, the only things on it the IGB may load.
func igbSafeAdminPath(p string) bool {
	for _, prefix := range []string{
		"/portrait/", "/typeicon/", "/alliancelogo/", "/alliancetile/",
		"/logoshape/", "/gidicon/", "/news-image/", "/scc/chart",
	} {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// corpIDFromAdminPath reads N out of /corporation/N. A trailing slash is allowed because the IGB
// adds one to every extensionless address (docs/galnet-igb.md#trailing-slash).
func corpIDFromAdminPath(p string) (int64, bool) {
	rest, ok := strings.CutPrefix(p, "/corporation/")
	if !ok {
		return 0, false
	}
	id, err := strconv.ParseInt(strings.TrimSuffix(rest, "/"), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// corpGalnetURL is a corporation's page on Galnet: the Corporate Portal profile for a capsuleer
// corp, CONCORD's dossier for an NPC one (the portal renders only player corporations; NPC corps
// sit in 1000000-1999999).
func corpGalnetURL(corpID int64) string {
	id := strconv.FormatInt(corpID, 10)
	if corpID >= 1000000 && corpID < 2000000 {
		return newsHost + "/concord/corporation?id=" + id
	}
	return newsHost + "/corp/profile?id=" + id
}

// igbRefreshTo sends the IGB elsewhere with a zero Refresh, not a 302, so the address bar keeps
// the requested URL (docs/galnet-igb.md#homepage-and-identity).
func igbRefreshTo(w http.ResponseWriter, target string) {
	w.Header().Set("Refresh", "0;URL="+target)
	writeIGBResponse(w, `<html><head><meta http-equiv="Content-Type" content="text/html; charset=utf-8"></head>`+
		`<body bgcolor=#000000 text=#c0c0c0 link=#e8d060 alink=#e8d060 vlink=#e8d060>`+
		`<font size=3><a href="`+html.EscapeString(target)+`">Continue to Galnet</a></font></body></html>`, 0)
}

// writeIGBAdminNotice is the one page the panel shows the IGB.
func writeIGBAdminNotice(w http.ResponseWriter, r *http.Request) {
	addr := "http://" + r.Host + r.URL.RequestURI()
	var b strings.Builder
	b.WriteString(`<html><head><meta http-equiv="Content-Type" content="text/html; charset=utf-8"><title>Administration</title></head>`)
	b.WriteString(`<body bgcolor=#000000 text=#c0c0c0 link=#e8d060 alink=#e8d060 vlink=#e8d060>`)
	b.WriteString(`<br><font size=5 color=#e8d060><b>Administration panel</b></font><br><br>`)
	b.WriteString(`<font size=3>This is the server's administration panel. The in-game browser cannot display it.<br><br>`)
	b.WriteString(`Open this address in a desktop browser instead:<br><br>`)
	b.WriteString(`<b>` + html.EscapeString(addr) + `</b><br><br>`)
	b.WriteString(`<a href="` + newsHost + `/galnet/">Return to Galnet</a></font>`)
	b.WriteString(`</body></html>`)
	writeIGBResponse(w, b.String(), 0)
}
