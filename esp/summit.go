package esp

// The Intergalactic Summit (/summit/): Galnet's open floor, where capsuleers address one another
// in character. One board of topics and responses, read by anyone; to speak, a pilot proves who
// they are with a key the Summit mails them (db/summitdb.go), because the identity headers are
// forgeable. Moderation lives on the ESP Forum page (forum.go).
//
// Two client facts shape the pages (ui/control/bastard/edit.py _GoTo). The browser handles Refresh
// headers and cookies only for UNTRUSTED sites, and this host is on its autoTrusted list, so
// neither can be relied on: a post renders its result directly instead of redirecting, and the
// session token rides in every Summit link and form as s= (a cookie is set too, in case a client
// honours it). And forms POST for real (ui/shared/browser/form.py), which is what lets a response
// longer than a URL reach the server.

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

// SummitMailHook mails a pilot from the Summit (wired in cmd/gurimoon/summit.go). nil = no key
// can be issued.
var SummitMailHook func(charID int64, subject, body string)

// corpInterBus hosts the Summit: a neutral carrier, owned by no empire.
const corpInterBus = 1000148

const summitCookie = "igs"

var siteSummit = igbSite{
	Key: "summit", Name: "INTERGALACTIC SUMMIT", Tagline: "The open floor of New Eden &middot; carried by InterBus",
	Home: "/summit/", EmblemCorpID: corpInterBus,
	Pal: igbPalette{
		Page: "#0d0d0c", Panel: "#1a1a18", RowA: "#141413", RowB: "#1c1c1a",
		Text: "#d2d2ca", Link: "#ead27a", Accent: "#d4b24a", Dim: "#7c7c74",
		Header: "#121211", Up: "#6fbf6f", Down: "#d9614f", Title: "#f4f4ee",
	},
	Nav: []igbNavItem{
		{"floor", "The Floor", "/summit/"},
		{"new", "Raise a Topic", "/summit/new"},
		{"key", "Credentials", "/summit/key"},
	},
}

// errSummitNeedKey asks a pilot without a session for their key.
var errSummitNeedKey = errors.New("enter your Summit key to speak")

// summitReq is who is reading, and whether they hold a live session.
type summitReq struct {
	who   igbChar
	token string // a live session token belonging to who, or ""
}

func (q summitReq) authed() bool { return q.token != "" }

// summitURL is a Summit address carrying the reader's session, so it survives a browser that keeps
// no cookies. kv are query pairs.
func (q summitReq) summitURL(path string, kv ...string) string {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			v.Set(kv[i], kv[i+1])
		}
	}
	if q.token != "" {
		v.Set("s", q.token)
	}
	if len(v) == 0 {
		return newsHost + path
	}
	return newsHost + path + "?" + v.Encode()
}

// summitSessionField is the hidden form field that carries the session.
func (q summitReq) summitSessionField() string {
	if q.token == "" {
		return ""
	}
	return `<input type=hidden name=s value="` + htmlAttrEscape(q.token) + `">`
}

// summitResolve reads the identity and the session (the s= field first, then the cookie). A token
// that names another pilot than the header — a capsuleer who changed character — is no session.
func (s *NewsServer) summitResolve(ctx context.Context, r *http.Request) summitReq {
	q := summitReq{who: igbIdentity(r)}
	if q.who.ID == 0 {
		return q
	}
	cands := []string{r.FormValue("s")}
	if c, err := r.Cookie(summitCookie); err == nil {
		cands = append(cands, c.Value)
	}
	for _, t := range cands {
		if t != "" && evedb.SummitSessionChar(ctx, s.db, t) == q.who.ID {
			q.token = t
			break
		}
	}
	return q
}

// summitSignIn verifies a key, opens a session and returns the request as signed in.
func (s *NewsServer) summitSignIn(ctx context.Context, w http.ResponseWriter, q summitReq, key string) (summitReq, error) {
	if strings.TrimSpace(key) == "" {
		return q, errSummitNeedKey
	}
	if err := evedb.VerifySummitKey(ctx, s.db, q.who.ID, key); err != nil {
		return q, err
	}
	token, err := evedb.NewSummitSession(ctx, s.db, q.who.ID)
	if err != nil {
		return q, err
	}
	http.SetCookie(w, &http.Cookie{Name: summitCookie, Value: token, Path: "/summit/", MaxAge: evedb.SummitSessionSecs})
	q.token = token
	return q, nil
}

