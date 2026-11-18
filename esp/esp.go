package esp

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/http/pprof"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/npc"
	"gurimoon/safego"
	"gurimoon/syncx"
)

//go:embed templates/*.html
var templateFS embed.FS

// Built 3D dungeon editor SPA (esp/editor). Rebuild with:
//
//	cd esp/editor && npm install && npm run build
//
//go:embed all:editor/dist
var editorFS embed.FS

// Static assets for admin pages (bundled NPC 3D viewer). Rebuild with:
//
//	cd esp/editor && npx esbuild npc-viewer-entry.js --bundle --minify --format=iife --outfile=../static/npc_viewer.js
//
//go:embed static
var staticFS embed.FS

// ModuleHandlers holds extra admin routes registered by modules (path →
// handler). Populate before esp.New is called; every handler is wrapped in
// the standard auth middleware. See modules/README.md.
var ModuleHandlers = map[string]http.HandlerFunc{}

// ModuleNavLink is one sidebar entry registered by a module. Page is the value the module's pages
// pass as "Page" in their render data; it drives the active-link highlight.
type ModuleNavLink struct {
	Label string
	Path  string
	Page  string
}

// ModuleNavSection is one sidebar category registered by a module. It renders
// identically to the built-in sections, after NPCs and above Log out (layout.html).
type ModuleNavSection struct {
	Title string
	Links []ModuleNavLink
}

// ModuleNav holds module sidebar sections. Populate before New.
var ModuleNav []ModuleNavSection

// ModulePages maps a page template name (e.g. "x_roster.html") to its source, which must contain
// {{define "content"}}. It is parsed with the panel layout and partials. Populate before New;
// modules cannot add to the embedded templates directory.
var ModulePages = map[string]string{}

// ModuleRender renders a ModulePages template through the panel layout. Set
// by New; nil until the ESP server exists.
var ModuleRender func(w http.ResponseWriter, page string, data any)

// moduleServerRoutes is for overlay modules that ADD files to this package
// and so can reach *Server's unexported helpers (auth middleware, render, db).
// Each runs once in New, after ModuleHandlers. Append from an init().
var moduleServerRoutes []func(s *Server, mux *http.ServeMux)

// Server is the ESP HTTP admin panel.
type Server struct {
	httpSrv      *http.Server
	db           *sql.DB
	sessions     *SessionRegistry
	tmpls        map[string]*template.Template
	startTime    time.Time
	minRole      int64
	iconsDir     string                    // path to extracted icons (empty if unavailable)
	portraitsDir string                    // path to client portrait cache (empty if unavailable)
	gidsDir      string                    // path to client rendered-type-icon cache (empty if unavailable)
	modelsDirs   []string                  // directories with converted .glb model files
	nebulasDir   string                    // path to converted nebula cubemap PNGs (optional)
	nebulaDDS    map[string]string         // nebula name → client DDS cubemap path
	nebulaCache  syncx.Map[string, []byte] // "name/face" → decoded PNG bytes
	logoLibDDS   map[int]string            // corp logo atlas number → client DDS path
	logoCache    syncx.Map[int64, []byte]  // shape graphicID → cropped PNG bytes

	allianceDDS       map[int]string            // alliance logo atlas number → client DDS path
	allianceTileCache syncx.Map[string, []byte] // "NN_T" tile name → cropped PNG bytes

	modelsMu sync.RWMutex // guards models
	models   *modelIndex  // .glb tree snapshot, see modelindex.go

	modelPipelineState // models composed from the client's .blue files, see modelbuild.go

	sessionKey []byte               // HMAC key for signed login cookies (see auth.go)
	authMu     sync.Mutex           //          guards revoked
	revoked    map[string]time.Time // logged-out cookie -> its expiry

	sciMu   sync.Mutex                 // guards sciMemo and sciAt
	sciMemo *evedb.ESPIndustryOverview // Science & Industry overview, see sciindustry.go
	sciAt   time.Time

	dashMu   sync.Mutex                  // guards dashMemo and dashAt
	dashMemo *evedb.ESPDashboardOverview // landing-page overview, see dashboard.go
	dashAt   time.Time
}

// New creates a new ESP server. Call Start() to begin serving.
func New(listenAddr string, db *sql.DB, sessions *SessionRegistry, startTime time.Time, minRole int64, clientPath string, modelsDirs []string) (*Server, error) {
	funcMap := template.FuncMap{
		"formatISK": func(v float64) string {
			// Simple ISK formatting with 2 decimal places.
			return formatISK(v)
		},
		// Compact ISK for narrow tiles: "18.33B ISK". Pair it with formatISK in a title.
		"shortISK": shortISK,
		"formatDuration": func(d time.Duration) string {
			days := int(d.Hours()) / 24
			hours := int(d.Hours()) % 24
			mins := int(d.Minutes()) % 60
			if days > 0 {
				return template.HTMLEscapeString(
					intToStr(days) + "d " + intToStr(hours) + "h " + intToStr(mins) + "m")
			}
			return template.HTMLEscapeString(intToStr(hours) + "h " + intToStr(mins) + "m")
		},
		"since": func(t time.Time) time.Duration {
			return time.Since(t)
		},
		"agentTypeName": func(id int64) string {
			names := map[int64]string{
				1: "Non-Agent", 2: "Basic Agent", 3: "Tutorial Agent",
				4: "Research Agent", 5: "CONCORD Agent",
				6: "Generic Storyline Agent", 7: "Storyline Agent",
				8: "Event Mission Agent", 9: "Factional Warfare Agent",
				10: "Epic Arc Agent", 11: "Aura Agent",
			}
			if n, ok := names[id]; ok {
				return n
			}
			return "Unknown (" + intToStr(int(id)) + ")"
		},
		"commas": func(v int64) string {
			return commaInt(v)
		},
		// Rounded thousands separators for a float that is conceptually a count (damage dealt).
		"commasf": func(v float64) string {
			return commaInt(int64(v + 0.5))
		},
		// Seconds as "3d 4h" / "2h 15m" / "45s": industry times are stored in seconds.
		"secs": formatSeconds,
		// True for the immobile dungeon groups that spawn as destructible entities; only these get the
		// Invuln toggle on Ball Setup.
		"isStructureGroup": npc.IsDestructibleStructure,
		"isCargoGroup":     npc.IsCargoStructure,
		"isSentryGroup":    npc.IsSentryStructure,
		"contains": func(s, substr string) bool {
			return strings.Contains(s, substr)
		},
		// Membership in a comma-separated ID allow-list. Substring contains would match division 1
		// against "11".
		"inList": func(list string, id int64) bool {
			want := strconv.FormatInt(id, 10)
			for _, part := range strings.Split(list, ",") {
				if strings.TrimSpace(part) == want {
					return true
				}
			}
			return false
		},
		"add": func(nums ...int) int {
			total := 0
			for _, n := range nums {
				total += n
			}
			return total
		},
		"sub": func(a, b int) int {
			return a - b
		},
		// Float arithmetic for the killboard's damage bars. Guarded division: a zero-damage killmail
		// would otherwise put a NaN width on the page.
		"divf": func(a, b float64) float64 {
			if b == 0 {
				return 0
			}
			return a / b
		},
		"mulf": func(a, b float64) float64 { return a * b },
		"printf": func(format string, a ...any) string {
			return fmt.Sprintf(format, a...)
		},
		"unixTime": func(sec int64) string {
			if sec == 0 {
				return "—"
			}
			return time.Unix(sec, 0).UTC().Format("2006-01-02 15:04 UTC")
		},
		"pct": func(f float64) string {
			// Fraction → percent, trimmed (0.05 → "5", 0.125 → "12.5").
			return strconv.FormatFloat(f*100, 'f', -1, 64)
		},
		"num": func(v float64) string {
			// Plain decimal for form inputs; %g would render 1000000 as "1e+06".
			return strconv.FormatFloat(v, 'f', -1, 64)
		},
		// Unit conversions for the Create NPC form, rounded so float noise (25.000000000000004)
		// does not reach an input.
		"msToSec":   func(ms float64) string { return formNum(ms / 1000) },
		"resistPct": func(resonance float64) string { return formNum((1 - resonance) * 100) },
		"fracPct":   func(f float64) string { return formNum(f * 100) },
		"sumf": func(vals ...float64) float64 {
			var t float64
			for _, v := range vals {
				t += v
			}
			return t
		},
		"filetimeRemaining": func(ft uint64) string {
			if ft == 0 {
				return ""
			}
			d := time.Until(evedb.FiletimeToTime(ft))
			if d <= 0 {
				return "due"
			}
			d = d.Round(time.Minute)
			days := int(d.Hours()) / 24
			hours := int(d.Hours()) % 24
			mins := int(d.Minutes()) % 60
			if days > 0 {
				return fmt.Sprintf("%dd %dh %dm", days, hours, mins)
			}
			if hours > 0 {
				return fmt.Sprintf("%dh %dm", hours, mins)
			}
			return fmt.Sprintf("%dm", mins)
		},
		"romanLevel": func(lvl int) string {
			r := [6]string{"", "I", "II", "III", "IV", "V"}
			if lvl < 0 || lvl > 5 {
				return ""
			}
			return r[lvl]
		},
		"moonClassName":  moonClassName,
		"moonClassColor": moonClassColor,
		"iconPath": func(icon string) string {
			// Normalize icon field: could be "09_07" or "res:/ui/Texture/Icons/09_07"
			if i := strings.LastIndex(icon, "/"); i >= 0 {
				icon = icon[i+1:]
			}
			if i := strings.LastIndex(icon, "\\"); i >= 0 {
				icon = icon[i+1:]
			}
			return "/icons/" + icon + ".png"
		},
		// Module sidebar sections for the layout template.
		"moduleNav": func() []ModuleNavSection { return ModuleNav },
		// Panel colour scheme variables for the layout template.
		"themeCSS": themeCSS,
	}

	// Parse each page template independently with the layout so that each
	// gets its own "content" block definition (they all define "content").
	layoutBytes, err := templateFS.ReadFile("templates/layout.html")
	if err != nil {
		return nil, err
	}
	// Shared blocks available to every page (the NPC authoring form is used
	// by both the create and edit pages).
	partialBytes, err := templateFS.ReadFile("templates/partials.html")
	if err != nil {
		return nil, err
	}
	pages := []string{
		"dashboard.html", "accounts.html",
		"characters.html", "character_view.html", "account_view.html",
		"corporations.html", "corporation.html", "alliances.html", "alliance_view.html",
		"wars.html", "killboard.html", "killmail_view.html",
		"stations.html", "station_view.html", "items.html", "console.html", "error.html",
		"dungeons.html", "dungeon_view.html", "dungeon_items.html",
		"npcs.html", "npc_view.html", "npc_new.html",
		"agents.html", "agent_view.html", "agent_create.html",
		"agent_distribution.html", "agent_services.html", "agent_dialogue.html", "rnd.html",
		"region_spawns.html", "region_spawn_view.html", "spawner_rules.html", "dungeon_spawners.html",
		"industry_belts.html", "industry_moons.html", "industry_moon.html",
		"blueprints.html", "blueprint_view.html",
		"factories.html", "factory_station.html",
		"missions.html", "mission_view.html",
		"offers.html", "offer_view.html",
		"news.html", "forum.html",
		"market_supply.html", "contraband.html",
		"bugs.html", "bug_view.html",
		"petitions.html", "petition_view.html",
		"loot.html", "loot_group.html", "loot_npc.html",
		"couriers.html", "escrows.html",
		"sov.html", "sov_region.html", "outposts.html", "starbases.html",
		"solarsystems.html", "solarsystem_view.html", "stargates.html",
		"skills.html", "content_packages.html",
		"coverage.html", "theme.html",
	}
	tmpls := make(map[string]*template.Template, len(pages))
	for _, page := range pages {
		pageBytes, err := templateFS.ReadFile("templates/" + page)
		if err != nil {
			return nil, err
		}
		t, err := template.New(page).Funcs(funcMap).Parse(string(layoutBytes))
		if err != nil {
			return nil, err
		}
		if _, err := t.Parse(string(partialBytes)); err != nil {
			return nil, err
		}
		if _, err := t.Parse(string(pageBytes)); err != nil {
			return nil, err
		}
		tmpls[page] = t
	}
	// Module pages parse with the same layout + partials.
	for page, src := range ModulePages {
		if _, dup := tmpls[page]; dup {
			return nil, fmt.Errorf("esp: module page %q collides with a built-in page", page)
		}
		t, err := template.New(page).Funcs(funcMap).Parse(string(layoutBytes))
		if err != nil {
			return nil, err
		}
		if _, err := t.Parse(string(partialBytes)); err != nil {
			return nil, err
		}
		if _, err := t.Parse(src); err != nil {
			return nil, fmt.Errorf("esp: module page %q: %w", page, err)
		}
		tmpls[page] = t
	}

	// Check if icons directory exists under the client path.
	var iconsDir string
	if clientPath != "" {
		candidate := filepath.Join(clientPath, "icons")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			iconsDir = candidate
		}
	}

	// Check for the client's rendered portrait cache (character/agent portraits).
	var portraitsDir string
	if clientPath != "" {
		candidate := filepath.Join(clientPath, "cache", "Pictures", "Portraits")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			portraitsDir = candidate
		}
	}

	// Check for the client's rendered type-icon cache (snapshots keyed by graphicID, shown where no
	// 2D icon exists).
	var gidsDir string
	if clientPath != "" {
		candidate := filepath.Join(clientPath, "cache", "Pictures", "Gids")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			gidsDir = candidate
		}
	}

	// Validate model directories; keep only ones that exist. Non-nil so the
	// models status API encodes "roots" as [] rather than null — the editor
	// calls roots.join() and blanks out on null (e.g. Docker, no modelsDirs).
	validModelDirs := []string{}
	for _, d := range modelsDirs {
		if d == "" {
			continue
		}
		if info, err := os.Stat(d); err == nil && info.IsDir() {
			validModelDirs = append(validModelDirs, d)
		}
	}

	// Detect nebulas directory as sibling of first model dir.
	var nebulasDir string
	if len(validModelDirs) > 0 {
		candidate := filepath.Join(filepath.Dir(validModelDirs[0]), "nebulas")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			nebulasDir = candidate
		}
	}

	// Discover nebula DDS cubemaps in the client folder; they are served decoded on demand.
	nebulaDDS := discoverNebulaDDS(clientPath)

	// Corporation logo shape atlases: the picker on /corporation/<id> crops thumbnails from them
	// (logolib.go).
	logoLibDDS := discoverLogoLibDDS(clientPath)

	// Alliance logo atlases: the logo picker and create form crop thumbnails from them
	// (alliancetile.go).
	allianceDDS := discoverAllianceDDS(clientPath)

	s := &Server{
		db:           db,
		sessions:     sessions,
		tmpls:        tmpls,
		startTime:    startTime,
		minRole:      minRole,
		iconsDir:     iconsDir,
		portraitsDir: portraitsDir,
		gidsDir:      gidsDir,
		modelsDirs:   validModelDirs,
		nebulasDir:   nebulasDir,
		nebulaDDS:    nebulaDDS,
		logoLibDDS:   logoLibDDS,
		allianceDDS:  allianceDDS,
		sessionKey:   loadSessionKey(),
		revoked:      make(map[string]time.Time),
	}
	s.initModelPipeline(clientPath)

	mux := http.NewServeMux()

	// Go's runtime profiler, behind the panel's own auth. The sub-paths are registered individually
	// because pprof.Index does not reach profile, symbol, trace or cmdline.
	if PprofEnabled {
		mux.HandleFunc("/debug/pprof/", s.authMiddleware(pprof.Index))
		mux.HandleFunc("/debug/pprof/cmdline", s.authMiddleware(pprof.Cmdline))
		mux.HandleFunc("/debug/pprof/profile", s.authMiddleware(pprof.Profile))
		mux.HandleFunc("/debug/pprof/symbol", s.authMiddleware(pprof.Symbol))
		mux.HandleFunc("/debug/pprof/trace", s.authMiddleware(pprof.Trace))
	}

	if iconsDir != "" {
		mux.Handle("/icons/", http.StripPrefix("/icons/", http.FileServer(http.Dir(iconsDir))))
		log.Printf("ESP serving item icons from %s", iconsDir)
	}
	// Portraits come from the server's own store first and the client cache second, so these routes
	// work on a host with no client installed.
	mux.HandleFunc("/portrait/", s.authMiddleware(s.handlePortrait))
	mux.HandleFunc("/portrait/ingest", s.authMiddleware(s.handlePortraitIngest))
	mux.HandleFunc("/portrait/sync", s.authMiddleware(s.handlePortraitSync))
	log.Printf("ESP portrait store: %s", portraitStoreDir)
	if portraitsDir != "" {
		log.Printf("ESP portrait fallback (client cache): %s", portraitsDir)
	}
	mux.HandleFunc("/typeicon/", s.authMiddleware(s.handleTypeIcon))
	if gidsDir != "" {
		log.Printf("ESP serving rendered type icons from %s", gidsDir)
	}
	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))
	if len(validModelDirs) > 0 {
		// Serve static files from first model dir for backward compat.
		mux.Handle("/models/", http.StripPrefix("/models/", http.FileServer(http.Dir(validModelDirs[0]))))
		for _, d := range validModelDirs {
			log.Printf("ESP serving 3D models from %s", d)
		}
		// Index the tree up front so the first picker request is not a walk.
		mi := s.rescanModels()
		log.Printf("ESP indexed %d .glb models across %d directories", mi.count, len(mi.byDir))
	}
	if nebulasDir != "" || len(nebulaDDS) > 0 {
		mux.HandleFunc("/nebulas/", s.handleNebulaFile)
		if nebulasDir != "" {
			log.Printf("ESP serving nebula skyboxes from %s", nebulasDir)
		}
		if len(nebulaDDS) > 0 {
			log.Printf("ESP serving %d nebula skyboxes decoded from client DDS cubemaps", len(nebulaDDS))
		}
	}
	// 3D dungeon editor SPA, the embedded production build (esp/editor/dist).
	editorDist, err := fs.Sub(editorFS, "editor/dist")
	if err != nil {
		return nil, err
	}
	editorHandler := http.StripPrefix("/editor/", http.FileServer(http.FS(editorDist)))
	mux.HandleFunc("/editor", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/editor/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/editor/", s.authMiddleware(editorHandler.ServeHTTP))
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/logout", s.handleLogout)
	mux.HandleFunc("/", s.authMiddleware(s.handleDashboard))
	// Sessions are folded into the dashboard; the old path redirects.
	mux.HandleFunc("/sessions", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/accounts", s.authMiddleware(s.handleAccounts))
	mux.HandleFunc("/account/", s.authMiddleware(s.handleAccountView))
	mux.HandleFunc("/characters", s.authMiddleware(s.handleCharacterSearch))
	mux.HandleFunc("/character/", s.authMiddleware(s.handleCharacterView))
	mux.HandleFunc("/skillwindow/", s.authMiddleware(s.handleSkillWindow))
	mux.HandleFunc("/corps", s.authMiddleware(s.handleCorporations))
	mux.HandleFunc("/corporation/", s.authMiddleware(s.handleCorporationView))
	mux.HandleFunc("/alliances", s.authMiddleware(s.handleAlliances))
	mux.HandleFunc("/wars", s.authMiddleware(s.handleWars))
	mux.HandleFunc("/couriers", s.authMiddleware(s.handleCouriers))
	mux.HandleFunc("/escrows", s.authMiddleware(s.handleEscrows))
	mux.HandleFunc("/killboard", s.authMiddleware(s.handleKillboard))
	mux.HandleFunc("/killmail/", s.authMiddleware(s.handleKillmailView))
	mux.HandleFunc("/alliance/", s.authMiddleware(s.handleAllianceView))
	// Alliance logos are the server's own store, so these exist with or
	// without a client install; the shape atlases need one.
	mux.HandleFunc("/alliancelogo/", s.authMiddleware(s.handleAllianceLogo))
	mux.HandleFunc("/alliancelogo/ingest", s.authMiddleware(s.handleAllianceLogoIngest))
	if len(allianceDDS) > 0 {
		mux.HandleFunc("/alliancetile/", s.authMiddleware(s.handleAllianceTile))
		log.Printf("ESP serving alliance logo tiles from %d client atlas file(s)", len(allianceDDS))
	}
	if len(logoLibDDS) > 0 {
		mux.HandleFunc("/logoshape/", s.authMiddleware(s.handleLogoShape))
		log.Printf("ESP serving corporation logo shapes from %d client atlas file(s)", len(logoLibDDS))
	}
	mux.HandleFunc("/stations", s.authMiddleware(s.handleStations))
	mux.HandleFunc("/station/", s.authMiddleware(s.handleStationView))
	mux.HandleFunc("/items", s.authMiddleware(s.handleItemSearch))
	mux.HandleFunc("/console", s.authMiddleware(s.handleConsole))
	mux.HandleFunc("/theme", s.authMiddleware(s.handleTheme))
	mux.HandleFunc("/coverage", s.authMiddleware(s.handleCoverage))
	mux.HandleFunc("/coverage/export.json", s.authMiddleware(s.handleCoverageExport))
	mux.HandleFunc("/dungeons", s.authMiddleware(s.handleDungeons))
	mux.HandleFunc("/dungeons/object/api", s.authMiddleware(s.handleDungeonObjectAPI))
	mux.HandleFunc("/dungeons/loot/api", s.authMiddleware(s.handleDungeonLootAPI))
	mux.HandleFunc("/dungeon/", s.authMiddleware(s.handleDungeonView))
	mux.HandleFunc("/dungeons/items/api", s.authMiddleware(s.handleDungeonItemsAPI))
	mux.HandleFunc("/dungeons/items", s.authMiddleware(s.handleDungeonItems))
	mux.HandleFunc("/npcs", s.authMiddleware(s.handleNPCs))
	mux.HandleFunc("/npcs/new", s.authMiddleware(s.handleNPCNew))
	mux.HandleFunc("/npcs/graphics/api", s.authMiddleware(s.handleGraphicsAPI))
	mux.HandleFunc("/npcs/missiles/api", s.authMiddleware(s.handleMissileTypesAPI))
	mux.HandleFunc("/npc/", s.authMiddleware(s.handleNPCView))
	mux.HandleFunc("/gidicon/", s.authMiddleware(s.handleGidIcon))
	mux.HandleFunc("/api/graphicmodel/", s.authMiddleware(s.handleGraphicModel))
	mux.HandleFunc("/api/models/rescan", s.authMiddleware(s.handleModelsRescan))
	mux.HandleFunc("/api/models/status", s.authMiddleware(s.handleModelsStatus))
	mux.HandleFunc("/agents", s.authMiddleware(s.handleAgents))
	mux.HandleFunc("/agents/create", s.authMiddleware(s.handleAgentCreate))
	mux.HandleFunc("/agents/stations/api", s.authMiddleware(s.handleAgentStationsAPI))
	mux.HandleFunc("/agents/corpstations/api", s.authMiddleware(s.handleAgentCorpStationsAPI))
	mux.HandleFunc("/agents/appearance/api", s.authMiddleware(s.handleAgentAppearanceOptsAPI))
	mux.HandleFunc("/agents/scramble", s.authMiddleware(s.handleAgentScramblePortraits))
	mux.HandleFunc("/agents/scramble-ceos", s.authMiddleware(s.handleCEOScramblePortraits))
	mux.HandleFunc("/agent/", s.authMiddleware(s.handleAgentView))
	mux.HandleFunc("/agents/distribution", s.authMiddleware(s.handleAgentDistribution))
	mux.HandleFunc("/agents/services", s.authMiddleware(s.handleAgentServices))
	mux.HandleFunc("/agents/dialogue", s.authMiddleware(s.handleAgentDialogue))
	mux.HandleFunc("/rnd", s.authMiddleware(s.handleRnD))
	mux.HandleFunc("/region-spawns", s.authMiddleware(s.handleRegionSpawns))
	mux.HandleFunc("/region-spawns/", s.authMiddleware(s.handleRegionSpawnView))
	mux.HandleFunc("/region-spawns/npctypes/api", s.authMiddleware(s.handleSpawnerNPCTypesAPI))
	mux.HandleFunc("/spawner-rules", s.authMiddleware(s.handleSpawnerRules))
	mux.HandleFunc("/dungeon-spawners", s.authMiddleware(s.handleDungeonSpawners))
	mux.HandleFunc("/content/packages", s.authMiddleware(s.handleContentPackages))
	mux.HandleFunc("/systems/api", s.authMiddleware(s.handleSystemsAPI))
	mux.HandleFunc("/industry/belts", s.authMiddleware(s.handleIndustryBelts))
	mux.HandleFunc("/industry/moons", s.authMiddleware(s.handleIndustryMoons))
	mux.HandleFunc("/industry/moon/", s.authMiddleware(s.handleIndustryMoon))
	mux.HandleFunc("/industry/blueprints", s.authMiddleware(s.handleBlueprints))
	mux.HandleFunc("/industry/blueprint/", s.authMiddleware(s.handleBlueprintView))
	mux.HandleFunc("/industry/types/api", s.authMiddleware(s.handleItemTypesAPI))
	mux.HandleFunc("/industry/factories", s.authMiddleware(s.handleFactories))
	mux.HandleFunc("/industry/factories/group", s.authMiddleware(s.handleFactoryGroup))
	mux.HandleFunc("/industry/factory/", s.authMiddleware(s.handleFactoryStation))
	mux.HandleFunc("/missions", s.authMiddleware(s.handleMissions))
	mux.HandleFunc("/mission/", s.authMiddleware(s.handleMissionView))
	// Old home of the weights editor, now the Agents → Mission Distribution page.
	mux.HandleFunc("/missions/weights", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/agents/distribution", http.StatusFound)
	})
	mux.HandleFunc("/offers", s.authMiddleware(s.handleOffers))
	mux.HandleFunc("/offer/", s.authMiddleware(s.handleOfferView))
	// Module-registered admin pages (populated before New via esp.ModuleHandlers).
	for path, h := range ModuleHandlers {
		mux.HandleFunc(path, s.authMiddleware(h))
	}
	for _, add := range moduleServerRoutes {
		add(s, mux)
	}
	mux.HandleFunc("/bugs", s.authMiddleware(s.handleBugs))
	mux.HandleFunc("/bug/", s.authMiddleware(s.handleBugView))
	mux.HandleFunc("/petitions", s.authMiddleware(s.handlePetitions))
	mux.HandleFunc("/petition/", s.authMiddleware(s.handlePetitionView))
	mux.HandleFunc("/solarsystems", s.authMiddleware(s.handleSolarSystems))
	mux.HandleFunc("/solarsystems/api", s.authMiddleware(s.handleSolarSystemsAPI))
	mux.HandleFunc("/solarsystem/", s.authMiddleware(s.handleSolarSystemView))
	mux.HandleFunc("/stargates", s.authMiddleware(s.handleStargates))
	mux.HandleFunc("/outposts", s.authMiddleware(s.handleOutposts))
	mux.HandleFunc("/starbases", s.authMiddleware(s.handleStarbases))
	mux.HandleFunc("/sov", s.authMiddleware(s.handleSov))
	mux.HandleFunc("/sov/region/", s.authMiddleware(s.handleSovRegion))
	mux.HandleFunc("/sov/systems/api", s.authMiddleware(s.handleSovSystemsAPI))
	mux.HandleFunc("/loot", s.authMiddleware(s.handleLoot))
	mux.HandleFunc("/loot/group/", s.authMiddleware(s.handleLootGroup))
	mux.HandleFunc("/loot/npc/", s.authMiddleware(s.handleLootNPC))
	mux.HandleFunc("/loot/items/api", s.authMiddleware(s.handleLootItemsAPI))
	mux.HandleFunc("/loot/groups/api", s.authMiddleware(s.handleLootGroupsAPI))
	mux.HandleFunc("/news", s.authMiddleware(s.handleNews))
	mux.HandleFunc("/forum", s.authMiddleware(s.handleForum))
	mux.HandleFunc("/forum/thread/", s.authMiddleware(s.handleForum))
	mux.HandleFunc("/news-image/", s.authMiddleware(s.handleNewsImagePreview))
	mux.HandleFunc("/market/supply", s.authMiddleware(s.handleMarketSupply))
	mux.HandleFunc("/market/contraband", s.authMiddleware(s.handleContraband))

	// Dungeon editor JSON API (used by Three.js editor)
	mux.HandleFunc("/api/dungeon-factions", s.authMiddleware(s.handleAPIDungeonFactions))
	mux.HandleFunc("/api/dungeons", s.authMiddleware(s.handleAPIDungeonCreate))
	mux.HandleFunc("/api/dungeon/", s.authMiddleware(s.handleAPIDungeonDetail))
	mux.HandleFunc("/api/objects", s.authMiddleware(s.handleAPIObjectCreate))
	mux.HandleFunc("/api/objects/batch", s.authMiddleware(s.handleAPIObjectBatch))
	mux.HandleFunc("/api/objects/snapshot", s.authMiddleware(s.handleAPIObjectSnapshot))
	mux.HandleFunc("/api/objects/", s.authMiddleware(s.handleAPIObject))
	mux.HandleFunc("/api/stances", s.authMiddleware(s.handleAPIStances))
	mux.HandleFunc("/api/npc-factions", s.authMiddleware(s.handleAPINPCFactions))
	mux.HandleFunc("/api/prefabs", s.authMiddleware(s.handleAPIPrefabs))
	mux.HandleFunc("/api/prefabs/", s.authMiddleware(s.handleAPIPrefabs))
	mux.HandleFunc("/api/types/search", s.authMiddleware(s.handleAPITypeSearch))
	mux.HandleFunc("/api/rooms", s.authMiddleware(s.handleAPIRoomCreate))
	mux.HandleFunc("/api/rooms/reorder", s.authMiddleware(s.handleAPIRoomReorder))
	mux.HandleFunc("/api/rooms/", s.authMiddleware(s.handleAPIRoom))
	mux.HandleFunc("/api/model/debug", s.authMiddleware(s.handleModelDebug))
	mux.HandleFunc("/api/model/", s.authMiddleware(s.handleAPIModel))
	mux.HandleFunc("/api/nebulas", s.authMiddleware(s.handleAPINebulas))

	s.httpSrv = &http.Server{
		Addr:    listenAddr,
		Handler: hostGuard(listenAddr, csrfGuard(igbGuard(mux))),
		// A stalled client must not hold an fd + goroutine forever. Write
		// timeout is generous for large .glb model downloads.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Minute,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	// Expose the panel renderer to module handlers.
	ModuleRender = func(w http.ResponseWriter, page string, data any) {
		s.render(w, page, data)
	}
	return s, nil
}

// Start begins serving HTTP in a background goroutine.
func (s *Server) Start() {
	go func() {
		defer safego.Recover("Start")
		log.Printf("ESP admin panel listening on %s", s.httpSrv.Addr)
		if err := s.httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("ESP server error: %v", err)
		}
	}()
}

