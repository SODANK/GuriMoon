package contentpkg

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// WriteSQL writes a MySQL script that installs the package on a server without the ESP panel or
// cmd/contentpkg. Only a valid package is written, so every value in the script is checked
// text or a checked number. Every row gets a new ID through LAST_INSERT_ID(), and the script records the
// install in the registry so the panel can uninstall it later. It is for a fresh install only:
// it does not upgrade, and it fails on a column the target server lacks.
func WriteSQL(w io.Writer, p *Package) error {
	if _, err := validate(p); err != nil {
		return err
	}
	e := &sqlEmitter{w: w, vars: map[string]string{}, used: map[string]bool{}}
	m := p.Manifest
	e.line("-- GuriMoon content package: %s %s", oneLine(m.Name), oneLine(m.Version))
	e.line("-- uuid %s, format %d, made %s for %s", m.UUID, m.Format, m.Created, m.Build)
	if m.Author != "" {
		e.line("-- author: %s", oneLine(m.Author))
	}
	e.line("-- Fresh install only. To upgrade or uninstall, use ESP Content Packages or cmd/contentpkg.")
	e.line("")
	e.line("SET NAMES utf8mb4;")
	e.line("SET SESSION sql_mode = REPLACE(@@sql_mode, 'NO_BACKSLASH_ESCAPES', '');")
	e.line("CREATE TABLE IF NOT EXISTS contentpackages (pkgUUID CHAR(36) NOT NULL PRIMARY KEY, name VARCHAR(255) NOT NULL, " +
		"version VARCHAR(64) NOT NULL DEFAULT '', author VARCHAR(255) NOT NULL DEFAULT '', description TEXT, manifest MEDIUMTEXT, " +
		"installedAt DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, updatedAt DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP) " +
		"ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;")
	e.line("CREATE TABLE IF NOT EXISTS contentpackagerows (pkgUUID CHAR(36) NOT NULL, pkgKey VARCHAR(64) NOT NULL, " +
		"tableName VARCHAR(64) NOT NULL, rowID BIGINT NOT NULL, PRIMARY KEY (pkgUUID, pkgKey), INDEX idx_cpr_row (tableName, rowID)) " +
		"ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;")
	e.line("START TRANSACTION;")

	for _, ct := range p.Types {
		v := e.varFor(ct.Type.Key())
		e.line("")
		e.line("-- custom type %s: %s", ct.Type.Key(), oneLine(asString(ct.Type["typeName"])))
		e.line("SELECT COALESCE(MAX(typeID), %d) + 1 INTO %s FROM invtypes WHERE typeID >= %d;", CustomTypeFloor, v, CustomTypeFloor)
		e.insert(tType, ct.Type, v, nil)
		e.insertAll(tAttr, ct.Attributes)
		e.insertAll(tEffect, ct.Effects)
	}
	for _, ct := range p.Types {
		for _, r := range ct.Loot {
			e.insert(tNPCLoot, r, "", nil)
		}
		e.insertAll(tNPCCap, ct.LootCaps)
	}
	for _, d := range p.Dungeons {
		e.line("")
		e.line("-- dungeon %s: %s", d.Dungeon.Key(), oneLine(asString(d.Dungeon["dungeonVName"])))
		e.insert(tDungeon, d.Dungeon, "", nil)
		for i, r := range d.Rooms {
			e.insert(tRoom, r, "", map[string]any{"roomOrder": int64(i + 1)})
		}
		for _, r := range d.Objects {
			e.insert(tObject, r, "", nil)
		}
		for _, r := range d.Loot {
			e.insert(tObjLoot, r, "", nil)
		}
		for _, r := range d.Triggers {
			e.insert(tTrigger, r, "", nil)
		}
	}
	if len(p.Missions) > 0 {
		e.line("")
		e.line("-- mission templates")
	}
	for _, r := range p.Missions {
		e.insert(tMission, r, "", nil)
	}
	if len(p.Spawners) > 0 {
		e.line("")
		e.line("-- spawners")
	}
	for _, r := range p.Spawners {
		e.insert(tSpawner, r, "", map[string]any{"lastDespawnAt": int64(0)})
	}
	if len(e.late) > 0 {
		e.line("")
		e.line("-- links written once every row exists")
		for _, l := range e.late {
			e.line("UPDATE %s SET `%s` = %s WHERE `%s` = %s;", strings.ToLower(l.t.name), l.col, e.varFor(l.key),
				l.t.pk, e.varFor(l.owner))
		}
	}

	manifest, err := json.Marshal(m)
	if err != nil {
		return err
	}
	e.line("")
	e.line("-- install registry")
	e.line("INSERT INTO contentpackages (pkgUUID, name, version, author, description, manifest) VALUES (%s, %s, %s, %s, %s, %s);",
		quote(m.UUID), quote(m.Name), quote(m.Version), quote(m.Author), quote(m.Description), quote(string(manifest)))
	for _, k := range e.keys {
		e.line("INSERT INTO contentpackagerows (pkgUUID, pkgKey, tableName, rowID) VALUES (%s, %s, %s, %s);",
			quote(m.UUID), quote(k.key), quote(k.table), e.varFor(k.key))
	}
	e.line("COMMIT;")
	return e.err
}

