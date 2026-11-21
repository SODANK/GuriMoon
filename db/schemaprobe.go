package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// Boot-time schema probes. Several Ensure* functions used to issue a MODIFY or a
// primary-key rebuild on every start; depending on the change MariaDB rebuilds the
// whole table for it, which on a large table is a burst of gigabytes of writes per
// restart. These let the caller skip the ALTER when the column already has its target
// shape.

// columnShape returns a column's COLUMN_TYPE (lower case, e.g. "varchar(50)",
// "int(11) unsigned", "double") and nullability. ok is false when the column (or
// table) does not exist.
func columnShape(ctx context.Context, db *sql.DB, table, column string) (colType string, nullable, ok bool, err error) {
	var ct, isNull string
	err = db.QueryRowContext(ctx, `
		SELECT COLUMN_TYPE, IS_NULLABLE FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`,
		table, column).Scan(&ct, &isNull)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, false, nil
	}
	if err != nil {
		return "", false, false, err
	}
	return strings.ToLower(ct), isNull == "YES", true, nil
}

// primaryKeyColumns returns a table's primary-key columns in key order, lower case.
func primaryKeyColumns(ctx context.Context, db *sql.DB, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT COLUMN_NAME FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND INDEX_NAME = 'PRIMARY'
		ORDER BY SEQ_IN_INDEX`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, strings.ToLower(c))
	}
	return out, rows.Err()
}

// columnNeedsModify reports whether a MODIFY is needed: the column exists and its
// type does not satisfy match, or its nullability differs from wantNullable. A failed
// probe answers true, so the ALTER runs as it always did.
func columnNeedsModify(ctx context.Context, db *sql.DB, table, column string, wantNullable bool, match func(colType string) bool) bool {
	ct, nullable, ok, err := columnShape(ctx, db, table, column)
	if err != nil {
		return true
	}
	if !ok {
		return false // nothing to modify; the ALTER would only fail
	}
	return !match(ct) || nullable != wantNullable
}
