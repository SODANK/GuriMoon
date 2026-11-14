package esp

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
	"gurimoon/npc"
)

// isHex reports whether s consists only of hexadecimal digits.
func isHex(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return len(s) > 0
}

// DashboardSession is a Session decorated with resolved names; the registry holds only IDs.
type DashboardSession struct {
	Session
	StationName string
	SystemName  string
	CorpName    string
}

// Location returns where the pilot is, preferring the station.
func (d DashboardSession) Location() string {
	switch {
	case d.StationName != "":
		return d.StationName
	case d.StationID != 0:
		return "Station " + strconv.FormatInt(d.StationID, 10)
	case d.SystemName != "":
		return d.SystemName
	case d.SolarSystemID != 0:
		return "System " + strconv.FormatInt(d.SolarSystemID, 10)
	}
	return ""
}

// Docked reports whether the pilot is in a station rather than in space.
func (d DashboardSession) Docked() bool { return d.StationID != 0 }

// handleDashboard renders the landing page: the server overview, or the connected-player table
// on ?view=players. Any other view falls back to the overview.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	view := q.Get("view")
	if view != "players" {
		view = "overview"
	}
	data := map[string]any{
		"Page":   "dashboard",
		"View":   view,
		"Vitals": s.dashVitals(),
	}
	switch view {
	case "players":
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		rows, docked, inSpace, selecting := s.dashboardPlayers(ctx)
		data["Uptime"] = time.Since(s.startTime)
		data["PlayerCount"] = len(rows)
		data["Docked"] = docked
		data["InSpace"] = inSpace
		data["Selecting"] = selecting
		data["Sessions"] = rows
	default:
		data["O"] = s.dashOverview(r.Context(), q.Get("refresh") == "1")
		data["Sites"] = s.dashSitesLive()
	}
	s.render(w, "dashboard.html", data)
}

func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	accounts, err := evedb.ESPListAccounts(ctx, s.db, query)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "accounts"})
		return
	}
	data := map[string]any{
		"Accounts": accounts,
		"Query":    query,
		"Msg":      r.URL.Query().Get("msg"),
		"Page":     "accounts",
	}
	s.render(w, "accounts.html", data)
}

func (s *Server) handleAccountView(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 3 {
		http.NotFound(w, r)
		return
	}
	accountID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		switch r.FormValue("action") {
		case "delete":
			if err := evedb.ESPDeleteAccount(ctx, s.db, accountID); err != nil {
				http.Redirect(w, r, fmt.Sprintf("/account/%d?err=%s", accountID, url.QueryEscape(err.Error())), http.StatusSeeOther)
				return
			}
			http.Redirect(w, r, "/accounts?msg="+url.QueryEscape(fmt.Sprintf("account %d deleted", accountID)), http.StatusSeeOther)
		default:
			http.Redirect(w, r, fmt.Sprintf("/account/%d?err=unknown+action", accountID), http.StatusSeeOther)
		}
		return
	}

	acct, err := evedb.ESPGetAccountDetail(ctx, s.db, accountID)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "accounts"})
		return
	}
	if acct == nil {
		s.render(w, "error.html", map[string]any{
			"Error": fmt.Sprintf("account %d not found", accountID), "Page": "accounts"})
		return
	}

	chars, _ := evedb.ESPGetAccountCharacters(ctx, s.db, accountID)

	s.render(w, "account_view.html", map[string]any{
		"Account": acct,
		"Chars":   chars,
		"Msg":     r.URL.Query().Get("msg"),
		"Err":     r.URL.Query().Get("err"),
		"Page":    "accounts",
	})
}

// charactersPageSize is the server-side page. The filters are deep-linkable and cut the payload;
// a client-side quick filter over the rendered rows handles typing.
const charactersPageSize = 200

func (s *Server) handleCharacterSearch(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	q := r.URL.Query()
	query := strings.TrimSpace(q.Get("q"))
	page := int(espQueryInt(q.Get("page"), 0))
	if page < 0 {
		page = 0
	}

	showBots := q.Get("bots") == "1"
	filter := evedb.ESPCharacterFilter{
		Search:    query,
		CorpID:    espQueryInt(q.Get("corp"), 0),
		StationID: espQueryInt(q.Get("station"), 0),
		ShowBots:  showBots,
		Limit:     charactersPageSize,
		Offset:    page * charactersPageSize,
	}

	chars, err := evedb.ESPListCharacters(ctx, s.db, filter)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "characters"})
		return
	}
	total, _ := evedb.ESPCountCharacters(ctx, s.db, filter)
	pageCount := int((total + charactersPageSize - 1) / charactersPageSize)

	data := map[string]any{
		"Characters":   chars,
		"Query":        query,
		"CorpID":       filter.CorpID,
		"StationID":    filter.StationID,
		"ShowBots":     showBots,
		"Total":        total,
		"PageNum":      page,
		"PageCount":    pageCount,
		"Prev":         espPageQuery(q, page-1),
		"Next":         espPageQuery(q, page+1),
		"HasPortraits": s.portraitsDir != "",
		"Page":         "characters",
	}
	s.render(w, "characters.html", data)
}

// handleCharacterView serves the character profile page: portrait, live skill queue, the
// appearance values and the pilot-level actions (ISK, station, corporation). POSTs redirect back
// to the GET so a refresh never repeats an action.
func (s *Server) handleCharacterView(w http.ResponseWriter, r *http.Request) {
	// URL: /character/12345
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 3 {
		http.NotFound(w, r)
		return
	}
	charID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if r.Method == http.MethodPost {
		s.handleCharacterEdit(w, r, ctx, charID)
		return
	}

	detail, err := evedb.ESPGetCharacterDetail(ctx, s.db, charID)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "characters"})
		return
	}
	skills, _ := evedb.ESPGetCharacterSkills(ctx, s.db, charID)
	totals, _ := evedb.ESPGetCharSkillTotals(ctx, s.db, charID)
	ship, _ := evedb.ESPGetCharacterShip(ctx, s.db, charID)
	itemCount, _ := evedb.ESPGetCharacterItemCount(ctx, s.db, charID)
	corps, _ := evedb.ESPGetAllCorps(ctx, s.db)
	training, queue := skillQueueViewData(ctx, s.db, charID)
	var accountChars []evedb.ESPAccountCharacter
	if detail.AccountID > 0 {
		accountChars, _ = evedb.ESPGetAccountCharacters(ctx, s.db, detail.AccountID)
	}
	journal := charJournalView(ctx, s, charID)

	// Portrait: the appearance numbers the client renders from, the option lists legal for this
	// pilot's bloodline and gender, and which rendered copies the server holds.
	appearance, aerr := evedb.ESPGetCharAppearance(ctx, s.db, charID)
	if aerr != nil {
		log.Printf("esp: character %d appearance: %v", charID, aerr)
	}
	opts := evedb.ESPGetAllAppearanceOptions(ctx, s.db, detail.BloodlineID, detail.Gender)

	created := ""
	if detail.CreateDateTime > 0 {
		created = espFmtTick(detail.CreateDateTime)
	}
	var online *Session
	if s.sessions != nil {
		online = s.sessions.FindByCharID(charID)
	}

	s.render(w, "character_view.html", map[string]any{
		"Char":           detail,
		"Created":        created,
		"Online":         online,
		"Skills":         skills,
		"Totals":         totals,
		"Ship":           ship,
		"ItemCount":      itemCount,
		"Corps":          corps,
		"Training":       training,
		"Queue":          queue,
		"AccountChars":   accountChars,
		"Journal":        journal,
		"HasPortraits":   s.hasPortraitSource(),
		"Appearance":     appearance,
		"AppearanceOpts": opts,
		"Stored":         listPortraits(portraitStoreDir, charID),
		"ClientCached":   listPortraits(s.portraitsDir, charID),
		"StoreDir":       portraitStoreDir,
		"ClientDir":      s.portraitsDir,
		"Self":           fmt.Sprintf("/character/%d", charID),
		"Msg":            r.URL.Query().Get("msg"),
		"Page":           "characters",
	})
}

// espJournalRow is one wallet journal line on the character page, its date already formatted.
type espJournalRow struct {
	Date    string
	RefType string
	Amount  float64
	Balance float64
	Reason  string
}

// charJournalLimit is how many wallet journal rows the character page shows.
const charJournalLimit = 15

// charJournalView reads the character's newest wallet journal rows for the profile page. A read
// error is logged and shows as an empty journal.
func charJournalView(ctx context.Context, s *Server, charID int64) []espJournalRow {
	entries, err := evedb.ESPGetCharacterJournal(ctx, s.db, charID, charJournalLimit)
	if err != nil {
		log.Printf("esp: character %d journal: %v", charID, err)
		return nil
	}
	rows := make([]espJournalRow, 0, len(entries))
	for _, e := range entries {
		date := "—"
		if e.TransDate > 0 {
			date = espFmtTick(uint64(e.TransDate))
		}
		rows = append(rows, espJournalRow{
			Date: date, RefType: e.RefType, Amount: e.Amount, Balance: e.Balance, Reason: e.Reason,
		})
	}
	return rows
}

// espFormFloat reads a float form field, defaulting to 0 on anything unparsable (a blank
// appearance box means zero).
func espFormFloat(r *http.Request, name string) float64 {
	v, _ := strconv.ParseFloat(strings.TrimSpace(r.FormValue(name)), 64)
	return v
}

// espFormInt reads an int64 form field, 0 when absent or unparsable.
func espFormInt(r *http.Request, name string) int64 {
	v, _ := strconv.ParseInt(strings.TrimSpace(r.FormValue(name)), 10, 64)
	return v
}

// handleCharacterEdit applies one POSTed action and redirects back to the profile page with the
// result.
func (s *Server) handleCharacterEdit(w http.ResponseWriter, r *http.Request, ctx context.Context, charID int64) {
	self := fmt.Sprintf("/character/%d", charID)
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, self+"?msg="+url.QueryEscape("Bad form: "+err.Error()), http.StatusSeeOther)
		return
	}
	var result string

	switch r.FormValue("action") {
	case "giveisk":
		amount, err := strconv.ParseFloat(r.FormValue("amount"), 64)
		if err != nil {
			result = "Invalid ISK amount."
			break
		}
		if err := evedb.GiveISK(ctx, s.db, charID, amount); err != nil {
			result = "Error: " + err.Error()
			break
		}
		bal, _ := evedb.GetCharBalance(ctx, s.db, charID)
		_ = evedb.InsertJournalEntry(ctx, s.db, charID, 10, charID, charID, amount, bal, "")
		result = formatISK(amount) + " ISK granted. New balance: " + formatISK(bal) + " ISK."

	case "changecorp":
		newCorpID, err := strconv.ParseInt(r.FormValue("corporationID"), 10, 64)
		if err != nil || newCorpID == 0 {
			result = "Select a corporation."
			break
		}
		if err := evedb.ESPUpdateCharacterCorp(ctx, s.db, charID, newCorpID); err != nil {
			result = "Error: " + err.Error()
			break
		}
		result = "Corporation changed. Relog required."

	case "moveto":
		stationID, err := strconv.ParseInt(r.FormValue("stationID"), 10, 64)
		if err != nil {
			result = "Invalid station ID."
			break
		}
		dest, err := evedb.GetStationLocation(ctx, s.db, stationID)
		if err != nil {
			result = "Error: " + err.Error()
			break
		}
		shipID, _ := evedb.GetCurrentShipID(ctx, s.db, charID)
		if shipID > 0 {
			_ = evedb.UpdateEntityLocation(ctx, s.db, shipID, stationID)
		}
		if err := evedb.UpdateCharacterStation(ctx, s.db, charID, stationID,
			dest.SolarSystemID, dest.ConstellationID, dest.RegionID); err != nil {
			result = "Error: " + err.Error()
			break
		}
		result = "Moved to " + dest.StationName + ". Relog required."

	case "appearance":
		a := &evedb.ESPCharAppearance{
			AccessoryID:  espFormInt(r, "accessoryID"),
			BeardID:      espFormInt(r, "beardID"),
			CostumeID:    espFormInt(r, "costumeID"),
			DecoID:       espFormInt(r, "decoID"),
			EyebrowsID:   espFormInt(r, "eyebrowsID"),
			EyesID:       espFormInt(r, "eyesID"),
			HairID:       espFormInt(r, "hairID"),
			LipstickID:   espFormInt(r, "lipstickID"),
			MakeupID:     espFormInt(r, "makeupID"),
			SkinID:       espFormInt(r, "skinID"),
			BackgroundID: espFormInt(r, "backgroundID"),
			LightID:      espFormInt(r, "lightID"),

			HeadRotation1: espFormFloat(r, "headRotation1"),
			HeadRotation2: espFormFloat(r, "headRotation2"),
			HeadRotation3: espFormFloat(r, "headRotation3"),
			EyeRotation1:  espFormFloat(r, "eyeRotation1"),
			EyeRotation2:  espFormFloat(r, "eyeRotation2"),
			EyeRotation3:  espFormFloat(r, "eyeRotation3"),
			CamPos1:       espFormFloat(r, "camPos1"),
			CamPos2:       espFormFloat(r, "camPos2"),
			CamPos3:       espFormFloat(r, "camPos3"),

			Morph1e: espFormFloat(r, "morph1e"), Morph1n: espFormFloat(r, "morph1n"),
			Morph1s: espFormFloat(r, "morph1s"), Morph1w: espFormFloat(r, "morph1w"),
			Morph2e: espFormFloat(r, "morph2e"), Morph2n: espFormFloat(r, "morph2n"),
			Morph2s: espFormFloat(r, "morph2s"), Morph2w: espFormFloat(r, "morph2w"),
			Morph3e: espFormFloat(r, "morph3e"), Morph3n: espFormFloat(r, "morph3n"),
			Morph3s: espFormFloat(r, "morph3s"), Morph3w: espFormFloat(r, "morph3w"),
			Morph4e: espFormFloat(r, "morph4e"), Morph4n: espFormFloat(r, "morph4n"),
			Morph4s: espFormFloat(r, "morph4s"), Morph4w: espFormFloat(r, "morph4w"),
		}
		if err := evedb.ESPSetCharAppearance(ctx, s.db, charID, a); err != nil {
			result = "Error: " + err.Error()
			break
		}
		result = "Appearance saved. The client renders the face from these numbers, " +
			"so any stored render is now out of date — drop it and hand over a fresh one."

	case "dropportraits":
		n, err := dropStoredPortraits(charID)
		if err != nil {
			result = fmt.Sprintf("Dropped %d stored render(s), then failed: %v", n, err)
			break
		}
		result = fmt.Sprintf("Dropped %d stored render(s) from %s. The client's own cache was not touched.",
			n, portraitStoreDir)

	default:
		result = "Unknown action."
	}

	http.Redirect(w, r, self+"?msg="+url.QueryEscape(result), http.StatusSeeOther)
}

// StationBrowserStats summarises whatever slice of the game the station
// browser is currently showing.
type StationBrowserStats struct {
	Shown       int64
	Total       int64
	Regions     int
	Systems     int
	Pilots      int64 // characters berthed here, online or not
	Online      int64 // sessions connected right now at one of these stations
	Agents      int64
	Orders      int64
	Conquerable int
	Filtered    bool
}

// handleStations renders the station browser. With no filter it lists every
// station in the game in alphabetical order; region, constellation and a name
// search narrow it server-side, and the page filters the rendered rows again
// client-side so typing is instant.
func (s *Server) handleStations(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	regionID, _ := strconv.ParseInt(r.URL.Query().Get("region"), 10, 64)
	constellationID, _ := strconv.ParseInt(r.URL.Query().Get("constellation"), 10, 64)

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	fail := func(err error) {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "stations"})
	}

	stations, err := evedb.ESPListStations(ctx, s.db, evedb.ESPStationBrowseFilter{
		Query: q, RegionID: regionID, ConstellationID: constellationID,
	})
	if err != nil {
		fail(err)
		return
	}
	regions, err := evedb.ESPStationRegions(ctx, s.db)
	if err != nil {
		fail(err)
		return
	}
	constellations, err := evedb.ESPStationConstellations(ctx, s.db)
	if err != nil {
		fail(err)
		return
	}

	// Live occupancy: DockedCount counts every character parked at a station, offline ones
	// included; only the session registry knows who is connected.
	online := make(map[int64]int64)
	for _, sess := range s.sessions.Snapshot() {
		if sess.StationID != 0 {
			online[sess.StationID]++
		}
	}

	stats := StationBrowserStats{
		Shown:    int64(len(stations)),
		Filtered: q != "" || regionID > 0 || constellationID > 0,
	}
	// Every station in the game is counted in the region option list, so the
	// grand total falls out of it without a second COUNT over staStations.
	for _, rg := range regions {
		stats.Total += rg.Stations
	}
	seenRegions := make(map[int64]bool)
	seenSystems := make(map[int64]bool)
	for _, st := range stations {
		seenRegions[st.RegionID] = true
		seenSystems[st.SolarSystemID] = true
		stats.Pilots += st.DockedCount
		stats.Agents += st.AgentCount
		stats.Orders += st.OrderCount
		stats.Online += online[st.StationID]
		if st.Conquerable {
			stats.Conquerable++
		}
	}
	stats.Regions = len(seenRegions)
	stats.Systems = len(seenSystems)

	data := map[string]any{
		"Stations":        stations,
		"Regions":         regions,
		"Constellations":  constellations,
		"Online":          online,
		"Stats":           stats,
		"Query":           q,
		"RegionID":        regionID,
		"ConstellationID": constellationID,
		"Page":            "stations",
	}
	s.render(w, "stations.html", data)
}

func (s *Server) handleItemSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	var items []evedb.ESPItemRow
	var err error
	if query != "" {
		items, err = evedb.ESPSearchItems(ctx, s.db, query)
		if err != nil {
			s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "items"})
			return
		}
	}
	data := map[string]any{
		"Items":    items,
		"Query":    query,
		"HasIcons": s.iconsDir != "",
		"Page":     "items",
	}
	s.render(w, "items.html", data)
}

func (s *Server) handleConsole(w http.ResponseWriter, r *http.Request) {
	var result string
	var cmdLine string
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err == nil {
			cmdLine = r.FormValue("command")
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			defer cancel()
			result, _ = executeSlashCommand(ctx, s.db, cmdLine)
		}
	}
	data := map[string]any{
		"Command": cmdLine,
		"Result":  result,
		"Page":    "console",
	}
	s.render(w, "console.html", data)
}

// executeSlashCommand runs a slash command without an EVE client connection.
// It delegates to the slash package's exported Execute function.
func executeSlashCommand(ctx context.Context, db *sql.DB, cmdLine string) (string, error) {
	// Import cycle prevention: we call the slash.Execute function that will
	// be wired in via a package-level variable.
	if SlashExecute == nil {
		return "Console not available (slash executor not configured).", nil
	}
	return SlashExecute(ctx, db, cmdLine)
}

// SlashExecute is set by the main package to wire in slash command execution
// without creating an import cycle.
var SlashExecute func(ctx context.Context, db *sql.DB, cmdLine string) (string, error)

// ── Dungeon Management ──────────────────────────────────────────────────────

func (s *Server) handleDungeons(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var result string

	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err == nil {
			name := strings.TrimSpace(r.FormValue("name"))
			factionID, _ := strconv.ParseInt(r.FormValue("factionID"), 10, 64)
			if name == "" {
				result = "Dungeon name is required."
			} else {
				vid, err := evedb.CreateDungeon(ctx, s.db, name, factionID)
				if err != nil {
					result = "Error: " + err.Error()
				} else {
					result = fmt.Sprintf("Created dungeon %q (VID %d)", name, vid)
				}
			}
		}
	}

	// Filter by faction.
	var filterFaction int64
	if f := r.URL.Query().Get("faction"); f != "" {
		filterFaction, _ = strconv.ParseInt(f, 10, 64)
	}

	dungeons, err := evedb.ESPGetAllDungeons(ctx, s.db, filterFaction)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "dungeons"})
		return
	}
	filterFactions, _ := evedb.ESPGetDungeonFactions(ctx, s.db)
	allFactions, _ := evedb.ESPGetAllFactions(ctx, s.db)

	data := map[string]any{
		"Dungeons":       dungeons,
		"FilterFactions": filterFactions,
		"AllFactions":    allFactions,
		"FilterFaction":  filterFaction,
		"Result":         result,
		"Page":           "dungeons",
	}
	s.render(w, "dungeons.html", data)
}

