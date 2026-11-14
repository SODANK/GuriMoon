package esp

// npcseed.go: the Create NPC authoring pages.
//
// An NPC is a destiny ball borrowing a ship hull: the invTypes row names it and points at a
// graphic, and dgmTypeAttributes supplies the tank, movement, weapon and AI numbers read at spawn.
// The handlers write both halves in one transaction via evedb.ESPSaveNPCSpec.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
	"gurimoon/npc"
)

// CacheInvalidator, when set before esp.New, is called after any write to invTypes or
// dgmTypeAttributes so the bulkdata cachables regenerate. main.go wires it to
// cacheStore.InvalidateTypeData.
var CacheInvalidator func()

func invalidateTypeCache() {
	if CacheInvalidator != nil {
		CacheInvalidator()
	}
}

// NPCTypeChanged, when set before esp.New, is called after any write to an NPC type's attributes
// so the running NPC manager drops its per-type caches (signature radius, EWAR loadout). main.go
// wires it to npcMgr.InvalidateTypeCaches.
var NPCTypeChanged func(typeID int64)

func npcTypeChanged(typeID int64) {
	if NPCTypeChanged != nil {
		NPCTypeChanged(typeID)
	}
}

// ── Model resolution ────────────────────────────────────────────────────────
//
// resolveGLB lives in modelindex.go and resolves against a cached snapshot of the converted .glb tree.

// handleGraphicsAPI serves the model catalogue for the picker.
//
// GET /npcs/graphics/api?q=&scope=ship|all
func (s *Server) handleGraphicsAPI(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	scope := r.URL.Query().Get("scope")
	if scope == "" {
		scope = "ship"
	}
	rows, err := evedb.ESPListGraphics(ctx, s.db, r.URL.Query().Get("q"), scope)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for i := range rows {
		rows[i].HasModel = s.hasModel(rows[i].URL3D)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rows)
}

// handleGraphicModel serves the converted .glb for a graphicID directly, so
// the model picker can preview a graphic that no type uses yet.
//
// GET /api/graphicmodel/{graphicID}.glb
func (s *Server) handleGraphicModel(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/graphicmodel/")
	trimmed = strings.TrimSuffix(trimmed, ".glb")
	graphicID, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	g, err := evedb.ESPGetGraphic(ctx, s.db, graphicID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.serveModel(w, r, g.URL3D)
}

// handleGidIcon serves the client's rendered snapshot for a graphicID, the picker's thumbnail.
// It reads the Gids cache handleTypeIcon reads, keyed by graphic so unused graphics preview.
//
// GET /gidicon/{graphicID}[.png][?size=N]
func (s *Server) handleGidIcon(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/gidicon/")
	trimmed = strings.TrimSuffix(trimmed, ".png")
	graphicID, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || graphicID <= 0 || s.gidsDir == "" {
		http.NotFound(w, r)
		return
	}
	size := int64(64)
	if v, err := strconv.ParseInt(r.URL.Query().Get("size"), 10, 64); err == nil && v > 0 {
		size = v
	}
	s.serveGidSnapshot(w, r, graphicID, size)
}

// ── Missile type search ─────────────────────────────────────────────────────

// handleMissileTypesAPI backs the missile picker on the weapon card.
//
// GET /npcs/missiles/api?q=
func (s *Server) handleMissileTypesAPI(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	rows, err := evedb.ESPSearchMissileTypes(ctx, s.db, r.URL.Query().Get("q"))
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rows)
}

// ── Form ⇄ spec ─────────────────────────────────────────────────────────────
//
// The Create NPC form shows people units (seconds, resist percent, chance percent) and
// specFromForm converts them to the attribute units NPCTypeSpec holds.

func formFloat(r *http.Request, name string) float64 {
	v, _ := strconv.ParseFloat(strings.TrimSpace(r.FormValue(name)), 64)
	return v
}

func formInt(r *http.Request, name string) int64 {
	v, _ := strconv.ParseInt(strings.TrimSpace(r.FormValue(name)), 10, 64)
	return v
}

