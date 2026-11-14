package esp

// Industry → Factories.
//
// The live half of industry: what is installed where, and how long it has left. A line is busy
// exactly when an undelivered ramJobs row points at it, so occupancy derives from the job table.
//
// Rush pulls a job's end time to now and leaves delivery to the in-game path: creating the output
// from here would land in a hangar the connected client has already cached. Cancel and Release
// free a blueprint stuck at flag 111 by a job whose station or line no longer exists.

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

// handleFactories serves /industry/factories (Science & Industry): the overview dashboard,
// grouped Manufacturing and Science lists, the same grouping by installer, and the per-station
// capacity table. The aggregates are in sciindustry.go.
func (s *Server) handleFactories(w http.ResponseWriter, r *http.Request) {
	var result, resultErr string
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err == nil {
			result, resultErr = s.factoryAction(r)
			s.invalidateSciOverview()
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	q := r.URL.Query()
	view := q.Get("view")
	switch view {
	case "manufacturing", "science", "installers", "stations":
	default:
		view = "overview"
	}

	ov, err := s.sciOverview(ctx, q.Get("refresh") == "1")
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "factories"})
		return
	}

	data := map[string]any{
		"Page":      "factories",
		"Result":    result,
		"ResultErr": resultErr,
		"O":         ov,
		"Stats":     ov.Stats,
		"View":      view,
		"Now":       time.Now().Unix(),
		// Seconds until the next job finishes; the stats row has no clock of its own.
		"NextInSec": nextInSeconds(ov.Stats.NextDoneUnix),
		"Query":     strings.TrimSpace(q.Get("q")),
		"RegionID":  parseInt64(q.Get("region")),
		"Activity":  parseInt64(q.Get("activity")),
		"Status":    sciStatus(q.Get("status")),
		"BusyOnly":  q.Get("busy") == "1",
	}

	switch view {
	case "stations":
		regions, _ := evedb.ESPListFactoryRegions(ctx, s.db)
		rows, err := evedb.ESPListFactoryStations(ctx, s.db, evedb.ESPStationFilter{
			Search:   strings.TrimSpace(q.Get("q")),
			RegionID: parseInt64(q.Get("region")),
			BusyOnly: q.Get("busy") == "1",
		})
		if err != nil {
			s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "factories"})
			return
		}
		data["Regions"] = regions
		data["Stations"] = rows
	case "manufacturing", "science", "installers":
		if err := s.fillGroupView(ctx, r, view, data); err != nil {
			s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "factories"})
			return
		}
	}

	s.render(w, "factories.html", data)
}

