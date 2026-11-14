package contentpkg

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ImportOptions control an install.
type ImportOptions struct {
	// DryRun runs the whole install inside a transaction and rolls it back, so the report shows
	// exactly what would happen. Auto-increment counters still advance.
	DryRun bool
	// Disabled installs new mission templates and spawners with enabled = 0. Rows that replace
	// an installed version keep their current enabled state.
	Disabled bool
}

// Report describes an install, upgrade or uninstall.
type Report struct {
	UUID        string
	Name        string
	Version     string
	PrevVersion string
	Upgrade     bool
	DryRun      bool
	Created     map[string]int // table -> rows added
	Updated     map[string]int // table -> rows replaced in place
	Removed     map[string]int // table -> rows deleted
	Warnings    []string
	// Review lists what the package pays out (ISK, LP, items, bounties, live sites), for the
	// admin to read before installing.
	Review []string
	// TypeIDs lists every custom type written or deleted; the caller drops its type caches.
	TypeIDs  []int64
	Dungeons []NamedID
	Missions []NamedID
}

// NamedID is an installed row with its ID on this server.
type NamedID struct {
	ID   int64
	Name string
}

func newReport() *Report {
	return &Report{Created: map[string]int{}, Updated: map[string]int{}, Removed: map[string]int{}}
}

func (r *Report) warn(format string, args ...any) {
	r.Warnings = append(r.Warnings, fmt.Sprintf(format, args...))
}