func (s *Server) handleDungeonView(w http.ResponseWriter, r *http.Request) {
	// URL: /dungeon/123
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 3 {
		http.NotFound(w, r)
		return
	}
	vid, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var result string

	// Handle POST actions: addroom, addobject, editobject, removeobject.
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err == nil {
			action := r.FormValue("action")
			switch action {
			case "addroom":
				roomName := strings.TrimSpace(r.FormValue("roomName"))
				if roomName == "" {
					roomName = "New Room"
				}
				roomID, err := evedb.CreateRoom(ctx, s.db, vid, roomName)
				if err != nil {
					result = "Error: " + err.Error()
				} else {
					result = fmt.Sprintf("Created room %q (ID %d)", roomName, roomID)
				}

			case "addobject":
				roomID, _ := strconv.ParseInt(r.FormValue("roomID"), 10, 64)
				typeID, _ := strconv.ParseInt(r.FormValue("typeID"), 10, 64)
				x, _ := strconv.ParseFloat(r.FormValue("x"), 64)
				y, _ := strconv.ParseFloat(r.FormValue("y"), 64)
				z, _ := strconv.ParseFloat(r.FormValue("z"), 64)
				if typeID == 0 {
					result = "Type ID is required."
				} else {
					objID, err := evedb.AddObject(ctx, s.db, roomID, typeID, x, y, z)
					if err != nil {
						result = "Error: " + err.Error()
					} else {
						result = fmt.Sprintf("Added object (ID %d) typeID=%d to room %d", objID, typeID, roomID)
					}
				}

			case "editobject":
				objectID, _ := strconv.ParseInt(r.FormValue("objectID"), 10, 64)
				fields := evedb.EditObjectFields{}
				if v := r.FormValue("x"); v != "" {
					f, _ := strconv.ParseFloat(v, 64)
					fields.X = &f
				}
				if v := r.FormValue("y"); v != "" {
					f, _ := strconv.ParseFloat(v, 64)
					fields.Y = &f
				}
				if v := r.FormValue("z"); v != "" {
					f, _ := strconv.ParseFloat(v, 64)
					fields.Z = &f
				}
				if v := r.FormValue("yaw"); v != "" {
					f, _ := strconv.ParseFloat(v, 64)
					fields.Yaw = &f
				}
				if v := r.FormValue("pitch"); v != "" {
					f, _ := strconv.ParseFloat(v, 64)
					fields.Pitch = &f
				}
				if v := r.FormValue("roll"); v != "" {
					f, _ := strconv.ParseFloat(v, 64)
					fields.Roll = &f
				}
				if v := r.FormValue("radius"); v != "" {
					f, _ := strconv.ParseFloat(v, 64)
					fields.Radius = &f
				}
				if v := r.FormValue("aggressionRange"); v != "" {
					f, _ := strconv.ParseFloat(v, 64)
					fields.AggressionRange = &f
				}
				if v, ok := r.Form["stance"]; ok && len(v) > 0 {
					if v[0] != "" && !npc.ValidStance(v[0]) {
						result = "Error: unknown stance " + v[0]
						break
					}
					fields.Stance = &v[0]
				}
				if v, ok := r.Form["musicUrl"]; ok && len(v) > 0 {
					fields.MusicUrl = &v[0]
				}
				if v, ok := r.Form["ballName"]; ok && len(v) > 0 {
					name := strings.TrimSpace(v[0])
					fields.BallName = &name
				}
				if v, ok := r.Form["hpScale"]; ok && len(v) > 0 {
					// Blank or unparsable reads 0, which EditObject stores as
					// NULL: the type's own HP.
					f, _ := strconv.ParseFloat(strings.TrimSpace(v[0]), 64)
					fields.HPScale = &f
				}
				if v, ok := r.Form["spawnWave"]; ok && len(v) > 0 {
					wave, _ := strconv.ParseInt(strings.TrimSpace(v[0]), 10, 64)
					if wave < 0 {
						wave = 0
					}
					fields.SpawnWave = &wave
				}
				parseSolidForm(r, &fields)
				if err := evedb.EditObject(ctx, s.db, objectID, fields); err != nil {
					result = "Error: " + err.Error()
				} else {
					result = fmt.Sprintf("Updated object %d", objectID)
				}

			case "copyobject":
				objectID, _ := strconv.ParseInt(r.FormValue("objectID"), 10, 64)
				offsetX, _ := strconv.ParseFloat(r.FormValue("offsetX"), 64)
				offsetY, _ := strconv.ParseFloat(r.FormValue("offsetY"), 64)
				offsetZ, _ := strconv.ParseFloat(r.FormValue("offsetZ"), 64)
				newID, err := evedb.CopyObject(ctx, s.db, objectID, offsetX, offsetY, offsetZ)
				if err != nil {
					result = "Error: " + err.Error()
				} else {
					result = fmt.Sprintf("Copied object %d -> new object %d", objectID, newID)
				}

			case "removeobject":
				objectID, _ := strconv.ParseInt(r.FormValue("objectID"), 10, 64)
				if err := evedb.RemoveObject(ctx, s.db, objectID); err != nil {
					result = "Error: " + err.Error()
				} else {
					result = fmt.Sprintf("Removed object %d", objectID)
				}

			case "deleteroom":
				roomID, _ := strconv.ParseInt(r.FormValue("roomID"), 10, 64)
				if err := evedb.DeleteRoom(ctx, s.db, roomID); err != nil {
					result = "Error: " + err.Error()
				} else {
					result = fmt.Sprintf("Deleted room %d", roomID)
				}

			case "renameroom":
				roomID, _ := strconv.ParseInt(r.FormValue("roomID"), 10, 64)
				newName := strings.TrimSpace(r.FormValue("roomName"))
				if newName == "" {
					result = "Room name is required."
				} else if err := evedb.RenameRoom(ctx, s.db, roomID, newName); err != nil {
					result = "Error: " + err.Error()
				} else {
					result = fmt.Sprintf("Renamed room %d to %q", roomID, newName)
				}

			case "roomphenomena":
				roomID, _ := strconv.ParseInt(r.FormValue("roomID"), 10, 64)
				on := r.FormValue("naturalPhenomena") != ""
				if err := evedb.SetRoomNaturalPhenomena(ctx, s.db, roomID, on); err != nil {
					result = "Error: " + err.Error()
				} else if on {
					result = fmt.Sprintf("Room %d: natural phenomena ON — warp drives are dead inside the pocket.", roomID)
				} else {
					result = fmt.Sprintf("Room %d: natural phenomena OFF — players can warp around inside this room.", roomID)
				}

			case "editdungeon":
				newName := strings.TrimSpace(r.FormValue("dungeonName"))
				newFaction, _ := strconv.ParseInt(r.FormValue("factionID"), 10, 64)
				newDED, _ := strconv.ParseInt(r.FormValue("dedLevel"), 10, 64)
				if newName == "" {
					result = "Dungeon name is required."
				} else if err := evedb.UpdateDungeonMeta(ctx, s.db, vid, newName, newFaction, newDED); err != nil {
					result = "Error: " + err.Error()
				} else {
					result = fmt.Sprintf("Updated dungeon name to %q, faction to %d, DED rating to %d", newName, newFaction, newDED)
				}

			case "deletedungeon":
				if err := evedb.DeleteDungeon(ctx, s.db, vid); err != nil {
					result = "Error: " + err.Error()
				} else {
					http.Redirect(w, r, "/dungeons", http.StatusSeeOther)
					return
				}

			case "savetrigger":
				t := evedb.TriggerRow{}
				t.TriggerID, _ = strconv.ParseInt(r.FormValue("triggerID"), 10, 64)
				t.RoomID, _ = strconv.ParseInt(r.FormValue("roomID"), 10, 64)
				t.Event = r.FormValue("event")
				t.Action = r.FormValue("triggerAction")
				if tgt, _ := strconv.ParseInt(r.FormValue("targetObjectID"), 10, 64); tgt > 0 {
					t.TargetObjectID = sql.NullInt64{Int64: tgt, Valid: true}
				}
				if v := strings.TrimSpace(r.FormValue("header")); v != "" {
					t.Header = sql.NullString{String: v, Valid: true}
				}
				if v := strings.TrimSpace(r.FormValue("icon")); v != "" {
					t.Icon = sql.NullString{String: v, Valid: true}
				}
				if v := r.FormValue("body"); strings.TrimSpace(v) != "" {
					t.Body = sql.NullString{String: v, Valid: true}
				}
				if v := strings.TrimSpace(r.FormValue("senderName")); v != "" {
					t.SenderName = sql.NullString{String: v, Valid: true}
				}
				// localChat text color: the picker posts "#rrggbb" and it counts only when chatColorOn is
				// ticked; otherwise the client default (white) is used.
				if r.FormValue("chatColorOn") != "" {
					if v := strings.TrimPrefix(strings.TrimSpace(r.FormValue("chatColor")), "#"); len(v) == 6 && isHex(v) {
						t.ChatColor = sql.NullString{String: strings.ToUpper(v), Valid: true}
					}
				}
				t.DeferUntilLanded = r.FormValue("deferUntilLanded") != ""
				t.OnceOnly = r.FormValue("onceOnly") != ""
				t.DelayMs, _ = strconv.ParseInt(r.FormValue("delayMs"), 10, 64)
				if wave, _ := strconv.ParseInt(r.FormValue("spawnWave"), 10, 64); wave > 0 {
					t.SpawnWave = sql.NullInt64{Int64: wave, Valid: true}
				}
				// The item filter belongs to itemPlaced alone; no item means any item.
				t.ItemQty = 1
				if t.Event == "itemPlaced" {
					if id, _ := strconv.ParseInt(r.FormValue("itemTypeID"), 10, 64); id > 0 {
						t.ItemTypeID = sql.NullInt64{Int64: id, Valid: true}
					}
					if q, _ := strconv.ParseInt(r.FormValue("itemQty"), 10, 64); q > 1 {
						t.ItemQty = q
					}
				}
				validEvents := map[string]bool{"roomEnter": true, "npcKilled": true, "npcDamaged": true, "roomCleared": true, "gateActivated": true, "itemPlaced": true}
				// A music trigger stores its track in body.
				if t.Action == "music" {
					track := r.FormValue("trigMusic")
					if track == "__custom__" {
						track = strings.TrimSpace(r.FormValue("trigMusicCustom"))
					}
					t.Body = sql.NullString{String: track, Valid: track != ""}
				}
				validActions := map[string]bool{"transmission": true, "localChat": true, "popup": true, "spawnWave": true, "music": true, "completeDungeon": true}
				if t.RoomID == 0 || !validEvents[t.Event] || !validActions[t.Action] {
					result = "Trigger needs a room, a valid event, and a valid action."
				} else if t.Action == "spawnWave" && !t.SpawnWave.Valid {
					result = "spawnWave trigger needs a wave number (1 or higher)."
				} else if t.Action == "music" && !evedb.ValidMusicURL(t.Body.String) {
					result = "Music trigger needs a track: a res:/ path ending in .ogg, .wav or .mp3."
				} else if id, err := evedb.SaveTrigger(ctx, s.db, t); err != nil {
					result = "Error: " + err.Error()
				} else {
					result = fmt.Sprintf("Saved trigger %d (%s → %s)", id, t.Event, t.Action)
				}

			case "deletetrigger":
				triggerID, _ := strconv.ParseInt(r.FormValue("triggerID"), 10, 64)
				if err := evedb.DeleteTrigger(ctx, s.db, triggerID); err != nil {
					result = "Error: " + err.Error()
				} else {
					result = fmt.Sprintf("Deleted trigger %d", triggerID)
				}

			case "refreshplayers":
				// Push the updated dungeon layout to all connected players
				// currently viewing any room in this dungeon.
				roomIDs, err := evedb.GetDungeonRoomIDs(ctx, s.db, vid)
				if err != nil {
					result = "Error: " + err.Error()
				} else {
					n := s.sessions.RefreshDungeonAll(roomIDs)
					if n > 0 {
						result = fmt.Sprintf("Saved. Refreshed dungeon for %d connected player(s).", n)
					} else {
						result = "Saved. No players currently viewing this dungeon."
					}
				}
			}
		}
	}

	detail, err := evedb.ESPGetDungeonDetail(ctx, s.db, vid)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "dungeons"})
		return
	}

	prefillTypeID, _ := strconv.ParseInt(r.URL.Query().Get("prefillTypeID"), 10, 64)

	allFactions, _ := evedb.ESPGetAllFactions(ctx, s.db)

	data := map[string]any{
		"Dungeon":       detail,
		"Result":        result,
		"PrefillTypeID": prefillTypeID,
		"Page":          "dungeons",
		"Stances":       npc.Stances(),
		"AllFactions":   allFactions,
		"DEDLevels":     []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
	}
	s.render(w, "dungeon_view.html", data)
}

