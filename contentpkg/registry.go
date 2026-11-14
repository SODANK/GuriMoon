package contentpkg

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// EnsureTables creates the install registry: one contentPackages row per installed package and
// one contentPackageRows row per package key, naming the row it became on this server.
func EnsureTables(ctx context.Context, db *sql.DB) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS contentPackages (
			pkgUUID     CHAR(36) NOT NULL PRIMARY KEY,
			name        VARCHAR(255) NOT NULL,
			version     VARCHAR(64) NOT NULL DEFAULT '',
			author      VARCHAR(255) NOT NULL DEFAULT '',
			description TEXT,
			manifest    MEDIUMTEXT,
			installedAt DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updatedAt   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS contentPackageRows (
			pkgUUID   CHAR(36) NOT NULL,
			pkgKey    VARCHAR(64) NOT NULL,
			tableName VARCHAR(64) NOT NULL,
			rowID     BIGINT NOT NULL,
			PRIMARY KEY (pkgUUID, pkgKey),
			INDEX idx_cpr_row (tableName, rowID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("contentpkg.EnsureTables: %w", err)
		}
	}
	return nil
}

// Installed is one installed package.
type Installed struct {
	UUID        string
	Name        string
	Version     string
	Author      string
	Description string
	InstalledAt time.Time
	UpdatedAt   time.Time
	Rows        map[string]int // table -> rows owned
}

// RowCount sums the rows a package owns in the given tables.
func (p Installed) RowCount(tables ...string) int {
	n := 0
	for _, t := range tables {
		n += p.Rows[t]
	}
	return n
}

// List returns the installed packages, newest first.
func List(ctx context.Context, db *sql.DB) ([]Installed, error) {
	if !tableExists(ctx, db, "contentPackages") {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT pkgUUID, name, version, author, COALESCE(description, ''),
		       DATE_FORMAT(installedAt, '%Y-%m-%d %H:%i:%s'), DATE_FORMAT(updatedAt, '%Y-%m-%d %H:%i:%s')
		  FROM contentPackages ORDER BY updatedAt DESC, name`)
	if err != nil {
		return nil, fmt.Errorf("list packages: %w", err)
	}
	defer rows.Close()
	var out []Installed
	index := map[string]int{}
	for rows.Next() {
		var p Installed
		var inst, upd string
		if err := rows.Scan(&p.UUID, &p.Name, &p.Version, &p.Author, &p.Description, &inst, &upd); err != nil {
			return nil, err
		}
		p.InstalledAt, _ = time.Parse("2006-01-02 15:04:05", inst)
		p.UpdatedAt, _ = time.Parse("2006-01-02 15:04:05", upd)
		p.Rows = map[string]int{}
		index[p.UUID] = len(out)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	counts, err := db.QueryContext(ctx,
		`SELECT pkgUUID, tableName, COUNT(*) FROM contentPackageRows GROUP BY pkgUUID, tableName`)
	if err != nil {
		return nil, err
	}
	defer counts.Close()
	for counts.Next() {
		var uuid, table string
		var n int
		if err := counts.Scan(&uuid, &table, &n); err != nil {
			return nil, err
		}
		if i, ok := index[uuid]; ok {
			if t := specByName(table); t != nil {
				table = t.name
			}
			out[i].Rows[table] = n
		}
	}
	return out, counts.Err()
}

// loadPackageRows reads the key -> row map of an installed package. A missing registry reads as
// nothing installed.
func loadPackageRows(ctx context.Context, q queryer, uuid string) (map[string]prevRow, error) {
	out := map[string]prevRow{}
	rows, err := q.QueryContext(ctx,
		`SELECT pkgKey, tableName, rowID FROM contentPackageRows WHERE pkgUUID = ?`, uuid)
	if err != nil {
		if strings.Contains(err.Error(), "doesn't exist") {
			return out, nil
		}
		return nil, fmt.Errorf("read package rows: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key, table string
		var id int64
		if err := rows.Scan(&key, &table, &id); err != nil {
			return nil, err
		}
		out[key] = prevRow{table: table, id: id}
	}
	return out, rows.Err()
}

// Uninstall deletes every row an installed package owns, then the package's registry entry.
// Validates the UUID first; only rows the registry lists, inside the package's own dungeons and
// custom types, are deleted.
// A dungeon with a live mission instance or open site blocks it. A custom type still used by
// other content stays.
func Uninstall(ctx context.Context, db *sql.DB, uuid string) (*Report, error) {
	if err := EnsureTables(ctx, db); err != nil {
		return nil, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if !validUUID(uuid) {
		return nil, fmt.Errorf("%q is not a package ID", uuid)
	}
	rep := newReport()
	rep.UUID = uuid
	if err := tx.QueryRowContext(ctx, `SELECT name, version FROM contentPackages WHERE pkgUUID = ?`, uuid).
		Scan(&rep.Name, &rep.Version); err == sql.ErrNoRows {
		return nil, fmt.Errorf("package %s is not installed", uuid)
	} else if err != nil {
		return nil, err
	}
	owned, err := loadPackageRows(ctx, tx, uuid)
	if err != nil {
		return nil, err
	}
	dungeons := idsOf(owned, tDungeon)
	if err := liveDungeonUse(ctx, tx, dungeons); err != nil {
		return nil, err
	}
	warnOutsideUse(ctx, tx, owned, dungeons, rep)
	types, err := deleteRows(ctx, tx, owned, ownedScope(owned), rep)
	if err != nil {
		return nil, err
	}
	if err := dropUnusedTypes(ctx, tx, types, rep); err != nil {
		return nil, err
	}
	for _, q := range []string{
		`DELETE FROM contentPackageRows WHERE pkgUUID = ?`,
		`DELETE FROM contentPackages WHERE pkgUUID = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, uuid); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return rep, nil
}

// warnOutsideUse reports mission templates and spawners outside the package that use one of its
// dungeons; they lose their dungeon on uninstall.
func warnOutsideUse(ctx context.Context, q queryer, owned map[string]prevRow, dungeons []int64, rep *Report) {
	if len(dungeons) == 0 {
		return
	}
	mine := map[string]map[int64]bool{}
	for _, p := range owned {
		if mine[strings.ToLower(p.table)] == nil {
			mine[strings.ToLower(p.table)] = map[int64]bool{}
		}
		mine[strings.ToLower(p.table)][p.id] = true
	}
	for _, t := range []*tableSpec{tMission, tSpawner} {
		rows, err := q.QueryContext(ctx, fmt.Sprintf("SELECT %s FROM %s WHERE dungeonVID IN (%s)",
			t.pk, t.name, idList(dungeons)))
		if err != nil {
			continue
		}
		var outside []int64
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil && !mine[strings.ToLower(t.name)][id] {
				outside = append(outside, id)
			}
		}
		rows.Close()
		if len(outside) > 0 {
			rep.warn("%s %s use a dungeon of this package and no longer have one", t.name, idList(outside))
		}
	}
}
