package esp

// Killboard admin pages. Read-only: every number was snapshotted at kill time (names, ISK values,
// per-attacker damage), so nothing here re-prices or re-resolves. The in-game browser's pages
// are in igbkillboard.go.

import (
	"context"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

const killboardPageSize = 40

// killboardPurgeToken is what an operator types to arm the purge. The confirm() dialog is
// client-side and a curl never sees it, so this word is the server-side gate.
const killboardPurgeToken = "PURGE"

// handleKillboard serves /killboard: the filtered list plus the window's
// headline numbers and leaderboards. POST is the purge.
func (s *Server) handleKillboard(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if r.Method == http.MethodPost {
		s.handleKillboardPurge(ctx, w, r)
		return
	}

	q := r.URL.Query()
	days := int(espQueryInt(q.Get("days"), 7))
	if days < 1 {
		days = 1
	}
	page := int(espQueryInt(q.Get("page"), 0))
	if page < 0 {
		page = 0
	}
	kind := q.Get("kind")
	if kind != "ship" && kind != "pod" {
		kind = ""
	}
	npc := q.Get("npc") == "1"
	since := time.Now().Add(-time.Duration(days) * 24 * time.Hour)

	filter := evedb.KillmailFilter{
		Since:         since,
		SolarSystemID: espQueryInt(q.Get("sys"), 0),
		RegionID:      espQueryInt(q.Get("region"), 0),
		VictimCharID:  espQueryInt(q.Get("char"), 0),
		VictimCorpID:  espQueryInt(q.Get("corp"), 0),
		Involving:     espQueryInt(q.Get("involving"), 0),
		MinValue:      espQueryFloat(q.Get("minisk"), 0),
		Kind:          kind,
		NameLike:      strings.TrimSpace(q.Get("q")),
		IncludeNPCKills: npc,
		Limit:         killboardPageSize,
		Offset:        page * killboardPageSize,
	}

	kills, err := evedb.ListKillmails(ctx, s.db, filter)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "killboard"})
		return
	}
	total, _ := evedb.CountKillmails(ctx, s.db, filter)
	kills24, isk24, _ := evedb.KillboardTotals(ctx, s.db, time.Now().Add(-24*time.Hour), npc)
	winKills, winISK, _ := evedb.KillboardTotals(ctx, s.db, since, npc)
	topKillers, _ := evedb.TopKillers(ctx, s.db, since, 10, npc)
	topLosses, _ := evedb.TopLosses(ctx, s.db, since, 10, npc)

	pageCount := int((total + killboardPageSize - 1) / killboardPageSize)

	// An empty list is ambiguous (nothing died, or the filters cut everything); the page says which.
	var unfiltered int64
	if len(kills) == 0 {
		unfiltered, _ = evedb.CountKillmails(ctx, s.db, evedb.KillmailFilter{IncludeNPCKills: true, Limit: 1})
	}

	// The ranked panel shows on the front page only (page 0, nothing narrowed). The window ladder is
	// the in-game browser's: widen until five rows exist and name the window it settled on.
	var topValue []evedb.KillmailRow
	var topValueLabel string
	if page == 0 && kind == "" && filter.NameLike == "" && filter.MinValue == 0 &&
		filter.SolarSystemID == 0 && filter.RegionID == 0 && filter.VictimCharID == 0 &&
		filter.VictimCorpID == 0 && filter.Involving == 0 {
		topValue, topValueLabel = s.killboardTopValue(ctx, npc)
	}

	// The purge card is behind ?purge=1: its row counts are four unfiltered COUNT(*)s, a full index
	// scan on a large killmail_items.
	showPurge := q.Get("purge") == "1"
	var kmCount, botCount, atkCount, itemCount, rollCount int64
	if showPurge {
		kmCount, botCount, atkCount, itemCount, rollCount, _ = evedb.KillboardCounts(ctx, s.db)
	}

	s.render(w, "killboard.html", map[string]any{
		"Kills":         kills,
		"TopValue":      topValue,
		"TopValueLabel": topValueLabel,
		"Total":         total,
		"Unfiltered":    unfiltered,
		"Msg":           r.URL.Query().Get("msg"),
		"ShowPurge":     showPurge,
		"RowsKills":     kmCount,
		"RowsBots":      botCount,
		"RowsPlayer":    kmCount - botCount,
		"RowsAtk":       atkCount,
		"RowsItems":     itemCount,
		"RowsRollup":    rollCount,
		"PurgeWord":     killboardPurgeToken,
		"Kills24h":      kills24,
		"ISK24h":        isk24,
		"WindowKill":    winKills,
		"WindowISK":     winISK,
		"TopKillers":    topKillers,
		"TopLosses":     topLosses,
		"Days":          days,
		"Kind":          kind,
		"NPC":           npc,
		"Query":         filter.NameLike,
		"MinISK":        q.Get("minisk"),
		"PageNum":       page,
		"PageCount":     pageCount,
		"Prev":          killboardQuery(q, page-1),
		"Next":          killboardQuery(q, page+1),
		"Page":          "killboard",
	})
}

