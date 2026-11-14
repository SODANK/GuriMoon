package esp

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	evedb "gurimoon/db"
)

func (s *Server) handleEscrows(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if r.Method == http.MethodPost {
		s.handleEscrowsAction(w, r, ctx)
		return
	}

	stateFilter := -1
	if sq := r.URL.Query().Get("state"); sq != "" {
		if v, err := strconv.Atoi(sq); err == nil {
			stateFilter = v
		}
	}

	listings, err := evedb.ESPListEscrows(ctx, s.db, stateFilter)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error()})
		return
	}

	stats := evedb.ESPGetEscrowStats(ctx, s.db)

	s.render(w, "escrows.html", map[string]any{
		"Listings":    listings,
		"Stats":       stats,
		"StateFilter": stateFilter,
		// Closed listings are pruned (db/historyretention.go), so the
		// counts are a window, not a lifetime; the page says which.
		"ClosedKeepDays": evedb.EscrowClosedKeepDays(ctx, s.db),
		"Msg":            r.URL.Query().Get("msg"),
		"Page":           "escrows",
	})
}

func (s *Server) handleEscrowsAction(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/escrows?msg="+url.QueryEscape("Bad form: "+err.Error()), http.StatusSeeOther)
		return
	}

	var result string

	switch r.FormValue("action") {
	case "cancel", "expire":
		escrowID := espFormInt(r, "missionID")
		if err := evedb.ExpireEscrow(ctx, s.db, escrowID); err != nil {
			result = "Error: " + err.Error()
			break
		}
		result = "Escrow listing cancelled. Items returned to issuer."

	default:
		result = "Unknown action."
	}

	http.Redirect(w, r, "/escrows?msg="+url.QueryEscape(result), http.StatusSeeOther)
}