func (s *Server) handleDungeonObjectAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		// Fall back to url-encoded form parsing.
		if err2 := r.ParseForm(); err2 != nil {
			http.Error(w, err2.Error(), http.StatusBadRequest)
			return
		}
	}
	objectID, err := strconv.ParseInt(r.FormValue("objectID"), 10, 64)
	if err != nil || objectID == 0 {
		http.Error(w, "invalid objectID", http.StatusBadRequest)
		return
	}
	fields := evedb.EditObjectFields{}
	if v := r.FormValue("x"); v != "" {
		f, _ := strconv.ParseFloat(v, 64)
		fields.X = &f
	}
	if v := r.FormValue("y"); v != "" {
		f, _ := strconv.ParseFloat(v, 64)
		fields.Y = &f
	}
	if v := r.FormValue("z"); v != "" {
		f, _ := strconv.ParseFloat(v, 64)
		fields.Z = &f
	}
	if v := r.FormValue("yaw"); v != "" {
		f, _ := strconv.ParseFloat(v, 64)
		fields.Yaw = &f
	}
	if v := r.FormValue("pitch"); v != "" {
		f, _ := strconv.ParseFloat(v, 64)
		fields.Pitch = &f
	}
	if v := r.FormValue("roll"); v != "" {
		f, _ := strconv.ParseFloat(v, 64)
		fields.Roll = &f
	}
	if v := r.FormValue("radius"); v != "" {
		f, _ := strconv.ParseFloat(v, 64)
		fields.Radius = &f
	}
	if v := r.FormValue("aggressionRange"); v != "" {
		f, _ := strconv.ParseFloat(v, 64)
		fields.AggressionRange = &f
	}
	if v, ok := r.Form["stance"]; ok && len(v) > 0 {
		if v[0] != "" && !npc.ValidStance(v[0]) {
			http.Error(w, "unknown stance "+v[0], http.StatusBadRequest)
			return
		}
		fields.Stance = &v[0]
	}
	if v, ok := r.Form["npcFactionID"]; ok && len(v) > 0 {
		f, _ := strconv.ParseInt(v[0], 10, 64)
		fields.NPCFactionID = &f
	}
	if v, ok := r.Form["musicUrl"]; ok && len(v) > 0 {
		fields.MusicUrl = &v[0]
	}
	if v, ok := r.Form["leashObjectID"]; ok && len(v) > 0 {
		f, _ := strconv.ParseInt(v[0], 10, 64)
		fields.LeashObjectID = &f
	}
	if v, ok := r.Form["orbitDistance"]; ok && len(v) > 0 {
		f, _ := strconv.ParseFloat(v[0], 64)
		fields.OrbitDistance = &f
	}
	if v, ok := r.Form["description"]; ok && len(v) > 0 {
		fields.Description = &v[0]
	}
	if v, ok := r.Form["ballName"]; ok && len(v) > 0 {
		name := strings.TrimSpace(v[0])
		fields.BallName = &name
	}
	if v, ok := r.Form["hpScale"]; ok && len(v) > 0 {
		f, _ := strconv.ParseFloat(strings.TrimSpace(v[0]), 64)
		fields.HPScale = &f
	}
	if v, ok := r.Form["spawnWave"]; ok && len(v) > 0 {
		wave, _ := strconv.ParseInt(strings.TrimSpace(v[0]), 10, 64)
		if wave < 0 {
			wave = 0
		}
		fields.SpawnWave = &wave
	}
	// Only structure rows carry the checkbox and the page posts an explicit 0/1: absent means no
	// Invuln control, so a rat's save never clears a structure's flag.
	if v, ok := r.Form["invulnerable"]; ok && len(v) > 0 {
		inv := strings.TrimSpace(v[0]) == "1" || strings.EqualFold(strings.TrimSpace(v[0]), "on") || strings.EqualFold(strings.TrimSpace(v[0]), "true")
		fields.Invulnerable = &inv
	}
	parseSolidForm(r, &fields)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := evedb.EditObject(ctx, s.db, objectID, fields); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleDungeonLootAPI authors the per-ball drop list (dunObjectLoot) from the
// Dungeon Logic page. POST action=save adds/updates one item on a ball,
// action=delete removes an entry, action=range sets the item-count bounds and
// action=paste replaces the list with fromObjectID's. Each answers with the
// ball's full list so the page can redraw without a reload.
func (s *Server) handleDungeonLootAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		if err2 := r.ParseForm(); err2 != nil {
			http.Error(w, err2.Error(), http.StatusBadRequest)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	objectID, _ := strconv.ParseInt(r.FormValue("objectID"), 10, 64)

	writeErr := func(msg string, code int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]string{"error": msg})
	}

	switch r.FormValue("action") {
	case "delete":
		lootID, _ := strconv.ParseInt(r.FormValue("lootID"), 10, 64)
		if lootID == 0 {
			writeErr("invalid lootID", http.StatusBadRequest)
			return
		}
		if err := evedb.DeleteObjectLootEntry(ctx, s.db, lootID); err != nil {
			writeErr(err.Error(), http.StatusInternalServerError)
			return
		}

	case "save":
		itemTypeID, _ := strconv.ParseInt(r.FormValue("itemTypeID"), 10, 64)
		if objectID == 0 || itemTypeID == 0 {
			writeErr("objectID and itemTypeID are required", http.StatusBadRequest)
			return
		}
		minQty, _ := strconv.ParseInt(r.FormValue("minQty"), 10, 64)
		maxQty, _ := strconv.ParseInt(r.FormValue("maxQty"), 10, 64)
		chance, err := strconv.ParseFloat(strings.TrimSpace(r.FormValue("dropChance")), 64)
		if err != nil || chance <= 0 {
			chance = 1
		}
		if chance > 1 {
			// The form posts percentages; anything above 1 is read as one.
			chance = chance / 100
			if chance > 1 {
				chance = 1
			}
		}
		if err := evedb.SaveObjectLoot(ctx, s.db, objectID, itemTypeID, minQty, maxQty, chance); err != nil {
			writeErr(err.Error(), http.StatusInternalServerError)
			return
		}

	case "range":
		// How many distinct items this ball's authored list may yield. A blank
		// field is "no bound" on that side.
		if objectID == 0 {
			writeErr("objectID is required", http.StatusBadRequest)
			return
		}
		lo, _ := strconv.ParseInt(strings.TrimSpace(r.FormValue("minDrops")), 10, 64)
		hi, _ := strconv.ParseInt(strings.TrimSpace(r.FormValue("maxDrops")), 10, 64)
		lo, hi, err := evedb.ValidateLootDropRange(lo, hi)
		if err != nil {
			writeErr(err.Error(), http.StatusBadRequest)
			return
		}
		if err := evedb.EditObject(ctx, s.db, objectID, evedb.EditObjectFields{
			LootMinDrops: &lo, LootMaxDrops: &hi,
		}); err != nil {
			writeErr(err.Error(), http.StatusInternalServerError)
			return
		}

	case "paste":
		fromObjectID, _ := strconv.ParseInt(r.FormValue("fromObjectID"), 10, 64)
		if err := evedb.PasteObjectLoot(ctx, s.db, fromObjectID, objectID); err != nil {
			writeErr(err.Error(), http.StatusConflict)
			return
		}

	default:
		writeErr("unknown action", http.StatusBadRequest)
		return
	}

	entries, err := evedb.GetObjectLoot(ctx, s.db, objectID)
	if err != nil {
		writeErr(err.Error(), http.StatusInternalServerError)
		return
	}
	minDrops, maxDrops, err := evedb.GetObjectLootDropRange(ctx, s.db, objectID)
	if err != nil {
		writeErr(err.Error(), http.StatusInternalServerError)
		return
	}
	type apiLoot struct {
		LootID     int64   `json:"lootID"`
		ItemTypeID int64   `json:"itemTypeID"`
		ItemName   string  `json:"itemName"`
		MinQty     int64   `json:"minQty"`
		MaxQty     int64   `json:"maxQty"`
		DropChance  float64 `json:"dropChance"`
		IsBlueprint bool    `json:"isBlueprint"`
	}
	out := make([]apiLoot, len(entries))
	for i, e := range entries {
		out[i] = apiLoot{e.LootID, e.ItemTypeID, e.ItemName, e.MinQty, e.MaxQty, e.DropChance, e.IsBlueprint}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"status": "ok", "objectID": objectID, "loot": out,
		"minDrops": minDrops, "maxDrops": maxDrops})
}

func (s *Server) handleDungeonItemsAPI(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	items, err := evedb.ESPSearchDETypes(ctx, s.db, q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type apiItem struct {
		TypeID   int64   `json:"typeID"`
		TypeName string  `json:"typeName"`
		Group    string  `json:"group"`
		Category string  `json:"category"`
		Radius   float64 `json:"radius"`
	}
	out := make([]apiItem, len(items))
	for i, it := range items {
		out[i] = apiItem{it.TypeID, it.TypeName, it.GroupName, it.CategoryName, it.Radius}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// ── NPC Browser ─────────────────────────────────────────────────────────────

func (s *Server) handleNPCs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var result string

	query := r.URL.Query().Get("q")
	groupFilter, _ := strconv.ParseInt(r.URL.Query().Get("group"), 10, 64)
	sortBy := r.URL.Query().Get("sort")

	groups, _ := evedb.ESPGetNPCGroups(ctx, s.db)

	var npcs []evedb.ESPNPCTypeRow
	var err error
	if query != "" || groupFilter > 0 {
		npcs, err = evedb.ESPSearchNPCTypes(ctx, s.db, query, groupFilter, sortBy)
		if err != nil {
			s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "npcs"})
			return
		}
	}

	factions, _ := evedb.ESPGetAllFactions(ctx, s.db)
	custom, _ := evedb.ESPListCustomNPCTypes(ctx, s.db)

	data := map[string]any{
		"NPCs":      npcs,
		"Custom":    custom,
		"Groups":    groups,
		"Factions":  factions,
		"Query":     query,
		"GroupID":   groupFilter,
		"Sort":      sortBy,
		"Result":    result,
		"HasIcons":  s.iconsDir != "",
		"HasModels": len(s.modelsDirs) > 0,
		"Page":      "npcs",
	}
	s.render(w, "npcs.html", data)
}

