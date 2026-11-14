package esp

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
	"gurimoon/safego"
)

// NewsServer serves the legacy client news endpoints the 3913 client fetches from
// http://myeve.eve-online.com/ (hosts-file redirect). It listens on port 80 because the client
// URLs carry no port. Routes: /mb/news.asp (+ ?id=N), /mb/news-headlines.asp (plain-text
// "date;caption" lines for the billboard ticker), /bitmaps/billboards/<day>.jpg, /img/<file>,
// /serv.asp and /chr/serv.asp (portrait JPEGs for img.eve.is; the billboard WANTED poster fetches
// ?s=256&c=<charID>, so the client also needs a hosts entry for img.eve.is), and the Galnet sites
// (docs/galnet-igb.md).
type NewsServer struct {
	httpSrv       *http.Server
	db            *sql.DB
	sessions      *SessionRegistry
	startTime     time.Time
	billboardsDir string
	imagesDir     string
	portraitsDir  string
	// mux is kept so the catch-alls can re-dispatch a path the client has
	// given a trailing slash (igbsite.go redispatch).
	mux *http.ServeMux
}

// NewNewsServer creates a news feed server. Call Start() to begin serving.
func NewNewsServer(listenAddr string, db *sql.DB, sessions *SessionRegistry) *NewsServer {
	s := &NewsServer{
		db:            db,
		sessions:      sessions,
		startTime:     time.Now(),
		billboardsDir: filepath.Join("data", "billboards"),
		imagesDir:     filepath.Join("data", "news-images"),
		portraitsDir:  filepath.Join("data", "portraits"),
	}
	mux := http.NewServeMux()
	// Anything unregistered: a trailing slash the in-game browser added is
	// stripped and re-dispatched (igbsite.go redispatch); the rest is a 404.
	mux.HandleFunc("/", s.redispatch)
	// /mb/news.asp is both the client's homepage and the character-select news panel;
	// handleNewsPage tells them apart by identity (docs/galnet-igb.md#homepage-and-identity).
	mux.HandleFunc("/mb/news.asp", s.handleNewsPage)
	mux.HandleFunc("/mb/news-headlines.asp", s.handleHeadlines)
	mux.HandleFunc("/bitmaps/billboards/", s.handleBillboardImage)
	mux.HandleFunc("/img/", s.handleImage)
	mux.HandleFunc("/serv.asp", s.handlePortrait)
	mux.HandleFunc("/chr/serv.asp", s.handlePortrait)

	// Galnet sites (igbsite.go). Each old /ig/ address is an alias serving the same handler, never a
	// redirect.
	site := func(root string, h http.HandlerFunc, aliases ...string) {
		mux.HandleFunc(root, s.siteRoot(root, h))
		for _, a := range aliases {
			mux.HandleFunc(a, h)
		}
	}
	page := func(path string, h http.HandlerFunc, aliases ...string) {
		mux.HandleFunc(path, h)
		for _, a := range aliases {
			mux.HandleFunc(a, h)
		}
	}
	// "/ig/" is both the old portal's address (now Galnet) and the subtree
	// every other /ig/ alias hangs under, so its siteRoot redispatches them.
	site("/galnet/", s.handleGalnet)
	mux.HandleFunc("/ig/", s.siteRoot("/ig/", s.handleGalnet))

	site("/scope/", s.handleScope)
	page("/scope/story", s.handleScopeStory)

	site("/genolution/", s.handleGenolution, "/ig/skills")

	site("/concord/", s.handleConcord, "/ig/kb")
	page("/concord/incident", s.handleConcordIncident, "/ig/kill")
	page("/concord/capsuleer", s.handleConcordCapsuleer, "/ig/pilot")
	page("/concord/corporation", s.handleConcordCorporation, "/ig/corp")
	page("/concord/system", s.handleConcordSystem)
	page("/concord/battles", s.handleConcordBattles)
	page("/concord/battle", s.handleConcordBattle)
	page("/concord/wars", s.handleConcordWars)
	page("/concord/war", s.handleConcordWar)
	page("/concord/sovereignty", s.handleConcordSov)

	site("/summit/", s.handleSummit)
	page("/summit/thread", s.handleSummitThread)
	page("/summit/new", s.handleSummitNew)
	page("/summit/post", s.handleSummitPost)
	page("/summit/key", s.handleSummitKey)

	site("/corp/", s.handleCorpPortal)
	page("/corp/profile", s.handleCorpProfile)
	page("/corp/directory", s.handleCorpDirectory)
	// Module sections of the portal (igbmodule.go), populated before this runs.
	for _, sec := range CorpPortalSections {
		if sec.Path != "" && sec.Handler != nil {
			mux.HandleFunc(sec.Path, sec.Handler)
		}
	}

	// Whole sites added by overlay modules (igbmodule.go, moduleNewsRoutes).
	for _, add := range moduleNewsRoutes {
		add(s, mux)
	}

	// Retired: the commodity page is a stub and appears in no menu.
	mux.HandleFunc("/ig/market", s.handleIGBMarket)

	// Module-registered IGB pages (igbmodule.go), populated before this runs.
	// Kept for the module's old /ig/ addresses, which its letters carry.
	for path, h := range IGBModuleHandlers {
		mux.HandleFunc(path, h)
	}
	s.mux = mux
	corpPortalServer = s
	// Corporations are addressed by name on this server (corpslug.go).
	evedb.GalnetPathTaken = s.galnetPathTaken
	s.httpSrv = &http.Server{
		Addr:    listenAddr,
		Handler: hostGuard(listenAddr, csrfGuard(mux)),
		// Reachable unauthenticated by any game client; a peer stalling mid-request must not hold an fd
		// and goroutine indefinitely.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Minute,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	return s
}

// Start begins serving HTTP in a background goroutine.
func (s *NewsServer) Start() {
	// Corporations still pointed at their old ESP admin page move onto their
	// Galnet address (db/corppageurl.go); the mux is complete by now.
	evedb.GoMigrateCorpPageURLs(s.db)
	go func() {
		defer safego.Recover("Start")
		log.Printf("News feed server listening on %s", s.httpSrv.Addr)
		if err := s.httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("News feed server error: %v", err)
		}
	}()
}

