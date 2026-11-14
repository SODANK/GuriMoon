package contentpkg

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Limits on how much one package may hold.
const (
	maxDungeons       = 100
	maxRoomsPerDun    = 50
	maxObjectsPerDun  = 3000
	maxLootPerDun     = 5000
	maxTriggersPerDun = 1000
	maxMissions       = 500
	maxSpawners       = 100
	maxTypes          = 200
	maxAttrsPerType   = 400
	maxEffectsPerType = 100
	maxLootPerType    = 200
)

var (
	versionPattern = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._+-]{0,31}$`)
	filePattern    = regexp.MustCompile(`^(dungeons|missions|spawners|npcs)/[a-z0-9][a-z0-9_-]{0,79}\.json$`)
	idListPattern  = regexp.MustCompile(`^[1-9][0-9]{0,9}(,[1-9][0-9]{0,9})*$`)
	intPattern     = regexp.MustCompile(`^-?[0-9]{1,19}$`)
)

// kindFolder maps a content kind to its folder in the zip.
var kindFolder = map[string]string{"npc": "npcs", "dungeon": "dungeons", "mission": "missions", "spawner": "spawners"}

// Problems is the list of reasons a package was rejected.
type Problems []string

func (p Problems) Error() string {
	const show = 20
	if len(p) <= show {
		return "package rejected: " + strings.Join(p, "; ")
	}
	return fmt.Sprintf("package rejected: %s; and %d more", strings.Join(p[:show], "; "), len(p)-show)
}

type validator struct {
	problems Problems
	keys     map[string]*tableSpec // key -> table
	unit     map[string]string     // key -> key of its dungeon or custom type
	rooms    map[string]int        // dungeon key -> room count
}

func (v *validator) fail(format string, args ...any) {
	if len(v.problems) < 200 {
		v.problems = append(v.problems, fmt.Sprintf(format, args...))
	}
}

// validate checks every part of a package against the format and returns its key -> table map.
// Nothing outside the format passes: unknown columns, raw IDs where a package key belongs,
// out-of-range numbers, malformed text and references that leave their dungeon all reject.
func validate(p *Package) (map[string]*tableSpec, error) {
	v := &validator{keys: map[string]*tableSpec{}, unit: map[string]string{}, rooms: map[string]int{}}
	v.manifest(p)
	v.counts(p)
	if len(v.problems) > 0 {
		return nil, v.problems
	}

	// Keys first, so references can be checked in any order.
	for _, ct := range p.Types {
		tk := v.register(tType, ct.Type, "")
		for _, r := range ct.Loot {
			v.register(tNPCLoot, r, tk)
		}
	}
	for _, d := range p.Dungeons {
		dk := v.register(tDungeon, d.Dungeon, "")
		v.rooms[dk] = len(d.Rooms)
		for _, r := range d.Rooms {
			v.register(tRoom, r, dk)
		}
		for _, r := range d.Objects {
			v.register(tObject, r, dk)
		}
		for _, r := range d.Loot {
			v.register(tObjLoot, r, dk)
		}
		for _, r := range d.Triggers {
			v.register(tTrigger, r, dk)
		}
	}
	for _, r := range p.Missions {
		v.register(tMission, r, "")
	}
	for _, r := range p.Spawners {
		v.register(tSpawner, r, "")
	}
	if len(v.problems) > 0 {
		return nil, v.problems
	}

	for _, ct := range p.Types {
		tk := ct.Type.Key()
		v.row(tType, ct.Type, "", tk)
		attrs := map[int64]bool{}
		for _, r := range ct.Attributes {
			v.row(tAttr, r, tk, tk)
			id := intOf(r["attributeID"])
			if attrs[id] {
				v.fail("%s: attribute %d is listed twice", tk, id)
			}
			attrs[id] = true
			if r["valueInt"] == nil && r["valueFloat"] == nil {
				v.fail("%s: attribute %d has no value", tk, id)
			}
		}
		effects := map[int64]bool{}
		for _, r := range ct.Effects {
			v.row(tEffect, r, tk, tk)
			id := intOf(r["effectID"])
			if effects[id] {
				v.fail("%s: effect %d is listed twice", tk, id)
			}
			effects[id] = true
		}
		items := map[string]bool{}
		share := map[string]float64{}
		for _, r := range ct.Loot {
			v.row(tNPCLoot, r, tk, tk)
			v.qtyWindow(r)
			item := asString(r["itemTypeID"])
			if items[item] {
				v.fail("%s: loot item %s is listed twice", tk, item)
			}
			items[item] = true
			share[asString(r["requireRole"])] += floatOf(r["dropChance"])
		}
		for role, s := range share {
			if s > 1+1e-6 {
				v.fail("%s: loot shares of role %q add up to %.4f, more than 1", tk, role, s)
			}
		}
		if len(ct.LootCaps) > 1 {
			v.fail("%s: more than one loot cap row", tk)
		}
		for _, r := range ct.LootCaps {
			v.row(tNPCCap, r, tk, tk)
		}
	}
	for _, d := range p.Dungeons {
		dk := d.Dungeon.Key()
		v.row(tDungeon, d.Dungeon, "", dk)
		for _, r := range d.Rooms {
			v.row(tRoom, r, dk, dk)
		}
		for _, r := range d.Objects {
			v.row(tObject, r, dk, dk)
			if refKey(r["leashObjectID"]) == r.Key() {
				v.fail("%s: an object cannot be leashed to itself", r.Key())
			}
			if lo, hi := r["lootMinDrops"], r["lootMaxDrops"]; lo != nil && hi != nil && intOf(lo) > intOf(hi) {
				v.fail("%s: lootMinDrops is above lootMaxDrops", r.Key())
			}
		}
		pairs := map[string]bool{}
		for _, r := range d.Loot {
			v.row(tObjLoot, r, dk, dk)
			v.qtyWindow(r)
			pair := asString(r["objectID"]) + "/" + asString(r["itemTypeID"])
			if pairs[pair] {
				v.fail("%s: object %s lists item %s twice", r.Key(), asString(r["objectID"]), asString(r["itemTypeID"]))
			}
			pairs[pair] = true
		}
		for _, r := range d.Triggers {
			v.row(tTrigger, r, dk, dk)
			v.trigger(r)
		}
	}
	for _, r := range p.Missions {
		v.row(tMission, r, "", "")
		if refKey(r["nextMissionID"]) == r.Key() {
			v.fail("%s: a mission cannot chain to itself", r.Key())
		}
		if lo, hi := r["minSecurity"], r["maxSecurity"]; lo != nil && hi != nil && floatOf(lo) > floatOf(hi) {
			v.fail("%s: minSecurity is above maxSecurity", r.Key())
		}
		if dk := refKey(r["dungeonVID"]); dk != "" {
			if idx := intOf(r["dungeonRoomIdx"]); idx > int64(v.rooms[dk]) {
				v.fail("%s: dungeonRoomIdx %d is past the %d room(s) of %s", r.Key(), idx, v.rooms[dk], dk)
			}
		}
	}
	for _, r := range p.Spawners {
		v.row(tSpawner, r, "", "")
		lo, hi := -1.0, 1.0 // the column defaults
		if x, ok := r["secMin"]; ok {
			lo = floatOf(x)
		}
		if x, ok := r["secMax"]; ok {
			hi = floatOf(x)
		}
		if lo > hi {
			v.fail("%s: secMin is above secMax", r.Key())
		}
	}
	if len(v.problems) == 0 {
		v.contents(p)
		v.requires(p)
	}
	if len(v.problems) > 0 {
		return nil, v.problems
	}
	return v.keys, nil
}

// manifest checks the manifest's own fields.
func (v *validator) manifest(p *Package) {
	m := p.Manifest
	if m.Format != FormatVersion {
		v.fail("format %d is not supported; this server reads format %d", m.Format, FormatVersion)
	}
	if !validUUID(m.UUID) {
		v.fail("uuid %q is not a lower-case UUID", m.UUID)
	}
	v.plain("name", m.Name, 100, false, true)
	if !versionPattern.MatchString(m.Version) {
		v.fail("version %q may only use letters, digits and . _ + - (32 at most)", m.Version)
	}
	v.plain("author", m.Author, 100, false, false)
	v.plain("description", m.Description, 2000, true, false)
	v.plain("build", m.Build, 64, false, false)
	if _, err := time.Parse(time.RFC3339, m.Created); err != nil {
		v.fail("created %q is not an RFC 3339 time", m.Created)
	}
	if !utf8.ValidString(p.Readme) || strings.ContainsRune(p.Readme, 0) || len(p.Readme) > maxReadmeBytes {
		v.fail("README.md must be UTF-8 text of at most %d KB", maxReadmeBytes>>10)
	}
}

// plain checks a text field: valid UTF-8, no control characters (newlines and tabs only when
// multiline), at most size characters.
func (v *validator) plain(field, s string, size int, multiline, required bool) {
	if required && strings.TrimSpace(s) == "" {
		v.fail("%s is empty", field)
		return
	}
	if msg := textProblem(s, size, multiline); msg != "" {
		v.fail("%s %s", field, msg)
	}
}

func textProblem(s string, size int, multiline bool) string {
	if !utf8.ValidString(s) {
		return "is not valid UTF-8"
	}
	if n := utf8.RuneCountInString(s); n > size {
		return fmt.Sprintf("is %d characters, more than %d", n, size)
	}
	for _, c := range s {
		if multiline && (c == '\n' || c == '\r' || c == '\t') {
			continue
		}
		if unicode.IsControl(c) || c == 0x2028 || c == 0x2029 || c == 0xFEFF {
			return fmt.Sprintf("holds control character %U", c)
		}
	}
	return ""
}

// counts enforces the size limits.
func (v *validator) counts(p *Package) {
	limit := func(what string, n, max int) {
		if n > max {
			v.fail("%d %s, more than the %d allowed", n, what, max)
		}
	}
	if len(p.Dungeons)+len(p.Missions)+len(p.Spawners) == 0 {
		v.fail("holds no dungeon, mission or spawner")
	}
	limit("dungeons", len(p.Dungeons), maxDungeons)
	limit("missions", len(p.Missions), maxMissions)
	limit("spawners", len(p.Spawners), maxSpawners)
	limit("custom types", len(p.Types), maxTypes)
	for _, d := range p.Dungeons {
		name := d.Dungeon.Key()
		limit(name+" rooms", len(d.Rooms), maxRoomsPerDun)
		limit(name+" objects", len(d.Objects), maxObjectsPerDun)
		limit(name+" loot rows", len(d.Loot), maxLootPerDun)
		limit(name+" triggers", len(d.Triggers), maxTriggersPerDun)
		if len(d.Rooms) == 0 {
			v.fail("dungeon %s has no rooms", name)
		}
	}
	for _, ct := range p.Types {
		name := ct.Type.Key()
		limit(name+" attributes", len(ct.Attributes), maxAttrsPerType)
		limit(name+" effects", len(ct.Effects), maxEffectsPerType)
		limit(name+" loot rows", len(ct.Loot), maxLootPerType)
	}
}

// register records a keyed row's key.
func (v *validator) register(t *tableSpec, r Row, unit string) string {
	k, ok := r[KeyField].(string)
	switch {
	case !ok:
		v.fail("a %s row has no %s", t.name, KeyField)
		return ""
	case !keyPattern.MatchString(k) || k[:1] != t.prefix:
		v.fail("%s key %q is not of the form %s<number>", t.name, k, t.prefix)
		return ""
	case v.keys[k] != nil:
		v.fail("key %q is used twice", k)
		return ""
	}
	v.keys[k] = t
	if unit == "" {
		unit = k
	}
	v.unit[k] = unit
	return k
}

// row checks one row against its table's columns. parent is the key of the enclosing row and
// unit the key of the dungeon or custom type the row belongs to.
func (v *validator) row(t *tableSpec, r Row, parent, unit string) {
	where := t.name
	if k := r.Key(); k != "" {
		where += " " + k
	} else if parent != "" {
		where += " of " + parent
	}
	if _, has := r[KeyField]; has != (t.pk != "") {
		if t.pk == "" {
			v.fail("%s: child rows carry no %s", where, KeyField)
		}
		return
	}
	for col := range r {
		if col == KeyField {
			continue
		}
		if _, ok := t.col(col); !ok {
			v.fail("%s: column %q is not part of the format", where, col)
		}
	}
	for _, c := range t.cols {
		val, present := r[c.name]
		if !present {
			if c.required {
				v.fail("%s: %s is missing", where, c.name)
			}
			continue
		}
		if msg := v.value(c, val, parent, unit, r.Key()); msg != "" {
			v.fail("%s: %s %s", where, c.name, msg)
		}
	}
}

// value checks one column value and returns what is wrong with it, or "".
func (v *validator) value(c colDef, val any, parent, unit, self string) string {
	if val == nil {
		if c.null {
			return ""
		}
		return "may not be null"
	}
	switch c.kind {
	case kInt, kBool:
		n, ok := intValue(val)
		if !ok {
			return "must be a whole number"
		}
		lo, hi := c.min, c.max
		if c.kind == kBool {
			lo, hi = 0, 1
		}
		if float64(n) < lo || float64(n) > hi {
			return fmt.Sprintf("%d is outside %s..%s", n, fmtNum(lo), fmtNum(hi))
		}
	case kFloat:
		f, ok := floatValue(val)
		if !ok {
			return "must be a number"
		}
		if f < c.min || f > c.max {
			return fmt.Sprintf("%g is outside %s..%s", f, fmtNum(c.min), fmtNum(c.max))
		}
	case kText:
		s, ok := val.(string)
		if !ok {
			return "must be text"
		}
		if msg := textProblem(s, c.size, c.multiline); msg != "" {
			return msg
		}
		if c.required && strings.TrimSpace(s) == "" {
			return "is empty"
		}
		if c.check != nil && !c.check(s) {
			return fmt.Sprintf("%q is not in the expected form", s)
		}
	case kEnum:
		s, ok := val.(string)
		if !ok {
			return "must be text"
		}
		for _, e := range c.enum {
			if s == e {
				return ""
			}
		}
		return fmt.Sprintf("%q is not one of %s", s, strings.Join(c.enum, ", "))
	case kIDList:
		s, ok := val.(string)
		if !ok {
			return "must be text"
		}
		if len(s) > c.size || (s != "" && !idListPattern.MatchString(s)) {
			return "must be a comma-separated list of IDs"
		}
	case kType:
		if s, ok := val.(string); ok {
			k := refKey(s)
			if k == "" || v.keys[k] != tType {
				return fmt.Sprintf("%q is not a custom type of this package", s)
			}
			return ""
		}
		n, ok := intValue(val)
		if !ok || n < 1 || n >= CustomTypeFloor {
			return fmt.Sprintf("must be a base typeID below %d or a custom type key of this package", CustomTypeFloor)
		}
	case kRef:
		if s, ok := val.(string); ok {
			k := refKey(s)
			t := v.keys[k]
			switch {
			case k == "" || t == nil || t.prefix != c.ref:
				return fmt.Sprintf("%q is not a %s key of this package", s, prefixSpec(c.ref).name)
			case c.scope == scopeParent && k != parent:
				return fmt.Sprintf("must point at %s, its own parent", parent)
			case c.scope == scopeUnit && v.unit[k] != unit:
				return fmt.Sprintf("%q belongs to another dungeon", s)
			}
			return ""
		}
		if n, ok := intValue(val); ok && n == 0 && c.zeroNone {
			return ""
		}
		return "must be a package key (raw IDs are not allowed)"
	}
	return ""
}

// qtyWindow checks minQty <= maxQty on a loot row.
func (v *validator) qtyWindow(r Row) {
	if lo, hi := r["minQty"], r["maxQty"]; lo != nil && hi != nil && intOf(lo) > intOf(hi) {
		v.fail("%s: minQty is above maxQty", r.Key())
	}
}

// trigger checks the rules between a trigger's event, action and fields.
func (v *validator) trigger(r Row) {
	k := r.Key()
	event, action := asString(r["event"]), asString(r["action"])
	if action == "spawnWave" && intOf(r["spawnWave"]) < 1 {
		v.fail("%s: a spawnWave trigger needs a wave number of 1 or more", k)
	}
	if action == "music" && !musicURL.MatchString(asString(r["body"])) {
		v.fail("%s: a music trigger needs a res:/ track ending in .ogg, .wav or .mp3", k)
	}
	if event != "itemPlaced" {
		if r["itemTypeID"] != nil {
			v.fail("%s: only an itemPlaced trigger has an item", k)
		}
		if q, ok := r["itemQty"]; ok && intOf(q) != 1 {
			v.fail("%s: only an itemPlaced trigger has an item quantity", k)
		}
	}
}

// contents checks that the manifest lists exactly the package's items.
func (v *validator) contents(p *Package) {
	want := map[string]string{} // kind/key -> name
	for _, ct := range p.Types {
		want["npc/"+ct.Type.Key()] = asString(ct.Type["typeName"])
	}
	for _, d := range p.Dungeons {
		want["dungeon/"+d.Dungeon.Key()] = asString(d.Dungeon["dungeonVName"])
	}
	for _, r := range p.Missions {
		want["mission/"+r.Key()] = asString(r["missionName"])
	}
	for _, r := range p.Spawners {
		want["spawner/"+r.Key()] = asString(r["name"])
	}
	seen := map[string]bool{}
	files := map[string]bool{}
	for _, c := range p.Manifest.Contents {
		id := c.Kind + "/" + c.Key
		name, ok := want[id]
		switch {
		case !ok:
			v.fail("manifest lists %s, which the package does not hold", id)
		case seen[id]:
			v.fail("manifest lists %s twice", id)
		case c.Name != name:
			v.fail("manifest names %s %q but its file says %q", id, c.Name, name)
		case !filePattern.MatchString(c.File) || !strings.HasPrefix(c.File, kindFolder[c.Kind]+"/"):
			v.fail("manifest file name %q for %s is not allowed", c.File, id)
		case files[c.File]:
			v.fail("manifest uses file %s twice", c.File)
		}
		seen[id], files[c.File] = true, true
	}
	for id := range want {
		if !seen[id] {
			v.fail("manifest does not list %s", id)
		}
	}
}

// requires checks that the manifest's base-data list matches what the rows use, ID for ID.
func (v *validator) requires(p *Package) {
	need := computeRequires(p)
	check := func(kind string, have []BaseRef, want map[int64]bool) {
		got := map[int64]bool{}
		for _, r := range have {
			if got[r.ID] {
				v.fail("requires lists %s %d twice", kind, r.ID)
			}
			got[r.ID] = true
			if !want[r.ID] {
				v.fail("requires lists %s %d, which no row uses", kind, r.ID)
			}
			if msg := textProblem(r.Name, 255, false); msg != "" {
				v.fail("requires name of %s %d %s", kind, r.ID, msg)
			}
		}
		for id := range want {
			if !got[id] {
				v.fail("requires does not list %s %d, which a row uses", kind, id)
			}
		}
	}
	r := p.Manifest.Requires
	check("type", r.Types, need.types)
	check("group", r.Groups, need.groups)
	check("faction", r.Factions, need.factions)
	check("station", r.Stations, need.stations)
	check("region", r.Regions, need.regions)
}

// baseNeeds is the base data a package's rows refer to.
type baseNeeds struct {
	types, groups, factions, stations, regions map[int64]bool
}

// computeRequires collects every base-data ID the rows use.
func computeRequires(p *Package) baseNeeds {
	n := baseNeeds{map[int64]bool{}, map[int64]bool{}, map[int64]bool{}, map[int64]bool{}, map[int64]bool{}}
	add := func(set map[int64]bool, val any) {
		if id, ok := intValue(val); ok && id > 0 {
			set[id] = true
		}
	}
	types := func(t *tableSpec, rows ...Row) {
		for _, r := range rows {
			for _, col := range t.typeCols() {
				add(n.types, r[col])
			}
		}
	}
	for _, ct := range p.Types {
		add(n.groups, ct.Type["groupID"])
		types(tNPCLoot, ct.Loot...)
	}
	for _, d := range p.Dungeons {
		add(n.factions, d.Dungeon["factionID"])
		types(tObject, d.Objects...)
		types(tObjLoot, d.Loot...)
		types(tTrigger, d.Triggers...)
		for _, o := range d.Objects {
			add(n.factions, o["npcFactionID"])
		}
	}
	for _, m := range p.Missions {
		types(tMission, m)
		add(n.factions, m["enemyFactionID"])
		for _, id := range splitIDs(asString(m["allowedFactions"])) {
			n.factions[id] = true
		}
		for _, col := range []string{"pickupStationID", "dropoffStationID", "killItemDropoffStationID"} {
			add(n.stations, m[col])
		}
	}
	for _, s := range p.Spawners {
		for _, id := range splitIDs(asString(s["regionIDs"])) {
			n.regions[id] = true
		}
	}
	return n
}

// sortedIDs returns a set's IDs in order.
func sortedIDs(set map[int64]bool) []int64 {
	out := make([]int64, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// numText returns the literal of a numeric row value.
func numText(val any) (string, bool) {
	switch x := val.(type) {
	case int64:
		return strconv.FormatInt(x, 10), true
	case json.Number:
		return string(x), true
	}
	return "", false
}

// intValue reads a whole-number row value. Text, booleans and decimals do not count.
func intValue(val any) (int64, bool) {
	s, ok := numText(val)
	if !ok || !intPattern.MatchString(s) {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

// floatValue reads a finite numeric row value.
func floatValue(val any) (float64, bool) {
	s, ok := numText(val)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

func intOf(val any) int64 {
	n, _ := intValue(val)
	return n
}

func floatOf(val any) float64 {
	f, _ := floatValue(val)
	return f
}

func fmtNum(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }
