package esp

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
)

// CustomCelestialRemoved is set by the game server to take a deleted Dungeon Entrance's live
// dungeon out of the world (cmd/gurimoon/fixedsites.go). nil when the panel runs without one.
var CustomCelestialRemoved func(itemID int64)

// metersPerAU converts the seed form's AU distances to map coordinates.
const metersPerAU = 149_597_870_700.0

// celestialGroupNames maps mapDenormalize groupIDs to display labels.
var celestialGroupNames = map[int64]string{
	6:   "Sun",
	7:   "Planet",
	8:   "Moon",
	9:   "Asteroid Belt",
	10:  "Stargate",
	15:  "Station",
	310: "Beacon",
}

// seedKinds maps the add-celestial form kind to its target groupID.
var seedKinds = map[string]int64{
	"planet":  7,
	"moon":    8,
	"belt":    9,
	"beacon":  310,
	"dungeon": 310,
}

// seedDefaultRadius is the mapDenormalize radius written when the form leaves
// the radius field blank, per kind.
var seedDefaultRadius = map[string]float64{
	"planet":  3_000_000,
	"moon":    1_000_000,
	"belt":    25_000,
	"beacon":  5_000,
	"dungeon": 5_000,
}

// solarCelestialView is one row of the detail page's celestial table.
type solarCelestialView struct {
	ItemID      int64
	Name        string
	TypeID      int64
	GroupID     int64
	GroupName   string
	OrbitID     int64
	OrbitName   string
	DistAU      float64
	RadiusKM    int64
	IsCustom    bool
	DungeonVID  int64
	DungeonName string
}

// solarMapCel is the JSON shape consumed by the radial map script.
type solarMapCel struct {
	ItemID     int64   `json:"id"`
	Name       string  `json:"name"`
	GroupID    int64   `json:"group"`
	TypeID     int64   `json:"type"`
	X          float64 `json:"x"`
	Y          float64 `json:"y"`
	Z          float64 `json:"z"`
	Radius     float64 `json:"radius"`
	OrbitID    int64   `json:"orbit"`
	Custom     bool    `json:"custom"`
	DungeonVID int64   `json:"dungeon"`
}

// handleSolarSystems renders the search/list page.
func (s *Server) handleSolarSystems(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	query := strings.TrimSpace(r.URL.Query().Get("q"))
	var results []evedb.ESPSystemSearchRow
	if query != "" {
		var err error
		results, err = evedb.ESPSearchSolarSystems(ctx, s.db, query)
		if err != nil {
			s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "solarsystems"})
			return
		}
	}

	s.render(w, "solarsystems.html", map[string]any{
		"Page":    "solarsystems",
		"Query":   query,
		"Results": results,
		"Msg":     r.URL.Query().Get("msg"),
		"Err":     r.URL.Query().Get("err"),
	})
}

