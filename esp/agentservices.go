package esp

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

// ── Agents → Services ────────────────────────────────────────────────────────
//
// The admin surface for agent side-services: who offers what (the by-division
// distribution grid plus per-agent overrides), what it costs (fee settings),
// every authorable dialogue line with the choices that lead in and out of it,
// and the locator job log.

// LocatorDelayMinutes mirrors config.json agents.locatorDelayMinutes for display; it is the one
// services knob that is not a server setting. Set from main() at boot.
var LocatorDelayMinutes = 3

// espServiceMeta is one service column in the grid.
type espServiceMeta struct {
	Key  string
	Name string
	Stub bool
}

// espServiceCell is one division×service cell.
type espServiceCell struct {
	ServiceKey string
	Enabled    bool
	MinLevel   int
}

// espServiceDivRow is one division row in the grid.
type espServiceDivRow struct {
	DivisionID   int64
	DivisionName string
	Agents       int
	Cells        []espServiceCell
}

func (s *Server) handleAgentServices(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	var result string
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err == nil {
			result = s.agentServicesPost(ctx, r)
		}
	}
	s.renderAgentServices(ctx, w, r, result)
}

// agentServicesPost handles the page's POST actions; all land back on the
// same render.
func (s *Server) agentServicesPost(ctx context.Context, r *http.Request) string {
	switch r.FormValue("action") {
	case "settings":
		feeBase, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("locatorFeeBase")))
		per01, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("secRepairPer01")))
		if feeBase < 0 || per01 < 0 {
			return "Error: fees cannot be negative."
		}
		if err := evedb.SetLocatorFeeBase(ctx, s.db, feeBase); err != nil {
			return "Error: " + err.Error()
		}
		if err := evedb.SetSecRepairPer01(ctx, s.db, per01); err != nil {
			return "Error: " + err.Error()
		}
		return fmt.Sprintf("Fees saved: locator base %d ISK (level² multiplier), security repair %d ISK per 0.1.",
			evedb.GetLocatorFeeBase(ctx, s.db), evedb.GetSecRepairPer01(ctx, s.db))

	case "distribution":
		// The whole grid posts at once: an unticked cell is absent from the
		// form, so the save is a full replace, not a merge.
		divs, err := evedb.ESPGetNPCDivisions(ctx, s.db)
		if err != nil {
			return "Error: " + err.Error()
		}
		var grid []evedb.ServiceDivisionRow
		for _, d := range divs {
			for _, key := range evedb.ServiceKeys {
				field := fmt.Sprintf("svc_%s_%d", key, d.DivisionID)
				if r.FormValue(field) == "" {
					continue
				}
				lvl, _ := strconv.Atoi(r.FormValue(fmt.Sprintf("lvl_%s_%d", key, d.DivisionID)))
				if lvl < 1 {
					lvl = 1
				}
				if lvl > 4 {
					lvl = 4
				}
				grid = append(grid, evedb.ServiceDivisionRow{
					ServiceKey: key, DivisionID: d.DivisionID, MinLevel: lvl,
				})
			}
		}
		if err := evedb.ESPReplaceServiceDivisions(ctx, s.db, grid); err != nil {
			return "Error: " + err.Error()
		}
		return fmt.Sprintf("Distribution saved: %d division grants.", len(grid))

	case "override":
		agentID, _ := strconv.ParseInt(strings.TrimSpace(r.FormValue("agentID")), 10, 64)
		key := r.FormValue("serviceKey")
		mode := r.FormValue("mode")
		if agentID <= 0 {
			return "Error: agent ID is required."
		}
		var err error
		switch mode {
		case "on":
			err = evedb.ESPSetServiceOverride(ctx, s.db, agentID, key, true)
		case "off":
			err = evedb.ESPSetServiceOverride(ctx, s.db, agentID, key, false)
		case "clear":
			err = evedb.ESPDeleteServiceOverride(ctx, s.db, agentID, key)
		default:
			return "Error: unknown override mode."
		}
		if err != nil {
			return "Error: " + err.Error()
		}
		if mode == "clear" {
			return fmt.Sprintf("Override cleared: agent %d follows its division again for %s.", agentID, key)
		}
		return fmt.Sprintf("Override saved: agent %d forced %s for %s.", agentID, mode, key)

	}
	return "Error: unknown action."
}

func (s *Server) renderAgentServices(ctx context.Context, w http.ResponseWriter, r *http.Request, result string) {
	divs, err := evedb.ESPGetNPCDivisions(ctx, s.db)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "agent-services"})
		return
	}
	gridRows, err := evedb.ESPListServiceDivisions(ctx, s.db)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "agent-services"})
		return
	}
	granted := map[string]evedb.ServiceDivisionRow{}
	for _, g := range gridRows {
		granted[fmt.Sprintf("%s_%d", g.ServiceKey, g.DivisionID)] = g
	}

	services := []espServiceMeta{
		{evedb.ServiceLocator, evedb.ServiceNames[evedb.ServiceLocator], false},
		{evedb.ServiceSecStatus, evedb.ServiceNames[evedb.ServiceSecStatus], false},
		{evedb.ServiceGifts, evedb.ServiceNames[evedb.ServiceGifts], true},
		{evedb.ServiceRumors, evedb.ServiceNames[evedb.ServiceRumors], true},
	}

	rows := make([]espServiceDivRow, 0, len(divs))
	for _, d := range divs {
		row := espServiceDivRow{DivisionID: d.DivisionID, DivisionName: d.DivisionName, Agents: d.Agents}
		for _, svc := range services {
			cell := espServiceCell{ServiceKey: svc.Key, MinLevel: 1}
			if g, ok := granted[fmt.Sprintf("%s_%d", svc.Key, d.DivisionID)]; ok {
				cell.Enabled = true
				cell.MinLevel = g.MinLevel
			}
			row.Cells = append(row.Cells, cell)
		}
		rows = append(rows, row)
	}

	counts, _ := evedb.ESPServiceAgentCounts(ctx, s.db)
	overrides, _ := evedb.ESPListServiceOverrides(ctx, s.db)
	jobs, _ := evedb.ESPRecentLocatorJobs(ctx, s.db, 50)

	var search []evedb.ESPAgentServiceSearchRow
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q != "" {
		search, err = evedb.ESPAgentServiceSearch(ctx, s.db, q, 30)
		if err != nil {
			result = "Search error: " + err.Error()
		}
	}

	feeBase := evedb.GetLocatorFeeBase(ctx, s.db)
	s.render(w, "agent_services.html", map[string]any{
		"Page":           "agent-services",
		"Result":         result,
		"Services":       services,
		"Rows":           rows,
		"Counts":         counts,
		"Overrides":      overrides,
		"Jobs":           jobs,
		"Query":          q,
		"Search":         search,
		"LocatorFeeBase": feeBase,
		"LocatorFees": []int64{
			evedb.LocatorFee(1, feeBase), evedb.LocatorFee(2, feeBase),
			evedb.LocatorFee(3, feeBase), evedb.LocatorFee(4, feeBase),
		},
		"SecRepairPer01": evedb.GetSecRepairPer01(ctx, s.db),
		"LocatorDelay":   LocatorDelayMinutes,
	})
}
