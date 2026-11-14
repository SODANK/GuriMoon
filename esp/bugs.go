package esp

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

// bugStatuses is the workflow order shown in the ESP filter and status
// buttons.
var bugStatuses = []string{
	evedb.BugStatusOpen, evedb.BugStatusAcked,
	evedb.BugStatusFixed, evedb.BugStatusIgnored,
}

// bugCategories are the signal categories the bugtrack package emits.
var bugCategories = []string{"panic", "unhandled", "exception", "error"}

// handleBugs serves the Bug Tracker page: deduplicated bug signals collected
// automatically from the server log stream, unhandled-call reports and
// recovered panics.  GET filters by status/category; POST applies status
// changes and deletes.
func (s *Server) handleBugs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if r.Method == http.MethodPost {
		s.handleBugsPost(w, r, ctx)
		return
	}

	status := r.URL.Query().Get("status")
	if status == "" && !r.URL.Query().Has("status") {
		status = evedb.BugStatusOpen // default view: open bugs
	}
	category := r.URL.Query().Get("category")

	bugs, err := evedb.ListBugReports(ctx, s.db, status, category)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "bugs"})
		return
	}
	counts, err := evedb.CountBugsByStatus(ctx, s.db)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "bugs"})
		return
	}

	s.render(w, "bugs.html", map[string]any{
		"Bugs":       bugs,
		"Counts":     counts,
		"Statuses":   bugStatuses,
		"Categories": bugCategories,
		"Status":     status,
		"Category":   category,
		"Msg":        r.URL.Query().Get("msg"),
		"Err":        r.URL.Query().Get("err"),
		"Page":       "bugs",
	})
}

// handleBugsPost applies list-page actions (status change, delete) and
// redirects back preserving the active filters.
func (s *Server) handleBugsPost(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	bugID, err := strconv.ParseInt(r.FormValue("bugID"), 10, 64)
	if err != nil || bugID == 0 {
		redirectBugs(w, r, "", "invalid bug ID")
		return
	}
	switch r.FormValue("action") {
	case "status":
		if err := evedb.UpdateBugStatus(ctx, s.db, bugID, r.FormValue("status")); err != nil {
			redirectBugs(w, r, "", err.Error())
			return
		}
		redirectBugs(w, r, fmt.Sprintf("bug %d marked %s", bugID, r.FormValue("status")), "")
	case "delete":
		if err := evedb.DeleteBugReport(ctx, s.db, bugID); err != nil {
			redirectBugs(w, r, "", err.Error())
			return
		}
		redirectBugs(w, r, fmt.Sprintf("bug %d deleted", bugID), "")
	default:
		redirectBugs(w, r, "", "unknown action")
	}
}

// redirectBugs sends the browser back to /bugs, carrying over the filter
// query and a message or error.
func redirectBugs(w http.ResponseWriter, r *http.Request, msg, errMsg string) {
	q := url.Values{}
	if v := r.FormValue("filterStatus"); v != "" || r.Form.Has("filterStatus") {
		q.Set("status", v)
	}
	if v := r.FormValue("filterCategory"); v != "" {
		q.Set("category", v)
	}
	if msg != "" {
		q.Set("msg", msg)
	} else if errMsg != "" {
		q.Set("err", errMsg)
	}
	target := "/bugs"
	if enc := q.Encode(); enc != "" {
		target += "?" + enc
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// handleBugView serves the bug detail page: /bug/<id>.  Shows the full
// sample message and offers status buttons plus an admin notes form.
func (s *Server) handleBugView(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 3 {
		http.NotFound(w, r)
		return
	}
	bugID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		switch r.FormValue("action") {
		case "status":
			if err := evedb.UpdateBugStatus(ctx, s.db, bugID, r.FormValue("status")); err != nil {
				http.Redirect(w, r, fmt.Sprintf("/bug/%d?err=%s", bugID, url.QueryEscape(err.Error())), http.StatusSeeOther)
				return
			}
			http.Redirect(w, r, fmt.Sprintf("/bug/%d?msg=%s", bugID,
				url.QueryEscape("status set to "+r.FormValue("status"))), http.StatusSeeOther)
		case "notes":
			if err := evedb.UpdateBugNotes(ctx, s.db, bugID, r.FormValue("notes")); err != nil {
				http.Redirect(w, r, fmt.Sprintf("/bug/%d?err=%s", bugID, url.QueryEscape(err.Error())), http.StatusSeeOther)
				return
			}
			http.Redirect(w, r, fmt.Sprintf("/bug/%d?msg=%s", bugID,
				url.QueryEscape("notes saved")), http.StatusSeeOther)
		case "delete":
			if err := evedb.DeleteBugReport(ctx, s.db, bugID); err != nil {
				http.Redirect(w, r, fmt.Sprintf("/bug/%d?err=%s", bugID, url.QueryEscape(err.Error())), http.StatusSeeOther)
				return
			}
			http.Redirect(w, r, "/bugs?msg="+url.QueryEscape(fmt.Sprintf("bug %d deleted", bugID)), http.StatusSeeOther)
		default:
			http.Redirect(w, r, fmt.Sprintf("/bug/%d?err=unknown+action", bugID), http.StatusSeeOther)
		}
		return
	}

	bug, err := evedb.GetBugReport(ctx, s.db, bugID)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "bugs"})
		return
	}
	if bug == nil {
		s.render(w, "error.html", map[string]any{
			"Error": fmt.Sprintf("bug %d not found", bugID), "Page": "bugs"})
		return
	}

	s.render(w, "bug_view.html", map[string]any{
		"Bug":       bug,
		"Statuses":  bugStatuses,
		"FirstSeen": time.Unix(bug.FirstSeen, 0).UTC().Format("2006-01-02 15:04:05 UTC"),
		"LastSeen":  time.Unix(bug.LastSeen, 0).UTC().Format("2006-01-02 15:04:05 UTC"),
		"Msg":       r.URL.Query().Get("msg"),
		"Err":       r.URL.Query().Get("err"),
		"Page":      "bugs",
	})
}
