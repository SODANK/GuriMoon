package contentpkg

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"
)

// Selection names the top-level rows to export. Everything they depend on is added.
type Selection struct {
	Dungeons []int64
	Missions []int64
	Spawners []int64
}

// Empty reports whether nothing is selected.
func (s Selection) Empty() bool {
	return len(s.Dungeons) == 0 && len(s.Missions) == 0 && len(s.Spawners) == 0
}

// ExportOptions are the manifest fields of a new package. A UUID of an installed package keeps
// that package's identity and row keys, so the file installs as an upgrade.
type ExportOptions struct {
	UUID        string
	Name        string
	Version     string
	Author      string
	Description string
	Readme      string
}

type exporter struct {
	ctx      context.Context
	q        queryer
	keys     map[*tableSpec]map[int64]string // table -> row ID -> key
	used     map[string]bool
	reverse  map[string]map[int64]string // registry: table name -> row ID -> key
	custom   map[int64]bool              // custom typeIDs carried in the package
	unknown  map[string]bool             // table.column already warned about
	warnings []string
}

// Export reads the selected rows and their dependencies into a Package. The returned warnings
// list references that could not be followed (a mission whose dungeon was deleted, for example).
func Export(ctx context.Context, db *sql.DB, sel Selection, opt ExportOptions) (*Package, []string, error) {
	if sel.Empty() {
		return nil, nil, fmt.Errorf("nothing selected")
	}
	if opt.UUID != "" && !validUUID(opt.UUID) {
		return nil, nil, fmt.Errorf("invalid package uuid %q", opt.UUID)
	}
	e := &exporter{
		ctx: ctx, q: db,
		keys:    map[*tableSpec]map[int64]string{},
		used:    map[string]bool{},
		reverse: map[string]map[int64]string{},
		custom:  map[int64]bool{},
		unknown: map[string]bool{},
	}
	if opt.UUID != "" {
		if err := e.loadReverse(opt.UUID); err != nil {
			return nil, nil, err
		}
	}

	// Missions, following each nextMissionID chain.
	var missions []Row
	dungeonIDs := []int64{}
	dungeonSeen := map[int64]bool{}
	addDungeon := func(v any) {
		if id, ok := asInt(v); ok && id > 0 && !dungeonSeen[id] {
			dungeonSeen[id] = true
			dungeonIDs = append(dungeonIDs, id)
		}
	}
	for _, id := range sel.Dungeons {
		addDungeon(id)
	}
	explicit := map[int64]bool{}
	for _, id := range sel.Missions {
		explicit[id] = true
	}
	missionSeen := map[int64]bool{}
	queue := append([]int64(nil), sel.Missions...)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if missionSeen[id] {
			continue
		}
		missionSeen[id] = true
		rows, err := readRows(ctx, db, tMission, "templateID = ?", id)
		if err != nil {
			return nil, nil, err
		}
		if len(rows) == 0 {
			if explicit[id] {
				return nil, nil, fmt.Errorf("mission template %d not found", id)
			}
			e.warn("a mission chain points at template %d, which no longer exists; the link is dropped", id)
			continue
		}
		missions = append(missions, rows[0])
		addDungeon(rows[0]["dungeonVID"])
		if next, ok := asInt(rows[0]["nextMissionID"]); ok && next > 0 {
			queue = append(queue, next)
		}
	}

	// Spawners.
	var spawners []Row
	for _, id := range sel.Spawners {
		rows, err := readRows(ctx, db, tSpawner, "spawnerID = ?", id)
		if err != nil {
			return nil, nil, err
		}
		if len(rows) == 0 {
			return nil, nil, fmt.Errorf("spawner %d not found", id)
		}
		spawners = append(spawners, rows[0])
		addDungeon(rows[0]["dungeonVID"])
	}

	// Dungeons with every room, ball, ball loot row and trigger.
	type rawDungeon struct {
		dungeon                        Row
		rooms, objects, loot, triggers []Row
	}
	var dungeons []rawDungeon
	explicitDungeon := map[int64]bool{}
	for _, id := range sel.Dungeons {
		explicitDungeon[id] = true
	}
	for _, id := range dungeonIDs {
		rows, err := readRows(ctx, db, tDungeon, "dungeonVID = ?", id)
		if err != nil {
			return nil, nil, err
		}
		if len(rows) == 0 {
			if explicitDungeon[id] {
				return nil, nil, fmt.Errorf("dungeon %d not found", id)
			}
			e.warn("dungeon %d is referenced but no longer exists; the reference is dropped", id)
			continue
		}
		d := rawDungeon{dungeon: rows[0]}
		if d.rooms, err = readRows(ctx, db, tRoom,
			"dungeonVID = ? ORDER BY COALESCE(roomOrder, roomID), roomID", id); err != nil {
			return nil, nil, err
		}
		roomIDs := rowIDs(d.rooms, "roomID")
		if len(roomIDs) > 0 {
			if d.objects, err = readRows(ctx, db, tObject,
				"roomID IN ("+idList(roomIDs)+") ORDER BY objectID"); err != nil {
				return nil, nil, err
			}
			if d.triggers, err = readRows(ctx, db, tTrigger,
				"roomID IN ("+idList(roomIDs)+") ORDER BY triggerID"); err != nil {
				return nil, nil, err
			}
		}
		if objIDs := rowIDs(d.objects, "objectID"); len(objIDs) > 0 {
			if d.loot, err = readRows(ctx, db, tObjLoot,
				"objectID IN ("+idList(objIDs)+") ORDER BY lootID"); err != nil {
				return nil, nil, err
			}
		}
		dungeons = append(dungeons, d)
	}

	// Custom NPC types used anywhere above, and custom types their loot drops.
	typeQueue := []int64{}
	collect := func(t *tableSpec, rows []Row) {
		for _, r := range rows {
			for _, col := range t.typeCols() {
				if id, ok := asInt(r[col]); ok && id >= CustomTypeFloor {
					typeQueue = append(typeQueue, id)
				}
			}
		}
	}
	for _, d := range dungeons {
		collect(tObject, d.objects)
		collect(tObjLoot, d.loot)
		collect(tTrigger, d.triggers)
	}
	collect(tMission, missions)

	type rawType struct {
		typ                        Row
		attrs, effects, loot, caps []Row
	}
	var types []rawType
	hasEffects := tableExists(ctx, db, tEffect.name)
	typeSeen := map[int64]bool{}
	for len(typeQueue) > 0 {
		id := typeQueue[0]
		typeQueue = typeQueue[1:]
		if typeSeen[id] {
			continue
		}
		typeSeen[id] = true
		rows, err := readRows(ctx, db, tType, "typeID = ?", id)
		if err != nil {
			return nil, nil, err
		}
		if len(rows) == 0 {
			e.warn("custom type %d is referenced but has no invTypes row; it is left as a plain typeID", id)
			continue
		}
		rt := rawType{typ: rows[0]}
		if rt.attrs, err = readRows(ctx, db, tAttr, "typeID = ? ORDER BY attributeID", id); err != nil {
			return nil, nil, err
		}
		if hasEffects {
			if rt.effects, err = readRows(ctx, db, tEffect, "typeID = ? ORDER BY effectID", id); err != nil {
				return nil, nil, err
			}
		}
		if tableExists(ctx, db, tNPCLoot.name) {
			if rt.loot, err = readRows(ctx, db, tNPCLoot, "npcTypeID = ? ORDER BY entryID", id); err != nil {
				return nil, nil, err
			}
			if rt.caps, err = readRows(ctx, db, tNPCCap, "npcTypeID = ? AND npcGroupID = 0", id); err != nil {
				return nil, nil, err
			}
		}
		collect(tNPCLoot, rt.loot)
		types = append(types, rt)
		e.custom[id] = true
	}

	// Keys for every keyed row, before any row is encoded.
	for _, t := range types {
		e.assign(tType, t.typ)
		for _, r := range t.loot {
			e.assign(tNPCLoot, r)
		}
	}
	for _, d := range dungeons {
		e.assign(tDungeon, d.dungeon)
		for _, r := range d.rooms {
			e.assign(tRoom, r)
		}
		for _, r := range d.objects {
			e.assign(tObject, r)
		}
		for _, r := range d.loot {
			e.assign(tObjLoot, r)
		}
		for _, r := range d.triggers {
			e.assign(tTrigger, r)
		}
	}
	for _, r := range missions {
		e.assign(tMission, r)
	}
	for _, r := range spawners {
		e.assign(tSpawner, r)
	}

	// Encode.
	pkg := &Package{}
	m := &pkg.Manifest
	for _, t := range types {
		ct := CustomType{Type: e.encode(tType, t.typ)}
		ct.Attributes = e.encodeAll(tAttr, t.attrs)
		ct.Effects = e.encodeAll(tEffect, t.effects)
		ct.Loot = e.encodeAll(tNPCLoot, t.loot)
		ct.LootCaps = e.encodeAll(tNPCCap, t.caps)
		pkg.Types = append(pkg.Types, ct)
		m.Contents = append(m.Contents, entry("npc", ct.Type.Key(), asString(t.typ["typeName"])))
	}
	for _, d := range dungeons {
		pd := Dungeon{
			Dungeon:  e.encode(tDungeon, d.dungeon),
			Rooms:    e.encodeAll(tRoom, d.rooms),
			Objects:  e.encodeAll(tObject, d.objects),
			Loot:     e.encodeAll(tObjLoot, d.loot),
			Triggers: e.encodeAll(tTrigger, d.triggers),
		}
		pkg.Dungeons = append(pkg.Dungeons, pd)
		m.Contents = append(m.Contents, entry("dungeon", pd.Dungeon.Key(), asString(d.dungeon["dungeonVName"])))
	}
	for _, r := range missions {
		row := e.encode(tMission, r)
		pkg.Missions = append(pkg.Missions, row)
		m.Contents = append(m.Contents, entry("mission", row.Key(), asString(r["missionName"])))
	}
	for _, r := range spawners {
		row := e.encode(tSpawner, r)
		pkg.Spawners = append(pkg.Spawners, row)
		m.Contents = append(m.Contents, entry("spawner", row.Key(), asString(r["name"])))
	}

	// Manifest.
	m.Format = FormatVersion
	m.UUID = opt.UUID
	if m.UUID == "" {
		m.UUID = NewUUID()
	}
	m.Name = opt.Name
	if m.Name == "" && len(m.Contents) > len(types) {
		m.Name = m.Contents[len(types)].Name
	}
	m.Version = opt.Version
	if m.Version == "" {
		m.Version = "1.0"
	}
	m.Author = opt.Author
	m.Description = opt.Description
	m.Created = time.Now().UTC().Format(time.RFC3339)
	m.Build = ServerBuild
	pkg.Readme = opt.Readme
	m.Requires = e.requires(pkg)
	assignFiles(pkg)
	// A package that would be rejected on import is not exported.
	if _, err := validate(pkg); err != nil {
		return nil, e.warnings, err
	}
	return pkg, e.warnings, nil
}