// formSecToMs reads a seconds field as milliseconds.
func formSecToMs(r *http.Request, name string) float64 {
	return math.Round(formFloat(r, name) * 1000)
}

// specFromForm builds a spec from the authoring form. typeID 0 means create.
func specFromForm(r *http.Request, typeID int64) *evedb.NPCTypeSpec {
	s := &evedb.NPCTypeSpec{
		TypeID:          typeID,
		TypeName:        strings.TrimSpace(r.FormValue("typeName")),
		GroupID:         formInt(r, "groupID"),
		GraphicID:       formInt(r, "graphicID"),
		RaceID:          formInt(r, "raceID"),
		Description:     strings.TrimSpace(r.FormValue("description")),
		Mass:            formFloat(r, "mass"),
		Radius:          formFloat(r, "radius"),
		Volume:          formFloat(r, "volume"),
		Capacity:        formFloat(r, "capacity"),
		Published:       r.FormValue("published") != "",
		CloneFromTypeID: formInt(r, "cloneFrom"),

		CruiseSpeed: formFloat(r, "cruiseSpeed"),
		MaxVelocity: formFloat(r, "maxVelocity"),
		Agility:     formFloat(r, "agility"),

		ShieldHP:         formFloat(r, "shieldHP"),
		ArmorHP:          formFloat(r, "armorHP"),
		StructureHP:      formFloat(r, "structureHP"),
		ShieldRechargeMs: formSecToMs(r, "shieldRechargeSec"),

		WeaponKind:           r.FormValue("weaponKind"),
		Multiplier:           formFloat(r, "multiplier"),
		OptimalRange:         formFloat(r, "optimalRange"),
		Falloff:              formFloat(r, "falloff"),
		CycleTimeMs:          formSecToMs(r, "cycleTimeSec"),
		TrackingSpeed:        formFloat(r, "trackingSpeed"),
		TurretSlots:          formFloat(r, "turretSlots"),
		WeaponSigRes:         formFloat(r, "weaponSigRes"),
		MissileTypeID:        formInt(r, "missileTypeID"),
		MissileMult:          formFloat(r, "missileMult"),
		TurretTypeIDOverride: formInt(r, "turretTypeIDOverride"),

		SigRadius:      formFloat(r, "sigRadius"),
		SensorAttr:     formInt(r, "sensorAttr"),
		SensorStrength: formFloat(r, "sensorStrength"),

		FlyRange:         formFloat(r, "flyRange"),
		AttackRange:      formFloat(r, "attackRange"),
		ChaseMaxDist:     formFloat(r, "chaseMaxDist"),
		AttackDelayMinMs: formSecToMs(r, "attackDelayMinSec"),
		AttackDelayMaxMs: formSecToMs(r, "attackDelayMaxSec"),

		Bounty: formFloat(r, "bounty"),
	}
	if s.WeaponKind == "" {
		s.WeaponKind = "none"
	}

	// Damage: either a flavour split of a total, or four explicit values.
	if f, ok := evedb.NPCDamageFlavourByKey(r.FormValue("damageFlavour")); ok {
		total := formFloat(r, "damageTotal")
		s.DamageEM = f.EM * total
		s.DamageExp = f.Exp * total
		s.DamageKin = f.Kin * total
		s.DamageTherm = f.Therm * total
	} else {
		s.DamageEM = formFloat(r, "damageEM")
		s.DamageExp = formFloat(r, "damageExp")
		s.DamageKin = formFloat(r, "damageKin")
		s.DamageTherm = formFloat(r, "damageTherm")
	}

	// Resists arrive as percent; a blank is 0% (full damage). Validate rejects 100% and over.
	for i, layer := range []string{"shield", "armor", "hull"} {
		var vals [4]float64
		for j, dmg := range []string{"Em", "Exp", "Kin", "Therm"} {
			vals[j] = 1 - formFloat(r, layer+"Resist"+dmg)/100
		}
		switch i {
		case 0:
			s.ShieldRes = vals
		case 1:
			s.ArmorRes = vals
		case 2:
			s.HullRes = vals
		}
	}

	for i, k := range evedb.NPCEwarKinds() {
		p := "ewar_" + k.Key + "_"
		if r.FormValue(p+"on") == "" {
			continue
		}
		e := evedb.NPCEwarSpec{
			Chance: formFloat(r, p+"chance") / 100,
			Range:  formFloat(r, p+"range"),
		}
		if k.HasDuration {
			e.DurationMs = formSecToMs(r, p+"duration")
		}
		if k.HasFalloff {
			e.Falloff = formFloat(r, p+"falloff")
		}
		if k.EffectLabel != "" {
			e.Effect = k.EffectFromForm(formFloat(r, p+"effect"))
		}
		if e.Chance == 0 {
			// Ticked with a blank chance: keep the kind on so Validate reports it.
			e.Chance = -1
		}
		s.Ewar[i] = e
	}

	// Security status gain: blank leaves attr 252 absent so the fallback applies.
	if v := strings.TrimSpace(r.FormValue("secBonusPct")); v != "" {
		f, _ := strconv.ParseFloat(v, 64)
		s.SecStatusBonus, s.HasSecStatusBonus = f/100, true
	}

	return s
}