func (s *Server) handleNPCView(w http.ResponseWriter, r *http.Request) {
	// URL: /npc/12345
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 3 {
		http.NotFound(w, r)
		return
	}
	typeID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var result string
	isCustom := typeID >= 100000
	// posted holds the submitted spec when a save failed, so the form re-renders what was typed.
	var posted *evedb.NPCTypeSpec
	var saveErr error

	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err == nil {
			action := r.FormValue("action")
			switch action {
			case "update_attr":
				attrID, _ := strconv.ParseInt(r.FormValue("attributeID"), 10, 64)
				value, _ := strconv.ParseFloat(r.FormValue("value"), 64)
				if err := evedb.ESPUpdateTypeAttribute(ctx, s.db, typeID, attrID, value); err != nil {
					result = "Error: " + err.Error()
				} else {
					result = fmt.Sprintf("Updated attribute %d = %g", attrID, value)
				}
			case "add_attr":
				attrID, _ := strconv.ParseInt(r.FormValue("attributeID"), 10, 64)
				value, _ := strconv.ParseFloat(r.FormValue("value"), 64)
				if attrID == 0 {
					result = "Error: attribute ID is required."
				} else if err := evedb.ESPUpdateTypeAttribute(ctx, s.db, typeID, attrID, value); err != nil {
					result = "Error: " + err.Error()
				} else {
					result = fmt.Sprintf("Added attribute %d = %g", attrID, value)
				}
			case "delete_attr":
				attrID, _ := strconv.ParseInt(r.FormValue("attributeID"), 10, 64)
				if err := evedb.ESPDeleteTypeAttribute(ctx, s.db, typeID, attrID); err != nil {
					result = "Error: " + err.Error()
				} else {
					result = fmt.Sprintf("Deleted attribute %d", attrID)
				}
			case "save_spec":
				// Full authoring form: rewrites the invTypes row and every attribute the spawn path reads, in
				// one transaction. A stock (SDE) type needs the confirm box: saving drops the managed
				// attributes the form leaves at 0.
				posted = specFromForm(r, typeID)
				if !isCustom && r.FormValue("confirmStock") == "" {
					result = "Error: this is a stock game type. Tick the confirm box to overwrite it, or clone it instead."
				} else if _, err := evedb.ESPSaveNPCSpec(ctx, s.db, posted); err != nil {
					saveErr = err
				} else {
					invalidateTypeCache()
					npcTypeChanged(typeID)
					// Redirect so a browser refresh does not post the form again.
					http.Redirect(w, r, "/npc/"+strconv.FormatInt(typeID, 10)+"?saved=1", http.StatusSeeOther)
					return
				}
			}
			// The raw attribute actions touched dgmTypeAttributes.
			if action != "save_spec" && action != "" && !strings.HasPrefix(result, "Error:") {
				invalidateTypeCache()
				npcTypeChanged(typeID)
			}
		}
	}

	switch {
	case result != "" || saveErr != nil:
	case r.URL.Query().Get("created") != "":
		result = "NPC created."
	case r.URL.Query().Get("saved") != "":
		result = "Saved NPC definition."
	}

	spec, err := evedb.ESPLoadNPCSpec(ctx, s.db, typeID)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "npcs"})
		return
	}
	detail, err := evedb.ESPGetNPCType(ctx, s.db, typeID)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "npcs"})
		return
	}
	attrs, _ := evedb.ESPGetTypeAttributes(ctx, s.db, typeID)
	spawns, _ := evedb.ESPRegionSpawnsForType(ctx, s.db, typeID)

	if posted != nil {
		spec = posted
	}
	data := s.npcFormData(ctx, spec, result, "npcs")
	if posted != nil {
		if f := r.FormValue("damageFlavour"); f != "" {
			data["Flavour"] = f
		}
	}
	if saveErr != nil {
		addProblems(data, saveErr)
	}
	data["IsNew"] = false
	data["NPC"] = detail
	data["Attributes"] = attrs
	data["Weapon"] = s.buildWeaponSummary(ctx, typeID)
	data["IsCustom"] = isCustom
	data["Created"] = r.URL.Query().Get("created") != ""
	data["SpawnListings"] = spawns
	s.render(w, "npc_view.html", data)
}

// ── Agent Browser ───────────────────────────────────────────────────────────

func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	corpID, _ := strconv.ParseInt(r.URL.Query().Get("corp"), 10, 64)
	divisionID, _ := strconv.ParseInt(r.URL.Query().Get("division"), 10, 64)
	stationID, _ := strconv.ParseInt(r.URL.Query().Get("station"), 10, 64)

	divisions, _ := evedb.ESPGetAgentDivisions(ctx, s.db)
	corps, _ := evedb.ESPGetAgentCorps(ctx, s.db)
	stations, _ := evedb.ESPGetAgentStations(ctx, s.db, corpID)

	var agents []evedb.ESPAgentRow
	var err error
	searched := corpID > 0 || divisionID > 0 || stationID > 0
	if searched {
		agents, err = evedb.ESPSearchAgents(ctx, s.db, corpID, divisionID, stationID)
		if err != nil {
			s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "agents"})
			return
		}
	}

	data := map[string]any{
		"Agents":         agents,
		"Divisions":      divisions,
		"Corps":          corps,
		"Stations":       stations,
		"CorpID":         corpID,
		"Division":       divisionID,
		"StationID":      stationID,
		"Searched":       searched,
		"ScrambleResult": r.URL.Query().Get("scrambleResult"),
		"HasPortraits":   s.portraitsDir != "",
		"Page":           "agents",
	}
	s.render(w, "agents.html", data)
}

func (s *Server) handleAgentView(w http.ResponseWriter, r *http.Request) {
	// URL: /agent/12345
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 3 {
		http.NotFound(w, r)
		return
	}
	agentID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var result string

	// Ensure the appearance override table exists.
	_ = evedb.ESPEnsureAppearanceTable(ctx, s.db)

	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err == nil {
			action := r.FormValue("action")
			switch action {
			case "update_agent":
				agentTypeID, _ := strconv.ParseInt(r.FormValue("agentTypeID"), 10, 64)
				divisionID, _ := strconv.ParseInt(r.FormValue("divisionID"), 10, 64)
				level, _ := strconv.ParseInt(r.FormValue("level"), 10, 64)
				stationID, _ := strconv.ParseInt(r.FormValue("stationID"), 10, 64)
				quality, _ := strconv.ParseInt(r.FormValue("quality"), 10, 64)
				corporationID, _ := strconv.ParseInt(r.FormValue("corporationID"), 10, 64)
				if err := evedb.ESPUpdateAgent(ctx, s.db, agentID, agentTypeID, divisionID, level, stationID, 0, quality, corporationID, 0); err != nil {
					result = "Error: " + err.Error()
				} else {
					result = "Agent updated."
				}
			case "update_portrait":
				ap := &evedb.ESPAgentAppearance{
					AccessoryID:   parseInt64(r.FormValue("accessoryID")),
					BeardID:       parseInt64(r.FormValue("beardID")),
					CostumeID:     parseInt64(r.FormValue("costumeID")),
					DecoID:        parseInt64(r.FormValue("decoID")),
					EyebrowsID:    parseInt64(r.FormValue("eyebrowsID")),
					EyesID:        parseInt64(r.FormValue("eyesID")),
					HairID:        parseInt64(r.FormValue("hairID")),
					LipstickID:    parseInt64(r.FormValue("lipstickID")),
					MakeupID:      parseInt64(r.FormValue("makeupID")),
					SkinID:        parseInt64(r.FormValue("skinID")),
					BackgroundID:  parseInt64(r.FormValue("backgroundID")),
					LightID:       parseInt64(r.FormValue("lightID")),
					HeadRotation1: parseFloat64(r.FormValue("headRotation1")),
					HeadRotation2: parseFloat64(r.FormValue("headRotation2")),
					HeadRotation3: parseFloat64(r.FormValue("headRotation3")),
					EyeRotation1:  parseFloat64(r.FormValue("eyeRotation1")),
					EyeRotation2:  parseFloat64(r.FormValue("eyeRotation2")),
					EyeRotation3:  parseFloat64(r.FormValue("eyeRotation3")),
				}
				// Also update bloodlineID and gender on agtAgents.
				bloodlineID, _ := strconv.ParseInt(r.FormValue("bloodlineID"), 10, 64)
				gender, _ := strconv.ParseInt(r.FormValue("gender"), 10, 64)
				_ = evedb.ESPUpdateAgentIdentity(ctx, s.db, agentID, bloodlineID, gender)
				if err := evedb.ESPUpdateAgentAppearance(ctx, s.db, agentID, ap); err != nil {
					result = "Error: " + err.Error()
				} else {
					result = "Portrait updated."
				}
			}
		}
	}

	detail, err := evedb.ESPGetAgent(ctx, s.db, agentID)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "agents"})
		return
	}
	divisions, _ := evedb.ESPGetAgentDivisions(ctx, s.db)
	corps, _ := evedb.ESPGetAgentCorps(ctx, s.db)
	agentTypes, _ := evedb.ESPGetAgentTypes(ctx, s.db)
	bloodlines, _ := evedb.ESPGetAgentBloodlines(ctx, s.db)
	appearance, _ := evedb.ESPGetAgentAppearance(ctx, s.db, agentID, detail.BloodlineID, detail.Gender)
	appearanceOpts := evedb.ESPGetAllAppearanceOptions(ctx, s.db, detail.BloodlineID, detail.Gender)

	data := map[string]any{
		"Agent":          detail,
		"Divisions":      divisions,
		"Corps":          corps,
		"AgentTypes":     agentTypes,
		"Bloodlines":     bloodlines,
		"Appearance":     appearance,
		"AppearanceOpts": appearanceOpts,
		"Result":         result,
		"HasPortraits":   s.portraitsDir != "",
		"Page":           "agents",
	}
	if detail.AgentTypeID == evedb.AgentTypeResearch {
		if fieldIDs, ferr := evedb.GetAgentResearchFields(ctx, s.db, agentID); ferr == nil {
			type researchField struct {
				SkillID   int64
				FieldName string
			}
			var rfs []researchField
			for _, f := range fieldIDs {
				name, _ := evedb.GetTypeName(ctx, s.db, f)
				rfs = append(rfs, researchField{SkillID: f, FieldName: name})
			}
			data["ResearchFields"] = rfs
		}
	}
	s.render(w, "agent_view.html", data)
}

func (s *Server) handleAgentCreate(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var result string

	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err == nil {
			agentID, _ := strconv.ParseInt(r.FormValue("agentID"), 10, 64)
			name := strings.TrimSpace(r.FormValue("agentName"))
			agentTypeID, _ := strconv.ParseInt(r.FormValue("agentTypeID"), 10, 64)
			divisionID, _ := strconv.ParseInt(r.FormValue("divisionID"), 10, 64)
			level, _ := strconv.ParseInt(r.FormValue("level"), 10, 64)
			stationID, _ := strconv.ParseInt(r.FormValue("stationID"), 10, 64)
			bloodlineID, _ := strconv.ParseInt(r.FormValue("bloodlineID"), 10, 64)
			quality, _ := strconv.ParseInt(r.FormValue("quality"), 10, 64)
			corporationID, _ := strconv.ParseInt(r.FormValue("corporationID"), 10, 64)
			gender, _ := strconv.ParseInt(r.FormValue("gender"), 10, 64)

			log.Printf("[ESP] CreateAgent: id=%d name=%q type=%d div=%d lv=%d station=%d blood=%d qual=%d corp=%d gender=%d",
				agentID, name, agentTypeID, divisionID, level, stationID, bloodlineID, quality, corporationID, gender)

			if agentID == 0 {
				result = "Agent ID is required."
			} else if name == "" {
				result = "Agent name is required."
			} else {
				if err := evedb.ESPCreateAgent(ctx, s.db, agentID, name, agentTypeID, divisionID, level, stationID, bloodlineID, quality, corporationID, gender); err != nil {
					log.Printf("[ESP] CreateAgent error: %v", err)
					result = "Error: " + err.Error()
				} else {
					log.Printf("[ESP] CreateAgent success: agent %d created", agentID)
					http.Redirect(w, r, "/agent/"+strconv.FormatInt(agentID, 10), http.StatusSeeOther)
					return
				}
			}
		} else {
			log.Printf("[ESP] CreateAgent ParseForm error: %v", err)
		}
	}

	divisions, _ := evedb.ESPGetAgentDivisions(ctx, s.db)
	corps, _ := evedb.ESPGetAgentCorps(ctx, s.db)
	agentTypes, _ := evedb.ESPGetAgentTypes(ctx, s.db)
	bloodlines, _ := evedb.ESPGetAgentBloodlines(ctx, s.db)
	nextID := evedb.ESPNextAgentID(ctx, s.db)

	data := map[string]any{
		"Divisions":  divisions,
		"Corps":      corps,
		"AgentTypes": agentTypes,
		"Bloodlines": bloodlines,
		"NextID":     nextID,
		"Result":     result,
		"Page":       "agents",
	}
	s.render(w, "agent_create.html", data)
}

func (s *Server) handleAgentStationsAPI(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	stations, err := evedb.ESPSearchStations(ctx, s.db, q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type apiStation struct {
		StationID   int64  `json:"stationID"`
		StationName string `json:"stationName"`
		SystemName  string `json:"systemName"`
	}
	out := make([]apiStation, len(stations))
	for i, st := range stations {
		out[i] = apiStation{st.StationID, st.StationName, st.SystemName}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (s *Server) handleAgentCorpStationsAPI(w http.ResponseWriter, r *http.Request) {
	corpID, _ := strconv.ParseInt(r.URL.Query().Get("corp"), 10, 64)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	stations, _ := evedb.ESPGetAgentStations(ctx, s.db, corpID)
	type apiStation struct {
		StationID   int64  `json:"stationID"`
		StationName string `json:"stationName"`
	}
	out := make([]apiStation, 0, len(stations))
	for _, st := range stations {
		out = append(out, apiStation{st.StationID, st.StationName})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (s *Server) handleAgentAppearanceOptsAPI(w http.ResponseWriter, r *http.Request) {
	bloodlineID, _ := strconv.ParseInt(r.URL.Query().Get("bloodline"), 10, 64)
	gender, _ := strconv.ParseInt(r.URL.Query().Get("gender"), 10, 64)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	opts := evedb.ESPGetAllAppearanceOptions(ctx, s.db, bloodlineID, gender)

	// Convert to a JSON-friendly map of slot → [{id, name}].
	type optJSON struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	out := make(map[string][]optJSON, len(opts))
	for slot, items := range opts {
		list := make([]optJSON, len(items))
		for i, o := range items {
			list[i] = optJSON{o.ID, o.Name}
		}
		out[slot] = list
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (s *Server) handleAgentScramblePortraits(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/agents", http.StatusSeeOther)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	_ = r.ParseForm()
	randomizeMorphs := r.FormValue("randomizeMorphs") == "on"
	randomizeHeadRotation := r.FormValue("randomizeHeadRotation") == "on"
	randomizeEyeRotation := r.FormValue("randomizeEyeRotation") == "on"

	_ = evedb.ESPEnsureAppearanceTable(ctx, s.db)
	count, err := evedb.ESPScrambleAllAgentPortraits(ctx, s.db, randomizeMorphs, randomizeHeadRotation, randomizeEyeRotation)
	if err != nil {
		http.Redirect(w, r, "/agents?scrambleResult=Error:+"+err.Error(), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/agents?scrambleResult=Scrambled+%d+agent+portraits.", count), http.StatusSeeOther)
}

// handleCEOScramblePortraits randomizes portraits for NPC corporation CEOs.
// Separate from the agent scrambler because CEOs are not agents: they exist
// only as corporation.ceoID + an eveStaticOwners row, with the bloodline coming
// from the character typeID and the gender derived from the ID itself.
func (s *Server) handleCEOScramblePortraits(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/agents", http.StatusSeeOther)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	_ = r.ParseForm()
	randomizeMorphs := r.FormValue("randomizeMorphs") == "on"
	randomizeHeadRotation := r.FormValue("randomizeHeadRotation") == "on"
	randomizeEyeRotation := r.FormValue("randomizeEyeRotation") == "on"

	_ = evedb.ESPEnsureAppearanceTable(ctx, s.db)
	count, err := evedb.ESPScrambleAllCEOPortraits(ctx, s.db, randomizeMorphs, randomizeHeadRotation, randomizeEyeRotation)
	if err != nil {
		http.Redirect(w, r, "/agents?scrambleResult=Error:+"+err.Error(), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/agents?scrambleResult=Scrambled+%d+NPC+CEO+portraits.", count), http.StatusSeeOther)
}

func (s *Server) handleDungeonItems(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	query := r.URL.Query().Get("q")
	categoryID, _ := strconv.ParseInt(r.URL.Query().Get("category"), 10, 64)
	groupID, _ := strconv.ParseInt(r.URL.Query().Get("group"), 10, 64)

	var items []evedb.ESPDETypeRow
	var groups []evedb.ESPDEGroup
	var err error

	categories, _ := evedb.ESPGetDECategories(ctx, s.db)

	if query != "" {
		items, err = evedb.ESPSearchDETypes(ctx, s.db, query)
		if err != nil {
			s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "dungeon_items"})
			return
		}
	} else if groupID > 0 {
		items, err = evedb.ESPGetDETypes(ctx, s.db, groupID)
		if err != nil {
			s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "dungeon_items"})
			return
		}
	}

	if categoryID > 0 {
		groups, _ = evedb.ESPGetDEGroups(ctx, s.db, categoryID)
	}

	dungeonVID, _ := strconv.ParseInt(r.URL.Query().Get("dungeon"), 10, 64)

	data := map[string]any{
		"Categories": categories,
		"Groups":     groups,
		"Items":      items,
		"Query":      query,
		"CategoryID": categoryID,
		"GroupID":    groupID,
		"DungeonVID": dungeonVID,
		"Page":       "dungeon_items",
	}
	s.render(w, "dungeon_items.html", data)
}

// parseInt64 parses a form value to int64, returning 0 on failure.
func parseInt64(s string) int64 {
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}

// arrStr/arrInt/arrInt64 safely index a parallel form-value array, returning a
// zero value when the index is out of range (used by bulk-save handlers).
func arrStr(a []string, i int) string {
	if i < 0 || i >= len(a) {
		return ""
	}
	return strings.TrimSpace(a[i])
}

func arrInt(a []string, i int) int {
	return int(parseInt64(arrStr(a, i)))
}

func arrInt64(a []string, i int) int64 {
	return parseInt64(arrStr(a, i))
}

// parseFloat64 parses a form value to float64, returning 0 on failure.
func parseFloat64(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

// ── Spawner Management ──────────────────────────────────────────────────────
func (s *Server) handleSpawnerNPCTypesAPI(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	groupID, _ := strconv.ParseInt(r.URL.Query().Get("group"), 10, 64)
	if q == "" && groupID == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
		return
	}
	if q == "" {
		q = "%"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	npcs, err := evedb.ESPSearchNPCTypes(ctx, s.db, q, groupID, "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type apiNPC struct {
		TypeID   int64  `json:"typeID"`
		TypeName string `json:"typeName"`
		Group    string `json:"group"`
	}
	out := make([]apiNPC, len(npcs))
	for i, n := range npcs {
		out[i] = apiNPC{n.TypeID, n.TypeName, n.GroupName}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// newsImageNameRE restricts uploaded news image filenames to safe characters.
var newsImageNameRE = regexp.MustCompile(`^[a-zA-Z0-9._-]+\.(jpg|jpeg|png|gif|bmp)$`)

func (s *Server) handleNews(w http.ResponseWriter, r *http.Request) {
	var result string
	newsImagesDir := filepath.Join("data", "news-images")

	if r.Method == http.MethodPost {
		if ct := r.Header.Get("Content-Type"); strings.HasPrefix(ct, "multipart/form-data") {
			result = s.handleNewsImageUpload(r, newsImagesDir)
		} else if err := r.ParseForm(); err == nil {
			action := r.FormValue("action")
			switch action {
			case "save":
				date, err := time.Parse("2006-01-02T15:04", r.FormValue("publishDate"))
				if err != nil {
					date = time.Now()
				}
				a := evedb.NewsArticle{
					ID:            parseInt64(r.FormValue("newsID")),
					Date:          date,
					Title:         strings.TrimSpace(r.FormValue("title")),
					Author:        strings.TrimSpace(r.FormValue("author")),
					Body:          r.FormValue("body"),
					ImageURL:      strings.TrimSpace(r.FormValue("imageURL")),
					ShowTicker:    r.FormValue("showTicker") == "1",
					ShowBillboard: r.FormValue("showBillboard") == "1",
					Published:     r.FormValue("published") == "1",
					CorpID:        parseInt64(strings.TrimSpace(r.FormValue("corpID"))),
					Section:       newsSection(r.FormValue("section")),
				}
				if a.Title == "" {
					result = "Title is required."
				} else if a.ID == 0 {
					if id, err := evedb.InsertNews(s.db, a); err != nil {
						result = "Error: " + err.Error()
					} else {
						result = fmt.Sprintf("Created article #%d: %s", id, a.Title)
					}
				} else {
					if err := evedb.UpdateNews(s.db, a); err != nil {
						result = "Error: " + err.Error()
					} else {
						result = fmt.Sprintf("Saved article #%d: %s", a.ID, a.Title)
					}
				}
			case "delete":
				id := parseInt64(r.FormValue("newsID"))
				if err := evedb.DeleteNews(s.db, id); err != nil {
					result = "Error: " + err.Error()
				} else {
					result = fmt.Sprintf("Deleted article #%d", id)
				}
			case "deleteImage":
				name := r.FormValue("imageName")
				if newsImageNameRE.MatchString(name) {
					if err := os.Remove(filepath.Join(newsImagesDir, name)); err != nil {
						result = "Error: " + err.Error()
					} else {
						result = fmt.Sprintf("Deleted image %s", name)
					}
				} else {
					result = "Invalid image name."
				}
			}
		}
	}

	var edit *evedb.NewsArticle
	if idStr := r.URL.Query().Get("edit"); idStr != "" && r.Method == http.MethodGet {
		if a, err := evedb.GetNewsArticle(s.db, parseInt64(idStr)); err == nil {
			edit = a
		}
	}

	articles, err := evedb.GetAllNews(s.db)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "news"})
		return
	}

	// List uploaded images so authors can pick from what's on disk.
	var images []string
	if entries, err := os.ReadDir(newsImagesDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() && newsImageNameRE.MatchString(e.Name()) {
				images = append(images, e.Name())
			}
		}
	}

	corps, err := evedb.NewsCorpChoices(r.Context(), s.db)
	if err != nil {
		log.Printf("esp: NewsCorpChoices: %v", err)
	}
	data := map[string]any{
		"Articles": articles,
		"Edit":     edit,
		"Images":   images,
		"Sections": evedb.NewsSections,
		"Corps":    corps,
		"Result":   result,
		"Now":      time.Now(),
		"Page":     "news",
	}
	s.render(w, "news.html", data)
}

// newsSection keeps a story's section to one of The Scope's desks.
func newsSection(v string) string {
	for _, sec := range evedb.NewsSections {
		if sec == v {
			return sec
		}
	}
	return ""
}

// handleNewsImagePreview serves files from data/news-images/ to the admin UI
// (the game client fetches the same files from the news server's /img/ route).
func (s *Server) handleNewsImagePreview(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/news-image/")
	if !newsImageNameRE.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, filepath.Join("data", "news-images", name))
}

// handleNewsImageUpload saves an uploaded article image into data/news-images/
// and returns a status message.
func (s *Server) handleNewsImageUpload(r *http.Request, dir string) string {
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		return "Error: " + err.Error()
	}
	file, hdr, err := r.FormFile("imageFile")
	if err != nil {
		return "Error: no file selected."
	}
	defer file.Close()
	name := strings.ToLower(filepath.Base(hdr.Filename))
	name = strings.ReplaceAll(name, " ", "_")
	if !newsImageNameRE.MatchString(name) {
		return "Error: filename must be letters/digits/._- and end in .jpg .jpeg .png .gif or .bmp"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "Error: " + err.Error()
	}
	dst, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		return "Error: " + err.Error()
	}
	defer dst.Close()
	if _, err := io.Copy(dst, file); err != nil {
		return "Error: " + err.Error()
	}
	return fmt.Sprintf("Uploaded %s — reference it as /img/%s", name, name)
}

// ── Mission Template Management ─────────────────────────────────────────────

func (s *Server) handleMissions(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var result string

	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err == nil {
			name := strings.TrimSpace(r.FormValue("name"))
			mType := r.FormValue("missionType")
			level, _ := strconv.Atoi(r.FormValue("agentLevel"))
			if name == "" {
				result = "Mission name is required."
			} else {
				if mType == "" {
					mType = "kill"
				}
				if level < 1 {
					level = 1
				}
				tid, err := evedb.ESPCreateMissionTemplate(ctx, s.db, name, mType, level,
					r.FormValue("storyline") == "1")
				if err != nil {
					result = "Error: " + err.Error()
				} else {
					http.Redirect(w, r, "/mission/"+strconv.FormatInt(tid, 10), http.StatusSeeOther)
					return
				}
			}
		}
	}

	templates, err := evedb.ESPGetAllMissionTemplates(ctx, s.db)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "missions"})
		return
	}

	data := map[string]any{
		"Templates": templates,
		"Result":    result,
		"Page":      "missions",
	}
	s.render(w, "missions.html", data)
}

