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

// StationServicesInvalidator is set by the game server. It drops the cached
// map.GetStationExtraInfo tuple, which carries the operationID → serviceID mapping the client
// joins itself (S&I install gate, star map service overlay). See db/espstationservicesdb.go.
var StationServicesInvalidator func()

func invalidateStationServices() {
	if StationServicesInvalidator != nil {
		StationServicesInvalidator()
	}
}

// handleStationView renders the station detail page and handles edits.
func (s *Server) handleStationView(w http.ResponseWriter, r *http.Request) {
	stationID := lootPathID(r.URL.Path, "/station/")
	if stationID == 0 {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodPost {
		s.handleStationPost(w, r, stationID)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	fail := func(err error) {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "stations"})
	}

	detail, err := evedb.ESPGetStationDetail(ctx, s.db, stationID)
	if err != nil {
		fail(err)
		return
	}
	if detail == nil {
		http.NotFound(w, r)
		return
	}

	services, err := evedb.ESPGetStationServices(ctx, s.db, detail.OperationID)
	if err != nil {
		fail(err)
		return
	}
	serviceOptions, err := evedb.ESPGetStationServiceOptions(ctx, s.db, detail.OperationID)
	if err != nil {
		fail(err)
		return
	}
	opInfo, err := evedb.ESPGetStationOperationInfo(ctx, s.db, stationID)
	if err != nil {
		fail(err)
		return
	}
	operations, err := evedb.ESPListStationOperations(ctx, s.db)
	if err != nil {
		fail(err)
		return
	}
	corps, err := evedb.ESPGetAllNPCCorps(ctx, s.db)
	if err != nil {
		fail(err)
		return
	}
	// A conquered station can be owned by a player corp outside the NPC dropdown; keep the current
	// owner selectable.
	haveOwner := false
	for _, c := range corps {
		if c.CorporationID == detail.CorporationID {
			haveOwner = true
			break
		}
	}
	if !haveOwner {
		name := detail.CorpName
		if name == "" {
			name = "Corp " + strconv.FormatInt(detail.CorporationID, 10)
		}
		corps = append([]evedb.ESPNPCCorpOption{{CorporationID: detail.CorporationID, CorpName: name + " (current)"}}, corps...)
	}
	agents, err := evedb.ESPSearchAgents(ctx, s.db, 0, 0, stationID)
	if err != nil {
		fail(err)
		return
	}
	docked, err := evedb.ESPGetStationDockedChars(ctx, s.db, stationID)
	if err != nil {
		fail(err)
		return
	}
	for i := range docked {
		docked[i].Online = s.sessions.FindByCharID(docked[i].CharacterID) != nil
	}
	marketStats, err := evedb.ESPGetStationMarketStats(ctx, s.db, stationID)
	if err != nil {
		fail(err)
		return
	}
	orders, err := evedb.ESPGetStationOrders(ctx, s.db, stationID, 30)
	if err != nil {
		fail(err)
		return
	}
	assetOwners, err := evedb.ESPGetStationAssetOwners(ctx, s.db, stationID, 20)
	if err != nil {
		fail(err)
		return
	}
	stationTypes, err := evedb.ESPListStationTypes(ctx, s.db)
	if err != nil {
		fail(err)
		return
	}
	// Only this layer knows the converted-model index, so the "no .glb" mark
	// and the current model's own labels are filled in here.
	var current evedb.ESPStationTypeOption
	for i := range stationTypes {
		stationTypes[i].HasModel = s.resolveGLB(stationTypes[i].URL3D) != ""
		if stationTypes[i].StationTypeID == detail.StationTypeID {
			current = stationTypes[i]
		}
	}

	s.render(w, "station_view.html", map[string]any{
		"Page":         "stations",
		"D":            detail,
		"Services":     services,
		"ServiceOpts":  serviceOptions,
		"OpInfo":       opInfo,
		"Operations":   operations,
		"Corps":        corps,
		"Agents":       agents,
		"Docked":       docked,
		"MarketStats":  marketStats,
		"Orders":       orders,
		"AssetOwners":  assetOwners,
		"StationTypes": stationTypes,
		"CurrentType":  current,
		"HasModels":    len(s.modelsDirs) > 0,
		"Msg":          r.URL.Query().Get("msg"),
		"Err":          r.URL.Query().Get("err"),
	})
}

