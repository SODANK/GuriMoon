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

// The petition desk from the admin side: reply, claim, unclaim, escalate and
// close over the rows the in-game GM tabs read. A reply is signed
// PetitionAdminSenderID and reaches the pilot through PetitionEventHook.

var petitionFilters = []string{"open", "claimed", "closed", "all"}

func (s *Server) handlePetitions(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	filter := r.URL.Query().Get("filter")
	if filter == "" {
		filter = "open"
	}
	ps, err := evedb.ListPetitionsForAdmin(ctx, s.db, filter)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "petitions"})
		return
	}
	cats, _ := evedb.ListPetitionCategories(ctx, s.db)
	catName := map[int64]string{}
	for _, c := range cats {
		catName[c.CategoryID] = c.DisplayName
	}
	open, claimed, closed := evedb.CountPetitionsByState(ctx, s.db)
	type row struct {
		evedb.Petition
		Category   string
		Petitioner string
		Created    string
		Touched    string
		Status     string
	}
	rows := make([]row, 0, len(ps))
	for _, p := range ps {
		rows = append(rows, row{
			Petition: p, Category: catName[p.CategoryID],
			Petitioner: s.charName(ctx, p.PetitionerID),
			Created:    time.Unix(p.CreateDate, 0).UTC().Format("2006-01-02 15:04"),
			Touched:    time.Unix(p.TouchDate, 0).UTC().Format("2006-01-02 15:04"),
			Status:     petitionStatus(p),
		})
	}
	s.render(w, "petitions.html", map[string]any{
		"Petitions": rows, "Filters": petitionFilters, "Filter": filter,
		"Open": open, "Claimed": claimed, "Closed": closed,
		"Msg": r.URL.Query().Get("msg"), "Err": r.URL.Query().Get("err"), "Page": "petitions",
	})
}

func petitionStatus(p evedb.Petition) string {
	switch {
	case p.Deleted:
		return "cancelled"
	case p.Closed:
		return "closed"
	case p.ClaimedBy != 0:
		return "in action"
	}
	return "open"
}

func (s *Server) charName(ctx context.Context, charID int64) string {
	if charID == PetitionAdminSenderID {
		return "CONCORD"
	}
	var name string
	_ = s.db.QueryRowContext(ctx, `SELECT characterName FROM character_ WHERE characterID = ?`, charID).Scan(&name)
	if name == "" {
		return fmt.Sprintf("#%d", charID)
	}
	return name
}

func redirectPetition(w http.ResponseWriter, r *http.Request, id int64, msg, errMsg string) {
	q := url.Values{}
	if msg != "" {
		q.Set("msg", msg)
	} else if errMsg != "" {
		q.Set("err", errMsg)
	}
	target := fmt.Sprintf("/petition/%d", id)
	if enc := q.Encode(); enc != "" {
		target += "?" + enc
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (s *Server) handlePetitionView(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 3 {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	p, err := evedb.GetPetition(ctx, s.db, id)
	if err != nil || p == nil {
		http.NotFound(w, r)
		return
	}

	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		push := func(event string, text string, messageID int64) {
			if PetitionEventHook != nil {
				PetitionEventHook(event, p.PetitionID, p.PetitionerID, text, messageID)
			}
		}
		switch r.FormValue("action") {
		case "reply":
			text := strings.TrimSpace(r.FormValue("text"))
			if text == "" {
				redirectPetition(w, r, id, "", "empty reply")
				return
			}
			if p.Closed {
				redirectPetition(w, r, id, "", "the petition is closed")
				return
			}
			comment := r.FormValue("comment") == "1"
			mid, err := evedb.AddPetitionMessage(ctx, s.db, id, PetitionAdminSenderID, text, comment)
			if err != nil {
				redirectPetition(w, r, id, "", err.Error())
				return
			}
			if !comment {
				evedb.SetPetitionUpdated(ctx, s.db, id, false)
				push("message", text, mid)
			}
			evedb.AddPetitionLog(ctx, s.db, id, 0, "ESP", map[bool]string{true: "Comment added", false: "GM wrote"}[comment])
			redirectPetition(w, r, id, "reply sent", "")
		case "claim":
			if ok, _ := evedb.ClaimPetition(ctx, s.db, id, PetitionAdminSenderID); !ok {
				redirectPetition(w, r, id, "", "already claimed or closed")
				return
			}
			evedb.AddPetitionLog(ctx, s.db, id, 0, "ESP", "Claimed")
			push("assign", "", 0)
			redirectPetition(w, r, id, "claimed", "")
		case "unclaim":
			_ = evedb.UnclaimPetition(ctx, s.db, id)
			evedb.AddPetitionLog(ctx, s.db, id, 0, "ESP", "Unclaimed")
			push("assign", "", 0)
			redirectPetition(w, r, id, "unclaimed", "")
		case "escalate":
			to := p.EscalatesTo
			if to == 0 {
				redirectPetition(w, r, id, "", "nothing to escalate to")
				return
			}
			_ = evedb.EscalatePetition(ctx, s.db, id, to)
			evedb.AddPetitionLog(ctx, s.db, id, 0, "ESP", fmt.Sprintf("Escalated to queue %d", to))
			redirectPetition(w, r, id, "escalated", "")
		case "close":
			_ = evedb.ClosePetition(ctx, s.db, id, PetitionAdminSenderID)
			evedb.AddPetitionLog(ctx, s.db, id, 0, "ESP", "Closed")
			push("close", "", 0)
			redirectPetition(w, r, id, "closed", "")
		default:
			redirectPetition(w, r, id, "", "unknown action")
		}
		return
	}

	cat, _ := evedb.GetPetitionCategory(ctx, s.db, p.CategoryID)
	catName := fmt.Sprintf("category %d", p.CategoryID)
	if cat != nil {
		catName = cat.DisplayName
	}
	msgs, _ := evedb.ListPetitionMessages(ctx, s.db, id, true)
	type msgRow struct {
		evedb.PetitionMessage
		Sender string
		Sent   string
	}
	mrows := make([]msgRow, 0, len(msgs))
	for _, m := range msgs {
		mrows = append(mrows, msgRow{m, s.charName(ctx, m.SenderID), time.Unix(m.SentDate, 0).UTC().Format("2006-01-02 15:04")})
	}
	logs, _ := evedb.ListPetitionLog(ctx, s.db, id)
	type logRow struct {
		evedb.PetitionLogEntry
		When string
	}
	lrows := make([]logRow, 0, len(logs))
	for _, e := range logs {
		lrows = append(lrows, logRow{e, time.Unix(e.LogDateTime, 0).UTC().Format("2006-01-02 15:04")})
	}
	claimer := ""
	if p.ClaimedBy != 0 {
		claimer = s.charName(ctx, p.ClaimedBy)
	}
	s.render(w, "petition_view.html", map[string]any{
		"P": p, "Category": catName, "Petitioner": s.charName(ctx, p.PetitionerID), "Claimer": claimer,
		"Status":   petitionStatus(*p),
		"Created":  time.Unix(p.CreateDate, 0).UTC().Format("2006-01-02 15:04"),
		"Messages": mrows, "Log": lrows,
		"Msg": r.URL.Query().Get("msg"), "Err": r.URL.Query().Get("err"), "Page": "petitions",
	})
}