// ── chrome ──────────────────────────────────────────────────────────────────

// writeSummitHeader is the site header with the session carried on the menu's links.
func writeSummitHeader(b *strings.Builder, q summitReq, active string, crumbs ...igbCrumb) {
	site := siteSummit
	site.Nav = make([]igbNavItem, len(siteSummit.Nav))
	for i, n := range siteSummit.Nav {
		site.Nav[i] = n
		site.Nav[i].Path = strings.TrimPrefix(q.summitURL(n.Path), newsHost)
	}
	p := site.Pal
	writeSiteOpen(b, site, "Intergalactic Summit")
	writeSiteTopBar(b, site, "")
	right := ""
	if q.who.ID != 0 {
		state := "<font size=2 color=" + p.Dim + ">LISTENING</font>"
		if q.authed() {
			state = "<font size=2 color=" + p.Up + ">CREDENTIALS ACCEPTED</font>"
		}
		right = state + "<br><font size=4 color=" + p.Title + "><b>" + igbText(q.who.Name) + "</b></font>"
	}
	writeSiteMasthead(b, site, right)
	writeSiteMenu(b, site, active)
	writeSiteCrumbs(b, site, crumbs...)
}

func summitMsg(s string, bad bool) string {
	c := siteSummit.Pal.Up
	if bad {
		c = siteSummit.Pal.Down
	}
	return "<br><font size=4 color=" + c + "><b>" + igbText(s) + "</b></font><br>\n"
}

func summitTime(unix int64) string {
	t := time.Unix(unix, 0)
	return igbYC(t) + " <font size=3>" + t.UTC().Format("15:04") + "</font>"
}

// writeSummitPager is "Page 2 of 5" with the neighbours linked.
func writeSummitPager(b *strings.Builder, page, pages int, link func(int) string) {
	if pages <= 1 {
		return
	}
	p := siteSummit.Pal
	b.WriteString("<font size=3 color=" + p.Dim + ">Page " + strconv.Itoa(page+1) + " of " + strconv.Itoa(pages) + "</font> &nbsp; <font size=3>")
	if page > 0 {
		b.WriteString(`<a href="` + link(0) + `">&laquo; First</a> &nbsp; <a href="` + link(page-1) + `">&lsaquo; Previous</a> &nbsp; `)
	}
	if page < pages-1 {
		b.WriteString(`<a href="` + link(page+1) + `">Next &rsaquo;</a> &nbsp; <a href="` + link(pages-1) + `">Last &raquo;</a>`)
	}
	b.WriteString("</font><br>\n")
}

func summitPages(total, per int) int {
	if total <= 0 {
		return 1
	}
	return (total + per - 1) / per
}

// ── the floor ───────────────────────────────────────────────────────────────