type sqlEmitter struct {
	w    io.Writer
	err  error
	vars map[string]string // package key -> @variable
	used map[string]bool
	late []sqlLate
	keys []sqlKey
}

type sqlLate struct {
	t          *tableSpec
	col        string
	key, owner string
}

type sqlKey struct{ key, table string }

func (e *sqlEmitter) line(format string, args ...any) {
	if e.err != nil {
		return
	}
	if len(args) == 0 {
		_, e.err = io.WriteString(e.w, format+"\n")
		return
	}
	_, e.err = fmt.Fprintf(e.w, format+"\n", args...)
}

// varFor returns the session variable that holds a key's new row ID.
func (e *sqlEmitter) varFor(key string) string {
	if v, ok := e.vars[key]; ok {
		return v
	}
	var b strings.Builder
	b.WriteString("@k_")
	for _, c := range key {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' {
			b.WriteRune(c)
		} else {
			b.WriteByte('_')
		}
	}
	v := b.String()
	for n := 2; e.used[v]; n++ {
		v = b.String() + "_" + strconv.Itoa(n)
	}
	e.used[v] = true
	e.vars[key] = v
	return v
}

func (e *sqlEmitter) insertAll(t *tableSpec, rows []Row) {
	for _, r := range rows {
		e.insert(t, r, "", nil)
	}
}

// insert writes one INSERT of a validated row, columns in format order. pkVar, when set, is the
// variable already holding the primary key. overrides replace or add column values.
func (e *sqlEmitter) insert(t *tableSpec, r Row, pkVar string, overrides map[string]any) {
	var names, vals []string
	if pkVar != "" {
		names = append(names, "`"+t.pk+"`")
		vals = append(vals, pkVar)
	}
	for _, c := range t.cols {
		v, present := r[c.name]
		if ov, ok := overrides[c.name]; ok {
			v, present = ov, true
		}
		if !present {
			continue
		}
		names = append(names, "`"+c.name+"`")
		vals = append(vals, e.value(t, c, v, r.Key()))
	}
	extra := make([]string, 0, len(overrides))
	for col := range overrides {
		if _, inSchema := t.col(col); !inSchema {
			extra = append(extra, col)
		}
	}
	sort.Strings(extra)
	for _, col := range extra {
		names = append(names, "`"+col+"`")
		vals = append(vals, e.value(t, colDef{name: col, kind: kInt}, overrides[col], r.Key()))
	}
	e.line("INSERT INTO %s (%s) VALUES (%s);", strings.ToLower(t.name), strings.Join(names, ", "), strings.Join(vals, ", "))
	if t.pk != "" && r.Key() != "" {
		if pkVar == "" {
			e.line("SET %s = LAST_INSERT_ID();", e.varFor(r.Key()))
		}
		e.keys = append(e.keys, sqlKey{key: r.Key(), table: t.name})
	}
}

// value renders one column value; references become the variable of the row they point at.
func (e *sqlEmitter) value(t *tableSpec, c colDef, v any, owner string) string {
	if key := refKey(v); key != "" && (c.kind == kRef || c.kind == kType) {
		if t.isLate(c.name) {
			e.late = append(e.late, sqlLate{t: t, col: c.name, key: key, owner: owner})
			return "NULL"
		}
		return e.varFor(key)
	}
	switch x := v.(type) {
	case nil:
		return "NULL"
	case json.Number:
		return x.String()
	case int64:
		return strconv.FormatInt(x, 10)
	}
	return quote(asString(v))
}

// quote renders a MySQL string literal with backslash escapes.
func quote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('\'')
	for _, c := range s {
		switch c {
		case 0:
			b.WriteString(`\0`)
		case '\'':
			b.WriteString(`\'`)
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case 0x1a:
			b.WriteString(`\Z`)
		default:
			b.WriteRune(c)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

// oneLine flattens text for an SQL comment.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