func (s *Server) handleMissionView(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 3 {
		http.NotFound(w, r)
		return
	}
	templateID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var result string

	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err == nil {
			action := r.FormValue("action")
			switch action {
			case "update_template":
				t := &evedb.ESPMissionTemplateDetail{TemplateID: templateID}
				t.MissionName = strings.TrimSpace(r.FormValue("missionName"))
				t.MissionType = r.FormValue("missionType")
				t.AgentLevel, _ = strconv.Atoi(r.FormValue("agentLevel"))
				t.Storyline = r.FormValue("storyline") == "1"
				t.BriefingText = r.FormValue("briefingText")
				t.ObjectiveText = nullStr(r.FormValue("objectiveText"))
				t.AcceptText = nullStr(r.FormValue("acceptText"))
				t.CompleteText = nullStr(r.FormValue("completeText"))
				t.FailText = nullStr(r.FormValue("failText"))
				t.DeclineText = nullStr(r.FormValue("declineText"))
				t.MoreInfoText = nullStr(r.FormValue("moreInfoText"))
				t.AcceptLabel = nullStr(r.FormValue("acceptLabel"))
				t.DeclineLabel = nullStr(r.FormValue("declineLabel"))
				t.ViewMissionLabel = nullStr(r.FormValue("viewMissionLabel"))
				t.MoreInfoLabel = nullStr(r.FormValue("moreInfoLabel"))
				t.CompleteLabel = nullStr(r.FormValue("completeLabel"))
				t.QuitMissionLabel = nullStr(r.FormValue("quitMissionLabel"))
				t.DungeonVID = nullInt64(r.FormValue("dungeonVID"))
				t.DungeonRoomIdx, _ = strconv.Atoi(r.FormValue("dungeonRoomIdx"))
				if t.DungeonRoomIdx < 1 {
					t.DungeonRoomIdx = 1
				}
				t.CompletionType = r.FormValue("completionType")
				if t.CompletionType == "" {
					t.CompletionType = "killAll"
				}
				t.TargetTypeID = nullInt64(r.FormValue("targetTypeID"))
				t.TargetCount, _ = strconv.Atoi(r.FormValue("targetCount"))
				if t.TargetCount < 1 {
					t.TargetCount = 1
				}
				t.KillItemTypeID = nullInt64(r.FormValue("killItemTypeID"))
				t.KillItemQty, _ = strconv.Atoi(r.FormValue("killItemQty"))
				if t.KillItemQty < 1 {
					t.KillItemQty = 1
				}
				t.KillItemDropoffMode = r.FormValue("killItemDropoffMode")
				if t.KillItemDropoffMode == "" {
					t.KillItemDropoffMode = "agent"
				}
				t.KillItemDropoffStationID = nullInt64(r.FormValue("killItemDropoffStationID"))
				t.EnemyFactionID = nullInt64(r.FormValue("enemyFactionID"))
				t.CourierTypeID = nullInt64(r.FormValue("courierTypeID"))
				t.CourierQty, _ = strconv.Atoi(r.FormValue("courierQty"))
				if t.CourierQty < 1 {
					t.CourierQty = 1
				}
				t.PickupMode = r.FormValue("pickupMode")
				if t.PickupMode == "" {
					t.PickupMode = "agent"
				}
				t.PickupStationID = nullInt64(r.FormValue("pickupStationID"))
				t.DropoffMode = r.FormValue("dropoffMode")
				if t.DropoffMode == "" {
					t.DropoffMode = "nearby"
				}
				t.DropoffStationID = nullInt64(r.FormValue("dropoffStationID"))
				t.TradeTypeID = nullInt64(r.FormValue("tradeTypeID"))
				t.TradeQty, _ = strconv.Atoi(r.FormValue("tradeQty"))
				if t.TradeQty < 1 {
					t.TradeQty = 1
				}
				t.MiningTypeID = nullInt64(r.FormValue("miningTypeID"))
				t.MiningQty, _ = strconv.Atoi(r.FormValue("miningQty"))
				if t.MiningQty < 1 {
					t.MiningQty = 1
				}
				t.MinSecurity = nullFloat64(r.FormValue("minSecurity"))
				t.MaxSecurity = nullFloat64(r.FormValue("maxSecurity"))
				t.BaseRewardISK, _ = strconv.ParseInt(r.FormValue("baseRewardISK"), 10, 64)
				t.BaseRewardLP, _ = strconv.Atoi(r.FormValue("baseRewardLP"))
				t.BaseBonusISK, _ = strconv.ParseInt(r.FormValue("baseBonusISK"), 10, 64)
				t.BaseRewardItemTypeID = nullInt64(r.FormValue("baseRewardItemTypeID"))
				t.BaseRewardItemQty, _ = strconv.Atoi(r.FormValue("baseRewardItemQty"))
				if t.BaseRewardItemQty < 1 {
					t.BaseRewardItemQty = 1
				}
				t.BonusRewardItemTypeID = nullInt64(r.FormValue("bonusRewardItemTypeID"))
				t.BonusRewardItemQty, _ = strconv.Atoi(r.FormValue("bonusRewardItemQty"))
				if t.BonusRewardItemQty < 1 {
					t.BonusRewardItemQty = 1
				}
				t.BonusTimeSec, _ = strconv.Atoi(r.FormValue("bonusTimeSec"))
				t.ExpiryDays, _ = strconv.Atoi(r.FormValue("expiryDays"))
				t.ExpiryDays = evedb.ClampMissionExpiryDays(t.ExpiryDays)
				t.StandingGain, _ = strconv.ParseFloat(r.FormValue("standingGain"), 64)
				t.CorpStandingGain, _ = strconv.ParseFloat(r.FormValue("corpStandingGain"), 64)
				t.FactionStandingGain, _ = strconv.ParseFloat(r.FormValue("factionStandingGain"), 64)
				t.StandingLoss, _ = strconv.ParseFloat(r.FormValue("standingLoss"), 64)
				t.FactionStandingLoss, _ = strconv.ParseFloat(r.FormValue("factionStandingLoss"), 64)
				// Faction filter: checkboxes → comma-separated string.
				factionIDs := r.Form["allowedFactions"]
				if len(factionIDs) > 0 {
					t.AllowedFactions = sql.NullString{String: strings.Join(factionIDs, ","), Valid: true}
				}
				t.NextMissionID = nullInt64(r.FormValue("nextMissionID"))
				t.Enabled = r.FormValue("enabled") == "1"

				if t.MissionName == "" {
					result = "Mission name is required."
				} else if err := evedb.ESPUpdateMissionTemplate(ctx, s.db, t); err != nil {
					result = "Error: " + err.Error()
				} else {
					result = "Template updated."
					// Missions already accepted from this template follow the new
					// expiry; the sweep fails any that are now past it.
					if n, err := evedb.RestampMissionExpiry(ctx, s.db, templateID); err != nil {
						result += " Expiry restamp failed: " + err.Error()
					} else if n > 0 {
						result += fmt.Sprintf(" Expiry re-dated on %d accepted mission(s).", n)
					}
				}

			case "delete_template":
				if err := evedb.ESPDeleteMissionTemplate(ctx, s.db, templateID); err != nil {
					result = "Error: " + err.Error()
				} else {
					http.Redirect(w, r, "/missions", http.StatusSeeOther)
					return
				}
			}
		}
	}

	detail, err := evedb.ESPGetMissionTemplate(ctx, s.db, templateID)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "missions"})
		return
	}
	dungeons, _ := evedb.ESPGetDungeonOptions(ctx, s.db)
	factions, _ := evedb.ESPGetAllFactions(ctx, s.db)
	allMissions, _ := evedb.ESPGetAllMissionTemplates(ctx, s.db)

	data := map[string]any{
		"Template":    detail,
		"Dungeons":    dungeons,
		"Factions":    factions,
		"AllMissions": allMissions,
		"Result":      result,
		"Page":        "missions",
	}
	s.render(w, "mission_view.html", data)
}

// handleAgentDistribution is the mission distribution editor: what mission type
// mix each agent division rolls, what that mix actually produces given the
// template inventory, and the weights themselves.
func (s *Server) handleAgentDistribution(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var result string

	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err == nil {
			if r.FormValue("action") == "storyline" {
				n, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("storylineThreshold")))
				if n < 1 {
					result = "Error: the storyline run must be at least 1 mission."
				} else if err := evedb.SetStorylineThreshold(ctx, s.db, n); err != nil {
					result = "Error: " + err.Error()
				} else {
					result = fmt.Sprintf("Storyline run set to %d consecutive missions for one faction.", n)
				}
				s.renderAgentDistribution(ctx, w, result)
				return
			}
			if r.FormValue("action") == "declines" {
				hours, err := strconv.Atoi(strings.TrimSpace(r.FormValue("declineHours")))
				switch {
				case err != nil || hours < 0 || hours > 720:
					result = "Error: the free-decline window must be 0 to 720 hours."
				default:
					if err := evedb.SetAgentDeclineHours(ctx, s.db, hours); err != nil {
						result = "Error: " + err.Error()
					} else if hours == 0 {
						result = "Free declines off — every decline costs the template's standing loss."
					} else {
						result = fmt.Sprintf("Free declines saved: one per agent every %d hours.", hours)
					}
				}
				s.renderAgentDistribution(ctx, w, result)
				return
			}
			if r.FormValue("action") == "cooldown" {
				capN, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("missionCap")))
				hours, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("cooldownHours")))
				switch {
				case capN < 0:
					result = "Error: the mission cap cannot be negative (0 disables the gate)."
				case hours < 1:
					result = "Error: the cooldown window must be at least 1 hour."
				default:
					if err := evedb.SetAgentMissionCap(ctx, s.db, capN); err != nil {
						result = "Error: " + err.Error()
					} else if err := evedb.SetAgentCooldownHours(ctx, s.db, hours); err != nil {
						result = "Error: " + err.Error()
					} else if capN == 0 {
						result = "Mission cap disabled — agents hand out unlimited missions."
					} else {
						result = fmt.Sprintf("Mission cap saved: %d missions per %d hours per agent, counted on accepts.", capN, hours)
					}
				}
				s.renderAgentDistribution(ctx, w, result)
				return
			}
			divID, _ := strconv.ParseInt(r.FormValue("divisionID"), 10, 64)
			// Negative weights would break RollMissionType's range walk: the
			// total shrinks while an earlier band still swallows the roll.
			kill := clampWeight(r.FormValue("killWeight"))
			courier := clampWeight(r.FormValue("courierWeight"))
			trade := clampWeight(r.FormValue("tradeWeight"))
			mining := clampWeight(r.FormValue("miningWeight"))
			switch {
			case divID <= 0:
				result = "Error: no division selected."
			case kill+courier+trade+mining == 0:
				result = "Error: all four weights are zero — every offer would be a kill mission. Give at least one type a weight."
			default:
				if err := evedb.ESPSetDivisionWeights(ctx, s.db, divID, kill, courier, trade, mining); err != nil {
					result = "Error: " + err.Error()
				} else {
					result = fmt.Sprintf("Division %d saved: kill %d, courier %d, trade %d, mining %d.",
						divID, kill, courier, trade, mining)
				}
			}
		}
	}

	s.renderAgentDistribution(ctx, w, result)
}