func entry(kind, key, name string) ContentEntry {
	folder := map[string]string{"npc": "npcs", "dungeon": "dungeons", "mission": "missions", "spawner": "spawners"}[kind]
	file := folder + "/" + key
	if s := slug(name); s != "" {
		file += "-" + s
	}
	return ContentEntry{Kind: kind, Key: key, Name: name, File: file + ".json"}
}

func (e *exporter) warn(format string, args ...any) {
	e.warnings = append(e.warnings, fmt.Sprintf(format, args...))
}

// loadReverse reads an installed package's key map so re-exported rows keep their keys.
func (e *exporter) loadReverse(uuid string) error {
	if !tableExists(e.ctx, e.q, "contentPackageRows") {
		return nil
	}
	rows, err := e.q.QueryContext(e.ctx,
		`SELECT tableName, rowID, pkgKey FROM contentPackageRows WHERE pkgUUID = ?`, uuid)
	if err != nil {
		return fmt.Errorf("read package rows: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var table, key string
		var id int64
		if err := rows.Scan(&table, &id, &key); err != nil {
			return err
		}
		if t := specByName(table); t != nil {
			if e.reverse[t.name] == nil {
				e.reverse[t.name] = map[int64]string{}
			}
			e.reverse[t.name][id] = key
			e.used[key] = true
		}
	}
	return rows.Err()
}

// assign gives a keyed row its package key.
func (e *exporter) assign(t *tableSpec, r Row) {
	id, _ := asInt(r[t.pk])
	if e.keys[t] == nil {
		e.keys[t] = map[int64]string{}
	}
	if _, done := e.keys[t][id]; done {
		return
	}
	key, ok := e.reverse[t.name][id]
	if !ok {
		key = t.prefix + strconv.FormatInt(id, 10)
		for n := 2; e.used[key]; n++ {
			key = t.prefix + strconv.FormatInt(id, 10) + "_" + strconv.Itoa(n)
		}
	}
	e.used[key] = true
	e.keys[t][id] = key
}

// encode turns a database row into a package row: the primary key becomes the row's key, columns
// pointing at other package rows hold their keys, and only the format's columns are kept.
func (e *exporter) encode(t *tableSpec, raw Row) Row {
	out := Row{}
	if t.pk != "" {
		id, _ := asInt(raw[t.pk])
		out[KeyField] = e.keys[t][id]
	}
	for col := range raw {
		if _, ok := t.col(col); !ok && col != t.pk && !t.isIgnored(col) && !e.unknown[t.name+"."+col] {
			e.unknown[t.name+"."+col] = true
			e.warn("%s.%s is not part of the package format and is left out", t.name, col)
		}
	}
	for _, c := range t.cols {
		v, ok := raw[c.name]
		if !ok {
			continue
		}
		switch c.kind {
		case kRef:
			v = e.ref(t, c, v)
		case kType:
			if id, ok := asInt(v); ok && e.custom[id] {
				v = refValue(e.keys[tType][id])
			}
		case kBool:
			if n, ok := v.(int64); ok && n != 0 {
				v = int64(1)
			}
		}
		out[c.name] = v
	}
	return out
}

func (e *exporter) encodeAll(t *tableSpec, rows []Row) []Row {
	if len(rows) == 0 {
		return nil
	}
	out := make([]Row, len(rows))
	for i, r := range rows {
		out[i] = e.encode(t, r)
	}
	return out
}

// ref encodes a reference column. NULL and 0 stay as they are; a reference to a row outside the
// package becomes NULL.
func (e *exporter) ref(t *tableSpec, c colDef, v any) any {
	id, ok := asInt(v)
	if !ok || id == 0 {
		return v
	}
	if key, ok := e.keys[prefixSpec(c.ref)][id]; ok {
		return refValue(key)
	}
	e.warn("%s.%s points at %d, which is not in the package; it is exported as NULL", t.name, c.name, id)
	return nil
}

// requires lists the base data the package refers to, with names from this server.
func (e *exporter) requires(p *Package) Requires {
	n := computeRequires(p)
	return Requires{
		Types:    e.named(n.types, "invTypes", "typeID", "typeName"),
		Groups:   e.named(n.groups, "invGroups", "groupID", "groupName"),
		Factions: e.named(n.factions, "chrFactions", "factionID", "factionName"),
		Stations: e.named(n.stations, "staStations", "stationID", "stationName"),
		Regions:  e.named(n.regions, "mapRegions", "regionID", "regionName"),
	}
}

// named returns the IDs sorted, each with its name from table.
func (e *exporter) named(set map[int64]bool, table, idCol, nameCol string) []BaseRef {
	if len(set) == 0 {
		return nil
	}
	ids := sortedIDs(set)
	names := lookupNames(e.ctx, e.q, table, idCol, nameCol, ids)
	out := make([]BaseRef, len(ids))
	for i, id := range ids {
		out[i] = BaseRef{ID: id, Name: names[id]}
	}
	return out
}

// lookupNames reads id -> name for the given IDs. A missing table yields an empty map.
func lookupNames(ctx context.Context, q queryer, table, idCol, nameCol string, ids []int64) map[int64]string {
	out := map[int64]string{}
	if len(ids) == 0 {
		return out
	}
	rows, err := q.QueryContext(ctx, fmt.Sprintf("SELECT %s, %s FROM %s WHERE %s IN (%s)",
		idCol, nameCol, table, idCol, idList(ids)))
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name sql.NullString
		if rows.Scan(&id, &name) == nil {
			out[id] = name.String
		}
	}
	return out
}

// rowIDs collects the integer values of one column.
func rowIDs(rows []Row, col string) []int64 {
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		if id, ok := asInt(r[col]); ok {
			out = append(out, id)
		}
	}
	return out
}