// Shutdown gracefully stops the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpSrv.Shutdown(ctx)
}

// authMiddleware accepts either a login session cookie (set by /login) or
// HTTP Basic Auth validated against the EVE account database. Requests with
// neither are sent to the login form; API calls get a bare 401 instead so
// fetch() callers don't receive an HTML page.
func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if as, ok := s.cookieSession(r); ok {
			// Sliding expiry: refresh the cookie once it is past halfway so
			// an active admin is never logged out mid-session.
			if time.Until(as.expires) < SessionTTL/2 {
				as.expires = time.Now().Add(SessionTTL)
				s.setSessionCookie(w, as)
			}
			next(w, r)
			return
		}
		user, pass, ok := r.BasicAuth()
		if ok {
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			_, role, err := evedb.DoLogin(ctx, s.db, user, pass, false, 0)
			if err == nil {
				if !s.roleAllowed(role) {
					http.Error(w, "Forbidden: insufficient role", http.StatusForbidden)
					return
				}
				next(w, r)
				return
			}
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	}
}

// render executes a named page template with the given data.
func (s *Server) render(w http.ResponseWriter, name string, data any) {
	t, ok := s.tmpls[name]
	if !ok {
		log.Printf("ESP template %q not found", name)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, "layout", data); err != nil {
		log.Printf("ESP template %q: %v", name, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

// commaInt formats an integer with thousands separators.
func commaInt(v int64) string {
	negative := v < 0
	if negative {
		v = -v
	}
	s := intToStr(int(v))
	result := ""
	for i, ch := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result += ","
		}
		result += string(ch)
	}
	if negative {
		result = "-" + result
	}
	return result
}

// formatISK formats a float64 ISK value with commas and 2 decimal places.
func formatISK(v float64) string {
	// Format the integer part with commas, then append decimals.
	negative := v < 0
	if negative {
		v = -v
	}
	intPart := int64(v)
	frac := v - float64(intPart)

	result := commaInt(intPart)

	// Append 2 decimal places.
	d := int(frac*100 + 0.5)
	result += "." + intToStr(d/10) + intToStr(d%10)

	if negative {
		result = "-" + result
	}
	return result + " ISK"
}

// shortISK formats an ISK value with a k/M/B/T suffix and two decimals; below a thousand it is
// whole ISK.
func shortISK(v float64) string {
	sign := ""
	if v < 0 {
		sign, v = "-", -v
	}
	for _, u := range []struct {
		div    float64
		suffix string
	}{{1e12, "T"}, {1e9, "B"}, {1e6, "M"}, {1e3, "k"}} {
		if v >= u.div {
			return sign + strconv.FormatFloat(v/u.div, 'f', 2, 64) + u.suffix + " ISK"
		}
	}
	return sign + strconv.FormatFloat(v, 'f', 0, 64) + " ISK"
}

func intToStr(n int) string {
	if n < 0 {
		return "-" + intToStr(-n)
	}
	if n < 10 {
		return string(rune('0' + n))
	}
	return intToStr(n/10) + string(rune('0'+n%10))
}
