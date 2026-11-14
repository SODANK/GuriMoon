package esp

// Solar-system helpers shared by the system-picking pages (Asteroid Belts,
// Moons): the security band label and the name autocomplete behind their
// search boxes.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	evedb "gurimoon/db"
)

// secBand returns a label + css color for a security value.
func secBand(sec float64) (string, string) {
	switch {
	case sec >= 0.5:
		return "High-sec", "#6ee7b7"
	case sec >= 0.0:
		return "Low-sec", "#fbbf24"
	default:
		return "Null-sec", "#f87171"
	}
}

// handleSystemsAPI serves JSON system-name autocomplete for system search boxes.
func (s *Server) handleSystemsAPI(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) < 2 {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	matches, err := evedb.ESPSearchSolarSystems(ctx, s.db, q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type apiSys struct {
		ID       int64   `json:"id"`
		Name     string  `json:"name"`
		Security float64 `json:"security"`
		Region   string  `json:"region"`
	}
	out := make([]apiSys, len(matches))
	for i, m := range matches {
		out[i] = apiSys{m.SolarSystemID, m.SystemName, m.Security, m.RegionName}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}