// Shutdown gracefully stops the HTTP server.
func (s *NewsServer) Shutdown(ctx context.Context) error {
	return s.httpSrv.Shutdown(ctx)
}

// escapeEveHTML prepares a page body for the 3913 client, which runs
// old-style Python % substitution (txt %= {...}) on everything it fetches.
// A bare % would abort the substitution, so double it.
func escapeEveHTML(s string) string {
	return strings.ReplaceAll(s, "%", "%%")
}

// htmlTextEscape escapes text placed inside HTML text content (titles).
func htmlTextEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

var newsTagRE = regexp.MustCompile(`<[^>]*>`)

// stripHTML removes markup for plain-text contexts (billboard ticker).
func stripHTML(s string) string {
	return newsTagRE.ReplaceAllString(s, "")
}

// moreMarker splits an article body into list excerpt and full text.
// Authors insert <!--more--> in the body; the list page shows everything
// before it plus a "» read more" link to the single-article view.
const moreMarker = "<!--more-->"

// newsHost is the hostname the client fetches news from (hosts-file redirect). Image references
// are absolute URLs on this host because the browser's relative-URL resolution is unreliable on
// a re-rendered page.
const newsHost = evedb.GalnetHost

// absoluteImageURL normalizes an article image reference to an absolute URL:
// "myimage.jpg" → "http://…/img/myimage.jpg", "/img/x.jpg" → "http://…/img/x.jpg",
// full http(s) URLs pass through unchanged.
func absoluteImageURL(u string) string {
	u = strings.TrimSpace(u)
	if u == "" {
		return ""
	}
	if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		return u
	}
	if !strings.HasPrefix(u, "/") {
		u = "/img/" + u
	}
	return newsHost + u
}

// absolutizeBodyImages rewrites root-relative img references inside a raw
// EVE-HTML body to absolute URLs on newsHost.
func absolutizeBodyImages(body string) string {
	body = strings.ReplaceAll(body, `src="/img/`, `src="`+newsHost+`/img/`)
	body = strings.ReplaceAll(body, `src='/img/`, `src='`+newsHost+`/img/`)
	body = strings.ReplaceAll(body, `src=/img/`, `src=`+newsHost+`/img/`)
	return body
}

// Colors of the old shared IGB style. Still read by pages not yet moved onto
// a Galnet site palette (the SCC's fallbacks, the module's legacy pages).
const (
	newsLinkColor  = "#4a9ce8"
	newsTextColor  = "#c8c8c8"
	newsTitleColor = "#ffffff"
	newsMetaColor  = "#c8c8c8"
)

// handleNewsPage serves /mb/news.asp, two screens at one fixed address: the Galnet front door
// for a client with an identity, The Scope's compact feed at character select. ?id= is a Scope
// story either way. See docs/galnet-igb.md#homepage-and-identity.
func (s *NewsServer) handleNewsPage(w http.ResponseWriter, r *http.Request) {
	log.Printf("news: %s %s from %s (%s)", r.Method, r.URL.RequestURI(), r.RemoteAddr, r.UserAgent())
	switch {
	case r.URL.Query().Get("id") != "":
		s.handleScopeStory(w, r)
	case igbIdentity(r).ID != 0:
		s.galnetHomepage(w)
	default:
		s.handleScopeCompact(w, r)
	}
}

// galnetHomepage sends the browser from its fixed homepage to /galnet/ with a Refresh: 0 header,
// not a redirect, so the address bar names the site. The body links to /galnet/ for a client that
// ignores the header. See docs/galnet-igb.md#homepage-and-identity.
func (s *NewsServer) galnetHomepage(w http.ResponseWriter) {
	w.Header().Set("Refresh", "0;URL="+newsHost+"/galnet/")
	var b strings.Builder
	writeSiteOpen(&b, siteGalnet, "Galnet")
	b.WriteString(`<font size=3 color=` + siteGalnet.Pal.Dim + `>Connecting to <a href="` + newsHost +
		`/galnet/">Galnet</a>&hellip;</font></body></html>` + "\n")
	writeIGBResponse(w, b.String(), 0)
}

func (s *NewsServer) handleHeadlines(w http.ResponseWriter, r *http.Request) {
	log.Printf("news: %s %s from %s (%s)", r.Method, r.URL.Path, r.RemoteAddr, r.UserAgent())
	articles, err := evedb.GetTickerHeadlines(s.db, 20)
	if err != nil {
		log.Printf("news: GetTickerHeadlines: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var b strings.Builder
	// Module captions ride the billboards first, so a full news feed cannot
	// push them off the 256-character strip (moduleBillboardCaptions).
	for _, captions := range moduleBillboardCaptions {
		for _, c := range captions(r.Context(), s.db, 4) {
			b.WriteString(time.Now().UTC().Format("2006.01.02") + ";" + c + "\r\n")
		}
	}
	for _, a := range articles {
		// Client splits each line on ';' once: "date;caption".
		caption := stripHTML(a.Title)
		caption = strings.ReplaceAll(caption, "\r", " ")
		caption = strings.ReplaceAll(caption, "\n", " ")
		b.WriteString(a.Date.Format("2006.01.02"))
		b.WriteString(";")
		b.WriteString(caption)
		b.WriteString("\r\n")
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(b.String()))
}

var billboardDayRE = regexp.MustCompile(`^(sunday|monday|tuesday|wednesday|thursday|friday|saturday|default)\.jpg$`)

func (s *NewsServer) handleBillboardImage(w http.ResponseWriter, r *http.Request) {
	log.Printf("news: %s %s from %s (%s)", r.Method, r.URL.Path, r.RemoteAddr, r.UserAgent())
	name := strings.TrimPrefix(r.URL.Path, "/bitmaps/billboards/")
	if !billboardDayRE.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	// Pragma: no-cache keeps the client's browserCache from pinning the image, so the 60 s billboard
	// refresh picks up a new article image.
	w.Header().Set("Pragma", "no-cache")

	// A published article flagged for the billboard takes priority over the
	// static per-weekday files.
	if a, err := evedb.GetBillboardArticle(s.db); err == nil && a != nil {
		if p := s.localImagePath(a.ImageURL); p != "" {
			if jpg := renderScaledJPEG(p, 512, 512); jpg != nil {
				w.Header().Set("Content-Type", "image/jpeg")
				w.Write(jpg)
				return
			}
		}
	}

	path := filepath.Join(s.billboardsDir, name)
	if _, err := os.Stat(path); err != nil {
		path = filepath.Join(s.billboardsDir, "default.jpg")
	}
	if jpg := renderScaledJPEG(path, 512, 512); jpg != nil {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(jpg)
		return
	}
	// No decodable image on disk: serve a generated placeholder so the client's billboard refresh
	// chain never sees a 404.
	w.Header().Set("Content-Type", "image/jpeg")
	w.Write(placeholderBillboardJPEG())
}

// renderScaledJPEG decodes an image file and re-encodes it as a baseline JPEG stretched to w x h.
// Textures on 3D surfaces (billboards, WANTED portraits) must be power-of-two baseline JPEGs; the
// client's trinity loader and PIL fail on progressive or odd-sized ones. Returns nil if the file
// cannot be decoded.
func renderScaledJPEG(path string, w, h int) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	src, _, err := image.Decode(f)
	if err != nil {
		log.Printf("news: cannot decode %s for billboard/portrait: %v", path, err)
		return nil
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	sb := src.Bounds()
	for y := 0; y < h; y++ {
		sy := sb.Min.Y + y*sb.Dy()/h
		for x := 0; x < w; x++ {
			sx := sb.Min.X + x*sb.Dx()/w
			dst.Set(x, y, src.At(sx, sy))
		}
	}
	var buf bytes.Buffer
	jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 90})
	return buf.Bytes()
}