// handleSummit serves /summit/: the list of topics.
func (s *NewsServer) handleSummit(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	q := s.summitResolve(ctx, r)
	p := siteSummit.Pal
	page, _ := strconv.Atoi(r.URL.Query().Get("p"))
	page = max(page, 0)

	var b strings.Builder
	writeSummitHeader(&b, q, "floor")
	threads, total, err := evedb.ListSummitThreads(ctx, s.db, page, false)
	if err != nil {
		log.Printf("summit: list: %v", err)
		b.WriteString(summitMsg("The Summit's records cannot be reached just now.", true))
		finishSitePage(w, &b, siteSummit, 0)
		return
	}
	b.WriteString("<br><font size=3>Capsuleers of every empire, corporation and allegiance speak here in their own names. " +
		"InterBus carries the floor and keeps no side.</font> <font size=3><b><a href=\"" + q.summitURL("/summit/new") +
		"\">Raise a topic &raquo;</a></b></font><br><br>\n")
	writeSummitPager(&b, page, summitPages(total, evedb.SummitThreadsPerPage), func(n int) string {
		return q.summitURL("/summit/", "p", strconv.Itoa(n))
	})

	b.WriteString("<table width=100%><tr bgcolor=" + p.Header + ">" +
		"<td width=24></td><td><font size=3 color=" + p.Title + "><b>Topic</b></font></td>" +
		"<td align=right width=80><font size=3 color=" + p.Title + "><b>Responses</b></font></td>" +
		"<td align=right width=200><font size=3 color=" + p.Title + "><b>Last word</b></font></td></tr>\n")
	if len(threads) == 0 {
		b.WriteString("<tr bgcolor=" + p.RowA + "><td></td><td colspan=3><font size=3 color=" + p.Dim +
			">The floor is silent. Be the first to address it.</font></td></tr>\n")
	}
	for i, t := range threads {
		shade := p.RowA
		if i%2 == 1 {
			shade = p.RowB
		}
		mark := ""
		switch {
		case t.Sticky && t.Locked:
			mark = "<font color=" + p.Accent + "><b>&bull;</b></font><font color=" + p.Down + "><b>x</b></font>"
		case t.Sticky:
			mark = "<font color=" + p.Accent + " size=4><b>&bull;</b></font>"
		case t.Locked:
			mark = "<font color=" + p.Down + "><b>x</b></font>"
		}
		title := `<a href="` + q.summitURL("/summit/thread", "id", strconv.FormatInt(t.ThreadID, 10)) + `">` + igbText(t.Title) + `</a>`
		if t.Sticky {
			title = "<b>" + title + "</b>"
		}
		b.WriteString("<tr bgcolor=" + shade + "><td align=center valign=top><font size=3>" + mark + "</font></td>" +
			"<td valign=top><font size=4>" + title + "</font><br><font size=2 color=" + p.Dim + ">raised by </font><font size=2>" +
			igbPilotLink(t.AuthorCharID, t.AuthorName) + "</font></td>" +
			"<td align=right valign=top><font size=4>" + igbInt(int64(t.ReplyCount)) + "</font></td>" +
			"<td align=right valign=top><font size=2>" + summitTime(t.LastPostAt) + "</font><br><font size=2>" +
			igbPilotLink(t.LastPostCharID, t.LastPostName) + "</font></td></tr>\n")
	}
	b.WriteString("</table>\n")
	writeSummitPager(&b, page, summitPages(total, evedb.SummitThreadsPerPage), func(n int) string {
		return q.summitURL("/summit/", "p", strconv.Itoa(n))
	})
	b.WriteString("<br><font size=2 color=" + p.Dim + "><font color=" + p.Accent + "><b>&bull;</b></font> pinned by the moderators &nbsp; " +
		"<font color=" + p.Down + "><b>x</b></font> closed to responses</font><br>\n")
	finishSitePage(w, &b, siteSummit, 0)
}

// ── a topic ─────────────────────────────────────────────────────────────────

// handleSummitThread serves /summit/thread?id=N[&p=page|last].
func (s *NewsServer) handleSummitThread(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	q := s.summitResolve(ctx, r)
	id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	page := -1
	if v := r.URL.Query().Get("p"); v != "last" {
		page, _ = strconv.Atoi(v)
		page = max(page, 0)
	}
	s.writeSummitThread(w, ctx, q, id, page, "", false, "")
}

// writeSummitThread draws one page of a topic (page -1 = the last) with the response form under
// it. banner is a result line for the top; draft is text to put back in the form.
func (s *NewsServer) writeSummitThread(w http.ResponseWriter, ctx context.Context, q summitReq, id int64, page int,
	banner string, bad bool, draft string) {
	p := siteSummit.Pal
	var b strings.Builder
	t, err := evedb.GetSummitThread(ctx, s.db, id, false)
	if err != nil {
		writeSummitHeader(&b, q, "", igbCrumb{"The Floor", q.summitURL("/summit/")}, igbCrumb{"Unknown topic", ""})
		b.WriteString(summitMsg("No such topic stands before the Summit.", true))
		finishSitePage(w, &b, siteSummit, 0)
		return
	}
	if page < 0 {
		_, total, err := evedb.ListSummitPosts(ctx, s.db, id, 0, false)
		if err == nil {
			page = evedb.SummitLastPage(total)
		} else {
			page = 0
		}
	}
	posts, total, err := evedb.ListSummitPosts(ctx, s.db, id, page, false)
	if err != nil {
		log.Printf("summit: posts %d: %v", id, err)
	}
	writeSummitHeader(&b, q, "", igbCrumb{"The Floor", q.summitURL("/summit/")}, igbCrumb{igbText(igbTrim(t.Title, 60)), ""})
	if banner != "" {
		b.WriteString(summitMsg(banner, bad))
	}
	b.WriteString("<br><font size=6 color=" + p.Title + "><b>" + igbText(t.Title) + "</b></font><br>")
	var flags []string
	if t.Sticky {
		flags = append(flags, "<font color="+p.Accent+">Pinned by the moderators</font>")
	}
	if t.Locked {
		flags = append(flags, "<font color="+p.Down+">Closed to responses</font>")
	}
	b.WriteString("<font size=2 color=" + p.Dim + ">Raised " + igbYC(time.Unix(t.CreatedAt, 0)) + " &middot; " +
		igbInt(int64(t.ReplyCount)) + " responses</font>")
	if len(flags) > 0 {
		b.WriteString(" <font size=2>&middot; " + strings.Join(flags, " &middot; ") + "</font>")
	}
	b.WriteString("<br><br>\n")

	pages := summitPages(total, evedb.SummitPostsPerPage)
	link := func(n int) string {
		return q.summitURL("/summit/thread", "id", strconv.FormatInt(id, 10), "p", strconv.Itoa(n))
	}
	writeSummitPager(&b, page, pages, link)

	// One flat table, the speaker beside the words: nested tables do not lay out in this browser.
	b.WriteString("<table width=100%>\n")
	for i, post := range posts {
		shade := p.RowA
		if i%2 == 1 {
			shade = p.RowB
		}
		speaker := igbPortrait(post.AuthorCharID, 64) + "<br><font size=3><b>" + igbPilotLink(post.AuthorCharID, post.AuthorName) + "</b></font>"
		if post.AuthorCorpID != 0 {
			speaker += "<br>" + `<a href="` + corpPublicURL(post.AuthorCorpID) + `">` + igbCorpLogo(post.AuthorCorpID, 32) + "</a>" +
				"<br><font size=2>" + `<a href="` + corpPublicURL(post.AuthorCorpID) + `">` + igbText(post.AuthorCorpName) + "</a></font>"
		}
		when := "<font size=2 color=" + p.Dim + ">" + summitTime(post.CreatedAt)
		if post.EditedAt != 0 {
			when += " &middot; amended by the moderators"
		}
		when += "</font>"
		b.WriteString("<tr><td width=150 valign=top bgcolor=" + shade + ">" + speaker + "</td>" +
			"<td valign=top bgcolor=" + shade + ">" + when + "<br><br><font size=3>" + summitBodyHTML(post.Body) + "</font><br><br></td></tr>\n")
	}
	b.WriteString("</table>\n")
	writeSummitPager(&b, page, pages, link)

	if t.Locked {
		b.WriteString("<br><font size=3 color=" + p.Down + "><b>The moderators have closed this topic. No further responses are heard.</b></font><br>\n")
	} else {
		b.WriteString("<br><font size=4 color=" + p.Title + "><b>Respond</b></font><br>\n")
		s.writeSummitCompose(ctx, &b, q, id, "", draft)
	}
	finishSitePage(w, &b, siteSummit, 0)
}