// renderAgentDistribution draws the page. Both POST paths (the per-division weights and the
// storyline run length) land here.
func (s *Server) renderAgentDistribution(ctx context.Context, w http.ResponseWriter, result string) {
	rows, summary, err := evedb.ESPGetMissionDistribution(ctx, s.db)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "agent-distribution"})
		return
	}
	storyline, err := evedb.ESPGetStorylineOverview(ctx, s.db)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "agent-distribution"})
		return
	}

	data := map[string]any{
		"Divisions":     rows,
		"Summary":       summary,
		"Storyline":     storyline,
		"MissionCap":    evedb.GetAgentMissionCap(ctx, s.db),
		"CooldownHours": evedb.GetAgentCooldownHours(ctx, s.db),
		"DeclineHours":  evedb.GetAgentDeclineHours(ctx, s.db),
		"Result":        result,
		"Page":          "agent-distribution",
	}
	s.render(w, "agent_distribution.html", data)
}

// clampWeight parses a weight field, flooring it at zero.
func clampWeight(v string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(v))
	if n < 0 {
		return 0
	}
	return n
}

// nullStr converts an empty string to sql.NullString{Valid:false}.
func nullStr(s string) sql.NullString {
	s = strings.TrimSpace(s)
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// nullInt64 converts a form string to sql.NullInt64; empty/0 → null.
func nullInt64(s string) sql.NullInt64 {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || v == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: v, Valid: true}
}

// nullFloat64 converts a form string to sql.NullFloat64; empty → null.
func nullFloat64(s string) sql.NullFloat64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return sql.NullFloat64{}
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: v, Valid: true}
}

// ── Agent LP-store offers editor ─────────────────────────────────────────────

// espOfferListRow is one offer on the list page with its availability collapsed to a phrase. An
// offer that narrows nothing reaches every agent.
type espOfferListRow struct {
	Offer *evedb.AgentOffer
	Scope string
}

// offerScopePhrase describes who presents an offer, in the order an author
// thinks about it: corporation narrows hardest, then faction, then division.
func offerScopePhrase(o *evedb.AgentOffer, corpNames map[int64]string) string {
	var parts []string
	if o.CorporationID != 0 {
		name := corpNames[o.CorporationID]
		if name == "" {
			name = "corp " + strconv.FormatInt(o.CorporationID, 10)
		}
		parts = append(parts, name)
	}
	if n := len(listIDs(o.Factions)); n > 0 {
		parts = append(parts, plural(n, "faction"))
	}
	if n := len(listIDs(o.Divisions)); n > 0 {
		parts = append(parts, plural(n, "division"))
	}
	if len(parts) == 0 {
		return "every agent"
	}
	return strings.Join(parts, " · ")
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

// listIDs splits a comma-separated allow-list, dropping blanks.
func listIDs(list string) []string {
	var out []string
	for _, p := range strings.Split(list, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// espOfferItemRow is an offer item with its type name resolved for display.
type espOfferItemRow struct {
	EntryID   int64
	TypeID    int64
	TypeName  string
	Quantity  int
	ExtraInfo string
}

func (s *Server) espOfferItemRows(ctx context.Context, items []evedb.OfferItem) []espOfferItemRow {
	rows := make([]espOfferItemRow, 0, len(items))
	for _, it := range items {
		name, _ := evedb.GetTypeName(ctx, s.db, it.TypeID)
		rows = append(rows, espOfferItemRow{
			EntryID: it.EntryID, TypeID: it.TypeID, TypeName: name,
			Quantity: it.Quantity, ExtraInfo: it.ExtraInfo,
		})
	}
	return rows
}

// handleOffers lists all offers and creates new ones.
func (s *Server) handleOffers(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var result string
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err == nil {
			if r.FormValue("action") == "notices" {
				pct, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("noticeChance")))
				days, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("noticeCooldown")))
				if err := evedb.SetOfferNoticeChancePct(ctx, s.db, pct); err != nil {
					result = "Error: " + err.Error()
				} else if err := evedb.SetOfferNoticeCooldownDays(ctx, s.db, days); err != nil {
					result = "Error: " + err.Error()
				} else {
					result = fmt.Sprintf("Notice sweep saved: %d%% per pilot every 30 minutes, %d-day cooldown per offer.",
						evedb.GetOfferNoticeChancePct(ctx, s.db), evedb.GetOfferNoticeCooldownDays(ctx, s.db))
				}
				s.renderOffers(ctx, w, result)
				return
			}
			name := strings.TrimSpace(r.FormValue("name"))
			if name == "" {
				result = "Offer name is required."
			} else if oid, err := evedb.CreateOffer(ctx, s.db, name); err != nil {
				result = "Error: " + err.Error()
			} else {
				http.Redirect(w, r, "/offer/"+strconv.FormatInt(oid, 10), http.StatusSeeOther)
				return
			}
		}
	}

	s.renderOffers(ctx, w, result)
}

// renderOffers draws the offer list. Both POST paths (creating an offer and saving the
// notice-sweep settings) land here.
func (s *Server) renderOffers(ctx context.Context, w http.ResponseWriter, result string) {
	offers, err := evedb.GetAllOffers(ctx, s.db)
	if err != nil {
		s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "offers"})
		return
	}
	corpNames := map[int64]string{}
	if corps, err := evedb.ESPGetAgentCorporations(ctx, s.db); err == nil {
		for _, c := range corps {
			corpNames[c.CorporationID] = c.CorpName
		}
	}
	rows := make([]espOfferListRow, 0, len(offers))
	for _, o := range offers {
		rows = append(rows, espOfferListRow{Offer: o, Scope: offerScopePhrase(o, corpNames)})
	}
	s.render(w, "offers.html", map[string]any{
		"Offers":         rows,
		"NoticeChance":   evedb.GetOfferNoticeChancePct(ctx, s.db),
		"NoticeCooldown": evedb.GetOfferNoticeCooldownDays(ctx, s.db),
		"Result":         result,
		"Page":           "offers",
	})
}

// handleOfferView edits a single offer and its reward/cost items.
func (s *Server) handleOfferView(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 3 {
		http.NotFound(w, r)
		return
	}
	offerID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var result string
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err == nil {
			switch r.FormValue("action") {
			case "update_offer":
				o := &evedb.AgentOffer{OfferID: offerID}
				o.OfferName = strings.TrimSpace(r.FormValue("offerName"))
				o.Description = r.FormValue("description")
				o.CorporationID, _ = strconv.ParseInt(r.FormValue("corporationID"), 10, 64)
				// Checkbox grids → comma-separated allow-lists. Nothing ticked
				// is the empty list, which every agent on that axis matches.
				o.Factions = strings.Join(r.Form["allowedFactions"], ",")
				o.Divisions = strings.Join(r.Form["allowedDivisions"], ",")
				o.LPCost, _ = strconv.Atoi(r.FormValue("lpCost"))
				o.ISKCost, _ = strconv.ParseInt(r.FormValue("iskCost"), 10, 64)
				o.MinStanding, _ = strconv.ParseFloat(r.FormValue("minStanding"), 64)
				o.ExpiresDays, _ = strconv.Atoi(r.FormValue("expiresDays"))
				o.Enabled = r.FormValue("enabled") == "1"
				if o.OfferName == "" {
					result = "Offer name is required."
				} else if err := evedb.UpdateOffer(ctx, s.db, o); err != nil {
					result = "Error: " + err.Error()
				} else {
					result = "Offer saved."
				}
			case "add_item":
				typeID, _ := strconv.ParseInt(r.FormValue("typeID"), 10, 64)
				qty, _ := strconv.Atoi(r.FormValue("quantity"))
				if qty < 1 {
					qty = 1
				}
				if typeID <= 0 {
					result = "Type ID is required."
				} else if err := evedb.AddOfferItem(ctx, s.db, offerID, r.FormValue("role"),
					typeID, qty, strings.TrimSpace(r.FormValue("extraInfo"))); err != nil {
					result = "Error: " + err.Error()
				} else {
					result = "Item added."
				}
			case "delete_item":
				entryID, _ := strconv.ParseInt(r.FormValue("entryID"), 10, 64)
				if err := evedb.DeleteOfferItem(ctx, s.db, entryID); err != nil {
					result = "Error: " + err.Error()
				} else {
					result = "Item deleted."
				}
			case "delete_offer":
				if err := evedb.DeleteOffer(ctx, s.db, offerID); err != nil {
					result = "Error: " + err.Error()
				} else {
					http.Redirect(w, r, "/offers", http.StatusSeeOther)
					return
				}
			}
		}
	}

	offer, err := evedb.GetOfferByID(ctx, s.db, offerID)
	if err != nil || offer == nil {
		http.NotFound(w, r)
		return
	}
	factions, _ := evedb.ESPGetAllFactions(ctx, s.db)
	divisions, _ := evedb.ESPGetNPCDivisions(ctx, s.db)
	corps, _ := evedb.ESPGetAgentCorporations(ctx, s.db)
	s.render(w, "offer_view.html", map[string]any{
		"Offer":     offer,
		"Rewards":   s.espOfferItemRows(ctx, offer.Rewards),
		"Costs":     s.espOfferItemRows(ctx, offer.Costs),
		"Factions":  factions,
		"Divisions": divisions,
		"Corps":     corps,
		"Result":    result,
		"Page":      "offers",
	})
}

// parseSolidForm reads the collision select: "" = follow the type default
// (clears an override), "1" solid, "0" flown through. Absent = no change, so a
// row or form without the control never touches the column.
func parseSolidForm(r *http.Request, fields *evedb.EditObjectFields) {
	v, ok := r.Form["solid"]
	if !ok || len(v) == 0 {
		return
	}
	switch strings.TrimSpace(v[0]) {
	case "":
		fields.SolidDefault = true
	case "1":
		t := true
		fields.Solid = &t
	case "0":
		f := false
		fields.Solid = &f
	}
}