// localImagePath maps an article image reference (bare filename, /img/<file>,
// or absolute URL on newsHost) to its file in data/news-images/, or "" if the
// reference points somewhere this server does not host.
func (s *NewsServer) localImagePath(u string) string {
	u = strings.TrimSpace(u)
	u = strings.TrimPrefix(u, newsHost)
	u = strings.TrimPrefix(u, "/img/")
	if u == "" || strings.ContainsAny(u, `/\`) || strings.Contains(u, "..") {
		return ""
	}
	return filepath.Join(s.imagesDir, u)
}

// handleImage serves article images from data/news-images/. newsinfo.png is
// generated on the fly when no file with that name exists on disk.
func (s *NewsServer) handleImage(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/img/")
	// Forbid path traversal; single flat directory.
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(s.imagesDir, name)
	if _, err := os.Stat(path); err == nil {
		http.ServeFile(w, r, path)
		return
	}
	if name == "newsinfo.png" {
		w.Header().Set("Content-Type", "image/png")
		w.Write(newsInfoIconPNG())
		return
	}
	http.NotFound(w, r)
}

// handlePortrait serves character portraits for the img.eve.is endpoints (billboard WANTED
// poster /serv.asp?s=256&c=<charID>, /chr/serv.asp?s=128&c=<charID>). In-game UI renders
// portraits locally and never calls it. Files in data/portraits/ (<charID>_<size> or <charID>,
// jpg or png) win; otherwise a generated silhouette is served.
func (s *NewsServer) handlePortrait(w http.ResponseWriter, r *http.Request) {
	charID, err := strconv.ParseInt(r.URL.Query().Get("c"), 10, 64)
	if err != nil || charID <= 0 {
		http.NotFound(w, r)
		return
	}
	size, err := strconv.Atoi(r.URL.Query().Get("s"))
	if err != nil || size < 32 || size > 512 {
		size = 256
	}
	log.Printf("news: portrait request charID=%d size=%d from %s", charID, size, r.RemoteAddr)
	w.Header().Set("Pragma", "no-cache")

	id := strconv.FormatInt(charID, 10)
	sz := strconv.Itoa(size)
	candidates := []string{
		id + "_" + sz + ".jpg", id + "_" + sz + ".png",
		id + ".jpg", id + ".png",
		id + "_256.jpg", id + "_256.png",
		id + "_128.jpg", id + "_128.png",
	}
	for _, name := range candidates {
		p := filepath.Join(s.portraitsDir, name)
		if jpg := renderScaledJPEG(p, size, size); jpg != nil {
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write(jpg)
			return
		}
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Write(placeholderPortraitJPEG(charID))
}

// placeholderPortraitJPEG draws a simple head-and-shoulders silhouette on a
// dark background, tinted per-character so different pilots are visually
// distinct on the billboard.
func placeholderPortraitJPEG(charID int64) []byte {
	const sz = 256
	img := image.NewRGBA(image.Rect(0, 0, sz, sz))
	// Per-character tint from the ID.
	h := uint32(charID)
	h = h*2654435761 + 0x9e3779b9
	tr := uint8(30 + (h>>0)&31)
	tg := uint8(35 + (h>>8)&31)
	tb := uint8(45 + (h>>16)&31)
	silhouette := color.RGBA{tr + 40, tg + 40, tb + 50, 0xff}
	for y := 0; y < sz; y++ {
		f := float64(y) / sz
		bg := color.RGBA{uint8(float64(tr) * (0.4 + 0.6*f)), uint8(float64(tg) * (0.4 + 0.6*f)), uint8(float64(tb) * (0.4 + 0.6*f)), 0xff}
		for x := 0; x < sz; x++ {
			img.Set(x, y, bg)
		}
	}
	// Head: circle centered at (128, 100), r=52.
	for y := 0; y < sz; y++ {
		for x := 0; x < sz; x++ {
			dx, dy := float64(x-128), float64(y-100)
			if dx*dx+dy*dy <= 52*52 {
				img.Set(x, y, silhouette)
			}
		}
	}
	// Shoulders: widening trapezoid from y=160 down.
	for y := 160; y < sz; y++ {
		halfW := 40 + (y-160)*2
		if halfW > 118 {
			halfW = 118
		}
		for x := 128 - halfW; x <= 128+halfW; x++ {
			img.Set(x, y, silhouette)
		}
	}
	var buf bytes.Buffer
	jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85})
	return buf.Bytes()
}

// newsInfoIconPNG draws the blue square "i" info icon used next to article
// titles, in the style of the 2005 myeve news page.
func newsInfoIconPNG() []byte {
	const sz = 20
	img := image.NewRGBA(image.Rect(0, 0, sz, sz))
	bg := color.RGBA{0x1e, 0x6a, 0xd4, 0xff}
	border := color.RGBA{0x8a, 0xb8, 0xf0, 0xff}
	white := color.RGBA{0xff, 0xff, 0xff, 0xff}
	for y := 0; y < sz; y++ {
		for x := 0; x < sz; x++ {
			if x == 0 || y == 0 || x == sz-1 || y == sz-1 {
				img.Set(x, y, border)
			} else {
				img.Set(x, y, bg)
			}
		}
	}
	// "i": dot + stem, 3px wide, centered.
	for y := 4; y <= 5; y++ {
		for x := 9; x <= 11; x++ {
			img.Set(x, y, white)
		}
	}
	for y := 8; y <= 15; y++ {
		for x := 9; x <= 11; x++ {
			img.Set(x, y, white)
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

// placeholderBillboardJPEG draws a simple dark gradient screen so billboards
// have something to display when no jpg exists in data/billboards/.
func placeholderBillboardJPEG() []byte {
	const w, h = 512, 512
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		// Vertical blue-black gradient with a lighter band across the middle.
		f := float64(y) / h
		rC := uint8(10 + 20*f)
		gC := uint8(20 + 40*f)
		bC := uint8(50 + 120*f)
		if y > h/2-40 && y < h/2+40 {
			d := 1.0 - float64(abs(y-h/2))/40.0
			rC += uint8(40 * d)
			gC += uint8(60 * d)
			bC += uint8(80 * d)
		}
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{rC, gC, bC, 0xff})
		}
	}
	var buf bytes.Buffer
	jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85})
	return buf.Bytes()
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