// writeSummitCompose is the speaking form: a title for a new topic (thread 0), the words, and the
// key when the reader holds no session. It is written outside every table — a form in a cell
// breaks the client's layout (galnet_test.go formInCell).
func (s *NewsServer) writeSummitCompose(ctx context.Context, b *strings.Builder, q summitReq, thread int64, title, body string) {
	p := siteSummit.Pal
	if q.who.ID == 0 {
		b.WriteString("<font size=3 color=" + p.Dim + ">Only a capsuleer at a capsule terminal may address the Summit.</font><br>\n")
		return
	}
	if banned, _ := evedb.SummitBan(ctx, s.db, q.who.ID); banned {
		b.WriteString("<font size=3 color=" + p.Down + ">The moderators have withdrawn your right to address the Summit.</font><br>\n")
		return
	}
	b.WriteString(`<form method=post action="` + newsHost + `/summit/post">` + "\n")
	b.WriteString(`<input type=hidden name=thread value="` + strconv.FormatInt(thread, 10) + `">` + q.summitSessionField() + "\n")
	if thread == 0 {
		b.WriteString("<font size=3>Topic</font><br>" + `<input type=text name=title size=70 maxlength=` +
			strconv.Itoa(evedb.SummitTitleMax) + ` value="` + htmlAttrEscape(title) + `"><br><br>` + "\n")
	}
	b.WriteString("<font size=3>Your words</font><br>" + `<textarea name=body cols=90 rows=12 maxlength=` +
		strconv.Itoa(evedb.SummitBodyMax) + `>` + htmlTextEscape(body) + "</textarea><br>\n")
	if !q.authed() {
		b.WriteString("<font size=3>Summit key</font> <input type=password name=key size=24> <font size=2 color=" + p.Dim +
			">No key? <a href=\"" + q.summitURL("/summit/key", "issue", "1") + "\">Ask the Summit for one</a>; it arrives by EVE-mail.</font><br>\n")
	}
	b.WriteString("<br><input type=submit value=\"Address the Summit\">\n</form>\n")
	b.WriteString("<font size=2 color=" + p.Dim + ">You speak in your own name, under your corporation. Plain words only; formatting is not carried.</font><br>\n")
}

// ── raising and responding ──────────────────────────────────────────────────

// handleSummitNew serves /summit/new, the form for a new topic.
func (s *NewsServer) handleSummitNew(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	q := s.summitResolve(ctx, r)
	s.writeSummitNew(w, ctx, q, "", false, "", "")
}

func (s *NewsServer) writeSummitNew(w http.ResponseWriter, ctx context.Context, q summitReq, banner string, bad bool, title, body string) {
	p := siteSummit.Pal
	var b strings.Builder
	writeSummitHeader(&b, q, "new")
	if banner != "" {
		b.WriteString(summitMsg(banner, bad))
	}
	b.WriteString("<br><font size=5 color=" + p.Title + "><b>Raise a topic</b></font><br>" +
		"<font size=3 color=" + p.Dim + ">Name what you wish to discuss, then make your opening address. " +
		"Every capsuleer on Galnet may read and answer it.</font><br><br>\n")
	s.writeSummitCompose(ctx, &b, q, 0, title, body)
	finishSitePage(w, &b, siteSummit, 0)
}

// handleSummitPost receives the speaking form. It draws its result itself rather than redirecting:
// this browser ignores Refresh on a trusted site. A GET (the browser reloading the result) shows
// the topic or the floor.
func (s *NewsServer) handleSummitPost(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	q := s.summitResolve(ctx, r)
	threadID, _ := strconv.ParseInt(r.FormValue("thread"), 10, 64)
	if r.Method != http.MethodPost {
		if threadID != 0 {
			s.writeSummitThread(w, ctx, q, threadID, -1, "", false, "")
		} else {
			s.handleSummit(w, r)
		}
		return
	}
	title := summitCleanLine(r.PostFormValue("title"))
	body := summitCleanText(r.PostFormValue("body"))
	fail := func(msg string) {
		if threadID != 0 {
			s.writeSummitThread(w, ctx, q, threadID, -1, msg, true, body)
		} else {
			s.writeSummitNew(w, ctx, q, msg, true, title, body)
		}
	}
	if q.who.ID == 0 {
		fail("Only a capsuleer at a capsule terminal may address the Summit.")
		return
	}
	if threadID == 0 && title == "" {
		fail("Name your topic.")
		return
	}
	if body == "" {
		fail("You have said nothing.")
		return
	}
	if !q.authed() {
		var err error
		if q, err = s.summitSignIn(ctx, w, q, r.PostFormValue("key")); err != nil {
			fail(summitErrText(err))
			return
		}
	}
	if threadID == 0 {
		id, err := evedb.CreateSummitThread(ctx, s.db, q.who.ID, title, body)
		if err != nil {
			log.Printf("summit: create by %d: %v", q.who.ID, err)
			fail(summitErrText(err))
			return
		}
		log.Printf("summit: topic %d raised by %s (%d)", id, q.who.Name, q.who.ID)
		s.writeSummitThread(w, ctx, q, id, 0, "Your topic stands before the Summit.", false, "")
		return
	}
	page, err := evedb.ReplySummitThread(ctx, s.db, threadID, q.who.ID, body)
	if err != nil {
		log.Printf("summit: reply to %d by %d: %v", threadID, q.who.ID, err)
		fail(summitErrText(err))
		return
	}
	s.writeSummitThread(w, ctx, q, threadID, page, "Your words are on the record.", false, "")
}