// damageFlavourOf names the flavour whose split the spec's damage matches, or "custom".
func damageFlavourOf(s *evedb.NPCTypeSpec) string {
	total := s.DamageEM + s.DamageExp + s.DamageKin + s.DamageTherm
	if total <= 0 {
		return "custom"
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-6 }
	for _, f := range evedb.NPCDamageFlavours() {
		if near(s.DamageEM/total, f.EM) && near(s.DamageExp/total, f.Exp) &&
			near(s.DamageKin/total, f.Kin) && near(s.DamageTherm/total, f.Therm) {
			return f.Key
		}
	}
	return "custom"
}

// ── Pages ───────────────────────────────────────────────────────────────────

// npcFormTab is one tab of the Create NPC form. Validation problems name tabs by Key.
type npcFormTab struct{ Key, Label string }

// npcFormTabs is the Create NPC tab order.
var npcFormTabs = []npcFormTab{
	{"start", "Start"}, {"identity", "Identity"}, {"model", "Model"},
	{"movement", "Movement"}, {"defence", "Defence"}, {"weapons", "Weapons"},
	{"ewar", "Electronic warfare"}, {"sensors", "Sensors"}, {"behaviour", "Behaviour"},
	{"rewards", "Rewards & spawning"}, {"review", "Review"},
}

// npcFormData assembles the shared context both the create and edit pages need. page is the
// layout's nav key.
func (s *Server) npcFormData(ctx context.Context, spec *evedb.NPCTypeSpec, result, page string) map[string]any {
	groups, _ := evedb.ESPGetNPCGroups(ctx, s.db)
	groupLoot, _ := evedb.ESPGroupLootSummaries(ctx, s.db)
	// Groups whose 0-speed members still spawn as live targets, for the Review warnings.
	structureGroups := []int64{}
	for _, g := range groups {
		if npc.IsDestructibleStructure(g.GroupID) {
			structureGroups = append(structureGroups, g.GroupID)
		}
	}

	data := map[string]any{
		"StructureGroups": structureGroups,
		"Archetype":       "",
		"Spec":            spec,
		"Groups":          groups,
		"GroupLoot":       groupLoot,
		"Archetypes":      evedb.NPCArchetypes(),
		"Flavours":        evedb.NPCDamageFlavours(),
		"Flavour":         damageFlavourOf(spec),
		"EwarKinds":       evedb.NPCEwarKinds(),
		"Tabs":            npcFormTabs,
		"ErrTabs":         map[string]bool{},
		"OpenTab":         "",
		"Result":          result,
		"HasModels":       len(s.modelsDirs) > 0,
		"HasIcons":        s.iconsDir != "",
		"Page":            page,
	}

	if spec.GraphicID > 0 {
		if g, err := evedb.ESPGetGraphic(ctx, s.db, spec.GraphicID); err == nil {
			g.HasModel = s.resolveGLB(g.URL3D) != ""
			data["Graphic"] = g
		}
	}
	ids := []int64{}
	if spec.MissileTypeID > 0 {
		ids = append(ids, spec.MissileTypeID)
	}
	if spec.CloneFromTypeID > 0 {
		ids = append(ids, spec.CloneFromTypeID)
	}
	if len(ids) > 0 {
		names := evedb.ESPGetTypeNames(ctx, s.db, ids)
		data["MissileName"] = names[spec.MissileTypeID]
		data["CloneName"] = names[spec.CloneFromTypeID]
	}
	return data
}

