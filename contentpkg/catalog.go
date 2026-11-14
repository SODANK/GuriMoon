package contentpkg

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// CatalogItem is one exportable row for the export picker.
type CatalogItem struct {
	ID      int64
	Name    string
	Detail  string // faction, type and level, or the dungeon it uses
	Enabled bool
	Package string // name of the installed package that owns the row, if any
}

// Catalog lists everything that can be exported.
type Catalog struct {
	Dungeons []CatalogItem
	Missions []CatalogItem
	Spawners []CatalogItem
}

// LoadCatalog reads the dungeons, mission templates and spawners on this server.
func LoadCatalog(ctx context.Context, db *sql.DB) (*Catalog, error) {
	owners := packageOwners(ctx, db)
	c := &Catalog{}
	var err error
	if c.Dungeons, err = catalogQuery(ctx, db, owners[tDungeon.name], `
		SELECT d.dungeonVID, d.dungeonVName,
		       CONCAT(COALESCE(f.factionName, CONCAT('faction ', d.factionID)), ' · ',
		              (SELECT COUNT(*) FROM dunRooms r WHERE r.dungeonVID = d.dungeonVID), ' room(s)'),
		       1
		  FROM dunDungeons d LEFT JOIN chrFactions f ON f.factionID = d.factionID
		 ORDER BY d.dungeonVName, d.dungeonVID`); err != nil {
		return nil, err
	}
	if c.Missions, err = catalogQuery(ctx, db, owners[tMission.name], `
		SELECT m.templateID, m.missionName,
		       CONCAT(m.missionType, ' L', m.agentLevel,
		              IF(m.dungeonVID > 0, CONCAT(' · ', COALESCE(d.dungeonVName, CONCAT('dungeon ', m.dungeonVID))), '')),
		       COALESCE(m.enabled, 0)
		  FROM missionTemplates m LEFT JOIN dunDungeons d ON d.dungeonVID = m.dungeonVID
		 ORDER BY m.missionName, m.templateID`); err != nil {
		return nil, err
	}
	if tableExists(ctx, db, tSpawner.name) {
		if c.Spawners, err = catalogQuery(ctx, db, owners[tSpawner.name], `
			SELECT s.spawnerID, s.name, COALESCE(d.dungeonVName, CONCAT('dungeon ', s.dungeonVID)), s.enabled
			  FROM dunSpawners s LEFT JOIN dunDungeons d ON d.dungeonVID = s.dungeonVID
			 ORDER BY s.name, s.spawnerID`); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func catalogQuery(ctx context.Context, db *sql.DB, owners map[int64]string, q string) ([]CatalogItem, error) {
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	defer rows.Close()
	var out []CatalogItem
	for rows.Next() {
		var it CatalogItem
		var detail sql.NullString
		var enabled int64
		if err := rows.Scan(&it.ID, &it.Name, &detail, &enabled); err != nil {
			return nil, err
		}
		it.Detail, it.Enabled, it.Package = detail.String, enabled != 0, owners[it.ID]
		out = append(out, it)
	}
	return out, rows.Err()
}

// packageOwners maps table -> row ID -> name of the installed package that owns the row.
func packageOwners(ctx context.Context, db *sql.DB) map[string]map[int64]string {
	out := map[string]map[int64]string{}
	if !tableExists(ctx, db, "contentPackageRows") {
		return out
	}
	rows, err := db.QueryContext(ctx, `
		SELECT r.tableName, r.rowID, p.name FROM contentPackageRows r
		  JOIN contentPackages p ON p.pkgUUID = r.pkgUUID
		 WHERE r.tableName IN ('dunDungeons', 'missionTemplates', 'dunSpawners')`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var table, name string
		var id int64
		if rows.Scan(&table, &id, &name) != nil {
			continue
		}
		if t := specByName(table); t != nil {
			table = t.name
		}
		if out[table] == nil {
			out[table] = map[int64]string{}
		}
		out[table][id] = name
	}
	return out
}

// PackageOf returns the UUID of the one installed package that owns every selected row, or ""
// when the rows are unowned or belong to different packages.
func PackageOf(ctx context.Context, db *sql.DB, sel Selection) string {
	if !tableExists(ctx, db, "contentPackageRows") || sel.Empty() {
		return ""
	}
	var conds []string
	add := func(table string, ids []int64) {
		if len(ids) > 0 {
			conds = append(conds, fmt.Sprintf("(tableName = '%s' AND rowID IN (%s))", table, idList(ids)))
		}
	}
	add(tDungeon.name, sel.Dungeons)
	add(tMission.name, sel.Missions)
	add(tSpawner.name, sel.Spawners)
	rows, err := db.QueryContext(ctx, `SELECT pkgUUID, COUNT(*) FROM contentPackageRows WHERE `+
		strings.Join(conds, " OR ")+` GROUP BY pkgUUID`)
	if err != nil {
		return ""
	}
	defer rows.Close()
	want := len(sel.Dungeons) + len(sel.Missions) + len(sel.Spawners)
	var uuid string
	n := 0
	for rows.Next() {
		var u string
		var c int
		if rows.Scan(&u, &c) == nil {
			uuid, n = u, n+1
			if c != want {
				return ""
			}
		}
	}
	if n != 1 {
		return ""
	}
	return uuid
}
