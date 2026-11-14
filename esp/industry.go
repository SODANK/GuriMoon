package esp

// Industry > Asteroid Belts. Ore and ice distribution is decided in destiny.GenerateAsteroids
// from the system's security and sovereign NPC faction. This page shows the distribution table
// read-only, plus a belt survey from running the real generator over a scratch copy of the
// system.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/rates"
)

// beltOreRow is one ore family in the system's distribution table.
type beltOreRow struct {
	Family    string
	Weight    int
	Pct       float64
	Tier      string
	Volume    float64 // m³ per unit
	BaseName  string
	ConcName  string
	DenseName string
	BaseID    int64
	Note      string // why this ore is here (or gated)
}

// beltIceRow is one ice type the system can spawn.
type beltIceRow struct {
	TypeID  int64
	Name    string
	Weight  int
	Pct     float64
	UnitMin int64
	UnitMax int64
	VolMin  int64 // m³ per formation
	VolMax  int64
}

// beltDepositRow is one 0.0 deposit cluster type the system's truesec allows.
type beltDepositRow struct {
	Name string
	Ores string
}

// surveyBelt is one belt's line in the survey.
type surveyBelt struct {
	Index   int
	Kind    string
	Rocks   int
	Units   int64
	Volume  float64
	Biggest string
	TopOres string
}

// surveyOre aggregates one ore/ice family across the whole survey.
type surveyOre struct {
	Family   string
	Rocks    int
	Units    int64
	Volume   float64
	Refined  string // "Tritanium 12,000 · Pyerite 3,400"
	AvgUnits int64
}

// mineralTotal is one refined mineral across the whole survey.
type mineralTotal struct {
	Name   string
	Amount int64
}

// beltToolData is everything the belt tool renders for one system.
type beltToolData struct {
	System   *evedb.ESPSystemSearchRow
	SecLabel string
	SecColor string
	SecClass string
	Band     string

	FactionID   int64
	FactionName string
	Bloc        string

	Ore      []beltOreRow
	Ice      []beltIceRow
	NoIce    bool
	IceNote  string
	Deposits []beltDepositRow

	Belts       []surveyBelt
	Ores        []surveyOre
	Minerals    []mineralTotal
	BeltsSimmed int
	TotalRocks  int
	TotalUnits  int64
	TotalVolume float64
	IceBelts    int

	MiningRate float64
	Warnings   []string
}

