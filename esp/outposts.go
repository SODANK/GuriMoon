package esp

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	evedb "gurimoon/db"
)

// Outposts / Conquerable panel: the player-built stations, the construction platforms still on
// their way to becoming one, and the NPC-built conquerable stations of nullsec.
//
// Force build and Remove exist because a platform can strand: its only in-game exit is the Build
// menu, which needs a pilot of the owning corp in the system. Force build runs the client's Build
// path without the role, skill and password gates; the sovereignty and anchoring-delay checks
// still apply, since skipping them would mint a station the rest of the world disagrees with.

// PlatformForceBuilder is set by the game server. The panel calls it to complete a platform
// through the live path (create the station, drop caches, swap balls). Nil in a panel-only build,
// where Force Build is refused.
var PlatformForceBuilder func(platformID int64) (stationID int64, err error)

// PlatformRemover is set by the game server to delete an anchored platform and
// its ball.  Nil outside the game server.
var PlatformRemover func(platformID int64) error

func (s *Server) handleOutposts(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if r.Method == http.MethodPost {
		s.handleOutpostPost(w, r, ctx)
		return
	}

	platforms, err := evedb.ListOutpostPlatforms(ctx, s.db)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "outposts"})
		return
	}
	outposts, err := evedb.ListOutposts(ctx, s.db)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "outposts"})
		return
	}
	conquerables, err := evedb.ListConquerableStations(ctx, s.db)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "outposts"})
		return
	}

	// Remaining anchor time is the stored timestamp against the type's anchoringDelay attribute.
	type platformRow struct {
		evedb.OutpostPlatform
		Remaining string
		Ready     bool
	}
	rows := make([]platformRow, 0, len(platforms))
	ready := 0
	for _, p := range platforms {
		row := platformRow{OutpostPlatform: p, Ready: true}
		if delayMs, ok, err := evedb.GetTypeAttributeFloat(ctx, s.db, p.TypeID, "anchoringDelay"); err == nil && ok {
			if left := anchorRemaining(p.AnchoredAt, delayMs); left > 0 {
				row.Remaining = left.Round(time.Second).String()
				row.Ready = false
			}
		}
		if row.Ready {
			ready++
		}
		rows = append(rows, row)
	}

	s.render(w, "outposts.html", map[string]any{
		"Platforms":     rows,
		"Outposts":      outposts,
		"Conquerables":  conquerables,
		"ReadyCount":    ready,
		"CanForceBuild": PlatformForceBuilder != nil,
		"Msg":           r.URL.Query().Get("msg"),
		"Err":           r.URL.Query().Get("err"),
		"Page":          "outposts",
	})
}

// anchorRemaining mirrors the game server's own check so the panel and the
// Build RPC never disagree about whether a platform is ready.  anchoredAt is a
// Win32 FILETIME (100-ns ticks since 1601); delayMs is the dogma attribute.
func anchorRemaining(anchoredAt uint64, delayMs float64) time.Duration {
	if anchoredAt == 0 {
		return 0
	}
	now := uint64(time.Now().UnixNano()/100) + 116444736000000000
	if now <= anchoredAt {
		return time.Duration(delayMs) * time.Millisecond
	}
	elapsed := time.Duration(now-anchoredAt) * 100 * time.Nanosecond
	total := time.Duration(delayMs) * time.Millisecond
	if elapsed >= total {
		return 0
	}
	return total - elapsed
}

func (s *Server) handleOutpostPost(w http.ResponseWriter, r *http.Request, ctx context.Context) {
	redirect := func(msg, errMsg string) {
		q := url.Values{}
		if msg != "" {
			q.Set("msg", msg)
		}
		if errMsg != "" {
			q.Set("err", errMsg)
		}
		http.Redirect(w, r, "/outposts?"+q.Encode(), http.StatusSeeOther)
	}
	if err := r.ParseForm(); err != nil {
		redirect("", err.Error())
		return
	}
	platformID, _ := strconv.ParseInt(r.FormValue("platformID"), 10, 64)

	switch r.FormValue("action") {
	case "forcebuild":
		if PlatformForceBuilder == nil {
			redirect("", "force build is only available on the running game server")
			return
		}
		stationID, err := PlatformForceBuilder(platformID)
		if err != nil {
			redirect("", err.Error())
			return
		}
		redirect(fmt.Sprintf("Outpost %d built from platform %d.", stationID, platformID), "")

	case "remove":
		if PlatformRemover == nil {
			redirect("", "removing a platform is only available on the running game server")
			return
		}
		if err := PlatformRemover(platformID); err != nil {
			redirect("", err.Error())
			return
		}
		redirect(fmt.Sprintf("Platform %d removed.", platformID), "")

	case "transfer":
		stationID, _ := strconv.ParseInt(r.FormValue("stationID"), 10, 64)
		corpID, _ := strconv.ParseInt(r.FormValue("corporationID"), 10, 64)
		if stationID == 0 || corpID == 0 {
			redirect("", "pick a station and a corporation")
			return
		}
		if err := evedb.UpdateStationOwner(ctx, s.db, stationID, corpID); err != nil {
			redirect("", err.Error())
			return
		}
		// Owner names reach the lobby and Show Info through caches built at the last station-set
		// rebuild, and the owner name bulk falls back to staStations.
		invalidateStationServices()
		if OwnerNamesInvalidator != nil {
			OwnerNamesInvalidator()
		}
		if StationOwnerChangedHook != nil {
			StationOwnerChangedHook(stationID, corpID)
		}
		redirect(fmt.Sprintf("Station %d transferred to corporation %d.", stationID, corpID), "")

	default:
		redirect("", "unknown action")
	}
}