// addProblems puts a failed save's problems on the form and opens the first failing tab.
func addProblems(data map[string]any, err error) {
	var problems evedb.NPCSpecErrors
	if !errors.As(err, &problems) || len(problems) == 0 {
		data["Result"] = "Error: " + err.Error()
		return
	}
	errTabs := map[string]bool{}
	for _, p := range problems {
		errTabs[p.Tab] = true
	}
	data["Problems"] = problems
	data["ErrTabs"] = errTabs
	data["OpenTab"] = problems[0].Tab
	// The form's own problem list is the message; a second banner would repeat it.
	data["Result"] = ""
}

// handleNPCNew serves and processes the Create NPC form.
//
// GET  /npcs/new[?archetype=cruiser][&clone=TYPEID]
// POST /npcs/new
func (s *Server) handleNPCNew(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "npc-new"})
			return
		}
		spec := specFromForm(r, 0)
		newID, err := evedb.ESPSaveNPCSpec(ctx, s.db, spec)
		if err != nil {
			data := s.npcFormData(ctx, spec, "", "npc-new")
			data["IsNew"] = true
			data["Archetype"] = r.FormValue("archetype")
			if f := r.FormValue("damageFlavour"); f != "" {
				data["Flavour"] = f
			}
			addProblems(data, err)
			s.render(w, "npc_new.html", data)
			return
		}
		invalidateTypeCache()
		npcTypeChanged(newID)
		http.Redirect(w, r, "/npc/"+strconv.FormatInt(newID, 10)+"?created=1", http.StatusSeeOther)
		return
	}

	// Seed the form: clone of an existing type, or a hull archetype.
	var spec *evedb.NPCTypeSpec
	archetype := r.URL.Query().Get("archetype")
	if cloneID, _ := strconv.ParseInt(r.URL.Query().Get("clone"), 10, 64); cloneID > 0 {
		loaded, err := evedb.ESPLoadNPCSpec(ctx, s.db, cloneID)
		if err != nil {
			s.render(w, "error.html", map[string]any{"Error": err.Error(), "Page": "npc-new"})
			return
		}
		loaded.TypeID = 0
		loaded.TypeName = loaded.TypeName + " Copy"
		loaded.CloneFromTypeID = cloneID
		spec = loaded
		archetype = ""
	} else {
		if _, ok := evedb.NPCArchetypeByKey(archetype); !ok {
			archetype = "cruiser"
		}
		base, _ := evedb.NPCArchetypeByKey(archetype)
		spec = &base
	}

	data := s.npcFormData(ctx, spec, "", "npc-new")
	data["IsNew"] = true
	data["Archetype"] = archetype
	s.render(w, "npc_new.html", data)
}

// npcWeaponSummary describes what GetNPCWeaponProfile produces for a type: the values the combat
// loop uses after db.GetNPCWeaponProfile applies its defaults and floors.
type npcWeaponSummary struct {
	Unarmed        bool
	IsMissile      bool
	TurretTypeID   int64
	TurretTypeName string
	TurretSlots    int
	MissileName    string
	MissileRange   float64
	TotalDamage    float64
	DPS            float64
	EffectiveRange float64
	AttackRange    float64
	CycleTimeMs    int64
	Notes          []string
}

// buildWeaponSummary resolves the live weapon profile for display.
func (s *Server) buildWeaponSummary(ctx context.Context, typeID int64) *npcWeaponSummary {
	wp, err := evedb.GetNPCWeaponProfile(ctx, s.db, typeID)
	if err != nil || wp == nil {
		return &npcWeaponSummary{Unarmed: true}
	}

	sum := &npcWeaponSummary{
		IsMissile:      wp.MissileTypeID != 0,
		TurretTypeID:   wp.TurretTypeID,
		TurretSlots:    wp.TurretSlots,
		MissileRange:   wp.MissileRange,
		AttackRange:    wp.AttackRange,
		CycleTimeMs:    wp.CycleTimeMs,
		EffectiveRange: wp.OptimalRange,
	}
	sum.TotalDamage = (wp.Em + wp.Explosive + wp.Kinetic + wp.Thermal) * wp.Multiplier
	if wp.CycleTimeMs > 0 {
		sum.DPS = sum.TotalDamage / (float64(wp.CycleTimeMs) / 1000.0)
	}

	ids := []int64{}
	if wp.TurretTypeID != 0 {
		ids = append(ids, wp.TurretTypeID)
	}
	if wp.MissileTypeID != 0 {
		ids = append(ids, wp.MissileTypeID)
	}
	if len(ids) > 0 {
		names := evedb.ESPGetTypeNames(ctx, s.db, ids)
		sum.TurretTypeName = names[wp.TurretTypeID]
		sum.MissileName = names[wp.MissileTypeID]
	}

	// Surface the defaults and floors the loader silently applies, so the
	// author can see when an authored value was overridden.
	if minAttack := wp.OptimalRange + wp.Falloff*2; wp.AttackRange <= minAttack {
		sum.Notes = append(sum.Notes, fmt.Sprintf(
			"Attack range floored to optimal + 2×falloff (%.0f m) — an entityAttackRange below that would stop the NPC firing at ranges it can hit.",
			minAttack))
	}
	if sum.IsMissile && wp.MissileRange <= 0 {
		sum.Notes = append(sum.Notes,
			"Missile range resolved to 0 — the missile type is missing maxVelocity (37) or explosionDelay (281).")
	}
	if !sum.IsMissile && wp.TurretTypeID == 0 {
		sum.Notes = append(sum.Notes,
			"No turret model resolved — the client will render no hardpoint guns and combat FX fall back to the hull.")
	}
	if !sum.IsMissile && wp.TurretTypeID != 0 && wp.TurretOverridden {
		sum.Notes = append(sum.Notes,
			fmt.Sprintf("Turret typeID overridden to %d (attr 10001) — auto-derivation from damage profile bypassed.", wp.TurretTypeID))
	}
	if wp.TrackingSpeed <= 0 && !sum.IsMissile {
		sum.Notes = append(sum.Notes,
			"Tracking speed 0 — the tracking term drops out of the hit roll, so this NPC never misses due to transversal.")
	}
	return sum
}

// serveGidSnapshot is the shared Gids-cache lookup used by both the type-icon
// and graphic-icon endpoints.
func (s *Server) serveGidSnapshot(w http.ResponseWriter, r *http.Request, graphicID, want int64) {
	entries, err := os.ReadDir(s.gidsDir)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	prefix := strconv.FormatInt(graphicID, 10) + "_"
	var bestName string
	var bestDiff int64 = -1
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".dds") {
			continue
		}
		size, err := strconv.ParseInt(name[len(prefix):len(name)-len(".dds")], 10, 64)
		if err != nil {
			continue
		}
		diff := size - want
		if diff < 0 {
			diff = -diff
		}
		if bestDiff < 0 || diff < bestDiff {
			bestDiff = diff
			bestName = name
		}
	}
	if bestName == "" {
		http.NotFound(w, r)
		return
	}
	img, err := decodeUncompressedDDS(filepath.Join(s.gidsDir, bestName))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_ = png.Encode(w, img)
}

// formNum renders a number for a form input, rounded to 4 decimals.
func formNum(v float64) string {
	return strconv.FormatFloat(math.Round(v*1e4)/1e4, 'f', -1, 64)
}