// factoryAction handles the job and line writes from both factory pages.
func (s *Server) factoryAction(r *http.Request) (result, resultErr string) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	jobID := parseInt64(r.FormValue("jobID"))

	switch r.FormValue("action") {
	case "rush":
		if err := evedb.ESPRushJob(ctx, s.db, jobID); err != nil {
			return "", "Error: " + err.Error()
		}
		return "Job " + strconv.FormatInt(jobID, 10) +
			" is ready to deliver. The installer collects it in-game as normal.", ""

	case "rushGroup":
		n, err := evedb.ESPRushGroup(ctx, s.db, groupBaseFilter(r, r.FormValue("view")),
			r.FormValue("by"), parseInt64(r.FormValue("id")), parseInt64(r.FormValue("act")))
		if err != nil {
			return "", "Error: " + err.Error()
		}
		return plural(n, "running job") +
			" pulled to now. The installers collect them in-game as normal.", ""

	case "nudge":
		delta := parseInt64(r.FormValue("deltaSec"))
		if delta == 0 {
			return "", "Nothing to change — the offset was zero."
		}
		if err := evedb.ESPSetJobEndTime(ctx, s.db, jobID, delta); err != nil {
			return "", "Error: " + err.Error()
		}
		verb := "later"
		if delta < 0 {
			verb = "earlier"
			delta = -delta
		}
		return "Job " + strconv.FormatInt(jobID, 10) + " now finishes " +
			formatSeconds(delta) + " " + verb + ".", ""

	case "cancel":
		if err := evedb.ESPCancelJob(ctx, s.db, jobID); err != nil {
			return "", "Error: " + err.Error()
		}
		return "Job " + strconv.FormatInt(jobID, 10) +
			" aborted. The blueprint is back in the installer's hangar; consumed materials are not refunded.", ""

	case "release":
		if err := evedb.ESPReleaseJob(ctx, s.db, jobID); err != nil {
			return "", "Error: " + err.Error()
		}
		return "Job " + strconv.FormatInt(jobID, 10) +
			" deleted and its blueprint freed back to the hangar.", ""

	case "saveLine":
		lineID := parseInt64(r.FormValue("assemblyLineID"))
		if err := evedb.ESPUpdateAssemblyLine(ctx, s.db, lineID,
			parseFloat64(r.FormValue("costInstall")),
			parseFloat64(r.FormValue("costPerHour")),
			parseFloat64(r.FormValue("minimumStanding"))); err != nil {
			return "", "Error: " + err.Error()
		}
		return "Assembly line " + strconv.FormatInt(lineID, 10) + " saved.", ""

	case "stationCosts":
		stationID := parseInt64(r.FormValue("stationID"))
		activity := parseInt64(r.FormValue("activityID"))
		n, err := evedb.ESPSetStationLineCosts(ctx, s.db, stationID, activity,
			parseFloat64(r.FormValue("costInstall")), parseFloat64(r.FormValue("costPerHour")))
		if err != nil {
			return "", "Error: " + err.Error()
		}
		scope := "every line"
		if activity != 0 {
			scope = activityLabel(activity) + " lines"
		}
		return "Applied to " + strconv.FormatInt(n, 10) + " of " + scope + " at this station.", ""
	}
	return "", ""
}

// handleFactoryStation serves /industry/factory/<stationID>: one station's assembly lines with
// the job sitting on each.
func (s *Server) handleFactoryStation(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 4 {
		http.NotFound(w, r)
		return
	}
	stationID, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	var result, resultErr string
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err == nil {
			result, resultErr = s.factoryAction(r)
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	header, err := evedb.ESPGetFactoryStation(ctx, s.db, stationID)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "factories"})
		return
	}
	lines, err := evedb.ESPListAssemblyLines(ctx, s.db, stationID)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "factories"})
		return
	}
	jobs, err := evedb.ESPListJobs(ctx, s.db, evedb.ESPJobFilter{Status: "active", StationID: stationID})
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "factories"})
		return
	}

	var busy int64
	for _, l := range lines {
		if l.Busy() {
			busy++
		}
	}

	s.render(w, "factory_station.html", map[string]any{
		"Page":      "factories",
		"Result":    result,
		"ResultErr": resultErr,
		"Station":   header,
		"Lines":     lines,
		"Jobs":      jobs,
		"BusyLines": busy,
		"JobActs":   blueprintActivityChoices(),
		"Now":       time.Now().Unix(),
	})
}

// nextInSeconds is the wait until a Unix timestamp, floored at zero. A next
// finish already in the past means the job ticker has not swept, which the
// overdue banner covers; the countdown just reads "now".
func nextInSeconds(unix int64) int64 {
	if unix <= 0 {
		return 0
	}
	if d := unix - time.Now().Unix(); d > 0 {
		return d
	}
	return 0
}

// formatSeconds renders a duration in seconds as "2h 15m" / "45s".
func formatSeconds(sec int64) string {
	neg := sec < 0
	if neg {
		sec = -sec
	}
	var out string
	switch {
	case sec >= 86400:
		out = strconv.FormatInt(sec/86400, 10) + "d " + strconv.FormatInt((sec%86400)/3600, 10) + "h"
	case sec >= 3600:
		out = strconv.FormatInt(sec/3600, 10) + "h " + strconv.FormatInt((sec%3600)/60, 10) + "m"
	case sec >= 60:
		out = strconv.FormatInt(sec/60, 10) + "m " + strconv.FormatInt(sec%60, 10) + "s"
	default:
		out = strconv.FormatInt(sec, 10) + "s"
	}
	if neg {
		return "−" + out
	}
	return out
}