// handleStationPost applies the edit form and redirects back to the page.
func (s *Server) handleStationPost(w http.ResponseWriter, r *http.Request, stationID int64) {
	back := "/station/" + strconv.FormatInt(stationID, 10)
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, back+"?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if r.FormValue("action") == "model" {
		typeID, _ := strconv.ParseInt(strings.TrimSpace(r.FormValue("stationTypeID")), 10, 64)
		if typeID <= 0 {
			http.Redirect(w, r, back+"?err="+url.QueryEscape("pick a station model"), http.StatusSeeOther)
			return
		}
		if err := evedb.ESPSetStationType(ctx, s.db, stationID, typeID); err != nil {
			http.Redirect(w, r, back+"?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
			return
		}
		msg := "Station model changed. The hull, the docking bay and the undock point " +
			"all come from the type — clients already in this system keep the old model " +
			"until the system reloads."
		http.Redirect(w, r, back+"?msg="+url.QueryEscape(msg), http.StatusSeeOther)
		return
	}

	if r.FormValue("action") == "services" {
		s.handleStationServicesPost(w, r, ctx, stationID, back)
		return
	}

	num := func(key string) float64 {
		v, _ := strconv.ParseFloat(strings.TrimSpace(r.FormValue(key)), 64)
		return v
	}
	// Moving a station off a custom operation with the dropdown can orphan it;
	// the clone exists only to carry that one station's service set.
	oldOp, _ := evedb.ESPGetStationOperationInfo(ctx, s.db, stationID)
	upd := evedb.ESPStationUpdate{
		StationName:      strings.TrimSpace(r.FormValue("stationName")),
		CorporationID:    int64(num("corporationID")),
		OperationID:      int64(num("operationID")),
		Security:         int64(num("security")),
		DockingCost:      num("dockingCost"),
		MaxShipVolume:    int64(num("maxShipVolume")),
		OfficeRental:     int64(num("officeRental")),
		ReprocEfficiency: num("reprocEfficiency"),
		ReprocTake:       num("reprocTake"),
	}
	// Reprocessing values arrive as percentages (50 = 50%).
	upd.ReprocEfficiency /= 100
	upd.ReprocTake /= 100

	if err := evedb.ESPUpdateStation(ctx, s.db, stationID, upd); err != nil {
		http.Redirect(w, r, back+"?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	if oldOp.Custom && oldOp.OperationID != upd.OperationID {
		_ = evedb.ESPDeleteOrphanCustomOperation(ctx, s.db, oldOp.OperationID)
	}
	if oldOp.OperationID != upd.OperationID {
		invalidateStationServices()
	}
	http.Redirect(w, r, back+"?msg="+url.QueryEscape("Station saved."), http.StatusSeeOther)
}

// handleStationServicesPost applies the service checkbox grid. A station's services belong to its
// operation, which hundreds of stations share, so a diverging set moves the station onto its own
// cloned operation. See db/espstationservicesdb.go.
func (s *Server) handleStationServicesPost(w http.ResponseWriter, r *http.Request, ctx context.Context, stationID int64, back string) {
	var ids []int64
	for _, v := range r.Form["serviceID"] {
		id, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			http.Redirect(w, r, back+"?err="+url.QueryEscape("bad serviceID "+v), http.StatusSeeOther)
			return
		}
		ids = append(ids, id)
	}

	opID, cloned, err := evedb.ESPSetStationServices(ctx, s.db, stationID, ids)
	if err != nil {
		http.Redirect(w, r, back+"?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	invalidateStationServices()

	msg := "Services saved"
	if cloned {
		msg += " — this station was moved onto its own operation (" +
			strconv.FormatInt(opID, 10) + ") so the change does not reach every other " +
			"station that shared the old one"
	}
	msg += ". A pilot already docked keeps the old service buttons until they " +
		"undock and dock again — the client reads the mask once, at dock."
	http.Redirect(w, r, back+"?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}
