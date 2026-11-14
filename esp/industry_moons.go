package esp

// Industry → Moons: what every moon in the galaxy holds for a Moon Harvesting Array, why
// (racial R8, constellation R16, rat-faction R32, uniform R64), and the levers: edit one moon,
// re-roll one moon, rescramble the galaxy.
//
// A rescramble rewrites every survey row and material line in one transaction, so it runs in the
// background and the page reports the last run. Moons under towers are re-rolled like any other:
// a harvester whose material vanished yields nothing until its owner re-points it, and the panel
// counts those.

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/safego"
)

func moonClassName(c int) string {
	if c < 0 || c >= len(evedb.MoonClassNames) {
		return "?"
	}
	return evedb.MoonClassNames[c]
}

// moonClassColor: grey gas, then green → gold → red as the class climbs.
func moonClassColor(c int) string {
	switch c {
	case evedb.MoonClassR4:
		return "#9ca3af"
	case evedb.MoonClassR8:
		return "#86efac"
	case evedb.MoonClassR16:
		return "#22c55e"
	case evedb.MoonClassR32:
		return "#fbbf24"
	case evedb.MoonClassR64:
		return "#f87171"
	}
	return "#6b7280"
}

// moonJob is the one background rescramble.
var moonJob struct {
	sync.Mutex
	running bool
	started time.Time
	last    string
	lastErr string
}

func moonJobState() (running bool, started time.Time, last, lastErr string) {
	moonJob.Lock()
	defer moonJob.Unlock()
	return moonJob.running, moonJob.started, moonJob.last, moonJob.lastErr
}