// handleIndustryBelts serves /industry/belts.
func (s *Server) handleIndustryBelts(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.handleBeltRegen(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	query := strings.TrimSpace(r.URL.Query().Get("system"))
	data := map[string]any{
		"Query": query,
		"Page":  "industry-belts",
		"Msg":   r.URL.Query().Get("msg"),
		"Err":   r.URL.Query().Get("err"),
	}

	// Field status: what is seeded, how much has expired, how much has been mined (three indexed
	// counts, read on every render).
	regenDays := evedb.AsteroidRegenDays(ctx, s.db)
	data["RegenDays"] = regenDays
	data["RespawnMinutes"] = evedb.AsteroidRespawnMinutes(ctx, s.db)
	if st, err := evedb.GetAsteroidStats(ctx, s.db, regenDays); err == nil {
		data["Stats"] = st
		if st.Oldest > 0 {
			data["OldestAge"] = humanAge(time.Since(time.Unix(st.Oldest, 0)))
			data["NewestAge"] = humanAge(time.Since(time.Unix(st.Newest, 0)))
		}
	}

	if query != "" {
		var matches []evedb.ESPSystemSearchRow
		if id, err := strconv.ParseInt(query, 10, 64); err == nil {
			if row, err := evedb.ESPGetSolarSystemInfo(ctx, s.db, id); err == nil && row != nil {
				matches = []evedb.ESPSystemSearchRow{*row}
			}
		} else {
			matches, _ = evedb.ESPSearchSolarSystems(ctx, s.db, query)
		}
		exact := -1
		for i := range matches {
			if strings.EqualFold(matches[i].SystemName, query) {
				exact = i
				break
			}
		}
		switch {
		case len(matches) == 1 || exact >= 0:
			idx := 0
			if exact >= 0 {
				idx = exact
			}
			data["Belts"] = s.buildBeltTool(ctx, &matches[idx])
		case len(matches) > 1:
			data["Matches"] = matches
		default:
			data["NotFound"] = true
		}
	}

	s.render(w, "industry_belts.html", data)
}

// maxSurveyBelts bounds the survey: a full 12-belt system is already ~2000
// rocks of collision settling, and nothing on the page needs more.
const maxSurveyBelts = 10

// buildBeltTool assembles the distribution tables and runs one belt survey.
func (s *Server) buildBeltTool(ctx context.Context, sys *evedb.ESPSystemSearchRow) *beltToolData {
	b := &beltToolData{System: sys, MiningRate: rates.MiningYield}
	b.SecLabel, b.SecColor = secBand(sys.Security)
	b.SecClass = string(destiny.SecurityClassFromStatus(sys.Security))
	b.Band = beltBandLabel(sys.Security)

	// Sovereignty decides the lowsec ore split and the faction ice.
	factionID, err := evedb.GetSystemSovFactionID(ctx, s.db, sys.SolarSystemID)
	if err == nil {
		b.FactionID = factionID
	}
	if b.FactionID != 0 {
		if name, err := evedb.GetChrFactionName(ctx, s.db, b.FactionID); err == nil {
			b.FactionName = name
		}
	}
	bloc := destiny.BlocForFaction(b.FactionID)
	b.Bloc = bloc.String()

	dist := destiny.OreDistribution(sys.Security, b.FactionID)
	iceTypes := destiny.IceTypesFor(sys.Security, b.FactionID)

	// Which belts are Ice Fields is the map's call, so the survey reads the
	// real celestial types rather than inventing belts of no type.
	beltTypes, err := evedb.ESPSystemBeltTypes(ctx, s.db, sys.SolarSystemID)
	if err != nil {
		b.Warnings = append(b.Warnings, "Belt celestial types unavailable: "+err.Error())
	}
	iceFields := 0
	for _, t := range beltTypes {
		if t == destiny.IceFieldTypeID {
			iceFields++
		}
	}

	// ── Collect every typeID the system can produce, for one DB round trip ──
	var typeIDs []int64
	for _, w := range dist {
		fam := destiny.OreFamilies[w.Name]
		typeIDs = append(typeIDs, fam.Base.TypeID, fam.Concentrated.TypeID, fam.Dense.TypeID)
	}
	for _, ice := range iceTypes {
		typeIDs = append(typeIDs, ice.TypeID)
	}
	for _, d := range destiny.EligibleDeposits(sys.Security) {
		for _, name := range d.Ores {
			fam := destiny.OreFamilies[name]
			typeIDs = append(typeIDs, fam.Base.TypeID, fam.Concentrated.TypeID, fam.Dense.TypeID)
		}
	}
	types, _ := evedb.ESPGetAsteroidTypes(ctx, s.db, typeIDs)
	yields, _ := evedb.ESPGetReprocessOutputs(ctx, s.db, typeIDs)
	nameOf := func(typeID int64) string {
		if t, ok := types[typeID]; ok && t.TypeName != "" {
			return t.TypeName
		}
		return "typeID " + strconv.FormatInt(typeID, 10)
	}

	// ── Distribution table ──
	total := 0
	for _, w := range dist {
		total += w.Weight
	}
	for _, w := range dist {
		fam := destiny.OreFamilies[w.Name]
		row := beltOreRow{
			Family:    w.Name,
			Weight:    w.Weight,
			Tier:      oreTierLabel(w.Name),
			Volume:    fam.UnitVolume,
			BaseID:    fam.Base.TypeID,
			BaseName:  nameOf(fam.Base.TypeID),
			ConcName:  nameOf(fam.Concentrated.TypeID),
			DenseName: nameOf(fam.Dense.TypeID),
			Note:      oreNote(w.Name, sys.Security, bloc),
		}
		if total > 0 {
			row.Pct = float64(w.Weight) / float64(total) * 100
		}
		b.Ore = append(b.Ore, row)
	}
	sort.SliceStable(b.Ore, func(i, j int) bool { return b.Ore[i].Weight > b.Ore[j].Weight })

	// ── Ice table ──
	if iceFields == 0 || len(iceTypes) == 0 {
		b.NoIce = true
		if iceFields == 0 {
			b.IceNote = "No Ice Field celestial in this system — ice spawns only in Ice Fields."
		} else {
			b.IceNote = "No ice types resolve for this system."
		}
	} else {
		iceTotal := 0
		for _, ice := range iceTypes {
			iceTotal += ice.Weight
		}
		for _, ice := range iceTypes {
			row := beltIceRow{
				TypeID:  ice.TypeID,
				Name:    nameOf(ice.TypeID),
				Weight:  ice.Weight,
				UnitMin: int64(ice.VolMin / 1000),
				UnitMax: int64(ice.VolMax / 1000),
				VolMin:  int64(ice.VolMin),
				VolMax:  int64(ice.VolMax),
			}
			if iceTotal > 0 {
				row.Pct = float64(ice.Weight) / float64(iceTotal) * 100
			}
			b.Ice = append(b.Ice, row)
		}
		sort.SliceStable(b.Ice, func(i, j int) bool { return b.Ice[i].Weight > b.Ice[j].Weight })
		verb := " are Ice Fields"
		if iceFields == 1 {
			verb = " is an Ice Field"
		}
		b.IceNote = strconv.Itoa(iceFields) + " of this system's belts" + verb + "; every other belt is ore."
	}

	// ── Deposits (0.0 only) ──
	for _, d := range destiny.EligibleDeposits(sys.Security) {
		b.Deposits = append(b.Deposits, beltDepositRow{Name: d.Name, Ores: strings.Join(d.Ores, ", ")})
	}

	if sys.BeltCount == 0 {
		b.Warnings = append(b.Warnings,
			"This system has no asteroid belt celestials — nothing here spawns rocks, whatever the distribution says.")
		return b
	}

	// ── Belt survey: run the real generator over a scratch system ──
	nBelts := sys.BeltCount
	if nBelts > maxSurveyBelts {
		nBelts = maxSurveyBelts
		b.Warnings = append(b.Warnings,
			"Survey covers the first "+strconv.Itoa(maxSurveyBelts)+" of this system's "+
				strconv.Itoa(sys.BeltCount)+" belts.")
	}
	b.BeltsSimmed = nBelts

	scratch := &destiny.SolarSystem{Security: sys.Security, FactionID: b.FactionID}
	var celBalls []destiny.BallDesc
	var celSlims []destiny.SlimItem
	for i := 0; i < nBelts; i++ {
		id := uint32(1000 + i)
		// Belts sit 1e9 m apart so the fields cannot reach each other. A rock's belt is read from
		// scratch.AsteroidBelt, not recovered from X / spacing.
		celBalls = append(celBalls, destiny.BallDesc{EntityID: id, Pos: destiny.Vec3{X: float64(i) * 1e9}})
		slim := destiny.SlimItem{BallID: int64(id), ItemID: int64(id), GroupID: 9}
		if i < len(beltTypes) {
			slim.TypeID = beltTypes[i]
		}
		celSlims = append(celSlims, slim)
	}
	scratch.GenerateAsteroids(celBalls, celSlims)

	// Fetch names/volumes for anything the roll produced that we did not
	// pre-collect (deposit ores in an unusual band, say).
	var extra []int64
	for _, slim := range scratch.AsteroidSlims {
		if _, ok := types[slim.TypeID]; !ok {
			extra = append(extra, slim.TypeID)
		}
	}
	if len(extra) > 0 {
		if more, err := evedb.ESPGetAsteroidTypes(ctx, s.db, extra); err == nil {
			for id, t := range more {
				types[id] = t
			}
		}
		if more, err := evedb.ESPGetReprocessOutputs(ctx, s.db, extra); err == nil {
			for id, y := range more {
				yields[id] = y
			}
		}
	}

	type famAgg struct {
		rocks    int
		units    int64
		volume   float64
		minerals map[string]int64
	}
	byFamily := map[string]*famAgg{}
	byBelt := map[int]*surveyBelt{}
	beltFamilies := map[int]map[string]float64{}
	beltBiggest := map[int]float64{}
	minerals := map[string]int64{}

	for _, slim := range scratch.AsteroidSlims {
		qty := scratch.AsteroidQty[uint32(slim.ItemID)]
		if qty <= 0 {
			continue
		}
		beltIdx := int(scratch.AsteroidBelt[uint32(slim.ItemID)] - 1000)
		vol := float64(qty) * destiny.UnitVolume(slim.TypeID)

		family := "Ice"
		if slim.GroupID != destiny.GroupIce {
			family = familyOfType(slim.TypeID)
		}

		agg := byFamily[family]
		if agg == nil {
			agg = &famAgg{minerals: map[string]int64{}}
			byFamily[family] = agg
		}
		agg.rocks++
		agg.units += qty
		agg.volume += vol

		// Refined output: whole batches only, the way a refinery pays out.
		if t, ok := types[slim.TypeID]; ok && t.PortionSize > 0 {
			batches := qty / t.PortionSize
			for _, y := range yields[slim.TypeID] {
				name := y.TypeName
				if name == "" {
					name = "typeID " + strconv.FormatInt(y.TypeID, 10)
				}
				agg.minerals[name] += y.Amount * batches
				minerals[name] += y.Amount * batches
			}
		}

		sb := byBelt[beltIdx]
		if sb == nil {
			sb = &surveyBelt{Index: beltIdx + 1, Kind: "Ore field"}
			byBelt[beltIdx] = sb
			beltFamilies[beltIdx] = map[string]float64{}
		}
		if slim.GroupID == destiny.GroupIce {
			sb.Kind = "Ice field"
		}
		sb.Rocks++
		sb.Units += qty
		sb.Volume += vol
		beltFamilies[beltIdx][familyOrIce(slim, types)] += vol
		if vol > beltBiggest[beltIdx] {
			beltBiggest[beltIdx] = vol
			sb.Biggest = types[slim.TypeID].TypeName
		}

		b.TotalRocks++
		b.TotalUnits += qty
		b.TotalVolume += vol
	}

	for idx, sb := range byBelt {
		sb.TopOres = topShare(beltFamilies[idx], 3)
		if sb.Kind == "Ice field" {
			b.IceBelts++
		}
		b.Belts = append(b.Belts, *sb)
	}
	sort.Slice(b.Belts, func(i, j int) bool { return b.Belts[i].Index < b.Belts[j].Index })

	for name, agg := range byFamily {
		row := surveyOre{
			Family: name, Rocks: agg.rocks, Units: agg.units, Volume: agg.volume,
			Refined: topMinerals(agg.minerals, 4),
		}
		if agg.rocks > 0 {
			row.AvgUnits = agg.units / int64(agg.rocks)
		}
		b.Ores = append(b.Ores, row)
	}
	sort.Slice(b.Ores, func(i, j int) bool { return b.Ores[i].Volume > b.Ores[j].Volume })

	for name, amount := range minerals {
		b.Minerals = append(b.Minerals, mineralTotal{Name: name, Amount: amount})
	}
	sort.Slice(b.Minerals, func(i, j int) bool { return b.Minerals[i].Amount > b.Minerals[j].Amount })

	return b
}

// familyOfType maps an ore variant typeID back to its family name.
func familyOfType(typeID int64) string {
	base := destiny.BaseOreType(typeID)
	for name, fam := range destiny.OreFamilies {
		if fam.Base.TypeID == base {
			return name
		}
	}
	return "typeID " + strconv.FormatInt(typeID, 10)
}

// familyOrIce labels a rock for the per-belt breakdown: ore rocks by family,
// ice rocks by their own type name (a belt's ice mix is the interesting part).
func familyOrIce(slim destiny.SlimItem, types map[int64]evedb.ESPAsteroidType) string {
	if slim.GroupID == destiny.GroupIce {
		if t, ok := types[slim.TypeID]; ok && t.TypeName != "" {
			return t.TypeName
		}
		return "Ice"
	}
	return familyOfType(slim.TypeID)
}

// topShare renders the largest few entries of a name→m³ map as
// "Veldspar 62% · Scordite 21% · Omber 9%".
func topShare(m map[string]float64, n int) string {
	type kv struct {
		name string
		vol  float64
	}
	var all []kv
	total := 0.0
	for k, v := range m {
		all = append(all, kv{k, v})
		total += v
	}
	sort.Slice(all, func(i, j int) bool { return all[i].vol > all[j].vol })
	if total <= 0 {
		return ""
	}
	var parts []string
	for i, e := range all {
		if i >= n {
			break
		}
		parts = append(parts, e.name+" "+strconv.FormatInt(int64(e.vol/total*100+0.5), 10)+"%")
	}
	return strings.Join(parts, " · ")
}

// topMinerals renders the largest few entries of a name→amount map.
func topMinerals(m map[string]int64, n int) string {
	type kv struct {
		name   string
		amount int64
	}
	var all []kv
	for k, v := range m {
		all = append(all, kv{k, v})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].amount > all[j].amount })
	var parts []string
	for i, e := range all {
		if i >= n {
			break
		}
		parts = append(parts, e.name+" "+commaInt(e.amount))
	}
	return strings.Join(parts, " · ")
}