// summitErrText is a refusal as the pilot reads it.
func summitErrText(err error) string {
	switch {
	case errors.Is(err, errSummitNeedKey), errors.Is(err, evedb.ErrSummitNoAccount), errors.Is(err, evedb.ErrSummitBadKey),
		errors.Is(err, evedb.ErrSummitKeyLocked), errors.Is(err, evedb.ErrSummitBanned), errors.Is(err, evedb.ErrSummitLocked),
		errors.Is(err, evedb.ErrSummitNoThread), errors.Is(err, evedb.ErrSummitEmpty), errors.Is(err, evedb.ErrSummitNoPilot):
		msg := err.Error()
		return strings.ToUpper(msg[:1]) + msg[1:] + "."
	}
	return "The Summit could not record that just now."
}

// ── credentials ─────────────────────────────────────────────────────────────

// handleSummitKey serves /summit/key: ask for a key (?issue=1), present one (POST key), or leave
// (?leave=1).
func (s *NewsServer) handleSummitKey(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	_ = r.ParseForm()
	q := s.summitResolve(ctx, r)
	p := siteSummit.Pal
	var b strings.Builder
	banner, bad := "", false

	switch {
	case q.who.ID == 0:
	case r.FormValue("leave") == "1":
		http.SetCookie(w, &http.Cookie{Name: summitCookie, Value: "", Path: "/summit/", MaxAge: -1})
		q.token = ""
		banner = "You have stepped down from the floor. Present your key to speak again."
	case r.FormValue("issue") == "1":
		key, err := evedb.IssueSummitKey(ctx, s.db, q.who.ID)
		if err != nil || SummitMailHook == nil {
			if err != nil {
				log.Printf("summit: issue key %d: %v", q.who.ID, err)
			}
			banner, bad = "The Summit could not issue a key just now.", true
			break
		}
		q.token = ""
		SummitMailHook(q.who.ID, "Your Intergalactic Summit credentials",
			"InterBus, on behalf of the Intergalactic Summit, has registered you to address the floor.\n\n"+
				"Your Summit key: "+key+"\n\n"+
				"Present it at any Galnet terminal when you first speak; the Summit will know you for the rest of "+
				"the session. Keep it to yourself: anyone holding it may speak in your name. Asking for a new key "+
				"voids this one.")
		banner = "Your Summit key is on its way by EVE-mail. It arrives in a moment."
	case r.Method == http.MethodPost:
		var err error
		if q, err = s.summitSignIn(ctx, w, q, r.PostFormValue("key")); err != nil {
			banner, bad = summitErrText(err), true
		} else {
			banner = "Credentials accepted. The floor is yours."
		}
	}

	writeSummitHeader(&b, q, "key")
	if banner != "" {
		b.WriteString(summitMsg(banner, bad))
	}
	b.WriteString("<br><font size=5 color=" + p.Title + "><b>Credentials</b></font><br>\n")
	if q.who.ID == 0 {
		b.WriteString("<font size=3>The Summit cannot confirm who you are. Open it from a capsule terminal.</font><br>")
		finishSitePage(w, &b, siteSummit, 0)
		return
	}
	if banned, reason := evedb.SummitBan(ctx, s.db, q.who.ID); banned {
		b.WriteString("<font size=3 color=" + p.Down + ">The moderators have withdrawn your right to address the Summit.")
		if reason != "" {
			b.WriteString(" Reason given: " + igbText(reason))
		}
		b.WriteString("</font><br>")
		finishSitePage(w, &b, siteSummit, 0)
		return
	}
	b.WriteString("<font size=3>Anyone may listen. To speak, a capsuleer proves who they are with a key the Summit sends to their " +
		"EVE-mail. Present it once and the Summit knows you until you leave Galnet.</font><br><br>\n")
	if q.authed() {
		b.WriteString("<font size=3 color=" + p.Up + "><b>You hold the floor as " + igbText(q.who.Name) + ".</b></font><br><br>" +
			"<font size=3><a href=\"" + q.summitURL("/summit/") + "\">Return to the floor</a> &nbsp; " +
			"<a href=\"" + q.summitURL("/summit/new") + "\">Raise a topic</a> &nbsp; " +
			"<a href=\"" + newsHost + "/summit/key?leave=1\">Step down</a></font><br>\n")
		finishSitePage(w, &b, siteSummit, 0)
		return
	}
	if evedb.HasSummitKey(ctx, s.db, q.who.ID) {
		b.WriteString(`<form method=post action="` + newsHost + `/summit/key">` + "\n" +
			"<font size=3>Summit key</font> <input type=password name=key size=24> <input type=submit value=\"Present\">\n</form>\n")
		b.WriteString("<font size=2 color=" + p.Dim + ">Lost it? <a href=\"" + newsHost +
			"/summit/key?issue=1\">Ask for a new key</a> (the old one stops working).</font><br>\n")
	} else {
		b.WriteString("<font size=4><b><a href=\"" + newsHost + "/summit/key?issue=1\">Ask the Summit for a key</a></b></font><br>" +
			"<font size=2 color=" + p.Dim + ">It is sent to " + igbText(q.who.Name) + " by EVE-mail.</font><br>\n")
	}
	finishSitePage(w, &b, siteSummit, 0)
}