// handleSolarSystemsAPI is the autocomplete JSON endpoint.
func (s *Server) handleSolarSystemsAPI(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	w.Header().Set("Content-Type", "application/json")
	if len(q) < 2 {
		w.Write([]byte("[]"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	hits, err := evedb.ESPSearchSovSystems(ctx, s.db, q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if hits == nil {
		hits = []evedb.SovSystemHit{}
	}
	json.NewEncoder(w).Encode(hits)
}

// handleSolarSystemView renders one system's detail page and handles the
// seed/delete POST actions.
func (s *Server) handleSolarSystemView(w http.ResponseWriter, r *http.Request) {
	systemID := lootPathID(r.URL.Path, "/solarsystem/")
	if systemID == 0 {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodPost {
		s.handleSolarSystemPost(w, r, systemID)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	fail := func(err error) {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "solarsystems"})
	}

	info, err := evedb.ESPGetSolarSystemInfo(ctx, s.db, systemID)
	if err != nil {
		fail(err)
		return
	}
	if info == nil {
		http.NotFound(w, r)
		return
	}

	cels, err := evedb.GetSystemCelestials(ctx, s.db, systemID)
	if err != nil {
		fail(err)
		return
	}
	custom, err := evedb.GetCustomCelestials(ctx, s.db, systemID)
	if err != nil {
		fail(err)
		return
	}
	seedTypes, err := evedb.ListCelestialSeedTypes(ctx, s.db)
	if err != nil {
		fail(err)
		return
	}
	dungeons, err := evedb.ListDungeonOptions(ctx, s.db)
	if err != nil {
		fail(err)
		return
	}

	// Star anchors distances; systems without one fall back to the origin.
	var starX, starY, starZ float64
	for _, c := range cels {
		if c.GroupID == 6 {
			starX, starY, starZ = c.X, c.Y, c.Z
			break
		}
	}

	nameByID := make(map[int64]string, len(cels))
	for _, c := range cels {
		nameByID[c.ItemID] = c.Name
	}

	views := make([]solarCelestialView, 0, len(cels))
	mapCels := make([]solarMapCel, 0, len(cels))
	for _, c := range cels {
		gname := celestialGroupNames[c.GroupID]
		if gname == "" {
			gname = fmt.Sprintf("Group %d", c.GroupID)
		}
		cc := custom[c.ItemID]
		dist := math.Sqrt((c.X-starX)*(c.X-starX) + (c.Y-starY)*(c.Y-starY) + (c.Z-starZ)*(c.Z-starZ))
		views = append(views, solarCelestialView{
			ItemID:      c.ItemID,
			Name:        c.Name,
			TypeID:      c.TypeID,
			GroupID:     c.GroupID,
			GroupName:   gname,
			OrbitID:     c.OrbitID,
			OrbitName:   nameByID[c.OrbitID],
			DistAU:      dist / metersPerAU,
			RadiusKM:    int64(c.Radius / 1000),
			IsCustom:    cc.ItemID != 0,
			DungeonVID:  cc.DungeonVID,
			DungeonName: cc.DungeonName,
		})
		mapCels = append(mapCels, solarMapCel{
			ItemID:     c.ItemID,
			Name:       c.Name,
			GroupID:    c.GroupID,
			TypeID:     c.TypeID,
			X:          c.X,
			Y:          c.Y,
			Z:          c.Z,
			Radius:     c.Radius,
			OrbitID:    c.OrbitID,
			Custom:     cc.ItemID != 0,
			DungeonVID: cc.DungeonVID,
		})
	}
	sort.Slice(views, func(i, j int) bool { return views[i].DistAU < views[j].DistAU })

	celJSON, err := json.Marshal(mapCels)
	if err != nil {
		fail(err)
		return
	}

	// Orbit-parent choices: star, planets and moons only.
	type parentOption struct {
		ItemID int64
		Name   string
		Group  string
	}
	var parents []parentOption
	for _, v := range views {
		if v.GroupID == 6 || v.GroupID == 7 || v.GroupID == 8 {
			parents = append(parents, parentOption{ItemID: v.ItemID, Name: v.Name, Group: v.GroupName})
		}
	}

	players := s.sessions.FindBySystemID(systemID)
	type playerView struct {
		CharID   int64
		CharName string
	}
	var playerViews []playerView
	for _, p := range players {
		playerViews = append(playerViews, playerView{CharID: p.CharID, CharName: p.CharName})
	}

	s.render(w, "solarsystem_view.html", map[string]any{
		"Page":       "solarsystems",
		"Info":       info,
		"Celestials": views,
		"CelJSON":    template.JS(celJSON),
		"Parents":    parents,
		"SeedTypes":  seedTypes,
		"Dungeons":   dungeons,
		"Players":    playerViews,
		"Msg":        r.URL.Query().Get("msg"),
		"Err":        r.URL.Query().Get("err"),
	})
}

// handleSolarSystemPost dispatches the detail page's POST actions.
func (s *Server) handleSolarSystemPost(w http.ResponseWriter, r *http.Request, systemID int64) {
	redirect := fmt.Sprintf("/solarsystem/%d", systemID)
	fail := func(msg string) {
		http.Redirect(w, r, redirect+"?err="+url.QueryEscape(msg), http.StatusSeeOther)
	}
	ok := func(msg string) {
		http.Redirect(w, r, redirect+"?msg="+url.QueryEscape(msg), http.StatusSeeOther)
	}
	if err := r.ParseForm(); err != nil {
		fail("bad form: " + err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	switch r.FormValue("action") {
	case "addCelestial":
		s.addCustomCelestial(ctx, w, r, systemID, fail, ok)
	case "deleteCelestial":
		itemID, _ := strconv.ParseInt(r.FormValue("itemID"), 10, 64)
		if itemID == 0 {
			fail("missing itemID")
			return
		}
		if err := evedb.DeleteCustomCelestial(ctx, s.db, itemID); err != nil {
			fail(err.Error())
			return
		}
		if CustomCelestialRemoved != nil {
			CustomCelestialRemoved(itemID)
		}
		ok(fmt.Sprintf("Deleted custom celestial %d. A dungeon behind it is removed now; the beacon goes when the system next loads.", itemID))
	default:
		fail("unknown action")
	}
}

// addCustomCelestial validates the seed form and inserts the new celestial.
func (s *Server) addCustomCelestial(ctx context.Context, w http.ResponseWriter, r *http.Request,
	systemID int64, fail func(string), ok func(string)) {

	kind := r.FormValue("kind")
	groupID, valid := seedKinds[kind]
	if !valid {
		fail("unknown celestial kind")
		return
	}

	typeID, _ := strconv.ParseInt(r.FormValue("typeID"), 10, 64)
	if typeID == 0 {
		fail("pick a type")
		return
	}
	// The type must belong to the kind's group, which keeps the client rendering sane.
	seedTypes, err := evedb.ListCelestialSeedTypes(ctx, s.db)
	if err != nil {
		fail(err.Error())
		return
	}
	var typeName string
	for _, t := range seedTypes {
		if t.TypeID == typeID && t.GroupID == groupID {
			typeName = t.TypeName
			break
		}
	}
	if typeName == "" {
		fail(fmt.Sprintf("typeID %d is not valid for kind %s", typeID, kind))
		return
	}

	info, err := evedb.ESPGetSolarSystemInfo(ctx, s.db, systemID)
	if err != nil || info == nil {
		fail("system lookup failed")
		return
	}
	cels, err := evedb.GetSystemCelestials(ctx, s.db, systemID)
	if err != nil {
		fail(err.Error())
		return
	}

	// Resolve the orbit parent: explicit selection, else the system's star.
	parentID, _ := strconv.ParseInt(r.FormValue("parentID"), 10, 64)
	var parent *evedb.CelestialEntity
	for i := range cels {
		if parentID != 0 && cels[i].ItemID == parentID {
			parent = &cels[i]
			break
		}
		if parentID == 0 && cels[i].GroupID == 6 {
			parent = &cels[i]
			break
		}
	}
	if parent == nil {
		fail("orbit parent not found in this system")
		return
	}

	dist, _ := strconv.ParseFloat(strings.TrimSpace(r.FormValue("dist")), 64)
	if dist <= 0 {
		fail("distance must be > 0")
		return
	}
	switch r.FormValue("distUnit") {
	case "au":
		dist *= metersPerAU
	case "km":
		dist *= 1000
	default:
		fail("unknown distance unit")
		return
	}

	var angle float64
	if a := strings.TrimSpace(r.FormValue("angle")); a != "" {
		deg, err := strconv.ParseFloat(a, 64)
		if err != nil {
			fail("bad angle")
			return
		}
		angle = deg * math.Pi / 180
	} else {
		angle = rand.Float64() * 2 * math.Pi
	}

	yOffsetKM, _ := strconv.ParseFloat(strings.TrimSpace(r.FormValue("yOffset")), 64)

	radius := seedDefaultRadius[kind]
	if rv := strings.TrimSpace(r.FormValue("radius")); rv != "" {
		pr, err := strconv.ParseFloat(rv, 64)
		if err != nil || pr <= 0 {
			fail("bad radius")
			return
		}
		radius = pr
	}

	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		name = fmt.Sprintf("%s - %s", info.SystemName, typeName)
	}

	var dungeonVID int64
	roomIdx := 1
	if kind == "dungeon" {
		dungeonVID, _ = strconv.ParseInt(r.FormValue("dungeonVID"), 10, 64)
		if dungeonVID == 0 {
			fail("pick a dungeon")
			return
		}
		if ri, err := strconv.Atoi(r.FormValue("roomIdx")); err == nil && ri >= 1 {
			roomIdx = ri
		}
	}

	seed := evedb.CustomCelestialSeed{
		SolarSystemID:   systemID,
		ConstellationID: info.ConstellationID,
		RegionID:        info.RegionID,
		TypeID:          typeID,
		GroupID:         groupID,
		OrbitID:         parent.ItemID,
		X:               parent.X + dist*math.Cos(angle),
		Y:               parent.Y + yOffsetKM*1000,
		Z:               parent.Z + dist*math.Sin(angle),
		Radius:          radius,
		Name:            name,
		Security:        info.Security,
		DungeonVID:      dungeonVID,
		DungeonRoomIdx:  roomIdx,
	}
	itemID, err := evedb.InsertCustomCelestial(ctx, s.db, seed)
	if err != nil {
		fail(err.Error())
		return
	}

	note := ""
	if n := len(s.sessions.FindBySystemID(systemID)); n > 0 {
		note = fmt.Sprintf(" System is currently loaded (%d player(s) inside) — the object appears once the system reloads.", n)
	}
	ok(fmt.Sprintf("Seeded %q (itemID %d).%s", name, itemID, note))
}
