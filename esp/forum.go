package esp

// The Forum admin page: moderation of the Intergalactic Summit (summit.go, db/summitdb.go). Pin,
// close, hide and restore topics; hide, restore and amend responses; bar a pilot or void their
// key. Nothing is ever removed, so every action can be undone here.

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	evedb "gurimoon/db"
)

// handleForum serves /forum (the topic list, ?tab=posters for the pilots) and /forum/thread/<id>.
func (s *Server) handleForum(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		msg := s.forumAction(r)
		back := r.FormValue("back")
		if !strings.HasPrefix(back, "/forum") {
			back = "/forum"
		}
		sep := "?"
		if strings.Contains(back, "?") {
			sep = "&"
		}
		http.Redirect(w, r, back+sep+"msg="+url.QueryEscape(msg), http.StatusSeeOther)
		return
	}

	q := r.URL.Query()
	data := map[string]any{"Page": "forum", "Msg": q.Get("msg"), "MsgBad": strings.HasPrefix(q.Get("msg"), "Error")}
	data["Threads"], data["Posts"], data["Pilots"] = evedb.SummitCounts(ctx, s.db)

	if rest := strings.TrimPrefix(r.URL.Path, "/forum/thread/"); rest != r.URL.Path {
		id, _ := strconv.ParseInt(rest, 10, 64)
		t, err := evedb.GetSummitThread(ctx, s.db, id, true)
		if err != nil {
			s.render(w, "error.html", map[string]any{"Error": "No such topic.", "Page": "forum"})
			return
		}
		posts, _, err := evedb.ListSummitPosts(ctx, s.db, id, -1, true)
		if err != nil {
			s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "forum"})
			return
		}
		data["View"] = "thread"
		data["Thread"] = t
		data["PostList"] = posts
		data["Back"] = r.URL.Path
		s.render(w, "forum.html", data)
		return
	}

	if q.Get("tab") == "posters" {
		accounts, err := evedb.ListSummitAccounts(ctx, s.db, 500)
		if err != nil {
			s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "forum"})
			return
		}
		data["View"] = "posters"
		data["Accounts"] = accounts
		data["Back"] = "/forum?tab=posters"
		s.render(w, "forum.html", data)
		return
	}

	page, _ := strconv.Atoi(q.Get("p"))
	page = max(page, 0)
	threads, total, err := evedb.ListSummitThreads(ctx, s.db, page, true)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "forum"})
		return
	}
	pages := summitPages(total, evedb.SummitThreadsPerPage)
	data["View"] = "list"
	data["List"] = threads
	data["PageNo"] = page
	data["Pages"] = pages
	data["Prev"] = page - 1
	data["Next"] = page + 1
	data["HasNext"] = page+1 < pages
	data["Back"] = "/forum?p=" + strconv.Itoa(page)
	s.render(w, "forum.html", data)
}

// forumAction applies one moderation action and returns the line to show.
func (s *Server) forumAction(r *http.Request) string {
	ctx := r.Context()
	id := parseInt64(r.FormValue("id"))
	sticky, locked := r.FormValue("sticky") == "1", r.FormValue("locked") == "1"
	var err error
	var done string
	switch r.FormValue("action") {
	case "flags":
		err = evedb.SetSummitThreadFlags(ctx, s.db, id, sticky, locked)
		done = "Topic #" + strconv.FormatInt(id, 10) + " updated."
	case "hideThread", "restoreThread":
		hide := r.FormValue("action") == "hideThread"
		err = evedb.SetSummitThreadDeleted(ctx, s.db, id, hide)
		done = "Topic #" + strconv.FormatInt(id, 10) + map[bool]string{true: " hidden.", false: " restored."}[hide]
	case "hidePost", "restorePost":
		hide := r.FormValue("action") == "hidePost"
		err = evedb.SetSummitPostDeleted(ctx, s.db, id, hide)
		done = "Response #" + strconv.FormatInt(id, 10) + map[bool]string{true: " hidden.", false: " restored."}[hide]
	case "editPost":
		err = evedb.EditSummitPost(ctx, s.db, id, summitCleanText(r.FormValue("body")))
		done = "Response #" + strconv.FormatInt(id, 10) + " amended."
	case "ban", "unban":
		ban := r.FormValue("action") == "ban"
		err = evedb.SetSummitBan(ctx, s.db, id, ban, r.FormValue("reason"))
		done = "Pilot " + strconv.FormatInt(id, 10) + map[bool]string{true: " barred from the Summit.", false: " readmitted."}[ban]
	case "revoke":
		err = evedb.RevokeSummitKey(ctx, s.db, id)
		done = "Pilot " + strconv.FormatInt(id, 10) + "'s key voided."
	default:
		return "Unknown action."
	}
	if err != nil {
		return "Error: " + err.Error()
	}
	return done
}
