package db

import (
	"context"
	"database/sql"

	"gurimoon/marshal"
)

// queryRows executes query with args, scans every row into cols.Items, and
// returns a util.Rowset. The query must return exactly len(cols) columns in
// the same order. A query error (for example a missing table) returns an
// empty rowset instead of an error.
func queryRows(ctx context.Context, db *sql.DB, cols []string, query string, args ...interface{}) (marshal.PyRep, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		// The table may not exist in this schema: return an empty rowset.
		return makeRowset(cols, nil), nil
	}
	defer rows.Close()

	var result [][]marshal.PyRep
	for rows.Next() {
		raw := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range raw {
			ptrs[i] = &raw[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make([]marshal.PyRep, len(cols))
		for i, v := range raw {
			row[i] = scanToRep(v)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return makeRowset(cols, result), nil
}

// scanToRep converts an interface{} value from sql.Scan to a marshal.PyRep.
// The go-sql-driver/mysql driver returns nil, int64, float64, []byte, or bool.
func scanToRep(v interface{}) marshal.PyRep {
	switch t := v.(type) {
	case nil:
		return marshal.PyNone{}
	case int64:
		return marshal.PyInt{Value: uint64(t)}
	case float32:
		return marshal.PyReal{Value: float64(t)}
	case float64:
		return marshal.PyReal{Value: t}
	case []byte:
		return marshal.PyString{Value: string(t)}
	case string:
		return marshal.PyString{Value: t}
	case bool:
		return marshal.PyBool{Value: t}
	default:
		return marshal.PyNone{}
	}
}