// ── Galnet front door ───────────────────────────────────────────────────────

// galnetSummitLine is the directory tile's live line: the liveliest topic.
func (s *NewsServer) galnetSummitLine(ctx context.Context) string {
	ts, err := evedb.LatestSummitThreads(ctx, s.db, 1)
	if err != nil || len(ts) == 0 {
		return "The floor is silent."
	}
	t := ts[0]
	return `On the floor: <a href="` + newsHost + `/summit/thread?id=` + strconv.FormatInt(t.ThreadID, 10) + `&p=last">` +
		igbText(igbTrim(t.Title, 50)) + `</a> (` + igbInt(int64(t.ReplyCount)) + `)`
}

// galnetSummitPanel is the "On the network" panel: the topics most recently spoken in.
func (s *NewsServer) galnetSummitPanel(ctx context.Context) igbBandPanel {
	pn := igbBandPanel{Title: galnetTitle("Intergalactic Summit", "/summit/"), Note: "latest on the floor",
		Align: []string{"", "right", "right"}, Width: []string{"", "40", "160"}, Empty: "The Summit floor is quiet."}
	ts, err := evedb.LatestSummitThreads(ctx, s.db, galnetPanelRows)
	if err != nil {
		return pn
	}
	dim := siteGalnet.Pal.Dim
	for _, t := range ts {
		pn.Rows = append(pn.Rows, []string{
			`<font size=3><a href="` + newsHost + `/summit/thread?id=` + strconv.FormatInt(t.ThreadID, 10) + `&p=last">` +
				igbText(igbTrim(t.Title, 70)) + `</a></font><br><font size=2 color=` + dim + `>raised by ` + igbText(t.AuthorName) + `</font>`,
			"<font size=3>" + igbInt(int64(t.ReplyCount)) + "</font>",
			"<font size=2 color=" + dim + ">" + igbText(t.LastPostName) + "</font><br><font size=2>" + igbYC(time.Unix(t.LastPostAt, 0)) + "</font>",
		})
	}
	return pn
}
