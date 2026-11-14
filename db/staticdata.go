package db

// Process-wide cache of dgmTypeAttributes rows. GetTypeAttributes feeds the whole combat path,
// so each type is read once. The writers (ESPUpdateTypeAttribute, ESPDeleteTypeAttribute and the
// managed rewrite in ESPSaveNPCSpec) invalidate; there is no TTL. TypeAttrs and TypeAttrsMany
// return shared read-only maps; GetTypeAttributes clones. See docs/db-11-notes.md#type-attribute-cache.

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"strings"
	"sync"
)

var (
	typeAttrMu    sync.RWMutex
	typeAttrCache = map[int64]map[int64]float64{}
)

// TypeAttrs returns the dgmTypeAttributes rows for typeID, from cache when present. The returned
// map is shared and read-only; callers that mutate use GetTypeAttributes, which clones.
func TypeAttrs(ctx context.Context, db *sql.DB, typeID int64) (map[int64]float64, error) {
	typeAttrMu.RLock()
	attrs, ok := typeAttrCache[typeID]
	typeAttrMu.RUnlock()
	if ok {
		return attrs, nil
	}

	attrs, err := loadTypeAttrs(ctx, db, typeID)
	if err != nil {
		return nil, err
	}

	typeAttrMu.Lock()
	typeAttrCache[typeID] = attrs
	typeAttrMu.Unlock()
	return attrs, nil
}

// TypeAttrsMany resolves a set of typeIDs, reading every miss in one query. Returned maps are
// shared and read-only. A typeID with no rows gets an empty map so the miss is not retried.
func TypeAttrsMany(ctx context.Context, db *sql.DB, typeIDs []int64) (map[int64]map[int64]float64, error) {
	out := make(map[int64]map[int64]float64, len(typeIDs))
	var missing []int64

	typeAttrMu.RLock()
	for _, id := range typeIDs {
		if id == 0 {
			continue
		}
		if _, done := out[id]; done {
			continue
		}
		if attrs, ok := typeAttrCache[id]; ok {
			out[id] = attrs
			continue
		}
		out[id] = nil // placeholder: dedupes the missing list
		missing = append(missing, id)
	}
	typeAttrMu.RUnlock()

	if len(missing) == 0 {
		return out, nil
	}

	loaded, err := loadTypeAttrsMany(ctx, db, missing)
	if err != nil {
		return nil, err
	}

	typeAttrMu.Lock()
	for _, id := range missing {
		attrs := loaded[id]
		if attrs == nil {
			// No rows for this type. Cache the empty map so the miss is not
			// re-queried forever.
			attrs = map[int64]float64{}
		}
		typeAttrCache[id] = attrs
		out[id] = attrs
	}
	typeAttrMu.Unlock()
	return out, nil
}

// InvalidateTypeStatics drops cached attributes (everything when called with no arguments).
// Every writer of dgmTypeAttributes in this package calls it; a new writer must too.
func InvalidateTypeStatics(typeIDs ...int64) {
	typeAttrMu.Lock()
	defer typeAttrMu.Unlock()
	if len(typeIDs) == 0 {
		clear(typeAttrCache)
		return
	}
	for _, id := range typeIDs {
		delete(typeAttrCache, id)
	}
}

// typeAttrValueExpr is the value projection shared by both loaders: valueFloat unless zero or
// NULL, then valueInt, then valueFloat again so a genuine 0.0 survives.
const typeAttrValueExpr = `COALESCE(NULLIF(valueFloat, 0), CAST(valueInt AS DOUBLE), valueFloat)`

func loadTypeAttrs(ctx context.Context, db *sql.DB, typeID int64) (map[int64]float64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT attributeID, `+typeAttrValueExpr+`
		FROM dgmTypeAttributes WHERE typeID = ?`, typeID)
	if err != nil {
		return nil, fmt.Errorf("TypeAttrs: %w", err)
	}
	defer rows.Close()
	attrs := make(map[int64]float64)
	for rows.Next() {
		var attrID int64
		var val float64
		if err := rows.Scan(&attrID, &val); err != nil {
			return nil, fmt.Errorf("TypeAttrs scan: %w", err)
		}
		attrs[attrID] = val
	}
	return attrs, rows.Err()
}

func loadTypeAttrsMany(ctx context.Context, db *sql.DB, typeIDs []int64) (map[int64]map[int64]float64, error) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(typeIDs)), ",")
	args := make([]any, len(typeIDs))
	for i, id := range typeIDs {
		args[i] = id
	}
	rows, err := db.QueryContext(ctx, `
		SELECT typeID, attributeID, `+typeAttrValueExpr+`
		FROM dgmTypeAttributes WHERE typeID IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("TypeAttrsMany: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]map[int64]float64, len(typeIDs))
	for rows.Next() {
		var typeID, attrID int64
		var val float64
		if err := rows.Scan(&typeID, &attrID, &val); err != nil {
			return nil, fmt.Errorf("TypeAttrsMany scan: %w", err)
		}
		attrs := out[typeID]
		if attrs == nil {
			attrs = make(map[int64]float64)
			out[typeID] = attrs
		}
		attrs[attrID] = val
	}
	return out, rows.Err()
}

// cloneAttrs copies a cached attribute map for a caller that may write to it.
func cloneAttrs(attrs map[int64]float64) map[int64]float64 {
	if attrs == nil {
		return map[int64]float64{}
	}
	return maps.Clone(attrs)
}