// handleIndustryMoons serves /industry/moons.
func (s *Server) handleIndustryMoons(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.handleMoonsPost(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	q := r.URL.Query()

	data := map[string]any{
		"Page":     "industry-moons",
		"Msg":      q.Get("msg"),
		"Err":      q.Get("err"),
		"View":     q.Get("view"),
		"Profiles": evedb.MoonProfileNames(),
		"Bands":    evedb.MoonBandNames,
		"Classes":  evedb.MoonClassNames,
	}
	running, started, last, lastErr := moonJobState()
	data["JobRunning"] = running
	if running {
		data["JobAge"] = humanAge(time.Since(started))
	}
	data["JobLast"] = last
	data["JobErr"] = lastErr

	if c, err := evedb.GetMoonCensus(ctx, s.db); err != nil {
		data["Err"] = "Moon census: " + err.Error()
	} else {
		data["Census"] = c
		if c.LastGen > 0 {
			data["LastGenAge"] = humanAge(time.Since(time.Unix(c.LastGen, 0)))
		}
		prof := evedb.MoonProfile(c.Settings.Profile)
		data["Profile"] = prof
		data["SeedStr"] = strconv.FormatUint(c.Settings.WorldSeed, 10)
	}
	if mats, err := evedb.MoonMaterials(ctx, s.db); err == nil {
		data["Materials"] = mats
	}
	if regions, err := evedb.ListMoonRegions(ctx, s.db); err == nil {
		data["Regions"] = regions
	}

	if q.Get("view") == "regions" {
		if m, err := evedb.GetMoonRegionMatrix(ctx, s.db); err != nil {
			data["Err"] = "Region matrix: " + err.Error()
		} else {
			data["RegionRows"] = m
		}
	}

	f := evedb.MoonListFilter{
		System:   strings.TrimSpace(q.Get("system")),
		MinClass: -1,
		Band:     -1,
		Towers:   q.Get("towers") == "1",
		Empty:    q.Get("empty") == "1",
	}
	f.RegionID, _ = strconv.ParseInt(q.Get("region"), 10, 64)
	f.TypeID, _ = strconv.ParseInt(q.Get("type"), 10, 64)
	if v, err := strconv.Atoi(q.Get("class")); err == nil {
		f.MinClass = v
	}
	if v, err := strconv.Atoi(q.Get("band")); err == nil {
		f.Band = v
	}
	f.Page, _ = strconv.Atoi(q.Get("page"))
	data["Filter"] = f
	if f.HasFilter() {
		rows, total, err := evedb.ListMoons(ctx, s.db, f)
		if err != nil {
			data["Err"] = "Moon list: " + err.Error()
		} else {
			data["Moons"] = rows
			data["Total"] = total
			pages := int((total + evedb.MoonListPageSize - 1) / evedb.MoonListPageSize)
			data["Pages"] = pages
			// Paging links keep every filter but the page.
			base := url.Values{}
			for k, v := range q {
				if k != "page" && k != "msg" && k != "err" && len(v) > 0 && v[0] != "" {
					base.Set(k, v[0])
				}
			}
			if f.Page > 0 {
				base.Set("page", strconv.Itoa(f.Page-1))
				data["PrevURL"] = "/industry/moons?" + base.Encode()
			}
			if f.Page+1 < pages {
				base.Set("page", strconv.Itoa(f.Page+1))
				data["NextURL"] = "/industry/moons?" + base.Encode()
			}
		}
	}

	s.render(w, "industry_moons.html", data)
}

// handleMoonsPost runs the global actions: save settings, rescramble.
func (s *Server) handleMoonsPost(w http.ResponseWriter, r *http.Request) {
	back := func(key, msg string) {
		http.Redirect(w, r, "/industry/moons?"+key+"="+url.QueryEscape(msg), http.StatusSeeOther)
	}
	if err := r.ParseForm(); err != nil {
		back("err", "Bad form: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	switch r.FormValue("action") {
	case "rescramble":
		if strings.TrimSpace(r.FormValue("confirm")) != "RESCRAMBLE" {
			back("err", "Type RESCRAMBLE to confirm — this re-rolls every moon in the game.")
			return
		}
		prof := r.FormValue("profile")
		if _, ok := evedb.MoonProfiles[prof]; !ok {
			back("err", "Unknown profile "+prof)
			return
		}
		seed := evedb.RandomMoonSeed()
		if raw := strings.TrimSpace(r.FormValue("seed")); raw != "" {
			v, err := strconv.ParseUint(raw, 10, 64)
			if err != nil {
				back("err", "The seed must be a whole number (or blank for a random one).")
				return
			}
			seed = v
		}
		keepEdited := r.FormValue("keepEdited") == "1"

		moonJob.Lock()
		if moonJob.running {
			moonJob.Unlock()
			back("err", "A rescramble is already running.")
			return
		}
		moonJob.running, moonJob.started, moonJob.lastErr = true, time.Now(), ""
		moonJob.Unlock()

		// Settings are saved before the run so the lazy fill and the bulk pass agree on what every moon
		// rolls while the transaction is in flight.
		if err := evedb.SetMoonGenSettings(ctx, s.db, evedb.MoonGenSettings{WorldSeed: seed, Profile: prof}); err != nil {
			moonJob.Lock()
			moonJob.running = false
			moonJob.Unlock()
			back("err", "Could not save the seed: "+err.Error())
			return
		}
		go s.runMoonRescramble(seed, prof, keepEdited)
		back("msg", fmt.Sprintf("Rescramble started: profile %s, seed %d%s. Refresh in a few seconds.",
			prof, seed, map[bool]string{true: ", hand-edited moons kept", false: ""}[keepEdited]))
	default:
		back("err", "Unknown action.")
	}
}

func (s *Server) runMoonRescramble(seed uint64, prof string, keepEdited bool) {
	defer safego.Recover("runMoonRescramble")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	rep, err := evedb.SeedAllMoons(ctx, s.db, evedb.MoonSeedOptions{KeepEdited: keepEdited})
	offPool, _ := evedb.MoonHarvestersOffPool(ctx, s.db)

	moonJob.Lock()
	defer moonJob.Unlock()
	moonJob.running = false
	if err != nil {
		moonJob.lastErr = err.Error()
		log.Printf("[esp] moon rescramble failed: %v", err)
		return
	}
	moonJob.last = fmt.Sprintf("%s — profile %s, seed %d: %s moons rolled (%s empty, %s material lines), %s kept, in %s.",
		time.Now().Format("2006-01-02 15:04"), prof, seed,
		commaInt(int64(rep.Moons)), commaInt(int64(rep.Empty)), commaInt(int64(rep.Resources)),
		commaInt(int64(rep.Skipped)), rep.Took.Round(time.Millisecond))
	if offPool > 0 {
		moonJob.last += fmt.Sprintf(" %d harvester(s) now point at a material their moon no longer holds and will yield nothing until re-set.", offPool)
	}
	log.Printf("[esp] moon rescramble: %s", moonJob.last)
}

// handleIndustryMoon serves /industry/moon/<moonID>.
func (s *Server) handleIndustryMoon(w http.ResponseWriter, r *http.Request) {
	idStr := strings.Trim(strings.TrimPrefix(r.URL.Path, "/industry/moon/"), "/")
	moonID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || moonID <= 0 {
		http.Redirect(w, r, "/industry/moons?err="+url.QueryEscape("Bad moon ID."), http.StatusSeeOther)
		return
	}
	self := fmt.Sprintf("/industry/moon/%d", moonID)
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	if r.Method == http.MethodPost {
		back := func(key, msg string) {
			http.Redirect(w, r, self+"?"+key+"="+url.QueryEscape(msg), http.StatusSeeOther)
		}
		if err := r.ParseForm(); err != nil {
			back("err", "Bad form: "+err.Error())
			return
		}
		typeID, _ := strconv.ParseInt(r.FormValue("typeID"), 10, 64)
		qty, _ := strconv.ParseInt(r.FormValue("quantity"), 10, 64)
		var aerr error
		var msg string
		switch r.FormValue("action") {
		case "set":
			aerr, msg = evedb.SetMoonResource(ctx, s.db, moonID, typeID, qty), "Material saved."
		case "delete":
			aerr, msg = evedb.DeleteMoonResource(ctx, s.db, moonID, typeID), "Material removed."
		case "reroll":
			aerr, msg = evedb.RerollMoon(ctx, s.db, moonID), "Moon re-rolled."
		case "restore":
			aerr, msg = evedb.RestoreMoon(ctx, s.db, moonID), "Moon restored to what the world seed generates."
		default:
			aerr = fmt.Errorf("unknown action")
		}
		if aerr != nil {
			back("err", aerr.Error())
			return
		}
		back("msg", msg)
		return
	}

	d, err := evedb.GetMoonDetail(ctx, s.db, moonID)
	if err != nil {
		http.Redirect(w, r, "/industry/moons?err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	data := map[string]any{
		"Page":   "industry-moons",
		"Msg":    r.URL.Query().Get("msg"),
		"Err":    r.URL.Query().Get("err"),
		"Moon":   d,
		"MaxQty": evedb.MoonMaxQuantity,
	}
	data["SecLabel"], data["SecColor"] = secBand(d.Security)
	if d.GeneratedAt > 0 {
		data["GenAge"] = humanAge(time.Since(time.Unix(d.GeneratedAt, 0)))
	}
	if mats, err := evedb.MoonMaterials(ctx, s.db); err == nil {
		data["Materials"] = mats
	}
	s.render(w, "industry_moon.html", data)
}
