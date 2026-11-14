package esp

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

func (s *Server) handleCouriers(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if r.Method == http.MethodPost {
		s.handleCouriersAction(w, r, ctx)
		return
	}

	// State filter from query.
	stateFilter := -1
	if sq := r.URL.Query().Get("state"); sq != "" {
		if v, err := strconv.Atoi(sq); err == nil {
			stateFilter = v
		}
	}

	contracts, err := evedb.ESPListCouriers(ctx, s.db, stateFilter)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error()})
		return
	}

	stats := evedb.ESPGetCourierStats(ctx, s.db)

	// Read settings.
	listingFee := evedb.CourierBasicFee
	maxVolume := evedb.CourierMaxVolumeM3

	s.render(w, "couriers.html", map[string]any{
		"Contracts":   contracts,
		"Stats":       stats,
		"StateFilter": stateFilter,
		"ListingFee":  listingFee,
		"MaxVolume":   maxVolume,
		"Msg":         r.URL.Query().Get("msg"),
		"Page":        "couriers",
	})
}

func (s *Server) handleCouriersAction(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/couriers?msg="+url.QueryEscape("Bad form: "+err.Error()), http.StatusSeeOther)
		return
	}

	var result string

	switch r.FormValue("action") {
	case "complete":
		missionID := espFormInt(r, "missionID")
		row, err := evedb.GetCourierByID(ctx, s.db, missionID)
		if err != nil || row == nil {
			result = "Contract not found."
			break
		}
		if row.State != evedb.CourierStateAccepted {
			result = "Contract is not in Accepted state."
			break
		}
		if err := evedb.DeliverCourierMissionDB(ctx, s.db, missionID, row.AcceptorID, row.DropStationID); err != nil {
			result = "Error: " + err.Error()
			break
		}
		result = "Contract force-completed."

	case "fail":
		missionID := espFormInt(r, "missionID")
		if err := evedb.FailCourierMission(ctx, s.db, missionID); err != nil {
			result = "Error: " + err.Error()
			break
		}
		result = "Contract force-failed. Collateral transferred to issuer."

	case "cancel":
		missionID := espFormInt(r, "missionID")
		row, err := evedb.GetCourierByID(ctx, s.db, missionID)
		if err != nil || row == nil {
			result = "Contract not found."
			break
		}
		if err := evedb.CancelCourierMission(ctx, s.db, missionID, row.IssuerID); err != nil {
			result = "Error: " + err.Error()
			break
		}
		result = "Contract force-cancelled. Reward refunded to issuer."

	case "settings":
		// Read and apply settings.
		if v := strings.TrimSpace(r.FormValue("listingFee")); v != "" {
			evedb.SetServerSetting(ctx, s.db, "courier.listingFee", v)
		}
		if v := strings.TrimSpace(r.FormValue("maxDays")); v != "" {
			evedb.SetServerSetting(ctx, s.db, "courier.maxDays", v)
		}
		if v := strings.TrimSpace(r.FormValue("maxVolumeM3")); v != "" {
			evedb.SetServerSetting(ctx, s.db, "courier.maxVolumeM3", v)
		}
		if v := strings.TrimSpace(r.FormValue("acceptMinISKPerJump")); v != "" {
			evedb.SetServerSetting(ctx, s.db, "courier.acceptMinISKPerJump", v)
		}
		result = "Courier settings saved."

	default:
		result = "Unknown action."
	}

	http.Redirect(w, r, "/couriers?msg="+url.QueryEscape(result), http.StatusSeeOther)
}