// beltBandLabel names the distribution band a security value falls in.
func beltBandLabel(sec float64) string {
	switch {
	case sec >= 0.95:
		return "1.0 — Veldspar/Scordite only"
	case sec >= 0.85:
		return "0.9 — highsec roster"
	case sec >= 0.65:
		return "0.7–0.8 — highsec roster"
	case sec >= 0.45:
		return "0.5–0.6 — Omber and Kernite arrive"
	case sec >= 0.35:
		return "0.4 — the Jaspet band"
	case sec > 0.0:
		return "0.3–0.1 — Hemorphite or Hedbergite"
	case sec > -0.25:
		return "shallow 0.0 — Gneiss country"
	case sec > -0.5:
		return "mid 0.0 — Crokite, Dark Ochre, Spodumain"
	case sec > -0.75:
		return "deep 0.0 — Bistot and Arkonor"
	default:
		return "deepest 0.0 — Mercoxit"
	}
}

// oreTierLabel is the rock-size tier an ore family sits in.
func oreTierLabel(family string) string {
	switch family {
	case "Veldspar", "Scordite":
		return "common"
	case "Pyroxeres", "Plagioclase", "Omber", "Kernite":
		return "uncommon"
	default:
		return "rare"
	}
}

// oreNote explains why an ore is in this system's table (the empire gates of the lowsec bands).
func oreNote(family string, sec float64, bloc destiny.OreBloc) string {
	switch family {
	case "Jaspet":
		if bloc == destiny.BlocNone {
			return "0.4 only; unclaimed space carries it at half weight"
		}
		return "0.4 only, Amarr and Gallente space"
	case "Hemorphite":
		if sec > 0.0 {
			if bloc == destiny.BlocNone {
				return "lowsec; unclaimed space carries both specials at half weight"
			}
			return "Amarr and Gallente lowsec — never alongside Hedbergite"
		}
	case "Hedbergite":
		if sec > 0.0 {
			if bloc == destiny.BlocNone {
				return "lowsec; unclaimed space carries both specials at half weight"
			}
			return "Caldari and Minmatar lowsec — never alongside Hemorphite"
		}
	case "Mercoxit":
		return "needs a deep core mining laser"
	}
	return ""
}