// killboardTopValue walks the shared window ladder until the ranked panel has enough rows, and
// returns the window it settled on. Same ladder and count as the in-game browser's panel.
func (s *Server) killboardTopValue(ctx context.Context, npc bool) ([]evedb.KillmailRow, string) {
	for _, w := range igbTopValueWindows {
		f := evedb.KillmailFilter{IncludeNPCKills: npc, Limit: igbTopValueCount}
		if w.Dur > 0 {
			f.Since = time.Now().Add(-w.Dur)
		}
		rows, err := evedb.ListValuableKillmails(ctx, s.db, f)
		if err != nil {
			// A failed panel query does not fail the page.
			log.Printf("esp: killboard top value: %v", err)
			return nil, ""
		}
		if len(rows) >= igbTopValueCount || w.Dur == 0 {
			return rows, w.Label
		}
	}
	return nil, ""
}

// handleKillboardPurge runs the purge and redirects back with a result line, refusals included.
// The scope has no default and the confirmation word must match exactly. The request context can
// cut a long batched DELETE short between batches; running it again finishes the job.
func (s *Server) handleKillboardPurge(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	back := func(msg string) {
		// Back to the card: the counts under it are the receipt.
		http.Redirect(w, r, "/killboard?purge=1&msg="+url.QueryEscape(msg), http.StatusSeeOther)
	}
	if err := r.ParseForm(); err != nil {
		back("Bad form: " + err.Error())
		return
	}
	if r.FormValue("action") != "purge" {
		back("Unknown action.")
		return
	}
	if strings.TrimSpace(r.FormValue("confirm")) != killboardPurgeToken {
		back("Purge cancelled: type " + killboardPurgeToken + " to confirm. Nothing was deleted.")
		return
	}

	scope := evedb.KillPurgeScope(strings.TrimSpace(r.FormValue("scope")))
	if !scope.Valid() {
		back("Purge cancelled: pick what to remove. Nothing was deleted.")
		return
	}
	alsoRollup := r.FormValue("rollup") == "1"

	res, err := evedb.PurgeKillboard(ctx, s.db, scope, alsoRollup)
	evedb.LogKillPurge(res, err)
	if err != nil {
		back("Purge failed: " + err.Error())
		return
	}

	what := "every killmail"
	if scope == evedb.KillPurgeBots {
		what = "every synthetic killmail"
	}
	msg := fmt.Sprintf("Purged %s: %s kill(s), %s attacker row(s), %s item row(s)",
		what, commaInt(res.Kills), commaInt(res.Attackers), commaInt(res.Items))
	if alsoRollup {
		msg += fmt.Sprintf(", %s archived rollup row(s)", commaInt(res.Rollup))
	}
	if res.Truncated {
		msg += " (tables truncated; killIDs restart at 1)"
	}
	back(msg + fmt.Sprintf(" in %s.", res.Elapsed.Round(time.Millisecond)))
}

// handleKillmailView serves /killmail/<id>.
func (s *Server) handleKillmailView(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	idStr := strings.TrimPrefix(r.URL.Path, "/killmail/")
	killID, err := strconv.ParseInt(strings.TrimSuffix(idStr, "/"), 10, 64)
	if err != nil || killID <= 0 {
		s.render(w, "error.html", map[string]any{"Error": "Bad kill ID.", "Page": "killboard"})
		return
	}
	km, err := evedb.GetKillmail(ctx, s.db, killID)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "killboard"})
		return
	}
	if km == nil {
		s.render(w, "error.html", map[string]any{"Error": "No such kill report.", "Page": "killboard"})
		return
	}

	// Bands, not survival-split lists: the page renders the same fit as the in-game browser.
	s.render(w, "killmail_view.html", map[string]any{
		"K":         km,
		"Bands":     km.FitBands(),
		"Dropped":   km.Dropped(),
		"Destroyed": km.Destroyed(),
		"Page":      "killboard",
	})
}

// killboardQuery rebuilds the current query string at a different page, so the
// pager keeps every filter the user set.
func killboardQuery(q map[string][]string, page int) string {
	parts := make([]string, 0, 8)
	for _, key := range []string{"days", "kind", "npc", "q", "minisk", "sys", "region", "char", "corp", "involving"} {
		if v := firstValue(q, key); v != "" {
			parts = append(parts, key+"="+url.QueryEscape(v))
		}
	}
	if page > 0 {
		parts = append(parts, "page="+strconv.Itoa(page))
	}
	if len(parts) == 0 {
		return "/killboard"
	}
	return "/killboard?" + strings.Join(parts, "&")
}

func firstValue(q map[string][]string, key string) string {
	if vs, ok := q[key]; ok && len(vs) > 0 {
		return vs[0]
	}
	return ""
}

func espQueryInt(s string, def int64) int64 {
	if s == "" {
		return def
	}
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return def
	}
	return v
}

// espPageQuery re-renders the current query string with page replaced, keeping every other
// parameter. It returns a leading-"?" template.URL; templates write href="{{.Next}}", never
// href="?{{.Next}}", which html/template escapes as one query value and drops the filters.
func espPageQuery(q url.Values, page int) template.URL {
	out := url.Values{}
	for k, vs := range q {
		if k == "page" || len(vs) == 0 || vs[0] == "" {
			continue
		}
		out.Set(k, vs[0])
	}
	if page > 0 {
		out.Set("page", strconv.Itoa(page))
	}
	if len(out) == 0 {
		return template.URL("?")
	}
	return template.URL("?" + out.Encode())
}

func espQueryFloat(s string, def float64) float64 {
	if s == "" {
		return def
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return def
	}
	return v
}