// Total sums a count map.
func Total(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// prevRow is a row an installed version of the package owns.
type prevRow struct {
	table string
	id    int64
}

type lateRef struct {
	t   *tableSpec
	id  int64
	col string
	key string
}

// parentCol names the column that ties a child row to its parent; reuse checks it so a stale
// registry entry never replaces a row that now belongs to other content.
var parentCol = map[*tableSpec]string{tRoom: "dungeonVID", tObject: "roomID", tObjLoot: "objectID", tTrigger: "roomID"}

type importer struct {
	ctx      context.Context
	tx       *sql.Tx
	opt      ImportOptions
	rep      *Report
	cols     map[string]map[string]bool // table -> lower-case column names on this server
	keyTable map[string]*tableSpec      // package key -> its table
	ids      map[string]int64           // package key -> row ID on this server
	prev     map[string]prevRow         // package key -> row of the installed version
	carry    map[string]any             // package key -> enabled value of the replaced row
	late     []lateRef
}

// Import validates and installs a package. Installing a package whose UUID is already installed
// upgrades it: rows keep their IDs, new rows are added and rows the new version dropped are
// deleted. The whole install is one transaction; any problem rolls all of it back.
func Import(ctx context.Context, db *sql.DB, pkg *Package, opt ImportOptions) (*Report, error) {
	keyTable, err := validate(pkg)
	if err != nil {
		return nil, err
	}
	if err := EnsureTables(ctx, db); err != nil {
		return nil, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	m := pkg.Manifest
	rep := newReport()
	rep.UUID, rep.Name, rep.Version, rep.DryRun = m.UUID, m.Name, m.Version, opt.DryRun
	im := &importer{
		ctx: ctx, tx: tx, opt: opt, rep: rep,
		cols:     map[string]map[string]bool{},
		keyTable: keyTable,
		ids:      map[string]int64{},
		carry:    map[string]any{},
	}
	var prevName, prevAuthor string
	err = tx.QueryRowContext(ctx, `SELECT name, version, author FROM contentPackages WHERE pkgUUID = ?`, m.UUID).
		Scan(&prevName, &rep.PrevVersion, &prevAuthor)
	switch {
	case err == nil:
		rep.Upgrade = true
		if prevName != m.Name || prevAuthor != m.Author {
			rep.warn("this replaces the installed package %q by %q with %q by %q (same package ID)",
				prevName, prevAuthor, m.Name, m.Author)
		}
	case err != sql.ErrNoRows:
		return nil, err
	}
	if im.prev, err = loadPackageRows(ctx, tx, m.UUID); err != nil {
		return nil, err
	}
	if err := im.checkBase(pkg); err != nil {
		return nil, err
	}
	rep.Review = review(ctx, tx, pkg)

	// Rows the installed version owns that this version dropped go first, so unique keys
	// (one loot row per item) are free before the new rows arrive.
	removed := map[string]prevRow{}
	for key, p := range im.prev {
		if keyTable[key] == nil {
			removed[key] = p
		}
	}
	if err := liveDungeonUse(ctx, tx, idsOf(removed, tDungeon)); err != nil {
		return nil, err
	}
	dropTypes, err := deleteRows(ctx, tx, removed, ownedScope(im.prev), rep)
	if err != nil {
		return nil, err
	}

	if err := im.installTypes(pkg.Types); err != nil {
		return nil, err
	}
	for _, d := range pkg.Dungeons {
		if err := im.installDungeon(d); err != nil {
			return nil, err
		}
	}
	for _, r := range pkg.Missions {
		id, err := im.putKeyed(tMission, r, im.enabledOverride(r.Key()))
		if err != nil {
			return nil, err
		}
		rep.Missions = append(rep.Missions, NamedID{ID: id, Name: asString(r["missionName"])})
	}
	for _, r := range pkg.Spawners {
		ov := im.enabledOverride(r.Key())
		ov["lastDespawnAt"] = int64(0)
		if _, err := im.putKeyed(tSpawner, r, ov); err != nil {
			return nil, err
		}
	}
	for _, l := range im.late {
		target, ok := im.ids[l.key]
		if !ok {
			return nil, fmt.Errorf("%s.%s points at %s, which was not installed", l.t.name, l.col, l.key)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("UPDATE %s SET %s = ? WHERE %s = ?",
			l.t.name, l.col, l.t.pk), target, l.id); err != nil {
			return nil, fmt.Errorf("link %s.%s: %w", l.t.name, l.col, err)
		}
	}
	if err := dropUnusedTypes(ctx, tx, dropTypes, rep); err != nil {
		return nil, err
	}
	if err := im.writeRegistry(pkg); err != nil {
		return nil, err
	}
	sort.Slice(rep.TypeIDs, func(i, j int) bool { return rep.TypeIDs[i] < rep.TypeIDs[j] })
	if opt.DryRun {
		return rep, nil
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return rep, nil
}

// existing returns which of ids are present in table.col; extra narrows the match.
func existing(ctx context.Context, q queryer, table, col string, ids []int64, extra string) (map[int64]bool, error) {
	out := map[int64]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	query := fmt.Sprintf("SELECT %s FROM %s WHERE %s IN (%s)", col, table, col, idList(ids))
	if extra != "" {
		query += " AND " + extra
	}
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("check %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// checkBase rejects a package that refers to base data this server lacks: item types, NPC
// groups, factions, stations, regions, dogma attributes and effects, graphics and transmission
// icons. Names that differ from the exporting server's are warnings.
func (im *importer) checkBase(pkg *Package) error {
	var problems Problems
	need := computeRequires(pkg)
	req := pkg.Manifest.Requires
	check := func(kind, table, col string, set map[int64]bool, extra, why string, refs []BaseRef, nameCol string) {
		ids := sortedIDs(set)
		have, err := existing(im.ctx, im.tx, table, col, ids, extra)
		if err != nil {
			problems = append(problems, err.Error())
			return
		}
		for _, id := range ids {
			if !have[id] {
				problems = append(problems, fmt.Sprintf("%s %d %s on this server", kind, id, why))
			}
		}
		if nameCol == "" || len(refs) == 0 {
			return
		}
		names := lookupNames(im.ctx, im.tx, table, col, nameCol, ids)
		for _, r := range refs {
			if name, ok := names[r.ID]; ok && r.Name != "" && name != r.Name {
				im.rep.warn("%s %d is %q here but %q in the package", kind, r.ID, name, r.Name)
			}
		}
	}
	check("type", "invTypes", "typeID", need.types, "", "does not exist", req.Types, "typeName")
	check("group", "invGroups", "groupID", need.groups, "categoryID = 11", "is not an NPC group", req.Groups, "groupName")
	check("faction", "chrFactions", "factionID", need.factions, "", "does not exist", req.Factions, "factionName")
	check("station", "staStations", "stationID", need.stations, "", "does not exist", req.Stations, "stationName")
	check("region", "mapRegions", "regionID", need.regions, "", "does not exist", req.Regions, "regionName")

	attrs, effects, graphics, icons := map[int64]bool{}, map[int64]bool{}, map[int64]bool{}, map[int64]bool{}
	for _, ct := range pkg.Types {
		for _, r := range ct.Attributes {
			attrs[intOf(r["attributeID"])] = true
		}
		for _, r := range ct.Effects {
			effects[intOf(r["effectID"])] = true
		}
		if g := intOf(ct.Type["graphicID"]); g > 0 {
			graphics[g] = true
		}
	}
	for _, d := range pkg.Dungeons {
		for _, r := range d.Triggers {
			if id, err := strconv.ParseInt(asString(r["icon"]), 10, 64); err == nil {
				icons[id] = true
			}
		}
	}
	check("dogma attribute", "dgmAttributeTypes", "attributeID", attrs, "", "does not exist", nil, "")
	check("dogma effect", "dgmEffects", "effectID", effects, "", "does not exist", nil, "")
	check("graphic", "eveGraphics", "graphicID", graphics, "", "does not exist", nil, "")

	// A transmission icon is a graphicID with an icon, or a typeID whose graphic has one.
	if len(icons) > 0 {
		ids := sortedIDs(icons)
		asGraphic, err := existing(im.ctx, im.tx, "eveGraphics", "graphicID", ids, "COALESCE(icon, '') <> ''")
		if err != nil {
			problems = append(problems, err.Error())
		}
		asType, err := existing(im.ctx, im.tx, "invTypes", "typeID", ids,
			"graphicID IN (SELECT graphicID FROM eveGraphics WHERE COALESCE(icon, '') <> '')")
		if err != nil {
			problems = append(problems, err.Error())
		}
		for _, id := range ids {
			if !asGraphic[id] && !asType[id] {
				problems = append(problems, fmt.Sprintf("transmission icon %d is neither a graphic with an icon nor a type with one", id))
			}
		}
	}
	if len(problems) > 0 {
		return problems
	}
	return nil
}

// columns returns the lower-case column names a table has on this server.
func (im *importer) columns(t *tableSpec) (map[string]bool, error) {
	if c, ok := im.cols[t.name]; ok {
		return c, nil
	}
	c, err := tableColumns(im.ctx, im.tx, t.name)
	if err != nil {
		return nil, err
	}
	im.cols[t.name] = c
	return c, nil
}

// enabledOverride returns the enabled value a mission or spawner installs with.
func (im *importer) enabledOverride(key string) map[string]any {
	ov := map[string]any{}
	if v, ok := im.carry[key]; ok {
		ov["enabled"] = v
	} else if im.opt.Disabled {
		ov["enabled"] = int64(0)
	}
	return ov
}

// reuse returns the row ID the installed version used for key, after deleting that row so it
// can be written again. 0 means the key is new here, or the old row now belongs to other
// content and is left alone.
func (im *importer) reuse(t *tableSpec, key string, parentID int64) (int64, error) {
	p, ok := im.prev[key]
	if !ok || !strings.EqualFold(p.table, t.name) || (t == tType && p.id < CustomTypeFloor) {
		return 0, nil
	}
	cols, err := im.columns(t)
	if err != nil {
		return 0, err
	}
	sel := []string{"1"}
	if cols["enabled"] {
		sel = append(sel, "enabled")
	}
	pcol := parentCol[t]
	if pcol != "" {
		sel = append(sel, pcol)
	}
	dest := make([]any, len(sel))
	var one, enabled any
	var parent int64
	dest[0] = &one
	for i, c := range sel[1:] {
		if c == "enabled" {
			dest[i+1] = &enabled
		} else {
			dest[i+1] = &parent
		}
	}
	err = im.tx.QueryRowContext(im.ctx, fmt.Sprintf("SELECT %s FROM %s WHERE %s = ?",
		strings.Join(sel, ", "), t.name, t.pk), p.id).Scan(dest...)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if pcol != "" && parent != parentID {
		return 0, nil
	}
	if cols["enabled"] {
		if b, ok := enabled.([]byte); ok {
			enabled = string(b)
		}
		im.carry[key] = enabled
	}
	if _, err := im.tx.ExecContext(im.ctx, fmt.Sprintf("DELETE FROM %s WHERE %s = ?", t.name, t.pk), p.id); err != nil {
		return 0, err
	}
	return p.id, nil
}

// count adds a written row to the report: replaced when the installed version had the key.
func (im *importer) count(t *tableSpec, key string) {
	if _, ok := im.prev[key]; ok {
		im.rep.Updated[t.name]++
	} else {
		im.rep.Created[t.name]++
	}
}

// putKeyed writes one keyed row, reusing the installed version's row ID when there is one.
func (im *importer) putKeyed(t *tableSpec, r Row, overrides map[string]any) (int64, error) {
	key := r.Key()
	var parentID int64
	if pcol := parentCol[t]; pcol != "" {
		parentID = im.ids[refKey(r[pcol])]
	}
	pk, err := im.reuse(t, key, parentID)
	if err != nil {
		return 0, err
	}
	im.count(t, key)
	id, err := im.insert(t, r, pk, overrides)
	if err != nil {
		return 0, err
	}
	im.ids[key] = id
	return id, nil
}

// insert writes a validated row. pk 0 lets the table assign the primary key. overrides replace
// or add column values (roomOrder, enabled, lastDespawnAt).
func (im *importer) insert(t *tableSpec, r Row, pk int64, overrides map[string]any) (int64, error) {
	have, err := im.columns(t)
	if err != nil {
		return 0, err
	}
	var names []string
	var vals []any
	var lates []lateRef
	if pk != 0 {
		names, vals = append(names, t.pk), append(vals, pk)
	}
	for _, c := range t.cols {
		v, present := r[c.name]
		ov, overridden := overrides[c.name]
		if !present && !overridden {
			continue
		}
		if !have[strings.ToLower(c.name)] {
			return 0, fmt.Errorf("this server's %s table has no column %s; update the server first", t.name, c.name)
		}
		if overridden {
			v = ov
		}
		val, lateKey, err := im.resolve(t, c, v)
		if err != nil {
			return 0, err
		}
		if lateKey != "" {
			lates = append(lates, lateRef{t: t, col: c.name, key: lateKey})
		}
		names, vals = append(names, c.name), append(vals, val)
	}
	for col, v := range overrides {
		if _, inSchema := t.col(col); !inSchema && have[strings.ToLower(col)] {
			names, vals = append(names, col), append(vals, v)
		}
	}
	q := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", t.name, strings.Join(names, ", "),
		strings.TrimSuffix(strings.Repeat("?, ", len(names)), ", "))
	res, err := im.tx.ExecContext(im.ctx, q, vals...)
	if err != nil {
		return 0, fmt.Errorf("insert %s row %s: %w", t.name, r.Key(), err)
	}
	id := pk
	if id == 0 && t.pk != "" {
		if id, err = res.LastInsertId(); err != nil {
			return 0, err
		}
	}
	for _, l := range lates {
		l.id = id
		im.late = append(im.late, l)
	}
	return id, nil
}

// resolve turns a validated package value into the value written to the column. A late
// reference is written as NULL and returned as its key, to be linked once every row exists.
func (im *importer) resolve(t *tableSpec, c colDef, v any) (any, string, error) {
	if key := refKey(v); key != "" && (c.kind == kRef || c.kind == kType) {
		if t.isLate(c.name) {
			return nil, key, nil
		}
		id, ok := im.ids[key]
		if !ok {
			return nil, "", fmt.Errorf("%s.%s points at %s, which is not installed yet", t.name, c.name, key)
		}
		return id, "", nil
	}
	if n, ok := v.(json.Number); ok {
		return n.String(), "", nil
	}
	return v, "", nil
}

// installTypes writes every custom type, then their loot rows (loot may drop another package type).
func (im *importer) installTypes(types []CustomType) error {
	for _, ct := range types {
		key := ct.Type.Key()
		pk, err := im.reuse(tType, key, 0)
		if err != nil {
			return err
		}
		im.count(tType, key)
		if pk == 0 {
			if err := im.tx.QueryRowContext(im.ctx,
				`SELECT COALESCE(MAX(typeID), ?) + 1 FROM invTypes WHERE typeID >= ?`,
				CustomTypeFloor, CustomTypeFloor).Scan(&pk); err != nil {
				return fmt.Errorf("allocate typeID: %w", err)
			}
		}
		if _, err := im.insert(tType, ct.Type, pk, nil); err != nil {
			return err
		}
		im.ids[key] = pk
		im.rep.TypeIDs = append(im.rep.TypeIDs, pk)
		if err := im.replaceChildren(tAttr, "typeID", pk, ct.Attributes); err != nil {
			return err
		}
		if tableExists(im.ctx, im.tx, tEffect.name) {
			if err := im.replaceChildren(tEffect, "typeID", pk, ct.Effects); err != nil {
				return err
			}
		} else if len(ct.Effects) > 0 {
			return fmt.Errorf("this server has no %s table for the effects of %s", tEffect.name, key)
		}
	}
	for _, ct := range types {
		typeID := im.ids[ct.Type.Key()]
		// The type's loot is the package's loot: every row scoped to it is rewritten.
		if _, err := im.tx.ExecContext(im.ctx, `DELETE FROM npc_loot_entries WHERE npcTypeID = ?`, typeID); err != nil {
			return err
		}
		for _, r := range ct.Loot {
			im.count(tNPCLoot, r.Key())
			id, err := im.insert(tNPCLoot, r, 0, nil)
			if err != nil {
				return err
			}
			im.ids[r.Key()] = id
		}
		if err := im.replaceChildren(tNPCCap, "npcTypeID", typeID, ct.LootCaps); err != nil {
			return err
		}
	}
	return nil
}

// replaceChildren deletes a custom type's child rows and writes the package's rows in their place.
func (im *importer) replaceChildren(t *tableSpec, parentCol string, parentID int64, rows []Row) error {
	if parentID < CustomTypeFloor {
		return fmt.Errorf("refusing to rewrite %s of base type %d", t.name, parentID)
	}
	where := parentCol + " = ?"
	if t == tNPCCap {
		where += " AND npcGroupID = 0"
	}
	if _, err := im.tx.ExecContext(im.ctx, "DELETE FROM "+t.name+" WHERE "+where, parentID); err != nil {
		return fmt.Errorf("clear %s: %w", t.name, err)
	}
	for _, r := range rows {
		if _, err := im.insert(t, r, 0, nil); err != nil {
			return err
		}
	}
	return nil
}

// installDungeon writes a dungeon, its rooms in package order, its objects, object loot and triggers.
func (im *importer) installDungeon(d Dungeon) error {
	vid, err := im.putKeyed(tDungeon, d.Dungeon, nil)
	if err != nil {
		return err
	}
	im.rep.Dungeons = append(im.rep.Dungeons, NamedID{ID: vid, Name: asString(d.Dungeon["dungeonVName"])})
	for i, r := range d.Rooms {
		// Rooms are stored in play order; an explicit roomOrder keeps that order under new IDs.
		if _, err := im.putKeyed(tRoom, r, map[string]any{"roomOrder": int64(i + 1)}); err != nil {
			return err
		}
	}
	for _, r := range d.Objects {
		id, err := im.putKeyed(tObject, r, nil)
		if err != nil {
			return err
		}
		// Loot rows added to a replaced object by hand would clash with the package's loot rows.
		if _, err := im.tx.ExecContext(im.ctx, `DELETE FROM dunObjectLoot WHERE objectID = ?`, id); err != nil {
			return err
		}
	}
	for _, r := range d.Loot {
		if _, err := im.putKeyed(tObjLoot, r, nil); err != nil {
			return err
		}
	}
	for _, r := range d.Triggers {
		if _, err := im.putKeyed(tTrigger, r, nil); err != nil {
			return err
		}
	}
	return nil
}

// writeRegistry records the package and the row ID every key got.
func (im *importer) writeRegistry(pkg *Package) error {
	m := pkg.Manifest
	manifest, err := encodeJSON(m)
	if err != nil {
		return err
	}
	if _, err := im.tx.ExecContext(im.ctx, `
		INSERT INTO contentPackages (pkgUUID, name, version, author, description, manifest)
		VALUES (?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE name = VALUES(name), version = VALUES(version), author = VALUES(author),
		       description = VALUES(description), manifest = VALUES(manifest), updatedAt = CURRENT_TIMESTAMP`,
		m.UUID, m.Name, m.Version, m.Author, m.Description, string(manifest)); err != nil {
		return fmt.Errorf("record package: %w", err)
	}
	if _, err := im.tx.ExecContext(im.ctx, `DELETE FROM contentPackageRows WHERE pkgUUID = ?`, m.UUID); err != nil {
		return err
	}
	keys := make([]string, 0, len(im.ids))
	for k := range im.ids {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	const batch = 200
	for start := 0; start < len(keys); start += batch {
		end := min(start+batch, len(keys))
		var sb strings.Builder
		args := make([]any, 0, (end-start)*4)
		sb.WriteString(`INSERT INTO contentPackageRows (pkgUUID, pkgKey, tableName, rowID) VALUES `)
		for i, k := range keys[start:end] {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString("(?, ?, ?, ?)")
			args = append(args, m.UUID, k, im.keyTable[k].name, im.ids[k])
		}
		if _, err := im.tx.ExecContext(im.ctx, sb.String(), args...); err != nil {
			return fmt.Errorf("record package rows: %w", err)
		}
	}
	return nil
}

// idsOf lists the row IDs of one table in a registry map.
func idsOf(rows map[string]prevRow, t *tableSpec) []int64 {
	var out []int64
	for _, p := range rows {
		if strings.EqualFold(p.table, t.name) {
			out = append(out, p.id)
		}
	}
	return out
}

// ownScope is what a package owns: its dungeons and custom types. Deletes of rooms, objects,
// loot, triggers and NPC loot only touch rows under these.
type ownScope struct {
	dungeons []int64
	types    []int64
}

func ownedScope(rows map[string]prevRow) ownScope {
	s := ownScope{dungeons: idsOf(rows, tDungeon)}
	for _, id := range idsOf(rows, tType) {
		if id >= CustomTypeFloor {
			s.types = append(s.types, id)
		}
	}
	return s
}

// deleteRows deletes package-owned rows, each dungeon, room and object with everything under it.
// Custom types are returned rather than deleted, for dropUnusedTypes once the rest is written.
func deleteRows(ctx context.Context, q queryer, rows map[string]prevRow, own ownScope, rep *Report) ([]int64, error) {
	byTable := map[string][]int64{}
	for _, p := range rows {
		if t := specByName(p.table); t != nil {
			byTable[t.name] = append(byTable[t.name], p.id)
			rep.Removed[t.name]++
		}
	}
	exec := func(what, query string) error {
		if _, err := q.ExecContext(ctx, query); err != nil {
			return fmt.Errorf("delete %s: %w", what, err)
		}
		return nil
	}
	// pick narrows ids to those inside the package's own dungeons or types.
	pick := func(query string) ([]int64, error) {
		r, err := q.QueryContext(ctx, query)
		if err != nil {
			return nil, err
		}
		defer r.Close()
		var out []int64
		for r.Next() {
			var id int64
			if err := r.Scan(&id); err != nil {
				return nil, err
			}
			out = append(out, id)
		}
		return out, r.Err()
	}
	ownRooms := "SELECT roomID FROM dunRooms WHERE dungeonVID IN (0" + listTail(own.dungeons) + ")"
	ownObjects := "SELECT objectID FROM dunObjects WHERE roomID IN (" + ownRooms + ")"
	cascadeRooms := func(rooms []int64) error {
		if len(rooms) == 0 {
			return nil
		}
		in := idList(rooms)
		objs, err := pick("SELECT objectID FROM dunObjects WHERE roomID IN (" + in + ")")
		if err != nil {
			return err
		}
		if len(objs) > 0 {
			if err := exec("object loot", "DELETE FROM dunObjectLoot WHERE objectID IN ("+idList(objs)+")"); err != nil {
				return err
			}
		}
		for _, s := range []string{
			"DELETE FROM dunTriggers WHERE roomID IN (" + in + ")",
			"DELETE FROM dunObjects WHERE roomID IN (" + in + ")",
			"DELETE FROM dunRooms WHERE roomID IN (" + in + ")",
		} {
			if err := exec("rooms", s); err != nil {
				return err
			}
		}
		return nil
	}

	if ids := byTable[tDungeon.name]; len(ids) > 0 {
		rooms, err := pick("SELECT roomID FROM dunRooms WHERE dungeonVID IN (" + idList(ids) + ")")
		if err != nil {
			return nil, err
		}
		if err := cascadeRooms(rooms); err != nil {
			return nil, err
		}
		if err := exec("dungeons", "DELETE FROM dunDungeons WHERE dungeonVID IN ("+idList(ids)+")"); err != nil {
			return nil, err
		}
	}
	if ids := byTable[tRoom.name]; len(ids) > 0 {
		rooms, err := pick("SELECT roomID FROM dunRooms WHERE roomID IN (" + idList(ids) + ") AND dungeonVID IN (0" + listTail(own.dungeons) + ")")
		if err != nil {
			return nil, err
		}
		if err := cascadeRooms(rooms); err != nil {
			return nil, err
		}
	}
	if ids := byTable[tObject.name]; len(ids) > 0 {
		objs, err := pick("SELECT objectID FROM dunObjects WHERE objectID IN (" + idList(ids) + ") AND roomID IN (" + ownRooms + ")")
		if err != nil {
			return nil, err
		}
		if len(objs) > 0 {
			in := idList(objs)
			if err := exec("object loot", "DELETE FROM dunObjectLoot WHERE objectID IN ("+in+")"); err != nil {
				return nil, err
			}
			if err := exec("objects", "DELETE FROM dunObjects WHERE objectID IN ("+in+")"); err != nil {
				return nil, err
			}
		}
	}
	if ids := byTable[tObjLoot.name]; len(ids) > 0 {
		loot, err := pick("SELECT lootID FROM dunObjectLoot WHERE lootID IN (" + idList(ids) + ") AND objectID IN (" + ownObjects + ")")
		if err != nil {
			return nil, err
		}
		if len(loot) > 0 {
			if err := exec("object loot", "DELETE FROM dunObjectLoot WHERE lootID IN ("+idList(loot)+")"); err != nil {
				return nil, err
			}
		}
	}
	if ids := byTable[tTrigger.name]; len(ids) > 0 {
		if err := exec("triggers", "DELETE FROM dunTriggers WHERE triggerID IN ("+idList(ids)+") AND roomID IN ("+ownRooms+")"); err != nil {
			return nil, err
		}
	}
	if ids := byTable[tNPCLoot.name]; len(ids) > 0 {
		if err := exec("NPC loot", "DELETE FROM npc_loot_entries WHERE entryID IN ("+idList(ids)+") AND npcTypeID IN (0"+listTail(own.types)+")"); err != nil {
			return nil, err
		}
	}
	for _, t := range []*tableSpec{tMission, tSpawner} {
		if ids := byTable[t.name]; len(ids) > 0 {
			if err := exec(t.name, "DELETE FROM "+t.name+" WHERE "+t.pk+" IN ("+idList(ids)+")"); err != nil {
				return nil, err
			}
		}
	}
	var types []int64
	for _, id := range byTable[tType.name] {
		if id >= CustomTypeFloor {
			types = append(types, id)
		}
	}
	return types, nil
}

// listTail renders ",1,2,3" for appending to an IN list that starts with a placeholder 0.
func listTail(ids []int64) string {
	if len(ids) == 0 {
		return ""
	}
	return "," + idList(ids)
}

// typeUses lists the columns outside the custom type tables that can hold a typeID.
var typeUses = []struct{ table, col string }{
	{"dunObjects", "typeID"}, {"dunObjectLoot", "itemTypeID"}, {"dunTriggers", "itemTypeID"},
	{"npc_loot_entries", "itemTypeID"},
	{"missionTemplates", "targetTypeID"}, {"missionTemplates", "killItemTypeID"},
	{"missionTemplates", "courierTypeID"}, {"missionTemplates", "tradeTypeID"},
	{"missionTemplates", "miningTypeID"}, {"missionTemplates", "baseRewardItemTypeID"},
	{"missionTemplates", "bonusRewardItemTypeID"},
}

// dropUnusedTypes deletes custom types with their attributes, effects and loot. A type still
// used by content outside the package stays and is reported. Base types are never touched.
func dropUnusedTypes(ctx context.Context, q queryer, ids []int64, rep *Report) error {
	for _, id := range ids {
		if id < CustomTypeFloor {
			continue
		}
		var uses []string
		for _, u := range typeUses {
			var n int
			if err := q.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s = ?", u.table, u.col), id).Scan(&n); err != nil {
				continue
			}
			if n > 0 {
				uses = append(uses, fmt.Sprintf("%d %s row(s)", n, u.table))
			}
		}
		if len(uses) > 0 {
			rep.Removed[tType.name]--
			rep.warn("custom type %d stays on this server: still used by %s", id, strings.Join(uses, ", "))
			continue
		}
		for _, t := range []struct{ table, col string }{
			{"npc_loot_entries", "npcTypeID"}, {"npc_loot_caps", "npcTypeID"},
			{"dgmTypeEffects", "typeID"}, {"dgmTypeAttributes", "typeID"}, {"invTypes", "typeID"},
		} {
			if !tableExists(ctx, q, t.table) {
				continue
			}
			if _, err := q.ExecContext(ctx, "DELETE FROM "+t.table+" WHERE "+t.col+" = ?", id); err != nil {
				return fmt.Errorf("delete custom type %d: %w", id, err)
			}
		}
		rep.TypeIDs = append(rep.TypeIDs, id)
	}
	return nil
}

// liveDungeonUse returns an error naming the dungeons that players are in right now.
func liveDungeonUse(ctx context.Context, q queryer, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	var missions, sites int
	if tableExists(ctx, q, "missionDungeonInstances") {
		if err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM missionDungeonInstances WHERE dungeonVID IN ("+
			idList(ids)+")").Scan(&missions); err != nil {
			return err
		}
	}
	if tableExists(ctx, q, "dunOpenSites") {
		if err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM dunOpenSites WHERE clearedAt IS NULL AND dungeonVID IN ("+
			idList(ids)+")").Scan(&sites); err != nil {
			return err
		}
	}
	if missions > 0 || sites > 0 {
		return fmt.Errorf("dungeon(s) %s are in use: %d mission instance(s) and %d open site(s); "+
			"despawn the sites on Dungeon Spawners and wait for the missions to end", idList(ids), missions, sites)
	}
	return nil
}