// humanAge renders a field age in the coarsest unit that still says something.
func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// handleBeltRegen re-rolls persisted asteroid fields. POST only with a confirm(), because it
// discards mined-down belts and a GET could be triggered by a link or prefetch. It writes seeds
// only (one UPDATE); each system rebuilds on its next load, and systems with players keep their
// current field until they empty.
func (s *Server) handleBeltRegen(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	sysArg := strings.TrimSpace(r.FormValue("system"))
	back := func(key, msg string) {
		dest := "/industry/belts?" + key + "=" + url.QueryEscape(msg)
		if sysArg != "" {
			dest = "/industry/belts?system=" + url.QueryEscape(sysArg) +
				"&" + key + "=" + url.QueryEscape(msg)
		}
		http.Redirect(w, r, dest, http.StatusSeeOther)
	}

	if err := r.ParseForm(); err != nil {
		back("err", "Bad form: "+err.Error())
		return
	}
	sysArg = strings.TrimSpace(r.FormValue("system"))
	action := r.FormValue("action")

	// The expiry travels with the regenerate form and is saved before the reseed, so the new seeds
	// are the first ones the new number governs.
	var daysNote string
	if raw := strings.TrimSpace(r.FormValue("days")); raw != "" {
		days, err := strconv.Atoi(raw)
		if err != nil || days < 0 {
			back("err", "Regeneration cycle must be a whole number of days (0 = never).")
			return
		}
		if days != evedb.AsteroidRegenDays(ctx, s.db) {
			if err := evedb.SetAsteroidRegenDays(ctx, s.db, days); err != nil {
				back("err", "Could not save the regeneration cycle: "+err.Error())
				return
			}
			if days == 0 {
				daysNote = " Automatic regeneration is now OFF — nothing will refill a mined belt."
			} else {
				daysNote = fmt.Sprintf(" Fields now regenerate every %d day(s).", days)
			}
		}
	}
	if raw := strings.TrimSpace(r.FormValue("respawnMinutes")); raw != "" {
		mins, err := strconv.Atoi(raw)
		if err != nil || mins < 0 {
			back("err", "Rock respawn must be a whole number of minutes (0 = off).")
			return
		}
		if err := evedb.SetAsteroidRespawnMinutes(ctx, s.db, mins); err != nil {
			back("err", "Could not save the respawn delay: "+err.Error())
			return
		}
	}

	switch action {
	case "settings":
		back("msg", "Saved."+daysNote)

	case "regen-one":
		systemID, err := strconv.ParseInt(sysArg, 10, 64)
		if err != nil || systemID == 0 {
			back("err", "Regenerating one system needs a system ID.")
			return
		}
		if err := evedb.ReseedAsteroidField(ctx, s.db, systemID); err != nil {
			back("err", "Regeneration failed: "+err.Error())
			return
		}
		back("msg", "System reseeded. Its new field appears the next time the system loads"+
			" — a system with players in it keeps the current one until they leave."+daysNote)

	case "regen-all":
		n, err := evedb.ReseedAllAsteroids(ctx, s.db, destiny.AsteroidGenVersion)
		if err != nil {
			back("err", "Regeneration failed: "+err.Error())
			return
		}
		loaded := 0
		if LoadedSystemCount != nil {
			loaded = LoadedSystemCount()
		}
		msg := fmt.Sprintf("Reseeded %d system(s) and cleared all mined-rock state.", n)
		if loaded > 0 {
			msg += fmt.Sprintf(" %d system(s) are loaded and keep their current field until they empty.", loaded)
		}
		back("msg", msg+daysNote)

	default:
		back("err", "Unknown action.")
	}
}
